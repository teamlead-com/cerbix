// Pure helpers for EVERY release-gate surface (FR-024, D-0207): the service card
// (components/ServiceGate.vue) and the three ledger views (lib/gateLedger.ts re-exports the shared
// part of this module and keeps only what is ledger-specific). Everything here is a function of
// its arguments — no store, no clock unless one is passed — so the rendering rules are
// unit-testable without mounting anything, and there is ONE place for each of them:
//
//   * how a decision STATE is said (statePill, PILL_*): the five answers borrow the hues the
//     language already has — ALLOW → up, WARN → degraded, BLOCK → down, UNKNOWN and NOT_CONFIGURED
//     → pending with a DASHED ring ("deficient evidence" / "a different kind of answer"). FR-024
//     adds no colour, and status is never colour alone: dot + text, always.
//   * how a chip is drawn (CHIP_*), and how a reason's TONE follows the D4 algebra (reasonTone).
//   * how a refusal is spelled (GATE_ERROR_TEXT, describeFailure): one code→message map, the
//     Retry-After of a 429, the transport's own words for a network failure, an unknown code
//     verbatim — never swallowed.
import type { components } from "@/api/schema";

type Schemas = components["schemas"];
export type GatePolicy = Schemas["GatePolicy"];
export type GatePolicyWrite = Schemas["GatePolicyWrite"];
export type GateDecision = Schemas["GateDecision"];
export type GateReason = Schemas["GateReason"];
export type GateState = GateDecision["state"];
export type GateAction = NonNullable<GateDecision["action"]>;
export type GateWindow = GatePolicy["window"];
export type GateClause = keyof Schemas["GateClauses"];
export type ClauseAssignment = Schemas["GateClauseAssignment"];
export type UnknownBehavior = GatePolicy["unknown_behavior"];
export type ServiceSLATarget = Schemas["ServiceSLATarget"];

/** The five answers, in the order the mock's legend lists them. */
export const GATE_STATES: readonly GateState[] = ["ALLOW", "WARN", "BLOCK", "UNKNOWN", "NOT_CONFIGURED"];
/** The standard windows in the order every other service screen lists them. */
export const GATE_WINDOWS: readonly GateWindow[] = ["24h", "7d", "30d", "90d"];
/** Schema version 1's clause vocabulary (D11), in the mock's order. */
export const GATE_CLAUSES: readonly GateClause[] = [
  "budget_exhausted",
  "budget_consumed",
  "page_burn_firing",
  "ticket_burn_firing",
  "service_incident_open",
];
export const ASSIGNMENTS: readonly ClauseAssignment[] = ["block", "warn", "ignore"];
export const GATE_SCHEMA_VERSION = 1;
/** D8a: bucket 60 s + grace 120 s + two buckets of headroom; the maximum is a day. */
export const MIN_SEAL_LAG_SECONDS = 300;
export const MAX_SEAL_LAG_SECONDS = 86_400;
/** D8a / D-0190: the owner's default bound. */
export const DEFAULT_SEAL_LAG_SECONDS = 900;
/** D3: the default `budget_consumed` threshold. */
export const DEFAULT_BUDGET_CONSUMED_PERCENT = 90;
/** The template's preferred window when the service has a target for it (the mock's example). */
export const PREFERRED_WINDOW: GateWindow = "30d";
/** D9: an override expires at most seven days ahead — a hard maximum, no default. */
export const OVERRIDE_MAX_DAYS = 7;
/** The card reads the newest ledger row over this many days (D-0207 item 4). */
export const LEDGER_LOOKBACK_DAYS = 30;
/** §5: one ledger page may span at most 31 days. */
export const LEDGER_MAX_RANGE_DAYS = 31;

// ── Pills ───────────────────────────────────────────────────────────────────────────────────────

export const PILL_BASE = "inline-flex h-6 items-center gap-[6px] rounded-full pl-[7px] pr-[9px] text-[12px] font-semibold tracking-[0.01em]";
export const PILL_DOT = "inline-block h-[7px] w-[7px] shrink-0 rounded-full";

export interface StatePill {
  label: string;
  /** Tailwind classes of the pill itself (token-mapped; see tailwind.config.js). */
  cls: string;
  /** Tailwind classes of the dot. */
  dot: string;
  /** UNKNOWN and NOT_CONFIGURED carry the dashed ring. */
  dashed: boolean;
}

const PILLS: Record<GateState, StatePill> = {
  ALLOW: { label: "ALLOW", cls: "bg-up-weak text-up", dot: "bg-up", dashed: false },
  WARN: { label: "WARN", cls: "bg-degraded-weak text-degraded", dot: "bg-degraded", dashed: false },
  BLOCK: { label: "BLOCK", cls: "bg-down-weak text-down", dot: "bg-down", dashed: false },
  UNKNOWN: {
    label: "UNKNOWN",
    cls: "border border-dashed border-border-strong bg-pending-weak text-ink-2",
    dot: "bg-pending",
    dashed: true,
  },
  NOT_CONFIGURED: {
    label: "not configured",
    cls: "border border-dashed border-border-strong bg-inset text-ink-3",
    dot: "border-[1.5px] border-dashed border-pending bg-transparent",
    dashed: true,
  },
};

/** The pill for a state; an unfamiliar value (a newer server) renders as deficient evidence, never as ALLOW. */
export function statePill(state: string): StatePill {
  return PILLS[state as GateState] ?? { ...PILLS.UNKNOWN, label: state };
}

/** The empty latest-decision card: NOT_CONFIGURED's grammar with its own words. */
export const NO_DECISION_PILL: StatePill = { ...PILLS.NOT_CONFIGURED, label: "no decision yet" };

/** The five answers, for the card's legend (mock screen 6). */
export const LEGEND: readonly { state: GateState; text: string }[] = [
  { state: "ALLOW", text: "Nothing matched. exit 0." },
  { state: "WARN", text: "A warn clause matched; nothing blocked. Printed in full, exit 0." },
  { state: "BLOCK", text: "A block clause matched — known, not guessed. exit 2 unless an override is in force." },
  { state: "UNKNOWN", text: "A block/warn clause's fact is unavailable. Action per unknown_behavior. Dashed: deficient evidence." },
  { state: "NOT_CONFIGURED", text: "No policy. Nothing allowed, nothing blocked, exit 4. Dashed: a different kind of answer." },
];

/** The CLI's exit code for an action (D16). */
export function exitCode(action: GateAction | undefined): 0 | 2 | 4 {
  if (!action) return 4;
  return action === "BLOCK" ? 2 : 0;
}

// ── Chips ───────────────────────────────────────────────────────────────────────────────────────

export const CHIP_BASE = "inline-flex items-center gap-[5px] rounded-[5px] border px-[7px] py-[2px] text-[11.5px] leading-[1.4]";
export const CHIP_PLAIN = "border-border bg-surface text-ink-2";
/** An operator's ACT in force — the "declared here" accent, never a status hue. */
export const CHIP_ACC = "border-accent bg-accent-weak text-accent";
/** Dormant: closed, unavailable, gone. Dashed, muted. */
export const CHIP_DORM = "border-dashed border-border bg-surface text-ink-3";
export const CHIP_WARN = "border-degraded bg-degraded-weak text-degraded";
export const CHIP_DOWN = "border-down bg-down-weak text-down";
/** The mock's `.chip.file`: a file-managed marker. */
export const CHIP_FILE = "border-border bg-inset text-ink-2";

// ── Clauses ─────────────────────────────────────────────────────────────────────────────────────

/** The mock's one-line `.sub` description of each release-risk fact. */
export const CLAUSE_DESCRIPTIONS: Record<GateClause, string> = {
  budget_exhausted: "the window's error budget is spent — burned ≥ 100 %",
  budget_consumed: "burned ≥ the threshold · 1..100, an integer",
  page_burn_firing: "a page-severity burn rule of this window is firing right now",
  ticket_burn_firing: "a ticket-severity burn rule of this window is firing",
  service_incident_open:
    "an unresolved incident opened automatically on this service — warns by default: the deploy is often the fix",
};

export const UNKNOWN_BEHAVIOR_LABELS: Record<UnknownBehavior, string> = {
  warn: "warn — proceed, say why",
  block: "block — hold the release",
};

/** The segmented control's "on" classes per assignment; `ignore` is outlined only — not a state. */
export function assignmentClass(a: ClauseAssignment, on: boolean): string {
  if (!on) return "text-ink-3";
  switch (a) {
    case "block":
      return "bg-down-weak text-down font-semibold";
    case "warn":
      return "bg-degraded-weak text-degraded font-semibold";
    default:
      return "bg-inset text-ink-2 font-semibold";
  }
}

// ── The policy draft ────────────────────────────────────────────────────────────────────────────

/**
 * What the editor holds. The two numeric fields are `string | number`: a `v-model` on an
 * `<input type="number">` hands back a Number for a parseable value and "" for an empty one, and
 * a half-typed value has to be representable either way.
 */
export interface PolicyDraft {
  window: GateWindow | "";
  clauses: Record<GateClause, ClauseAssignment>;
  threshold: string | number;
  sealLagMinutes: string | number;
  unknown_behavior: UnknownBehavior;
}

/** The windows of a service's target inventory, in the order the server lists them (canonical). */
export function windowsOf(targets: readonly ServiceSLATarget[]): GateWindow[] {
  return targets.map((t) => t.window);
}

/** The template's window: 30d when it has a target (the mock's example), else the first with one. */
export function templateWindow(windowsWithTarget: readonly GateWindow[]): GateWindow | "" {
  if (windowsWithTarget.includes(PREFERRED_WINDOW)) return PREFERRED_WINDOW;
  return GATE_WINDOWS.find((w) => windowsWithTarget.includes(w)) ?? "";
}

/**
 * The UI's create template (D11/D14): the mock's shipped defaults. The window is one the service
 * has a target for — never one it has not, because the server refuses that write.
 */
export function createTemplate(windowsWithTarget: readonly GateWindow[]): PolicyDraft {
  return {
    window: templateWindow(windowsWithTarget),
    clauses: {
      budget_exhausted: "block",
      budget_consumed: "warn",
      page_burn_firing: "block",
      ticket_burn_firing: "warn",
      service_incident_open: "warn",
    },
    threshold: String(DEFAULT_BUDGET_CONSUMED_PERCENT),
    sealLagMinutes: String(DEFAULT_SEAL_LAG_SECONDS / 60),
    unknown_behavior: "warn",
  };
}

export function draftFromPolicy(p: GatePolicy): PolicyDraft {
  return {
    window: p.window,
    clauses: { ...p.clauses },
    threshold: String(p.budget_consumed_percent),
    sealLagMinutes: String(p.max_seal_lag_seconds / 60),
    unknown_behavior: p.unknown_behavior,
  };
}

/** The WHOLE document (D11): every field explicit, the server fills nothing in. */
export function draftToBody(d: PolicyDraft, expectedRevision: number | null): GatePolicyWrite {
  return {
    expected_revision: expectedRevision,
    schema_version: GATE_SCHEMA_VERSION,
    window: d.window as GateWindow,
    clauses: { ...d.clauses },
    budget_consumed_percent: Number(d.threshold),
    max_seal_lag_seconds: Number(d.sealLagMinutes) * 60,
    unknown_behavior: d.unknown_behavior,
  };
}

// ── Client validation, mirroring the server's rules ─────────────────────────────────────────────

export type DraftField = "threshold" | "seal-lag" | "window" | "clauses";

export const SEAL_LAG_MIN_MESSAGE =
  "Minimum is 5 minutes (300 s): bucket 60 s + grace 120 s + two buckets of headroom.";
export const SEAL_LAG_MAX_MESSAGE = "Maximum is 24 hours (1440 minutes).";
export const SEAL_LAG_WHOLE_MESSAGE = "Whole minutes only.";
export const THRESHOLD_MESSAGE = "Must be a whole number from 1 to 100.";
export const WINDOW_MESSAGE = "Pick a window this service has a target for.";
export const WINDOW_LOST_MESSAGE = "This window no longer has a target — pick one that does before saving.";
export const CLAUSES_MESSAGE = "Every fact must be assigned block, warn or ignore.";

function wholeNumber(raw: string | number): number | null {
  const t = String(raw ?? "").trim();
  if (!/^-?\d+$/.test(t)) return null;
  return Number(t);
}

/** "" when valid; otherwise the message the mock shows. */
export function validateSealLagMinutes(raw: string | number): string {
  const n = wholeNumber(raw);
  if (n === null) return String(raw ?? "").trim() === "" ? SEAL_LAG_MIN_MESSAGE : SEAL_LAG_WHOLE_MESSAGE;
  if (n * 60 < MIN_SEAL_LAG_SECONDS) return SEAL_LAG_MIN_MESSAGE;
  if (n * 60 > MAX_SEAL_LAG_SECONDS) return SEAL_LAG_MAX_MESSAGE;
  return "";
}

export function validateThreshold(raw: string | number): string {
  const n = wholeNumber(raw);
  if (n === null || n < 1 || n > 100) return THRESHOLD_MESSAGE;
  return "";
}

/**
 * The window must be one the service has a target for. A stored window whose target has since
 * disappeared is a DIFFERENT message: the operator has to know why a saved policy no longer saves.
 */
export function validateWindow(
  window: string,
  windowsWithTarget: readonly GateWindow[],
  storedWindow?: GateWindow | "",
): string {
  if (!window) return WINDOW_MESSAGE;
  if (windowsWithTarget.includes(window as GateWindow)) return "";
  return storedWindow && window === storedWindow ? WINDOW_LOST_MESSAGE : WINDOW_MESSAGE;
}

export function validateClauses(clauses: Partial<Record<GateClause, ClauseAssignment>>): string {
  for (const c of GATE_CLAUSES) {
    const a = clauses[c];
    if (!a || !ASSIGNMENTS.includes(a)) return CLAUSES_MESSAGE;
  }
  return "";
}

export function validateDraft(
  d: PolicyDraft,
  windowsWithTarget: readonly GateWindow[],
  storedWindow?: GateWindow | "",
): Partial<Record<DraftField, string>> {
  const out: Partial<Record<DraftField, string>> = {};
  const w = validateWindow(d.window, windowsWithTarget, storedWindow);
  if (w) out.window = w;
  const c = validateClauses(d.clauses);
  if (c) out.clauses = c;
  const t = validateThreshold(d.threshold);
  if (t) out.threshold = t;
  const s = validateSealLagMinutes(d.sealLagMinutes);
  if (s) out["seal-lag"] = s;
  return out;
}

// ── Refusals: ONE code→message map, ONE reader, ONE sentence ────────────────────────────────────

/** The mock's sentence for a range the server would refuse; the client says it BEFORE asking. */
export const RANGE_TOO_WIDE_TEXT =
  "Pick at most 31 days at a time. The ledger is read one day-partition at a time, and a month is the most one page may span.";

/** The one code→message map of D-0207 item 5; an unknown code is shown verbatim, never hidden. */
export const GATE_ERROR_TEXT: Record<string, string> = {
  // ledger reads
  range_too_wide: RANGE_TOO_WIDE_TEXT,
  range_required: "Pick both dates.",
  range_invalid: "The end must be after the start.",
  limit_invalid: "The page size was refused.",
  cursor_invalid: "This page marker is no longer valid — apply the filters again to start from the newest decision.",
  process_inflight: "The ledger is busy right now.",
  principal_inflight: "You already have as many ledger reads in flight as one principal may.",
  // policy and override
  not_configured: "No gate policy is configured for this service.",
  none_active: "No override is active.",
  revision_conflict:
    "This policy changed while you were editing it. Your changes were not applied. Reload to see the current policy, then re-apply what you still want.",
  override_active:
    "One override at a time. An active override exists — revoke it first; creating another is refused.",
  override_not_active:
    "This override is no longer active. Nothing was changed — whatever is active now was created after this screen loaded. Reload to see it.",
  expected_revision_required: "The request did not say which revision it expected. Reload and try again.",
  expected_revision_invalid: "The revision sent with the request was not a whole number. Reload and try again.",
};

/** Where a refusal is being read; a few codes have a sentence of their own there (mock screen 2/3). */
export type FailureContext = "read" | "save" | "delete" | "override" | "revoke" | "ledger";

const CONTEXT_TEXT: Partial<Record<FailureContext, Record<string, string>>> = {
  delete: {
    revision_conflict:
      "The policy changed while this dialog was open. Nothing was deleted. Reload, read what changed, then decide again.",
    not_configured: "There is no policy to delete any more — somebody deleted it first. Reload to see the current state.",
  },
  override: {
    revision_conflict:
      "The policy changed since this screen loaded, so an override bound to the revision you saw cannot be created. Reload to see the current policy, then decide again.",
  },
};

export interface GateFailure {
  /** 0 when there was no HTTP response (the transport failed). */
  status: number;
  /** The `{error}` body, a code or a sentence; "" when absent. */
  code: string;
  /** `Retry-After` in whole seconds, when the server sent one (429). */
  retryAfter: number | null;
  /** The transport's own words when `status` is 0 (see `transportFailure`); absent for an HTTP refusal. */
  message?: string;
}

/**
 * Read an openapi-fetch result that carried `error`. Tolerates a missing `response` (test doubles)
 * and a `Retry-After` given as an HTTP-date rather than seconds.
 */
export function failureOf(res: {
  error?: unknown;
  response?: { status?: number; headers?: { get?: (name: string) => string | null } } | Response;
}): GateFailure {
  const code = (res.error as { error?: unknown } | undefined)?.error;
  const ra = res.response?.headers?.get?.("Retry-After");
  return {
    status: res.response?.status ?? 0,
    code: typeof code === "string" ? code : "",
    retryAfter: parseRetryAfter(ra),
  };
}

export function parseRetryAfter(v: string | null | undefined): number | null {
  if (v == null || v === "") return null;
  const n = Number(v);
  if (Number.isFinite(n) && n >= 0) return Math.ceil(n);
  const at = Date.parse(v);
  if (Number.isNaN(at)) return null;
  return Math.max(1, Math.ceil((at - Date.now()) / 1000));
}

/** A fetch aborted by our own controller — a stale read, not a failure to show. */
export function isAbort(e: unknown): boolean {
  return (e instanceof DOMException && e.name === "AbortError") || (e instanceof Error && e.name === "AbortError");
}

/** The transport's own words, verbatim — a network failure is never paraphrased into a guess. */
export function transportFailure(e: unknown): string {
  const msg = e instanceof Error ? e.message : String(e);
  return `Could not reach the server: ${msg || "network error"}`;
}

/** A thrown transport error as a failure the same reader can spell. */
export function transportFailureOf(e: unknown): GateFailure {
  return { status: 0, code: "", retryAfter: null, message: transportFailure(e) };
}

/** The 409 codes after which every mutation is blocked until an explicit Reload. */
export const CONFLICT_CODES: readonly string[] = ["revision_conflict", "override_active", "override_not_active"];

export function isConflict(f: GateFailure): boolean {
  return f.status === 409 || CONFLICT_CODES.includes(f.code);
}

/**
 * One sentence for a refusal. 401/403 are one line (the caller shows no controls); a context's own
 * sentence for its codes comes next (the delete dialog's 409 is not the editor's 409); 404 is the
 * caller's own sentence, because "does not exist" means a different thing for a project, a decision
 * and a service — or the code's, when the caller gave none; 429 carries `Retry-After` when present;
 * a 503 and every other code is its code's sentence, or the body verbatim when the code is
 * unfamiliar; a transport failure is the transport's own words.
 */
export function describeFailure(
  f: GateFailure,
  opts: { notFound?: string; denied?: string; context?: FailureContext; fallback?: string } = {},
): string {
  if (f.status === 401) return "Your session has ended — sign in again.";
  if (f.status === 403) return opts.denied ?? "You cannot see this.";
  const local = opts.context ? CONTEXT_TEXT[opts.context]?.[f.code] : undefined;
  if (local) return local;
  if (f.status === 404) return opts.notFound ?? GATE_ERROR_TEXT[f.code] ?? (f.code || "Not found.");
  if (f.status === 429) {
    const why = GATE_ERROR_TEXT[f.code] ?? "Too many requests at once.";
    return f.retryAfter != null ? `${why} Try again in ${f.retryAfter} s.` : `${why} Try again shortly.`;
  }
  if (f.code) return GATE_ERROR_TEXT[f.code] ?? f.code;
  if (!f.status && f.message) return f.message;
  return f.status ? `The server answered HTTP ${f.status}.` : (opts.fallback ?? "The request failed.");
}

// ── Formatting ──────────────────────────────────────────────────────────────────────────────────

/** `0191c2a4…5b04` — the mock's shortening; the full id always rides in a `title`. */
export function shortId(id: string): string {
  return id.length > 13 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id;
}

/** The snapshot instant with its milliseconds: "2026-08-28 14:03:02.417Z". */
export function preciseLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace("T", " ");
}

/** A compact duration: "3m 02s", "15m", "22m 10s", "1h 05m", "5h 49m", "24h", "2d 03h". */
export function durationShort(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const pad = (n: number) => String(n).padStart(2, "0");
  const d = Math.floor(s / 86_400);
  const h = Math.floor((s % 86_400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d > 0) return h ? `${d}d ${pad(h)}h` : `${d}d`;
  if (h > 0) return m ? `${h}h ${pad(m)}m` : `${h}h`;
  if (m > 0) return sec ? `${m}m ${pad(sec)}s` : `${m}m`;
  return `${sec}s`;
}

/** "in 5h 49m", or "already past" once the instant is behind the clock. */
export function untilLabel(iso: string, now: Date = new Date()): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "";
  const diff = t - now.getTime();
  if (diff <= 0) return "already past";
  return `in ${durationShort(diff / 1000)}`;
}

export function isPast(iso: string, now: Date = new Date()): boolean {
  const t = new Date(iso).getTime();
  return !Number.isNaN(t) && t <= now.getTime();
}

/** "99.90 %" — a space before the sign, as every mock of this product writes it. */
export function fmtPercent(v: number, digits = 2): string {
  return `${v.toFixed(digits)} %`;
}

/** The date part only: "2026-08-01". */
export function dateLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().slice(0, 10);
}

// ── Reasons ─────────────────────────────────────────────────────────────────────────────────────

export type ReasonKind = "matched" | "unavailable" | "whole";

/**
 * D4/D7: a clause that MATCHED has `code === clause` and carries `value`; a clause that was
 * UNAVAILABLE has `clause` and a `code` naming the unavailability; an entry with no clause is
 * about the whole decision (`not_configured`, `no_governing_revision`, `never_evaluated`).
 */
export function reasonKind(r: GateReason): ReasonKind {
  if (!r.clause) return "whole";
  return r.code === r.clause ? "matched" : "unavailable";
}

export type ReasonTone = "down" | "warn" | "dorm";

/**
 * The ONE tone rule for a reason, from the D4 algebra: a clause that MATCHED under `block` reads
 * down, under `warn` reads degraded; everything else — an unavailability (whatever its clause was
 * assigned), an ignored match, `not_configured`, an unfamiliar shape — is dormant (dashed), because
 * it decided nothing on its own.
 */
export function reasonTone(r: GateReason): ReasonTone {
  if (reasonKind(r) !== "matched") return "dorm";
  if (r.assignment === "block") return "down";
  if (r.assignment === "warn") return "warn";
  return "dorm";
}

export function reasonToneClass(r: GateReason): string {
  const t = reasonTone(r);
  return t === "down" ? CHIP_DOWN : t === "warn" ? CHIP_WARN : CHIP_DORM;
}

export function isBudgetClause(c: string | undefined): boolean {
  return c === "budget_exhausted" || c === "budget_consumed";
}

/** The chip in front of a card's reason row: the assignment when matched, "unavailable" otherwise. */
export function reasonChip(r: GateReason): { label: string; cls: string } {
  const kind = reasonKind(r);
  const cls = reasonToneClass(r);
  if (kind === "matched") return { label: r.assignment === "ignore" ? "ignored" : (r.assignment ?? "matched"), cls };
  return { label: kind === "unavailable" ? "unavailable" : r.code, cls };
}

/**
 * The value a clause was judged on, rendered by KIND: a number is the burned percent of a budget
 * clause (the decision quotes no threshold, so it is "96.4 % burned", not "≥ 90 %"); a string is
 * a burn rule key or an incident id and is shown as is; an unavailable clause shows its assignment.
 */
export function reasonValueLabel(r: GateReason): string {
  const kind = reasonKind(r);
  if (kind === "unavailable") return r.assignment ? `assigned ${r.assignment}` : "";
  if (kind === "whole") return "";
  if (typeof r.value === "number") {
    return isBudgetClause(r.clause) ? `${fmtPercent(r.value, 1)} burned` : String(r.value);
  }
  if (typeof r.value === "string") return r.value;
  if (r.value === true) return "matched";
  if (r.value == null) return "";
  return JSON.stringify(r.value);
}

/** An incident id the reasons can link to, when the incident clause matched. */
export function reasonIncidentId(r: GateReason): string {
  return r.clause === "service_incident_open" && reasonKind(r) === "matched" && typeof r.value === "string"
    ? r.value
    : "";
}

/**
 * D-0207 item 4: the budget KPI is rendered from the value THE DECISION quoted — a budget clause's
 * numeric `value` — and from nothing else. Returns the entry so the caller can colour by its
 * assignment; `percent` is undefined when the clause was unavailable (withheld).
 */
export function budgetOfDecision(reasons: readonly GateReason[]): { reason: GateReason; percent?: number } | null {
  const numeric = reasons.find((r) => isBudgetClause(r.clause) && typeof r.value === "number");
  if (numeric) return { reason: numeric, percent: numeric.value as number };
  const withheld = reasons.find((r) => isBudgetClause(r.clause));
  return withheld ? { reason: withheld } : null;
}

/** The one unavailability that explains every budget clause at once (mock screen 6). */
export function sealStaleReason(reasons: readonly GateReason[]): GateReason | undefined {
  return reasons.find((r) => r.code === "seal_stale");
}

// ── The ledger read ─────────────────────────────────────────────────────────────────────────────

/** An EXPLICIT half-open RFC3339 range, `from` inclusive, `to` exclusive, never wider than §5 allows. */
export function ledgerRange(now: Date = new Date(), days = LEDGER_LOOKBACK_DAYS): { from: string; to: string } {
  const span = Math.min(days, LEDGER_MAX_RANGE_DAYS);
  const to = new Date(now.getTime());
  const from = new Date(to.getTime() - span * 86_400_000);
  return { from: from.toISOString(), to: to.toISOString() };
}

// ── The CLI ─────────────────────────────────────────────────────────────────────────────────────

/**
 * The exact command for THIS project and service, by canonical id. The token is the literal
 * placeholder `…` — never a value, never anything read from the session or from storage.
 */
export function cliCommand(origin: string, projectId: string, serviceId: string): string {
  return `CERBIX_URL=${origin} CERBIX_TOKEN=… cerbix gate check --project ${projectId} --service ${serviceId}`;
}

export const CLI_EXITS: readonly { code: string; text: string }[] = [
  { code: "exit 0", text: "ALLOW or WARN — the step may proceed; WARN is printed in full" },
  { code: "exit 2", text: "BLOCK — the step must not proceed" },
  { code: "exit 4", text: "NOT_CONFIGURED — nobody has said what matters here" },
  { code: "exit 1", text: "transport — auth, timeout, 429 (Retry-After printed), 503 — never a decision" },
];

// ── The override form ───────────────────────────────────────────────────────────────────────────

/** A `datetime-local` value in the viewer's zone, minute precision. */
export function toDatetimeLocal(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function defaultOverrideUntil(now: Date = new Date()): string {
  return toDatetimeLocal(new Date(now.getTime() + 24 * 3600_000));
}

export function maxOverrideUntil(now: Date = new Date()): string {
  return toDatetimeLocal(new Date(now.getTime() + OVERRIDE_MAX_DAYS * 86_400_000));
}

export const OVERRIDE_REASON_MAX = 500;

export function validateOverride(
  reason: string,
  untilLocal: string,
  now: Date = new Date(),
): { reason?: string; until?: string } {
  const out: { reason?: string; until?: string } = {};
  const r = reason.trim();
  if (!r) out.reason = "A reason is required. It is recorded on every decision the override changes and in the audit log.";
  else if (r.length > OVERRIDE_REASON_MAX) out.reason = `At most ${OVERRIDE_REASON_MAX} characters.`;
  const t = untilLocal ? new Date(untilLocal).getTime() : NaN;
  if (Number.isNaN(t)) out.until = "Enter when the override ends.";
  else if (t <= now.getTime()) out.until = "Pick a time in the future.";
  else if (t - now.getTime() > OVERRIDE_MAX_DAYS * 86_400_000) out.until = `At most ${OVERRIDE_MAX_DAYS} days ahead.`;
  return out;
}
