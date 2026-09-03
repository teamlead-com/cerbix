import { describe, expect, it } from "vitest";

import { buildPoints, emptySpans, panelStats, widestSpan, type HeartbeatLike } from "./latencypanel";

// func-truthful-rendering §6 (FR-031, D-0235).
const T0 = Date.UTC(2026, 8, 3, 14, 12, 0);
const at = (min: number) => new Date(T0 + min * 60_000).toISOString();

describe("buildPoints — every fetched heartbeat is drawn", () => {
  it("keeps a failure that recorded NO latency instead of dropping it", () => {
    // The defect: composite evaluation carries no latency on any path, and neither does a config
    // refusal, an ICMP setup failure or an unreadable canary workflow. They vanished.
    const hbs: HeartbeatLike[] = [
      { ts: at(0), up: true, latency_ms: 75 },
      { ts: at(1), up: false, latency_ms: 0, msg: "bad request: unsupported scheme" },
      { ts: at(2), up: false, latency_ms: null, msg: "composite has no child monitors" },
      { ts: at(3), up: true, latency_ms: 78 },
    ];
    const pts = buildPoints(hbs);
    expect(pts).toHaveLength(4);
    expect(pts.map((p) => p.latency)).toEqual([75, null, null, 78]);
    // and each point keeps its whole heartbeat, which is what a readout needs
    expect(pts[1].hb.msg).toContain("unsupported scheme");
  });

  it("keeps a timeout, which DOES carry elapsed latency, as an ordinary measured point", () => {
    const pts = buildPoints([{ ts: at(0), up: false, latency_ms: 10_000, msg: "timeout" }]);
    expect(pts[0].latency).toBe(10_000);
  });

  it("orders oldest first and drops only what has no parseable time", () => {
    const pts = buildPoints([{ ts: at(5), latency_ms: 1 }, { ts: "nonsense", latency_ms: 2 }, { ts: at(1), latency_ms: 3 }]);
    expect(pts.map((p) => p.latency)).toEqual([3, 1]);
  });
});

describe("panelStats — one population, named", () => {
  const pts = buildPoints([
    { ts: at(0), latency_ms: 70 },
    { ts: at(1), latency_ms: 80 },
    { ts: at(2), latency_ms: 90 },
    { ts: at(3), latency_ms: null },
  ]);

  it("computes avg and p95 over the DRAWN series and counts both populations", () => {
    const s = panelStats(pts, 10_000);
    expect(s.drawn).toBe(4);
    expect(s.measured).toBe(3);
    expect(s.avg).toBeCloseTo(80, 6);
    expect(s.p95).toBe(90);
  });

  it("draws the timeout only when it falls inside the computed extent", () => {
    // 90ms of samples against a 10s timeout: off-scale, and no percentage decides that.
    expect(panelStats(pts, 10_000).timeoutInScale).toBe(false);
    // a real timeout is recorded at roughly the timeout, so it raises the extent by itself
    const withTimeout = buildPoints([...[70, 80].map((v, i) => ({ ts: at(i), latency_ms: v })), { ts: at(9), latency_ms: 10_000 }]);
    expect(panelStats(withTimeout, 10_000).timeoutInScale).toBe(true);
  });

  it("has no number to state when nothing carried a latency, rather than inventing one", () => {
    const s = panelStats(buildPoints([{ ts: at(0), latency_ms: null }]), 10_000);
    expect(s.avg).toBeNull();
    expect(s.p95).toBeNull();
    expect(s.timeoutInScale).toBe(false);
  });
});

describe("emptySpans — absence rendered positively, with no threshold on meaning", () => {
  // 60 checks a minute apart with a real six-minute hole: the panel's own fixture.
  const hbs: HeartbeatLike[] = [];
  for (let i = 0, m = 0; i < 60; i++, m++) {
    if (m >= 22 && m <= 27) m = 28; // no check recorded across those minutes
    hbs.push({ ts: at(m), latency_ms: 75 });
  }
  const pts = buildPoints(hbs);
  const PXMS = 1072 / (pts[pts.length - 1].ms - pts[0].ms);

  it("gives every interval between adjacent checks its own focus target at this density", () => {
    const spans = emptySpans(pts, PXMS, 12);
    expect(spans).toHaveLength(59);
    expect(spans.every((s) => !s.merged)).toBe(true);
  });

  it("names the widest interval as an interval, never as a count of missing checks", () => {
    const w = widestSpan(emptySpans(pts, PXMS, 12))!;
    expect((w.toMs - w.fromMs) / 60_000).toBe(7); // seven minutes BETWEEN two records
    expect(w.intervals).toBe(1);
  });

  it("merges adjacent spans too narrow to focus, for interaction only, and states real bounds", () => {
    // three times the density: ordinary spans fall under the hit target, the hole does not
    const dense: HeartbeatLike[] = [];
    for (let i = 0; i < 180; i++) {
      const m = i / 3;
      if (m >= 22 && m <= 28) continue;
      dense.push({ ts: at(m), latency_ms: 75 });
    }
    const dp = buildPoints(dense);
    const dpx = 1072 / (dp[dp.length - 1].ms - dp[0].ms);
    const spans = emptySpans(dp, dpx, 12);
    const merged = spans.filter((s) => s.merged);
    expect(merged.length).toBeGreaterThan(0);
    expect(spans.filter((s) => !s.merged).length).toBeGreaterThan(0);
    // a merged target covers many intervals and reports its OWN outer bounds
    const m0 = merged[0];
    expect(m0.intervals).toBeGreaterThan(1);
    expect(m0.toMs).toBeGreaterThan(m0.fromMs);
    // every tick is still its own point: merging changed interaction, not geometry
    expect(dp.length).toBe(dense.length);
  });

  it("has nothing to say about a single point, and does not invent a span", () => {
    expect(emptySpans(buildPoints([{ ts: at(0), latency_ms: 1 }]), PXMS, 12)).toEqual([]);
    expect(widestSpan([])).toBeNull();
  });
});
