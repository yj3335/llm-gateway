package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Target is one hop in a route's failover chain.
type Target struct {
	Provider string
	Model    string // empty = pass the client's model through
}

// Route maps a model pattern ("gpt-4o", "gpt-*", "*") to a failover chain.
type Route struct {
	Match   string
	Targets []Target
}

func (r Route) matches(model string) bool {
	switch {
	case r.Match == "*":
		return true
	case strings.HasSuffix(r.Match, "*"):
		return strings.HasPrefix(model, strings.TrimSuffix(r.Match, "*"))
	default:
		return r.Match == model
	}
}

// Attempt records one upstream try.
type Attempt struct {
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Status   int           `json:"status,omitempty"`
	Error    string        `json:"error,omitempty"`
	Skipped  bool          `json:"skipped,omitempty"` // breaker open
	Latency  time.Duration `json:"latency_ns"`
}

// Outcome summarizes a routed call.
type Outcome struct {
	Provider        string
	Model           string
	Failover        bool // served by a provider other than the primary
	Attempts        []Attempt
	UpstreamLatency time.Duration // latency of the successful attempt
}

// Chaos requests synthetic upstream failures (dev/benchmark only).
type Chaos struct {
	// FailPrimary makes the first target return HTTP 503 by forwarding
	// X-Mock-Fail: 503 (honoured by cmd/mockllm).
	FailPrimary bool
}

// Router dispatches requests to providers with circuit breaking and failover.
type Router struct {
	providers map[string]Provider
	breakers  map[string]*Breaker
	routes    []Route
	order     []string

	mu    sync.Mutex
	stats map[string]*providerStats
}

type providerStats struct {
	requests, failures, rejected int64
	ewmaMs                       float64
	lastError                    string
	lastErrorAt                  time.Time
	samples                      []float64 // ring of recent latencies (ms)
	pos                          int
}

const latencyRing = 512

// New builds a router. onChange observes breaker state transitions.
func New(providers []Provider, routes []Route, cfg BreakerConfig, onChange func(provider string, from, to State)) (*Router, error) {
	r := &Router{providers: map[string]Provider{}, breakers: map[string]*Breaker{}, stats: map[string]*providerStats{}}
	for _, p := range providers {
		if _, dup := r.providers[p.Name()]; dup {
			return nil, fmt.Errorf("duplicate provider %q", p.Name())
		}
		r.providers[p.Name()] = p
		r.breakers[p.Name()] = NewBreaker(p.Name(), cfg, onChange)
		r.stats[p.Name()] = &providerStats{}
		r.order = append(r.order, p.Name())
	}
	for _, rt := range routes {
		for _, t := range rt.Targets {
			if _, ok := r.providers[t.Provider]; !ok {
				return nil, fmt.Errorf("route %q: unknown provider %q", rt.Match, t.Provider)
			}
		}
	}
	r.routes = routes
	return r, nil
}

// Breaker returns the breaker for a provider (nil if unknown).
func (r *Router) Breaker(name string) *Breaker { return r.breakers[name] }

// Resolve returns the failover chain for model.
func (r *Router) Resolve(model string) ([]Target, error) {
	for _, rt := range r.routes {
		if rt.matches(model) {
			return rt.Targets, nil
		}
	}
	return nil, fmt.Errorf("no route for model %q", model)
}

// ErrNoRoute is wrapped when no route matches the model.
var ErrNoRoute = errors.New("no route")

// AllFailedError is returned when every target failed.
type AllFailedError struct {
	Attempts []Attempt
	Last     error
}

func (e *AllFailedError) Error() string {
	return fmt.Sprintf("all %d upstream providers failed; last error: %v", len(e.Attempts), e.Last)
}

func (e *AllFailedError) Unwrap() error { return e.Last }

// StatusCode maps the terminal failure to an HTTP status for the client.
func (e *AllFailedError) StatusCode() int {
	var ue *UpstreamError
	if errors.As(e.Last, &ue) {
		if ue.Timeout {
			return http.StatusGatewayTimeout
		}
		if ue.Status >= 400 && ue.Status < 500 && ue.Status != http.StatusTooManyRequests {
			return ue.Status
		}
	}
	return http.StatusBadGateway
}

func (r *Router) prepare(t Target, req *ChatRequest) *ChatRequest {
	cp := *req
	if t.Model != "" {
		cp.Model = t.Model
	}
	return &cp
}

func (r *Router) opts(i int, base CallOptions, chaos Chaos) CallOptions {
	if i == 0 && chaos.FailPrimary {
		h := base.Headers.Clone()
		if h == nil {
			h = http.Header{}
		}
		h.Set("X-Mock-Fail", "503")
		return CallOptions{Headers: h}
	}
	return base
}

// Complete performs a non-streaming completion with failover.
func (r *Router) Complete(ctx context.Context, req *ChatRequest, base CallOptions, chaos Chaos) (*Completion, Outcome, error) {
	targets, err := r.Resolve(req.Model)
	if err != nil {
		return nil, Outcome{}, fmt.Errorf("%w: %v", ErrNoRoute, err)
	}
	var out Outcome
	var last error
	for i, t := range targets {
		if ctx.Err() != nil {
			return nil, out, ctx.Err()
		}
		p := r.providers[t.Provider]
		br := r.breakers[t.Provider]
		ticket, berr := br.Allow()
		if berr != nil {
			r.reject(t.Provider)
			out.Attempts = append(out.Attempts, Attempt{Provider: t.Provider, Model: t.Model, Skipped: true, Error: berr.Error()})
			last = fmt.Errorf("%s: %w", t.Provider, berr)
			continue
		}
		preq := r.prepare(t, req)
		start := time.Now()
		c, cerr := p.Complete(ctx, preq, r.opts(i, base, chaos))
		lat := time.Since(start)
		if cerr == nil {
			br.Done(ticket, true)
			r.observe(t.Provider, lat, nil)
			out.Attempts = append(out.Attempts, Attempt{Provider: t.Provider, Model: preq.Model, Status: 200, Latency: lat})
			out.Provider, out.Model, out.UpstreamLatency = t.Provider, preq.Model, lat
			out.Failover = i > 0
			if c.Model == "" {
				c.Model = preq.Model
			}
			return c, out, nil
		}
		retry := IsRetryable(cerr)
		// Client-side 4xx must not trip the breaker.
		br.Done(ticket, !retry)
		r.observe(t.Provider, lat, cerr)
		out.Attempts = append(out.Attempts, attemptFor(t.Provider, preq.Model, lat, cerr))
		last = cerr
		if !retry || errors.Is(cerr, context.Canceled) {
			break
		}
	}
	return nil, out, &AllFailedError{Attempts: out.Attempts, Last: last}
}

// Stream opens a streaming completion with failover. Failover happens only
// before the first byte is relayed to the client: once the upstream returns
// 200 the stream is committed.
func (r *Router) Stream(ctx context.Context, req *ChatRequest, base CallOptions, chaos Chaos) (Stream, Outcome, error) {
	targets, err := r.Resolve(req.Model)
	if err != nil {
		return nil, Outcome{}, fmt.Errorf("%w: %v", ErrNoRoute, err)
	}
	var out Outcome
	var last error
	for i, t := range targets {
		if ctx.Err() != nil {
			return nil, out, ctx.Err()
		}
		p := r.providers[t.Provider]
		br := r.breakers[t.Provider]
		ticket, berr := br.Allow()
		if berr != nil {
			r.reject(t.Provider)
			out.Attempts = append(out.Attempts, Attempt{Provider: t.Provider, Model: t.Model, Skipped: true, Error: berr.Error()})
			last = fmt.Errorf("%s: %w", t.Provider, berr)
			continue
		}
		preq := r.prepare(t, req)
		start := time.Now()
		s, serr := p.Stream(ctx, preq, r.opts(i, base, chaos))
		lat := time.Since(start)
		if serr == nil {
			out.Attempts = append(out.Attempts, Attempt{Provider: t.Provider, Model: preq.Model, Status: 200, Latency: lat})
			out.Provider, out.Model, out.UpstreamLatency = t.Provider, preq.Model, lat
			out.Failover = i > 0
			return &trackedStream{Stream: s, r: r, br: br, ticket: ticket, provider: t.Provider, start: start}, out, nil
		}
		retry := IsRetryable(serr)
		br.Done(ticket, !retry)
		r.observe(t.Provider, lat, serr)
		out.Attempts = append(out.Attempts, attemptFor(t.Provider, preq.Model, lat, serr))
		last = serr
		if !retry || errors.Is(serr, context.Canceled) {
			break
		}
	}
	return nil, out, &AllFailedError{Attempts: out.Attempts, Last: last}
}

func attemptFor(provider, model string, lat time.Duration, err error) Attempt {
	a := Attempt{Provider: provider, Model: model, Latency: lat, Error: err.Error()}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		a.Status = ue.Status
	}
	return a
}

// trackedStream reports the breaker outcome when the stream terminates.
type trackedStream struct {
	Stream
	r        *Router
	br       *Breaker
	ticket   Ticket
	provider string
	start    time.Time
	once     sync.Once
}

func (t *trackedStream) finish(err error) {
	t.once.Do(func() {
		ok := err == nil || errors.Is(err, io.EOF)
		t.br.Done(t.ticket, ok)
		if ok {
			err = nil
		}
		t.r.observe(t.provider, time.Since(t.start), err)
	})
}

func (t *trackedStream) Recv() (Delta, error) {
	d, err := t.Stream.Recv()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Client went away: neither success nor upstream failure.
			t.once.Do(func() { t.br.Done(t.ticket, true) })
		} else {
			t.finish(err)
		}
	}
	return d, err
}

func (t *trackedStream) Close() error {
	t.finish(nil)
	return t.Stream.Close()
}

func (r *Router) reject(provider string) {
	r.mu.Lock()
	r.stats[provider].rejected++
	r.mu.Unlock()
}

func (r *Router) observe(provider string, lat time.Duration, err error) {
	ms := float64(lat.Microseconds()) / 1000
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats[provider]
	s.requests++
	if err != nil {
		s.failures++
		s.lastError = err.Error()
		s.lastErrorAt = time.Now()
		return
	}
	if s.ewmaMs == 0 {
		s.ewmaMs = ms
	} else {
		s.ewmaMs = 0.8*s.ewmaMs + 0.2*ms
	}
	if len(s.samples) < latencyRing {
		s.samples = append(s.samples, ms)
	} else {
		s.samples[s.pos] = ms
		s.pos = (s.pos + 1) % latencyRing
	}
}

// ProviderHealth is a snapshot for dashboards.
type ProviderHealth struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	State        string  `json:"state"`
	Requests     int64   `json:"requests"`
	Failures     int64   `json:"failures"`
	Rejected     int64   `json:"rejected"`
	ErrorRate    float64 `json:"error_rate"`
	LatencyEWMA  float64 `json:"latency_ewma_ms"`
	LatencyP50   float64 `json:"latency_p50_ms"`
	LatencyP95   float64 `json:"latency_p95_ms"`
	LastError    string  `json:"last_error,omitempty"`
	LastErrorAgo float64 `json:"last_error_ago_s,omitempty"`
}

// Health returns per-provider health in configuration order.
func (r *Router) Health() []ProviderHealth {
	out := make([]ProviderHealth, 0, len(r.order))
	for _, name := range r.order {
		state := r.breakers[name].State().String()
		r.mu.Lock()
		s := r.stats[name]
		h := ProviderHealth{
			Name: name, Type: r.providers[name].Type(), State: state,
			Requests: s.requests, Failures: s.failures, Rejected: s.rejected,
			LatencyEWMA: round2(s.ewmaMs), LastError: s.lastError,
		}
		if s.requests > 0 {
			h.ErrorRate = round2(float64(s.failures) / float64(s.requests))
		}
		if !s.lastErrorAt.IsZero() {
			h.LastErrorAgo = round2(time.Since(s.lastErrorAt).Seconds())
		}
		samples := append([]float64(nil), s.samples...)
		r.mu.Unlock()
		if len(samples) > 0 {
			sort.Float64s(samples)
			h.LatencyP50 = round2(samples[len(samples)/2])
			h.LatencyP95 = round2(samples[int(math.Min(float64(len(samples)-1), float64(len(samples))*0.95))])
		}
		out = append(out, h)
	}
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
