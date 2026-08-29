// FR-024 (D-0207 items 1, 4, 5): the LEDGER-specific helpers of the three history views — the
// project-scoped decision history, one decision by id, and a service's override history.
//
// Everything the views share with the service card — how a state is said (statePill, PILL_*),
// how a chip is drawn (CHIP_*), how a reason's tone follows the algebra, how a refusal is spelled
// (GATE_ERROR_TEXT, failureOf, describeFailure), the transport's words, shortId — lives ONCE in
// lib/gate.ts and is re-exported here so the views keep one import. What stays in this module is
// what only the ledger needs:
//
//   * how the listing's RANGE is built. The server requires an explicit half-open `[from, to)` of
//     at most 31 days and the SPA never relies on a server default: the calendar days the operator
//     picks become `from` at 00:00Z of the first day and `to` at 00:00Z of the day AFTER the last,
//     so the "to" date reads inclusively while the request stays half-open. Every page — the first
//     and each "Show 50 more" — sends the SAME pair; only the cursor moves.
//   * the table's compact chips and cells: a reason as its CODE, an override's status, its
//     closure, a revision, a duration in seconds.

import type { components } from "@/api/schema";

import { CHIP_ACC, CHIP_DORM, RANGE_TOO_WIDE_TEXT, reasonToneClass } from "@/lib/gate";
import { humanDuration, sealedLabel } from "@/lib/services";

export {
  CHIP_ACC,
  CHIP_BASE,
  CHIP_DORM,
  CHIP_DOWN,
  CHIP_PLAIN,
  CHIP_WARN,
  GATE_ERROR_TEXT,
  GATE_STATES,
  PILL_BASE,
  PILL_DOT,
  RANGE_TOO_WIDE_TEXT,
  describeFailure,
  failureOf,
  isAbort,
  shortId,
  statePill,
  transportFailure,
  type GateFailure,
  type GateState,
  type StatePill,
} from "@/lib/gate";

type GateReason = components["schemas"]["GateReason"];
type GateOverrideRecord = components["schemas"]["GateOverrideRecord"];

// ── The table's chips and cells ──────────────────────────────────────────────────────────────

export interface ReasonChip {
  text: string;
  cls: string;
  title: string;
}

/**
 * A reason as a TABLE chip. The tone is the one rule of lib/gate.ts (`reasonTone`): a clause that
 * matched under `block` reads down, under `warn` reads degraded; an unavailability, `not_configured`
 * or an unfamiliar shape is dormant (dashed). The text is the CODE, because for an unavailable
 * clause the code is what happened (`seal_stale`) and the clause it silenced is the detail; the
 * title carries clause, assignment, value and source.
 */
export function reasonChip(r: GateReason): ReasonChip {
  const bits: string[] = [];
  if (r.clause && r.clause !== r.code) bits.push(`clause ${r.clause}`);
  if (r.assignment) bits.push(`assigned ${r.assignment}`);
  if (r.value !== undefined && r.value !== null) bits.push(`value ${formatValue(r.value)}`);
  if (r.source) bits.push(`from ${r.source}`);
  return { text: r.code, cls: reasonToneClass(r), title: bits.join(" · ") || r.code };
}

export function formatValue(v: unknown): string {
  if (typeof v === "number") return Number.isInteger(v) ? String(v) : v.toFixed(3).replace(/\.?0+$/, "");
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

/** `active` is the accent (an act in force); `revoked`, `inert` and `expired` are dormant. */
export function overrideStatusChip(status: GateOverrideRecord["status"] | string): string {
  return status === "active" ? CHIP_ACC : CHIP_DORM;
}

/**
 * The "Closed" cell of the override history (mock screen 5): when, and by what. A manual closure
 * names its revoker; a system closure names its cause. Empty while the row is unclosed.
 */
export function closureLabel(r: Pick<GateOverrideRecord, "revoked_at" | "revoked_reason" | "revoked_by_label">): string {
  if (!r.revoked_at) return "";
  const when = sealedLabel(r.revoked_at);
  switch (r.revoked_reason) {
    case "manual":
      return `${when} · by ${r.revoked_by_label || "—"}`;
    case "expired":
      return `${when} · expired`;
    case "policy_changed":
      return `${when} · policy changed`;
    case "policy_deleted":
      return `${when} · policy deleted`;
    default:
      return when;
  }
}

/** `rev N`, or the dash — `policy_revision` is ABSENT when no policy applied (D7), never null. */
export function revisionLabel(rev: number | undefined | null): string {
  return rev == null ? "—" : `rev ${rev}`;
}

/** Seconds (the API's unit for lags and bounds) as a human duration. */
export function secondsLabel(s: number | undefined | null): string {
  if (s == null || !Number.isFinite(s)) return "—";
  return humanDuration(Math.max(0, Math.round(s * 1000)));
}

// ── The listing's range ──────────────────────────────────────────────────────────────────────

/** The server's cap on one page's span (§5: `to − from ≤ 31 days`, 400 `range_too_wide` above). */
export const MAX_RANGE_DAYS = 31;
/** The default window, "last 30 days" (mock screen 4). */
export const DEFAULT_RANGE_DAYS = 30;
/** The server's default page; sent explicitly so the two cannot drift apart silently. */
export const PAGE_SIZE = 50;

const DAY_MS = 86_400_000;

/** A calendar day, `YYYY-MM-DD`, as `<input type="date">` holds it — read as a UTC day, the ledger's partition unit. */
export function isoDay(d: Date): string {
  return d.toISOString().slice(0, 10);
}

/** Today (UTC) and the 29 days before it: 30 calendar days, inclusive of both ends. */
export function defaultRange(now: Date = new Date()): { from: string; to: string } {
  const today = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
  return { from: isoDay(new Date(today - (DEFAULT_RANGE_DAYS - 1) * DAY_MS)), to: isoDay(new Date(today)) };
}

export interface RangeBounds {
  /** RFC3339, inclusive: 00:00:00Z of the first day. */
  from: string;
  /** RFC3339, EXCLUSIVE: 00:00:00Z of the day after the last. */
  to: string;
  /** Whole days spanned by `[from, to)`. */
  days: number;
}

/**
 * The half-open request range for two calendar days. Returns null when either day does not parse.
 * `days` may be zero or negative when the end precedes the start; `rangeRefusal` says so.
 */
export function rangeBounds(fromDay: string, toDay: string): RangeBounds | null {
  const f = Date.parse(`${fromDay}T00:00:00Z`);
  const t = Date.parse(`${toDay}T00:00:00Z`);
  if (!fromDay || !toDay || Number.isNaN(f) || Number.isNaN(t)) return null;
  const toExclusive = t + DAY_MS;
  return { from: rfc3339(f), to: rfc3339(toExclusive), days: Math.round((toExclusive - f) / DAY_MS) };
}

function rfc3339(ms: number): string {
  return new Date(ms).toISOString().replace(".000Z", "Z");
}

/** "" when the two days make a request the server will accept; otherwise the sentence to show instead of asking. */
export function rangeRefusal(fromDay: string, toDay: string): string {
  const b = rangeBounds(fromDay, toDay);
  if (!b) return "Pick both dates.";
  if (b.days <= 0) return "The end must not be before the start.";
  if (b.days > MAX_RANGE_DAYS) return RANGE_TOO_WIDE_TEXT;
  return "";
}
