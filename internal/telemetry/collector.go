package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Trace is one request record shown in the live audit table.
type Trace struct {
	ID               string   `json:"id"`
	Timestamp        string   `json:"timestamp"`
	TenantID         string   `json:"tenant_id"`
	Model            string   `json:"model"`
	Provider         string   `json:"provider"`
	CacheStatus      string   `json:"cache_status"`
	Similarity       float64  `json:"similarity"`
	Status           int      `json:"status"`
	LatencyMs        float64  `json:"latency_ms"`
	OverheadMs       float64  `json:"overhead_ms"`
	UpstreamMs       float64  `json:"upstream_ms"`
	GuardrailAction  string   `json:"guardrail_action"`
	GuardrailTypes   []string `json:"guardrail_types,omitempty"`
	InjectionScore   float64  `json:"injection_score,omitempty"`
	Failover         bool     `json:"failover"`
	Stream           bool     `json:"stream"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TokensSaved      int      `json:"tokens_saved"`
	CostSavedUSD     float64  `json:"cost_saved_usd"`
	Error            string   `json:"error,omitempty"`
}

// LatencyStats are percentile summaries in milliseconds.
type LatencyStats struct {
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Mean  float64 `json:"mean"`
	Count int     `json:"count"`
}

// Summary is the dashboard payload for /v1/telemetry/summary and the stream.
type Summary struct {
	GeneratedAt       string            `json:"generated_at"`
	UptimeSeconds     float64           `json:"uptime_seconds"`
	TotalRequests     int64             `json:"total_requests"`
	ActiveConnections int64             `json:"active_connections"`
	RequestsPerSecond float64           `json:"requests_per_second"`
	StatusCodes       map[string]int64  `json:"status_codes"`
	Cache             CacheSummary      `json:"cache"`
	Latency           LatencySummary    `json:"latency"`
	Guardrails        GuardSummary      `json:"guardrails"`
	RateLimited       int64             `json:"rate_limited"`
	RateLimitDegraded int64             `json:"rate_limit_degraded"`
	Failovers         int64             `json:"failovers"`
	Providers         any               `json:"providers"`
	RecentTraces      []Trace           `json:"recent_traces"`
	Backends          map[string]string `json:"backends,omitempty"`
}

// CacheSummary covers cache effectiveness.
type CacheSummary struct {
	HitsExact     int64   `json:"hits_exact"`
	HitsSemantic  int64   `json:"hits_semantic"`
	Misses        int64   `json:"misses"`
	Bypass        int64   `json:"bypass"`
	HitRatio      float64 `json:"hit_ratio"`
	SemanticRatio float64 `json:"semantic_hit_ratio"`
	TokensSaved   int64   `json:"tokens_saved"`
	CostSavedUSD  float64 `json:"cost_saved_usd"`
	AvgSimilarity float64 `json:"avg_semantic_similarity"`
}

// LatencySummary splits gateway overhead from upstream latency.
type LatencySummary struct {
	Overhead      LatencyStats `json:"gateway_overhead_ms"`
	Upstream      LatencyStats `json:"upstream_ms"`
	Total         LatencyStats `json:"total_ms"`
	CacheHitTotal LatencyStats `json:"cache_hit_total_ms"`
	MissTotal     LatencyStats `json:"cache_miss_total_ms"`
}

// GuardSummary counts guardrail interventions.
type GuardSummary struct {
	Total      int64            `json:"total"`
	Blocked    int64            `json:"blocked"`
	Redacted   int64            `json:"redacted"`
	Flagged    int64            `json:"flagged"`
	ByCategory map[string]int64 `json:"by_category"`
}

// Collector aggregates rolling in-memory statistics.
type Collector struct {
	start     time.Time
	window    int
	providers func() any
	backends  map[string]string
	memoTTL   time.Duration

	mu          sync.Mutex
	total       int64
	active      int64
	codes       map[string]int64
	hitsExact   int64
	hitsSem     int64
	misses      int64
	bypass      int64
	tokensSaved int64
	costSaved   float64
	simSum      float64
	guard       GuardSummary
	rateLimited int64
	degraded    int64
	failovers   int64
	overhead    *ring
	upstream    *ring
	totalLat    *ring
	hitLat      *ring
	missLat     *ring
	recent      []Trace
	recentPos   int
	perSecond   [60]int64
	perSecondTS [60]int64

	memo   []byte
	memoAt time.Time
}

const recentTraces = 50

// NewCollector creates a collector keeping `window` latency samples per series.
func NewCollector(window int) *Collector {
	if window <= 0 {
		window = 10000
	}
	return &Collector{
		start: time.Now(), window: window, memoTTL: 250 * time.Millisecond,
		codes:    map[string]int64{},
		guard:    GuardSummary{ByCategory: map[string]int64{}},
		overhead: newRing(window), upstream: newRing(window), totalLat: newRing(window),
		hitLat: newRing(window), missLat: newRing(window),
	}
}

// SetProviderSource registers a callback returning provider health.
func (c *Collector) SetProviderSource(f func() any) { c.providers = f }

// SetBackends records which storage backends are active (shown in the UI).
func (c *Collector) SetBackends(b map[string]string) { c.backends = b }

// ConnOpened / ConnClosed track in-flight requests.
func (c *Collector) ConnOpened() { c.mu.Lock(); c.active++; c.mu.Unlock() }
func (c *Collector) ConnClosed() { c.mu.Lock(); c.active--; c.mu.Unlock() }

// RateLimitDegraded counts a fallback decision.
func (c *Collector) RateLimitDegraded() { c.mu.Lock(); c.degraded++; c.mu.Unlock() }

// Record ingests a finished request trace.
func (c *Collector) Record(t Trace) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	now := time.Now().Unix()
	slot := now % 60
	if c.perSecondTS[slot] != now {
		c.perSecondTS[slot], c.perSecond[slot] = now, 0
	}
	c.perSecond[slot]++
	c.codes[fmt.Sprintf("%dxx", t.Status/100)]++

	switch t.CacheStatus {
	case "HIT-EXACT":
		c.hitsExact++
	case "HIT-SEMANTIC":
		c.hitsSem++
		c.simSum += t.Similarity
	case "MISS":
		c.misses++
	case "BYPASS":
		c.bypass++
	}
	c.tokensSaved += int64(t.TokensSaved)
	c.costSaved += t.CostSavedUSD

	switch t.GuardrailAction {
	case "BLOCK":
		c.guard.Blocked++
	case "REDACT":
		c.guard.Redacted++
	case "FLAG":
		c.guard.Flagged++
	}
	if t.GuardrailAction != "" && t.GuardrailAction != "NONE" {
		c.guard.Total++
		for _, g := range t.GuardrailTypes {
			c.guard.ByCategory[g]++
		}
	}
	if t.Status == http.StatusTooManyRequests {
		c.rateLimited++
	}
	if t.Failover {
		c.failovers++
	}
	if t.Status < 400 {
		c.totalLat.add(t.LatencyMs)
		c.overhead.add(t.OverheadMs)
		if t.UpstreamMs > 0 {
			c.upstream.add(t.UpstreamMs)
		}
		if t.CacheStatus == "HIT-EXACT" || t.CacheStatus == "HIT-SEMANTIC" {
			c.hitLat.add(t.LatencyMs)
		} else {
			c.missLat.add(t.LatencyMs)
		}
	}
	if len(c.recent) < recentTraces {
		c.recent = append(c.recent, t)
	} else {
		c.recent[c.recentPos] = t
		c.recentPos = (c.recentPos + 1) % recentTraces
	}
}

// Summary builds the dashboard snapshot.
func (c *Collector) Summary() Summary {
	c.mu.Lock()
	s := Summary{
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		UptimeSeconds:     round(time.Since(c.start).Seconds(), 1),
		TotalRequests:     c.total,
		ActiveConnections: c.active,
		StatusCodes:       copyMap(c.codes),
		RateLimited:       c.rateLimited,
		RateLimitDegraded: c.degraded,
		Failovers:         c.failovers,
		Backends:          c.backends,
	}
	lookups := c.hitsExact + c.hitsSem + c.misses
	s.Cache = CacheSummary{
		HitsExact: c.hitsExact, HitsSemantic: c.hitsSem, Misses: c.misses, Bypass: c.bypass,
		TokensSaved: c.tokensSaved, CostSavedUSD: round(c.costSaved, 6),
	}
	if lookups > 0 {
		s.Cache.HitRatio = round(float64(c.hitsExact+c.hitsSem)/float64(lookups), 4)
		s.Cache.SemanticRatio = round(float64(c.hitsSem)/float64(lookups), 4)
	}
	if c.hitsSem > 0 {
		s.Cache.AvgSimilarity = round(c.simSum/float64(c.hitsSem), 4)
	}
	g := c.guard
	g.ByCategory = copyMap(c.guard.ByCategory)
	s.Guardrails = g

	now := time.Now().Unix()
	var last10 int64
	for i := int64(1); i <= 10; i++ { // completed seconds only
		slot := (now - i) % 60
		if c.perSecondTS[slot] == now-i {
			last10 += c.perSecond[slot]
		}
	}
	s.RequestsPerSecond = round(float64(last10)/10, 2)

	ov, up, tot, hit, miss := c.overhead.snapshot(), c.upstream.snapshot(), c.totalLat.snapshot(), c.hitLat.snapshot(), c.missLat.snapshot()
	// newest first
	s.RecentTraces = make([]Trace, 0, len(c.recent))
	for i := 0; i < len(c.recent); i++ {
		idx := (c.recentPos - 1 - i + 2*len(c.recent)) % len(c.recent)
		if len(c.recent) < recentTraces {
			idx = len(c.recent) - 1 - i
		}
		s.RecentTraces = append(s.RecentTraces, c.recent[idx])
	}
	c.mu.Unlock()

	s.Latency = LatencySummary{Overhead: stats(ov), Upstream: stats(up), Total: stats(tot), CacheHitTotal: stats(hit), MissTotal: stats(miss)}
	if c.providers != nil {
		s.Providers = c.providers()
	}
	return s
}

// SummaryJSON returns the serialized summary, memoized briefly so many
// dashboard clients do not each re-sort the sample windows.
func (c *Collector) SummaryJSON() []byte {
	c.mu.Lock()
	if c.memo != nil && time.Since(c.memoAt) < c.memoTTL {
		b := c.memo
		c.mu.Unlock()
		return b
	}
	c.mu.Unlock()
	b, _ := json.Marshal(c.Summary())
	c.mu.Lock()
	c.memo, c.memoAt = b, time.Now()
	c.mu.Unlock()
	return b
}

// SummaryHandler serves GET /v1/telemetry/summary.
func (c *Collector) SummaryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(c.SummaryJSON())
	}
}

// StreamHandler serves GET /v1/telemetry/stream as Server-Sent Events,
// pushing a summary every interval.
func (c *Collector) StreamHandler(interval time.Duration) http.HandlerFunc {
	if interval <= 0 {
		interval = time.Second
	}
	return func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		send := func() bool {
			if _, err := fmt.Fprintf(w, "event: summary\ndata: %s\n\n", c.SummaryJSON()); err != nil {
				return false
			}
			f.Flush()
			return true
		}
		if !send() {
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				if !send() {
					return
				}
			}
		}
	}
}

// ------------------------------------------------------------- helpers ---

type ring struct {
	buf  []float64
	pos  int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]float64, n)} }

func (r *ring) add(v float64) {
	r.buf[r.pos] = v
	r.pos++
	if r.pos == len(r.buf) {
		r.pos, r.full = 0, true
	}
}

func (r *ring) snapshot() []float64 {
	n := r.pos
	if r.full {
		n = len(r.buf)
	}
	out := make([]float64, n)
	copy(out, r.buf[:n])
	return out
}

func stats(v []float64) LatencyStats {
	if len(v) == 0 {
		return LatencyStats{}
	}
	sort.Float64s(v)
	var sum float64
	for _, x := range v {
		sum += x
	}
	return LatencyStats{
		P50: round(Percentile(v, 50), 3), P95: round(Percentile(v, 95), 3), P99: round(Percentile(v, 99), 3),
		Mean: round(sum/float64(len(v)), 3), Count: len(v),
	}
}

// Percentile returns the p-th percentile (nearest-rank) of sorted values.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func round(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
