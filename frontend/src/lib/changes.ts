// Pure helpers for EVERY change-intelligence surface (FR-025, D-0210): the service card
// (components/ServiceChanges.vue), the comparison view (views/ChangeCompareView.vue), the marks on
// ReliabilityStrip.vue, and — task 7 part 2 — the per-service timeline and the incident page's
// "Preceded by". Everything here is a function of its arguments — no store, no clock unless one is
// passed — so the rendering rules are unit-testable without mounting anything, and there is ONE
// place for each of them:
//
//   * how a KIND is said (kindGlyph, kindLabel, KIND_CLIP): SHAPE + text — ▲ deploy, ▼ rollback,
//     ◆ flag — drawn in the accent or in ink, never a status hue. A deploy is not good or bad; what
//     FOLLOWED it is, and the facts strip beside it says that in the hues the facts already own (D14).
//   * the phases' domain order (PHASE_ORDER, isTerminal, terminalOf, groupLatest): `started`, then
//     exactly one of `succeeded · failed · cancelled`; a terminal alone is a valid group (D3, D6).
//   * how a comparison side is quoted (formatCompareSide): a FIGURE, or WITHHELD with the page's
//     own reason word, or PENDING with `sealed_through` — never a partial number (D8, D-0211).
//   * the delta's chip (deltaChip): coloured by its SIGN and by nothing else.
//   * the horizons, the card's ranges and page size (HORIZONS, RANGES, defaultRange, CARD_PAGE_SIZE).
//   * how a refusal is spelled (CHANGE_ERROR_TEXT, describeChangeFailure): one code→message map,
//     the code split off the server's `code: rule` rendering, a 429's Retry-After, the transport's
//     own words for a network failure, an unknown code verbatim — never swallowed.
//   * the strip marks (stripMarksOf): one per TERMINAL phase, placed by `occurred_at` (D14).
import type { components } from "@/api/schema";
import { CHIP_DOWN, CHIP_PLAIN, describeFailure, failureOf, fmtPercent, shortId, transportFailureOf, type GateFailure } from "@/lib/gate";
import { humanDuration, sealedLabel } from "@/lib/services";

type Schemas = components["schemas"];
export type ChangeKind = Schemas["ChangeKind"];
export type ChangePhaseName = Schemas["ChangePhaseName"];
export type ChangePhase = Schemas["ChangePhase"];
export type ChangeGroup = Schemas["ChangeGroup"];
export type ChangeGroupList = Schemas["ChangeGroupList"];
export type ChangeDecisionLink = Schemas["ChangeDecisionLink"];
export type ChangeIncidentLink = Schemas["ChangeIncidentLink"];
export type ChangeCompare = Schemas["ChangeCompare"];
export type ChangeCompareSide = Schemas["ChangeCompareSide"];
export type ChangeHorizon = ChangeCompare["horizon"];
export type ChangeTerminalPhase = ChangeCompare["terminal_phase"];
export type ChangeWithheldReason = NonNullable<ChangeCompareSide["withheld"]>;

// ── Kinds: shape + text, never a hue ────────────────────────────────────────────────────────────

/** The three kinds, closed (D2), in the mock's order. */
export const CHANGE_KINDS: readonly ChangeKind[] = ["deploy", "rollback", "flag"];

const GLYPH: Record<ChangeKind, string> = { deploy: "▲", rollback: "▼", flag: "◆" };

/** The text glyph of a kind; an unfamiliar kind (a newer server) is a plain square, never hidden. */
export function kindGlyph(kind: string): string {
  return GLYPH[kind as ChangeKind] ?? "■";
}

/** The kind's word as the API spells it; "" reads as "change". */
export function kindLabel(kind: string): string {
  return kind || "change";
}

/**
 * The mock's `clip-path` per kind (`.mark`, `.strip .cm`): the SHAPE is the kind, the fill is the
 * accent. Shared by the card's row glyph and the strip's marks so they cannot drift apart.
 */
export const KIND_CLIP: Record<ChangeKind, string> = {
  deploy: "polygon(50% 0, 100% 100%, 0 100%)",
  rollback: "polygon(0 0, 100% 0, 50% 100%)",
  flag: "polygon(50% 0, 100% 50%, 50% 100%, 0 50%)",
};

/** `clip-path` for a kind; an unfamiliar kind is `none` — a square, still a mark. */
export function kindClip(kind: string): string {
  return KIND_CLIP[kind as ChangeKind] ?? "none";
}

// ── Phases: the domain's order ──────────────────────────────────────────────────────────────────

/** `started` then the three terminals (D3). */
export const PHASE_ORDER: readonly ChangePhaseName[] = ["started", "succeeded", "failed", "cancelled"];
export const TERMINAL_PHASES: readonly ChangePhaseName[] = ["succeeded", "failed", "cancelled"];

export function isTerminal(phase: string): boolean {
  return TERMINAL_PHASES.includes(phase as ChangePhaseName);
}

/** The phase's word as the API spells it. */
export function phaseLabel(phase: string): string {
  return phase;
}

export type PhaseTone = "plain" | "down" | "muted";

/** A failed phase reads down, a cancelled one muted, everything else plain (mock `.phases span.fail`). */
export function phaseTone(phase: string): PhaseTone {
  if (phase === "failed") return "down";
  if (phase === "cancelled") return "muted";
  return "plain";
}

function phaseRank(phase: string): number {
  const i = PHASE_ORDER.indexOf(phase as ChangePhaseName);
  return i < 0 ? PHASE_ORDER.length : i;
}

/** The phases in the domain's order (the server nests them so; this is the one place that relies on it). */
export function sortPhases(phases: readonly ChangePhase[]): ChangePhase[] {
  return [...phases].sort((a, b) => phaseRank(a.phase) - phaseRank(b.phase) || a.occurred_at.localeCompare(b.occurred_at));
}

/** The group's ONE terminal phase, or undefined for a started-only group (no mark, no comparison). */
export function terminalOf(group: Pick<ChangeGroup, "phases">): ChangePhase | undefined {
  return group.phases.find((p) => isTerminal(p.phase));
}

/** The group's latest phase by `occurred_at` — the row's actor, `ref` and `url` are its (D6). */
export function groupLatest(group: Pick<ChangeGroup, "phases">): ChangePhase | undefined {
  let latest: ChangePhase | undefined;
  for (const p of group.phases) {
    if (!latest || p.occurred_at > latest.occurred_at || (p.occurred_at === latest.occurred_at && phaseRank(p.phase) > phaseRank(latest.phase))) {
      latest = p;
    }
  }
  return latest;
}

/** The identity key `(source, external_id)` — the only key a group has (D4); the separator cannot occur in a slug. */
export function groupKey(g: Pick<ChangeGroup, "source" | "external_id">): string {
  return `${g.source}${g.external_id}`;
}

/** The row's actor: the latest phase's label (`token:<name>` for an API token). */
export function groupActor(group: Pick<ChangeGroup, "phases">): string {
  return groupLatest(group)?.actor_label ?? "";
}

/** Every distinct actor across the phases, in phase order — for the actor chip's title when they differ. */
export function groupActors(group: Pick<ChangeGroup, "phases">): string[] {
  const out: string[] = [];
  for (const p of sortPhases(group.phases)) if (p.actor_label && !out.includes(p.actor_label)) out.push(p.actor_label);
  return out;
}

// ── The decision link (D11) ─────────────────────────────────────────────────────────────────────

export type DecisionView =
  | { kind: "live"; id: string; short: string; state: string; action?: string; overridden: boolean }
  | { kind: "aged_out"; id: string; short: string };

/** The decision a change rested on: the live ledger row's state/action, or the id said to be aged out. */
export function decisionView(d: ChangeDecisionLink | undefined | null): DecisionView | null {
  if (!d) return null;
  if (d.aged_out || !d.state) return { kind: "aged_out", id: d.decision_id, short: shortId(d.decision_id) };
  return { kind: "live", id: d.decision_id, short: shortId(d.decision_id), state: d.state, action: d.action, overridden: !!d.overridden };
}

// ── Horizons and the card's ranges ──────────────────────────────────────────────────────────────

/** The closed horizon vocabulary of D8, in the request's spelling and the mock's order. */
export const HORIZONS: readonly ChangeHorizon[] = ["15m", "1h", "6h", "24h"];
export const DEFAULT_HORIZON: ChangeHorizon = "1h";
const HORIZON_LABELS: Record<ChangeHorizon, string> = { "15m": "15 m", "1h": "1 h", "6h": "6 h", "24h": "24 h" };
export const HORIZON_MS: Record<ChangeHorizon, number> = { "15m": 15 * 60_000, "1h": 3_600_000, "6h": 6 * 3_600_000, "24h": 24 * 3_600_000 };

export function isHorizon(v: unknown): v is ChangeHorizon {
  return typeof v === "string" && (HORIZONS as readonly string[]).includes(v);
}

/** "1 h" — the mock's spacing; an unfamiliar value verbatim. */
export function horizonLabel(h: string): string {
  return HORIZON_LABELS[h as ChangeHorizon] ?? h;
}

export interface CardRange {
  days: number;
  label: string;
}

/** The card's three ranges (mock screen 1), 30 days by default; the timeline view has its own pickers. */
export const RANGES: readonly CardRange[] = [
  { days: 30, label: "30 d" },
  { days: 7, label: "7 d" },
  { days: 1, label: "24 h" },
];
export const DEFAULT_RANGE_DAYS = 30;
/** D6: one timeline page may span at most a quarter. */
export const CHANGE_RANGE_MAX_DAYS = 92;
/** The card shows at most this many groups before "Show 10 more". */
export const CARD_PAGE_SIZE = 10;

/** An EXPLICIT half-open RFC3339 range ending now, never wider than D6 allows. */
export function defaultRange(days: number = DEFAULT_RANGE_DAYS, now: Date = new Date()): { from: string; to: string } {
  const span = Math.min(Math.max(days, 0), CHANGE_RANGE_MAX_DAYS);
  const to = new Date(now.getTime());
  const from = new Date(to.getTime() - span * 86_400_000);
  return { from: from.toISOString(), to: to.toISOString() };
}

/** "last 30 days" · "last 7 days" · "last 24 hours" — the count chip's words. */
export function rangeLabel(days: number): string {
  if (days === 1) return "last 24 hours";
  return `last ${days} days`;
}

// ── Instants ────────────────────────────────────────────────────────────────────────────────────

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const pad = (n: number) => String(n).padStart(2, "0");

/** "14:05" — the UTC clock of an instant; an unparseable value verbatim. */
export function clockLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}

/** "14:05:12 Z" — the clock with seconds, as the compare header quotes a phase. */
export function clockSecondsLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())} Z`;
}

/**
 * The mock's compact instant: "14:05" on the same UTC day as `now`, "Aug 27 16:40" in the same
 * year, "2025-08-27 16:40" otherwise. The full `sealedLabel` always rides in a title beside it.
 */
export function instantLabel(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const clock = `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
  if (d.toISOString().slice(0, 10) === now.toISOString().slice(0, 10)) return clock;
  if (d.getUTCFullYear() === now.getUTCFullYear()) return `${MONTHS[d.getUTCMonth()]} ${d.getUTCDate()} ${clock}`;
  return `${d.toISOString().slice(0, 10)} ${clock}`;
}

/** "13:05 → 14:05" — a side's window; the full range belongs in a title (`sealedLabel`). */
export function windowLabel(from: string, to: string): string {
  return `${clockLabel(from)} → ${clockLabel(to)}`;
}

/** "−26 m" · "−0 s" · "−1 h 05 m": the lag a change PRECEDED an incident by (D7's word, never "caused"). */
export function lagText(lagSeconds: number): string {
  const s = Math.max(0, Math.round(lagSeconds));
  if (s < 60) return `−${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `−${m} m`;
  const h = Math.floor(m / 60);
  const mm = m % 60;
  return mm ? `−${h} h ${pad(mm)} m` : `−${h} h`;
}

// ── The comparison (D8, D-0211) ─────────────────────────────────────────────────────────────────

export type CompareSideView =
  | { kind: "figure"; text: string; availability: number; good: number; bad: number; unknown: number; excluded: number; buckets: number }
  | { kind: "withheld"; text: string; reason: string; detail: string }
  | { kind: "pending"; text: string; sealedThrough: string | null };

/** The page's own reason words, spelled for the operator. */
export const WITHHELD_TEXT: Record<string, string> = {
  definition_changed: "definition changed",
  undecidable: "undecidable",
  no_facts: "no facts",
};

/** The one-line explanation under a withheld side (mock screen 3). */
export const WITHHELD_HINT: Record<string, string> = {
  definition_changed: "a declaration revision or epoch boundary sits inside this side; the page would not quote one number across it, so neither does this",
  undecidable: "coverage the page would not decide",
  no_facts: "no sealed minute in this range",
};

export function withheldLabel(reason: string): string {
  return WITHHELD_TEXT[reason] ?? reason;
}

/**
 * EXACTLY one of three shapes (the API's contract): pending first — either side may be, D-0211 —
 * then withheld, then the figure. A side that fits none (a newer server) is said to be withheld
 * with the word `unquoted`; a number is never invented.
 */
export function formatCompareSide(side: ChangeCompareSide): CompareSideView {
  if (side.pending) return { kind: "pending", text: "pending", sealedThrough: side.sealed_through ?? null };
  if (side.withheld) return { kind: "withheld", text: `withheld: ${withheldLabel(side.withheld)}`, reason: side.withheld, detail: side.detail ?? "" };
  if (typeof side.availability === "number" && Number.isFinite(side.availability)) {
    return {
      kind: "figure",
      text: fmtPercent(side.availability),
      availability: side.availability,
      good: side.good_seconds ?? 0,
      bad: side.bad_seconds ?? 0,
      unknown: side.unknown_seconds ?? 0,
      excluded: side.excluded_seconds ?? 0,
      buckets: side.buckets ?? 0,
    };
  }
  return { kind: "withheld", text: "withheld: unquoted", reason: "unquoted", detail: "" };
}

/** The bar's three shares of the decidable time (good + bad + unknown), in percent; all zero when nothing was measured. */
export function sideBar(v: Extract<CompareSideView, { kind: "figure" }>): { good: number; bad: number; unknown: number } {
  const total = v.good + v.bad + v.unknown;
  if (total <= 0) return { good: 0, bad: 0, unknown: 0 };
  return { good: (v.good / total) * 100, bad: (v.bad / total) * 100, unknown: (v.unknown / total) * 100 };
}

/** "59m 59s" · "0" — a side's seconds as the durations line spells them. */
export function secondsLabel(seconds: number): string {
  if (!seconds || seconds <= 0) return "0";
  return humanDuration(seconds * 1000);
}

/** "good 59m 59s · bad 1s · unknown 0 · excluded 0". */
export function durationsLine(v: Extract<CompareSideView, { kind: "figure" }>): string {
  return `good ${secondsLabel(v.good)} · bad ${secondsLabel(v.bad)} · unknown ${secondsLabel(v.unknown)} · excluded ${secondsLabel(v.excluded)}`;
}

/** The `up` chip — the only one lib/gate.ts does not carry (the gate never says "better"). */
export const CHIP_UP = "border-up bg-up-weak text-up";

export interface DeltaChip {
  /** "+1.66" · "−1.67" · "0.00" — two decimals, the typographic minus. */
  text: string;
  /** Chip classes: up for a rise, down for a fall, plain for no change — the SIGN, nothing else. */
  cls: string;
  sign: -1 | 0 | 1;
}

/** Two decimals with the sign; a value that rounds to zero is "0.00", never "−0.00". */
export function deltaText(delta: number): string {
  const r = Math.round(delta * 100) / 100;
  if (r === 0) return "0.00";
  return `${r > 0 ? "+" : "−"}${Math.abs(r).toFixed(2)}`;
}

/** The Δ chip, or null when the API carried no delta (a side was withheld or pending). */
export function deltaChip(delta: number | undefined | null): DeltaChip | null {
  if (delta == null || !Number.isFinite(delta)) return null;
  const r = Math.round(delta * 100) / 100;
  const sign: -1 | 0 | 1 = r > 0 ? 1 : r < 0 ? -1 : 0;
  return { text: deltaText(delta), sign, cls: sign > 0 ? CHIP_UP : sign < 0 ? CHIP_DOWN : CHIP_PLAIN };
}

/** The big Δ figure's text colour on the compare view — by sign, as the chip. */
export function deltaValueClass(sign: -1 | 0 | 1): string {
  return sign > 0 ? "text-up" : sign < 0 ? "text-down" : "text-ink";
}

// ── Refusals: ONE code→message map, ONE reader ──────────────────────────────────────────────────

/** D6's bound, the mock's sentence (screen 5). */
export const RANGE_TOO_WIDE_TEXT = "Pick at most 92 days at a time.";
/** The dashed chip on a started-only row (D-0210 item 1). */
export const NO_TERMINAL_TEXT = "before/after unavailable until a terminal phase";

/** The closed codes of FR-025's reads (D6, D8, §5a) → one sentence each; an unknown code is shown verbatim. */
export const CHANGE_ERROR_TEXT: Record<string, string> = {
  range_required: "Pick both dates.",
  range_invalid: "The end must be after the start.",
  range_too_wide: RANGE_TOO_WIDE_TEXT,
  limit_invalid: "The page size was refused.",
  cursor_invalid: "This page marker is no longer valid — apply the filters again to start from the newest change.",
  kind_invalid: "The kind filter was refused — a kind is deploy, rollback or flag.",
  source_invalid: "The source is not a valid slug.",
  external_id_invalid: "The external id was refused.",
  horizon_invalid: "The horizon must be one of 15m, 1h, 6h or 24h.",
  no_terminal_phase: "This change has no terminal phase yet — before/after is unavailable until one is recorded.",
  process_inflight: "The change reads are busy right now.",
  principal_inflight: "You already have as many change reads in flight as one principal may.",
  principal_rate: "This principal is recording changes faster than the limit allows.",
  process_rate: "The server is recording changes faster than the limit allows.",
  change_not_wired: "Change intelligence is not enabled on this server.",
};

/**
 * The code at the head of a change refusal. The server renders a closed code as `code: rule` or
 * `code (field): rule` (domain.ChangeError), the timeline's plain codes as the bare word, and a
 * tenant 404 as `not found` — this takes the first token so every one of them maps.
 */
export function changeCode(body: string): string {
  const m = /^[a-z_]+/.exec(body ?? "");
  return m ? m[0] : "";
}

/**
 * One sentence for a change refusal. 401/403 are one line (the caller shows no controls); a 404
 * with a closed code (`no_terminal_phase`) is that code's sentence and a bare `not found` is the
 * caller's own; 429 carries Retry-After; a known code its sentence; an unknown body verbatim; the
 * transport's own words for a network failure.
 */
export function describeChangeFailure(f: GateFailure, opts: { notFound?: string; denied?: string; fallback?: string } = {}): string {
  const code = changeCode(f.code);
  if (f.status === 401 || f.status === 403) return describeFailure(f, opts);
  if (f.status === 404) return CHANGE_ERROR_TEXT[code] ?? opts.notFound ?? (f.code || "Not found.");
  if (f.status === 429) {
    const why = CHANGE_ERROR_TEXT[code] ?? "Too many requests at once.";
    return f.retryAfter != null ? `${why} Try again in ${f.retryAfter} s.` : `${why} Try again shortly.`;
  }
  if (code && CHANGE_ERROR_TEXT[code]) return CHANGE_ERROR_TEXT[code];
  if (f.code) return f.code;
  return describeFailure(f, opts);
}

// ── The CLI ─────────────────────────────────────────────────────────────────────────────────────

/**
 * The exact record command for THIS project and service, by canonical id (mock screen 1's empty
 * state). The token is the literal placeholder `…` — never a value, never anything read from the
 * session or from storage. `origin` prefixes `CERBIX_URL=` when given.
 */
export function cliRecordLine(projectId: string, serviceId: string, origin = ""): string {
  const env = origin ? `CERBIX_URL=${origin} ` : "";
  return [
    `${env}CERBIX_TOKEN=… cerbix change record --project ${projectId} --service ${serviceId} \\`,
    `    --kind deploy --phase succeeded --ref v4.2.1 \\`,
    `    --source github-actions --external-id $GITHUB_RUN_ID`,
  ].join("\n");
}

// ── Strip marks (D14, invariant 19) ─────────────────────────────────────────────────────────────

/** One mark on ReliabilityStrip: an instant, a kind (the shape), a label for the title, and whether it is the selected row's. */
export interface StripMark {
  /** RFC3339 — the TERMINAL phase's `occurred_at`. */
  at: string;
  kind: ChangeKind | string;
  label: string;
  selected?: boolean;
  /** The group's identity key, for keyed rendering. */
  key?: string;
}

/** "deploy · v4.2.1 · 14:05" — the mark's title; the ref falls back to the external id. */
export function markLabel(group: Pick<ChangeGroup, "kind" | "ref" | "external_id">, terminal: ChangePhase, now: Date = new Date()): string {
  return `${kindLabel(group.kind)} · ${group.ref || group.external_id} · ${terminal.phase} ${instantLabel(terminal.occurred_at, now)} (${sealedLabel(terminal.occurred_at)})`;
}

/** One mark per TERMINAL phase: a started-only group contributes none (D14). */
export function stripMarksOf(groups: readonly ChangeGroup[], selectedKey = "", now: Date = new Date()): StripMark[] {
  const out: StripMark[] = [];
  for (const g of groups) {
    const t = terminalOf(g);
    if (!t) continue;
    const key = groupKey(g);
    out.push({ at: t.occurred_at, kind: g.kind, label: markLabel(g, t, now), key, selected: !!selectedKey && key === selectedKey });
  }
  return out;
}

/** At most this many comparison reads in flight from one screen at a time (FR-025 §5a). */
export const COMPARE_POOL = 4;

/**
 * Run `work` over `items` with at most `limit` of them in flight, in order, stopping as soon as
 * `stop()` says the caller has moved on. Resolves when every started worker has drained.
 *
 * The bound is the point. `change.read_inflight_process` is configurable down to 1, and a timeline
 * page holds up to 50 groups, so a screen that fired one request per row could saturate the
 * instance's own read permits, manufacture its own 429s and crowd out other reads — review [32].
 * The card already worked this way; this is that loop, owned once, so the two screens cannot drift
 * apart on it.
 */
export async function inPool<T>(
  items: readonly T[],
  limit: number,
  work: (item: T) => Promise<void>,
  stop?: () => boolean,
): Promise<void> {
  let i = 0;
  const worker = async () => {
    for (;;) {
      if (stop?.()) return;
      const item = items[i++];
      if (item === undefined) return;
      await work(item);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, () => worker()));
}

// ── Bounded requests ──────────────────────────────────────────────────────────────────────────
// A lax response shape shared by every boundary: openapi-fetch's `{ data, error, response }`, with
// every member optional so a rejected transport and a test fixture fit the same reader.
export type ScopeRes<T> = {
  data?: T;
  error?: unknown;
  response?: { status?: number; headers?: { get?: (name: string) => string | null } };
};
export type ScopeOutcome<T> = { kind: "ok"; data: T | undefined } | { kind: "stale" } | { kind: "failed"; failure: GateFailure };

/** No single change read may occupy its slot longer than this. */
export const REQUEST_TIMEOUT_MS = 10_000;

/**
 * One generation counter, one set of controllers, and a DEADLINE on every request.
 *
 * The deadline is not decoration once reads are pooled: `inPool` holds a slot until its work
 * resolves, so four comparisons that never settle would occupy the whole pool and leave every
 * queued row loading forever, with no fifth request able to start (review [37] — a failure the
 * bound itself introduced, since an unbounded fan-out at least asked every row). A request that
 * outlives the deadline aborts, releases its slot and reports `failed`, which is a cell the reader
 * can see rather than a spinner that never ends.
 */
export function requestScope() {
  let gen = 0;
  const inflight = new Set<AbortController>();
  const abortAll = () => {
    for (const c of inflight) c.abort();
    inflight.clear();
  };
  return {
    get gen() {
      return gen;
    },
    stale: (g: number) => g !== gen,
    /** Abort everything in flight and open the next generation. */
    begin: () => {
      abortAll();
      return ++gen;
    },
    /** Invalidate without opening a new generation (unmount). */
    close: () => {
      gen++;
      abortAll();
    },
    /**
     * Abort what is in flight WITHOUT retiring the generation — for a caller that has decided the
     * rest of its own work is pointless but is still the current one and must go on to render the
     * decision it just made. The comparison view uses it when the SUBJECT refuses: the ancillary
     * read is abandoned at once rather than left to its deadline, while the view stays live enough
     * to clear `loading` and show the service's 404 (reviews [31] and [42]).
     */
    abort: abortAll,
    async request<T>(g: number, run: (signal: AbortSignal) => Promise<ScopeRes<T>>): Promise<ScopeOutcome<T>> {
      const controller = new AbortController();
      inflight.add(controller);
      let deadline: ReturnType<typeof setTimeout> | undefined;
      try {
        const timeout = new Promise<never>((_, reject) => {
          deadline = setTimeout(() => {
            controller.abort();
            reject(new Error("the request timed out"));
          }, REQUEST_TIMEOUT_MS);
        });
        const res = (await Promise.race([run(controller.signal), timeout])) as ScopeRes<T>;
        if (g !== gen) return { kind: "stale" };
        const status = res.response?.status;
        if (res.error !== undefined || (typeof status === "number" && status >= 400)) {
          return { kind: "failed", failure: failureOf(res) };
        }
        return { kind: "ok", data: res.data };
      } catch (e) {
        if (g !== gen) return { kind: "stale" };
        return { kind: "failed", failure: transportFailureOf(e) };
      } finally {
        if (deadline) clearTimeout(deadline);
        inflight.delete(controller);
      }
    },
  };
}
