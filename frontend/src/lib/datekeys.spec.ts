import { describe, expect, it } from "vitest";

import { isoInstant, localDatetimeInputValue, utcDayBefore, utcDayKey, utcDayStart } from "./datekeys";

// `func-truthful-rendering.md` §9 (AC-NFR-025c). Everything in this module is a VALUE — a map key,
// a comparison, a wire string, an HTML control value — and the point of these tests is the
// property that makes each one legitimately zone-free, not the digits.

describe("date keys and control values", () => {
  it("keys a day in UTC, so two viewers in different zones look up the same row", () => {
    expect(utcDayKey(new Date("2026-09-05T23:30:00Z"))).toBe("2026-09-05");
    expect(utcDayKey("2026-09-05T00:30:00Z")).toBe("2026-09-05");
  });

  it("gives an unparseable value an empty key rather than a wrong one", () => {
    expect(utcDayKey("not-a-date")).toBe("");
  });

  it("walks back whole UTC days, and the walk does not drift at a month boundary", () => {
    const now = new Date("2026-03-02T12:00:00Z");
    expect(utcDayKey(utcDayBefore(now, 0))).toBe("2026-03-02");
    expect(utcDayKey(utcDayBefore(now, 1))).toBe("2026-03-01");
    expect(utcDayKey(utcDayBefore(now, 2))).toBe("2026-02-28");
    // and it does not mutate its argument, which a shared `today` in three strips depends on
    expect(utcDayKey(now)).toBe("2026-03-02");
  });

  it("takes midnight UTC of the day an instant falls in", () => {
    expect(utcDayStart(new Date("2026-09-05T23:30:00Z"))).toBe(Date.parse("2026-09-05T00:00:00Z"));
  });

  it("hands the API an RFC 3339 instant", () => {
    expect(isoInstant(new Date("2026-09-05T14:05:00Z"))).toBe("2026-09-05T14:05:00.000Z");
  });

  it("builds a datetime-local control value with no zone, because the HTML format has none", () => {
    // Deliberately asserted against the LOCAL getters rather than a fixed string: the value is
    // the viewer's wall clock by definition, so pinning digits would only pin the runner's zone.
    const d = new Date(2026, 8, 5, 14, 5);
    expect(localDatetimeInputValue(d)).toBe("2026-09-05T14:05");
    expect(localDatetimeInputValue(d)).not.toMatch(/Z|UTC|[+-]\d{2}:\d{2}$/);
  });

  it("pads every field, so the control never receives a value it would reject", () => {
    expect(localDatetimeInputValue(new Date(2026, 0, 2, 3, 4))).toBe("2026-01-02T03:04");
  });
});
