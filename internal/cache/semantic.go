package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// SemanticStore is the L2 nearest-neighbour store.
type SemanticStore interface {
	// Search returns the most similar entry with similarity >= threshold.
	Search(ctx context.Context, k Key, threshold float64) (*Entry, float64, error)
	Put(ctx context.Context, k Key, e *Entry, ttl time.Duration) error
	Name() string
}

// ErrNotFound is returned by Search when no entry clears the threshold.
var ErrNotFound = errors.New("cache: not found")

// PGVectorStore stores embeddings in PostgreSQL and searches with pgvector
// HNSW indexes on cosine distance. The embedding column is dimensionless so
// several embedders can share the table (e.g. 768-d nomic-embed-text and the
// 384-d hashing fallback). Each dimension gets a partial expression index,
// the pattern pgvector documents for mixed dimensions:
//
//	CREATE INDEX ... USING hnsw ((embedding::vector(768)) vector_cosine_ops) WHERE dims = 768
//
// Queries cast with the same literal dimension so the planner can use it.
type PGVectorStore struct {
	Pool *pgxpool.Pool

	indexed sync.Map // dims -> struct{}
}

func (s *PGVectorStore) Name() string { return "pgvector" }

func searchSQL(dims int) string {
	return fmt.Sprintf(`
SELECT id, response_payload, model, 1 - (embedding::vector(%[1]d) <=> $1::vector(%[1]d)) AS similarity
FROM semantic_cache
WHERE tenant_id = $2 AND model = $4 AND scope_hash = $5 AND embedder = $6 AND dims = %[1]d
  AND expires_at > now()
  AND 1 - (embedding::vector(%[1]d) <=> $1::vector(%[1]d)) >= $3
ORDER BY embedding::vector(%[1]d) <=> $1::vector(%[1]d)
LIMIT 1`, dims)
}

func validDims(d int) error {
	if d < 1 || d > MaxIndexedDims {
		return fmt.Errorf("pgvector: unsupported embedding dimension %d (1..%d)", d, MaxIndexedDims)
	}
	return nil
}

// EnsureIndex creates the partial HNSW index for a dimension if missing.
// Concurrent replicas may race; a duplicate-index error is treated as success.
func (s *PGVectorStore) EnsureIndex(ctx context.Context, dims int) error {
	if err := validDims(dims); err != nil {
		return err
	}
	if _, ok := s.indexed.Load(dims); ok {
		return nil
	}
	_, err := s.Pool.Exec(ctx, fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS semantic_cache_hnsw_d%[1]d ON semantic_cache
USING hnsw ((embedding::vector(%[1]d)) vector_cosine_ops) WITH (m = 16, ef_construction = 64)
WHERE dims = %[1]d`, dims))
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && (pgErr.Code == "42P07" || pgErr.Code == "23505")) {
		return fmt.Errorf("pgvector: create index for %d dims: %w", dims, err)
	}
	s.indexed.Store(dims, struct{}{})
	return nil
}

// Search implements SemanticStore.
func (s *PGVectorStore) Search(ctx context.Context, k Key, threshold float64) (*Entry, float64, error) {
	if err := validDims(k.Dims()); err != nil {
		return nil, 0, err
	}
	var (
		id      int64
		payload []byte
		model   string
		sim     float64
	)
	err := s.Pool.QueryRow(ctx, searchSQL(k.Dims()), pgvector.NewVector(k.Embedding), k.TenantID, threshold, k.Model, k.ScopeHash, k.Embedder).
		Scan(&id, &payload, &model, &sim)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("pgvector search: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, 0, fmt.Errorf("decode cached payload: %w", err)
	}
	e.cacheID = id
	return &e, sim, nil
}

const upsertSQL = `
INSERT INTO semantic_cache (tenant_id, model, embedder, dims, scope_hash, prompt_hash, prompt_text, embedding,
  response_payload, prompt_tokens, completion_tokens, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now() + make_interval(secs => $12))
ON CONFLICT (tenant_id, model, embedder, prompt_hash) DO UPDATE SET
  response_payload = EXCLUDED.response_payload, embedding = EXCLUDED.embedding, dims = EXCLUDED.dims,
  scope_hash = EXCLUDED.scope_hash, expires_at = EXCLUDED.expires_at, created_at = now()`

// Put implements SemanticStore.
func (s *PGVectorStore) Put(ctx context.Context, k Key, e *Entry, ttl time.Duration) error {
	if err := s.EnsureIndex(ctx, k.Dims()); err != nil {
		return err
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, upsertSQL, k.TenantID, k.Model, k.Embedder, k.Dims(), k.ScopeHash, k.ExactHash, truncate(k.PromptText, 4000),
		pgvector.NewVector(k.Embedding), payload, e.PromptTokens, e.CompletionTokens, ttl.Seconds())
	if err != nil {
		return fmt.Errorf("pgvector upsert: %w", err)
	}
	return nil
}

// RecordHits applies aggregated hit-count increments in one statement.
// Hits are batched because per-hit UPDATEs on hot rows cause row-lock
// contention and dead tuples that inflate tail latency of concurrent searches.
func (s *PGVectorStore) RecordHits(ctx context.Context, hits map[int64]int64) error {
	if len(hits) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(hits))
	counts := make([]int64, 0, len(hits))
	for id, n := range hits {
		ids = append(ids, id)
		counts = append(counts, n)
	}
	_, err := s.Pool.Exec(ctx, `
UPDATE semantic_cache c SET hit_count = c.hit_count + u.n
FROM unnest($1::bigint[], $2::bigint[]) AS u(id, n)
WHERE c.id = u.id`, ids, counts)
	return err
}

// Purge deletes expired rows; returns rows removed.
func (s *PGVectorStore) Purge(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM semantic_cache WHERE expires_at <= now()`)
	return tag.RowsAffected(), err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// MemoryVectorStore is an exact brute-force in-process store, used in unit
// tests and as a degraded-mode fallback when PostgreSQL is unavailable.
type MemoryVectorStore struct {
	MaxPerScope int

	mu    sync.RWMutex
	items map[string][]memVec
}

type memVec struct {
	vec     []float32
	entry   *Entry
	expires time.Time
	hash    string
}

// NewMemoryVectorStore returns an empty in-memory vector store.
func NewMemoryVectorStore() *MemoryVectorStore {
	return &MemoryVectorStore{MaxPerScope: 10_000, items: map[string][]memVec{}}
}

func (m *MemoryVectorStore) Name() string { return "memory" }

func scopeKey(k Key) string {
	return k.TenantID + "\x00" + k.Model + "\x00" + k.ScopeHash + "\x00" + k.Embedder
}

// Search implements SemanticStore.
func (m *MemoryVectorStore) Search(_ context.Context, k Key, threshold float64) (*Entry, float64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	var best *Entry
	bestSim := -1.0
	for _, it := range m.items[scopeKey(k)] {
		if now.After(it.expires) {
			continue
		}
		if sim := Cosine(k.Embedding, it.vec); sim > bestSim {
			best, bestSim = it.entry, sim
		}
	}
	if best == nil || bestSim < threshold {
		return nil, 0, ErrNotFound
	}
	return best, bestSim, nil
}

// Put implements SemanticStore.
func (m *MemoryVectorStore) Put(_ context.Context, k Key, e *Entry, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sk := scopeKey(k)
	list := m.items[sk]
	for i := range list {
		if list[i].hash == k.ExactHash {
			list[i] = memVec{vec: k.Embedding, entry: e, expires: time.Now().Add(ttl), hash: k.ExactHash}
			return nil
		}
	}
	if len(list) >= m.MaxPerScope {
		list = list[1:]
	}
	m.items[sk] = append(list, memVec{vec: k.Embedding, entry: e, expires: time.Now().Add(ttl), hash: k.ExactHash})
	return nil
}
