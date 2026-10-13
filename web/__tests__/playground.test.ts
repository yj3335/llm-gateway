import { INJECTION_PREFIX, PII_SUFFIX, buildPrompt, latencyFromHeaders, paraphrase, parseSSEContent, planRequests, type PlaygroundOptions } from "@/lib/playground";

const base: PlaygroundOptions = {
  prompt: "Explain how Kafka consumer groups work in simple terms.",
  model: "gpt-4o-mini",
  semanticCache: false,
  injectPII: false,
  promptInjection: false,
  simulateOutage: false,
  stream: false,
};

describe("playground request planning", () => {
  it("sends a single plain request by default", () => {
    const plan = planRequests(base);
    expect(plan).toHaveLength(1);
    expect(plan[0].body.messages[1].content).toBe(base.prompt);
    expect(plan[0].headers["X-Gateway-Chaos"]).toBeUndefined();
    expect(plan[0].body.stream).toBe(false);
  });

  it("primes then paraphrases for the semantic cache test", () => {
    const plan = planRequests({ ...base, semanticCache: true });
    expect(plan.map((p) => p.label)).toEqual(["prime cache (original prompt)", "paraphrased prompt"]);
    expect(plan[1].body.messages[1].content).toBe(paraphrase(base.prompt));
    expect(plan[1].body.messages[1].content).not.toBe(plan[0].body.messages[1].content);
  });

  it("injects PII and prompt injection payloads", () => {
    const p = buildPrompt({ prompt: "hello", injectPII: true, promptInjection: true });
    expect(p.startsWith(INJECTION_PREFIX)).toBe(true);
    expect(p.endsWith(PII_SUFFIX)).toBe(true);
    expect(p).toContain("123-45-6789");
  });

  it("adds chaos and cache-bypass headers for the failover test", () => {
    const plan = planRequests({ ...base, simulateOutage: true, semanticCache: true });
    expect(plan).toHaveLength(1);
    expect(plan[0].headers["X-Gateway-Chaos"]).toBe("fail-primary");
    expect(plan[0].headers["Cache-Control"]).toBe("no-cache");
  });

  it("requests usage when streaming", () => {
    const plan = planRequests({ ...base, stream: true });
    expect(plan[0].body.stream).toBe(true);
    expect(plan[0].body.stream_options).toEqual({ include_usage: true });
  });
});

describe("paraphrase", () => {
  it("rewrites while preserving content words", () => {
    expect(paraphrase("What is the CAP theorem?")).toBe("Please, could you tell me: what is the CAP theorem?");
    expect(paraphrase("")).toBe("");
  });
});

describe("response helpers", () => {
  it("parses SSE deltas", () => {
    const text = [
      'data: {"choices":[{"delta":{"role":"assistant"}}]}',
      "",
      'data: {"choices":[{"delta":{"content":"Hello "}}]}',
      "",
      'data: {"choices":[{"delta":{"content":"world"}}]}',
      "",
      "data: [DONE]",
    ].join("\n");
    expect(parseSSEContent(text)).toBe("Hello world");
  });

  it("reads the latency breakdown from gateway headers", () => {
    const h = new Headers({ "X-Gateway-Overhead-Ms": "0.812", "X-Gateway-Upstream-Latency-Ms": "165.2", "X-Gateway-Total-Latency-Ms": "166.0" });
    expect(latencyFromHeaders(h, 999)).toEqual({ overheadMs: 0.812, upstreamMs: 165.2, totalMs: 166 });
    expect(latencyFromHeaders(new Headers(), 12).totalMs).toBe(12);
  });
});
