package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yj3335/llm-gateway/internal/storage"
)

// fakeOllama serves /api/embeddings like Ollama: unnormalized vectors of
// `dims` dimensions derived from the hashing embedder (so paraphrases stay
// similar). Setting down=true makes it fail.
type fakeOllama struct {
	srv     *httptest.Server
	dims    int
	down    atomic.Bool
	calls   atomic.Int64
	lastReq atomic.Value
}

func newFakeOllama(t *testing.T, dims int) *fakeOllama {
	f := &fakeOllama{dims: dims}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.down.Load() {
			http.Error(w, "model not loaded", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Model, Prompt string
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastReq.Store(req.Prompt)
		base := NewHashingEmbedder().embed(req.Prompt)
		v := make([]float32, f.dims)
		for i := range v {
			v[i] = base[i%len(base)] * 7 // unnormalized, like Ollama's /api/embeddings
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": v})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func TestOllamaEmbedder(t *testing.T) {
	f := newFakeOllama(t, 768)
	e := NewOllamaEmbedder(f.srv.URL, "nomic-embed-text", 0, "search_query: ", time.Second)
	v, err := e.Embed(context.Background(), "What is the CAP theorem?")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 768 || math.Abs(norm(v)-1) > 1e-5 {
		t.Fatalf("want normalized 768-d vector, got %d dims norm %.4f", len(v), norm(v))
	}
	if got := f.lastReq.Load(); got != "search_query: What is the CAP theorem?" {
		t.Fatalf("prefix not sent: %q", got)
	}
	if e.Name() != "ollama:nomic-embed-text" {
		t.Fatalf("name %q", e.Name())
	}

	trunc := NewOllamaEmbedder(f.srv.URL, "nomic-embed-text", 256, "", time.Second)
	v, err = trunc.Embed(context.Background(), "x")
	if err != nil || len(v) != 256 || math.Abs(norm(v)-1) > 1e-5 || trunc.Name() != "ollama:nomic-embed-text:256" {
		t.Fatalf("matryoshka truncation: %d dims norm %.4f name %s err %v", len(v), norm(v), trunc.Name(), err)
	}
	if _, err := NewOllamaEmbedder(f.srv.URL, "m", 1024, "", time.Second).Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error when embedding_dimensions exceeds native size")
	}
	f.down.Store(true)
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error from a failing server")
	}
}

func TestFallbackEmbedderSwitchesAndRecovers(t *testing.T) {
	f := newFakeOllama(t, 768)
	primary := NewOllamaEmbedder(f.srv.URL, "nomic-embed-text", 0, "", time.Second)
	fb := NewFallbackEmbedder(primary, NewHashingEmbedder(), 50*time.Millisecond)
	var fallbacks atomic.Int64
	fb.OnFallback = func(error) { fallbacks.Add(1) }

	v, name, err := fb.EmbedNamed(context.Background(), "hello")
	if err != nil || name != "ollama:nomic-embed-text" || len(v) != 768 {
		t.Fatalf("primary path: %s %d %v", name, len(v), err)
	}
	f.down.Store(true)
	v, name, err = fb.EmbedNamed(context.Background(), "hello")
	if err != nil || name != "hashing-ngram-384" || len(v) != Dim || fb.Healthy() {
		t.Fatalf("fallback path: %s %d %v healthy=%v", name, len(v), err, fb.Healthy())
	}
	calls := f.calls.Load()
	_, _, _ = fb.EmbedNamed(context.Background(), "again") // within backoff: primary not retried
	if f.calls.Load() != calls || fallbacks.Load() != 2 {
		t.Fatalf("primary retried during backoff (calls %d -> %d), fallbacks=%d", calls, f.calls.Load(), fallbacks.Load())
	}
	f.down.Store(false)
	time.Sleep(60 * time.Millisecond)
	if _, name, _ = fb.EmbedNamed(context.Background(), "hello"); name != "ollama:nomic-embed-text" || !fb.Healthy() {
		t.Fatalf("did not recover to primary: %s", name)
	}
}

// Entries embedded by different models must never match each other.
func TestCacheKeepsEmbeddersSeparate(t *testing.T) {
	f := newFakeOllama(t, 768)
	fb := NewFallbackEmbedder(NewOllamaEmbedder(f.srv.URL, "nomic-embed-text", 0, "", time.Second), NewHashingEmbedder(), time.Hour)
	c := New(Options{SimilarityThreshold: 0.92, Thresholds: map[string]float64{"hashing-ngram-384": 0.86}, WriteWorkers: 1}, fb, NewMemoryVectorStore(), nil)
	t.Cleanup(c.Close)
	ctx := context.Background()
	q := func(s string) Key { return BuildKey("t", "m", []Message{{Role: "user", Content: s}}) }

	k := q("Explain how Kafka consumer groups work in simple terms.")
	if r := c.Lookup(ctx, &k); r.Status != StatusMiss || k.Embedder != "ollama:nomic-embed-text" || k.Dims() != 768 {
		t.Fatalf("expected neural miss: %+v %s %d", r, k.Embedder, k.Dims())
	}
	c.Store(k, &Entry{Content: "answer"})
	_ = c.Flush(ctx)

	p := q("Can you explain in simple terms how Kafka consumer groups work?")
	if r := c.Lookup(ctx, &p); r.Status != StatusHitSemantic || r.Embedder != "ollama:nomic-embed-text" {
		t.Fatalf("expected neural semantic hit: %+v", r)
	}

	f.down.Store(true) // Ollama goes away: hashing vectors must not match 768-d entries
	p2 := q("Can you explain in simple terms how Kafka consumer groups work?")
	if r := c.Lookup(ctx, &p2); r.Status != StatusMiss || r.Embedder != "hashing-ngram-384" {
		t.Fatalf("cross-embedder match must not happen: %+v", r)
	}
	if c.ThresholdFor("hashing-ngram-384") != 0.86 || c.ThresholdFor("ollama:nomic-embed-text") != 0.92 {
		t.Fatal("per-embedder thresholds not applied")
	}
}

// Mixed dimensions share one pgvector table, each with its own partial index.
func TestPGVectorStoreMixedDimensions(t *testing.T) {
	dsn := os.Getenv("LLMGW_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("LLMGW_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	pool, err := storage.Connect(ctx, storage.Options{DSN: dsn, MaxConns: 4, Migrate: true})
	if err != nil {
		t.Fatal(err)
	}
	tenant := fmt.Sprintf("mixed-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM semantic_cache WHERE tenant_id=$1", tenant)
		pool.Close()
	})
	store := &PGVectorStore{Pool: pool}
	f := newFakeOllama(t, 512) // a dimension with no pre-created index
	embedders := []Embedder{NewHashingEmbedder(), NewOllamaEmbedder(f.srv.URL, "fake-512", 0, "", time.Second)}
	for _, e := range embedders {
		k := BuildKey(tenant, "m", []Message{{Role: "user", Content: "Explain how Raft leader election works."}})
		k.Embedding, k.Embedder, _ = embedNamed(ctx, e, k.PromptText)
		if err := store.Put(ctx, k, &Entry{Content: "answer from " + e.Name()}, time.Hour); err != nil {
			t.Fatalf("put %s: %v", e.Name(), err)
		}
	}
	for _, e := range embedders {
		k := BuildKey(tenant, "m", []Message{{Role: "user", Content: "Can you explain how Raft leader election works?"}})
		k.Embedding, k.Embedder, _ = embedNamed(ctx, e, k.PromptText)
		got, sim, err := store.Search(ctx, k, 0.8)
		if err != nil || got.Content != "answer from "+e.Name() {
			t.Fatalf("%s: wrong or missing hit %+v sim=%.3f err=%v", e.Name(), got, sim, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE indexname='semantic_cache_hnsw_d512'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("EnsureIndex did not create the 512-d partial index: n=%d err=%v", n, err)
	}
	k := BuildKey(tenant, "m", []Message{{Role: "user", Content: "x"}})
	k.Embedding = make([]float32, MaxIndexedDims+1)
	if _, _, err := store.Search(ctx, k, 0.5); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("expected an error for dimensions beyond pgvector's HNSW limit")
	}
}

// A slow (busy) embedder must not trigger the fallback: the lookup skips the
// semantic tier, and the background write still stores a neural embedding.
func TestSlowEmbedderDoesNotSplitTheCache(t *testing.T) {
	var delay atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(delay.Load()))
		var req struct{ Prompt string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": NewHashingEmbedder().embed(req.Prompt)})
	}))
	defer srv.Close()
	fb := NewFallbackEmbedder(NewOllamaEmbedder(srv.URL, "nomic-embed-text", 0, "", time.Second), NewHashingEmbedder(), time.Hour)
	var fallbacks atomic.Int64
	fb.OnFallback = func(error) { fallbacks.Add(1) }
	sem := NewMemoryVectorStore()
	c := New(Options{SimilarityThreshold: 0.9, LookupEmbedTimeout: 50 * time.Millisecond, WriteWorkers: 1}, fb, sem, nil)
	t.Cleanup(c.Close)
	ctx := context.Background()

	delay.Store(int64(200 * time.Millisecond)) // busy: slower than the lookup budget
	k := BuildKey("t", "m", []Message{{Role: "user", Content: "Explain how Kafka consumer groups work in simple terms."}})
	r := c.Lookup(ctx, &k)
	if r.Status != StatusMiss || !strings.Contains(r.Embedder, "busy") || !fb.Healthy() || fallbacks.Load() != 0 {
		t.Fatalf("busy embedder must skip the lookup without falling back: %+v healthy=%v fallbacks=%d", r, fb.Healthy(), fallbacks.Load())
	}
	c.Store(k, &Entry{Content: "answer"}) // background write: no lookup deadline
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	delay.Store(0)
	p := BuildKey("t", "m", []Message{{Role: "user", Content: "Can you explain in simple terms how Kafka consumer groups work?"}})
	if r := c.Lookup(ctx, &p); r.Status != StatusHitSemantic || r.Embedder != "ollama:nomic-embed-text" {
		t.Fatalf("entry written during the busy period must be a neural embedding: %+v", r)
	}

	// Offline (connection refused) is different: that does fall back.
	off := NewFallbackEmbedder(NewOllamaEmbedder("http://127.0.0.1:1/api/embeddings", "nomic-embed-text", 0, "", 200*time.Millisecond), NewHashingEmbedder(), time.Hour)
	if _, name, err := off.EmbedNamed(ctx, "x"); err != nil || name != "hashing-ngram-384" || off.Healthy() {
		t.Fatalf("unreachable embedder must fall back: %s %v", name, err)
	}
}
