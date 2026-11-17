// Shared presentation helpers for incident status and impact — a single source
// of labels and token-driven colors used across the incident views.
import type { components } from "@/api/schema";

export type IncidentStatus = "investigating" | "identified" | "monitoring" | "resolved";
export type IncidentImpact = "none" | "minor" | "major" | "critical";

interface Badge {
  label: string;
  cls: string;
}

const STATUS: Record<IncidentStatus, Badge> = {
  investigating: { label: "Investigating", cls: "text-degraded bg-degraded-weak" },
  identified: { label: "Identified", cls: "text-maint bg-maint-weak" },
  monitoring: { label: "Monitoring", cls: "text-accent bg-accent-weak" },
  resolved: { label: "Resolved", cls: "text-up bg-up-weak" },
};

// FR-042 / NFR-035 (I13): `none` is an ordinary value of automatic incidents — a service that pages on
// `unknown` opens at it by default — so it is spelled in words. "Not specified" is the LABEL, for where
// impact stands alone (badge, pill, option); `impactPhrase` below is the PHRASE, for where it is named.
const IMPACT: Record<IncidentImpact, Badge> = {
  none: { label: "Not specified", cls: "text-ink-3 bg-surface-2" },
  minor: { label: "Minor", cls: "text-maint bg-maint-weak" },
  major: { label: "Major", cls: "text-degraded bg-degraded-weak" },
  critical: { label: "Critical", cls: "text-down bg-down-weak" },
};

export function statusBadge(s?: string): Badge {
  return STATUS[(s as IncidentStatus)] ?? { label: s ?? "—", cls: "text-ink-3 bg-surface-2" };
}

export function impactBadge(i?: string): Badge {
  return IMPACT[(i as IncidentImpact)] ?? { label: i ?? "—", cls: "text-ink-3 bg-surface-2" };
}

/**
 * Impact NAMED as a phrase: "Major impact", and "Impact not specified" for none — the word order flips
 * for none only, because "Not specified impact" is not English. Every view that names impact goes
 * through here (or lower-cases it, as the past-incident tail does); composing "<label> impact" by hand
 * is exactly what used to print "None impact" (FR-042 I13, mock §4).
 */
export function impactPhrase(i?: string): string {
  if (i === "none") return "Impact not specified";
  return `${impactBadge(i).label} impact`;
}

/** The impact as a lower-case display word, for prose that lists impacts ("down → not specified"). */
export function impactWord(i?: string): string {
  return impactBadge(i).label.toLowerCase();
}

// The forward lifecycle order, for status selects.
export const STATUS_ORDER: IncidentStatus[] = ["investigating", "identified", "monitoring", "resolved"];

// The lifecycle only moves forward, and the server enforces it: an update that would move an incident
// backward is refused. Offering a backward step therefore produces a button that can only fail, which
// is worse than not offering it — the operator reads the refusal as a bug rather than as a rule.
// `resolved` is deliberately excluded here: resolving is its own action with its own confirmation,
// not one more rung on this picker.
export function forwardStatuses(current?: IncidentStatus | string): IncidentStatus[] {
  const from = Math.max(0, STATUS_ORDER.indexOf((current ?? "investigating") as IncidentStatus));
  return STATUS_ORDER.slice(from).filter((s) => s !== "resolved");
}
export const IMPACT_ORDER: IncidentImpact[] = ["none", "minor", "major", "critical"];

/** Severity rank, none < minor < major < critical; -1 for a value this table does not know. */
export function impactRank(i?: string): number {
  return IMPACT_ORDER.indexOf(i as IncidentImpact);
}

// ── FR-042: the basis of an incident's impact (spec §4.6, mock §3) ─────────────────────────────

type ImpactBasis = components["schemas"]["ImpactBasis"];
type ImpactByState = components["schemas"]["ImpactByState"];

/**
 * "mapping frozen at open: down → major · degraded → minor · unknown → not specified". The states run
 * in the order of the alerting editor's "Pages for" row (mock D-2), not the API's key order, and the
 * impacts are display words, so even this mono line never prints a bare `none` (D-11, I13).
 */
export function frozenMappingLine(m: ImpactByState): string {
  const pairs = (["down", "degraded", "unknown"] as const).map((s) => `${s} → ${impactWord(m[s])}`);
  return `mapping frozen at open: ${pairs.join(" · ")}`;
}

/** Which of the six forms of the incident page's basis line applies (spec §4.6). */
export type ImpactBasisForm =
  | { kind: "service_mapping"; serviceName: string; state: string; mapping: string }
  | { kind: "project_default" | "manual" | "alertmanager" | "not_recorded" | "unavailable" };

/**
 * The detail answers in three encodings that must stay apart (§4.6): an object (recorded), `null`
 * (never recorded — an incident older than the feature) and `null` with `impact_basis_unavailable`
 * (the read failed). The flag is checked FIRST: a failed read must never borrow the honest "not
 * recorded", any more than a failed impact-links read may borrow "no links".
 */
export function impactBasisForm(basis: ImpactBasis | null | undefined, unavailable: boolean | undefined): ImpactBasisForm {
  if (unavailable) return { kind: "unavailable" };
  if (!basis) return { kind: "not_recorded" };
  switch (basis.source) {
    case "service_mapping":
      return {
        kind: "service_mapping",
        serviceName: basis.service.name,
        state: basis.state,
        mapping: frozenMappingLine(basis.mapping),
      };
    case "project_default":
    case "manual":
    case "alertmanager":
      return { kind: basis.source };
    default:
      // A source this build does not know (the SPA ships in the same binary as the API, so this is
      // unreachable today). It is something the page cannot read, so it says that — never a guess.
      return { kind: "unavailable" };
  }
}

export function relTime(ts?: string): string {
  if (!ts) return "—";
  const diff = Date.now() - new Date(ts).getTime();
  const s = Math.max(0, Math.round(diff / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

// Human-readable incident duration (started → resolved); "" when either end is missing or the
// span is not positive. Shared by the status page's past incidents and the incident history.
export function incidentDuration(startISO?: string | null, endISO?: string | null): string {
  if (!startISO || !endISO) return "";
  const ms = new Date(endISO).getTime() - new Date(startISO).getTime();
  if (!(ms > 0)) return "";
  const mins = Math.round(ms / 60000);
  if (mins < 60) return `${mins} min`;
  const h = Math.floor(mins / 60);
  const rm = mins % 60;
  if (h < 24) return rm ? `${h}h ${rm}m` : `${h}h`;
  const d = Math.floor(h / 24);
  const rh = h % 24;
  return rh ? `${d}d ${rh}h` : `${d}d`;
}

// ── System-authored notes in an incident's timeline ──────────────────────────────────────────

/**
 * The prefixes of the notes cerbix itself writes into `incident_updates` (the `internal/domain`
 * markers): `⚡ Context:` and `⏸ Suppressed:` (func-incident-context), `🕸 Impact:`
 * (func-service-reliability §14.4), from FR-025 D7 `🚀 Changes:` and, from FR-042, `⬆ Impact raised:`
 * — a rise of the incident's impact, NOT the dependency graph's `🕸 Impact:` note, which keeps the key
 * `impact`. A rise has no gutter glyph of its own (mock D-17). A note is detected by its
 * PREFIX — the marker is also the server's idempotency guard on redelivery — and every one renders
 * the same way in the timeline (the body in mono); the changes note also carries the change glyph.
 */
export const SYSTEM_NOTE_MARKERS = {
  context: "⚡ Context:",
  suppressed: "⏸ Suppressed:",
  impact: "🕸 Impact:",
  changes: "🚀 Changes:",
  impact_raised: "⬆ Impact raised:",
} as const;
export type SystemNoteKind = keyof typeof SYSTEM_NOTE_MARKERS;

export function systemNoteKind(body: string | undefined | null): SystemNoteKind | null {
  if (!body) return null;
  for (const k of Object.keys(SYSTEM_NOTE_MARKERS) as SystemNoteKind[]) {
    if (body.startsWith(SYSTEM_NOTE_MARKERS[k])) return k;
  }
  return null;
}
