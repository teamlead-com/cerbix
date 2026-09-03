// The monitor's Response time panel (func-truthful-rendering §6, FR-031, D-0235).
//
// WHAT WAS WRONG. `latencySeries` ended in `.filter((v) => v > 0)`, so every failure carrying no
// latency vanished from the picture while the table two panels down showed it — composite
// evaluation carries no latency on any path, and neither does a config refusal, an ICMP setup
// failure or an unreadable canary workflow. A network timeout was NOT affected: the prober records
// elapsed latency on failure. The x axis was a bare index, so position carried no time, and after
// that filter index `i` did not even correspond to heartbeat `i`. And the dashed reference came
// from the 24h window while the drawn series was the last <=60 checks — two populations, one
// picture, nothing saying so.
//
// WHY THERE IS NO LINE, and why that is structural rather than a matter of care. A stroke over a
// real time axis draws through time nobody measured, and the frontend may not decide that a check
// was DUE and missing: cadence, probe overlap and dispatch delay are application semantics. Two
// designs for deriving an allowance from published values were rejected, the second on three
// readings of the tree — a run may occupy `(retries+1) * timeout`, `result.allowed_skew` bounds a
// clock test rather than delivery, and `job_issued_at` is not a column of `heartbeats` at all. So
// two received heartbeats bound OBSERVED SPACING and nothing more. FR-032 is the requirement that
// would earn a stroke back.
//
// SO ABSENCE IS RENDERED POSITIVELY. The observation ruler carries one neutral tick per recorded
// heartbeat and nothing between them; its empty spans mean one thing uniformly — no check was
// recorded between adjacent points — with NO threshold. A fixed pixel threshold for marking gaps
// was specified and withdrawn once the arithmetic was done: at this panel's density the ordinary
// spacing is larger than the threshold, so it would mark every interval and tell a hole apart from
// an ordinary minute not at all, and any threshold relative to the panel's own spacing would be an
// anomaly detector in a legibility costume.

export type HeartbeatLike = {
  ts?: string;
  up?: boolean;
  latency_ms?: number | null;
  code?: number | null;
  msg?: string | null;
};

export type PanelPoint = {
  ms: number;
  /** null when the check recorded no latency at all — drawn on the baseline, never dropped */
  latency: number | null;
  hb: HeartbeatLike;
};

/** Every fetched heartbeat, oldest first. Nothing is filtered out; that was the defect. */
export function buildPoints(heartbeats: HeartbeatLike[]): PanelPoint[] {
  const out: PanelPoint[] = [];
  for (const hb of heartbeats) {
    const ms = hb.ts ? Date.parse(hb.ts) : Number.NaN;
    if (Number.isNaN(ms)) continue;
    const raw = hb.latency_ms;
    out.push({ ms, latency: raw == null || raw <= 0 ? null : raw, hb });
  }
  return out.sort((a, b) => a.ms - b.ms);
}

export type PanelStats = {
  /** points carrying a latency — the population avg and p95 describe */
  measured: number;
  /** every point drawn, latency or not */
  drawn: number;
  avg: number | null;
  p95: number | null;
  /** the plot's upper bound, from the data */
  extent: number;
  /** the timeout is drawn ONLY when it falls inside that extent — a factual boundary */
  timeoutInScale: boolean;
};

export function panelStats(points: PanelPoint[], timeoutMs: number): PanelStats {
  const vals = points.map((p) => p.latency).filter((v): v is number => v != null);
  const sorted = vals.slice().sort((a, b) => a - b);
  const p95 = sorted.length ? sorted[Math.min(sorted.length - 1, Math.ceil(0.95 * sorted.length) - 1)] : null;
  const avg = vals.length ? vals.reduce((a, b) => a + b, 0) / vals.length : null;
  const extent = (sorted.length ? sorted[sorted.length - 1] : 0) * 1.15;
  return {
    measured: vals.length,
    drawn: points.length,
    avg,
    p95,
    extent,
    timeoutInScale: timeoutMs > 0 && extent > 0 && timeoutMs <= extent,
  };
}

export type EmptySpan = {
  fromMs: number;
  toMs: number;
  /** how many adjacent intervals this ONE focus target covers */
  intervals: number;
  merged: boolean;
};

/**
 * The ruler's focusable empty spans.
 *
 * Every interval between adjacent recorded checks is unobserved time — that is true of each pair
 * by construction and needs no rule. Adjacent spans too narrow to be their own hit target are
 * MERGED for interaction only, and a merged span states its real outer bounds. Interaction
 * grouping is never rendered as semantic grouping: the ticks and the geometry are untouched.
 */
export function emptySpans(points: PanelPoint[], pxPerMs: number, minHitPx: number): EmptySpan[] {
  const out: EmptySpan[] = [];
  let open: EmptySpan | null = null;
  for (let i = 1; i < points.length; i++) {
    const fromMs = points[i - 1].ms;
    const toMs = points[i].ms;
    if ((toMs - fromMs) * pxPerMs >= minHitPx) {
      if (open) { out.push(open); open = null; }
      out.push({ fromMs, toMs, intervals: 1, merged: false });
      continue;
    }
    if (!open) open = { fromMs, toMs, intervals: 1, merged: false };
    else { open.toMs = toMs; open.intervals += 1; open.merged = true; }
  }
  if (open) out.push(open);
  return out;
}

/** The widest interval between two RECORDED checks — never a count of checks thought to be due. */
export function widestSpan(spans: EmptySpan[]): EmptySpan | null {
  if (!spans.length) return null;
  return spans.reduce((a, s) => (s.toMs - s.fromMs > a.toMs - a.fromMs ? s : a), spans[0]);
}

/**
 * An interval in a unit that fits it.
 *
 * Found by looking at the running stack: a fixed `Math.round(ms / 60000)` renders an
 * eleven-second interval as "0 min", which is not merely useless — it is a false statement about
 * the interval, on a panel whose whole subject is not making those.
 */
export function gapLabel(ms: number): string {
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rs = s % 60;
  if (m < 60) return rs ? `${m}m ${rs}s` : `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return rm ? `${h}h ${rm}m` : `${h}h`;
}
