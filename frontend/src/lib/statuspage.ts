// Presentation helpers for status-page component statuses and the page summary —
// a single source of labels and token-driven colors.

export type ComponentStatus =
  | "operational"
  | "degraded"
  | "partial_outage"
  | "major_outage"
  | "maintenance"
  | "no_data";

export type PageSummaryState = "operational" | "impaired" | "no_data" | "empty";

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
  // FR-021 §15.0: measurement is ABSENT. It borrows the pending token deliberately — the same
  // neutral cerbix already uses for "not established yet" — because dressing an unknown in a
  // health colour is the defect the status was added to remove. It is NOT a severity: no red, no
  // amber, and never rolled into the worst-of ladder.
  no_data: { label: "No data", text: "text-ink-3", dot: "bg-pending", band: "bg-surface-2" },
};

const FALLBACK: Meta = { label: "Unknown", text: "text-ink-3", dot: "bg-pending", band: "bg-surface-2" };

export function componentMeta(s?: string): Meta {
  return COMPONENT[s as ComponentStatus] ?? FALLBACK;
}

// The page-level headline shown in the summary banner.
//
// `state` is what the server computed, and it is preferred over `summary` because one component
// status cannot express "operational, but part of this page was not measured". The `summary`
// argument stays as the fallback for a server that has not been updated yet.
export function summaryHeadline(summary?: string, state?: string, unmeasured = 0): string {
  switch (state) {
    case "empty":
      // Never "all systems operational": there are no systems.
      return "No components configured";
    case "no_data":
      return "No measurements available";
    case "operational":
      return unmeasured > 0
        ? `Measured systems operational · ${unmeasured} not measured`
        : "All systems operational";
    case "impaired":
      break; // fall through to the per-status headline below
  }
  switch (summary) {
    case "operational":
      return unmeasured > 0
        ? `Measured systems operational · ${unmeasured} not measured`
        : "All systems operational";
    case "degraded":
      return "Degraded performance";
    case "partial_outage":
      return "Partial system outage";
    case "major_outage":
      return "Major system outage";
    case "maintenance":
      return "Under maintenance";
    case "no_data":
      return "No measurements available";
    default:
      return "Status";
  }
}

// The operator-facing reason a component is not a measurement. The public page never receives
// these; an operator needs the WHY that a customer does not.
const REASONS: Record<string, string> = {
  no_manual_status: "No status has been set for this manual component.",
  monitor_never_confirmed: "The monitor has not confirmed a state yet.",
  monitor_deleted: "The bound monitor was deleted; convert or remove this component.",
  no_sli_declared: "The service declares no reliability inputs, so there is nothing to measure.",
  no_decidable_observation: "No decidable observation covers this moment.",
  excluded_by_maintenance: "A declared maintenance window is in force.",
  service_unreadable: "This service could not be read — the value shown is not a measurement.",
};

export function reasonText(reason?: string): string {
  if (!reason) return "";
  return REASONS[reason] ?? reason;
}

// The component's ACTIVE source. The label says which fact the line reports, because after a
// conversion the dormant binding is still stored and an operator has to be able to tell them apart.
export function sourceLabel(source?: string): string {
  switch (source) {
    case "service":
      return "service";
    case "monitor":
      return "monitor";
    case "manual":
      return "manual";
    default:
      return "";
  }
}

// Why a 90-day number is absent. Every one of these is a REASON, not an apology: §11.2/§11.3 say a
// withheld number must carry why it was withheld, because a blank is indistinguishable from a
// number nobody computed.
const WITHHELD: Record<string, string> = {
  no_sli: "No reliability inputs are declared, so there is nothing to measure.",
  nothing_sealed: "No history recorded yet.",
  nothing_measured: "No measurements in this window.",
  window_precedes_materialization_era: "Less than 90 days of history so far.",
  storage_gap: "Part of this window is missing, so no single figure can stand for it.",
  zero_decidable_time: "Nothing in this window was decidable.",
  decidable_coverage_below_min: "Coverage in this window is too low to quote a figure.",
  spans_definition_revisions: "Availability was redefined during this window, so one figure would mix two meanings.",
};

export function withheldText(reason?: string): string {
  if (!reason) return "No history recorded yet.";
  return WITHHELD[reason] ?? "No figure available for this window.";
}
