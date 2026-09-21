import { describe, expect, it } from "vitest";

import type { IncidentImpact } from "@/lib/incident";
import {
  overallStatusPresentation,
  type ComponentStatus,
  type OverallStatusVisual,
  type PageSummaryState,
} from "@/lib/statuspage";

const IMPACT_CASES: Array<{
  impact: IncidentImpact;
  copy: string;
  visual: OverallStatusVisual;
  bandClass: string;
}> = [
  {
    impact: "none",
    copy: "Impact not specified",
    visual: "neutral",
    bandClass: "bg-surface-2",
  },
  {
    impact: "minor",
    copy: "Minor impact",
    visual: "warning",
    bandClass: "bg-degraded-weak",
  },
  {
    impact: "major",
    copy: "Major impact",
    visual: "warning",
    bandClass: "bg-degraded-weak",
  },
  {
    impact: "critical",
    copy: "Critical impact",
    visual: "danger",
    bandClass: "bg-down-weak",
  },
];

interface ComponentFact {
  name: string;
  summary: ComponentStatus;
  state: PageSummaryState;
  unmeasuredCount: number;
  zero: {
    headline: string;
    supportingCopy: string;
    visual: OverallStatusVisual;
    icon: "check" | "alert";
    bandClass: string;
  };
  active: (
    count: number,
    impactCopy: string,
  ) => {
    headline: string;
    supportingCopy: string;
    visual?: OverallStatusVisual;
    bandClass?: string;
  };
}

const COMPONENT_FACTS: ComponentFact[] = [
  {
    name: "operational",
    summary: "operational",
    state: "operational",
    unmeasuredCount: 0,
    zero: {
      headline: "All systems operational",
      supportingCopy: "All measured services are running normally.",
      visual: "operational",
      icon: "check",
      bandClass: "bg-up-weak",
    },
    active: (count, impactCopy) => ({
      headline: `${count} active incident${count === 1 ? "" : "s"}`,
      supportingCopy: `${impactCopy}. All measured services are currently operational.`,
    }),
  },
  {
    name: "impaired",
    summary: "degraded",
    state: "impaired",
    unmeasuredCount: 0,
    zero: {
      headline: "Degraded performance",
      supportingCopy:
        "Some services are experiencing elevated latency. We’re on it.",
      visual: "warning",
      icon: "alert",
      bandClass: "bg-degraded-weak",
    },
    active: (count, impactCopy) => ({
      headline: "Degraded performance",
      supportingCopy: `Some services are experiencing elevated latency. We’re on it. ${count} active incident${count === 1 ? "" : "s"} · ${impactCopy}.`,
      visual: "warning",
      bandClass: "bg-degraded-weak",
    }),
  },
  {
    name: "no-data",
    summary: "no_data",
    state: "no_data",
    unmeasuredCount: 2,
    zero: {
      headline: "No measurements available",
      supportingCopy:
        "None of the components on this page have a measurement yet.",
      visual: "neutral",
      icon: "alert",
      bandClass: "bg-surface-2",
    },
    active: (count, impactCopy) => ({
      headline: `${count} active incident${count === 1 ? "" : "s"}`,
      supportingCopy: `${impactCopy}. Component measurements are not available yet.`,
    }),
  },
  {
    name: "empty",
    summary: "no_data",
    state: "empty",
    unmeasuredCount: 0,
    zero: {
      headline: "No components configured",
      supportingCopy:
        "This page has no components yet, so there is nothing to report.",
      visual: "neutral",
      icon: "alert",
      bandClass: "bg-surface-2",
    },
    active: (count, impactCopy) => ({
      headline: `${count} active incident${count === 1 ? "" : "s"}`,
      supportingCopy: `${impactCopy}. No components are configured on this page.`,
    }),
  },
];

const MATRIX_CASES = COMPONENT_FACTS.flatMap((fact) => {
  const zero = {
    name: `${fact.name} × zero incidents`,
    input: {
      summary: fact.summary,
      state: fact.state,
      unmeasuredCount: fact.unmeasuredCount,
      activeIncidentImpacts: [] as IncidentImpact[],
    },
    expected: {
      ...fact.zero,
      activeCount: 0,
      worstImpact: "none" as IncidentImpact,
    },
  };

  const active = IMPACT_CASES.flatMap((impactCase) =>
    ([1, 3] as const).map((count) => {
      const factExpected = fact.active(count, impactCase.copy);
      return {
        name: `${fact.name} × ${count === 1 ? "one" : "many"} incidents × ${impactCase.impact}`,
        input: {
          summary: fact.summary,
          state: fact.state,
          unmeasuredCount: fact.unmeasuredCount,
          activeIncidentImpacts: Array<IncidentImpact>(count).fill(
            impactCase.impact,
          ),
        },
        expected: {
          ...factExpected,
          visual: factExpected.visual ?? impactCase.visual,
          icon: "alert" as const,
          activeCount: count,
          worstImpact: impactCase.impact,
          bandClass: factExpected.bandClass ?? impactCase.bandClass,
        },
      };
    }),
  );

  return [zero, ...active];
});

describe("incident-aware overall status presentation", () => {
  it.each(MATRIX_CASES)("$name", ({ input, expected }) => {
    expect(overallStatusPresentation(input)).toMatchObject(expected);
  });

  it("uses summary fallback only when summary_state is absent", () => {
    expect(
      overallStatusPresentation({
        summary: "degraded",
        state: "operational",
        activeIncidentImpacts: ["major"],
      }),
    ).toMatchObject({
      headline: "1 active incident",
      supportingCopy:
        "Major impact. All measured services are currently operational.",
      visual: "warning",
      icon: "alert",
    });

    expect(
      overallStatusPresentation({
        summary: "degraded",
        activeIncidentImpacts: ["major"],
      }),
    ).toMatchObject({
      headline: "Degraded performance",
      visual: "warning",
      icon: "alert",
    });
  });

  it.each([
    {
      state: "operational",
      summary: "major_outage",
      headline: "1 active incident",
      supportingCopy:
        "Major impact. All measured services are currently operational.",
    },
    {
      state: "operational",
      summary: "no_data",
      headline: "1 active incident",
      supportingCopy:
        "Major impact. All measured services are currently operational.",
    },
    {
      state: "no_data",
      summary: "major_outage",
      headline: "1 active incident",
      supportingCopy:
        "Major impact. Component measurements are not available yet.",
    },
    {
      state: "empty",
      summary: "major_outage",
      headline: "1 active incident",
      supportingCopy:
        "Major impact. No components are configured on this page.",
    },
    {
      state: "impaired",
      summary: "operational",
      headline: "Service disruption",
      supportingCopy:
        "One or more measured services are impaired. 1 active incident · Major impact.",
    },
  ] as Array<{
    state: PageSummaryState;
    summary: ComponentStatus;
    headline: string;
    supportingCopy: string;
  }>)(
    "uses authoritative $state component truth despite conflicting $summary summary",
    ({ state, summary, headline, supportingCopy }) => {
      const presentation = overallStatusPresentation({
        summary,
        state,
        activeIncidentImpacts: ["major"],
      });
      expect(presentation.headline).toBe(headline);
      expect(presentation.supportingCopy).toBe(supportingCopy);
      expect(presentation.visual).not.toBe("operational");
      expect(presentation.bandClass).not.toBe("bg-up-weak");
      expect(presentation.icon).toBe("alert");
    },
  );

  it("keeps maintenance as the component-owned headline", () => {
    expect(
      overallStatusPresentation({
        summary: "maintenance",
        state: "impaired",
        activeIncidentImpacts: ["major"],
      }),
    ).toMatchObject({
      headline: "Under maintenance",
      supportingCopy:
        "Scheduled maintenance is in progress. 1 active incident · Major impact.",
      visual: "maintenance",
      icon: "alert",
      worstImpact: "major",
    });
  });

  it("preserves unmeasured disclosure with active incidents", () => {
    expect(
      overallStatusPresentation({
        summary: "operational",
        state: "operational",
        unmeasuredCount: 2,
        activeIncidentImpacts: ["major"],
      }),
    ).toMatchObject({
      headline: "1 active incident",
      supportingCopy:
        "Major impact. All measured services are currently operational. 2 components on this page have no measurement.",
      visual: "warning",
      icon: "alert",
    });
  });

  it("is independent of active incident order and lets Critical win", () => {
    const input = {
      summary: "operational" as const,
      state: "operational" as const,
      unmeasuredCount: 0,
    };
    const forward = overallStatusPresentation({
      ...input,
      activeIncidentImpacts: ["none", "minor", "major", "critical"],
    });
    const reverse = overallStatusPresentation({
      ...input,
      activeIncidentImpacts: ["critical", "major", "minor", "none"],
    });
    expect(reverse).toEqual(forward);
    expect(forward.worstImpact).toBe("critical");
    expect(forward.visual).toBe("danger");
  });

  it("ignores resolved or recent incidents because only the active array is an input", () => {
    const presentation = overallStatusPresentation({
      summary: "operational",
      state: "operational",
      unmeasuredCount: 0,
      activeIncidentImpacts: [],
    });
    expect(presentation.headline).toBe("All systems operational");
    expect(presentation.icon).toBe("check");
    expect(presentation.activeCount).toBe(0);
  });
});
