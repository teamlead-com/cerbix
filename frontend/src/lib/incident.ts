// Shared presentation helpers for incident status and impact — a single source
// of labels and token-driven colors used across the incident views.

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

const IMPACT: Record<IncidentImpact, Badge> = {
  none: { label: "None", cls: "text-ink-3 bg-surface-2" },
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

// ── System-authored notes in an incident's timeline ──────────────────────────────────────────

/**
 * The prefixes of the notes cerbix itself writes into `incident_updates` (the `internal/domain`
 * markers): `⚡ Context:` and `⏸ Suppressed:` (func-incident-context), `🕸 Impact:`
 * (func-service-reliability §14.4) and, from FR-025 D7, `🚀 Changes:`. A note is detected by its
 * PREFIX — the marker is also the server's idempotency guard on redelivery — and every one renders
 * the same way in the timeline (the body in mono); the changes note also carries the change glyph.
 */
export const SYSTEM_NOTE_MARKERS = {
  context: "⚡ Context:",
  suppressed: "⏸ Suppressed:",
  impact: "🕸 Impact:",
  changes: "🚀 Changes:",
} as const;
export type SystemNoteKind = keyof typeof SYSTEM_NOTE_MARKERS;

export function systemNoteKind(body: string | undefined | null): SystemNoteKind | null {
  if (!body) return null;
  for (const k of Object.keys(SYSTEM_NOTE_MARKERS) as SystemNoteKind[]) {
    if (body.startsWith(SYSTEM_NOTE_MARKERS[k])) return k;
  }
  return null;
}
