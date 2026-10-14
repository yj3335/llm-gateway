package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Status is reported to clients in the X-Gateway-Cache header.
type Status string

const (
	StatusHitExact    Status = "HIT-EXACT"
	StatusHitSemantic Status = "HIT-SEMANTIC"
	StatusMiss        Status = "MISS"
	StatusBypass      Status = "BYPASS"
)

// Entry is a cached completion.
type Entry struct {
	Content          string    `json:"content"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	FinishReason     string    `json:"finish_reason"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CreatedAt        time.Time `json:"created_at"`
	// SourcePrompt is the prompt that produced this completion; on a semantic
	// hit it differs from the incoming prompt.
	SourcePrompt string `json:"source_prompt"`

	cacheID int64
}

// Message is a role/content pair.
type Message struct {
	Role    string
	Content string
}

// Key identifies a cacheable request.
type Key struct {
	TenantID   string
	Model      string
	ExactHash  string // sha256(tenant, model, normalized full conversation)
	ScopeHash  string // sha256(model, normalized context excluding the final user turn)
	PromptText string // final user turn: what gets embedded
	Embedding  []float32
	// Embedder names the model that produced Embedding; semantic matches are
	// only valid between vectors from the same embedder.
	Embedder string
}

// Dims returns the embedding dimensionality.
func (k Key) Dims() int { return len(k.Embedding) }

// BuildKey derives exact and scope hashes. Messages should already be
// guardrail-redacted so PII never reaches the cache.
func BuildKey(tenant, model string, msgs []Message) Key {
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			last = i
			break
		}
	}
	full := sha256.New()
	scope := sha256.New()
	write := func(h interface{ Write([]byte) (int, error) }, parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write(full, tenant, model)
	write(scope, model)
	for i, m := range msgs {
		norm := NormalizeText(m.Content)
		write(full, m.Role, norm)
		if i != last {
			write(scope, m.Role, norm)
		}
	}
	k := Key{
		TenantID:  tenant,
		Model:     model,
		ExactHash: hex.EncodeToString(full.Sum(nil)),
		ScopeHash: hex.EncodeToString(scope.Sum(nil)),
	}
	if last >= 0 {
		k.PromptText = strings.TrimSpace(msgs[last].Content)
	}
	return k
}

// Result is the outcome of a cache lookup.
type Result struct {
	Status     Status
	Entry      *Entry
	Similarity float64
	Layer      string // memory | redis | pgvector | memory-vector
	Embedder   string // embedder used for the semantic probe ("" if none ran)
	Elapsed    time.Duration
}

// Options configures the cache.
type Options struct {
	// SimilarityThreshold is the default cosine threshold; Thresholds overrides
	// it per embedder name (each embedder has its own similarity scale).
	SimilarityThreshold float64
	Thresholds          map[string]float64
	TTL                 time.Duration
	L1MaxEntries        int
	WriteQueueSize      int
	WriteWorkers        int
}

// Cache composes the exact (L1 memory + Redis) and semantic (L2) tiers.
type Cache struct {
	opts     Options
	l1       *LRU
	rdb      redis.UniversalClient // optional
	sem      SemanticStore         // optional
	embedder Embedder

	queue   chan writeJob
	wg      sync.WaitGroup
	qmu     sync.RWMutex // guards closed vs. sends on queue
	closed  bool
	pending atomic.Int64
	dropped atomic.Int64

	hitMu sync.Mutex
	hits  map[int64]int64 // pending semantic hit counts, flushed in batches
	stop  chan struct{}

	// OnError observes non-fatal backend errors (telemetry hook).
	OnError func(op string, err error)
}

type writeJob struct {
	key   Key
	entry *Entry
}

// New builds a cache. rdb and sem may be nil to disable those tiers.
func New(o Options, emb Embedder, sem SemanticStore, rdb redis.UniversalClient) *Cache {
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.L1MaxEntries <= 0 {
		o.L1MaxEntries = 50_000
	}
	if o.WriteQueueSize <= 0 {
		o.WriteQueueSize = 4096
	}
	if o.WriteWorkers <= 0 {
		o.WriteWorkers = 4
	}
	if o.SimilarityThreshold <= 0 {
		o.SimilarityThreshold = 0.86
	}
	c := &Cache{opts: o, l1: NewLRU(o.L1MaxEntries, o.TTL), rdb: rdb, sem: sem, embedder: emb,
		queue: make(chan writeJob, o.WriteQueueSize), hits: map[int64]int64{}, stop: make(chan struct{})}
	for i := 0; i < o.WriteWorkers; i++ {
		c.wg.Add(1)
		go c.writer()
	}
	if pg, ok := sem.(*PGVectorStore); ok {
		c.wg.Add(1)
		go c.hitFlusher(pg, 2*time.Second)
	}
	return c
}

// Threshold returns the default semantic similarity threshold.
func (c *Cache) Threshold() float64 { return c.opts.SimilarityThreshold }

// ThresholdFor returns the similarity threshold for an embedder.
func (c *Cache) ThresholdFor(embedder string) float64 {
	if t, ok := c.opts.Thresholds[embedder]; ok {
		return t
	}
	return c.opts.SimilarityThreshold
}

// Embed embeds text with the configured embedder and reports which embedder
// produced the vector (the fallback may have been used).
func (c *Cache) Embed(ctx context.Context, text string) ([]float32, string, error) {
	if c.embedder == nil {
		return nil, "", errors.New("cache: no embedder configured")
	}
	return embedNamed(ctx, c.embedder, text)
}

// EmbedderName names the configured (primary) embedder.
func (c *Cache) EmbedderName() string {
	if c.embedder == nil {
		return "none"
	}
	return c.embedder.Name()
}

// SemanticBackend names the L2 store ("none" if disabled).
func (c *Cache) SemanticBackend() string {
	if c.sem == nil {
		return "none"
	}
	return c.sem.Name()
}

func redisKey(k Key) string { return "llmgw:cache:" + k.ExactHash }

func (c *Cache) report(op string, err error) {
	if err != nil && c.OnError != nil {
		c.OnError(op, err)
	}
}

// Lookup checks L1 memory, L1 Redis, then L2 semantic. It fills k.Embedding
// on a semantic probe so a subsequent Store can reuse it.
func (c *Cache) Lookup(ctx context.Context, k *Key) Result {
	start := time.Now()
	done := func(r Result) Result {
		r.Elapsed = time.Since(start)
		return r
	}
	if e, ok := c.l1.Get(k.ExactHash); ok {
		return done(Result{Status: StatusHitExact, Entry: e, Similarity: 1, Layer: "memory"})
	}
	if c.rdb != nil {
		raw, err := c.rdb.Get(ctx, redisKey(*k)).Bytes()
		switch {
		case err == nil:
			var e Entry
			if jerr := json.Unmarshal(raw, &e); jerr == nil {
				c.l1.Set(k.ExactHash, &e)
				return done(Result{Status: StatusHitExact, Entry: &e, Similarity: 1, Layer: "redis"})
			}
		case !errors.Is(err, redis.Nil):
			c.report("redis_get", err)
		}
	}
	if c.sem == nil || c.embedder == nil || k.PromptText == "" {
		return done(Result{Status: StatusMiss})
	}
	if k.Embedding == nil {
		v, name, err := embedNamed(ctx, c.embedder, k.PromptText)
		if err != nil {
			c.report("embed", err)
			return done(Result{Status: StatusMiss})
		}
		k.Embedding, k.Embedder = v, name
	}
	e, sim, err := c.sem.Search(ctx, *k, c.ThresholdFor(k.Embedder))
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			c.report("semantic_search", err)
		}
		return done(Result{Status: StatusMiss, Embedder: k.Embedder})
	}
	if e.cacheID > 0 {
		c.hitMu.Lock()
		c.hits[e.cacheID]++
		c.hitMu.Unlock()
	}
	return done(Result{Status: StatusHitSemantic, Entry: e, Similarity: sim, Layer: c.sem.Name(), Embedder: k.Embedder})
}

// Store writes e into L1 memory synchronously and to Redis + L2 asynchronously.
func (c *Cache) Store(k Key, e *Entry) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.SourcePrompt == "" {
		e.SourcePrompt = k.PromptText
	}
	c.l1.Set(k.ExactHash, e)
	c.enqueue(writeJob{key: k, entry: e})
}

func (c *Cache) enqueue(j writeJob) {
	c.qmu.RLock()
	defer c.qmu.RUnlock()
	if c.closed {
		return
	}
	c.pending.Add(1)
	select {
	case c.queue <- j:
	default:
		c.pending.Add(-1)
		c.dropped.Add(1) // shed load rather than add latency to the hot path
	}
}

// Dropped returns the number of async writes shed due to a full queue.
func (c *Cache) Dropped() int64 { return c.dropped.Load() }

func (c *Cache) writer() {
	defer c.wg.Done()
	for j := range c.queue {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c.write(ctx, j)
		cancel()
		c.pending.Add(-1)
	}
}

func (c *Cache) hitFlusher(pg *PGVectorStore, every time.Duration) {
	defer c.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	flush := func() {
		c.hitMu.Lock()
		batch := c.hits
		c.hits = map[int64]int64{}
		c.hitMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c.report("semantic_hits", pg.RecordHits(ctx, batch))
		cancel()
	}
	for {
		select {
		case <-c.stop:
			flush()
			return
		case <-t.C:
			flush()
		}
	}
}

func (c *Cache) write(ctx context.Context, j writeJob) {
	if c.rdb != nil {
		if raw, err := json.Marshal(j.entry); err == nil {
			c.report("redis_set", c.rdb.Set(ctx, redisKey(j.key), raw, c.opts.TTL).Err())
		}
	}
	if c.sem != nil && c.embedder != nil && j.key.PromptText != "" {
		if j.key.Embedding == nil {
			v, name, err := embedNamed(ctx, c.embedder, j.key.PromptText)
			if err != nil {
				c.report("embed", err)
				return
			}
			j.key.Embedding, j.key.Embedder = v, name
		}
		c.report("semantic_put", c.sem.Put(ctx, j.key, j.entry, c.opts.TTL))
	}
}

// Flush blocks until queued writes complete or ctx expires (used by tests
// and benchmarks to observe write-behind results deterministically).
func (c *Cache) Flush(ctx context.Context) error {
	for c.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	return nil
}

// Close drains pending writes and stops workers.
func (c *Cache) Close() {
	c.qmu.Lock()
	if c.closed {
		c.qmu.Unlock()
		return
	}
	c.closed = true
	close(c.queue)
	close(c.stop)
	c.qmu.Unlock()
	c.wg.Wait()
}
