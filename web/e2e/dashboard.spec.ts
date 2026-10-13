import { expect, test, type Page, type Route } from "@playwright/test";
import { summaryFixture } from "../__tests__/fixtures/summary";

/**
 * Deterministic E2E: the gateway API is mocked at the network layer so the
 * dashboard and playground can be verified without a running backend.
 * The SSE stream is aborted to exercise the polling fallback.
 */
async function mockGateway(page: Page, captured: { chat: { headers: Record<string, string>; body: any }[] }) {
  await page.route("**/v1/telemetry/stream", (route) => route.abort());
  await page.route("**/v1/telemetry/summary", (route) => route.fulfill({ json: summaryFixture, headers: { "Access-Control-Allow-Origin": "*" } }));
  await page.route("**/v1/auth/token", (route) => route.fulfill({ json: { access_token: "e2e-token", token_type: "Bearer", expires_in: 3600 }, headers: { "Access-Control-Allow-Origin": "*" } }));
  await page.route("**/v1/chat/completions", async (route: Route) => {
    const req = route.request();
    if (req.method() === "OPTIONS") {
      return route.fulfill({ status: 204, headers: { "Access-Control-Allow-Origin": "*", "Access-Control-Allow-Headers": "*", "Access-Control-Allow-Methods": "POST" } });
    }
    const body = req.postDataJSON();
    const headers = req.headers();
    captured.chat.push({ headers, body });
    const prompt: string = body.messages[1].content;
    const expose = {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Expose-Headers": "*",
    };
    if (prompt.startsWith("Ignore all previous instructions")) {
      return route.fulfill({
        status: 400,
        headers: { ...expose, "X-Gateway-Guardrail": "BLOCK", "X-Gateway-Guardrail-Categories": "PROMPT_INJECTION" },
        json: { error: { code: "PROMPT_INJECTION", type: "policy_violation", message: "prompt-injection score 0.99 >= threshold 0.70" } },
      });
    }
    const paraphrased = prompt.startsWith("Please, could you tell me");
    const failover = headers["x-gateway-chaos"] === "fail-primary";
    const pii = prompt.includes("123-45-6789");
    return route.fulfill({
      status: 200,
      headers: {
        ...expose,
        "X-Gateway-Cache": paraphrased ? "HIT-SEMANTIC" : "MISS",
        ...(paraphrased ? { "X-Gateway-Cache-Similarity": "0.9641" } : {}),
        "X-Gateway-Provider": paraphrased ? "cache" : failover ? "anthropic" : "openai",
        "X-Gateway-Provider-Failover": failover ? "true" : "false",
        ...(pii ? { "X-Gateway-Guardrail": "REDACT", "X-Gateway-Guardrail-Categories": "CREDIT_CARD,SSN" } : {}),
        "X-RateLimit-Remaining-Requests": "117",
        "X-Gateway-Overhead-Ms": "0.84",
        "X-Gateway-Upstream-Latency-Ms": paraphrased ? "0" : "166.2",
        "X-Gateway-Total-Latency-Ms": paraphrased ? "0.84" : "167.0",
      },
      json: { choices: [{ message: { role: "assistant", content: `[mock] Re: "${prompt.replace("123-45-6789", "[REDACTED_SSN]")}"` }, finish_reason: "stop" }] },
    });
  });
}

test.describe("LLM Gateway control plane", () => {
  test("renders KPIs, provider breakers, and the audit stream", async ({ page }) => {
    const captured = { chat: [] as { headers: Record<string, string>; body: any }[] };
    await mockGateway(page, captured);
    await page.goto("/");

    await expect(page.getByRole("heading", { name: "LLM Gateway, Cache & Guardrails" })).toBeVisible();
    await expect(page.getByTestId("connection-state")).toHaveText("POLLING");
    await expect(page.getByTestId("kpi-requests-value")).toHaveText("12,345");
    await expect(page.getByTestId("kpi-hit-ratio-value")).toHaveText("61.8%");
    await expect(page.getByTestId("kpi-saved-value")).toHaveText("$4.31");
    await expect(page.getByTestId("kpi-overhead-value")).toHaveText("3.25 ms");
    await expect(page.getByTestId("kpi-guardrails-value")).toHaveText("673");

    await expect(page.getByTestId("breaker-openai")).toHaveText("OPEN");
    await expect(page.getByTestId("breaker-anthropic")).toHaveText("HALF-OPEN");
    await expect(page.getByTestId("breaker-gemini")).toHaveText("CLOSED");
    await expect(page.getByTestId("breaker-ollama")).toHaveText("CLOSED");

    await expect(page.getByTestId("audit-row")).toHaveCount(3);
    await expect(page.getByTestId("audit-row").first()).toContainText("HIT-SEMANTIC");
    await expect(page.getByTestId("guardrail-categories")).toContainText("PROMPT_INJECTION 112");
  });

  test("playground: semantic cache + PII toggles show live gateway headers", async ({ page }) => {
    const captured = { chat: [] as { headers: Record<string, string>; body: any }[] };
    await mockGateway(page, captured);
    await page.goto("/");

    await page.getByTestId("toggle-semanticCache").check();
    await page.getByTestId("toggle-injectPII").check();
    await page.getByTestId("send-button").click();

    const cards = page.getByTestId("result-card");
    await expect(cards).toHaveCount(2);
    await expect(cards.nth(0).getByTestId("result-cache")).toHaveText("MISS");
    await expect(cards.nth(1).getByTestId("result-cache")).toHaveText("HIT-SEMANTIC");
    await expect(cards.nth(1).getByTestId("hdr-X-Gateway-Cache-Similarity")).toHaveText("0.9641");
    await expect(cards.nth(1).getByTestId("hdr-X-RateLimit-Remaining-Requests")).toHaveText("117");
    await expect(cards.nth(0).getByTestId("hdr-X-Gateway-Guardrail-Categories")).toHaveText("CREDIT_CARD,SSN");
    await expect(cards.nth(0).getByTestId("result-content")).toContainText("[REDACTED_SSN]");

    expect(captured.chat).toHaveLength(2);
    expect(captured.chat[0].headers["authorization"]).toBe("Bearer e2e-token");
    expect(captured.chat[0].body.messages[1].content).toContain("4111 1111 1111 1111");
  });

  test("playground: failover and prompt-injection toggles", async ({ page }) => {
    const captured = { chat: [] as { headers: Record<string, string>; body: any }[] };
    await mockGateway(page, captured);
    await page.goto("/");

    await page.getByTestId("toggle-simulateOutage").check();
    await page.getByTestId("send-button").click();
    await expect(page.getByTestId("result-card")).toHaveCount(1);
    await expect(page.getByTestId("hdr-X-Gateway-Provider")).toHaveText("anthropic");
    await expect(page.getByTestId("hdr-X-Gateway-Provider-Failover")).toHaveText("true");
    expect(captured.chat[0].headers["x-gateway-chaos"]).toBe("fail-primary");

    await page.getByTestId("toggle-simulateOutage").uncheck();
    await page.getByTestId("toggle-promptInjection").check();
    await page.getByTestId("send-button").click();
    await expect(page.getByTestId("result-status")).toHaveText("HTTP 400");
    await expect(page.getByTestId("result-error")).toContainText("PROMPT_INJECTION");
  });

  test("shows an offline banner when the gateway is unreachable", async ({ page }) => {
    await page.route("**/v1/telemetry/**", (route) => route.abort());
    await page.goto("/");
    await expect(page.getByTestId("connection-state")).toHaveText("OFFLINE");
    await expect(page.getByTestId("offline-banner")).toBeVisible();
  });
});

/**
 * Live E2E against a real gateway (make up / make run). Enabled with E2E_LIVE=1.
 */
test.describe("live gateway", () => {
  test.skip(!process.env.E2E_LIVE, "set E2E_LIVE=1 with the gateway running on :8080");

  test("real request flow: miss -> semantic hit, redaction, failover", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByTestId("connection-state")).toHaveText("LIVE · SSE", { timeout: 15_000 });
    const unique = `Explain how ${Date.now()} ledger reconciliation batches work in simple terms.`;
    await page.getByTestId("prompt-input").fill(unique);
    await page.getByTestId("toggle-semanticCache").check();
    await page.getByTestId("toggle-injectPII").check();
    await page.getByTestId("send-button").click();
    const cards = page.getByTestId("result-card");
    await expect(cards).toHaveCount(2, { timeout: 15_000 });
    await expect(cards.nth(0).getByTestId("result-cache")).toHaveText("MISS");
    await expect(cards.nth(1).getByTestId("result-cache")).toHaveText(/HIT-(SEMANTIC|EXACT)/);
    await expect(cards.nth(0).getByTestId("result-content")).toContainText("[REDACTED_SSN]");

    await page.getByTestId("toggle-semanticCache").uncheck();
    await page.getByTestId("toggle-injectPII").uncheck();
    await page.getByTestId("toggle-simulateOutage").check();
    await page.getByTestId("send-button").click();
    await expect(page.getByTestId("hdr-X-Gateway-Provider-Failover")).toHaveText("true", { timeout: 15_000 });
    await expect(page.getByTestId("audit-row").first()).toBeVisible({ timeout: 10_000 });
  });
});
