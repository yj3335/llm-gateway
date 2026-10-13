package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yj3335/llm-gateway/internal/mockllm"
)

// ----------------------------------------------------------- breaker ---

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type transitions struct {
	mu  sync.Mutex
	log []string
}

func (tr *transitions) record(_ string, from, to State) {
	tr.mu.Lock()
	tr.log = append(tr.log, from.String()+"->"+to.String())
	tr.mu.Unlock()
}

func newTestBreaker(cfg BreakerConfig) (*Breaker, *fakeClock, *transitions) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	tr := &transitions{}
	b := NewBreaker("p", cfg, tr.record)
	b.now = clk.now
	return b, clk, tr
}

func fail(t *testing.T, b *Breaker) {
	t.Helper()
	tk, err := b.Allow()
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	b.Done(tk, false)
}

func succeed(t *testing.T, b *Breaker) {
	t.Helper()
	tk, err := b.Allow()
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	b.Done(tk, true)
}

func TestBreakerStateMachine(t *testing.T) {
	b, clk, tr := newTestBreaker(BreakerConfig{FailureThreshold: 3, FailureRateWindow: 100, OpenTimeout: 10 * time.Second, HalfOpenMaxRequests: 1, HalfOpenSuccesses: 2})

	// CLOSED: a success resets the consecutive-failure count.
	fail(t, b)
	fail(t, b)
	succeed(t, b)
	fail(t, b)
	fail(t, b)
	if b.State() != StateClosed {
		t.Fatal("should still be CLOSED")
	}
	fail(t, b) // 3rd consecutive
	if b.State() != StateOpen {
		t.Fatalf("expected OPEN, got %s", b.State())
	}
	// OPEN: calls rejected until the timeout elapses.
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("OPEN breaker admitted a call")
	}
	clk.advance(9 * time.Second)
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("admitted before open timeout")
	}
	clk.advance(time.Second)

	// HALF-OPEN: one probe at a time.
	probe, err := b.Allow()
	if err != nil || b.State() != StateHalfOpen {
		t.Fatalf("expected HALF-OPEN probe, got %v %s", err, b.State())
	}
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("second concurrent probe admitted")
	}
	b.Done(probe, false) // probe fails -> OPEN again
	if b.State() != StateOpen {
		t.Fatalf("failed probe should re-open, got %s", b.State())
	}
	clk.advance(10 * time.Second)
	succeed(t, b)
	if b.State() != StateHalfOpen {
		t.Fatal("needs 2 probe successes to close")
	}
	succeed(t, b)
	if b.State() != StateClosed {
		t.Fatalf("expected CLOSED, got %s", b.State())
	}
	want := "CLOSED->OPEN,OPEN->HALF-OPEN,HALF-OPEN->OPEN,OPEN->HALF-OPEN,HALF-OPEN->CLOSED"
	if got := strings.Join(tr.log, ","); got != want {
		t.Fatalf("transitions\n got %s\nwant %s", got, want)
	}
}

func TestBreakerFailureRateTrip(t *testing.T) {
	b, _, _ := newTestBreaker(BreakerConfig{FailureRateWindow: 10, MinRequests: 10, FailureRate: 0.5, OpenTimeout: time.Second})
	for i := 0; i < 10; i++ { // alternate: never 2 consecutive failures
		if i%2 == 1 {
			fail(t, b)
		} else {
			succeed(t, b)
		}
	}
	if b.State() != StateOpen {
		t.Fatalf("50%% failure rate over window should trip, got %s", b.State())
	}
}

// Fast failures that complete ahead of slow in-flight successes must not
// trip the breaker when the true failure rate is low.
func TestBreakerNoFalseTripOnFastFailures(t *testing.T) {
	// Consecutive rule enabled too: it must also be immune to reordering.
	b, _, _ := newTestBreaker(BreakerConfig{FailureRateWindow: 20, MinRequests: 3, FailureRate: 0.5, FailureThreshold: 3, OpenTimeout: time.Second})
	var tickets []Ticket
	for i := 0; i < 50; i++ { // 50 concurrent calls admitted
		tk, err := b.Allow()
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, tk)
	}
	failing := map[int]bool{3: true, 13: true, 23: true, 33: true, 43: true}
	for i := range failing { // 10% fail fast and report before anything else
		b.Done(tickets[i], false)
	}
	if b.State() != StateClosed {
		t.Fatal("early fast failures tripped the breaker")
	}
	for i := 0; i < 50; i++ {
		if !failing[i] {
			b.Done(tickets[i], true)
		}
	}
	if b.State() != StateClosed {
		t.Fatalf("10%% failure rate must not trip, got %s", b.State())
	}
	for i := 0; i < 20; i++ { // a real outage still trips
		fail(t, b)
		if b.State() == StateOpen {
			return
		}
	}
	t.Fatal("total outage did not trip the breaker")
}

func TestBreakerIgnoresStaleTickets(t *testing.T) {
	b, clk, _ := newTestBreaker(BreakerConfig{FailureThreshold: 1, OpenTimeout: time.Second, HalfOpenSuccesses: 1})
	stale, _ := b.Allow()   // issued while CLOSED
	b.ForceState(StateOpen) // generation changes while the call is in flight
	clk.advance(time.Second)
	probe, _ := b.Allow() // HALF-OPEN
	b.Done(stale, false)  // late failure from old generation must be ignored
	if b.State() != StateHalfOpen {
		t.Fatalf("stale ticket changed state to %s", b.State())
	}
	b.Done(probe, true)
	if b.State() != StateClosed {
		t.Fatalf("expected CLOSED, got %s", b.State())
	}
}

func TestBreakerConcurrency(t *testing.T) {
	b := NewBreaker("p", BreakerConfig{FailureThreshold: 50, OpenTimeout: time.Millisecond, HalfOpenMaxRequests: 4}, nil)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if tk, err := b.Allow(); err == nil {
					b.Done(tk, (i+g)%3 != 0)
				}
				_ = b.State()
			}
		}(g)
	}
	wg.Wait()
}

// ----------------------------------------------------------- providers ---

func mockServer(t *testing.T, cfg mockllm.Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mockllm.New(cfg))
	t.Cleanup(srv.Close)
	return srv
}

func fastMock() mockllm.Config {
	return mockllm.Config{TTFT: 5 * time.Millisecond, TPOT: 0, Tokens: 12, FailStatus: 503}
}

func provider(t *testing.T, name, typ, base string, query map[string]string, timeout time.Duration) Provider {
	t.Helper()
	p, err := NewProvider(ProviderSpec{Name: name, Type: typ, BaseURL: base, Query: query, Timeout: timeout, DefaultModel: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func chatReq(prompt string) *ChatRequest {
	return &ChatRequest{Model: "gpt-4o-mini", Messages: []ChatMessage{{Role: "system", Content: "Be brief."}, {Role: "user", Content: prompt}}}
}

func drain(t *testing.T, s Stream) (string, string, *Usage) {
	t.Helper()
	defer s.Close()
	var sb strings.Builder
	var finish string
	var usage *Usage
	for {
		d, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return sb.String(), finish, usage
		}
		if err != nil {
			t.Fatalf("stream recv: %v", err)
		}
		sb.WriteString(d.Content)
		if d.FinishReason != "" {
			finish = d.FinishReason
		}
		if d.Usage != nil {
			usage = d.Usage
		}
	}
}

func TestProviderWireFormats(t *testing.T) {
	srv := mockServer(t, fastMock())
	prompt := "Explain how Raft leader election works."
	want := strings.Join(mockllm.Completion(prompt, 12), " ")
	for _, typ := range []string{"openai", "anthropic", "gemini", "ollama", "mockllm"} {
		t.Run(typ, func(t *testing.T) {
			p := provider(t, typ, typ, srv.URL, nil, 5*time.Second)
			c, err := p.Complete(context.Background(), chatReq(prompt), CallOptions{})
			if err != nil {
				t.Fatalf("complete: %v", err)
			}
			if c.Content != want || c.FinishReason != "stop" || c.Usage.CompletionTokens != 12 || c.Usage.PromptTokens == 0 {
				t.Fatalf("complete mismatch: %+v", c)
			}
			s, err := p.Stream(context.Background(), chatReq(prompt), CallOptions{})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			got, finish, usage := drain(t, s)
			if got != want || finish != "stop" {
				t.Fatalf("stream mismatch:\n got %q (%s)\nwant %q", got, finish, want)
			}
			if usage == nil || usage.CompletionTokens != 12 {
				t.Fatalf("stream usage missing: %+v", usage)
			}
		})
	}
}

func TestMessageContentParts(t *testing.T) {
	var m ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url"},{"type":"text","text":"world"}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "hello\nworld" {
		t.Fatalf("got %q", m.Content)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":42}`), &m); err == nil {
		t.Fatal("expected error for numeric content")
	}
}

// ------------------------------------------------------------ failover ---

func newRouter(t *testing.T, providers []Provider, cfg BreakerConfig) *Router {
	t.Helper()
	var targets []Target
	for _, p := range providers {
		targets = append(targets, Target{Provider: p.Name()})
	}
	r, err := New(providers, []Route{{Match: "*", Targets: targets}}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFailoverOn5xxAndBreakerOpens(t *testing.T) {
	srv := mockServer(t, fastMock())
	primary := provider(t, "openai", "openai", srv.URL, map[string]string{"fail_rate": "1"}, 5*time.Second)
	secondary := provider(t, "anthropic", "anthropic", srv.URL, nil, 5*time.Second)
	r := newRouter(t, []Provider{primary, secondary}, BreakerConfig{FailureThreshold: 3, OpenTimeout: time.Hour})

	for i := 0; i < 3; i++ {
		c, out, err := r.Complete(context.Background(), chatReq("q"), CallOptions{}, Chaos{})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !out.Failover || out.Provider != "anthropic" || len(out.Attempts) != 2 || out.Attempts[0].Status != 503 || c.Content == "" {
			t.Fatalf("request %d outcome: %+v", i, out)
		}
	}
	if st := r.Breaker("openai").State(); st != StateOpen {
		t.Fatalf("primary breaker should be OPEN after 3 failures, got %s", st)
	}
	// Open breaker: primary skipped without an upstream call.
	_, out, err := r.Complete(context.Background(), chatReq("q"), CallOptions{}, Chaos{})
	if err != nil || !out.Failover || !out.Attempts[0].Skipped {
		t.Fatalf("expected skip of open primary: %+v %v", out, err)
	}
	h := r.Health()
	if h[0].State != "OPEN" || h[0].Failures != 3 || h[0].Rejected != 1 || h[1].Requests != 4 {
		t.Fatalf("health: %+v", h)
	}
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	srv := mockServer(t, fastMock())
	primary := provider(t, "openai", "openai", srv.URL, nil, 5*time.Second)
	secondary := provider(t, "gemini", "gemini", srv.URL, nil, 5*time.Second)
	r := newRouter(t, []Provider{primary, secondary}, BreakerConfig{FailureThreshold: 2, OpenTimeout: 50 * time.Millisecond, HalfOpenSuccesses: 1})

	for i := 0; i < 2; i++ {
		if _, out, err := r.Complete(context.Background(), chatReq("q"), CallOptions{}, Chaos{FailPrimary: true}); err != nil || out.Provider != "gemini" {
			t.Fatalf("chaos failover: %+v %v", out, err)
		}
	}
	if r.Breaker("openai").State() != StateOpen {
		t.Fatal("expected OPEN")
	}
	time.Sleep(60 * time.Millisecond)
	if r.Breaker("openai").State() != StateHalfOpen {
		t.Fatal("expected HALF-OPEN after timeout")
	}
	_, out, err := r.Complete(context.Background(), chatReq("q"), CallOptions{}, Chaos{})
	if err != nil || out.Provider != "openai" || out.Failover {
		t.Fatalf("probe should be served by recovered primary: %+v %v", out, err)
	}
	if r.Breaker("openai").State() != StateClosed {
		t.Fatal("expected CLOSED after successful probe")
	}
}

func TestFailoverOnTimeout(t *testing.T) {
	slow := mockServer(t, mockllm.Config{TTFT: 400 * time.Millisecond, Tokens: 4})
	fast := mockServer(t, fastMock())
	r := newRouter(t, []Provider{
		provider(t, "ollama", "ollama", slow.URL, nil, 50*time.Millisecond),
		provider(t, "openai", "openai", fast.URL, nil, 5*time.Second),
	}, BreakerConfig{})
	start := time.Now()
	_, out, err := r.Complete(context.Background(), chatReq("q"), CallOptions{}, Chaos{})
	if err != nil || out.Provider != "openai" || !out.Failover {
		t.Fatalf("timeout failover: %+v %v", out, err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("timeout not enforced")
	}
}

func TestNoFailoverOnClientError(t *testing.T) {
	srv := mockServer(t, fastMock())
	r := newRouter(t, []Provider{
		provider(t, "openai", "openai", srv.URL, nil, 5*time.Second),
		provider(t, "anthropic", "anthropic", srv.URL, nil, 5*time.Second),
	}, BreakerConfig{FailureThreshold: 1})
	hdr := http.Header{"X-Mock-Fail": {"400"}}
	_, out, err := r.Complete(context.Background(), chatReq("q"), CallOptions{Headers: hdr}, Chaos{})
	var af *AllFailedError
	if !errors.As(err, &af) || af.StatusCode() != 400 || len(out.Attempts) != 1 {
		t.Fatalf("expected single 400 attempt: %+v %v", out, err)
	}
	if r.Breaker("openai").State() != StateClosed {
		t.Fatal("4xx must not trip the breaker")
	}
}

func TestAllProvidersDown(t *testing.T) {
	srv := mockServer(t, mockllm.Config{TTFT: time.Millisecond, Tokens: 4, FailRate: 1, FailStatus: 503})
	r := newRouter(t, []Provider{
		provider(t, "openai", "openai", srv.URL, nil, time.Second),
		provider(t, "anthropic", "anthropic", srv.URL, nil, time.Second),
	}, BreakerConfig{})
	_, out, err := r.Complete(context.Background(), chatReq("q"), CallOptions{}, Chaos{})
	var af *AllFailedError
	if !errors.As(err, &af) || af.StatusCode() != http.StatusBadGateway || len(out.Attempts) != 2 {
		t.Fatalf("expected 502 after 2 attempts: %+v %v", out, err)
	}
}

func TestStreamFailover(t *testing.T) {
	srv := mockServer(t, fastMock())
	r := newRouter(t, []Provider{
		provider(t, "openai", "openai", srv.URL, nil, 5*time.Second),
		provider(t, "anthropic", "anthropic", srv.URL, nil, 5*time.Second),
	}, BreakerConfig{})
	s, out, err := r.Stream(context.Background(), chatReq("stream me"), CallOptions{}, Chaos{FailPrimary: true})
	if err != nil || out.Provider != "anthropic" || !out.Failover {
		t.Fatalf("stream failover: %+v %v", out, err)
	}
	got, _, _ := drain(t, s)
	if got != strings.Join(mockllm.Completion("stream me", 12), " ") {
		t.Fatalf("stream content %q", got)
	}
}

func TestRouteMatching(t *testing.T) {
	srv := mockServer(t, fastMock())
	a := provider(t, "openai", "openai", srv.URL, nil, time.Second)
	b := provider(t, "anthropic", "anthropic", srv.URL, nil, time.Second)
	r, err := New([]Provider{a, b}, []Route{
		{Match: "claude-*", Targets: []Target{{Provider: "anthropic"}, {Provider: "openai", Model: "gpt-4o-mini"}}},
		{Match: "gpt-4o", Targets: []Target{{Provider: "openai"}}},
	}, BreakerConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts, _ := r.Resolve("claude-3-5-haiku"); ts[0].Provider != "anthropic" || ts[1].Model != "gpt-4o-mini" {
		t.Fatalf("prefix route: %+v", ts)
	}
	if _, err := r.Resolve("gemini-pro"); err == nil {
		t.Fatal("expected no route")
	}
	if _, err := New([]Provider{a}, []Route{{Match: "*", Targets: []Target{{Provider: "nope"}}}}, BreakerConfig{}, nil); err == nil {
		t.Fatal("expected unknown provider error")
	}
}
