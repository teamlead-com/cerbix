// The reliability timeline's geometry (func-truthful-rendering §5, FR-031, D-0235).
//
// Pure functions, deliberately outside the component: this is the part that was wrong before, so
// it is the part that gets tested without a DOM. The component draws what these return.
//
// THE AXIS IS CLOCK TIME. Cells are generated from the REQUESTED UTC range at the rollup grain,
// not from whichever points the response happens to carry. The old strip laid points out
// consecutively by bucket count, so a 30-day window holding two and a half days of facts filled
// its whole width — 6.6% of a month drawn as the month. Bucket count was a sound width proxy
// INSIDE measured coverage and fails in exactly one place, a storage hole, where it compresses
// unmeasured time to zero width. Time with no stored bucket now occupies width in its own
// encoding, which refines rather than reverses the time-weighted ruling of party [218] P1-3: a
// boundary fragment still gets a width proportional to its real extent.
//
// HEIGHT CARRIES QUANTITY. A cell is a proportional stack of the states measured inside it, not
// one winning hue. Winner-takes-all colouring rendered a service at 99.667% availability as a wall
// of red once only three wide ticks were on screen. `bad`, `unknown` and `excluded` slices carry a
// FLOOR so a one-second outage cannot vanish; the floor is presentation only, it is capped so the
// other slices still fit, it is paid for out of `good`, and it is never applied to provisional good
// time — flooring that would inflate good time and buy no honesty.
//
// WIDTH CARRIES DURATION, and is never floored. That is why a sub-pixel segment gets a marker
// instead of a widened lane, and why the floor above and the lane rule are two mechanisms for two
// axes rather than two flavours of one.

/** The fixed width every fact is keyed by — the client mirror of `domain.CanonicalBucket`. */
export const CANONICAL_BUCKET_MS = 60_000;

/**
 * Minimum drawn height a `bad` / `unknown` / `excluded` slice is GRANTED where the cell can fund
 * it. It is not an absolute floor and the specification does not claim one: height is bounded by
 * the cell, so a cap that binds or a cell with nothing to fund from leaves a slice below it. What
 * IS absolute is that the problem is not hidden — see `Slice.belowFloor`.
 */
export const SLICE_FLOOR_PX = 2;
/** The floor's cap: floors together may not take more than this share of a cell. */
export const SLICE_FLOOR_CAP = 0.6;

export type Durations = {
  GoodUs?: number;
  BadUs?: number;
  UnknownUs?: number;
  ExcludedUs?: number;
};

export type SeriesPointLike = {
  start?: string;
  epoch_id?: string;
  revision_id?: string;
  provisional?: boolean;
  buckets?: number;
  durations?: Durations;
};

export type Interval = { from?: string; to?: string };

export type StateUs = { good: number; bad: number; unknown: number; excluded: number };

export type Cell = {
  startMs: number;
  endMs: number;
  /** durations of points sealed at or before `sealed_through` */
  sealed: StateUs;
  /** durations of points after `sealed_through`, drawn at reduced opacity and in no number */
  provisional: StateUs;
  /** canonical minute buckets actually stored inside this cell */
  storedMinutes: number;
  /** an active repair intersects this cell: it is rendered as work in progress, never as data */
  repairing: boolean;
};

const zero = (): StateUs => ({ good: 0, bad: 0, unknown: 0, excluded: 0 });

function add(into: StateUs, d: Durations | undefined) {
  into.good += d?.GoodUs ?? 0;
  into.bad += d?.BadUs ?? 0;
  into.unknown += d?.UnknownUs ?? 0;
  into.excluded += d?.ExcludedUs ?? 0;
}

/** Truncate to the rollup grain the server keyed the step by (`date_trunc` over the UTC projection). */
export function truncToStep(ms: number, stepMs: number): number {
  return Math.floor(ms / stepMs) * stepMs;
}

/**
 * Cells covering `[fromMs, toMs)` at the rollup grain, clipped at both ends, in order.
 *
 * A step with no points is still a cell — that is the whole change. Its `storedMinutes` is zero
 * and the component draws it in the `not-stored` encoding, which is NOT the same thing as
 * `unknown`: `unknown` is a decided verdict with `unknown_us` behind it, and a missing bucket row
 * is the absence of a verdict.
 */
export function buildCells(
  fromMs: number,
  toMs: number,
  stepMs: number,
  points: SeriesPointLike[],
  repairing: Interval[] = [],
): Cell[] {
  if (!(toMs > fromMs) || !(stepMs > 0)) return [];
  const byStep = new Map<number, SeriesPointLike[]>();
  for (const p of points) {
    const t = p.start ? Date.parse(p.start) : Number.NaN;
    if (Number.isNaN(t)) continue;
    const key = truncToStep(t, stepMs);
    const list = byStep.get(key);
    if (list) list.push(p);
    else byStep.set(key, [p]);
  }
  const repairs = repairing
    .map((r) => [Date.parse(r.from ?? ""), Date.parse(r.to ?? "")] as const)
    .filter(([a, b]) => !Number.isNaN(a) && !Number.isNaN(b) && b > a);

  const out: Cell[] = [];
  for (let step = truncToStep(fromMs, stepMs); step < toMs; step += stepMs) {
    const startMs = Math.max(step, fromMs);
    const endMs = Math.min(step + stepMs, toMs);
    if (endMs <= startMs) continue;
    const cell: Cell = { startMs, endMs, sealed: zero(), provisional: zero(), storedMinutes: 0, repairing: false };
    for (const p of byStep.get(step) ?? []) {
      add(p.provisional ? cell.provisional : cell.sealed, p.durations);
      cell.storedMinutes += p.buckets ?? 0;
    }
    cell.repairing = repairs.some(([a, b]) => a < endMs && startMs < b);
    out.push(cell);
  }
  return out;
}

export type SliceKind = "notStored" | "bad" | "unknown" | "excluded" | "good";
export type Slice = {
  kind: SliceKind;
  h: number;
  provisional: boolean;
  /**
   * A problem slice the geometry could NOT bring up to the floor — the cap bound, or nothing could
   * fund it. The promise is that a problem is never hidden, and the floor is only the FIRST
   * mechanism for keeping it: height is bounded by the cell, so it can run out. When it does, the
   * caller draws the same non-geometric marker a sub-pixel SEGMENT gets, for the same reason —
   * geometry cannot show this at this size, so something that is not geometry says so.
   */
  belowFloor?: boolean;
};

/**
 * A cell's stack, top to bottom, in a fixed order so cells stay comparable:
 * not-stored, then the sealed problems, then the provisional slices, then sealed good.
 *
 * Heights are shares of the cell's own extent, because height is QUANTITY. A cell under an active
 * repair returns nothing: the component masks it as work, and work is not data.
 */
export function stackSlices(cell: Cell, h: number): Slice[] {
  if (cell.repairing) return [];
  const extentUs = (cell.endMs - cell.startMs) * 1000;
  if (extentUs <= 0 || h <= 0) return [];
  const measuredUs =
    cell.sealed.good + cell.sealed.bad + cell.sealed.unknown + cell.sealed.excluded +
    cell.provisional.good + cell.provisional.bad + cell.provisional.unknown + cell.provisional.excluded;
  // The denominator is the LARGER of the cell's visible extent and the durations it actually
  // holds. Both directions matter and the first version of this function got one of them wrong:
  //   durations < extent  ->  the remainder is absence, and `notStored` shows it;
  //   durations > extent  ->  the cell is CLIPPED to the requested range while its point still
  //                           describes the whole rollup step, so the stack shows that step's
  //                           COMPOSITION and reports no absence. Dividing by the clipped extent
  //                           instead made a window starting mid-day overflow its own cell by the
  //                           ratio of the step to the visible part.
  const denomUs = Math.max(extentUs, measuredUs);
  const raw: Array<[SliceKind, number, boolean]> = [
    ["notStored", denomUs - measuredUs, false],
    ["bad", cell.sealed.bad, false],
    ["unknown", cell.sealed.unknown, false],
    ["excluded", cell.sealed.excluded, false],
    ["bad", cell.provisional.bad, true],
    ["unknown", cell.provisional.unknown, true],
    ["excluded", cell.provisional.excluded, true],
    ["good", cell.provisional.good, true],
    ["good", cell.sealed.good, false],
  ];
  const slices: Slice[] = [];
  for (const [kind, us, provisional] of raw) {
    if (us <= 0) continue;
    slices.push({ kind, h: (us / denomUs) * h, provisional });
  }

  // THE STACK TOTALS EXACTLY `h`, BY CONSTRUCTION. A cell that draws taller than its own extent
  // claims more time than it has — this requirement's own subject — and SVG clips the excess, so
  // the picture would both overstate and hide the overstatement.
  //
  // The cap therefore limits the GRANTS, not the debt. An earlier version raised every eligible
  // slice to the floor first and clamped the accumulated debt afterwards, which billed less than it
  // had already handed out: six near-zero problem slices in a 14 px lane asked for 12 px, were
  // billed 8.4 px by the cap, and the difference was never taken from anyone. Reviewer P1 at party
  // [180] found it, in the one branch this function's own sweep could not construct.
  //
  // The floor belongs to a PROBLEM or a missing verdict and to nothing else: `good` is not floored
  // in either form, because provisional good time is good time that is merely unsealed and
  // inflating it would buy no honesty.
  const eligible = slices.filter((s) => s.kind !== "notStored" && s.kind !== "good" && s.h < SLICE_FLOOR_PX);
  const want = eligible.map((s) => SLICE_FLOOR_PX - s.h);
  const wantTotal = want.reduce((a, b) => a + b, 0);
  if (wantTotal <= 0) return slices;
  // If nothing can be granted at all, every eligible slice stays sub-floor and says so.
  const markAll = () => eligible.forEach((s) => (s.belowFloor = true));

  // Funded in a stated order: sealed good, then provisional good, then — as a LAST RESORT — the
  // absence. That last funder overrules this function's own earlier note that absence may never
  // shrink: a cell holding one second of `bad` and no good at all has nothing else to pay with, and
  // "the problem is visible" (ruled at party [143]), "absence never shrinks" and "the total is
  // exact" cannot all hold there. The first was ruled and the third is not negotiable, so the
  // second yields — bounded by the cap, and only when nothing else can pay.
  const funders = [
    ...slices.filter((s) => s.kind === "good" && !s.provisional),
    ...slices.filter((s) => s.kind === "good" && s.provisional),
    ...slices.filter((s) => s.kind === "notStored"),
  ];
  const available = funders.reduce((a, s) => a + s.h, 0);
  const grant = Math.min(wantTotal, SLICE_FLOOR_CAP * h, available);
  if (grant <= 0) {
    markAll();
    return slices;
  }

  // Granted in proportion to what each slice asked for, so a cap that binds shortens every floor
  // rather than paying some slices in full and others not at all.
  const scale = grant / wantTotal;
  eligible.forEach((s, i) => {
    s.h += want[i] * scale;
    // Marked, not widened: the cell has no more height to give, so the promise moves off geometry.
    if (s.h < SLICE_FLOOR_PX) s.belowFloor = true;
  });

  let owed = grant;
  for (const f of funders) {
    if (owed <= 0) break;
    const take = Math.min(owed, f.h);
    f.h -= take;
    owed -= take;
  }
  return slices;
}

export type Transition = { ms: number; fromRevision: number; toRevision: number; epochOnly: boolean };
export type MarkCluster = {
  /** the anchor: the EARLIEST real boundary in the cluster, never its midpoint */
  ms: number;
  lastMs: number;
  count: number;
  members: Transition[];
};

/**
 * Boundary marks, clustered when their fixed-width marks would collide (§5.3, ruled at [140] and
 * [157]).
 *
 * Several definition changes minutes apart put their marks inside one pixel at ANY window zoom, so
 * the marks are clustered rather than overdrawn. The cluster's anchor is its earliest real
 * boundary: that anchor is the only geometric claim a cluster mark makes, and it stays true. The
 * count and the members travel with it so the readout can list them chronologically — a count may
 * not stand in for the changes it counts.
 */
export function clusterTransitions(
  transitions: Transition[],
  pxPerMs: number,
  markWidthPx: number,
): MarkCluster[] {
  const sorted = transitions.slice().sort((a, b) => a.ms - b.ms);
  const out: MarkCluster[] = [];
  for (const t of sorted) {
    const last = out[out.length - 1];
    if (last && (t.ms - last.lastMs) * pxPerMs < markWidthPx) {
      last.lastMs = t.ms;
      last.count += 1;
      last.members.push(t);
      continue;
    }
    out.push({ ms: t.ms, lastMs: t.ms, count: 1, members: [t] });
  }
  return out;
}

/**
 * Transitions INSIDE the window, read from consecutive series points.
 *
 * A segment's start is a boundary only when another segment ends there, so the first point's own
 * revision is not a transition — the first drawing of the mock counted it and claimed three
 * boundaries in a four-minute cluster when there were two.
 */
export function transitionsOf(points: SeriesPointLike[], revisionOf: (id?: string) => number): Transition[] {
  const out: Transition[] = [];
  for (let i = 1; i < points.length; i++) {
    const prev = points[i - 1];
    const cur = points[i];
    const ms = cur.start ? Date.parse(cur.start) : Number.NaN;
    if (Number.isNaN(ms)) continue;
    if (cur.revision_id !== prev.revision_id) {
      out.push({ ms, fromRevision: revisionOf(prev.revision_id), toRevision: revisionOf(cur.revision_id), epochOnly: false });
    } else if (cur.epoch_id !== prev.epoch_id) {
      out.push({ ms, fromRevision: revisionOf(prev.revision_id), toRevision: revisionOf(cur.revision_id), epochOnly: true });
    }
  }
  return out;
}

export type StorageVerdict = {
  storedMinutes: number;
  extentMinutes: number;
  complete: boolean;
  /**
   * DISPLAY-ONLY shape, with no code identity (ruled at party [176]). A client may derive
   * presentation from the series it holds; it may not manufacture a wire verdict whose canonical
   * meaning belongs server-side. This never leaves the component, never enters metrics, audit or
   * the API, and never drives reliability state.
   */
  shape: "complete" | "prefix" | "interior";
};

/**
 * A segment's storage, from the payload's own bucket count against the segment's real extent.
 *
 * §11.2 of `func-service-reliability.md` makes storage continuity and decidable coverage
 * INDEPENDENT questions that must both pass. A segment spanning 28 days with 300 stored minutes
 * renders 27 days of absence, and `coverage = 100%` says only that every observation that EXISTS
 * was decidable — which a hole makes worthless as a warrant. So an incomplete segment quotes no
 * availability, and the caller shows the dash.
 */
export function storageVerdict(
  seg: { from?: string; to?: string; buckets?: number },
  points: SeriesPointLike[],
  stepMs: number,
): StorageVerdict {
  const a = Date.parse(seg.from ?? "");
  const b = Date.parse(seg.to ?? "");
  const extentMinutes = Number.isNaN(a) || Number.isNaN(b) || b <= a ? 0 : (b - a) / CANONICAL_BUCKET_MS;
  const storedMinutes = seg.buckets ?? 0;
  if (extentMinutes <= 0 || storedMinutes >= extentMinutes) {
    return { storedMinutes, extentMinutes, complete: true, shape: "complete" };
  }
  const starts = points
    .map((p) => (p.start ? Date.parse(p.start) : Number.NaN))
    .filter((t) => !Number.isNaN(t))
    .sort((x, y) => x - y);
  if (!starts.length) return { storedMinutes, extentMinutes, complete: false, shape: "prefix" };
  const firstMs = Math.max(a, starts[0]);
  const lastEndMs = Math.min(b, starts[starts.length - 1] + stepMs);
  const missingHead = (firstMs - a) / CANONICAL_BUCKET_MS;
  const missingTail = (b - lastEndMs) / CANONICAL_BUCKET_MS;
  const missingInside = extentMinutes - storedMinutes - missingHead - missingTail;
  return {
    storedMinutes,
    extentMinutes,
    complete: false,
    shape: missingInside > 0.5 ? "interior" : "prefix",
  };
}
