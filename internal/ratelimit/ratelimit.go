// Package ratelimit implements a distributed dual token-bucket limiter
// (requests-per-minute and tokens-per-minute) executed atomically inside
// Redis via a Lua script, with a local in-memory fallback so the gateway
// degrades gracefully when Redis is unreachable.
package ratelimit

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Limits are per-tenant quotas, both expressed per minute.
type Limits struct {
	RPM int64
	TPM int64
}

// Result is the outcome of a limiter decision.
type Result struct {
	Allowed           bool
	Limits            Limits
	RemainingRequests int64
	RemainingTokens   int64
	// RetryAfter is how long until the request would be admitted (0 if allowed).
	RetryAfter time.Duration
	// Reset is the time until both buckets are full again.
	Reset time.Duration
	// Unsatisfiable is set when the token cost exceeds the TPM bucket capacity.
	Unsatisfiable bool
	// Degraded is set when the decision came from the local fallback limiter.
	Degraded bool
}

// Limiter admits or rejects a request costing `tokens` estimated tokens.
type Limiter interface {
	Allow(ctx context.Context, tenant string, l Limits, tokens int64) (Result, error)
	// Adjust credits (delta<0) or debits (delta>0) the TPM bucket after the
	// actual token usage is known, without consuming a request.
	Adjust(ctx context.Context, tenant string, l Limits, delta int64) error
}

// rates in tokens per millisecond.
func perMs(perMinute int64) float64 { return float64(perMinute) / 60000.0 }

// SetHeaders writes the standard rate-limit response headers.
func (r Result) SetHeaders(h http.Header) {
	h.Set("X-RateLimit-Limit-Requests", strconv.FormatInt(r.Limits.RPM, 10))
	h.Set("X-RateLimit-Remaining-Requests", strconv.FormatInt(max(r.RemainingRequests, 0), 10))
	h.Set("X-RateLimit-Limit-Tokens", strconv.FormatInt(r.Limits.TPM, 10))
	h.Set("X-RateLimit-Remaining-Tokens", strconv.FormatInt(max(r.RemainingTokens, 0), 10))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(ceilSeconds(r.Reset), 10))
	if r.Degraded {
		h.Set("X-RateLimit-Mode", "local-fallback")
	}
	if !r.Allowed && !r.Unsatisfiable {
		h.Set("Retry-After", strconv.FormatInt(max(ceilSeconds(r.RetryAfter), 1), 10))
	}
}

func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds()))
}

// EstimateTokens approximates prompt tokens from character count (~4 chars
// per token for English BPE vocabularies) plus requested completion tokens.
func EstimateTokens(promptChars int, maxCompletionTokens int64) int64 {
	return int64(math.Ceil(float64(promptChars)/4.0)) + maxCompletionTokens
}
