import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuditTable } from "@/components/AuditTable";
import { KpiCards, buildKpis } from "@/components/KpiCards";
import { Playground } from "@/components/Playground";
import { ProviderMatrix } from "@/components/ProviderMatrix";
import { summaryFixture } from "./fixtures/summary";

describe("KpiCards", () => {
  it("renders the five headline KPIs from a summary", () => {
    render(<KpiCards summary={summaryFixture} />);
    expect(screen.getByTestId("kpi-requests-value")).toHaveTextContent("12,345");
    expect(screen.getByTestId("kpi-hit-ratio-value")).toHaveTextContent("61.8%");
    expect(screen.getByTestId("kpi-saved-value")).toHaveTextContent("$4.31");
    expect(screen.getByTestId("kpi-overhead-value")).toHaveTextContent("3.25 ms");
    expect(screen.getByTestId("kpi-guardrails-value")).toHaveTextContent("673");
    expect(screen.getByText("Semantic Cache Hit Ratio")).toBeInTheDocument();
  });

  it("shows placeholders before data arrives", () => {
    const kpis = buildKpis(null);
    expect(kpis).toHaveLength(5);
    expect(kpis.every((k) => k.value === "—")).toBe(true);
  });
});

describe("ProviderMatrix", () => {
  it("renders a breaker badge per provider with the right tone", () => {
    render(<ProviderMatrix providers={summaryFixture.providers!} />);
    expect(screen.getByTestId("breaker-openai")).toHaveTextContent("OPEN");
    expect(screen.getByTestId("breaker-openai")).toHaveAttribute("data-tone", "bad");
    expect(screen.getByTestId("breaker-anthropic")).toHaveAttribute("data-tone", "warn");
    expect(screen.getByTestId("breaker-gemini")).toHaveAttribute("data-tone", "ok");
    expect(within(screen.getByTestId("provider-openai")).getByText(/upstream status 503/)).toBeInTheDocument();
  });

  it("handles no providers", () => {
    render(<ProviderMatrix providers={[]} />);
    expect(screen.getByText(/No provider data/)).toBeInTheDocument();
  });
});

describe("AuditTable", () => {
  it("renders one row per trace with cache, similarity and guardrail details", () => {
    render(<AuditTable traces={summaryFixture.recent_traces!} />);
    const rows = screen.getAllByTestId("audit-row");
    expect(rows).toHaveLength(3);
    expect(within(rows[0]).getByText("HIT-SEMANTIC")).toBeInTheDocument();
    expect(within(rows[0]).getByText("0.964")).toBeInTheDocument();
    expect(within(rows[1]).getByText("REDACT")).toBeInTheDocument();
    expect(within(rows[1]).getByText("CREDIT_CARD,SSN")).toBeInTheDocument();
    expect(within(rows[1]).getByTitle("served by fallback provider")).toBeInTheDocument();
    expect(within(rows[2]).getByText("BLOCK")).toBeInTheDocument();
    expect(within(rows[2]).getByText("400")).toBeInTheDocument();
  });

  it("shows an empty state", () => {
    render(<AuditTable traces={[]} />);
    expect(screen.getByText(/No requests yet/)).toBeInTheDocument();
  });
});

describe("Playground", () => {
  const originalFetch = global.fetch;
  afterEach(() => {
    global.fetch = originalFetch;
  });

  function mockResponse(body: unknown, status: number, headers: Record<string, string>) {
    return {
      ok: status < 400,
      status,
      headers: new Headers(headers),
      text: async () => JSON.stringify(body),
      json: async () => body,
    } as unknown as Response;
  }

  it("mints a token, sends toggled payloads, and renders gateway headers", async () => {
    global.fetch = jest.fn(async () => mockResponse({ access_token: "tok" }, 200, {})) as jest.Mock;
    const chat = jest.fn(async (_url: string, init: RequestInit) => {
      const body = JSON.parse(String(init.body));
      const isParaphrase = body.messages[1].content.startsWith("Please, could you tell me");
      return mockResponse(
        { choices: [{ message: { content: "[mock] answer" } }] },
        200,
        {
          "X-Gateway-Cache": isParaphrase ? "HIT-SEMANTIC" : "MISS",
          "X-Gateway-Cache-Similarity": isParaphrase ? "0.9641" : "",
          "X-Gateway-Provider": isParaphrase ? "cache" : "openai",
          "X-RateLimit-Remaining-Requests": "118",
          "X-Gateway-Guardrail": "REDACT",
          "X-Gateway-Overhead-Ms": "0.9",
          "X-Gateway-Upstream-Latency-Ms": isParaphrase ? "0" : "165",
          "X-Gateway-Total-Latency-Ms": isParaphrase ? "0.9" : "166",
        },
      );
    });
    const user = userEvent.setup();
    render(<Playground fetchImpl={chat as unknown as typeof fetch} />);

    await user.click(screen.getByTestId("toggle-semanticCache"));
    await user.click(screen.getByTestId("toggle-injectPII"));
    await user.click(screen.getByTestId("send-button"));

    await waitFor(() => expect(screen.getAllByTestId("result-card")).toHaveLength(2));
    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("/v1/auth/token"), expect.anything());
    expect(chat).toHaveBeenCalledTimes(2);
    const [, init] = chat.mock.calls[0];
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer tok");
    expect(JSON.parse(String(init.body)).messages[1].content).toContain("123-45-6789");

    const cards = screen.getAllByTestId("result-card");
    expect(within(cards[0]).getByTestId("result-cache")).toHaveTextContent("MISS");
    expect(within(cards[1]).getByTestId("result-cache")).toHaveTextContent("HIT-SEMANTIC");
    expect(within(cards[1]).getByTestId("hdr-X-RateLimit-Remaining-Requests")).toHaveTextContent("118");
    expect(within(cards[1]).getByTestId("hdr-X-Gateway-Cache-Similarity")).toHaveTextContent("0.9641");
  });

  it("sends the chaos header for the failover toggle and surfaces errors", async () => {
    global.fetch = jest.fn(async () => mockResponse({ access_token: "tok" }, 200, {})) as jest.Mock;
    const chat = jest.fn(async () =>
      mockResponse({ error: { code: "PROMPT_INJECTION", message: "blocked", type: "policy_violation" } }, 400, { "X-Gateway-Guardrail": "BLOCK" }),
    );
    const user = userEvent.setup();
    render(<Playground fetchImpl={chat as unknown as typeof fetch} />);
    await user.click(screen.getByTestId("toggle-simulateOutage"));
    await user.click(screen.getByTestId("toggle-promptInjection"));
    await user.click(screen.getByTestId("send-button"));

    await waitFor(() => expect(screen.getByTestId("result-error")).toHaveTextContent("PROMPT_INJECTION: blocked"));
    const init = (chat.mock.calls[0] as unknown as [string, RequestInit])[1];
    expect((init.headers as Record<string, string>)["X-Gateway-Chaos"]).toBe("fail-primary");
    expect(screen.getByTestId("result-status")).toHaveTextContent("HTTP 400");
  });

  it("reports an unreachable token endpoint", async () => {
    global.fetch = jest.fn(async () => mockResponse({}, 404, {})) as jest.Mock;
    const user = userEvent.setup();
    render(<Playground fetchImpl={jest.fn() as unknown as typeof fetch} />);
    await user.click(screen.getByTestId("send-button"));
    await waitFor(() => expect(screen.getByTestId("playground-error")).toHaveTextContent(/token endpoint returned HTTP 404/));
  });
});
