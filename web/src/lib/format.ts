import type { BreakerState, CacheStatus } from "./types";

export type Tone = "ok" | "warn" | "bad" | "info" | "muted";

/** 1234 -> "1,234"; 2_500_000 -> "2.5M". */
export function formatNumber(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  const abs = Math.abs(n);
  if (abs >= 1e9) return `${trim(n / 1e9)}B`;
  if (abs >= 1e6) return `${trim(n / 1e6)}M`;
  return Math.round(n).toLocaleString("en-US");
}

/** Ratio in [0,1] -> "61.9%". */
export function formatPercent(ratio: number | null | undefined, digits = 1): string {
  if (ratio == null || !Number.isFinite(ratio)) return "—";
  return `${(ratio * 100).toFixed(digits)}%`;
}

/** USD with adaptive precision: small savings keep 4 decimals. */
export function formatUSD(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return "—";
  if (v === 0) return "$0.00";
  if (Math.abs(v) < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

/** Milliseconds with adaptive precision: 0.42 ms, 12.3 ms, 1.23 s. */
export function formatMs(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms)) return "—";
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)} s`;
  if (ms >= 100) return `${Math.round(ms)} ms`;
  if (ms >= 10) return `${ms.toFixed(1)} ms`;
  return `${ms.toFixed(2)} ms`;
}

/** ISO timestamp -> local HH:MM:SS. */
export function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleTimeString("en-GB", { hour12: false });
}

export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

export function breakerTone(state: BreakerState | string): Tone {
  switch (state) {
    case "CLOSED":
      return "ok";
    case "HALF-OPEN":
      return "warn";
    case "OPEN":
      return "bad";
    default:
      return "muted";
  }
}

export function cacheTone(status: CacheStatus | string): Tone {
  switch (status) {
    case "HIT-EXACT":
      return "ok";
    case "HIT-SEMANTIC":
      return "info";
    case "MISS":
      return "warn";
    default:
      return "muted";
  }
}

export function statusTone(code: number): Tone {
  if (code >= 500) return "bad";
  if (code === 429 || code >= 400) return "warn";
  if (code >= 200 && code < 300) return "ok";
  return "muted";
}

export function guardrailTone(action: string): Tone {
  switch (action) {
    case "BLOCK":
      return "bad";
    case "REDACT":
      return "warn";
    case "FLAG":
      return "info";
    default:
      return "muted";
  }
}

/** Width (0–100) of a bar for value relative to max, with a visible floor. */
export function barWidth(value: number, maxValue: number, floor = 2): number {
  if (!Number.isFinite(value) || value <= 0 || !Number.isFinite(maxValue) || maxValue <= 0) return 0;
  return Math.min(100, Math.max(floor, (value / maxValue) * 100));
}

function trim(n: number): string {
  return n.toFixed(1).replace(/\.0$/, "");
}
