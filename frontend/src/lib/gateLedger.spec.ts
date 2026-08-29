import { describe, expect, it } from "vitest";

import {
  CHIP_DORM,
  CHIP_DOWN,
  MAX_RANGE_DAYS,
  RANGE_TOO_WIDE_TEXT,
  closureLabel,
  defaultRange,
  overrideStatusChip,
  rangeBounds,
  rangeRefusal,
  reasonChip,
  revisionLabel,
  secondsLabel,
} from "@/lib/gateLedger";

// FR-024 D-0207 items 1 and 5: the ledger's RANGE is built here and nowhere else. The calendar days
// an operator picks become an explicit half-open `[from, to)` — `to` is 00:00Z of the day AFTER the
// last picked day, so the "to" date reads inclusively while the request stays half-open — and a
// range the server would refuse is refused before any request, with the server's own sentence.

const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;

describe("defaultRange", () => {
  it("is today (UTC) and the 29 days before it — 30 calendar days inclusive", () => {
    expect(defaultRange(new Date("2026-08-29T15:00:00Z"))).toEqual({ from: "2026-07-31", to: "2026-08-29" });
    // Read as a UTC day even late in the evening of a positive-offset viewer.
    expect(defaultRange(new Date("2026-08-29T23:59:59Z"))).toEqual({ from: "2026-07-31", to: "2026-08-29" });
    expect(defaultRange(new Date("2026-03-01T00:00:00Z")), "across February").toEqual({ from: "2026-01-31", to: "2026-03-01" });
  });
});

describe("rangeBounds", () => {
  it("makes `to` 00:00Z of the day AFTER the last picked day, in RFC3339", () => {
    const b = rangeBounds("2026-08-01", "2026-08-31");
    expect(b).toEqual({ from: "2026-08-01T00:00:00Z", to: "2026-09-01T00:00:00Z", days: 31 });
    expect(b!.from).toMatch(RFC3339);
    expect(b!.to).toMatch(RFC3339);
  });
  it("a single picked day is one whole day", () => {
    expect(rangeBounds("2026-08-29", "2026-08-29")).toEqual({ from: "2026-08-29T00:00:00Z", to: "2026-08-30T00:00:00Z", days: 1 });
  });
  it("is null when a day does not parse or is missing", () => {
    expect(rangeBounds("", "2026-08-29")).toBeNull();
    expect(rangeBounds("2026-08-29", "")).toBeNull();
    expect(rangeBounds("nope", "2026-08-29")).toBeNull();
  });
  it("reports a non-positive span when the end precedes the start", () => {
    expect(rangeBounds("2026-08-10", "2026-08-01")!.days).toBeLessThanOrEqual(0);
  });
});

describe("rangeRefusal", () => {
  it("refuses 32 days with the server's sentence and accepts 31", () => {
    expect(MAX_RANGE_DAYS).toBe(31);
    expect(rangeRefusal("2026-08-01", "2026-09-01"), "32 inclusive days").toBe(RANGE_TOO_WIDE_TEXT);
    expect(rangeRefusal("2026-08-01", "2026-08-31"), "31 inclusive days — the cap itself").toBe("");
    expect(rangeRefusal("2026-08-29", "2026-08-29"), "one day").toBe("");
  });
  it("refuses an end before the start, and a missing date", () => {
    expect(rangeRefusal("2026-08-02", "2026-08-01")).toBe("The end must not be before the start.");
    expect(rangeRefusal("", "2026-08-01")).toBe("Pick both dates.");
    expect(rangeRefusal("2026-08-01", "")).toBe("Pick both dates.");
  });
});

describe("the table's cells", () => {
  const base = { revoked_at: "2026-08-29T10:00:00Z", revoked_by_label: "alice@example.com" };

  it("closureLabel per revoked_reason: who for a manual closure, why for a system one", () => {
    expect(closureLabel({ ...base, revoked_reason: "manual" })).toBe("2026-08-29 10:00:00Z · by alice@example.com");
    expect(closureLabel({ ...base, revoked_by_label: null, revoked_reason: "manual" }), "a manual closure without a label shows the dash").toBe("2026-08-29 10:00:00Z · by —");
    expect(closureLabel({ ...base, revoked_reason: "expired" })).toBe("2026-08-29 10:00:00Z · expired");
    expect(closureLabel({ ...base, revoked_reason: "policy_changed" })).toBe("2026-08-29 10:00:00Z · policy changed");
    expect(closureLabel({ ...base, revoked_reason: "policy_deleted" })).toBe("2026-08-29 10:00:00Z · policy deleted");
    expect(closureLabel({ revoked_at: null, revoked_reason: null, revoked_by_label: null }), "unclosed: empty").toBe("");
  });

  it("reasonChip's text is the CODE; the tone is lib/gate's one rule", () => {
    const stale = reasonChip({ code: "seal_stale", clause: "budget_exhausted", assignment: "block" });
    expect(stale.text).toBe("seal_stale");
    expect(stale.cls).toBe(CHIP_DORM);
    expect(stale.title).toBe("clause budget_exhausted · assigned block");
    const matched = reasonChip({ code: "budget_exhausted", clause: "budget_exhausted", assignment: "block", value: 100, source: "materializer" });
    expect(matched.text).toBe("budget_exhausted");
    expect(matched.cls).toBe(CHIP_DOWN);
    expect(matched.title).toBe("assigned block · value 100 · from materializer");
    expect(reasonChip({ code: "not_configured" }).title, "a bare code's title is the code").toBe("not_configured");
  });

  it("status chip, revision and seconds cells", () => {
    expect(overrideStatusChip("active")).not.toBe(CHIP_DORM);
    expect(overrideStatusChip("revoked")).toBe(CHIP_DORM);
    expect(overrideStatusChip("inert")).toBe(CHIP_DORM);
    expect(revisionLabel(3)).toBe("rev 3");
    expect(revisionLabel(undefined), "absent means no policy applied").toBe("—");
    expect(secondsLabel(900)).toBe("15m 0s");
    expect(secondsLabel(null)).toBe("—");
  });
});
