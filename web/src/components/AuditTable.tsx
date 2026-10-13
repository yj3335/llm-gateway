import { Badge, Panel } from "./Badge";
import { cacheTone, formatMs, formatTime, guardrailTone, statusTone } from "@/lib/format";
import type { Trace } from "@/lib/types";

export function AuditTable({ traces }: { traces: Trace[] }) {
  return (
    <Panel title="Live Request Audit Stream" subtitle="Most recent 50 requests, newest first" testId="audit-table">
      {traces.length === 0 ? (
        <p className="text-sm text-dim">No requests yet. Send one from the playground.</p>
      ) : (
        <div className="-mx-4 overflow-x-auto">
          <table className="w-full min-w-[880px] text-left text-xs">
            <thead className="text-[11px] uppercase tracking-wide text-faint">
              <tr className="border-b border-line">
                <th className="px-4 py-2 font-medium">Time</th>
                <th className="px-2 py-2 font-medium">Tenant</th>
                <th className="px-2 py-2 font-medium">Model</th>
                <th className="px-2 py-2 font-medium">Provider</th>
                <th className="px-2 py-2 font-medium">Cache</th>
                <th className="px-2 py-2 text-right font-medium">Similarity</th>
                <th className="px-2 py-2 font-medium">Guardrail</th>
                <th className="px-2 py-2 text-right font-medium">Status</th>
                <th className="px-2 py-2 text-right font-medium">Latency</th>
                <th className="px-4 py-2 text-right font-medium">Overhead</th>
              </tr>
            </thead>
            <tbody>
              {traces.map((t) => (
                <tr key={t.id} data-testid="audit-row" className="border-b border-line/60 font-mono text-[11px] text-dim hover:bg-panel-2">
                  <td className="tabular px-4 py-1.5 text-faint">{formatTime(t.timestamp)}</td>
                  <td className="max-w-[120px] truncate px-2 py-1.5 text-ink" title={t.tenant_id}>
                    {t.tenant_id}
                  </td>
                  <td className="px-2 py-1.5">{t.model}</td>
                  <td className="px-2 py-1.5">
                    {t.provider || "—"}
                    {t.failover ? <span className="ml-1 text-warn" title="served by fallback provider">↪</span> : null}
                  </td>
                  <td className="px-2 py-1.5">
                    {t.cache_status === "NONE" ? <span className="text-faint">—</span> : <Badge tone={cacheTone(t.cache_status)}>{t.cache_status}</Badge>}
                  </td>
                  <td className="tabular px-2 py-1.5 text-right">{t.cache_status === "HIT-SEMANTIC" ? t.similarity.toFixed(3) : "—"}</td>
                  <td className="px-2 py-1.5">
                    {t.guardrail_action === "NONE" ? (
                      <span className="text-faint">—</span>
                    ) : (
                      <span className="flex items-center gap-1">
                        <Badge tone={guardrailTone(t.guardrail_action)}>{t.guardrail_action}</Badge>
                        <span className="truncate text-faint">{t.guardrail_types?.join(",")}</span>
                      </span>
                    )}
                  </td>
                  <td className="px-2 py-1.5 text-right">
                    <Badge tone={statusTone(t.status)}>{t.status}</Badge>
                  </td>
                  <td className="tabular px-2 py-1.5 text-right text-ink">{formatMs(t.latency_ms)}</td>
                  <td className="tabular px-4 py-1.5 text-right">{formatMs(t.overhead_ms)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}
