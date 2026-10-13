import { barWidth, breakerTone, cacheTone, formatMs, formatNumber, formatPercent, formatTime, formatUSD, formatUptime, guardrailTone, statusTone } from "@/lib/format";

describe("formatNumber", () => {
  it("groups thousands and compacts large values", () => {
    expect(formatNumber(0)).toBe("0");
    expect(formatNumber(1234)).toBe("1,234");
    expect(formatNumber(999_999)).toBe("999,999");
    expect(formatNumber(2_500_000)).toBe("2.5M");
    expect(formatNumber(3_000_000_000)).toBe("3B");
  });
  it("handles missing values", () => {
    expect(formatNumber(undefined)).toBe("—");
    expect(formatNumber(NaN)).toBe("—");
  });
});

describe("formatPercent", () => {
  it("formats ratios", () => {
    expect(formatPercent(0.6189)).toBe("61.9%");
    expect(formatPercent(1)).toBe("100.0%");
    expect(formatPercent(0.98604, 2)).toBe("98.60%");
    expect(formatPercent(null)).toBe("—");
  });
});

describe("formatUSD", () => {
  it("keeps precision for tiny savings", () => {
    expect(formatUSD(0)).toBe("$0.00");
    expect(formatUSD(0.000062)).toBe("$0.0001");
    expect(formatUSD(0.0042)).toBe("$0.0042");
    expect(formatUSD(4.3127)).toBe("$4.31");
    expect(formatUSD(12345.678)).toBe("$12,345.68");
  });
});

describe("formatMs", () => {
  it("adapts precision to magnitude", () => {
    expect(formatMs(0.4234)).toBe("0.42 ms");
    expect(formatMs(3.246)).toBe("3.25 ms");
    expect(formatMs(24.36)).toBe("24.4 ms");
    expect(formatMs(167.08)).toBe("167 ms");
    expect(formatMs(1234)).toBe("1.23 s");
    expect(formatMs(undefined)).toBe("—");
  });
});

describe("time helpers", () => {
  it("formats uptime", () => {
    expect(formatUptime(42)).toBe("42s");
    expect(formatUptime(125)).toBe("2m 5s");
    expect(formatUptime(3725)).toBe("1h 2m");
    expect(formatUptime(-1)).toBe("—");
  });
  it("formats timestamps as HH:MM:SS and rejects garbage", () => {
    expect(formatTime("2026-10-13T23:29:59Z")).toMatch(/^\d{2}:\d{2}:\d{2}$/);
    expect(formatTime("not-a-date")).toBe("—");
  });
});

describe("tones", () => {
  it("maps breaker states", () => {
    expect(breakerTone("CLOSED")).toBe("ok");
    expect(breakerTone("HALF-OPEN")).toBe("warn");
    expect(breakerTone("OPEN")).toBe("bad");
    expect(breakerTone("??")).toBe("muted");
  });
  it("maps cache, status and guardrail tones", () => {
    expect(cacheTone("HIT-EXACT")).toBe("ok");
    expect(cacheTone("HIT-SEMANTIC")).toBe("info");
    expect(cacheTone("MISS")).toBe("warn");
    expect(statusTone(200)).toBe("ok");
    expect(statusTone(429)).toBe("warn");
    expect(statusTone(502)).toBe("bad");
    expect(guardrailTone("BLOCK")).toBe("bad");
    expect(guardrailTone("REDACT")).toBe("warn");
    expect(guardrailTone("NONE")).toBe("muted");
  });
});

describe("barWidth", () => {
  it("scales and clamps", () => {
    expect(barWidth(50, 100)).toBe(50);
    expect(barWidth(500, 100)).toBe(100);
    expect(barWidth(0.1, 100)).toBe(2);
    expect(barWidth(0, 100)).toBe(0);
    expect(barWidth(10, 0)).toBe(0);
  });
});
