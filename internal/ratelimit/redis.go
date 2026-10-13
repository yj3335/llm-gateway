package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// dualBucketScript atomically refills and checks two token buckets and only
// debits either bucket if BOTH have sufficient capacity. Time is read from the
// Redis server clock (TIME) so all gateway replicas share one time source.
//
// KEYS[1] = rpm bucket hash, KEYS[2] = tpm bucket hash
// ARGV    = rpm_capacity, tpm_capacity, rpm_cost, tpm_cost, mode
//
//	mode "check" = admit/deny, mode "adjust" = unconditional TPM delta
//
// Returns { allowed, rpm_remaining, tpm_remaining, retry_after_ms, reset_ms, unsatisfiable }
const dualBucketScript = `
local rpm_cap  = tonumber(ARGV[1])
local tpm_cap  = tonumber(ARGV[2])
local rpm_cost = tonumber(ARGV[3])
local tpm_cost = tonumber(ARGV[4])
local mode     = ARGV[5]

local t   = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local rpm_rate = rpm_cap / 60000.0
local tpm_rate = tpm_cap / 60000.0

local function load(key, cap, rate)
  local v = redis.call('HMGET', key, 'tokens', 'ts')
  local tokens = tonumber(v[1])
  local ts = tonumber(v[2])
  if tokens == nil or ts == nil then
    return cap
  end
  local elapsed = now - ts
  if elapsed < 0 then elapsed = 0 end
  local refilled = tokens + elapsed * rate
  if refilled > cap then refilled = cap end
  return refilled
end

local function save(key, tokens, cap, rate)
  redis.call('HSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
  local ttl = math.ceil((cap - tokens) / rate) + 1000
  if ttl < 1000 then ttl = 1000 end
  redis.call('PEXPIRE', key, ttl)
end

local rpm = load(KEYS[1], rpm_cap, rpm_rate)
local tpm = load(KEYS[2], tpm_cap, tpm_rate)

if mode == 'adjust' then
  tpm = tpm - tpm_cost
  if tpm > tpm_cap then tpm = tpm_cap end
  save(KEYS[2], tpm, tpm_cap, tpm_rate)
  return {1, math.floor(rpm), math.floor(tpm), 0, 0, 0}
end

if tpm_cost > tpm_cap or rpm_cost > rpm_cap then
  return {0, math.floor(rpm), math.floor(tpm), -1, 0, 1}
end

local allowed = 1
local retry = 0
if rpm < rpm_cost then
  allowed = 0
  local w = math.ceil((rpm_cost - rpm) / rpm_rate)
  if w > retry then retry = w end
end
if tpm < tpm_cost then
  allowed = 0
  local w = math.ceil((tpm_cost - tpm) / tpm_rate)
  if w > retry then retry = w end
end

if allowed == 1 then
  rpm = rpm - rpm_cost
  tpm = tpm - tpm_cost
  save(KEYS[1], rpm, rpm_cap, rpm_rate)
  save(KEYS[2], tpm, tpm_cap, tpm_rate)
end

local reset_rpm = math.ceil((rpm_cap - rpm) / rpm_rate)
local reset_tpm = math.ceil((tpm_cap - tpm) / tpm_rate)
local reset = reset_rpm
if reset_tpm > reset then reset = reset_tpm end

return {allowed, math.floor(rpm), math.floor(tpm), retry, reset, 0}
`

var script = redis.NewScript(dualBucketScript)

// RedisLimiter is the distributed limiter. When Redis errors it transparently
// falls back to a local in-memory limiter and retries Redis after a backoff,
// so an outage never takes the gateway down.
type RedisLimiter struct {
	rdb       redis.UniversalClient
	prefix    string
	timeout   time.Duration
	fallback  *MemoryLimiter
	backoff   time.Duration
	downUntil atomic.Int64 // unix nanos; while in the future, skip Redis

	// OnFallback is invoked (if set) whenever a decision is served locally.
	OnFallback func(err error)
}

// NewRedisLimiter creates a limiter using rdb. opTimeout bounds each script call.
func NewRedisLimiter(rdb redis.UniversalClient, opTimeout time.Duration) *RedisLimiter {
	if opTimeout <= 0 {
		opTimeout = 250 * time.Millisecond
	}
	return &RedisLimiter{rdb: rdb, prefix: "llmgw:rl:", timeout: opTimeout, fallback: NewMemoryLimiter(), backoff: time.Second}
}

func (l *RedisLimiter) keys(tenant string) []string {
	// Hash tag keeps both buckets on the same Redis Cluster slot.
	return []string{l.prefix + "{" + tenant + "}:rpm", l.prefix + "{" + tenant + "}:tpm"}
}

func (l *RedisLimiter) redisAvailable() bool {
	return time.Now().UnixNano() >= l.downUntil.Load()
}

func (l *RedisLimiter) markDown(err error) {
	l.downUntil.Store(time.Now().Add(l.backoff).UnixNano())
	if l.OnFallback != nil {
		l.OnFallback(err)
	}
}

// Allow implements Limiter.
func (l *RedisLimiter) Allow(ctx context.Context, tenant string, lim Limits, tokens int64) (Result, error) {
	if !l.redisAvailable() {
		r, err := l.fallback.Allow(ctx, tenant, lim, tokens)
		r.Degraded = true
		if l.OnFallback != nil {
			l.OnFallback(nil)
		}
		return r, err
	}
	res, err := l.run(ctx, tenant, lim, 1, tokens, "check")
	if err != nil {
		l.markDown(err)
		r, ferr := l.fallback.Allow(ctx, tenant, lim, tokens)
		r.Degraded = true
		return r, ferr
	}
	return res, nil
}

// Adjust implements Limiter.
func (l *RedisLimiter) Adjust(ctx context.Context, tenant string, lim Limits, delta int64) error {
	if delta == 0 {
		return nil
	}
	if !l.redisAvailable() {
		return l.fallback.Adjust(ctx, tenant, lim, delta)
	}
	if _, err := l.run(ctx, tenant, lim, 0, delta, "adjust"); err != nil {
		l.markDown(err)
		return l.fallback.Adjust(ctx, tenant, lim, delta)
	}
	return nil
}

func (l *RedisLimiter) run(ctx context.Context, tenant string, lim Limits, rpmCost, tpmCost int64, mode string) (Result, error) {
	if lim.RPM <= 0 || lim.TPM <= 0 {
		return Result{}, errors.New("ratelimit: limits must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	raw, err := script.Run(ctx, l.rdb, l.keys(tenant), lim.RPM, lim.TPM, rpmCost, tpmCost, mode).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit script: %w", err)
	}
	if len(raw) != 6 {
		return Result{}, fmt.Errorf("ratelimit script: unexpected reply %v", raw)
	}
	r := Result{
		Allowed:           raw[0] == 1,
		Limits:            lim,
		RemainingRequests: raw[1],
		RemainingTokens:   raw[2],
		Reset:             time.Duration(raw[4]) * time.Millisecond,
		Unsatisfiable:     raw[5] == 1,
	}
	if raw[3] > 0 {
		r.RetryAfter = time.Duration(raw[3]) * time.Millisecond
	}
	return r, nil
}
