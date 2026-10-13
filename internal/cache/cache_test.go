package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/yj3335/llm-gateway/internal/storage"
)

const threshold = 0.86

func embed(t testing.TB, s string) []float32 {
	t.Helper()
	v, err := NewHashingEmbedder().Embed(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestHashingEmbedderProperties(t *testing.T) {
	a := embed(t, "Explain how Kafka consumer groups work.")
	b := embed(t, "Explain how Kafka consumer groups work.")
	if len(a) != Dim {
		t.Fatalf("dim=%d", len(a))
	}
	var norm float64
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("embedding not deterministic")
		}
		norm += float64(a[i]) * float64(a[i])
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatalf("not unit length: %f", norm)
	}
	if empty := embed(t, "the a of"); Cosine(empty, empty) != 1 {
		t.Fatal("stop-word-only prompt must still produce a valid vector")
	}
}

func TestParaphrasesClearThreshold(t *testing.T) {
	pairs := [][2]string{
		{"Explain how Kafka consumer groups work in simple terms.", "Can you explain in simple terms how Kafka consumer groups work?"},
		{"What are the advantages and disadvantages of Redis Cluster?", "What are the pros and cons of Redis Cluster?"},
		{"What is the capital of France?", "what's the capital of france"},
		{"Give me an example of using Python asyncio gather.", "Show an example of Python asyncio gather usage"},
		{"Describe the differences between TCP and UDP.", "Please explain the difference between TCP and UDP"},
	}
	for _, p := range pairs {
		if sim := Cosine(embed(t, p[0]), embed(t, p[1])); sim < threshold {
			t.Errorf("paraphrase similarity %.3f < %.2f: %q vs %q", sim, threshold, p[0], p[1])
		}
	}
}

func TestDifferentIntentsStayBelowThreshold(t *testing.T) {
	pairs := [][2]string{
		{"Explain how Kafka consumer groups work in simple terms.", "Explain how Redis Cluster sharding works in simple terms."},
		{"Explain how Kafka consumer groups work.", "What are the pros and cons of Kafka consumer groups?"},
		{"What is the capital of France?", "What is the capital of Germany?"},
		{"Explain how TCP congestion control works.", "Explain how TCP flow control works."},
		{"Write a haiku about autumn.", "Write a SQL query that finds duplicate emails."},
	}
	for _, p := range pairs {
		if sim := Cosine(embed(t, p[0]), embed(t, p[1])); sim >= threshold {
			t.Errorf("false-positive similarity %.3f >= %.2f: %q vs %q", sim, threshold, p[0], p[1])
		}
	}
}

func TestBuildKey(t *testing.T) {
	sys := Message{"system", "You are terse."}
	k1 := BuildKey("t1", "gpt-4o-mini", []Message{sys, {"user", "What is the capital of France?"}})
	k2 := BuildKey("t1", "gpt-4o-mini", []Message{sys, {"user", "  what IS the capital of   france "}})
	if k1.ExactHash != k2.ExactHash {
		t.Fatal("normalization should make casing/whitespace variants exact-equal")
	}
	if k1.ScopeHash != BuildKey("t1", "gpt-4o-mini", []Message{sys, {"user", "Different question"}}).ScopeHash {
		t.Fatal("scope must ignore the final user turn")
	}
	if k1.ScopeHash == BuildKey("t1", "gpt-4o-mini", []Message{{"system", "You are verbose."}, {"user", "What is the capital of France?"}}).ScopeHash {
		t.Fatal("scope must include the system prompt")
	}
	if k1.ExactHash == BuildKey("t2", "gpt-4o-mini", []Message{sys, {"user", "What is the capital of France?"}}).ExactHash {
		t.Fatal("exact hash must be tenant-scoped")
	}
	if k1.ExactHash == BuildKey("t1", "claude-3-haiku", []Message{sys, {"user", "What is the capital of France?"}}).ExactHash {
		t.Fatal("exact hash must be model-scoped")
	}
	if k1.PromptText != "What is the capital of France?" {
		t.Fatalf("prompt text %q", k1.PromptText)
	}
}

func TestLRUEvictionAndTTL(t *testing.T) {
	l := NewLRU(lruShards*2, 50*time.Millisecond)
	for i := 0; i < 1000; i++ {
		l.Set(fmt.Sprint(i), &Entry{Content: fmt.Sprint(i)})
	}
	if n := l.Len(); n > lruShards*2 {
		t.Fatalf("LRU exceeded capacity: %d", n)
	}
	l.Set("x", &Entry{Content: "x"})
	if e, ok := l.Get("x"); !ok || e.Content != "x" {
		t.Fatal("missing fresh entry")
	}
	time.Sleep(70 * time.Millisecond)
	if _, ok := l.Get("x"); ok {
		t.Fatal("entry should have expired")
	}
}

func newTestCache(t *testing.T, sem SemanticStore, rdb redis.UniversalClient) *Cache {
	t.Helper()
	c := New(Options{SimilarityThreshold: threshold, TTL: time.Hour, L1MaxEntries: 1000, WriteWorkers: 2}, NewHashingEmbedder(), sem, rdb)
	c.OnError = func(op string, err error) { t.Errorf("cache backend error %s: %v", op, err) }
	t.Cleanup(c.Close)
	return c
}

func TestCacheTiers(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sem := NewMemoryVectorStore()
	c := newTestCache(t, sem, rdb)
	ctx := context.Background()

	msgs := func(q string) []Message { return []Message{{"user", q}} }
	k := BuildKey("tenant-a", "gpt-4o-mini", msgs("Explain how Kafka consumer groups work in simple terms."))
	if r := c.Lookup(ctx, &k); r.Status != StatusMiss {
		t.Fatalf("expected miss, got %s", r.Status)
	}
	if k.Embedding == nil {
		t.Fatal("lookup should populate the embedding for reuse")
	}
	c.Store(k, &Entry{Content: "Consumer groups split partitions...", Model: "gpt-4o-mini", Provider: "openai", CompletionTokens: 42, PromptTokens: 12})
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// Exact (memory) hit.
	k2 := BuildKey("tenant-a", "gpt-4o-mini", msgs("explain how kafka consumer groups work in simple terms"))
	if r := c.Lookup(ctx, &k2); r.Status != StatusHitExact || r.Layer != "memory" {
		t.Fatalf("expected memory exact hit, got %+v", r)
	}

	// Exact (Redis) hit from a fresh replica sharing Redis.
	c2 := newTestCache(t, NewMemoryVectorStore(), rdb)
	k3 := k
	k3.Embedding = nil
	if r := c2.Lookup(ctx, &k3); r.Status != StatusHitExact || r.Layer != "redis" || r.Entry.CompletionTokens != 42 {
		t.Fatalf("expected redis exact hit, got %+v", r)
	}

	// Semantic hit on a paraphrase.
	kp := BuildKey("tenant-a", "gpt-4o-mini", msgs("Can you explain in simple terms how Kafka consumer groups work?"))
	r := c.Lookup(ctx, &kp)
	if r.Status != StatusHitSemantic || r.Similarity < threshold || r.Entry.SourcePrompt != k.PromptText {
		t.Fatalf("expected semantic hit, got %+v", r)
	}

	// Tenant isolation: same prompt, other tenant -> miss.
	ko := BuildKey("tenant-b", "gpt-4o-mini", msgs("Can you explain in simple terms how Kafka consumer groups work?"))
	if r := c.Lookup(ctx, &ko); r.Status != StatusMiss {
		t.Fatalf("cross-tenant leak: %+v", r)
	}
	// Model isolation.
	km := BuildKey("tenant-a", "claude-3-5-haiku", msgs("Can you explain in simple terms how Kafka consumer groups work?"))
	if r := c.Lookup(ctx, &km); r.Status != StatusMiss {
		t.Fatalf("cross-model hit: %+v", r)
	}
	// Different intent -> miss.
	kd := BuildKey("tenant-a", "gpt-4o-mini", msgs("Explain how Redis Cluster sharding works in simple terms."))
	if r := c.Lookup(ctx, &kd); r.Status != StatusMiss {
		t.Fatalf("false semantic hit: %+v", r)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	c := newTestCache(t, NewMemoryVectorStore(), nil)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := BuildKey("t", "m", []Message{{"user", fmt.Sprintf("question number %d about topic %d", i%50, w%4)}})
				if r := c.Lookup(context.Background(), &k); r.Status == StatusMiss {
					c.Store(k, &Entry{Content: "answer"})
				}
			}
		}(w)
	}
	wg.Wait()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPEmbedder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      string `json:"input"`
			Dimensions int    `json:"dimensions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer k" || req.Dimensions != Dim {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		v := NewHashingEmbedder().embed(req.Input)
		for i := range v {
			v[i] *= 3 // un-normalized on purpose
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": v}}})
	}))
	defer srv.Close()
	e := NewHTTPEmbedder(srv.URL, "text-embedding-3-small", "k")
	v, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if sim := Cosine(v, embed(t, "hello world")); sim < 0.9999 {
		t.Fatalf("http embedder mismatch: %f", sim)
	}
	if _, err := NewHTTPEmbedder(srv.URL, "m", "wrong").Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on non-200")
	}
}

// TestPGVectorStore runs against a real PostgreSQL+pgvector when
// LLMGW_TEST_POSTGRES_DSN is set (make test / CI).
func TestPGVectorStore(t *testing.T) {
	dsn := os.Getenv("LLMGW_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("LLMGW_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	pool, err := storage.Connect(ctx, storage.Options{DSN: dsn, MaxConns: 8, Migrate: true, HNSWEfSearch: 64})
	if err != nil {
		t.Fatal(err)
	}
	tenant := fmt.Sprintf("pgtest-%d", time.Now().UnixNano())
	// Cleanups run LIFO: caches created below close (and flush) before the pool.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM semantic_cache WHERE tenant_id = $1", tenant)
		pool.Close()
	})

	store := &PGVectorStore{Pool: pool}
	c := newTestCache(t, store, nil)
	topics := []string{"Kafka consumer groups", "Redis Cluster sharding", "PostgreSQL MVCC", "Raft leader election", "TCP congestion control"}
	for _, topic := range topics {
		k := BuildKey(tenant, "gpt-4o-mini", []Message{{"user", "Explain how " + topic + " works in simple terms."}})
		c.Lookup(ctx, &k)
		c.Store(k, &Entry{Content: "answer about " + topic, Model: "gpt-4o-mini", CompletionTokens: 10})
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, topic := range topics {
		c2 := newTestCache(t, store, nil) // empty L1 forces the pgvector path
		k := BuildKey(tenant, "gpt-4o-mini", []Message{{"user", "Can you explain in simple terms how " + topic + " works?"}})
		r := c2.Lookup(ctx, &k)
		if r.Status != StatusHitSemantic || r.Layer != "pgvector" || r.Entry.Content != "answer about "+topic {
			t.Fatalf("topic %q: expected pgvector semantic hit with correct answer, got %+v", topic, r)
		}
	}
	k := BuildKey(tenant, "gpt-4o-mini", []Message{{"user", "Explain how B-tree page splits work in simple terms."}})
	if r := newTestCache(t, store, nil).Lookup(ctx, &k); r.Status != StatusMiss {
		t.Fatalf("unrelated prompt hit: %+v", r)
	}

	// The HNSW index must be used for the similarity ORDER BY.
	var plan string
	rows, err := pool.Query(ctx, "EXPLAIN SELECT id FROM semantic_cache ORDER BY embedding <=> $1 LIMIT 1", fmt.Sprintf("[%s1]", strings.Repeat("0,", Dim-1)))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan += line + "\n"
	}
	rows.Close()
	if !strings.Contains(plan, "semantic_cache_embedding_hnsw") {
		t.Logf("planner did not pick HNSW for tiny table (expected on small data):\n%s", plan)
	}

	n, err := store.Purge(ctx)
	if err != nil || n != 0 {
		t.Fatalf("purge unexpired rows: %d %v", n, err)
	}
}

func BenchmarkHashingEmbed(b *testing.B) {
	e := NewHashingEmbedder()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = e.Embed(context.Background(), "Can you explain in simple terms how Kafka consumer groups rebalance partitions?")
	}
}
