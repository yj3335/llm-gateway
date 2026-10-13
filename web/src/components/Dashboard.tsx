"use client";

import { AuditTable } from "./AuditTable";
import { Badge, Panel } from "./Badge";
import { KpiCards } from "./KpiCards";
import { Playground } from "./Playground";
import { ProviderMatrix } from "./ProviderMatrix";
import { type ConnectionState, useTelemetry } from "@/hooks/useTelemetry";
import { GATEWAY_URL } from "@/lib/api";
import { formatMs, formatNumber, formatUptime, type Tone } from "@/lib/format";
import type { Summary } from "@/lib/types";

const connTone: Record<ConnectionState, Tone> = { connecting: "muted", live: "ok", polling: "warn", offline: "bad" };
const connLabel: Record<ConnectionState, string> = { connecting: "CONNECTING", live: "LIVE · SSE", polling: "POLLING", offline: "OFFLINE" };

export function Dashboard() {
  const { summary, state, error } = useTelemetry();
  return (
    <main className="mx-auto max-w-[1440px] space-y-4 px-4 py-5 md:px-6">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-ink">LLM Gateway, Cache &amp; Guardrails</h1>
          <p className="text-xs text-dim">Real-time observability control plane · {GATEWAY_URL}</p>
        </div>
        <div className="flex items-center gap-2 text-xs text-dim">
          {summary ? <span className="tabular">uptime {formatUptime(summary.uptime_seconds)}</span> : null}
          <Badge tone={connTone[state]} testId="connection-state">
            {connLabel[state]}
          </Badge>
        </div>
      </header>
      {state === "offline" && error ? (
        <p data-testid="offline-banner" className="rounded-md border border-bad/40 bg-bad/10 px-3 py-2 text-xs text-bad">
          Cannot reach the gateway telemetry API ({error}). Start it with <code className="font-mono">make up</code> or{" "}
          <code className="font-mono">make run</code>.
        </p>
      ) : null}
      <KpiCards summary={summary} />
      <div className="grid gap-4 xl:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
        <Playground />
        <div className="space-y-4">
          <ProviderMatrix providers={summary?.providers ?? []} />
          <TrafficPanel summary={summary} />
        </div>
      </div>
      <AuditTable traces={summary?.recent_traces ?? []} />
    </main>
  );
}

function TrafficPanel({ summary }: { summary: Summary | null }) {
  const cats = Object.entries(summary?.guardrails.by_category ?? {}).sort((a, b) => b[1] - a[1]);
  const lat = summary?.latency;
  return (
    <Panel title="Latency & Guardrails" subtitle="Gateway overhead vs. upstream, interventions by category" testId="traffic-panel">
      <dl className="tabular grid grid-cols-2 gap-x-4 gap-y-1 font-mono text-[11px]">
        <dt className="text-faint">overhead p50 / p99</dt>
        <dd className="text-right text-ink">
          {formatMs(lat?.gateway_overhead_ms.p50)} / {formatMs(lat?.gateway_overhead_ms.p99)}
        </dd>
        <dt className="text-faint">upstream p50 / p95</dt>
        <dd className="text-right text-ink">
          {formatMs(lat?.upstream_ms.p50)} / {formatMs(lat?.upstream_ms.p95)}
        </dd>
        <dt className="text-faint">cache hit p50</dt>
        <dd className="text-right text-ink">{formatMs(lat?.cache_hit_total_ms.p50)}</dd>
        <dt className="text-faint">cache miss p50</dt>
        <dd className="text-right text-ink">{formatMs(lat?.cache_miss_total_ms.p50)}</dd>
        <dt className="text-faint">failovers / 429s</dt>
        <dd className="text-right text-ink">
          {formatNumber(summary?.failovers)} / {formatNumber(summary?.rate_limited)}
        </dd>
      </dl>
      <div className="mt-3 flex flex-wrap gap-1.5" data-testid="guardrail-categories">
        {cats.length === 0 ? (
          <span className="text-xs text-faint">No guardrail interventions yet.</span>
        ) : (
          cats.map(([c, n]) => (
            <Badge key={c} tone={c === "PROMPT_INJECTION" ? "bad" : "warn"}>
              {c} {n}
            </Badge>
          ))
        )}
      </div>
    </Panel>
  );
}
