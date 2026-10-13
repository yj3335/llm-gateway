import type { Summary } from "@/lib/types";

export const summaryFixture: Summary = {
  generated_at: "2026-10-13T23:30:00Z",
  uptime_seconds: 3725,
  total_requests: 12345,
  active_connections: 7,
  requests_per_second: 152.4,
  status_codes: { "2xx": 12000, "4xx": 345 },
  cache: {
    hits_exact: 4200,
    hits_semantic: 2600,
    misses: 4200,
    bypass: 10,
    hit_ratio: 0.6182,
    semantic_hit_ratio: 0.2364,
    tokens_saved: 812345,
    cost_saved_usd: 4.3127,
    avg_semantic_similarity: 0.9412,
  },
  latency: {
    gateway_overhead_ms: { p50: 0.89, p95: 3.25, p99: 24.4, mean: 1.4, count: 12000 },
    upstream_ms: { p50: 167.1, p95: 185.3, p99: 210.1, mean: 168, count: 4200 },
    total_ms: { p50: 1.2, p95: 180.2, p99: 205, mean: 64, count: 12000 },
    cache_hit_total_ms: { p50: 0.51, p95: 2.77, p99: 3.78, mean: 0.8, count: 6800 },
    cache_miss_total_ms: { p50: 170.5, p95: 187.8, p99: 210.1, mean: 171, count: 4200 },
  },
  guardrails: { total: 673, blocked: 112, redacted: 561, flagged: 0, by_category: { SSN: 300, CREDIT_CARD: 261, PROMPT_INJECTION: 112 } },
  rate_limited: 3,
  rate_limit_degraded: 0,
  failovers: 156,
  providers: [
    { name: "openai", type: "openai", state: "OPEN", requests: 900, failures: 120, rejected: 40, error_rate: 0.13, latency_ewma_ms: 170, latency_p50_ms: 168, latency_p95_ms: 186, last_error: "openai: upstream status 503", last_error_ago_s: 1.2 },
    { name: "anthropic", type: "anthropic", state: "HALF-OPEN", requests: 300, failures: 0, rejected: 0, error_rate: 0, latency_ewma_ms: 171, latency_p50_ms: 169, latency_p95_ms: 184 },
    { name: "gemini", type: "gemini", state: "CLOSED", requests: 20, failures: 0, rejected: 0, error_rate: 0, latency_ewma_ms: 160, latency_p50_ms: 158, latency_p95_ms: 170 },
    { name: "ollama", type: "ollama", state: "CLOSED", requests: 0, failures: 0, rejected: 0, error_rate: 0, latency_ewma_ms: 0, latency_p50_ms: 0, latency_p95_ms: 0 },
  ],
  recent_traces: [
    { id: "a1", timestamp: "2026-10-13T23:29:59.100Z", tenant_id: "demo", model: "gpt-4o-mini", provider: "cache", cache_status: "HIT-SEMANTIC", similarity: 0.9641, status: 200, latency_ms: 0.92, overhead_ms: 0.92, upstream_ms: 0, guardrail_action: "NONE", failover: false, stream: false, prompt_tokens: 12, completion_tokens: 48, tokens_saved: 60, cost_saved_usd: 0.0005 },
    { id: "a2", timestamp: "2026-10-13T23:29:58.000Z", tenant_id: "demo", model: "gpt-4o-mini", provider: "anthropic", cache_status: "MISS", similarity: 0, status: 200, latency_ms: 174.1, overhead_ms: 0.76, upstream_ms: 173.3, guardrail_action: "REDACT", guardrail_types: ["CREDIT_CARD", "SSN"], failover: true, stream: false, prompt_tokens: 20, completion_tokens: 48, tokens_saved: 0, cost_saved_usd: 0 },
    { id: "a3", timestamp: "2026-10-13T23:29:57.000Z", tenant_id: "strict-health", model: "gpt-4o-mini", provider: "", cache_status: "NONE", similarity: 0, status: 400, latency_ms: 0.65, overhead_ms: 0.65, upstream_ms: 0, guardrail_action: "BLOCK", guardrail_types: ["PROMPT_INJECTION"], injection_score: 0.99, failover: false, stream: false, prompt_tokens: 0, completion_tokens: 0, tokens_saved: 0, cost_saved_usd: 0, error: "PROMPT_INJECTION" },
  ],
  backends: { cache_l2: "pgvector", ratelimit: "redis-lua" },
};
