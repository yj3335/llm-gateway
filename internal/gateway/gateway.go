// Package gateway wires authentication, rate limiting, guardrails, the
// semantic cache, and the multi-provider router into the HTTP request
// pipeline served by cmd/gateway.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yj3335/llm-gateway/internal/auth"
	"github.com/yj3335/llm-gateway/internal/cache"
	"github.com/yj3335/llm-gateway/internal/config"
	"github.com/yj3335/llm-gateway/internal/guardrails"
	"github.com/yj3335/llm-gateway/internal/ratelimit"
	"github.com/yj3335/llm-gateway/internal/router"
	"github.com/yj3335/llm-gateway/internal/telemetry"
)

// Gateway is the request pipeline.
type Gateway struct {
	Cfg       config.Config
	Auth      *auth.Authenticator
	Limiter   ratelimit.Limiter // nil disables rate limiting
	Guard     *guardrails.Engine
	Cache     *cache.Cache // nil disables caching
	Router    *router.Router
	Metrics   *telemetry.Metrics
	Collector *telemetry.Collector
	Audit     *telemetry.Auditor
	Ready     func(ctx context.Context) map[string]string

	tokenHandler http.Handler
	sf           singleflight.Group
}

// chatRequest extends the OpenAI body with stream_options.
type chatRequest struct {
	router.ChatRequest
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// Exposed headers (CORS) for the control-plane playground.
var exposedHeaders = strings.Join([]string{
	"X-Request-ID", "X-Gateway-Cache", "X-Gateway-Cache-Similarity", "X-Gateway-Cache-Layer", "X-Gateway-Cache-Lookup-Ms",
	"X-Gateway-Provider", "X-Gateway-Provider-Failover", "X-Gateway-Upstream-Model", "X-Gateway-Attempts",
	"X-Gateway-Upstream-Latency-Ms", "X-Gateway-Overhead-Ms", "X-Gateway-Total-Latency-Ms",
	"X-Gateway-Guardrail", "X-Gateway-Guardrail-Categories", "X-Gateway-Tokens-Saved", "X-Gateway-Coalesced",
	"X-RateLimit-Limit-Requests", "X-RateLimit-Remaining-Requests", "X-RateLimit-Limit-Tokens",
	"X-RateLimit-Remaining-Tokens", "X-RateLimit-Reset", "X-RateLimit-Mode", "Retry-After",
}, ", ")

// Handler returns the root HTTP handler.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	authMW := g.Auth.Middleware(func(w http.ResponseWriter, _ *http.Request, err error) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_credentials", err.Error())
	})
	mux.Handle("POST /v1/chat/completions", authMW(http.HandlerFunc(g.chat)))
	mux.HandleFunc("GET /v1/models", g.models)
	if g.tokenHandler != nil {
		mux.Handle("POST /v1/auth/token", g.tokenHandler)
	}
	mux.HandleFunc("GET /v1/telemetry/summary", g.Collector.SummaryHandler())
	mux.HandleFunc("GET /v1/telemetry/stream", g.Collector.StreamHandler(g.Cfg.Telemetry.StreamInterval))
	mux.Handle("GET /metrics", promhttp.HandlerFor(g.Metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", g.readyz)
	return g.cors(recoverer(mux))
}

func (g *Gateway) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	wildcard := false
	for _, o := range g.Cfg.Server.CORSOrigins {
		if o == "*" {
			wildcard = true
		}
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && (wildcard || allowed[origin]) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", exposedHeaders)
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Admin-Key, X-Gateway-Chaos, X-Gateway-Cache-Bypass, Cache-Control")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				writeError(w, http.StatusInternalServerError, "internal_error", "panic", fmt.Sprint(v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) readyz(w http.ResponseWriter, r *http.Request) {
	status := map[string]string{"gateway": "ok"}
	if g.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		for k, v := range g.Ready(ctx) {
			status[k] = v
		}
		cancel()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (g *Gateway) models(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		OwnedBy string   `json:"owned_by"`
		Chain   []string `json:"failover_chain"`
	}
	var data []model
	for _, rt := range g.Cfg.Routes {
		var chain []string
		for _, t := range rt.Targets {
			chain = append(chain, t.Provider)
		}
		data = append(data, model{ID: rt.Match, Object: "model", OwnedBy: chain[0], Chain: chain})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// requestState accumulates per-request telemetry.
type requestState struct {
	start     time.Time
	trace     telemetry.Trace
	upstream  time.Duration
	principal auth.Principal
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	st := &requestState{start: time.Now()}
	p, _ := auth.FromContext(r.Context())
	st.principal = p
	id := r.Header.Get("X-Request-ID")
	if id == "" || len(id) > 64 {
		id = newRequestID()
	}
	w.Header().Set("X-Request-ID", id)
	// CacheStatus NONE = request never reached the cache stage (rejected earlier).
	st.trace = telemetry.Trace{ID: id, TenantID: p.TenantID, CacheStatus: "NONE", GuardrailAction: string(guardrails.ActionNone)}

	g.Collector.ConnOpened()
	g.Metrics.ActiveConnections.Inc()
	defer func() {
		g.Collector.ConnClosed()
		g.Metrics.ActiveConnections.Dec()
		g.finish(st)
	}()

	// ---- decode & validate -------------------------------------------------
	var req chatRequest
	body := http.MaxBytesReader(w, r.Body, g.Cfg.Server.MaxBodyBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		g.fail(w, st, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be valid JSON: "+err.Error())
		return
	}
	st.trace.Model = req.Model
	st.trace.Stream = req.Stream
	if req.Model == "" || len(req.Messages) == 0 {
		g.fail(w, st, http.StatusBadRequest, "invalid_request_error", "missing_fields", "model and messages are required")
		return
	}
	promptChars := 0
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool", "developer":
		default:
			g.fail(w, st, http.StatusBadRequest, "invalid_request_error", "invalid_role", "unsupported message role "+strconv.Quote(m.Role))
			return
		}
		promptChars += len(m.Content)
	}
	maxTokens := g.Cfg.RateLimit.DefaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = int64(*req.MaxTokens)
	}

	// ---- rate limit (RPM + TPM, atomic) -------------------------------------
	estimate := ratelimit.EstimateTokens(promptChars, maxTokens)
	limits := ratelimit.Limits{RPM: p.RPMLimit, TPM: p.TPMLimit}
	if g.Limiter != nil {
		res, err := g.Limiter.Allow(r.Context(), p.TenantID, limits, estimate)
		if err == nil {
			res.SetHeaders(w.Header())
			if res.Degraded {
				g.Metrics.RateLimitDegraded.Inc()
				g.Collector.RateLimitDegraded()
			}
			if !res.Allowed {
				reason, msg := "rate_limit", fmt.Sprintf("rate limit exceeded for tenant %s; retry after %ds", p.TenantID, int(res.RetryAfter.Seconds()+0.999))
				if res.Unsatisfiable {
					reason, msg = "tpm_capacity", fmt.Sprintf("request needs ~%d tokens which exceeds the tenant TPM limit of %d", estimate, p.TPMLimit)
				} else if res.RemainingRequests >= 1 {
					reason = "tpm"
				} else {
					reason = "rpm"
				}
				g.Metrics.RateLimited.WithLabelValues(reason).Inc()
				g.fail(w, st, http.StatusTooManyRequests, "rate_limit_exceeded", reason, msg)
				return
			}
		}
	}
	refund := func(actualTokens int64) {
		if g.Limiter != nil {
			_ = g.Limiter.Adjust(context.WithoutCancel(r.Context()), p.TenantID, limits, actualTokens-estimate)
		}
	}

	// ---- input guardrails -----------------------------------------------------
	gmsgs := make([]guardrails.Message, len(req.Messages))
	for i, m := range req.Messages {
		gmsgs[i] = guardrails.Message{Role: m.Role, Content: m.Content}
	}
	if g.Cfg.Guardrails.Enabled {
		dec := g.Guard.InspectInput(gmsgs, guardrails.Mode(p.PIIMode))
		g.Metrics.GuardrailLatency.WithLabelValues("input").Observe(dec.Elapsed.Seconds())
		injection := dec.Code == guardrails.CodePromptInjection || (dec.Action == guardrails.ActionFlag && len(dec.PII) == 0)
		g.recordGuardrail(st, "input", dec.Action, dec.PII, dec.Injection, injection)
		if dec.Action != guardrails.ActionNone {
			w.Header().Set("X-Gateway-Guardrail", string(dec.Action))
			w.Header().Set("X-Gateway-Guardrail-Categories", strings.Join(st.trace.GuardrailTypes, ","))
		}
		if dec.Action == guardrails.ActionBlock {
			refund(0)
			g.fail(w, st, http.StatusBadRequest, "policy_violation", dec.Code, dec.Reason)
			return
		}
		for i := range req.Messages {
			req.Messages[i].Content = dec.Messages[i].Content
			gmsgs[i].Content = dec.Messages[i].Content
		}
	}

	// ---- cache lookup -----------------------------------------------------------
	cmsgs := make([]cache.Message, len(req.Messages))
	for i, m := range req.Messages {
		cmsgs[i] = cache.Message{Role: m.Role, Content: m.Content}
	}
	key := cache.BuildKey(p.TenantID, req.Model, cmsgs)
	bypass := g.Cache == nil || r.Header.Get("X-Gateway-Cache-Bypass") == "true" ||
		strings.Contains(r.Header.Get("Cache-Control"), "no-cache") || strings.Contains(r.Header.Get("Cache-Control"), "no-store")
	if !bypass {
		res := g.Cache.Lookup(r.Context(), &key)
		w.Header().Set("X-Gateway-Cache-Lookup-Ms", fmt.Sprintf("%.3f", ms(res.Elapsed)))
		st.trace.CacheStatus = string(res.Status)
		g.Metrics.CacheLookups.WithLabelValues(string(res.Status)).Inc()
		if res.Entry != nil {
			g.serveCached(w, st, &req, res)
			refund(0)
			return
		}
	} else {
		st.trace.CacheStatus = string(cache.StatusBypass)
		g.Metrics.CacheLookups.WithLabelValues(string(cache.StatusBypass)).Inc()
	}
	w.Header().Set("X-Gateway-Cache", st.trace.CacheStatus)

	// ---- upstream ------------------------------------------------------------------
	chaos := router.Chaos{}
	if g.Cfg.Server.EnableChaosHeaders && r.Header.Get("X-Gateway-Chaos") == "fail-primary" {
		chaos.FailPrimary = true
	}
	if req.Stream {
		used := g.stream(w, r, st, &req, key, bypass, chaos)
		refund(int64(used))
		return
	}
	used := g.complete(w, r, st, &req, key, bypass, chaos)
	refund(int64(used))
}

type flightResult struct {
	completion *router.Completion
	outcome    router.Outcome
	content    string // after output guardrails
	outPII     map[guardrails.Category]int
}

// complete handles non-streaming upstream calls with request coalescing:
// concurrent identical misses share one upstream call.
func (g *Gateway) complete(w http.ResponseWriter, r *http.Request, st *requestState, req *chatRequest, key cache.Key, bypass bool, chaos router.Chaos) int {
	call := func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
		defer cancel()
		c, out, err := g.Router.Complete(ctx, &req.ChatRequest, router.CallOptions{}, chaos)
		if err != nil {
			return &flightResult{outcome: out}, err
		}
		fr := &flightResult{completion: c, outcome: out, content: c.Content}
		if g.Cfg.Guardrails.Enabled && g.Cfg.Guardrails.ScanOutput {
			od := g.Guard.InspectOutput(c.Content)
			g.Metrics.GuardrailLatency.WithLabelValues("output").Observe(od.Elapsed.Seconds())
			fr.content, fr.outPII = od.Content, od.PII
		}
		if !bypass && c.FinishReason != "content_filter" {
			g.Cache.Store(key, &cache.Entry{
				Content: fr.content, Model: c.Model, Provider: out.Provider, FinishReason: c.FinishReason,
				PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens,
			})
		}
		return fr, nil
	}

	waitStart := time.Now()
	var v any
	var err error
	var shared bool
	if bypass || chaos.FailPrimary {
		v, err = call()
	} else {
		v, err, shared = g.sf.Do(key.ExactHash, call)
	}
	wait := time.Since(waitStart)
	fr := v.(*flightResult)
	g.observeOutcome(st, fr.outcome, wait)
	if shared {
		w.Header().Set("X-Gateway-Coalesced", "true")
	}
	if err != nil {
		g.upstreamError(w, st, err)
		return 0
	}
	c := fr.completion
	if len(fr.outPII) > 0 {
		g.recordGuardrail(st, "output", guardrails.ActionRedact, fr.outPII, guardrails.InjectionResult{}, false)
	}
	st.trace.PromptTokens, st.trace.CompletionTokens = c.Usage.PromptTokens, c.Usage.CompletionTokens
	g.Metrics.TokensProcessed.WithLabelValues(fr.outcome.Provider, "prompt").Add(float64(c.Usage.PromptTokens))
	g.Metrics.TokensProcessed.WithLabelValues(fr.outcome.Provider, "completion").Add(float64(c.Usage.CompletionTokens))

	resp := router.ChatResponse{
		ID: "chatcmpl-" + st.trace.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []router.Choice{{Index: 0, Message: router.ChatMessage{Role: "assistant", Content: fr.content}, FinishReason: c.FinishReason}},
		Usage:   c.Usage,
	}
	g.setTimingHeaders(w, st)
	w.Header().Set("Content-Type", "application/json")
	st.trace.Status = http.StatusOK
	_ = json.NewEncoder(w).Encode(resp)
	return c.Usage.TotalTokens
}

func (g *Gateway) stream(w http.ResponseWriter, r *http.Request, st *requestState, req *chatRequest, key cache.Key, bypass bool, chaos router.Chaos) int {
	flusher, ok := w.(http.Flusher)
	if !ok {
		g.fail(w, st, http.StatusInternalServerError, "internal_error", "streaming_unsupported", "response writer cannot stream")
		return 0
	}
	upStart := time.Now()
	s, out, err := g.Router.Stream(r.Context(), &req.ChatRequest, router.CallOptions{}, chaos)
	if err != nil {
		g.observeOutcome(st, out, time.Since(upStart))
		g.upstreamError(w, st, err)
		return 0
	}
	defer s.Close()
	g.setProviderHeaders(w, out)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Gateway-Overhead-Ms", fmt.Sprintf("%.3f", ms(upStart.Sub(st.start))))
	w.Header().Set("X-Gateway-Upstream-Latency-Ms", fmt.Sprintf("%.3f", ms(out.UpstreamLatency)))
	w.WriteHeader(http.StatusOK)
	st.trace.Status = http.StatusOK

	sw := newSSEWriter(w, flusher, "chatcmpl-"+st.trace.ID, req.Model)
	sw.role()
	var redactor *guardrails.StreamRedactor
	if g.Cfg.Guardrails.Enabled && g.Cfg.Guardrails.ScanOutput {
		redactor = &guardrails.StreamRedactor{}
	}
	var full strings.Builder
	var finish string
	var usage *router.Usage
	var streamErr error
	for {
		d, err := s.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
		if d.FinishReason != "" {
			finish = d.FinishReason
		}
		if d.Usage != nil {
			usage = d.Usage
		}
		if d.Content == "" {
			continue
		}
		text := d.Content
		if redactor != nil {
			text = redactor.Push(d.Content)
		}
		if text != "" {
			full.WriteString(text)
			if !sw.content(text) {
				streamErr = r.Context().Err()
				break
			}
		}
	}
	if redactor != nil {
		if tail := redactor.Flush(); tail != "" {
			full.WriteString(tail)
			sw.content(tail)
		}
		if len(redactor.PII) > 0 {
			g.recordGuardrail(st, "output", guardrails.ActionRedact, redactor.PII, guardrails.InjectionResult{}, false)
		}
	}
	upstreamDur := time.Since(upStart)
	g.observeOutcome(st, out, 0)
	// For streams the upstream owns the whole token-generation window.
	st.upstream = upstreamDur

	if streamErr != nil {
		st.trace.Error = streamErr.Error()
		sw.errorEvent(streamErr.Error())
		sw.done()
		return 0
	}
	if finish == "" {
		finish = "stop"
	}
	content := full.String()
	if usage == nil {
		pt := 0
		for _, m := range req.Messages {
			pt += router.EstimateTokens(m.Content)
		}
		ct := router.EstimateTokens(content)
		usage = &router.Usage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct}
	}
	sw.finish(finish)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		sw.usage(*usage)
	}
	sw.done()
	st.trace.PromptTokens, st.trace.CompletionTokens = usage.PromptTokens, usage.CompletionTokens
	g.Metrics.TokensProcessed.WithLabelValues(out.Provider, "prompt").Add(float64(usage.PromptTokens))
	g.Metrics.TokensProcessed.WithLabelValues(out.Provider, "completion").Add(float64(usage.CompletionTokens))
	if !bypass && finish != "content_filter" {
		g.Cache.Store(key, &cache.Entry{
			Content: content, Model: out.Model, Provider: out.Provider, FinishReason: finish,
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		})
	}
	return usage.TotalTokens
}

func (g *Gateway) serveCached(w http.ResponseWriter, st *requestState, req *chatRequest, res cache.Result) {
	e := res.Entry
	saved := e.PromptTokens + e.CompletionTokens
	price := g.Cfg.PriceFor(req.Model)
	cost := (float64(e.PromptTokens)*price.Input + float64(e.CompletionTokens)*price.Output) / 1000
	st.trace.Provider = "cache"
	st.trace.Similarity = round4(res.Similarity)
	st.trace.TokensSaved, st.trace.CostSavedUSD = saved, cost
	g.Metrics.TokensSaved.Add(float64(saved))
	g.Metrics.CostSavedUSD.Add(cost)
	if res.Status == cache.StatusHitSemantic {
		g.Metrics.CacheSimilarity.Observe(res.Similarity)
		w.Header().Set("X-Gateway-Cache-Similarity", strconv.FormatFloat(res.Similarity, 'f', 4, 64))
	}
	h := w.Header()
	h.Set("X-Gateway-Cache", string(res.Status))
	h.Set("X-Gateway-Cache-Layer", res.Layer)
	h.Set("X-Gateway-Provider", "cache")
	h.Set("X-Gateway-Provider-Failover", "false")
	h.Set("X-Gateway-Upstream-Model", e.Model)
	h.Set("X-Gateway-Tokens-Saved", strconv.Itoa(saved))
	usage := router.Usage{PromptTokens: e.PromptTokens, CompletionTokens: e.CompletionTokens, TotalTokens: saved}
	st.trace.Status = http.StatusOK
	if req.Stream {
		flusher, ok := w.(http.Flusher)
		g.setTimingHeaders(w, st)
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if !ok {
			return
		}
		sw := newSSEWriter(w, flusher, "chatcmpl-"+st.trace.ID, req.Model)
		sw.role()
		words := strings.SplitAfter(e.Content, " ")
		for i := 0; i < len(words); i += 8 {
			if !sw.content(strings.Join(words[i:min(i+8, len(words))], "")) {
				return
			}
		}
		sw.finish(orStop(e.FinishReason))
		if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			sw.usage(usage)
		}
		sw.done()
		return
	}
	resp := router.ChatResponse{
		ID: "chatcmpl-" + st.trace.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []router.Choice{{Index: 0, Message: router.ChatMessage{Role: "assistant", Content: e.Content}, FinishReason: orStop(e.FinishReason)}},
		Usage:   usage,
	}
	g.setTimingHeaders(w, st)
	h.Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func orStop(s string) string {
	if s == "" {
		return "stop"
	}
	return s
}

func (g *Gateway) observeOutcome(st *requestState, out router.Outcome, wait time.Duration) {
	var upstream time.Duration
	for _, a := range out.Attempts {
		upstream += a.Latency
		outcome := "success"
		switch {
		case a.Skipped:
			outcome = "circuit_open"
		case a.Error != "":
			outcome = "error"
		}
		g.Metrics.UpstreamRequests.WithLabelValues(a.Provider, outcome).Inc()
		if !a.Skipped {
			g.Metrics.UpstreamLatency.WithLabelValues(a.Provider).Observe(a.Latency.Seconds())
		}
	}
	if wait > 0 && (upstream == 0 || wait < upstream) {
		upstream = wait // coalesced follower joined an in-flight call
	}
	st.upstream = upstream
	st.trace.Provider = out.Provider
	st.trace.Failover = out.Failover
	if out.Failover && len(out.Attempts) > 0 {
		g.Metrics.Failovers.WithLabelValues(out.Attempts[0].Provider, out.Provider).Inc()
	}
}

func (g *Gateway) setProviderHeaders(w http.ResponseWriter, out router.Outcome) {
	h := w.Header()
	h.Set("X-Gateway-Provider", out.Provider)
	h.Set("X-Gateway-Provider-Failover", strconv.FormatBool(out.Failover))
	h.Set("X-Gateway-Upstream-Model", out.Model)
	h.Set("X-Gateway-Attempts", strconv.Itoa(len(out.Attempts)))
}

func (g *Gateway) setTimingHeaders(w http.ResponseWriter, st *requestState) {
	total := time.Since(st.start)
	h := w.Header()
	if st.trace.Provider != "" && st.trace.Provider != "cache" {
		h.Set("X-Gateway-Provider", st.trace.Provider)
		h.Set("X-Gateway-Provider-Failover", strconv.FormatBool(st.trace.Failover))
	}
	h.Set("X-Gateway-Upstream-Latency-Ms", fmt.Sprintf("%.3f", ms(st.upstream)))
	h.Set("X-Gateway-Overhead-Ms", fmt.Sprintf("%.3f", ms(max(total-st.upstream, 0))))
	h.Set("X-Gateway-Total-Latency-Ms", fmt.Sprintf("%.3f", ms(total)))
}

func (g *Gateway) upstreamError(w http.ResponseWriter, st *requestState, err error) {
	var af *router.AllFailedError
	switch {
	case errors.Is(err, router.ErrNoRoute):
		g.fail(w, st, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error())
	case errors.As(err, &af):
		w.Header().Set("X-Gateway-Attempts", strconv.Itoa(len(af.Attempts)))
		g.fail(w, st, af.StatusCode(), "upstream_error", "all_providers_failed", err.Error())
	default:
		g.fail(w, st, http.StatusBadGateway, "upstream_error", "upstream_failure", err.Error())
	}
}

func (g *Gateway) recordGuardrail(st *requestState, direction string, action guardrails.Action, pii map[guardrails.Category]int, inj guardrails.InjectionResult, injection bool) {
	if action == guardrails.ActionNone {
		return
	}
	cats := make([]string, 0, len(pii)+1)
	if injection {
		cats = append(cats, string(guardrails.CatPromptInjection))
	}
	for c := range pii {
		cats = append(cats, string(c))
	}
	sort.Strings(cats)
	for _, c := range cats {
		g.Metrics.GuardrailTriggers.WithLabelValues(c, string(action)).Inc()
		score := 0.0
		if c == string(guardrails.CatPromptInjection) {
			score = inj.Score
		}
		detail := map[string]any{}
		if n, ok := pii[guardrails.Category(c)]; ok {
			detail["count"] = n
		}
		if len(inj.Signals) > 0 && c == string(guardrails.CatPromptInjection) {
			detail["signals"] = inj.Signals
		}
		g.Audit.Guardrail(telemetry.GuardrailEvent{
			RequestID: st.trace.ID, TenantID: st.principal.TenantID, Direction: direction,
			Category: c, Action: string(action), Score: score, Detail: detail,
		})
	}
	// Input decision takes precedence in the trace; output redactions append.
	if direction == "input" || st.trace.GuardrailAction == string(guardrails.ActionNone) {
		st.trace.GuardrailAction = string(action)
	}
	st.trace.GuardrailTypes = appendUnique(st.trace.GuardrailTypes, cats...)
	if inj.Score > st.trace.InjectionScore {
		st.trace.InjectionScore = round4(inj.Score)
	}
}

func appendUnique(dst []string, vals ...string) []string {
	for _, v := range vals {
		found := false
		for _, d := range dst {
			if d == v {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, v)
		}
	}
	return dst
}

func (g *Gateway) fail(w http.ResponseWriter, st *requestState, status int, typ, code, msg string) {
	st.trace.Status = status
	st.trace.Error = code
	g.setTimingHeaders(w, st)
	writeError(w, status, typ, code, msg)
}

func (g *Gateway) finish(st *requestState) {
	total := time.Since(st.start)
	overhead := max(total-st.upstream, 0)
	t := &st.trace
	t.Timestamp = st.start.UTC().Format(time.RFC3339Nano)
	t.LatencyMs = round3(ms(total))
	t.OverheadMs = round3(ms(overhead))
	t.UpstreamMs = round3(ms(st.upstream))
	if t.Status == 0 {
		t.Status = 499 // client closed before a response was written
	}
	g.Metrics.Requests.WithLabelValues(t.Model, t.CacheStatus, strconv.Itoa(t.Status)).Inc()
	if t.Status < 400 {
		g.Metrics.RequestDuration.WithLabelValues(t.CacheStatus).Observe(total.Seconds())
		g.Metrics.GatewayOverhead.WithLabelValues(t.CacheStatus).Observe(overhead.Seconds())
	}
	g.Collector.Record(*t)
	g.Audit.Request(*t)
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }
func round4(f float64) float64 { return float64(int64(f*10000+0.5)) / 10000 }

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

// sseWriter emits OpenAI-format chat.completion.chunk events.
type sseWriter struct {
	w       io.Writer
	f       http.Flusher
	id      string
	model   string
	created int64
	failed  bool
}

func newSSEWriter(w io.Writer, f http.Flusher, id, model string) *sseWriter {
	return &sseWriter{w: w, f: f, id: id, model: model, created: time.Now().Unix()}
}

func (s *sseWriter) send(v any) bool {
	if s.failed {
		return false
	}
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		s.failed = true
		return false
	}
	s.f.Flush()
	return true
}

func (s *sseWriter) chunk(delta router.StreamDelta, finish *string) router.StreamChunk {
	return router.StreamChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []router.StreamChoice{{Index: 0, Delta: delta, FinishReason: finish}}}
}

func (s *sseWriter) role() bool { return s.send(s.chunk(router.StreamDelta{Role: "assistant"}, nil)) }
func (s *sseWriter) content(text string) bool {
	return s.send(s.chunk(router.StreamDelta{Content: text}, nil))
}
func (s *sseWriter) finish(reason string) bool { return s.send(s.chunk(router.StreamDelta{}, &reason)) }
func (s *sseWriter) usage(u router.Usage) bool {
	return s.send(router.StreamChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []router.StreamChoice{}, Usage: &u})
}
func (s *sseWriter) errorEvent(msg string) bool {
	return s.send(map[string]any{"error": map[string]string{"message": msg, "type": "upstream_error"}})
}
func (s *sseWriter) done() {
	if s.failed {
		return
	}
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.f.Flush()
}
