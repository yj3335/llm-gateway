// Mirrors internal/telemetry.Summary served by GET /v1/telemetry/summary.

export interface LatencyStats {
  p50: number;
  p95: number;
  p99: number;
  mean: number;
  count: number;
}

export interface ProviderHealth {
  name: string;
  type: string;
  state: BreakerState;
  requests: number;
  failures: number;
  rejected: number;
  error_rate: number;
  latency_ewma_ms: number;
  latency_p50_ms: number;
  latency_p95_ms: number;
  last_error?: string;
  last_error_ago_s?: number;
}

export type BreakerState = "CLOSED" | "HALF-OPEN" | "OPEN";

export type CacheStatus = "HIT-EXACT" | "HIT-SEMANTIC" | "MISS" | "BYPASS" | "NONE";

export interface Trace {
  id: string;
  timestamp: string;
  tenant_id: string;
  model: string;
  provider: string;
  cache_status: CacheStatus;
  similarity: number;
  status: number;
  latency_ms: number;
  overhead_ms: number;
  upstream_ms: number;
  guardrail_action: "NONE" | "REDACT" | "BLOCK" | "FLAG";
  guardrail_types?: string[];
  injection_score?: number;
  failover: boolean;
  stream: boolean;
  prompt_tokens: number;
  completion_tokens: number;
  tokens_saved: number;
  cost_saved_usd: number;
  error?: string;
}

export interface Summary {
  generated_at: string;
  uptime_seconds: number;
  total_requests: number;
  active_connections: number;
  requests_per_second: number;
  status_codes: Record<string, number>;
  cache: {
    hits_exact: number;
    hits_semantic: number;
    misses: number;
    bypass: number;
    hit_ratio: number;
    semantic_hit_ratio: number;
    tokens_saved: number;
    cost_saved_usd: number;
    avg_semantic_similarity: number;
  };
  latency: {
    gateway_overhead_ms: LatencyStats;
    upstream_ms: LatencyStats;
    total_ms: LatencyStats;
    cache_hit_total_ms: LatencyStats;
    cache_miss_total_ms: LatencyStats;
  };
  guardrails: {
    total: number;
    blocked: number;
    redacted: number;
    flagged: number;
    by_category: Record<string, number>;
  };
  rate_limited: number;
  rate_limit_degraded: number;
  failovers: number;
  providers: ProviderHealth[] | null;
  recent_traces: Trace[] | null;
  backends?: Record<string, string>;
}
