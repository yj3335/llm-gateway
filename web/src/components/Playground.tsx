"use client";

import { useRef, useState } from "react";
import { Badge, Panel } from "./Badge";
import { GATEWAY_URL, mintToken } from "@/lib/api";
import { barWidth, cacheTone, formatMs, statusTone } from "@/lib/format";
import {
  GATEWAY_HEADERS,
  type LatencyBreakdown,
  type PlaygroundOptions,
  latencyFromHeaders,
  parseSSEContent,
  planRequests,
} from "@/lib/playground";

export interface PlaygroundResult {
  label: string;
  status: number;
  headers: Record<string, string>;
  latency: LatencyBreakdown;
  content: string;
  error?: string;
}

// Local Ollama models first (configs/gateway.yaml); the others route through the
// mock upstream (configs/gateway.mock.yaml) or map to the default local route.
const MODELS = ["qwen3.5:9b", "gemma4:e4b", "gpt-4o-mini", "claude-3-5-haiku-latest", "gemini-2.0-flash"];

const TOGGLES: { key: keyof PlaygroundOptions; label: string; hint: string }[] = [
  { key: "semanticCache", label: "Test Semantic Cache (send paraphrased prompt)", hint: "primes the cache, then sends a paraphrase" },
  { key: "injectPII", label: "Inject PII (SSN/Credit Card)", hint: "redacted before forwarding upstream" },
  { key: "promptInjection", label: "Trigger Prompt Injection", hint: "expect HTTP 400 PROMPT_INJECTION" },
  { key: "simulateOutage", label: "Simulate Primary Provider 503 Outage (Failover)", hint: "X-Gateway-Chaos: fail-primary" },
  { key: "stream", label: "Stream response (SSE)", hint: "stream: true" },
];

export function Playground({ fetchImpl }: { fetchImpl?: typeof fetch }) {
  const doFetch = fetchImpl ?? ((...args: Parameters<typeof fetch>) => fetch(...args));
  const [opts, setOpts] = useState<PlaygroundOptions>({
    prompt: "Explain how Kafka consumer groups work in simple terms.",
    model: MODELS[0],
    semanticCache: false,
    injectPII: false,
    promptInjection: false,
    simulateOutage: false,
    stream: false,
  });
  const [results, setResults] = useState<PlaygroundResult[]>([]);
  const [busy, setBusy] = useState(false);
  const [fatal, setFatal] = useState<string | null>(null);
  const tokenRef = useRef<string | null>(null);

  const send = async () => {
    setBusy(true);
    setFatal(null);
    setResults([]);
    try {
      if (!tokenRef.current) tokenRef.current = await mintToken();
      const out: PlaygroundResult[] = [];
      for (const req of planRequests(opts)) {
        const start = performance.now();
        const res = await doFetch(`${GATEWAY_URL}/v1/chat/completions`, {
          method: "POST",
          headers: { ...req.headers, Authorization: `Bearer ${tokenRef.current}` },
          body: JSON.stringify(req.body),
        });
        const text = await res.text();
        const clientMs = performance.now() - start;
        const headers: Record<string, string> = {};
        for (const h of GATEWAY_HEADERS) {
          const v = res.headers.get(h);
          if (v != null) headers[h] = v;
        }
        let content = "";
        let error: string | undefined;
        if (res.ok && req.body.stream) {
          content = parseSSEContent(text);
        } else {
          try {
            const json = JSON.parse(text);
            content = json?.choices?.[0]?.message?.content ?? "";
            if (json?.error) error = `${json.error.code ?? json.error.type}: ${json.error.message}`;
          } catch {
            content = text;
          }
        }
        out.push({ label: req.label, status: res.status, headers, latency: latencyFromHeaders(res.headers, clientMs), content, error });
        setResults([...out]);
      }
    } catch (e) {
      tokenRef.current = null;
      setFatal(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel title="Interactive Live Playground" subtitle={`Requests go through the real gateway at ${GATEWAY_URL}`} testId="playground">
      <div className="grid gap-4 lg:grid-cols-2">
        <div className="space-y-3">
          <label className="block">
            <span className="text-xs text-dim">Prompt</span>
            <textarea
              data-testid="prompt-input"
              className="mt-1 h-24 w-full resize-none rounded-md border border-line bg-panel-2 p-2 font-mono text-xs text-ink outline-none focus:border-accent"
              value={opts.prompt}
              onChange={(e) => setOpts({ ...opts, prompt: e.target.value })}
            />
          </label>
          <label className="block">
            <span className="text-xs text-dim">Model</span>
            <select
              data-testid="model-select"
              className="mt-1 w-full rounded-md border border-line bg-panel-2 p-2 font-mono text-xs text-ink outline-none focus:border-accent"
              value={opts.model}
              onChange={(e) => setOpts({ ...opts, model: e.target.value })}
            >
              {MODELS.map((m) => (
                <option key={m}>{m}</option>
              ))}
            </select>
          </label>
          <fieldset className="space-y-1.5">
            {TOGGLES.map((t) => (
              <label key={t.key} className="flex cursor-pointer items-start gap-2 text-xs">
                <input
                  type="checkbox"
                  data-testid={`toggle-${t.key}`}
                  className="mt-0.5 accent-[var(--color-accent)]"
                  checked={Boolean(opts[t.key])}
                  onChange={(e) => setOpts({ ...opts, [t.key]: e.target.checked })}
                />
                <span>
                  <span className="text-ink">{t.label}</span>
                  <span className="block text-[11px] text-faint">{t.hint}</span>
                </span>
              </label>
            ))}
          </fieldset>
          <button
            data-testid="send-button"
            onClick={send}
            disabled={busy || !opts.prompt.trim()}
            className="w-full rounded-md bg-accent px-3 py-2 text-sm font-medium text-bg transition hover:brightness-110 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {busy ? "Sending…" : "Send through gateway"}
          </button>
          {fatal ? (
            <p data-testid="playground-error" className="rounded-md border border-bad/40 bg-bad/10 p-2 font-mono text-[11px] text-bad">
              {fatal}
            </p>
          ) : null}
        </div>
        <div className="space-y-3" data-testid="playground-results">
          {results.length === 0 ? (
            <div className="flex h-full min-h-40 items-center justify-center rounded-md border border-dashed border-line text-xs text-faint">
              Response headers and latency breakdown appear here.
            </div>
          ) : (
            results.map((r, i) => <ResultCard key={i} r={r} />)
          )}
        </div>
      </div>
    </Panel>
  );
}

function ResultCard({ r }: { r: PlaygroundResult }) {
  const cache = r.headers["X-Gateway-Cache"];
  const max = Math.max(r.latency.totalMs, 1);
  return (
    <div data-testid="result-card" className="rounded-md border border-line bg-panel-2 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[11px] uppercase tracking-wide text-faint">{r.label}</span>
        <Badge tone={statusTone(r.status)} testId="result-status">
          HTTP {r.status}
        </Badge>
        {cache ? (
          <Badge tone={cacheTone(cache)} testId="result-cache">
            {cache}
          </Badge>
        ) : null}
        {r.headers["X-Gateway-Provider-Failover"] === "true" ? <Badge tone="warn">FAILOVER</Badge> : null}
        {r.headers["X-Gateway-Guardrail"] ? <Badge tone="bad">{r.headers["X-Gateway-Guardrail"]}</Badge> : null}
      </div>
      <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 font-mono text-[11px]">
        {Object.entries(r.headers).map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-faint">{k}</dt>
            <dd data-testid={`hdr-${k}`} className="truncate text-ink" title={v}>
              {v}
            </dd>
          </div>
        ))}
      </dl>
      <div className="mt-3 space-y-1 font-mono text-[11px]">
        {(
          [
            ["gateway overhead", r.latency.overheadMs, "bg-ok"],
            ["upstream", r.latency.upstreamMs, "bg-accent"],
            ["total", r.latency.totalMs, "bg-faint"],
          ] as const
        ).map(([label, v, color]) => (
          <div key={label} className="flex items-center gap-2">
            <span className="w-28 text-faint">{label}</span>
            <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-line">
              <div className={`h-full rounded-full ${color}`} style={{ width: `${barWidth(v, max)}%` }} />
            </div>
            <span className="tabular w-16 text-right text-dim">{formatMs(v)}</span>
          </div>
        ))}
      </div>
      {r.error ? (
        <p data-testid="result-error" className="mt-2 font-mono text-[11px] text-bad">
          {r.error}
        </p>
      ) : null}
      {r.content ? (
        <p data-testid="result-content" className="mt-2 max-h-28 overflow-y-auto whitespace-pre-wrap rounded bg-bg/60 p-2 font-mono text-[11px] text-dim">
          {r.content}
        </p>
      ) : null}
    </div>
  );
}
