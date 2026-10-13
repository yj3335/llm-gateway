import { Badge, Panel } from "./Badge";
import { barWidth, breakerTone, formatMs, formatNumber, formatPercent } from "@/lib/format";
import type { ProviderHealth } from "@/lib/types";

export function ProviderMatrix({ providers }: { providers: ProviderHealth[] }) {
  const maxLatency = Math.max(1, ...providers.map((p) => p.latency_p95_ms || p.latency_ewma_ms || 0));
  return (
    <Panel title="Provider Health & Circuit Breakers" subtitle="State machine per upstream: CLOSED → OPEN → HALF-OPEN" testId="provider-matrix">
      {providers.length === 0 ? (
        <p className="text-sm text-dim">No provider data yet.</p>
      ) : (
        <ul className="space-y-3">
          {providers.map((p) => (
            <li key={p.name} data-testid={`provider-${p.name}`} className="rounded-md border border-line bg-panel-2 px-3 py-2.5">
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-baseline gap-2">
                  <span className="font-medium text-ink">{p.name}</span>
                  <span className="font-mono text-[11px] text-faint">{p.type}</span>
                </div>
                <Badge tone={breakerTone(p.state)} testId={`breaker-${p.name}`}>
                  {p.state}
                </Badge>
              </div>
              <div className="mt-2 flex items-center gap-2">
                <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-line" aria-label={`${p.name} latency`}>
                  <div className="h-full rounded-full bg-accent" style={{ width: `${barWidth(p.latency_p50_ms, maxLatency)}%` }} />
                </div>
                <span className="tabular w-28 text-right font-mono text-[11px] text-dim">
                  p50 {formatMs(p.latency_p50_ms)}
                </span>
              </div>
              <div className="tabular mt-1.5 flex flex-wrap gap-x-4 gap-y-1 font-mono text-[11px] text-faint">
                <span>req {formatNumber(p.requests)}</span>
                <span>err {formatPercent(p.error_rate)}</span>
                <span>short-circuited {formatNumber(p.rejected)}</span>
                <span>p95 {formatMs(p.latency_p95_ms)}</span>
              </div>
              {p.last_error && p.state !== "CLOSED" ? (
                <p className="mt-1 truncate font-mono text-[11px] text-bad" title={p.last_error}>
                  {p.last_error}
                </p>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}
