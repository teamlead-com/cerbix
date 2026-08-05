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
