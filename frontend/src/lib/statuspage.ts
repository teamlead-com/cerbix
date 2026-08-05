// Presentation helpers for status-page component statuses and the page summary —
// a single source of labels and token-driven colors.

export type ComponentStatus =
  | "operational"
  | "degraded"
  | "partial_outage"
  | "major_outage"
  | "maintenance";

interface Meta {
  label: string;
  text: string; // text color token class
  dot: string; // background dot token class
  band: string; // subtle band background
}

const COMPONENT: Record<ComponentStatus, Meta> = {
  operational: { label: "Operational", text: "text-up", dot: "bg-up", band: "bg-up-weak" },
  degraded: { label: "Degraded", text: "text-degraded", dot: "bg-degraded", band: "bg-degraded-weak" },
  partial_outage: { label: "Partial outage", text: "text-degraded", dot: "bg-degraded", band: "bg-degraded-weak" },
  major_outage: { label: "Major outage", text: "text-down", dot: "bg-down", band: "bg-down-weak" },
  maintenance: { label: "Under maintenance", text: "text-maint", dot: "bg-maint", band: "bg-maint-weak" },
};

const FALLBACK: Meta = { label: "Unknown", text: "text-ink-3", dot: "bg-pending", band: "bg-surface-2" };

export function componentMeta(s?: string): Meta {
  return COMPONENT[(s as ComponentStatus)] ?? FALLBACK;
}

// The page-level headline shown in the summary banner.
export function summaryHeadline(s?: string): string {
  switch (s) {
    case "operational":
      return "All systems operational";
    case "degraded":
      return "Degraded performance";
    case "partial_outage":
      return "Partial system outage";
    case "major_outage":
      return "Major system outage";
    case "maintenance":
      return "Under maintenance";
    default:
      return "Status";
  }
}
