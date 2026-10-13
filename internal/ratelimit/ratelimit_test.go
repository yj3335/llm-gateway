package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis uses a real Redis when LLMGW_TEST_REDIS_ADDR is set (CI and
// `make test`), otherwise an in-process miniredis with Lua support.
func newTestRedis(t *testing.T) (redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	if addr := os.Getenv("LLMGW_TEST_REDIS_ADDR"); addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		if err := rdb.Ping(context.Background()).Err(); err != nil {
			t.Fatalf("LLMGW_TEST_REDIS_ADDR set but unreachable: %v", err)
		}
		return rdb, nil
	}
	mr := miniredis.RunT(t)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func tenant(t *testing.T) string {
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

func TestRPMExhaustion(t *testing.T) {
	rdb, _ := newTestRedis(t)
	l := NewRedisLimiter(rdb, time.Second)
	lim := Limits{RPM: 5, TPM: 1_000_000}
	id := tenant(t)
	for i := 0; i < 5; i++ {
		r, err := l.Allow(context.Background(), id, lim, 10)
		if err != nil || !r.Allowed {
			t.Fatalf("request %d: %+v %v", i, r, err)
		}
		if r.RemainingRequests != int64(4-i) {
			t.Fatalf("request %d remaining=%d", i, r.RemainingRequests)
		}
	}
	r, _ := l.Allow(context.Background(), id, lim, 10)
	if r.Allowed {
		t.Fatal("6th request should be limited")
	}
	// One request refills every 12s at 5 RPM.
	if r.RetryAfter < 11*time.Second || r.RetryAfter > 12*time.Second {
		t.Fatalf("unexpected retry-after %v", r.RetryAfter)
	}
	if r.Degraded {
		t.Fatal("should not be degraded")
	}
}

func TestTPMExhaustionAndAtomicity(t *testing.T) {
	rdb, _ := newTestRedis(t)
	l := NewRedisLimiter(rdb, time.Second)
	lim := Limits{RPM: 100, TPM: 1000}
	id := tenant(t)
	for i := 0; i < 2; i++ {
		if r, _ := l.Allow(context.Background(), id, lim, 400); !r.Allowed {
			t.Fatalf("request %d denied: %+v", i, r)
		}
	}
	r, _ := l.Allow(context.Background(), id, lim, 400)
	if r.Allowed {
		t.Fatal("TPM should be exhausted")
	}
	// Denied request must not have consumed an RPM token (dual-bucket atomicity).
	if r.RemainingRequests != 98 {
		t.Fatalf("denied request debited RPM: remaining=%d", r.RemainingRequests)
	}
	if r.RemainingTokens < 200 || r.RemainingTokens > 201 {
		t.Fatalf("remaining tokens=%d", r.RemainingTokens)
	}
	// A cheaper request still fits.
	if r, _ := l.Allow(context.Background(), id, lim, 150); !r.Allowed {
		t.Fatalf("cheap request denied: %+v", r)
	}
}

func TestUnsatisfiable(t *testing.T) {
	rdb, _ := newTestRedis(t)
	l := NewRedisLimiter(rdb, time.Second)
	r, err := l.Allow(context.Background(), tenant(t), Limits{RPM: 10, TPM: 100}, 101)
	if err != nil || r.Allowed || !r.Unsatisfiable {
		t.Fatalf("expected unsatisfiable: %+v %v", r, err)
	}
}

func TestAdjustRefundsTokens(t *testing.T) {
	rdb, _ := newTestRedis(t)
	l := NewRedisLimiter(rdb, time.Second)
	lim := Limits{RPM: 100, TPM: 1000}
	id := tenant(t)
	if r, _ := l.Allow(context.Background(), id, lim, 900); !r.Allowed {
		t.Fatal("initial request denied")
	}
	if r, _ := l.Allow(context.Background(), id, lim, 500); r.Allowed {
		t.Fatal("should be denied before refund")
	}
	// Request was a cache hit: refund the full estimate.
	if err := l.Adjust(context.Background(), id, lim, -900); err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Allow(context.Background(), id, lim, 500); !r.Allowed {
		t.Fatalf("should be allowed after refund: %+v", r)
	}
}

func TestRefillOverTime(t *testing.T) {
	// Always miniredis: deterministic time travel of the Redis TIME clock.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.SetTime(time.Now())
	l := NewRedisLimiter(rdb, time.Second)
	lim := Limits{RPM: 60, TPM: 60_000} // 1 req/s refill
	id := tenant(t)
	for i := 0; i < 60; i++ {
		if r, _ := l.Allow(context.Background(), id, lim, 1); !r.Allowed {
			t.Fatalf("request %d denied", i)
		}
	}
	if r, _ := l.Allow(context.Background(), id, lim, 1); r.Allowed {
		t.Fatal("bucket should be empty")
	}
	mr.SetTime(time.Now().Add(3 * time.Second))
	for i := 0; i < 3; i++ {
		if r, _ := l.Allow(context.Background(), id, lim, 1); !r.Allowed {
			t.Fatalf("refilled request %d denied: %+v", i, r)
		}
	}
	if r, _ := l.Allow(context.Background(), id, lim, 1); r.Allowed {
		t.Fatal("over-refilled bucket")
	}
}

func TestConcurrentAdmissionIsExact(t *testing.T) {
	rdb, _ := newTestRedis(t)
	l := NewRedisLimiter(rdb, 2*time.Second)
	lim := Limits{RPM: 50, TPM: 10_000_000}
	id := tenant(t)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := l.Allow(context.Background(), id, lim, 5); err == nil && r.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	// The bucket holds 50 tokens; at most floor(elapsed * 50/60s) more can
	// refill while the goroutines run (0 on a fast machine, maybe 1-2 on a
	// slow CI runner). Anything outside that window means a lost or
	// double-spent token.
	refill := int64(time.Since(start).Seconds() * 50 / 60)
	if got := allowed.Load(); got < 50 || got > 50+refill {
		t.Fatalf("expected 50..%d admissions under contention, got %d", 50+refill, got)
	}
}

func TestFallbackWhenRedisDown(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	l := NewRedisLimiter(rdb, 100*time.Millisecond)
	var fallbacks atomic.Int64
	l.OnFallback = func(error) { fallbacks.Add(1) }
	lim := Limits{RPM: 3, TPM: 1000}
	for i := 0; i < 3; i++ {
		r, err := l.Allow(context.Background(), "t", lim, 1)
		if err != nil || !r.Allowed || !r.Degraded {
			t.Fatalf("fallback request %d: %+v %v", i, r, err)
		}
	}
	r, _ := l.Allow(context.Background(), "t", lim, 1)
	if r.Allowed || !r.Degraded {
		t.Fatalf("fallback limiter must still enforce quotas: %+v", r)
	}
	if fallbacks.Load() == 0 {
		t.Fatal("OnFallback never invoked")
	}
	h := http.Header{}
	r.SetHeaders(h)
	if h.Get("X-RateLimit-Mode") != "local-fallback" || h.Get("Retry-After") == "" {
		t.Fatalf("headers: %v", h)
	}
}

func TestRecoversAfterRedisReturns(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	rdb := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	l := NewRedisLimiter(rdb, 100*time.Millisecond)
	l.backoff = 50 * time.Millisecond
	lim := Limits{RPM: 100, TPM: 1000}

	mr.Close()
	if r, _ := l.Allow(context.Background(), "t", lim, 1); !r.Degraded {
		t.Fatal("expected degraded while Redis is down")
	}
	if err := mr.StartAddr(addr); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if r, err := l.Allow(context.Background(), "t", lim, 1); err != nil || r.Degraded {
		t.Fatalf("expected Redis path after recovery: %+v %v", r, err)
	}
}

func TestHeaders(t *testing.T) {
	h := http.Header{}
	Result{Allowed: true, Limits: Limits{RPM: 60, TPM: 1000}, RemainingRequests: 59, RemainingTokens: 900, Reset: 1500 * time.Millisecond}.SetHeaders(h)
	want := map[string]string{
		"X-RateLimit-Limit-Requests": "60", "X-RateLimit-Remaining-Requests": "59",
		"X-RateLimit-Limit-Tokens": "1000", "X-RateLimit-Remaining-Tokens": "900", "X-RateLimit-Reset": "2",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Fatalf("%s=%q want %q", k, h.Get(k), v)
		}
	}
	if h.Get("Retry-After") != "" {
		t.Fatal("Retry-After set on allowed request")
	}
}

func TestMemoryLimiterRefill(t *testing.T) {
	m := NewMemoryLimiter()
	now := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return now }
	lim := Limits{RPM: 2, TPM: 100}
	for i := 0; i < 2; i++ {
		if r, _ := m.Allow(context.Background(), "x", lim, 10); !r.Allowed {
			t.Fatal("denied")
		}
	}
	r, _ := m.Allow(context.Background(), "x", lim, 10)
	if r.Allowed || r.RetryAfter != 30*time.Second {
		t.Fatalf("expected 30s retry, got %+v", r)
	}
	now = now.Add(30 * time.Second)
	if r, _ := m.Allow(context.Background(), "x", lim, 10); !r.Allowed {
		t.Fatalf("expected refill: %+v", r)
	}
	if r, _ := m.Allow(context.Background(), "x", lim, 1000); !r.Unsatisfiable {
		t.Fatal("expected unsatisfiable")
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(401, 256); got != 101+256 {
		t.Fatalf("got %d", got)
	}
}

func BenchmarkRedisAllow(b *testing.B) {
	mr := miniredis.RunT(b)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if addr := os.Getenv("LLMGW_TEST_REDIS_ADDR"); addr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: addr})
	}
	l := NewRedisLimiter(rdb, time.Second)
	lim := Limits{RPM: 1 << 40, TPM: 1 << 50}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = l.Allow(context.Background(), "bench", lim, 100)
		}
	})
}
