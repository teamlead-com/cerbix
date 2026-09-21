// Presentation helpers for status-page component statuses and the page summary —
// a single source of labels and token-driven colors.

import { IMPACT_ORDER, impactBadge, type IncidentImpact } from "@/lib/incident";

export type ComponentStatus =
  | "operational"
  | "degraded"
  | "partial_outage"
  | "major_outage"
  | "maintenance"
  | "no_data";

export type PageSummaryState = "operational" | "impaired" | "no_data" | "empty";

export type OverallStatusVisual =
  "operational" | "warning" | "danger" | "maintenance" | "neutral";
export type OverallStatusIcon = "check" | "alert";

export interface OverallStatusPresentation {
  headline: string;
  supportingCopy: string;
  visual: OverallStatusVisual;
  icon: OverallStatusIcon;
  activeCount: number;
  worstImpact: IncidentImpact;
  bandClass: string;
  textClass: string;
  iconClass: string;
}

interface Meta {
  label: string;
  text: string; // text color token class
  dot: string; // background dot token class
  band: string; // subtle band background
}

const COMPONENT: Record<ComponentStatus, Meta> = {
  operational: {
    label: "Operational",
    text: "text-up",
    dot: "bg-up",
    band: "bg-up-weak",
  },
  degraded: {
    label: "Degraded",
    text: "text-degraded",
    dot: "bg-degraded",
    band: "bg-degraded-weak",
  },
  partial_outage: {
    label: "Partial outage",
    text: "text-degraded",
    dot: "bg-degraded",
    band: "bg-degraded-weak",
  },
  major_outage: {
    label: "Major outage",
    text: "text-down",
    dot: "bg-down",
    band: "bg-down-weak",
  },
  maintenance: {
    label: "Under maintenance",
    text: "text-maint",
    dot: "bg-maint",
    band: "bg-maint-weak",
  },
  // FR-021 §15.0: measurement is ABSENT. It borrows the pending token deliberately — the same
  // neutral cerbix already uses for "not established yet" — because dressing an unknown in a
  // health colour is the defect the status was added to remove. It is NOT a severity: no red, no
  // amber, and never rolled into the worst-of ladder.
  no_data: {
    label: "No data",
    text: "text-ink-3",
    dot: "bg-pending",
    band: "bg-surface-2",
  },
};

const FALLBACK: Meta = {
  label: "Unknown",
  text: "text-ink-3",
  dot: "bg-pending",
  band: "bg-surface-2",
};

export function componentMeta(s?: string): Meta {
  return COMPONENT[s as ComponentStatus] ?? FALLBACK;
}

// The page-level headline shown in the summary banner.
//
// `state` is what the server computed, and it is preferred over `summary` because one component
// status cannot express "operational, but part of this page was not measured". The `summary`
// argument stays as the fallback for a server that has not been updated yet.
export function summaryHeadline(
  summary?: ComponentStatus,
  state?: PageSummaryState,
  unmeasured = 0,
): string {
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
      switch (summary) {
        case "degraded":
          return "Degraded performance";
        case "partial_outage":
          return "Partial system outage";
        case "major_outage":
          return "Major system outage";
        case "maintenance":
          return "Under maintenance";
        default:
          return "Service disruption";
      }
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

function normalizedImpact(impact?: string | null): IncidentImpact {
  return IMPACT_ORDER.includes(impact as IncidentImpact)
    ? (impact as IncidentImpact)
    : "none";
}

function worstIncidentImpact(
  impacts: readonly (string | null | undefined)[],
): IncidentImpact {
  return impacts.reduce<IncidentImpact>((worst, impact) => {
    const normalized = normalizedImpact(impact);
    return IMPACT_ORDER.indexOf(normalized) > IMPACT_ORDER.indexOf(worst)
      ? normalized
      : worst;
  }, "none");
}

function incidentImpactCopy(impact: IncidentImpact): string {
  return impact === "none"
    ? "Impact not specified"
    : `${impactBadge(impact).label} impact`;
}

function unmeasuredDisclosure(unmeasured: number): string {
  if (unmeasured <= 0) return "";
  return ` ${unmeasured} component${unmeasured === 1 ? "" : "s"} on this page ${unmeasured === 1 ? "has" : "have"} no measurement.`;
}

function componentSupportingCopy(
  summary?: ComponentStatus,
  state?: PageSummaryState,
  unmeasured = 0,
): string {
  switch (state) {
    case "empty":
      return "This page has no components yet, so there is nothing to report.";
    case "no_data":
      return unmeasured === 1
        ? "The one component on this page has no measurement yet."
        : "None of the components on this page have a measurement yet.";
    case "operational":
      return (
        "All measured services are running normally." +
        unmeasuredDisclosure(unmeasured)
      );
    case "impaired":
      if (
        summary !== "degraded" &&
        summary !== "partial_outage" &&
        summary !== "major_outage" &&
        summary !== "maintenance"
      ) {
        return (
          "One or more measured services are impaired." +
          unmeasuredDisclosure(unmeasured)
        );
      }
      break;
  }

  const disclosure = unmeasuredDisclosure(unmeasured);
  switch (summary) {
    case "operational":
      return "All measured services are running normally." + disclosure;
    case "degraded":
      return (
        "Some services are experiencing elevated latency. We’re on it." +
        disclosure
      );
    case "partial_outage":
      return "Some services are partially unavailable." + disclosure;
    case "major_outage":
      return "A major outage is affecting one or more services." + disclosure;
    case "maintenance":
      return "Scheduled maintenance is in progress." + disclosure;
    default:
      return "Live status of every monitored service." + disclosure;
  }
}

function componentVisual(
  summary?: ComponentStatus,
): Pick<
  OverallStatusPresentation,
  "visual" | "bandClass" | "textClass" | "iconClass"
> {
  const meta = componentMeta(summary);
  switch (summary) {
    case "operational":
      return {
        visual: "operational",
        bandClass: meta.band,
        textClass: meta.text,
        iconClass: meta.dot,
      };
    case "degraded":
    case "partial_outage":
      return {
        visual: "warning",
        bandClass: meta.band,
        textClass: meta.text,
        iconClass: meta.dot,
      };
    case "major_outage":
      return {
        visual: "danger",
        bandClass: meta.band,
        textClass: meta.text,
        iconClass: meta.dot,
      };
    case "maintenance":
      return {
        visual: "maintenance",
        bandClass: meta.band,
        textClass: meta.text,
        iconClass: meta.dot,
      };
    default:
      return {
        visual: "neutral",
        bandClass: meta.band,
        textClass: meta.text,
        iconClass: meta.dot,
      };
  }
}

function componentVisualForFact(
  summary?: ComponentStatus,
  state?: PageSummaryState,
): Pick<
  OverallStatusPresentation,
  "visual" | "bandClass" | "textClass" | "iconClass"
> {
  switch (state) {
    case "operational":
      return componentVisual("operational");
    case "no_data":
    case "empty":
      return componentVisual("no_data");
    case "impaired":
      return summary === "degraded" ||
        summary === "partial_outage" ||
        summary === "major_outage" ||
        summary === "maintenance"
        ? componentVisual(summary)
        : componentVisual("degraded");
    default:
      return componentVisual(summary);
  }
}

function incidentVisual(
  worstImpact: IncidentImpact,
): Pick<
  OverallStatusPresentation,
  "visual" | "bandClass" | "textClass" | "iconClass"
> {
  if (worstImpact === "critical") {
    return {
      visual: "danger",
      bandClass: "bg-down-weak",
      textClass: "text-down",
      iconClass: "bg-down",
    };
  }
  if (worstImpact === "major" || worstImpact === "minor") {
    return {
      visual: "warning",
      bandClass: "bg-degraded-weak",
      textClass: "text-degraded",
      iconClass: "bg-degraded",
    };
  }
  return {
    visual: "neutral",
    bandClass: "bg-surface-2",
    textClass: "text-ink-3",
    iconClass: "bg-pending",
  };
}

function componentOwnsHeadline(
  summary?: ComponentStatus,
  state?: PageSummaryState,
): boolean {
  if (state !== undefined) return state === "impaired";
  return (
    summary === "degraded" ||
    summary === "partial_outage" ||
    summary === "major_outage" ||
    summary === "maintenance"
  );
}

function incidentOwnedComponentCopy(
  summary?: ComponentStatus,
  state?: PageSummaryState,
  unmeasured = 0,
): string {
  switch (state) {
    case "empty":
      return "No components are configured on this page.";
    case "no_data":
      return "Component measurements are not available yet.";
    case "operational":
      return (
        "All measured services are currently operational." +
        unmeasuredDisclosure(unmeasured)
      );
    case "impaired":
      return componentSupportingCopy(summary, state, unmeasured);
  }

  if (!summary || summary === "no_data") {
    return "Component measurements are not available yet.";
  }
  return (
    "All measured services are currently operational." +
    unmeasuredDisclosure(unmeasured)
  );
}

export function overallStatusPresentation(input: {
  summary?: ComponentStatus;
  state?: PageSummaryState;
  unmeasuredCount?: number;
  activeIncidentImpacts?: readonly (string | null | undefined)[];
}): OverallStatusPresentation {
  const unmeasured = input.unmeasuredCount ?? 0;
  const impacts = input.activeIncidentImpacts ?? [];
  const activeCount = impacts.length;
  const worstImpact = worstIncidentImpact(impacts);
  const componentPresentation = componentVisualForFact(
    input.summary,
    input.state,
  );

  if (activeCount === 0) {
    return {
      headline: summaryHeadline(input.summary, input.state, unmeasured),
      supportingCopy: componentSupportingCopy(
        input.summary,
        input.state,
        unmeasured,
      ),
      ...componentPresentation,
      icon:
        (input.state === "operational" ||
          (input.state === undefined && input.summary === "operational")) &&
        unmeasured === 0
          ? "check"
          : "alert",
      activeCount,
      worstImpact,
    };
  }

  const impactCopy = incidentImpactCopy(worstImpact);
  if (componentOwnsHeadline(input.summary, input.state)) {
    const incidentCopy = `${activeCount} active incident${activeCount === 1 ? "" : "s"} · ${impactCopy}.`;
    return {
      headline: summaryHeadline(input.summary, input.state, unmeasured),
      supportingCopy: `${componentSupportingCopy(input.summary, input.state, unmeasured)} ${incidentCopy}`,
      ...componentPresentation,
      icon: "alert",
      activeCount,
      worstImpact,
    };
  }

  return {
    headline: `${activeCount} active incident${activeCount === 1 ? "" : "s"}`,
    supportingCopy: `${impactCopy}. ${incidentOwnedComponentCopy(input.summary, input.state, unmeasured)}`,
    ...incidentVisual(worstImpact),
    icon: "alert",
    activeCount,
    worstImpact,
  };
}

// The operator-facing reason a component is not a measurement. The public page never receives
// these; an operator needs the WHY that a customer does not.
const REASONS: Record<string, string> = {
  no_manual_status: "No status has been set for this manual component.",
  monitor_never_confirmed: "The monitor has not confirmed a state yet.",
  monitor_deleted:
    "The bound monitor was deleted; convert or remove this component.",
  no_sli_declared:
    "The service declares no reliability inputs, so there is nothing to measure.",
  no_decidable_observation: "No decidable observation covers this moment.",
  excluded_by_maintenance: "A declared maintenance window is in force.",
  service_unreadable:
    "This service could not be read — the value shown is not a measurement.",
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
  storage_gap:
    "Part of this window is missing, so no single figure can stand for it.",
  zero_decidable_time: "Nothing in this window was decidable.",
  decidable_coverage_below_min:
    "Coverage in this window is too low to quote a figure.",
  spans_definition_revisions:
    "Availability was redefined during this window, so one figure would mix two meanings.",
};

export function withheldText(reason?: string): string {
  if (!reason) return "No history recorded yet.";
  return WITHHELD[reason] ?? "No figure available for this window.";
}
