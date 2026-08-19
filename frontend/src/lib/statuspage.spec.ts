import { describe, expect, it } from "vitest";

import { componentMeta, reasonText, sourceLabel, summaryHeadline, withheldText } from "@/lib/statuspage";

// FR-021 §15.0 / §17: the presentation layer is where an unknown would most easily be dressed up
// as health, so the rules are pinned here rather than only in the views that use them.
describe("statuspage presentation", () => {
  it("gives no_data a neutral, non-severity treatment", () => {
    const m = componentMeta("no_data");
    expect(m.label).toBe("No data");
    // Not a health colour and not a severity colour: an unknown is neither good nor bad.
    expect(m.text).not.toContain("text-up");
    expect(m.dot).not.toContain("bg-up");
    expect(m.dot).not.toContain("bg-down");
    expect(m.dot).not.toContain("bg-degraded");
  });

  it("never says all-clear while part of the page is unmeasured", () => {
    expect(summaryHeadline("operational", "operational", 0)).toBe("All systems operational");
    const partial = summaryHeadline("operational", "operational", 2);
    expect(partial).not.toBe("All systems operational");
    expect(partial).toContain("2");
    expect(partial).toContain("not measured");
  });

  it("states an empty page as empty and an unmeasured page as unmeasured", () => {
    // Before phase 4 both of these read "All systems operational" (§17).
    expect(summaryHeadline("no_data", "empty", 0)).toBe("No components configured");
    expect(summaryHeadline("no_data", "no_data", 3)).toBe("No measurements available");
  });

  it("keeps an outage the headline even when something is also unmeasured", () => {
    expect(summaryHeadline("major_outage", "impaired", 1)).toBe("Major system outage");
  });

  it("falls back to the status alone when a server sends no state", () => {
    expect(summaryHeadline("degraded")).toBe("Degraded performance");
    expect(summaryHeadline(undefined)).toBe("Status");
  });

  it("turns each machine reason into an operator-readable sentence", () => {
    for (const r of [
      "no_manual_status",
      "monitor_never_confirmed",
      "monitor_deleted",
      "no_sli_declared",
      "no_decidable_observation",
      "excluded_by_maintenance",
      "service_unreadable",
    ]) {
      const text = reasonText(r);
      expect(text.length).toBeGreaterThan(10);
      // The raw machine token must not be what an operator reads.
      expect(text).not.toBe(r);
    }
    // An unknown reason is shown as-is rather than swallowed: a blank explanation would be worse
    // than an ugly one.
    expect(reasonText("something_new")).toBe("something_new");
    expect(reasonText(undefined)).toBe("");
  });

  it("labels the three sources and nothing else", () => {
    expect(sourceLabel("monitor")).toBe("monitor");
    expect(sourceLabel("service")).toBe("service");
    expect(sourceLabel("manual")).toBe("manual");
    expect(sourceLabel(undefined)).toBe("");
  });
});

// [314] P1-7 — a withheld number must carry its reason. A blank where a percentage belongs is
// indistinguishable from a number nobody bothered to compute, which is the §11.2/§11.3 rule the
// public page now has to honour too.
describe("withheld reasons", () => {
  it("explains every reason the server can send", () => {
    for (const r of [
      "no_sli",
      "nothing_sealed",
      "nothing_measured",
      "window_precedes_materialization_era",
      "storage_gap",
      "zero_decidable_time",
      "decidable_coverage_below_min",
      "spans_definition_revisions",
    ]) {
      const text = withheldText(r);
      expect(text.length).toBeGreaterThan(10);
      expect(text).not.toBe(r);
      // No apology, no hedging: the sentence states what is true.
      expect(text.toLowerCase()).not.toContain("sorry");
    }
  });

  it("never leaves the space blank, even for a reason it does not know", () => {
    expect(withheldText("something_new")).toContain("No figure");
    expect(withheldText(undefined)).toContain("No history");
  });

  it("distinguishes a redefinition from missing data", () => {
    // Both withhold, and an operator reading the page has to be able to tell them apart.
    expect(withheldText("spans_definition_revisions")).not.toBe(withheldText("storage_gap"));
    expect(withheldText("spans_definition_revisions")).toContain("redefined");
  });
});
