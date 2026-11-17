import { describe, expect, it } from "vitest";

import {
  IMPACT_ORDER,
  SYSTEM_NOTE_MARKERS,
  frozenMappingLine,
  impactBadge,
  impactBasisForm,
  impactPhrase,
  impactRank,
  systemNoteKind,
} from "@/lib/incident";

// FR-042 / NFR-035 (spec §4.6, I13; mock §4): impact `none` is an ordinary value of automatic
// incidents now, so it must read as words on every SPA surface. Two strings, one rule — a LABEL
// where impact stands alone, a PHRASE where it is named — and both come from here, so no view
// composes "<label> impact" by hand (that composition is exactly what produced "None impact").

describe("impact words (I13)", () => {
  it("labels none as 'Not specified' and leaves the other labels alone", () => {
    expect(IMPACT_ORDER.map((i) => impactBadge(i).label)).toEqual(["Not specified", "Minor", "Major", "Critical"]);
  });

  it("phrases none as 'Impact not specified' and the others as '<Label> impact'", () => {
    expect(IMPACT_ORDER.map((i) => impactPhrase(i))).toEqual([
      "Impact not specified",
      "Minor impact",
      "Major impact",
      "Critical impact",
    ]);
  });

  it("never yields the bare enum word or 'None impact' for none", () => {
    for (const s of [impactBadge("none").label, impactPhrase("none")]) {
      expect(s).not.toMatch(/\bnone\b/i);
    }
  });

  it("ranks impacts none < minor < major < critical", () => {
    expect(IMPACT_ORDER.map((i) => impactRank(i))).toEqual([0, 1, 2, 3]);
  });
});

describe("the frozen mapping line (mock §3, D-11)", () => {
  it("prints the three states in the Pages-for order with lower-case display words", () => {
    expect(frozenMappingLine({ degraded: "minor", down: "major", unknown: "none" })).toBe(
      "mapping frozen at open: down → major · degraded → minor · unknown → not specified",
    );
  });
});

describe("the basis line's form (spec §4.6, six cases)", () => {
  const mapping = { degraded: "minor", down: "critical", unknown: "none" } as const;

  it("service_mapping carries the snapshot name, the state and the frozen mapping", () => {
    expect(
      impactBasisForm({ source: "service_mapping", mapping, state: "degraded", service: { name: "Legacy search", slug: "legacy" } }, false),
    ).toEqual({
      kind: "service_mapping",
      serviceName: "Legacy search",
      state: "degraded",
      mapping: "mapping frozen at open: down → critical · degraded → minor · unknown → not specified",
    });
  });

  it("names each of the other recorded sources", () => {
    expect(impactBasisForm({ source: "project_default" }, false).kind).toBe("project_default");
    expect(impactBasisForm({ source: "manual" }, false).kind).toBe("manual");
    expect(impactBasisForm({ source: "alertmanager" }, false).kind).toBe("alertmanager");
  });

  it("tells 'never recorded' (null) apart from 'could not be read' (null + unavailable)", () => {
    expect(impactBasisForm(null, false).kind).toBe("not_recorded");
    expect(impactBasisForm(undefined, undefined).kind).toBe("not_recorded");
    expect(impactBasisForm(null, true).kind).toBe("unavailable");
  });
});

describe("system notes", () => {
  it("detects the impact-raised note by its own prefix, apart from the graph's 🕸 Impact: note", () => {
    expect(SYSTEM_NOTE_MARKERS.impact_raised).toBe("⬆ Impact raised:");
    expect(systemNoteKind("⬆ Impact raised: none → minor — the service is degraded.")).toBe("impact_raised");
    expect(systemNoteKind("🕸 Impact: payments is a probable root")).toBe("impact");
  });
});
