// FR-025 (D-0210 items 4, 5): what ONLY the per-service timeline (views/ServiceChangesView.vue) and
// the incident page's "Preceded by" (views/IncidentDetailView.vue) need.
//
// Every rule the change surfaces SHARE — a kind by shape and text (`kindClip`, `kindLabel`), the
// phases' domain order (`isTerminal`, `terminalOf`, `groupLatest`, `phaseTone`), the identity key
// (`groupKey`), the decision link (`decisionView`), the horizons, how a comparison side and its delta
// are quoted (`formatCompareSide`, `deltaChip`, `deltaValueClass`), a lag (`lagText`), a clock
// (`clockLabel`), the refusal map (`CHANGE_ERROR_TEXT`, `changeCode`, `describeChangeFailure`), the
// 92-day bound and its sentence — lives ONCE in lib/changes.ts (the owner, iter-0165 task 7 part 1)
// and is RE-EXPORTED here so the two views keep one import; nothing below restates it. What stays is
// the timeline's own: its calendar-day range (the ledger's date pickers under D6's bound, not the
// card's RFC3339 "ending now" range), the route-query kind set, the source slug grammar the filter
// refuses before asking, the phase strip's instant rule (mock screen 5), the link role's words, and
// the incident-side types. The token allow-list helpers are lib/tokenActions.ts.

import type { components } from "@/api/schema";

import { CHANGE_KINDS, CHANGE_RANGE_MAX_DAYS, type ChangeKind, RANGE_TOO_WIDE_TEXT } from "@/lib/changes";
import { defaultRange as ledgerDefaultRange, rangeRefusal } from "@/lib/gateLedger";

export {
  CHANGE_ERROR_TEXT,
  CHANGE_KINDS,
  CHANGE_RANGE_MAX_DAYS,
  COMPARE_POOL,
  DEFAULT_HORIZON,
  DEFAULT_RANGE_DAYS,
  HORIZONS,
  NO_TERMINAL_TEXT,
  PHASE_ORDER,
  RANGE_TOO_WIDE_TEXT,
  changeCode,
  clockLabel,
  decisionView,
  deltaChip,
  deltaValueClass,
  describeChangeFailure,
  formatCompareSide,
  groupKey,
  groupLatest,
  horizonLabel,
  inPool,
  isHorizon,
  isTerminal,
  kindClip,
  kindLabel,
  lagText,
  phaseLabel,
  phaseTone,
  requestScope,
  terminalOf,
  type ChangeCompare,
  type ChangeCompareSide,
  type ChangeDecisionLink,
  type ChangeGroup,
  type ChangeHorizon,
  type ChangeIncidentLink,
  type ChangeKind,
  type ChangePhase,
  type ChangePhaseName,
  type CompareSideView,
  type DecisionView,
} from "@/lib/changes";
export { CHIP_ACC, CHIP_BASE, CHIP_DORM, CHIP_PLAIN, PILL_BASE, PILL_DOT, failureOf, isAbort, shortId, statePill, transportFailure } from "@/lib/gate";
export { PAGE_SIZE, rangeBounds } from "@/lib/gateLedger";

type Schemas = components["schemas"];
/** One link from the incident side (D7): the anchored phase with identity, role, copied lag, and the live phases. */
export type IncidentChange = Schemas["IncidentChange"];
export type ChangeLinkRole = Schemas["ChangeIncidentLink"]["role"];

// ── The phase strip (mock screen 5) ──────────────────────────────────────────────────────────

/**
 * A phase's instant as the timeline's phase strip writes it: `08-28 16:40` for the first phase (and
 * for a phase on a later UTC day than the one before it), `15:25` when the day is the previous
 * phase's. This is relative to the PREVIOUS phase, not to now — the card's `instantLabel` answers a
 * different question ("how long ago"), so the two are not one rule.
 */
export function phaseInstantLabel(iso: string, previousIso?: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const s = d.toISOString();
  const clock = s.slice(11, 16);
  if (previousIso) {
    const prev = new Date(previousIso);
    if (!Number.isNaN(prev.getTime()) && prev.toISOString().slice(0, 10) === s.slice(0, 10)) return clock;
  }
  return `${s.slice(5, 10)} ${clock}`;
}

// ── The incident side (D7) ───────────────────────────────────────────────────────────────────

/** The link's role as the mock writes it: `own service`, or `upstream · probable root`. */
export function roleLabel(role: ChangeLinkRole | string): string {
  switch (role) {
    case "own_service":
      return "own service";
    case "upstream":
      return "upstream · probable root";
    default:
      return String(role);
  }
}

// ── The timeline's range and filters (D6) ────────────────────────────────────────────────────

/**
 * "" when the two calendar days make a request the timeline will accept; otherwise the sentence to
 * show instead of asking. The ledger's helper (lib/gateLedger.ts) with D6's bound and the mock's
 * sentence from lib/changes.ts — the gate's 31-day behaviour does not move.
 */
export function changeRangeRefusal(fromDay: string, toDay: string): string {
  return rangeRefusal(fromDay, toDay, CHANGE_RANGE_MAX_DAYS, RANGE_TOO_WIDE_TEXT);
}

/**
 * Today (UTC) and the 29 days before it as CALENDAR DAYS (`YYYY-MM-DD`) for the two date pickers —
 * the ledger's default; `rangeBounds` turns them into the half-open request. The card's
 * `defaultRange` (lib/changes.ts) is the RFC3339 range ending now, a different shape for a
 * different control.
 */
export function defaultChangeRange(now: Date = new Date()): { from: string; to: string } {
  return ledgerDefaultRange(now);
}

/** A route-query `kind` — one value, a comma list or a repeated key — narrowed to the closed set, deduplicated, in the mock's order. */
export function kindsFromQuery(v: unknown): ChangeKind[] {
  const raw = Array.isArray(v) ? v : typeof v === "string" ? v.split(",") : [];
  const picked = new Set(raw.map((x) => String(x).trim()).filter((x): x is ChangeKind => (CHANGE_KINDS as readonly string[]).includes(x)));
  return CHANGE_KINDS.filter((k) => picked.has(k));
}

/** The source slug grammar of D2 as the server checks a `source` filter (400 `source_invalid` otherwise); refused here before asking. */
export const SOURCE_SLUG = /^[a-z0-9][a-z0-9-]{0,63}$/;
