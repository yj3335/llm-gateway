import type { Summary } from "./types";

export const GATEWAY_URL = (process.env.NEXT_PUBLIC_GATEWAY_URL ?? "http://localhost:8080").replace(/\/$/, "");

export async function fetchSummary(signal?: AbortSignal): Promise<Summary> {
  const res = await fetch(`${GATEWAY_URL}/v1/telemetry/summary`, { signal, cache: "no-store" });
  if (!res.ok) throw new Error(`summary: HTTP ${res.status}`);
  return (await res.json()) as Summary;
}

/** Mints a short-lived dev JWT for the playground tenant via /v1/auth/token. */
export async function mintToken(tenant = "playground"): Promise<string> {
  const res = await fetch(`${GATEWAY_URL}/v1/auth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant_id: tenant, tier: "playground", rpm_limit: 120, tpm_limit: 200000, pii_mode: "REDACT", ttl_seconds: 3600 }),
  });
  if (!res.ok) throw new Error(`token endpoint returned HTTP ${res.status} (is auth.dev_token_endpoint enabled?)`);
  const data = (await res.json()) as { access_token: string };
  return data.access_token;
}
