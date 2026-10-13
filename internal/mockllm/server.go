// Package mockllm is a realistic mock upstream that speaks the OpenAI Chat
// Completions, Anthropic Messages, and Gemini generateContent wire formats
// (JSON and SSE streaming) with configurable latency and chaos injection.
//
// Per-request knobs (query string):
//
//	ttft_ms     time to first token (default from Config)
//	tpot_ms     time per output token
//	tokens      completion length in tokens
//	fail_rate   probability [0,1] of returning fail_status
//	fail_status status code for injected failures (default 503)
//
// Header X-Mock-Fail: <status> forces an immediate failure with that status.
package mockllm

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yj3335/llm-gateway/internal/cache"
)

// Config sets server-wide defaults.
type Config struct {
	TTFT       time.Duration
	TPOT       time.Duration
	Tokens     int
	FailRate   float64
	FailStatus int
	// Jitter adds uniform ±Jitter fraction to TTFT (e.g. 0.2 = ±20%).
	Jitter float64
}

// DefaultConfig approximates a fast hosted model.
func DefaultConfig() Config {
	return Config{TTFT: 120 * time.Millisecond, TPOT: time.Millisecond, Tokens: 48, FailStatus: http.StatusServiceUnavailable, Jitter: 0.15}
}

// Stats are cumulative counters.
type Stats struct {
	Requests       int64 `json:"requests"`
	InjectedFaults int64 `json:"injected_faults"`
	Streams        int64 `json:"streams"`
	Embeddings     int64 `json:"embeddings"`
	TokensOut      int64 `json:"tokens_out"`
}

// Server is the mock upstream http.Handler.
type Server struct {
	cfg      Config
	mux      *http.ServeMux
	embedder *cache.HashingEmbedder

	requests, faults, streams, embeds, tokensOut atomic.Int64
}

// New creates a mock server.
func New(cfg Config) *Server {
	if cfg.FailStatus == 0 {
		cfg.FailStatus = http.StatusServiceUnavailable
	}
	if cfg.Tokens <= 0 {
		cfg.Tokens = 48
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), embedder: cache.NewHashingEmbedder()}
	s.mux.HandleFunc("POST /v1/chat/completions", s.openai)
	s.mux.HandleFunc("POST /v1/messages", s.anthropic)
	s.mux.HandleFunc("POST /v1beta/models/{modelAction}", s.gemini)
	s.mux.HandleFunc("POST /v1/embeddings", s.embeddings)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	s.mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Stats())
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Stats returns a snapshot of counters.
func (s *Server) Stats() Stats {
	return Stats{Requests: s.requests.Load(), InjectedFaults: s.faults.Load(), Streams: s.streams.Load(), Embeddings: s.embeds.Load(), TokensOut: s.tokensOut.Load()}
}

type knobs struct {
	ttft, tpot time.Duration
	tokens     int
	failRate   float64
	failStatus int
}

func (s *Server) knobs(r *http.Request, maxTokens int) knobs {
	q := r.URL.Query()
	k := knobs{ttft: s.cfg.TTFT, tpot: s.cfg.TPOT, tokens: s.cfg.Tokens, failRate: s.cfg.FailRate, failStatus: s.cfg.FailStatus}
	if v, err := strconv.Atoi(q.Get("ttft_ms")); err == nil && v >= 0 {
		k.ttft = time.Duration(v) * time.Millisecond
	}
	if v, err := strconv.ParseFloat(q.Get("tpot_ms"), 64); err == nil && v >= 0 {
		k.tpot = time.Duration(v * float64(time.Millisecond))
	}
	if v, err := strconv.Atoi(q.Get("tokens")); err == nil && v > 0 {
		k.tokens = v
	}
	if v, err := strconv.ParseFloat(q.Get("fail_rate"), 64); err == nil && v >= 0 && v <= 1 {
		k.failRate = v
	}
	if v, err := strconv.Atoi(q.Get("fail_status")); err == nil && v >= 400 {
		k.failStatus = v
	}
	if maxTokens > 0 && maxTokens < k.tokens {
		k.tokens = maxTokens
	}
	if s.cfg.Jitter > 0 && k.ttft > 0 {
		f := 1 + (rand.Float64()*2-1)*s.cfg.Jitter
		k.ttft = time.Duration(float64(k.ttft) * f)
	}
	return k
}

// chaos returns true (after writing an error) if a failure should be injected.
func (s *Server) chaos(w http.ResponseWriter, r *http.Request, k knobs) bool {
	s.requests.Add(1)
	status := 0
	if v := r.Header.Get("X-Mock-Fail"); v != "" {
		if code, err := strconv.Atoi(v); err == nil && code >= 400 {
			status = code
		}
	} else if k.failRate > 0 && rand.Float64() < k.failRate {
		status = k.failStatus
	}
	if status == 0 {
		return false
	}
	s.faults.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"type":"mock_injected_fault","message":"mockllm injected HTTP %d"}}`, status)
	return true
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var vocabulary = strings.Fields(`the gateway routes each request through a token bucket then checks the
semantic cache before calling an upstream model which streams tokens back to the client while guardrails
redact sensitive data and the circuit breaker tracks provider health so failures trigger automatic failover
latency stays low because embeddings are computed locally and vector search uses an hnsw index in postgres
throughput scales horizontally behind a load balancer with shared redis state for quotas and exact matches`)

// Completion deterministically generates the mock answer for a prompt. The
// answer starts with an echo of the prompt so callers (e.g. the benchmark)
// can verify which prompt produced a cached response.
func Completion(prompt string, tokens int) []string {
	echo := strings.Join(strings.Fields(prompt), " ")
	if len(echo) > 120 {
		echo = echo[:120]
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(prompt))
	rng := rand.New(rand.NewPCG(h.Sum64(), 0x9e3779b97f4a7c15))
	out := make([]string, 0, tokens+1)
	out = append(out, "[mock] Re: \""+echo+"\" —")
	for i := 1; i < tokens; i++ {
		out = append(out, vocabulary[rng.IntN(len(vocabulary))])
	}
	if len(out) > 1 {
		out[len(out)-1] += "."
	}
	return out
}

func lastUser(roles, contents []string) string {
	for i := len(roles) - 1; i >= 0; i-- {
		if roles[i] == "user" {
			return contents[i]
		}
	}
	if len(contents) > 0 {
		return contents[len(contents)-1]
	}
	return ""
}

func countTokens(parts ...string) int {
	n := 0
	for _, p := range parts {
		n += (len(p) + 3) / 4
	}
	return n
}

func flusher(w http.ResponseWriter) http.Flusher {
	f, _ := w.(http.Flusher)
	return f
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// ------------------------------------------------------------------ OpenAI ---

func (s *Server) openai(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens     int `json:"max_tokens"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	k := s.knobs(r, req.MaxTokens)
	if s.chaos(w, r, k) {
		return
	}
	roles := make([]string, len(req.Messages))
	contents := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		roles[i] = m.Role
		var str string
		if json.Unmarshal(m.Content, &str) != nil {
			str = string(m.Content)
		}
		contents[i] = str
	}
	prompt := lastUser(roles, contents)
	words := Completion(prompt, k.tokens)
	promptTokens := countTokens(contents...)
	id := fmt.Sprintf("chatcmpl-mock-%x", rand.Uint64())
	created := time.Now().Unix()

	if !req.Stream {
		if !sleep(r.Context(), k.ttft+time.Duration(len(words))*k.tpot) {
			return
		}
		s.tokensOut.Add(int64(len(words)))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": strings.Join(words, " ")}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": promptTokens, "completion_tokens": len(words), "total_tokens": promptTokens + len(words)},
		})
		return
	}

	s.streams.Add(1)
	sseHeaders(w)
	f := flusher(w)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if f != nil {
			f.Flush()
		}
	}
	chunk := func(delta map[string]string, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	if f != nil {
		f.Flush()
	}
	if !sleep(r.Context(), k.ttft) {
		return
	}
	send(chunk(map[string]string{"role": "assistant"}, nil))
	for i, word := range words {
		if i > 0 && !sleep(r.Context(), k.tpot) {
			return
		}
		text := word
		if i < len(words)-1 {
			text += " "
		}
		send(chunk(map[string]string{"content": text}, nil))
	}
	s.tokensOut.Add(int64(len(words)))
	send(chunk(map[string]string{}, "stop"))
	if req.StreamOptions.IncludeUsage {
		send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{},
			"usage": map[string]int{"prompt_tokens": promptTokens, "completion_tokens": len(words), "total_tokens": promptTokens + len(words)}})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f != nil {
		f.Flush()
	}
}

// --------------------------------------------------------------- Anthropic ---

func (s *Server) anthropic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	if req.MaxTokens <= 0 {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is required"}}`, http.StatusBadRequest)
		return
	}
	k := s.knobs(r, req.MaxTokens)
	if s.chaos(w, r, k) {
		return
	}
	roles := make([]string, len(req.Messages))
	contents := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		roles[i], contents[i] = m.Role, m.Content
	}
	words := Completion(lastUser(roles, contents), k.tokens)
	inTok := countTokens(append(contents, req.System)...)
	id := fmt.Sprintf("msg_mock_%x", rand.Uint64())

	if !req.Stream {
		if !sleep(r.Context(), k.ttft+time.Duration(len(words))*k.tpot) {
			return
		}
		s.tokensOut.Add(int64(len(words)))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]string{{"type": "text", "text": strings.Join(words, " ")}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": inTok, "output_tokens": len(words)},
		})
		return
	}

	s.streams.Add(1)
	sseHeaders(w)
	f := flusher(w)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if f != nil {
			f.Flush()
		}
	}
	if f != nil {
		f.Flush()
	}
	if !sleep(r.Context(), k.ttft) {
		return
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "usage": map[string]int{"input_tokens": inTok, "output_tokens": 0}}})
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	send("ping", map[string]string{"type": "ping"})
	for i, word := range words {
		if i > 0 && !sleep(r.Context(), k.tpot) {
			return
		}
		text := word
		if i < len(words)-1 {
			text += " "
		}
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text}})
	}
	s.tokensOut.Add(int64(len(words)))
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": len(words)}})
	send("message_stop", map[string]string{"type": "message_stop"})
}

// ------------------------------------------------------------------ Gemini ---

func (s *Server) gemini(w http.ResponseWriter, r *http.Request) {
	modelAction := r.PathValue("modelAction")
	model, action, ok := strings.Cut(modelAction, ":")
	if !ok || (action != "generateContent" && action != "streamGenerateContent") {
		http.Error(w, `{"error":{"code":404,"message":"unknown method"}}`, http.StatusNotFound)
		return
	}
	var req struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		GenerationConfig struct {
			MaxOutputTokens int `json:"maxOutputTokens"`
		} `json:"generationConfig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"code":400,"message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	k := s.knobs(r, req.GenerationConfig.MaxOutputTokens)
	if s.chaos(w, r, k) {
		return
	}
	roles := make([]string, len(req.Contents))
	contents := make([]string, len(req.Contents))
	for i, c := range req.Contents {
		roles[i] = c.Role
		for _, p := range c.Parts {
			contents[i] += p.Text
		}
	}
	words := Completion(lastUser(roles, contents), k.tokens)
	inTok := countTokens(contents...)
	usage := map[string]int{"promptTokenCount": inTok, "candidatesTokenCount": len(words), "totalTokenCount": inTok + len(words)}
	candidate := func(text string, finish string) map[string]any {
		c := map[string]any{"content": map[string]any{"role": "model", "parts": []map[string]string{{"text": text}}}, "index": 0}
		if finish != "" {
			c["finishReason"] = finish
		}
		return c
	}

	if action == "generateContent" {
		if !sleep(r.Context(), k.ttft+time.Duration(len(words))*k.tpot) {
			return
		}
		s.tokensOut.Add(int64(len(words)))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{candidate(strings.Join(words, " "), "STOP")}, "usageMetadata": usage, "modelVersion": model})
		return
	}

	s.streams.Add(1)
	sseHeaders(w)
	f := flusher(w)
	if f != nil {
		f.Flush()
	}
	if !sleep(r.Context(), k.ttft) {
		return
	}
	for i, word := range words {
		if i > 0 && !sleep(r.Context(), k.tpot) {
			return
		}
		text := word
		finish := ""
		if i < len(words)-1 {
			text += " "
		} else {
			finish = "STOP"
		}
		msg := map[string]any{"candidates": []any{candidate(text, finish)}, "modelVersion": model}
		if finish != "" {
			msg["usageMetadata"] = usage
		}
		b, _ := json.Marshal(msg)
		_, _ = fmt.Fprintf(w, "data: %s\r\n\r\n", b)
		if f != nil {
			f.Flush()
		}
	}
	s.tokensOut.Add(int64(len(words)))
}

// -------------------------------------------------------------- Embeddings ---

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input any    `json:"input"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	s.embeds.Add(1)
	var inputs []string
	switch v := req.Input.(type) {
	case string:
		inputs = []string{v}
	case []any:
		for _, x := range v {
			if str, ok := x.(string); ok {
				inputs = append(inputs, str)
			}
		}
	}
	data := make([]map[string]any, len(inputs))
	for i, in := range inputs {
		vec, _ := s.embedder.Embed(r.Context(), in)
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vec}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model})
}
