import { formatMs, formatNumber, formatPercent, formatUSD } from "@/lib/format";
import type { Summary } from "@/lib/types";

interface Kpi {
  id: string;
  label: string;
  value: string;
  detail: string;
}

export function buildKpis(s: Summary | null): Kpi[] {
  if (!s) {
    return [
      { id: "requests", label: "Total Requests", value: "—", detail: "waiting for gateway" },
      { id: "hit-ratio", label: "Semantic Cache Hit Ratio", value: "—", detail: "exact · semantic" },
      { id: "saved", label: "Est. Token Spend Saved", value: "—", detail: "tokens saved" },
      { id: "overhead", label: "p95 Gateway Overhead", value: "—", detail: "p50 · p99" },
      { id: "guardrails", label: "Guardrail Interventions", value: "—", detail: "blocked · redacted" },
    ];
  }
  const ov = s.latency.gateway_overhead_ms;
  return [
    {
      id: "requests",
      label: "Total Requests",
      value: formatNumber(s.total_requests),
      detail: `${s.requests_per_second.toFixed(1)} req/s · ${s.active_connections} in flight`,
    },
    {
      id: "hit-ratio",
      label: "Semantic Cache Hit Ratio",
      value: formatPercent(s.cache.hit_ratio),
      detail: `exact ${formatNumber(s.cache.hits_exact)} · semantic ${formatNumber(s.cache.hits_semantic)} · miss ${formatNumber(s.cache.misses)}`,
    },
    {
      id: "saved",
      label: "Est. Token Spend Saved",
      value: formatUSD(s.cache.cost_saved_usd),
      detail: `${formatNumber(s.cache.tokens_saved)} tokens saved`,
    },
    {
      id: "overhead",
      label: "p95 Gateway Overhead",
      value: formatMs(ov.p95),
      detail: `p50 ${formatMs(ov.p50)} · p99 ${formatMs(ov.p99)}`,
    },
    {
      id: "guardrails",
      label: "Guardrail Interventions",
      value: formatNumber(s.guardrails.total),
      detail: `${formatNumber(s.guardrails.blocked)} blocked · ${formatNumber(s.guardrails.redacted)} redacted`,
    },
  ];
}

export function KpiCards({ summary }: { summary: Summary | null }) {
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
      {buildKpis(summary).map((k) => (
        <div key={k.id} data-testid={`kpi-${k.id}`} className="rounded-lg border border-line bg-panel px-4 py-3">
          <div className="text-xs text-dim">{k.label}</div>
          <div data-testid={`kpi-${k.id}-value`} className="tabular mt-1 text-2xl font-semibold tracking-tight text-ink">
            {k.value}
          </div>
          <div className="tabular mt-1 truncate text-[11px] text-faint">{k.detail}</div>
        </div>
      ))}
    </div>
  );
}
