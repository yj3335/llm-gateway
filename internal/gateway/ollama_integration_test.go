//go:build ollama

// Live integration test against a local Ollama server (make ollama-check).
// It loads configs/gateway.yaml (qwen3.5:9b primary, gemma4:e4b fallback,
// nomic-embed-text embeddings) and proves, through the gateway's HTTP API:
// chat completion, streaming, the embeddings endpoint, the neural semantic
// cache, failover on injected 503s and timeouts, and the circuit breaker
// tripping and recovering. Uses real Redis/PostgreSQL when
// LLMGW_TEST_REDIS_ADDR / LLMGW_TEST_POSTGRES_DSN are set.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yj3335/llm-gateway/internal/auth"
	"github.com/yj3335/llm-gateway/internal/config"
)

var requiredModels = []string{"qwen3.5:9b", "gemma4:e4b", "nomic-embed-text"}

func ollamaURL() string {
	if v := os.Getenv("LLMGW_OLLAMA_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:11434"
}

func requireOllama(t *testing.T) {
	t.Helper()
	resp, err := http.Get(ollamaURL() + "/api/tags")
	if err != nil {
		t.Fatalf("Ollama is not reachable at %s (start the Ollama app or `ollama serve`): %v", ollamaURL(), err)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tags)
	have := map[string]bool{}
	for _, m := range tags.Models {
		have[m.Name] = true
		have[strings.TrimSuffix(m.Name, ":latest")] = true
	}
	for _, m := range requiredModels {
		if !have[m] {
			t.Fatalf("model %q is not pulled; run `make ollama-pull` (or `ollama pull %s`)", m, m)
		}
	}
}

type liveGW struct {
	t     *testing.T
	url   string
	token string
}

func (g *liveGW) do(path string, hdr map[string]string, body any) (int, http.Header, []byte) {
	g.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, g.url+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		g.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (g *liveGW) chat(prompt string, maxTokens int, hdr map[string]string) (int, http.Header, string) {
	g.t.Helper()
	code, h, b := g.do("/v1/chat/completions", hdr, map[string]any{
		"model": "qwen3.5:9b", "max_tokens": maxTokens,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(b, &out)
	if code != 200 || len(out.Choices) == 0 {
		return code, h, string(b)
	}
	return code, h, out.Choices[0].Message.Content
}

func (g *liveGW) breakerState(provider string) string {
	g.t.Helper()
	resp, err := http.Get(g.url + "/v1/telemetry/summary")
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	var s struct {
		Providers []struct{ Name, State string } `json:"providers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&s)
	for _, p := range s.Providers {
		if p.Name == provider {
			return p.State
		}
	}
	return "?"
}

func TestOllamaLive(t *testing.T) {
	requireOllama(t)
	cfg, err := config.Load(filepath.Join("..", "..", "configs", "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Breaker.OpenTimeout = 4 * time.Second // keep the recovery wait short
	opts := BuildOptions{AuditWriter: io.Discard, SkipRedis: true, SkipPG: true}
	if addr := os.Getenv("LLMGW_TEST_REDIS_ADDR"); addr != "" {
		cfg.Redis.Addr, opts.SkipRedis = addr, false
	}
	if dsn := os.Getenv("LLMGW_TEST_POSTGRES_DSN"); dsn != "" {
		cfg.Postgres.DSN, cfg.Postgres.MinConns, opts.SkipPG = dsn, 2, false
	}
	gw, cleanup, err := Build(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(func() { srv.Close(); cleanup() })
	tenant := fmt.Sprintf("ollama-live-%d", time.Now().UnixNano())
	tok, _ := auth.MintHS256([]byte(cfg.Auth.JWTHMACSecret), auth.MintRequest{TenantID: tenant, RPMLimit: 100000, TPMLimit: 100000000, TTL: time.Hour, Issuer: cfg.Auth.Issuer, Audience: cfg.Auth.Audience})
	g := &liveGW{t: t, url: srv.URL, token: tok}
	t.Logf("backends: %v", gw.Collector.Summary().Backends)
	nocache := map[string]string{"Cache-Control": "no-cache"}

	t.Run("embeddings endpoint returns nomic-embed-text vectors", func(t *testing.T) {
		code, h, b := g.do("/v1/embeddings", nil, map[string]any{"input": "What is the CAP theorem?"})
		var out struct {
			Model string `json:"model"`
			Data  []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		_ = json.Unmarshal(b, &out)
		if code != 200 || len(out.Data) != 1 {
			t.Fatalf("embeddings: %d %s", code, b)
		}
		v := out.Data[0].Embedding
		var n float64
		for _, x := range v {
			n += x * x
		}
		if out.Model != "ollama:nomic-embed-text" || h.Get("X-Gateway-Embedder") != "ollama:nomic-embed-text" || len(v) != 768 || math.Abs(math.Sqrt(n)-1) > 1e-3 {
			t.Fatalf("want a normalized 768-d nomic vector, got model=%s dims=%d norm=%.4f", out.Model, len(v), math.Sqrt(n))
		}
		t.Logf("embedding: %s, %d dims, norm %.4f", out.Model, len(v), math.Sqrt(n))
	})

	t.Run("chat completion through qwen3.5:9b", func(t *testing.T) {
		start := time.Now()
		code, h, content := g.chat("In one sentence, what is a circuit breaker in distributed systems?", 64, nocache)
		if code != 200 || strings.TrimSpace(content) == "" {
			t.Fatalf("chat: %d %q (empty content means thinking was not disabled)", code, content)
		}
		if h.Get("X-Gateway-Provider") != "ollama-qwen" || h.Get("X-Gateway-Upstream-Model") != "qwen3.5:9b" || h.Get("X-Gateway-Provider-Failover") != "false" {
			t.Fatalf("expected qwen without failover: %v", h)
		}
		t.Logf("qwen3.5:9b (%v): %q", time.Since(start).Round(time.Millisecond), content)
	})

	t.Run("streaming through qwen3.5:9b", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"model": "qwen3.5:9b", "max_tokens": 32, "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "Name three Linux distributions."}}})
		req, _ := http.NewRequest(http.MethodPost, g.url+"/v1/chat/completions", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var text strings.Builder
		chunks, done := 0, false
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := strings.TrimPrefix(sc.Text(), "data: ")
			if line == "[DONE]" {
				done = true
				continue
			}
			var c struct {
				Choices []struct {
					Delta struct{ Content string } `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(line), &c) == nil && len(c.Choices) > 0 && c.Choices[0].Delta.Content != "" {
				chunks++
				text.WriteString(c.Choices[0].Delta.Content)
			}
		}
		if resp.StatusCode != 200 || !done || chunks < 2 || strings.TrimSpace(text.String()) == "" {
			t.Fatalf("stream: status %d done=%v chunks=%d text=%q", resp.StatusCode, done, chunks, text.String())
		}
		t.Logf("stream: %d chunks: %q", chunks, text.String())
	})

	t.Run("semantic cache with neural embeddings", func(t *testing.T) {
		q := fmt.Sprintf("Explain how Kafka consumer groups work in simple terms (%s).", tenant[len(tenant)-6:])
		code, h, first := g.chat(q, 48, nil)
		if code != 200 || h.Get("X-Gateway-Cache") != "MISS" || h.Get("X-Gateway-Embedder") != "ollama:nomic-embed-text" {
			t.Fatalf("first request: %d %v", code, h)
		}
		if err := gw.Cache.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		para := fmt.Sprintf("Can you explain in simple terms how Kafka consumer groups work (%s)?", tenant[len(tenant)-6:])
		code, h, second := g.chat(para, 48, nil)
		sim, _ := strconv.ParseFloat(h.Get("X-Gateway-Cache-Similarity"), 64)
		if code != 200 || h.Get("X-Gateway-Cache") != "HIT-SEMANTIC" || sim < 0.92 || second != first {
			t.Fatalf("paraphrase must hit the neural semantic cache: %d cache=%s sim=%.4f layer=%s", code, h.Get("X-Gateway-Cache"), sim, h.Get("X-Gateway-Cache-Layer"))
		}
		t.Logf("paraphrase: HIT-SEMANTIC via %s, similarity %.4f, layer %s", h.Get("X-Gateway-Embedder"), sim, h.Get("X-Gateway-Cache-Layer"))

		// Near-miss: same words, different question. Must not reuse the answer.
		_, _, _ = g.chat("Explain how TCP congestion control works.", 32, nil)
		_ = gw.Cache.Flush(context.Background())
		code, h, _ = g.chat("Explain how TCP flow control works.", 32, nil)
		if code != 200 || h.Get("X-Gateway-Cache") != "MISS" {
			t.Fatalf("near-miss must not be served from cache: cache=%s sim=%s", h.Get("X-Gateway-Cache"), h.Get("X-Gateway-Cache-Similarity"))
		}
	})

	t.Run("failover to gemma4:e4b on an injected 503", func(t *testing.T) {
		code, h, content := g.chat("Reply with one word: ready?", 16, map[string]string{"Cache-Control": "no-cache", "X-Gateway-Chaos": "fail-primary"})
		if code != 200 || h.Get("X-Gateway-Provider") != "ollama-gemma" || h.Get("X-Gateway-Upstream-Model") != "gemma4:e4b" || h.Get("X-Gateway-Provider-Failover") != "true" || strings.TrimSpace(content) == "" {
			t.Fatalf("503 failover: %d %v %q", code, h, content)
		}
		t.Logf("gemma4:e4b after injected 503: %q", content)
	})

	t.Run("failover to gemma4:e4b on an injected timeout", func(t *testing.T) {
		code, h, content := g.chat("Reply with one word: ready?", 16, map[string]string{"Cache-Control": "no-cache", "X-Gateway-Chaos": "timeout-primary"})
		if code != 200 || h.Get("X-Gateway-Provider") != "ollama-gemma" || h.Get("X-Gateway-Provider-Failover") != "true" || strings.TrimSpace(content) == "" {
			t.Fatalf("timeout failover: %d %v %q", code, h, content)
		}
	})

	t.Run("circuit breaker trips, short-circuits qwen, then recovers", func(t *testing.T) {
		chaos := map[string]string{"Cache-Control": "no-cache", "X-Gateway-Chaos": "fail-primary"}
		for i := 0; i < cfg.Breaker.MinRequests && g.breakerState("ollama-qwen") != "OPEN"; i++ {
			if code, h, _ := g.chat(fmt.Sprintf("Reply with the number %d.", i), 8, chaos); code != 200 || h.Get("X-Gateway-Provider") != "ollama-gemma" {
				t.Fatalf("chaos request %d not served by gemma: %d %v", i, code, h)
			}
		}
		if st := g.breakerState("ollama-qwen"); st != "OPEN" {
			t.Fatalf("breaker for ollama-qwen should be OPEN, got %s", st)
		}
		// No chaos header now: qwen is skipped without being called.
		code, h, content := g.chat("Reply with one word: open?", 16, nocache)
		if code != 200 || h.Get("X-Gateway-Provider") != "ollama-gemma" || h.Get("X-Gateway-Provider-Failover") != "true" || strings.TrimSpace(content) == "" {
			t.Fatalf("open breaker must route to gemma: %d %v %q", code, h, content)
		}
		t.Logf("breaker OPEN: request served by %s", h.Get("X-Gateway-Upstream-Model"))

		time.Sleep(cfg.Breaker.OpenTimeout + 500*time.Millisecond)
		for i := 0; i < cfg.Breaker.HalfOpenSuccesses; i++ {
			code, h, _ := g.chat(fmt.Sprintf("Reply with the word probe%d.", i), 8, nocache)
			if code != 200 || h.Get("X-Gateway-Provider") != "ollama-qwen" {
				t.Fatalf("half-open probe %d should go to qwen: %d %v", i, code, h)
			}
		}
		if st := g.breakerState("ollama-qwen"); st != "CLOSED" {
			t.Fatalf("breaker should be CLOSED after successful probes, got %s", st)
		}
		t.Logf("breaker recovered: OPEN -> HALF-OPEN -> CLOSED")
	})

	t.Run("embedder offline: hashing fallback keeps the gateway working", func(t *testing.T) {
		// A second gateway whose embedding endpoint is unreachable, simulating
		// Ollama's embedder being offline while chat models stay up.
		dead := cfg
		dead.Cache.EmbeddingURL = "http://127.0.0.1:1/api/embeddings"
		gw2, cleanup2, err := Build(context.Background(), dead, BuildOptions{AuditWriter: io.Discard, SkipRedis: true, SkipPG: true})
		if err != nil {
			t.Fatal(err)
		}
		srv2 := httptest.NewServer(gw2.Handler())
		defer func() { srv2.Close(); cleanup2() }()
		g2 := &liveGW{t: t, url: srv2.URL, token: g.token}
		code, h, content := g2.chat("Reply with one word: fallback?", 16, nil)
		if code != 200 || strings.TrimSpace(content) == "" || h.Get("X-Gateway-Embedder") != "hashing-ngram-384" {
			t.Fatalf("chat must still work with the hashing fallback embedder: %d embedder=%q %q", code, h.Get("X-Gateway-Embedder"), content)
		}
		code, _, b := g2.do("/v1/embeddings", nil, map[string]any{"input": "hello"})
		if code != 200 || !strings.Contains(string(b), `"model":"hashing-ngram-384"`) {
			t.Fatalf("embeddings must fall back to hashing: %d %s", code, b)
		}
		resp, err := http.Get(srv2.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ready map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&ready)
		if !strings.HasPrefix(ready["embedder"], "degraded") {
			t.Fatalf("readyz must report the degraded embedder: %v", ready)
		}
		t.Logf("embedder offline: chat served by %s, cache used %s, readyz embedder=%q", h.Get("X-Gateway-Upstream-Model"), h.Get("X-Gateway-Embedder"), ready["embedder"])
	})

	t.Run("readiness reports the embedder", func(t *testing.T) {
		resp, err := http.Get(g.url + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ready map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&ready)
		if ready["embedder"] != "ok" {
			t.Fatalf("readyz: %v", ready)
		}
	})
}
