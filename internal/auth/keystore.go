package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// KeyStore resolves a raw API key to a tenant Principal.
type KeyStore interface {
	Lookup(ctx context.Context, apiKey string) (Principal, error)
}

// StaticKeyStore is an in-memory key store keyed by SHA-256 of the API key.
type StaticKeyStore struct {
	mu   sync.RWMutex
	keys map[string]Principal
}

// NewStaticKeyStore returns an empty static store.
func NewStaticKeyStore() *StaticKeyStore {
	return &StaticKeyStore{keys: map[string]Principal{}}
}

// Add registers a raw API key for principal p.
func (s *StaticKeyStore) Add(apiKey string, p Principal) {
	s.mu.Lock()
	s.keys[HashAPIKey(apiKey)] = p
	s.mu.Unlock()
}

func (s *StaticKeyStore) Lookup(_ context.Context, apiKey string) (Principal, error) {
	s.mu.RLock()
	p, ok := s.keys[HashAPIKey(apiKey)]
	s.mu.RUnlock()
	if !ok {
		return Principal{}, ErrInvalidAPIKey
	}
	return p, nil
}

// PostgresKeyStore looks up hashed keys in the api_keys/tenants tables.
type PostgresKeyStore struct {
	Pool *pgxpool.Pool
}

const lookupKeySQL = `
SELECT t.tenant_id, t.tier, t.rpm_limit, t.tpm_limit, t.pii_mode
FROM api_keys k JOIN tenants t ON t.tenant_id = k.tenant_id
WHERE k.key_hash = $1 AND k.revoked_at IS NULL`

func (s *PostgresKeyStore) Lookup(ctx context.Context, apiKey string) (Principal, error) {
	var p Principal
	err := s.Pool.QueryRow(ctx, lookupKeySQL, HashAPIKey(apiKey)).
		Scan(&p.TenantID, &p.Tier, &p.RPMLimit, &p.TPMLimit, &p.PIIMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidAPIKey
	}
	if err != nil {
		return Principal{}, fmt.Errorf("api key lookup: %w", err)
	}
	return p, nil
}

// UpsertTenant creates or updates a tenant and registers its API keys.
func (s *PostgresKeyStore) UpsertTenant(ctx context.Context, p Principal, apiKeys []string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
INSERT INTO tenants (tenant_id, tier, rpm_limit, tpm_limit, pii_mode)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (tenant_id) DO UPDATE SET tier=EXCLUDED.tier, rpm_limit=EXCLUDED.rpm_limit,
  tpm_limit=EXCLUDED.tpm_limit, pii_mode=EXCLUDED.pii_mode, updated_at=now()`,
		p.TenantID, p.Tier, p.RPMLimit, p.TPMLimit, p.PIIMode); err != nil {
		return fmt.Errorf("upsert tenant: %w", err)
	}
	for _, k := range apiKeys {
		if _, err := tx.Exec(ctx, `
INSERT INTO api_keys (key_hash, tenant_id, key_prefix) VALUES ($1,$2,$3)
ON CONFLICT (key_hash) DO UPDATE SET tenant_id=EXCLUDED.tenant_id, revoked_at=NULL`,
			HashAPIKey(k), p.TenantID, keyPrefix(k)); err != nil {
			return fmt.Errorf("upsert api key: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func keyPrefix(k string) string {
	if len(k) > 8 {
		return k[:8]
	}
	return k
}

// CachedKeyStore layers an in-process L1 cache and an optional shared Redis
// cache in front of an authoritative store. Negative results are cached
// briefly to blunt credential-stuffing load on the database.
type CachedKeyStore struct {
	Next     KeyStore
	Redis    *redis.Client // optional
	TTL      time.Duration
	NegTTL   time.Duration
	MaxItems int

	mu sync.RWMutex
	l1 map[string]l1Entry
}

type l1Entry struct {
	p     Principal
	err   error
	until time.Time
}

// NewCachedKeyStore wraps next with L1 (and optional Redis) caching.
func NewCachedKeyStore(next KeyStore, rdb *redis.Client, ttl time.Duration) *CachedKeyStore {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &CachedKeyStore{Next: next, Redis: rdb, TTL: ttl, NegTTL: 5 * time.Second, MaxItems: 100_000, l1: map[string]l1Entry{}}
}

func (c *CachedKeyStore) Lookup(ctx context.Context, apiKey string) (Principal, error) {
	h := HashAPIKey(apiKey)
	now := time.Now()
	c.mu.RLock()
	e, ok := c.l1[h]
	c.mu.RUnlock()
	if ok && now.Before(e.until) {
		return e.p, e.err
	}

	if c.Redis != nil {
		if raw, err := c.Redis.Get(ctx, "llmgw:apikey:"+h).Bytes(); err == nil {
			var p Principal
			if json.Unmarshal(raw, &p) == nil {
				c.store(h, l1Entry{p: p, until: now.Add(c.TTL)})
				return p, nil
			}
		}
	}

	p, err := c.Next.Lookup(ctx, apiKey)
	switch {
	case err == nil:
		c.store(h, l1Entry{p: p, until: now.Add(c.TTL)})
		if c.Redis != nil {
			if raw, mErr := json.Marshal(p); mErr == nil {
				c.Redis.Set(ctx, "llmgw:apikey:"+h, raw, c.TTL)
			}
		}
	case errors.Is(err, ErrInvalidAPIKey):
		c.store(h, l1Entry{err: err, until: now.Add(c.NegTTL)})
	}
	return p, err
}

// Invalidate drops a key from both cache layers (e.g. after revocation).
func (c *CachedKeyStore) Invalidate(ctx context.Context, apiKey string) {
	h := HashAPIKey(apiKey)
	c.mu.Lock()
	delete(c.l1, h)
	c.mu.Unlock()
	if c.Redis != nil {
		c.Redis.Del(ctx, "llmgw:apikey:"+h)
	}
}

func (c *CachedKeyStore) store(h string, e l1Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.l1) >= c.MaxItems {
		now := time.Now()
		for k, v := range c.l1 {
			if now.After(v.until) || len(c.l1) >= c.MaxItems {
				delete(c.l1, k)
			}
		}
	}
	c.l1[h] = e
}

// ChainKeyStore tries each store in order, returning the first success.
type ChainKeyStore []KeyStore

func (c ChainKeyStore) Lookup(ctx context.Context, apiKey string) (Principal, error) {
	lastErr := ErrInvalidAPIKey
	for _, s := range c {
		p, err := s.Lookup(ctx, apiKey)
		if err == nil {
			return p, nil
		}
		lastErr = err
	}
	return Principal{}, lastErr
}
