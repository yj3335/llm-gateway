// Command bench runs a real high-concurrency benchmark of the gateway.
//
// By default it starts cmd/mockllm and the gateway in-process on loopback TCP
// ports (backed by the real Redis + PostgreSQL/pgvector when reachable),
// drives a realistic workload, and writes measured results to
// benchmarks/results.json.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yj3335/llm-gateway/internal/auth"
	"github.com/yj3335/llm-gateway/internal/cache"
	"github.com/yj3335/llm-gateway/internal/config"
	"github.com/yj3335/llm-gateway/internal/gateway"
	"github.com/yj3335/llm-gateway/internal/guardrails"
	"github.com/yj3335/llm-gateway/internal/mockllm"
	"github.com/yj3335/llm-gateway/internal/storage"
)

type options struct {
	requests, concurrency, baseline, hotRequests         int
	faultRate, piiRate, injectionRate, uniqueRate, zipfS float64
	ttft, tpot                                           time.Duration
	tokens                                               int
	redisAddr, pgDSN, out, cpuProfile                    string
	configPath, model                                    string
	maxTokens                                            int
	seed                                                 uint64
}

func main() {
	var o options
	flag.IntVar(&o.requests, "requests", 5000, "requests in the mixed workload")
	flag.IntVar(&o.concurrency, "concurrency", 100, "concurrent workers")
	flag.IntVar(&o.baseline, "baseline", 500, "direct-to-upstream baseline requests")
	flag.IntVar(&o.hotRequests, "hot-requests", 20000, "requests in the hot-cache throughput phase (0 to skip)")
	flag.Float64Var(&o.faultRate, "fault-rate", 0.10, "injected 503 rate on the primary upstream provider")
	flag.Float64Var(&o.piiRate, "pii-rate", 0.10, "fraction of prompts containing PII")
	flag.Float64Var(&o.injectionRate, "injection-rate", 0.02, "fraction of prompt-injection attempts")
	flag.Float64Var(&o.uniqueRate, "unique-rate", 0.25, "fraction of never-repeating long-tail prompts")
	flag.Float64Var(&o.zipfS, "zipf-s", 1.1, "zipf exponent for head-intent popularity")
	flag.DurationVar(&o.ttft, "ttft", 120*time.Millisecond, "mock upstream time-to-first-token")
	flag.DurationVar(&o.tpot, "tpot", time.Millisecond, "mock upstream time per output token")
	flag.IntVar(&o.tokens, "tokens", 48, "mock completion tokens")
	flag.StringVar(&o.redisAddr, "redis", envOr("LLMGW_REDIS_ADDR", "localhost:6379"), "Redis address ('' = in-memory)")
	flag.StringVar(&o.pgDSN, "postgres", envOr("LLMGW_POSTGRES_DSN", "postgres://llmgw@localhost:5432/llmgateway?sslmode=disable"), "PostgreSQL DSN ('' = in-memory)")
	flag.StringVar(&o.out, "out", "benchmarks/results.json", "results file")
	flag.Uint64Var(&o.seed, "seed", 42, "workload RNG seed")
	flag.StringVar(&o.cpuProfile, "cpuprofile", "", "write a CPU profile of the mixed-workload phase")
	flag.StringVar(&o.configPath, "config", "", "gateway config for real upstreams, e.g. configs/gateway.yaml (local Ollama); empty = in-process mock upstream")
	flag.StringVar(&o.model, "model", "", "model requested by clients (default: gpt-4o-mini for mock, qwen3.5:9b with -config)")
	flag.IntVar(&o.maxTokens, "max-tokens", 0, "max_tokens per request (default: -tokens for mock, 64 with -config)")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func listen() (net.Listener, string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	return l, "http://" + l.Addr().String()
}

func run(o options) error {
	ctx := context.Background()
	runID := time.Now().Format("20060102-150405")
	tenant := "bench-" + runID
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	real := o.configPath != ""
	if o.model == "" {
		o.model = "gpt-4o-mini"
		if real {
			o.model = "qwen3.5:9b"
		}
	}
	if o.maxTokens == 0 {
		o.maxTokens = o.tokens
		if real {
			o.maxTokens = 64
		}
	}

	var (
		mock    *mockllm.Server
		mockURL string
		cfg     config.Config
	)
	if real {
		// ---- real upstreams (e.g. local Ollama) ------------------------------------
		var err error
		if cfg, err = config.Load(o.configPath); err != nil {
			return err
		}
		if len(cfg.Providers) == 0 {
			return fmt.Errorf("%s defines no providers", o.configPath)
		}
		// 10% of calls to the primary fail with a synthetic 503 (chaos testing).
		cfg.Providers[0].FaultInjectionRate = o.faultRate
		cfg.Postgres.MaxConns, cfg.Postgres.MinConns = 32, 16
	} else {
		// ---- in-process mock upstream (isolates the gateway itself) -----------------
		mock = mockllm.New(mockllm.Config{TTFT: o.ttft, TPOT: o.tpot, Tokens: o.tokens, FailStatus: 503, Jitter: 0.15})
		var ml net.Listener
		ml, mockURL = listen()
		mockSrv := &http.Server{Handler: mock}
		go mockSrv.Serve(ml) //nolint:errcheck
		defer mockSrv.Close()

		cfg = config.Default()
		cfg.Providers = []config.ProviderConfig{
			{Name: "openai", Type: "openai", BaseURL: mockURL, Timeout: 10 * time.Second, Query: map[string]string{"fail_rate": strconv.FormatFloat(o.faultRate, 'f', -1, 64)}},
			{Name: "anthropic", Type: "anthropic", BaseURL: mockURL, Timeout: 10 * time.Second},
			{Name: "gemini", Type: "gemini", BaseURL: mockURL, Timeout: 10 * time.Second},
		}
		cfg.Routes = []config.RouteConfig{{Match: "*", Targets: []config.RouteTarget{
			{Provider: "openai"}, {Provider: "anthropic", Model: "claude-3-5-haiku-latest"}, {Provider: "gemini", Model: "gemini-2.0-flash"},
		}}}
		cfg.Pricing = map[string]config.Price{"default": {Input: 0.0025, Output: 0.01}}
		cfg.Postgres.MaxConns, cfg.Postgres.MinConns = 96, 64
	}

	// ---- gateway ------------------------------------------------------------------
	cfg.Auth.JWTHMACSecret = "bench-secret-" + runID
	cfg.Redis.Addr = o.redisAddr
	cfg.Postgres.DSN = o.pgDSN
	cfg.Cache.WriteQueueSize = 65536
	cfg.Cache.WriteWorkers = 8
	cfg.Telemetry.SampleWindow = 50000
	gw, cleanup, err := gateway.Build(ctx, cfg, gateway.BuildOptions{Logger: logger, AuditWriter: io.Discard, SkipRedis: o.redisAddr == "", SkipPG: o.pgDSN == ""})
	if err != nil {
		return err
	}
	defer cleanup()
	gl, gwURL := listen()
	gwSrv := &http.Server{Handler: gw.Handler()}
	go gwSrv.Serve(gl) //nolint:errcheck
	defer gwSrv.Close()
	backends := gw.Collector.Summary().Backends

	token, err := auth.MintHS256([]byte(cfg.Auth.JWTHMACSecret), auth.MintRequest{
		TenantID: tenant, Tier: "benchmark", RPMLimit: 10_000_000, TPMLimit: 10_000_000_000, PIIMode: "REDACT",
		TTL: time.Hour, Issuer: cfg.Auth.Issuer, Audience: cfg.Auth.Audience,
	})
	if err != nil {
		return err
	}

	client := &http.Client{Transport: &http.Transport{MaxIdleConns: o.concurrency * 2, MaxIdleConnsPerHost: o.concurrency * 2, IdleConnTimeout: time.Minute}, Timeout: 10 * time.Minute}
	fmt.Fprintf(os.Stderr, "backends: %v\nmode=%s gateway=%s tenant=%s model=%s max_tokens=%d\n", backends, map[bool]string{true: "real (" + o.configPath + ")", false: "mock " + mockURL}[real], gwURL, tenant, o.model, o.maxTokens)

	// ---- phase 1: direct upstream baseline (no gateway) ------------------------------
	// Real mode: one request at a time straight to the primary model, i.e. pure
	// model latency without queueing. Mock mode: same concurrency as the workload.
	baseURL, baseModel, baseConc, baseExtra := mockURL, "gpt-4o-mini", o.concurrency, map[string]any{}
	if real {
		baseURL, baseModel, baseConc, baseExtra = strings.TrimRight(cfg.Providers[0].BaseURL, "/"), cfg.Providers[0].DefaultModel, 1, cfg.Providers[0].ExtraBody
	}
	fmt.Fprintf(os.Stderr, "phase 1: %d direct upstream requests to %s (uncached baseline, concurrency %d)\n", o.baseline, baseModel, baseConc)
	baseline := runPhase(o.baseline, baseConc, func(i int) sample {
		b := map[string]any{"model": baseModel, "max_tokens": o.maxTokens, "messages": []map[string]string{
			{"role": "system", "content": "You are a concise senior engineer."},
			{"role": "user", "content": fmt.Sprintf("Baseline question %d: explain one trade-off in distributed systems.", i)}}}
		for k, v := range baseExtra {
			b[k] = v
		}
		body, _ := json.Marshal(b)
		return do(client, baseURL+"/v1/chat/completions", "", string(body), nil)
	})

	// ---- phase 2: mixed workload through the gateway ---------------------------------
	intents := buildIntents()
	type plan struct {
		kind   string // head | pii | unique | injection
		prompt string
		intent int
	}
	rng := rand.New(rand.NewPCG(o.seed, o.seed^0x5DEECE66D))
	zipf := rand.NewZipf(rng, o.zipfS, 1, uint64(len(intents)-1))
	// Shuffle intent popularity so the hottest intents are spread across topics.
	perm := rng.Perm(len(intents))
	plans := make([]plan, o.requests)
	for i := range plans {
		x := rng.Float64()
		switch {
		case x < o.injectionRate:
			plans[i] = plan{kind: "injection", prompt: injections[rng.IntN(len(injections))], intent: -1}
		case x < o.injectionRate+o.uniqueRate:
			plans[i] = plan{kind: "unique", prompt: uniquePrompt(rng, i), intent: -1}
		default:
			in := intents[perm[zipf.Uint64()]]
			p := plan{kind: "head", prompt: in.variants[rng.IntN(len(in.variants))], intent: in.id}
			if x < o.injectionRate+o.uniqueRate+o.piiRate {
				p.kind = "pii"
				p.prompt += piiSuffixes[rng.IntN(len(piiSuffixes))]
			}
			plans[i] = p
		}
	}
	var mockBefore mockllm.Stats
	if mock != nil {
		mockBefore = mock.Stats()
	}
	fmt.Fprintf(os.Stderr, "phase 2: %d mixed requests x %d workers (fault rate %.0f%%)\n", o.requests, o.concurrency, o.faultRate*100)
	if o.cpuProfile != "" {
		f, err := os.Create(o.cpuProfile)
		if err != nil {
			return err
		}
		_ = pprof.StartCPUProfile(f)
		defer f.Close()
	}
	start := time.Now()
	samples := runPhase(o.requests, o.concurrency, func(i int) sample {
		p := plans[i]
		body, _ := json.Marshal(map[string]any{"model": o.model, "max_tokens": o.maxTokens, "messages": []map[string]string{
			{"role": "system", "content": "You are a concise senior engineer."}, {"role": "user", "content": p.prompt},
		}})
		s := do(client, gwURL+"/v1/chat/completions", token, string(body), nil)
		s.kind, s.intent = p.kind, p.intent
		return s
	})
	elapsed := time.Since(start)
	if o.cpuProfile != "" {
		pprof.StopCPUProfile()
	}
	var mockAfter mockllm.Stats
	if mock != nil {
		mockAfter = mock.Stats()
	}
	costSaved := gw.Collector.Summary().Cache.CostSavedUSD
	providerHealth := gw.Router.Health()
	if gw.Cache != nil {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = gw.Cache.Flush(fctx)
		cancel()
	}

	// ---- phase 3: hot-cache throughput -------------------------------------------------
	var hot *throughput
	if o.hotRequests > 0 {
		fmt.Fprintf(os.Stderr, "phase 3: %d hot-cache requests x %d workers\n", o.hotRequests, o.concurrency)
		nWarm := 50
		if real {
			nWarm = 20 // each warm-up miss is a real generation
		}
		warm := intents[:nWarm]
		for _, in := range warm { // ensure warm (exact) entries exist
			body := fmt.Sprintf(`{"model":%q,"max_tokens":%d,"messages":[{"role":"user","content":%q}]}`, o.model, o.maxTokens, in.variants[0])
			do(client, gwURL+"/v1/chat/completions", token, body, nil)
		}
		hs := time.Now()
		hotSamples := runPhase(o.hotRequests, o.concurrency, func(i int) sample {
			body := fmt.Sprintf(`{"model":%q,"max_tokens":%d,"messages":[{"role":"user","content":%q}]}`, o.model, o.maxTokens, warm[i%len(warm)].variants[0])
			return do(client, gwURL+"/v1/chat/completions", token, body, nil)
		})
		hd := time.Since(hs)
		var lat, ov []float64
		hits := 0
		for _, s := range hotSamples {
			if s.status == 200 {
				lat = append(lat, s.latencyMs)
				ov = append(ov, s.overheadMs)
			}
			if strings.HasPrefix(s.cache, "HIT") {
				hits++
			}
		}
		hot = &throughput{Requests: len(hotSamples), DurationS: round(hd.Seconds(), 3), RPS: round(float64(len(hotSamples))/hd.Seconds(), 1),
			HitRatioPct: round(100*float64(hits)/float64(len(hotSamples)), 2), ClientLatencyMs: pct(lat), GatewayOverheadMs: pct(ov)}
	}

	// ---- phase 4: guardrail micro-benchmark ----------------------------------------------
	guard := guardrailMicrobench(intents)
	embed := embedMicrobench(intents)

	faults, upstreamCalls := mockAfter.InjectedFaults-mockBefore.InjectedFaults, mockAfter.Requests-mockBefore.Requests
	if real { // from the gateway's own per-provider accounting
		faults, upstreamCalls = 0, 0
		for i, h := range providerHealth {
			upstreamCalls += h.Requests
			if i == 0 {
				faults = h.Failures
			}
		}
	}
	res := analyze(o, samples, baseline, elapsed, faults, upstreamCalls)
	res.Workload.Cache.CostSavedUSD = round(costSaved, 4)
	res.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	res.Environment = map[string]any{
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(),
		"backends": backends, "topology": "gateway + mockllm + bench client in one process over loopback TCP; Redis 7.4 and PostgreSQL 16 + pgvector 0.6 on localhost",
	}
	if real {
		res.Environment["topology"] = "gateway + bench client in one process over loopback TCP; local Ollama (Apple Silicon GPU, generations served one at a time); Redis 7.4 and PostgreSQL 16 + pgvector 0.6 on localhost"
		var models []string
		for _, p := range cfg.Providers {
			models = append(models, p.Name+"="+p.DefaultModel)
		}
		res.Environment["upstream_models"] = models
		res.Environment["embedder"] = cfg.Cache.EmbeddingModel
	}
	res.Config = map[string]any{
		"mode":            map[bool]string{true: "real upstream: " + o.configPath, false: "in-process mock upstream"}[real],
		"model_requested": o.model, "max_tokens": o.maxTokens, "baseline_concurrency": baseConc,
		"requests": o.requests, "concurrency": o.concurrency, "fault_rate": o.faultRate, "pii_rate": o.piiRate,
		"injection_rate": o.injectionRate, "unique_rate": o.uniqueRate, "zipf_s": o.zipfS, "head_intents": len(intents),
		"paraphrases_per_intent": len(intents[0].variants), "mock_ttft_ms": o.ttft.Milliseconds(), "mock_tpot_ms": float64(o.tpot.Microseconds()) / 1000,
		"mock_tokens": o.tokens, "similarity_threshold": cfg.Cache.SimilarityThreshold, "seed": o.seed,
		"fallback_similarity_threshold": cfg.Cache.FallbackSimilarityThreshold,
	}
	res.HotCache = hot
	res.Providers = providerHealth
	res.GuardrailMicrobench = guard
	res.EmbedderMicrobench = embed

	if o.pgDSN != "" {
		if pool, err := storage.Connect(ctx, storage.Options{DSN: o.pgDSN, MaxConns: 2}); err == nil {
			var rows int64
			_ = pool.QueryRow(ctx, "SELECT count(*) FROM semantic_cache WHERE tenant_id = $1", tenant).Scan(&rows)
			res.Workload.Cache.PGVectorRows = rows
			_, _ = pool.Exec(ctx, "DELETE FROM semantic_cache WHERE tenant_id = $1", tenant)
			pool.Close()
		}
	}

	if err := os.MkdirAll(filepath.Dir(o.out), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(o.out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	printSummary(res)
	fmt.Fprintf(os.Stderr, "\nwrote %s\n", o.out)
	return nil
}

// --------------------------------------------------------------- driver ---

type sample struct {
	status       int
	latencyMs    float64
	overheadMs   float64
	upstreamMs   float64
	cache        string
	similarity   float64
	provider     string
	failover     bool
	guardrail    string
	coalesced    bool
	tokensSaved  int
	lookupMs     float64
	content      string
	errCode      string
	kind         string
	intent       int
	transportErr bool
}

func runPhase(n, workers int, fn func(i int) sample) []sample {
	out := make([]sample, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				out[i] = fn(i)
			}
		}()
	}
	wg.Wait()
	return out
}

func do(c *http.Client, url, token, body string, hdr map[string]string) sample {
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return sample{transportErr: true, latencyMs: msSince(start)}
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := sample{status: resp.StatusCode, latencyMs: msSince(start)}
	h := resp.Header
	s.cache = h.Get("X-Gateway-Cache")
	s.provider = h.Get("X-Gateway-Provider")
	s.failover = h.Get("X-Gateway-Provider-Failover") == "true"
	s.guardrail = h.Get("X-Gateway-Guardrail")
	s.coalesced = h.Get("X-Gateway-Coalesced") == "true"
	s.overheadMs, _ = strconv.ParseFloat(h.Get("X-Gateway-Overhead-Ms"), 64)
	s.upstreamMs, _ = strconv.ParseFloat(h.Get("X-Gateway-Upstream-Latency-Ms"), 64)
	s.similarity, _ = strconv.ParseFloat(h.Get("X-Gateway-Cache-Similarity"), 64)
	s.tokensSaved, _ = strconv.Atoi(h.Get("X-Gateway-Tokens-Saved"))
	s.lookupMs, _ = strconv.ParseFloat(h.Get("X-Gateway-Cache-Lookup-Ms"), 64)
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		if len(parsed.Choices) > 0 {
			s.content = parsed.Choices[0].Message.Content
		}
		s.errCode = parsed.Error.Code
	}
	return s
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

// ------------------------------------------------------------- analysis ---

type pctStats struct {
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
	N    int     `json:"n"`
}

func pct(v []float64) pctStats {
	if len(v) == 0 {
		return pctStats{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	at := func(p float64) float64 {
		i := int(math.Ceil(p/100*float64(len(s)))) - 1
		return s[max(0, min(i, len(s)-1))]
	}
	var sum float64
	for _, x := range s {
		sum += x
	}
	return pctStats{P50: round(at(50), 3), P95: round(at(95), 3), P99: round(at(99), 3), Mean: round(sum/float64(len(s)), 3), Max: round(s[len(s)-1], 3), N: len(s)}
}

type throughput struct {
	Requests          int      `json:"requests"`
	DurationS         float64  `json:"duration_s"`
	RPS               float64  `json:"rps"`
	HitRatioPct       float64  `json:"hit_ratio_pct"`
	ClientLatencyMs   pctStats `json:"client_latency_ms"`
	GatewayOverheadMs pctStats `json:"gateway_overhead_ms"`
}

type results struct {
	GeneratedAt         string         `json:"generated_at"`
	Environment         map[string]any `json:"environment"`
	Config              map[string]any `json:"config"`
	UpstreamBaseline    pctStats       `json:"upstream_baseline_latency_ms"`
	Workload            workload       `json:"mixed_workload"`
	HotCache            *throughput    `json:"hot_cache_throughput,omitempty"`
	Providers           any            `json:"provider_health_after_workload"`
	GuardrailMicrobench map[string]any `json:"guardrail_microbench_us"`
	EmbedderMicrobench  map[string]any `json:"embedder_microbench_us"`
}

type workload struct {
	Requests      int            `json:"requests"`
	DurationS     float64        `json:"duration_s"`
	RPS           float64        `json:"rps"`
	StatusCodes   map[string]int `json:"status_codes"`
	TransportErrs int            `json:"transport_errors"`
	ClientLatency struct {
		All      pctStats `json:"all"`
		CacheHit pctStats `json:"cache_hit"`
		CacheMis pctStats `json:"cache_miss"`
	} `json:"client_latency_ms"`
	GatewayOverhead struct {
		All      pctStats `json:"all"`
		CacheHit pctStats `json:"cache_hit"`
		CacheMis pctStats `json:"cache_miss"`
	} `json:"gateway_overhead_ms"`
	UpstreamLatency pctStats `json:"upstream_latency_ms"`
	CacheLookup     struct {
		Hit  pctStats `json:"cache_hit"`
		Miss pctStats `json:"cache_miss"`
	} `json:"cache_lookup_ms"`
	Cache struct {
		Lookups                int     `json:"lookups"`
		HitRatioPct            float64 `json:"hit_ratio_pct"`
		ExactHitRatioPct       float64 `json:"exact_hit_ratio_pct"`
		SemanticHitRatioPct    float64 `json:"semantic_hit_ratio_pct"`
		HeadHitRatioPct        float64 `json:"repeated_intent_hit_ratio_pct"`
		SemanticPrecisionPct   float64 `json:"semantic_precision_pct"`
		SemanticVerified       int     `json:"semantic_hits_verified"`
		AvgSimilarity          float64 `json:"avg_semantic_similarity"`
		CoalescedRequests      int     `json:"coalesced_requests"`
		LatencyReductionP50Pct float64 `json:"latency_reduction_p50_pct"`
		MeanLatencyReductPct   float64 `json:"mean_latency_reduction_vs_uncached_pct"`
		TokensSaved            int     `json:"tokens_saved"`
		CostSavedUSD           float64 `json:"est_cost_saved_usd"`
		UpstreamCallsAvoided   int64   `json:"upstream_calls_avoided"`
		PGVectorRows           int64   `json:"pgvector_rows_written"`
	} `json:"cache"`
	Failover struct {
		InjectedFaults      int64   `json:"injected_upstream_faults"`
		UpstreamCalls       int64   `json:"upstream_calls"`
		FailoverResponses   int     `json:"failover_responses"`
		UnrecoveredFailures int     `json:"unrecovered_5xx"`
		SuccessRatePct      float64 `json:"failover_success_rate_pct"`
	} `json:"failover"`
	Guardrails struct {
		PIIPrompts          int `json:"pii_prompts"`
		PIIRedacted         int `json:"pii_redacted"`
		InjectionAttempts   int `json:"injection_attempts"`
		InjectionBlocked    int `json:"injection_blocked"`
		FalsePositiveBlocks int `json:"false_positive_blocks"`
	} `json:"guardrails"`
}

func analyze(o options, ss []sample, base []sample, elapsed time.Duration, faults, upstreamCalls int64) results {
	// Ground truth for semantic precision: every answer in the cache was first
	// produced by an upstream call (a MISS) for a known intent. Map each such
	// answer to its intent; a semantic hit is correct when the answer it
	// returned belongs to the intent that was asked. Works for real models
	// (no prompt echo needed) and for the mock alike.
	answerIntent := map[string]int{}
	for _, s := range ss {
		if s.status == 200 && s.cache == "MISS" && s.intent >= 0 && s.content != "" {
			if _, seen := answerIntent[s.content]; !seen {
				answerIntent[s.content] = s.intent
			}
		}
	}
	var r results
	var bl []float64
	for _, s := range base {
		if s.status == 200 {
			bl = append(bl, s.latencyMs)
		}
	}
	r.UpstreamBaseline = pct(bl)

	w := &r.Workload
	w.Requests = len(ss)
	w.DurationS = round(elapsed.Seconds(), 3)
	w.RPS = round(float64(len(ss))/elapsed.Seconds(), 1)
	w.StatusCodes = map[string]int{}
	var all, hit, miss, ovAll, ovHit, ovMiss, up, lkHit, lkMiss []float64
	var sims []float64
	exact, sem, misses, headLookups, headHits, verified, correct := 0, 0, 0, 0, 0, 0, 0
	for _, s := range ss {
		if s.transportErr {
			w.TransportErrs++
			continue
		}
		w.StatusCodes[strconv.Itoa(s.status)]++
		switch s.kind {
		case "pii":
			w.Guardrails.PIIPrompts++
			if s.guardrail == "REDACT" {
				w.Guardrails.PIIRedacted++
			}
		case "injection":
			w.Guardrails.InjectionAttempts++
			if s.status == 400 && s.errCode == "PROMPT_INJECTION" {
				w.Guardrails.InjectionBlocked++
			}
		}
		if s.kind != "injection" && s.status == 400 {
			w.Guardrails.FalsePositiveBlocks++
		}
		if s.failover && s.status == 200 {
			w.Failover.FailoverResponses++
		}
		if s.status >= 500 {
			w.Failover.UnrecoveredFailures++
		}
		if s.status != 200 {
			continue
		}
		all = append(all, s.latencyMs)
		ovAll = append(ovAll, s.overheadMs)
		isHit := strings.HasPrefix(s.cache, "HIT")
		if s.kind == "head" || s.kind == "pii" {
			headLookups++
			if isHit {
				headHits++
			}
		}
		switch s.cache {
		case "HIT-EXACT":
			exact++
		case "HIT-SEMANTIC":
			sem++
			sims = append(sims, s.similarity)
			if s.intent >= 0 {
				if id, ok := answerIntent[s.content]; ok {
					verified++
					if id == s.intent {
						correct++
					}
				}
			}
		case "MISS":
			misses++
		}
		if isHit {
			lkHit = append(lkHit, s.lookupMs)
		} else if s.cache == "MISS" {
			lkMiss = append(lkMiss, s.lookupMs)
		}
		if isHit {
			hit = append(hit, s.latencyMs)
			ovHit = append(ovHit, s.overheadMs)
			w.Cache.TokensSaved += s.tokensSaved
		} else {
			miss = append(miss, s.latencyMs)
			ovMiss = append(ovMiss, s.overheadMs)
			if s.upstreamMs > 0 {
				up = append(up, s.upstreamMs)
			}
			if s.coalesced {
				w.Cache.CoalescedRequests++
			}
		}
	}
	w.ClientLatency.All, w.ClientLatency.CacheHit, w.ClientLatency.CacheMis = pct(all), pct(hit), pct(miss)
	w.GatewayOverhead.All, w.GatewayOverhead.CacheHit, w.GatewayOverhead.CacheMis = pct(ovAll), pct(ovHit), pct(ovMiss)
	w.UpstreamLatency = pct(up)
	w.CacheLookup.Hit, w.CacheLookup.Miss = pct(lkHit), pct(lkMiss)

	c := &w.Cache
	c.Lookups = exact + sem + misses
	if c.Lookups > 0 {
		c.HitRatioPct = round(100*float64(exact+sem)/float64(c.Lookups), 2)
		c.ExactHitRatioPct = round(100*float64(exact)/float64(c.Lookups), 2)
		c.SemanticHitRatioPct = round(100*float64(sem)/float64(c.Lookups), 2)
	}
	if headLookups > 0 {
		c.HeadHitRatioPct = round(100*float64(headHits)/float64(headLookups), 2)
	}
	c.SemanticVerified = verified
	if verified > 0 {
		c.SemanticPrecisionPct = round(100*float64(correct)/float64(verified), 2)
	}
	if len(sims) > 0 {
		var s float64
		for _, x := range sims {
			s += x
		}
		c.AvgSimilarity = round(s/float64(len(sims)), 4)
	}
	if mh, mm := pct(hit), pct(miss); mm.P50 > 0 {
		c.LatencyReductionP50Pct = round(100*(1-mh.P50/mm.P50), 2)
		c.MeanLatencyReductPct = round(100*(1-pct(all).Mean/mm.Mean), 2)
	}
	c.UpstreamCallsAvoided = int64(exact + sem + c.CoalescedRequests)

	f := &w.Failover
	f.InjectedFaults, f.UpstreamCalls = faults, upstreamCalls
	if den := f.FailoverResponses + f.UnrecoveredFailures; den > 0 {
		f.SuccessRatePct = round(100*float64(f.FailoverResponses)/float64(den), 2)
	}
	return r
}

func guardrailMicrobench(intents []intent) map[string]any {
	e := guardrails.NewEngine(0.7)
	var prompts []string
	for i, in := range intents {
		p := in.variants[0]
		if i%5 == 0 {
			p += piiSuffixes[i%len(piiSuffixes)]
		}
		prompts = append(prompts, p)
	}
	prompts = append(prompts, injections...)
	long := strings.Repeat("Please summarize the following meeting notes about our quarterly roadmap and hiring plan. ", 40) + piiSuffixes[0]
	var short, big []float64
	for i := 0; i < 20000; i++ {
		msgs := []guardrails.Message{{Role: "system", Content: "You are a concise senior engineer."}, {Role: "user", Content: prompts[i%len(prompts)]}}
		t := time.Now()
		_ = e.InspectInput(msgs, guardrails.ModeRedact)
		short = append(short, float64(time.Since(t).Nanoseconds())/1000)
	}
	for i := 0; i < 2000; i++ {
		t := time.Now()
		_ = e.InspectInput([]guardrails.Message{{Role: "user", Content: long}}, guardrails.ModeRedact)
		big = append(big, float64(time.Since(t).Nanoseconds())/1000)
	}
	return map[string]any{"typical_prompt": pct(short), "prompt_bytes_3_6k": pct(big)}
}

func embedMicrobench(intents []intent) map[string]any {
	e := cache.NewHashingEmbedder()
	var v []float64
	for i := 0; i < 20000; i++ {
		t := time.Now()
		_, _ = e.Embed(context.Background(), intents[i%len(intents)].variants[i%4])
		v = append(v, float64(time.Since(t).Nanoseconds())/1000)
	}
	// Offline separability check of the corpus under the configured threshold.
	minIntra, maxInter := 1.0, 0.0
	vecs := make([][][]float32, len(intents))
	for i, in := range intents {
		for _, s := range in.variants {
			x, _ := e.Embed(context.Background(), s)
			vecs[i] = append(vecs[i], x)
		}
	}
	for i := range vecs {
		for a := 0; a < len(vecs[i]); a++ {
			for b := a + 1; b < len(vecs[i]); b++ {
				minIntra = math.Min(minIntra, cache.Cosine(vecs[i][a], vecs[i][b]))
			}
		}
		for j := i + 1; j < len(vecs); j++ {
			maxInter = math.Max(maxInter, cache.Cosine(vecs[i][0], vecs[j][0]))
		}
	}
	return map[string]any{"embed_latency": pct(v), "corpus_min_paraphrase_similarity": round(minIntra, 4), "corpus_max_cross_intent_similarity": round(maxInter, 4)}
}

func round(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}

func printSummary(r results) {
	w := r.Workload
	fmt.Printf("\n=== LLM Gateway benchmark (%d req, %v workers) ===\n", w.Requests, r.Config["concurrency"])
	fmt.Printf("throughput           %8.1f req/s  (%.2fs)\n", w.RPS, w.DurationS)
	fmt.Printf("upstream baseline    p50 %7.2f  p95 %7.2f  p99 %7.2f ms\n", r.UpstreamBaseline.P50, r.UpstreamBaseline.P95, r.UpstreamBaseline.P99)
	fmt.Printf("gateway overhead     p50 %7.3f  p95 %7.3f  p99 %7.3f ms\n", w.GatewayOverhead.All.P50, w.GatewayOverhead.All.P95, w.GatewayOverhead.All.P99)
	fmt.Printf("client latency hit   p50 %7.2f  p95 %7.2f  p99 %7.2f ms\n", w.ClientLatency.CacheHit.P50, w.ClientLatency.CacheHit.P95, w.ClientLatency.CacheHit.P99)
	fmt.Printf("client latency miss  p50 %7.2f  p95 %7.2f  p99 %7.2f ms\n", w.ClientLatency.CacheMis.P50, w.ClientLatency.CacheMis.P95, w.ClientLatency.CacheMis.P99)
	fmt.Printf("cache hit ratio      %6.2f%%  (exact %.2f%%, semantic %.2f%%, precision %.2f%% of %d verified)\n",
		w.Cache.HitRatioPct, w.Cache.ExactHitRatioPct, w.Cache.SemanticHitRatioPct, w.Cache.SemanticPrecisionPct, w.Cache.SemanticVerified)
	fmt.Printf("latency reduction    p50 %.2f%%, mean %.2f%%\n", w.Cache.LatencyReductionP50Pct, w.Cache.MeanLatencyReductPct)
	fmt.Printf("failover             %d injected faults, %d failover responses, %d unrecovered -> %.2f%% success\n",
		w.Failover.InjectedFaults, w.Failover.FailoverResponses, w.Failover.UnrecoveredFailures, w.Failover.SuccessRatePct)
	fmt.Printf("guardrails           PII redacted %d/%d, injections blocked %d/%d, false-positive blocks %d\n",
		w.Guardrails.PIIRedacted, w.Guardrails.PIIPrompts, w.Guardrails.InjectionBlocked, w.Guardrails.InjectionAttempts, w.Guardrails.FalsePositiveBlocks)
	if r.HotCache != nil {
		fmt.Printf("hot-cache throughput %8.1f req/s  overhead p50 %.3f p99 %.3f ms\n", r.HotCache.RPS, r.HotCache.GatewayOverheadMs.P50, r.HotCache.GatewayOverheadMs.P99)
	}
}
