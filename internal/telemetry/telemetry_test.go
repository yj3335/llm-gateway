package telemetry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	v := make([]float64, 100)
	for i := range v {
		v[i] = float64(i + 1)
	}
	if Percentile(v, 50) != 50 || Percentile(v, 95) != 95 || Percentile(v, 99) != 99 || Percentile(v, 100) != 100 {
		t.Fatal("nearest-rank percentile mismatch")
	}
	if Percentile(nil, 50) != 0 {
		t.Fatal("empty percentile")
	}
}

func TestCollectorSummary(t *testing.T) {
	c := NewCollector(100)
	c.SetProviderSource(func() any { return []string{"openai"} })
	for i := 0; i < 60; i++ {
		c.Record(Trace{ID: string(rune('a' + i%26)), Status: 200, CacheStatus: "MISS", LatencyMs: 100, OverheadMs: 1, UpstreamMs: 99})
	}
	c.Record(Trace{Status: 200, CacheStatus: "HIT-EXACT", LatencyMs: 1, OverheadMs: 1, TokensSaved: 50, CostSavedUSD: 0.01})
	c.Record(Trace{Status: 200, CacheStatus: "HIT-SEMANTIC", Similarity: 0.9, LatencyMs: 2, OverheadMs: 2, TokensSaved: 50, CostSavedUSD: 0.01, GuardrailAction: "REDACT", GuardrailTypes: []string{"SSN"}})
	c.Record(Trace{Status: 400, CacheStatus: "NONE", GuardrailAction: "BLOCK", GuardrailTypes: []string{"PROMPT_INJECTION"}})
	c.Record(Trace{ID: "newest", Status: 429, CacheStatus: "NONE"})
	c.Record(Trace{Status: 200, CacheStatus: "MISS", Failover: true, LatencyMs: 100, OverheadMs: 1, UpstreamMs: 99})

	s := c.Summary()
	if s.TotalRequests != 65 || s.Cache.HitsExact != 1 || s.Cache.HitsSemantic != 1 || s.Cache.Misses != 61 {
		t.Fatalf("counts: %+v", s.Cache)
	}
	if s.Cache.HitRatio != round(2.0/63, 4) || s.Cache.TokensSaved != 100 || s.Cache.AvgSimilarity != 0.9 {
		t.Fatalf("ratios: %+v", s.Cache)
	}
	if s.Guardrails.Total != 2 || s.Guardrails.Blocked != 1 || s.Guardrails.ByCategory["SSN"] != 1 || s.RateLimited != 1 || s.Failovers != 1 {
		t.Fatalf("guard/ratelimit: %+v %d %d", s.Guardrails, s.RateLimited, s.Failovers)
	}
	if len(s.RecentTraces) != recentTraces || s.RecentTraces[1].ID != "newest" {
		t.Fatalf("recent traces order: len=%d second=%q", len(s.RecentTraces), s.RecentTraces[1].ID)
	}
	if s.Latency.Upstream.P50 != 99 || s.Latency.CacheHitTotal.Count != 2 || s.StatusCodes["4xx"] != 2 {
		t.Fatalf("latency: %+v codes=%v", s.Latency, s.StatusCodes)
	}
	var round map[string]any
	if err := json.Unmarshal(c.SummaryJSON(), &round); err != nil || round["providers"] == nil {
		t.Fatalf("summary json: %v", err)
	}
}

func TestAuditorJSONLines(t *testing.T) {
	var buf bytes.Buffer
	a := NewAuditor(&buf, nil)
	a.Request(Trace{ID: "r1", TenantID: "t", Status: 200, CacheStatus: "MISS"})
	a.Guardrail(GuardrailEvent{RequestID: "r1", TenantID: "t", Direction: "input", Category: "SSN", Action: "REDACT", At: time.Now()})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", buf.String())
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["request_id"] != "r1" {
			t.Fatalf("bad audit line %q", l)
		}
	}
	a.Close()
}

func TestMetricsRegistry(t *testing.T) {
	m := NewMetrics()
	m.GuardrailTriggers.WithLabelValues("SSN", "REDACT").Inc()
	if _, err := m.Registry.Gather(); err != nil {
		t.Fatal(err)
	}
	if BreakerGaugeValue("OPEN") != 2 || BreakerGaugeValue("HALF-OPEN") != 1 || BreakerGaugeValue("CLOSED") != 0 {
		t.Fatal("breaker gauge mapping")
	}
}
