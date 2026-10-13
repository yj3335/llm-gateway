"use client";

import { useEffect, useRef, useState } from "react";
import { GATEWAY_URL, fetchSummary } from "@/lib/api";
import type { Summary } from "@/lib/types";

export type ConnectionState = "connecting" | "live" | "polling" | "offline";

/**
 * Subscribes to /v1/telemetry/stream (SSE). If the stream errors it falls
 * back to polling /v1/telemetry/summary, and retries SSE periodically.
 */
export function useTelemetry(pollMs = 2000) {
  const [summary, setSummary] = useState<Summary | null>(null);
  const [state, setState] = useState<ConnectionState>("connecting");
  const [error, setError] = useState<string | null>(null);
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    let cancelled = false;
    let pollTimer: ReturnType<typeof setInterval> | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;

    const poll = async () => {
      try {
        const s = await fetchSummary();
        if (cancelled) return;
        setSummary(s);
        setError(null);
        setState((prev) => (prev === "live" ? prev : "polling"));
      } catch (e) {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : String(e));
        setState("offline");
      }
    };

    const startPolling = () => {
      if (pollTimer) return;
      void poll();
      pollTimer = setInterval(poll, pollMs);
    };

    const stopPolling = () => {
      if (pollTimer) clearInterval(pollTimer);
      pollTimer = null;
    };

    const connect = () => {
      if (cancelled || typeof EventSource === "undefined") {
        startPolling();
        return;
      }
      const es = new EventSource(`${GATEWAY_URL}/v1/telemetry/stream`);
      esRef.current = es;
      es.addEventListener("summary", (ev) => {
        try {
          setSummary(JSON.parse((ev as MessageEvent).data) as Summary);
          setState("live");
          setError(null);
          stopPolling();
        } catch {
          /* ignore malformed event */
        }
      });
      es.onerror = () => {
        es.close();
        esRef.current = null;
        if (cancelled) return;
        setState((prev) => (prev === "live" ? "polling" : prev));
        startPolling();
        retryTimer = setTimeout(connect, 10_000);
      };
    };

    connect();
    return () => {
      cancelled = true;
      esRef.current?.close();
      stopPolling();
      if (retryTimer) clearTimeout(retryTimer);
    };
  }, [pollMs]);

  return { summary, state, error };
}
