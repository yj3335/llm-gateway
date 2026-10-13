package ratelimit

import (
	"context"
	"hash/fnv"
	"math"
	"sync"
	"time"
)

const memShards = 64

// MemoryLimiter is a process-local dual token bucket with the same semantics
// as the Redis script. It backs RedisLimiter during outages and can be used
// standalone for single-replica deployments.
type MemoryLimiter struct {
	shards [memShards]memShard
	now    func() time.Time
}

type memShard struct {
	mu      sync.Mutex
	buckets map[string]*dualBucket
}

type dualBucket struct {
	rpm, tpm float64
	ts       time.Time
}

// NewMemoryLimiter returns an empty local limiter.
func NewMemoryLimiter() *MemoryLimiter {
	m := &MemoryLimiter{now: time.Now}
	for i := range m.shards {
		m.shards[i].buckets = map[string]*dualBucket{}
	}
	return m
}

func (m *MemoryLimiter) shard(tenant string) *memShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tenant))
	return &m.shards[h.Sum32()%memShards]
}

func (m *MemoryLimiter) refill(s *memShard, tenant string, lim Limits) *dualBucket {
	now := m.now()
	b, ok := s.buckets[tenant]
	if !ok {
		b = &dualBucket{rpm: float64(lim.RPM), tpm: float64(lim.TPM), ts: now}
		s.buckets[tenant] = b
		return b
	}
	elapsed := float64(now.Sub(b.ts).Milliseconds())
	if elapsed > 0 {
		b.rpm = math.Min(float64(lim.RPM), b.rpm+elapsed*perMs(lim.RPM))
		b.tpm = math.Min(float64(lim.TPM), b.tpm+elapsed*perMs(lim.TPM))
		b.ts = now
	}
	return b
}

// Allow implements Limiter.
func (m *MemoryLimiter) Allow(_ context.Context, tenant string, lim Limits, tokens int64) (Result, error) {
	s := m.shard(tenant)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := m.refill(s, tenant, lim)
	r := Result{Limits: lim}
	if tokens > lim.TPM {
		r.Unsatisfiable = true
		r.RemainingRequests, r.RemainingTokens = int64(b.rpm), int64(b.tpm)
		return r, nil
	}
	cost := float64(tokens)
	var wait float64
	if b.rpm < 1 {
		wait = math.Max(wait, math.Ceil((1-b.rpm)/perMs(lim.RPM)))
	}
	if b.tpm < cost {
		wait = math.Max(wait, math.Ceil((cost-b.tpm)/perMs(lim.TPM)))
	}
	if wait == 0 {
		b.rpm--
		b.tpm -= cost
		r.Allowed = true
	} else {
		r.RetryAfter = time.Duration(wait) * time.Millisecond
	}
	r.RemainingRequests, r.RemainingTokens = int64(math.Floor(b.rpm)), int64(math.Floor(b.tpm))
	reset := math.Max(math.Ceil((float64(lim.RPM)-b.rpm)/perMs(lim.RPM)), math.Ceil((float64(lim.TPM)-b.tpm)/perMs(lim.TPM)))
	r.Reset = time.Duration(reset) * time.Millisecond
	return r, nil
}

// Adjust implements Limiter.
func (m *MemoryLimiter) Adjust(_ context.Context, tenant string, lim Limits, delta int64) error {
	s := m.shard(tenant)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := m.refill(s, tenant, lim)
	b.tpm = math.Min(float64(lim.TPM), b.tpm-float64(delta))
	return nil
}
