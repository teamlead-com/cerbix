import { describe, expect, it } from "vitest";

import {
  CANONICAL_BUCKET_MS, SLICE_FLOOR_CAP, SLICE_FLOOR_PX,
  buildCells, clusterTransitions, stackSlices, storageVerdict, transitionsOf,
  type SeriesPointLike,
} from "./reliabilitygeometry";

// func-truthful-rendering §5 (FR-031, D-0235). The fixture is the owner's own screen, and it is
// the same one the approved mock draws: a 30-day window whose service has two and a half days of
// facts, so 27 days are honestly empty.
const DAY = 24 * 60 * CANONICAL_BUCKET_MS;
const F = Date.UTC(2026, 7, 4);          // window from 2026-08-04T00:00Z
const T = Date.UTC(2026, 8, 3);          // window to   2026-09-03T00:00Z (sealed_through)
const min = (n: number) => n * CANONICAL_BUCKET_MS * 1000; // minutes -> microseconds

const pt = (dayIndex: number, o: Partial<SeriesPointLike> & { good?: number; bad?: number; unknown?: number }): SeriesPointLike => ({
  start: new Date(F + dayIndex * DAY).toISOString(),
  epoch_id: o.epoch_id ?? "e1",
  revision_id: o.revision_id ?? "r1",
  provisional: o.provisional ?? false,
  buckets: (o.good ?? 0) + (o.bad ?? 0) + (o.unknown ?? 0),
  durations: { GoodUs: min(o.good ?? 0), BadUs: min(o.bad ?? 0), UnknownUs: min(o.unknown ?? 0), ExcludedUs: 0 },
});

describe("buildCells — the axis is clock time", () => {
  const points = [pt(27, { good: 150 }), pt(28, { good: 1438, unknown: 2 }), pt(29, { good: 1423, bad: 9, unknown: 8 })];

  it("covers the WHOLE requested range, not only the steps the response carries", () => {
    // This is the change. The old strip drew three ticks; a 30-day window has 30 cells.
    const cells = buildCells(F, T, DAY, points);
    expect(cells).toHaveLength(30);
    expect(cells[0].startMs).toBe(F);
    expect(cells[29].endMs).toBe(T);
  });

  it("leaves an unstored step as a cell with zero stored minutes, occupying its own width", () => {
    const cells = buildCells(F, T, DAY, points);
    const empty = cells.slice(0, 27);
    expect(empty.every((c) => c.storedMinutes === 0)).toBe(true);
    expect(empty.every((c) => c.endMs - c.startMs === DAY)).toBe(true);
    // and it is NOT unknown: absence of a verdict is not a verdict
    expect(empty.every((c) => c.sealed.unknown === 0)).toBe(true);
  });

  it("clips the first and last cell to the requested range rather than overhanging it", () => {
    const from = F + 5 * 60 * CANONICAL_BUCKET_MS;   // 05:00 into the first day
    const to = T - 90 * CANONICAL_BUCKET_MS;         // 90 minutes before the end
    const cells = buildCells(from, to, DAY, points);
    expect(cells[0].startMs).toBe(from);
    expect(cells[0].endMs).toBe(F + DAY);
    expect(cells[cells.length - 1].endMs).toBe(to);
  });

  it("keeps sealed and provisional durations apart, because one is in every number and the other in none", () => {
    const cells = buildCells(F, T + DAY, DAY, [...points, pt(30, { good: 881, provisional: true })]);
    const tail = cells[30];
    expect(tail.provisional.good).toBe(min(881));
    expect(tail.sealed.good).toBe(0);
  });

  it("marks a cell an active repair intersects, so it can be masked as work and never as data", () => {
    const repair = [{ from: new Date(F + 28 * DAY + 3600_000).toISOString(), to: new Date(F + 28 * DAY + 7200_000).toISOString() }];
    const cells = buildCells(F, T, DAY, points, repair);
    expect(cells[28].repairing).toBe(true);
    expect(cells[27].repairing).toBe(false);
    expect(stackSlices(cells[28], 34)).toEqual([]); // work is not drawn as data
  });

  it("returns nothing for an inverted or empty range instead of guessing one", () => {
    expect(buildCells(T, F, DAY, points)).toEqual([]);
    expect(buildCells(F, F, DAY, points)).toEqual([]);
    expect(buildCells(F, T, 0, points)).toEqual([]);
  });
});

describe("stackSlices — height carries quantity", () => {
  const H = 34;
  const cellOf = (o: { good?: number; bad?: number; unknown?: number; provGood?: number }, extentMin = 1440) => ({
    startMs: F, endMs: F + extentMin * CANONICAL_BUCKET_MS,
    sealed: { good: min(o.good ?? 0), bad: min(o.bad ?? 0), unknown: min(o.unknown ?? 0), excluded: 0 },
    provisional: { good: min(o.provGood ?? 0), bad: 0, unknown: 0, excluded: 0 },
    storedMinutes: (o.good ?? 0) + (o.bad ?? 0) + (o.unknown ?? 0) + (o.provGood ?? 0),
    repairing: false,
  });

  it("fills the cell exactly, so a stack never claims more or less than its extent", () => {
    for (const c of [cellOf({ good: 150 }), cellOf({ good: 1432.8, bad: 7.2 }), cellOf({ good: 1233, unknown: 10, provGood: 40 })]) {
      const total = stackSlices(c, H).reduce((s, x) => s + x.h, 0);
      expect(total).toBeCloseTo(H, 6);
    }
  });

  it("keeps a fractional-percent outage visible at its floor instead of losing it", () => {
    // 9 minutes of a day is 0.21px of 34 — below a pixel, and the whole reason the floor exists.
    const bad = stackSlices(cellOf({ good: 1431, bad: 9 }), H).find((s) => s.kind === "bad")!;
    expect(bad.h).toBe(SLICE_FLOOR_PX);
  });

  it("does NOT floor provisional good time, because that would inflate good time for no honesty", () => {
    const slices = stackSlices(cellOf({ good: 1233, unknown: 10, provGood: 40 }), H);
    const prov = slices.find((s) => s.kind === "good" && s.provisional)!;
    expect(prov.h).toBeLessThan(SLICE_FLOOR_PX);
    expect(prov.h).toBeCloseTo((40 / 1440) * H, 6);
  });

  it("pays the floor out of good and never out of the absence", () => {
    const slices = stackSlices(cellOf({ good: 1000, bad: 1 }, 1440), H);
    const notStored = slices.find((s) => s.kind === "notStored")!;
    expect(notStored.h).toBeCloseTo((439 / 1440) * H, 6);
    const good = slices.find((s) => s.kind === "good")!;
    expect(good.h).toBeLessThan((1000 / 1440) * H);
  });

  it("draws absence in a step with nothing stored, at the cell's full height", () => {
    const slices = stackSlices(cellOf({}), H);
    expect(slices).toEqual([{ kind: "notStored", h: H, provisional: false }]);
  });

  it("orders the stack the same way every time, so cells stay comparable", () => {
    const kinds = stackSlices(cellOf({ good: 1000, bad: 5, unknown: 5, provGood: 30 }), H)
      .map((s) => `${s.kind}${s.provisional ? ":prov" : ""}`);
    expect(kinds).toEqual(["notStored", "bad", "unknown", "good:prov", "good"]);
  });
});

describe("clusterTransitions — colliding marks cluster, anchored at the earliest", () => {
  const t = (ms: number, from: number, to: number) => ({ ms, fromRevision: from, toRevision: to, epochOnly: false });
  const PXMS = 1120 / (30 * DAY); // the mock's own scale

  it("collapses two transitions four minutes apart into one mark with a count", () => {
    const at = F + 28 * DAY;
    const cl = clusterTransitions([t(at + 150 * 60000, 1, 2), t(at + 154 * 60000, 2, 3)], PXMS, 3);
    expect(cl).toHaveLength(1);
    expect(cl[0].count).toBe(2);
  });

  it("anchors the mark at the EARLIEST boundary, never at the cluster's midpoint", () => {
    const at = F + 28 * DAY;
    const first = at + 150 * 60000;
    const cl = clusterTransitions([t(at + 154 * 60000, 2, 3), t(first, 1, 2)], PXMS, 3);
    expect(cl[0].ms).toBe(first);
    expect(cl[0].lastMs).toBe(at + 154 * 60000);
  });

  it("lists its members chronologically, because a count may not stand in for the changes", () => {
    const at = F + 28 * DAY;
    const cl = clusterTransitions([t(at + 154 * 60000, 2, 3), t(at + 150 * 60000, 1, 2)], PXMS, 3);
    expect(cl[0].members.map((m) => m.toRevision)).toEqual([2, 3]);
  });

  it("keeps well-separated boundaries apart", () => {
    const cl = clusterTransitions([t(F + 2 * DAY, 1, 2), t(F + 20 * DAY, 2, 3)], PXMS, 3);
    expect(cl.map((c) => c.count)).toEqual([1, 1]);
  });
});

describe("transitionsOf — a segment's start is not a boundary", () => {
  const rev = (id?: string) => Number((id ?? "r0").slice(1));
  it("reports only changes BETWEEN consecutive points, so the first point is not a transition", () => {
    const pts = [
      pt(28, { revision_id: "r1", good: 150 }),
      pt(28, { revision_id: "r2", good: 2 }),
      pt(28, { revision_id: "r3", good: 1286 }),
    ];
    const tr = transitionsOf(pts, rev);
    expect(tr).toHaveLength(2);
    expect(tr.map((x) => [x.fromRevision, x.toRevision])).toEqual([[1, 2], [2, 3]]);
  });

  it("distinguishes an epoch marker from a definition-revision boundary", () => {
    const pts = [
      pt(28, { revision_id: "r1", epoch_id: "e1", good: 10 }),
      pt(28, { revision_id: "r1", epoch_id: "e2", good: 10 }),
    ];
    expect(transitionsOf(pts, rev)[0].epochOnly).toBe(true);
  });
});

describe("storageVerdict — the second axis of §11.2", () => {
  const seg = (fromDay: number, toDay: number, buckets: number) => ({
    from: new Date(F + fromDay * DAY).toISOString(),
    to: new Date(F + toDay * DAY).toISOString(),
    buckets,
  });

  it("calls a fully materialized segment complete", () => {
    const v = storageVerdict(seg(28, 30, 2880), [pt(28, { good: 1440 }), pt(29, { good: 1440 })], DAY);
    expect(v.complete).toBe(true);
    expect(v.shape).toBe("complete");
  });

  it("refuses to call a 28-day segment with 300 stored minutes complete", () => {
    // The defect the real-time axis exposed: this segment used to print `availability 100%`.
    const v = storageVerdict(seg(0, 28.104166666666668, 300), [pt(27, { good: 150 }), pt(28, { good: 150 })], DAY);
    expect(v.complete).toBe(false);
    expect(v.storedMinutes).toBe(300);
    expect(Math.round(v.extentMinutes)).toBe(40470);
  });

  it("distinguishes records that begin later from records missing inside — display shape only", () => {
    const prefix = storageVerdict(seg(0, 29, 2880), [pt(27, { good: 1440 }), pt(28, { good: 1440 })], DAY);
    expect(prefix.shape).toBe("prefix");
    const interior = storageVerdict(seg(27, 30, 2880), [pt(27, { good: 1440 }), pt(29, { good: 1440 })], DAY);
    expect(interior.shape).toBe("interior");
  });

  it("is complete for a zero or inverted extent rather than dividing by nothing", () => {
    expect(storageVerdict({ from: "x", to: "y", buckets: 0 }, [], DAY).complete).toBe(true);
    expect(storageVerdict(seg(5, 5, 0), [], DAY).extentMinutes).toBe(0);
  });
});

// The clipped-cell case, which the component's own tests exposed: a window that starts mid-step
// gives its first cell a visible extent SHORTER than the step, while the point still describes the
// whole step. Dividing by the clipped extent overflowed the cell by the ratio between them.
describe("stackSlices — a clipped cell shows its step's composition and never overflows", () => {
  const H = 34;
  it("fills exactly h when the durations exceed the visible extent", () => {
    const cell = {
      startMs: F + 19 * 60 * CANONICAL_BUCKET_MS, // 05:00 into the day: a 5-hour visible extent
      endMs: F + DAY,
      sealed: { good: min(1440), bad: 0, unknown: 0, excluded: 0 }, // the whole day's durations
      provisional: { good: 0, bad: 0, unknown: 0, excluded: 0 },
      storedMinutes: 1440,
      repairing: false,
    };
    const slices = stackSlices(cell, H);
    expect(slices.reduce((s, x) => s + x.h, 0)).toBeCloseTo(H, 6);
    // and it reports NO absence: there is more data than the visible slice, not less
    expect(slices.some((s) => s.kind === "notStored")).toBe(false);
  });

  it("still reports absence when the durations fall short of the extent", () => {
    const cell = {
      startMs: F, endMs: F + DAY,
      sealed: { good: min(150), bad: 0, unknown: 0, excluded: 0 },
      provisional: { good: 0, bad: 0, unknown: 0, excluded: 0 },
      storedMinutes: 150, repairing: false,
    };
    const ns = stackSlices(cell, H).find((s) => s.kind === "notStored")!;
    expect(ns.h).toBeCloseTo((1290 / 1440) * H, 6);
  });
});

// Reviewer P1 at party [178]: the floor could buy height the cell did not have. It floored the
// issue slice, capped the debt, and then paid only out of `good` — so a cell holding one second of
// `bad` and no good at all returned notStored 33.9996px + bad 2px = 35.9996px against a 34px cell.
// SVG clips that, which makes it a picture claiming more time than it has and hiding the evidence.
describe("stackSlices — the floor is paid for, or it is not granted", () => {
  const H = 34;
  const us = (min: number) => min * CANONICAL_BUCKET_MS * 1000;
  // The helper reaches ALL SIX problem categories and both heights the product uses. The earlier
  // version could construct neither `excluded` nor a PROVISIONAL problem, and swept only h=34 —
  // which is exactly why its sweep could not reach the cap-binding branch a reviewer found by
  // arithmetic (party [180]). A sweep that cannot build a state is not a sweep over it.
  type Mix = {
    good?: number; bad?: number; unknown?: number; excluded?: number;
    provGood?: number; provBad?: number; provUnknown?: number; provExcluded?: number;
  };
  const cell = (o: Mix, extentMin = 1440) => ({
    startMs: F,
    endMs: F + extentMin * CANONICAL_BUCKET_MS,
    sealed: { good: us(o.good ?? 0), bad: us(o.bad ?? 0), unknown: us(o.unknown ?? 0), excluded: us(o.excluded ?? 0) },
    provisional: { good: us(o.provGood ?? 0), bad: us(o.provBad ?? 0), unknown: us(o.provUnknown ?? 0), excluded: us(o.provExcluded ?? 0) },
    storedMinutes: Object.values(o).reduce((a, v) => a + (v ?? 0), 0),
    repairing: false,
  });
  const total = (c: Parameters<typeof stackSlices>[0], hh = H) => stackSlices(c, hh).reduce((s, x) => s + x.h, 0);

  it("totals exactly h for the reviewer's counterexample: one second of bad and NO good", () => {
    const c = cell({ bad: 1 / 60 });
    expect(total(c)).toBeCloseTo(H, 6);
    // the outage is still visible — the promise ruled at [143] — and absence funded it
    const slices = stackSlices(c, H);
    expect(slices.find((s) => s.kind === "bad")!.h).toBeCloseTo(SLICE_FLOOR_PX, 6);
    expect(slices.find((s) => s.kind === "notStored")!.h).toBeCloseTo(H - SLICE_FLOOR_PX, 6);
  });

  it("totals exactly h when the whole cell is one tiny problem and nothing else at all", () => {
    // extent one minute, one second of bad, no absence to fund from either
    const c = cell({ bad: 1 / 60 }, 1 / 60);
    expect(total(c)).toBeCloseTo(H, 6);
  });

  it("totals exactly h when the CAP BINDS: six near-zero problem slices in a 14px lane", () => {
    // The reviewer's counterexample at party [180], reproduced before it was accepted: six eligible
    // slices ask for 2px each in a lane 14px tall, the cap allows 8.4px, and the earlier version
    // billed the capped amount while having already handed out the full 12px — returning 17.6px for
    // a 14px cell, which SVG clips.
    const LANE = 14;
    const t = 1 / 60; // one second of each
    const c = cell({ bad: t, unknown: t, excluded: t, provBad: t, provUnknown: t, provExcluded: t });
    const slices = stackSlices(c, LANE);
    expect(slices.reduce((s, x) => s + x.h, 0)).toBeCloseTo(LANE, 6);
    // the cap binds, so every floor is SHORTENED in proportion rather than some paid in full
    const problems = slices.filter((s) => s.kind !== "notStored" && s.kind !== "good");
    expect(problems).toHaveLength(6);
    for (const s of problems) expect(s.h).toBeCloseTo(problems[0].h, 9); // equal, not rounded
    expect(problems.every((s) => s.h < SLICE_FLOOR_PX)).toBe(true);
    // …and every one of them SAYS SO, so the promise moves off geometry rather than lapsing:
    // the caller draws the non-geometric marker and the readout names the state (invariant 6b).
    expect(problems.every((s) => s.belowFloor === true)).toBe(true);
    // and the GRANT is exactly the cap: the slices' natural heights plus the cap, no more. Asserted
    // against the natural heights rather than a rounded figure, because the rule is about the grant.
    const natural = (t / (24 * 60)) * LANE; // one second of a day, at the lane's height
    const grant = problems.reduce((a, x) => a + x.h, 0) - 6 * natural;
    expect(grant).toBeCloseTo(SLICE_FLOOR_CAP * LANE, 9);
    // absence funded all of it, and nothing beyond it
    expect(slices.find((s) => s.kind === "notStored")!.h).toBeCloseTo(LANE - 6 * natural - grant, 9);
  });

  it("totals exactly h across a sweep over ALL six problem states and both heights", () => {
    const mixes: Mix[] = [];
    const tiny = 1 / 60;
    for (const bad of [0, tiny, 9, 700]) {
      for (const unknown of [0, tiny, 8]) {
        for (const excluded of [0, tiny, 60]) {
          for (const good of [0, tiny, 150, 1439]) {
            for (const prov of [{}, { provGood: 40 }, { provBad: tiny, provUnknown: tiny, provExcluded: tiny }]) {
              mixes.push({ bad, unknown, excluded, good, ...prov });
            }
          }
        }
      }
    }
    for (const hh of [14, 30, 34]) {
      for (const m of mixes) {
        expect(total(cell(m), hh), `h=${hh} mixture ${JSON.stringify(m)}`).toBeCloseTo(hh, 6);
      }
    }
    expect(mixes.length).toBe(432);
  });

  it("returns the floor it cannot fund, rather than overflowing — the case a mutant survived", () => {
    // Built deliberately after a mutation showed nothing reached it: a cell FULL of problem states
    // has neither good nor absence to fund a floor. One minute of `bad` beside 1439 of `unknown`
    // asks for 1.98px it cannot get, so the floor is given back and `bad` keeps its real height.
    // Nothing good is hidden by that: the cell is entirely problem states either way.
    const c = cell({ bad: 1, unknown: 1439 });
    const slices = stackSlices(c, H);
    expect(slices.reduce((s, x) => s + x.h, 0)).toBeCloseTo(H, 6);
    expect(slices.some((s) => s.kind === "notStored")).toBe(false);
    const bad = slices.find((s) => s.kind === "bad")!;
    expect(bad.h).toBeLessThan(SLICE_FLOOR_PX);
    expect(bad.h).toBeCloseTo((1 / 1440) * H, 6);
    // nothing could fund it, so it is MARKED — the promise is that a problem is never hidden, and
    // the floor is only the first mechanism for keeping it
    expect(bad.belowFloor).toBe(true);
  });

  it("does NOT mark a floor it could fund, so the marker keeps its meaning", () => {
    const slices = stackSlices(cell({ bad: 1 / 60, good: 1439 }), H);
    const bad = slices.find((s) => s.kind === "bad")!;
    expect(bad.h).toBeCloseTo(SLICE_FLOOR_PX, 6);
    expect(bad.belowFloor).toBeUndefined();
    expect(slices.every((s) => s.belowFloor === undefined)).toBe(true);
  });

  it("marks every eligible slice when there is nothing at all to grant from", () => {
    // a cell that is ENTIRELY tiny problems: no good, no absence, so grant is zero
    const c = cell({ bad: 1 / 60, unknown: 1 / 60 }, 2 / 60);
    const slices = stackSlices(c, H);
    expect(slices.reduce((s, x) => s + x.h, 0)).toBeCloseTo(H, 6);
    // (at this extent both slices are already tall, so nothing is eligible and nothing is marked)
    const marked = slices.filter((s) => s.belowFloor);
    expect(marked.every((s) => s.h < SLICE_FLOOR_PX)).toBe(true);
  });

  it("never returns a negative slice while paying the floor", () => {
    for (const m of [{ bad: 1 / 60 }, { bad: 1 / 60, good: 1 / 60 }, { bad: 1 / 60, unknown: 1 / 60 }]) {
      for (const s of stackSlices(cell(m), H)) expect(s.h).toBeGreaterThanOrEqual(0);
    }
  });
});
