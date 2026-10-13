// Package telemetry provides Prometheus metrics, an in-memory rolling
// collector that powers the control-plane dashboard (summary + SSE stream),
// and a structured JSON audit logger with an optional PostgreSQL sink.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds every Prometheus series exported at /metrics.
type Metrics struct {
	Registry *prometheus.Registry

	Requests           *prometheus.CounterVec
	ActiveConnections  prometheus.Gauge
	RequestDuration    *prometheus.HistogramVec
	GatewayOverhead    *prometheus.HistogramVec
	UpstreamLatency    *prometheus.HistogramVec
	UpstreamRequests   *prometheus.CounterVec
	CacheLookups       *prometheus.CounterVec
	CacheSimilarity    prometheus.Histogram
	CacheBackendErrors *prometheus.CounterVec
	TokensSaved        prometheus.Counter
	CostSavedUSD       prometheus.Counter
	GuardrailTriggers  *prometheus.CounterVec
	GuardrailLatency   *prometheus.HistogramVec
	RateLimited        *prometheus.CounterVec
	RateLimitDegraded  prometheus.Counter
	BreakerState       *prometheus.GaugeVec
	BreakerTransitions *prometheus.CounterVec
	Failovers          *prometheus.CounterVec
	TokensProcessed    *prometheus.CounterVec
}

var latencyBuckets = []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

// NewMetrics registers all metrics on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(c prometheus.Collector) prometheus.Collector { reg.MustRegister(c); return c }
	m := &Metrics{Registry: reg}
	m.Requests = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_requests_total", Help: "Chat completion requests by model, cache status and HTTP status.",
	}, []string{"model", "cache", "code"})).(*prometheus.CounterVec)
	m.ActiveConnections = f(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "llmgateway_active_connections", Help: "In-flight chat completion requests.",
	})).(prometheus.Gauge)
	m.RequestDuration = f(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "llmgateway_request_duration_seconds", Help: "End-to-end request latency.", Buckets: latencyBuckets,
	}, []string{"cache"})).(*prometheus.HistogramVec)
	m.GatewayOverhead = f(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "llmgateway_gateway_overhead_seconds", Help: "Latency added by the gateway (total minus upstream).", Buckets: latencyBuckets,
	}, []string{"cache"})).(*prometheus.HistogramVec)
	m.UpstreamLatency = f(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "llmgateway_upstream_latency_seconds", Help: "Upstream provider latency.", Buckets: latencyBuckets,
	}, []string{"provider"})).(*prometheus.HistogramVec)
	m.UpstreamRequests = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_upstream_requests_total", Help: "Upstream attempts by provider and outcome.",
	}, []string{"provider", "outcome"})).(*prometheus.CounterVec)
	m.CacheLookups = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_cache_lookups_total", Help: "Cache lookups by result (HIT-EXACT, HIT-SEMANTIC, MISS, BYPASS).",
	}, []string{"result"})).(*prometheus.CounterVec)
	m.CacheSimilarity = f(prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "llmgateway_cache_similarity", Help: "Cosine similarity of semantic cache hits.",
		Buckets: []float64{.8, .82, .84, .86, .88, .9, .92, .94, .96, .98, 1},
	})).(prometheus.Histogram)
	m.CacheBackendErrors = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_cache_backend_errors_total", Help: "Non-fatal cache backend errors by operation.",
	}, []string{"op"})).(*prometheus.CounterVec)
	m.TokensSaved = f(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "llmgateway_tokens_saved_total", Help: "Upstream tokens avoided via cache hits.",
	})).(prometheus.Counter)
	m.CostSavedUSD = f(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "llmgateway_cost_saved_usd_total", Help: "Estimated USD saved via cache hits.",
	})).(prometheus.Counter)
	m.GuardrailTriggers = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_guardrail_triggers_total", Help: "Guardrail interventions by type and action.",
	}, []string{"type", "action"})).(*prometheus.CounterVec)
	m.GuardrailLatency = f(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "llmgateway_guardrail_latency_seconds", Help: "Guardrail evaluation time.",
		Buckets: []float64{.00001, .000025, .00005, .0001, .00025, .0005, .001, .0025},
	}, []string{"direction"})).(*prometheus.HistogramVec)
	m.RateLimited = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_ratelimit_rejections_total", Help: "Requests rejected with 429.",
	}, []string{"reason"})).(*prometheus.CounterVec)
	m.RateLimitDegraded = f(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "llmgateway_ratelimit_degraded_total", Help: "Rate-limit decisions served by the local fallback (Redis unavailable).",
	})).(prometheus.Counter)
	m.BreakerState = f(prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmgateway_circuit_breaker_state", Help: "Circuit breaker state per provider (0=CLOSED, 1=HALF-OPEN, 2=OPEN).",
	}, []string{"provider"})).(*prometheus.GaugeVec)
	m.BreakerTransitions = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_circuit_breaker_transitions_total", Help: "Circuit breaker transitions.",
	}, []string{"provider", "from", "to"})).(*prometheus.CounterVec)
	m.Failovers = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_provider_failovers_total", Help: "Requests served by a fallback provider.",
	}, []string{"from", "to"})).(*prometheus.CounterVec)
	m.TokensProcessed = f(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmgateway_tokens_total", Help: "Tokens processed upstream by provider and kind.",
	}, []string{"provider", "kind"})).(*prometheus.CounterVec)
	return m
}

// BreakerGaugeValue maps a state name to the gauge encoding.
func BreakerGaugeValue(state string) float64 {
	switch state {
	case "HALF-OPEN":
		return 1
	case "OPEN":
		return 2
	}
	return 0
}
