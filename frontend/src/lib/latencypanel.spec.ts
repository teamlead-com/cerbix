import { describe, expect, it } from "vitest";

import { buildPoints, emptySpans, gapLabel, panelStats, strokeSegments, widestSpan, type HeartbeatLike } from "./latencypanel";

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

// Found by looking at the running stack rather than at a fixture: an eleven-second interval was
// rendered as "0 min", which is a false statement about the interval on a panel whose subject is
// not making those.
describe("gapLabel — an interval in a unit that fits it", () => {
  it("never renders a real interval as zero", () => {
    expect(gapLabel(11_000)).toBe("11s");
    expect(gapLabel(1_000)).toBe("1s");
    expect(gapLabel(59_400)).toBe("59s");
  });

  it("uses minutes with seconds only when there are any", () => {
    expect(gapLabel(7 * 60_000)).toBe("7m");
    expect(gapLabel(7 * 60_000 + 30_000)).toBe("7m 30s");
  });

  it("reaches hours for a long silence", () => {
    expect(gapLabel(3 * 3_600_000)).toBe("3h");
    expect(gapLabel(3 * 3_600_000 + 20 * 60_000)).toBe("3h 20m");
  });
});

// ---------------------------------------------------------------------------------------------
// FR-032 phase E — when a stroke is defensible (§14).
//
// FR-031 removed the line because the subject of any allowance — that a run was EXPECTED — was not
// in the data. FR-032 records it, and these cases are the whole of what the panel may now claim.

/** Points a minute apart, oldest first, as `buildPoints` produces them. */
function pointsEveryMinute(count: number, startMs = Date.UTC(2026, 8, 5, 10, 0, 0)) {
  return Array.from({ length: count }, (_, i) => ({
    ms: startMs + i * 60_000,
    latency: 100,
    hb: {},
  }));
}

/** One window per interval between the points, all with the same verdict. */
function windowsBetween(points: { ms: number }[], verdict: string) {
  return points.slice(1).map((p) => ({ due_at: new Date(p.ms).toISOString(), verdict }));
}

describe("strokeSegments — a stroke only across time the ledger can defend", () => {
  it("joins the whole series when every window between the points is covered", () => {
    const points = pointsEveryMinute(5);
    const segments = strokeSegments(points, {
      windows: windowsBetween(points, "covered"),
      ledger_from: new Date(points[0].ms - 3_600_000).toISOString(),
    });
    expect(segments).toEqual([{ fromIndex: 0, toIndex: 4 }]);
  });

  it("refuses covered_late, which licenses no stroke however small the lateness", () => {
    const points = pointsEveryMinute(5);
    const windows = windowsBetween(points, "covered");
    windows[2].verdict = "covered_late";
    const segments = strokeSegments(points, {
      windows,
      ledger_from: new Date(points[0].ms - 3_600_000).toISOString(),
    });
    // The series splits AROUND the offending interval: a monitor with one unanswerable minute
    // still has defensible strokes on either side of it, and refusing the whole series would
    // understate what the ledger proves.
    expect(segments).toEqual([
      { fromIndex: 0, toIndex: 2 },
      { fromIndex: 3, toIndex: 4 },
    ]);
  });

  it.each(["expected_never_issued", "issued_never_claimed", "claimed_never_finished", "unknown"])(
    "refuses %s",
    (verdict) => {
      const points = pointsEveryMinute(3);
      const windows = windowsBetween(points, "covered");
      windows[1].verdict = verdict;
      const segments = strokeSegments(points, {
        windows,
        ledger_from: new Date(points[0].ms - 3_600_000).toISOString(),
      });
      expect(segments).toEqual([{ fromIndex: 0, toIndex: 1 }]);
    },
  );

  it("refuses any interval reaching back before ledger_from, whatever its windows say", () => {
    const points = pointsEveryMinute(5);
    const segments = strokeSegments(points, {
      windows: windowsBetween(points, "covered"),
      // The bound sits between the second and third point: everything before it is unanswerable,
      // and "no window says otherwise" is the ABSENCE of evidence rather than evidence.
      ledger_from: new Date(points[2].ms).toISOString(),
    });
    expect(segments).toEqual([{ fromIndex: 2, toIndex: 4 }]);
  });

  it("draws nothing where there are no windows at all — the pre-FR-032 situation", () => {
    const points = pointsEveryMinute(4);
    const segments = strokeSegments(points, {
      windows: [],
      ledger_from: new Date(points[0].ms - 3_600_000).toISOString(),
    });
    expect(segments).toEqual([]);
  });

  it("draws nothing when the monitor has no expectation at all", () => {
    const points = pointsEveryMinute(4);
    // A push monitor, or a disabled one: `ledger_from` is null and the whole range is not stored.
    expect(strokeSegments(points, { windows: windowsBetween(points, "covered"), ledger_from: null })).toEqual([]);
    // And with no answer at all — an older server, or a failed fetch — the panel keeps FR-031's
    // behaviour rather than assuming the best.
    expect(strokeSegments(points, null)).toEqual([]);
  });

  it("treats the interval as half-open, so a window at the earlier point vouches for nothing after it", () => {
    const points = pointsEveryMinute(2);
    // The ONLY window is due exactly at the first point. That point answers it; it says nothing
    // about the minute that follows.
    const segments = strokeSegments(points, {
      windows: [{ due_at: new Date(points[0].ms).toISOString(), verdict: "covered" }],
      ledger_from: new Date(points[0].ms - 3_600_000).toISOString(),
    });
    expect(segments).toEqual([]);
  });

  it("never reads interval_assumed as licence to promote a covered_late window", () => {
    const points = pointsEveryMinute(3);
    const windows = windowsBetween(points, "covered_late").map((w) => ({
      ...w,
      // The flag is EXPLANATORY ONLY (invariant 20g). A consumer that softened the verdict here
      // would widen the truthfulness gate without anyone deciding to widen it, which is why the
      // rule requires deleting a clause rather than reinterpreting one.
      interval_assumed: true,
    }));
    expect(
      strokeSegments(points, {
        windows,
        ledger_from: new Date(points[0].ms - 3_600_000).toISOString(),
      }),
    ).toEqual([]);
  });

  it("needs two points to stroke between", () => {
    const points = pointsEveryMinute(1);
    expect(
      strokeSegments(points, { windows: [], ledger_from: new Date(0).toISOString() }),
    ).toEqual([]);
  });

  it("ignores an unparseable bound rather than guessing at it", () => {
    const points = pointsEveryMinute(3);
    expect(
      strokeSegments(points, { windows: windowsBetween(points, "covered"), ledger_from: "not-a-time" }),
    ).toEqual([]);
  });
});
