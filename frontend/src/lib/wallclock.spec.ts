import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, resolve } from "node:path";

import {
  instantLabel, instantLabelShort, instantRangeLabel,
  utcCellExtentLabel, utcExtentLabel, utcInstantLabel,
} from "./wallclock";

// func-truthful-rendering §8 (FR-031 / NFR-025a, D-0235): identity is UTC, presentation is local,
// and the offset is resolved AT THE INSTANT. Every case here is a rule the specification states,
// not a description of what the code happens to do.
//
// `zone` is passed throughout so the DST property is testable at a NAMED zone rather than at
// whatever zone the runner happens to sit in. The last test in this file asserts that no product
// call site does the same.

describe("instantLabel", () => {
  it("renders an instant in the viewer's zone and names the offset in force then", () => {
    expect(instantLabel("2026-09-03T15:04:31Z", "Asia/Yekaterinburg")).toBe(
      "03.09.2026 20:04:31 (UTC+05:00)",
    );
  });

  it("keeps the minutes of a non-whole-hour zone", () => {
    // +05:30 is why an 'hours' offset would be wrong, and why hourly cells cannot be local.
    expect(instantLabel("2026-09-03T15:04:31Z", "Asia/Kolkata")).toBe(
      "03.09.2026 20:34:31 (UTC+05:30)",
    );
  });

  it("renders UTC itself as +00:00 rather than as a bare GMT", () => {
    expect(instantLabel("2026-09-03T15:04:31Z", "UTC")).toBe("03.09.2026 15:04:31 (UTC+00:00)");
  });

  it("crosses local midnight without losing the date", () => {
    // 21:23Z at UTC+05 is the NEXT local day; a formatter that kept the UTC date would lie here.
    expect(instantLabel("2026-09-03T21:23:00Z", "Asia/Yekaterinburg")).toBe(
      "04.09.2026 02:23:00 (UTC+05:00)",
    );
  });

  it("is a dash for an absent or unparseable instant, never a fabricated one", () => {
    expect(instantLabel(null)).toBe("—");
    expect(instantLabel(undefined)).toBe("—");
    expect(instantLabel("")).toBe("—");
    expect(instantLabel("not a time")).toBe("—");
  });
});

// The load-bearing property: a CACHED current offset mislabels every instant on the far side of a
// DST boundary, and a 30-day window in late March or late October crosses one — so this is the
// ordinary case, not an edge. Both instants below are formatted for the SAME zone and must carry
// DIFFERENT offsets; an implementation that resolved the offset once would fail this and nothing
// else in the file.
describe("the offset is resolved at the instant, not cached", () => {
  it("gives one zone two different offsets across a DST boundary", () => {
    const winter = instantLabel("2026-01-15T12:00:00Z", "Europe/Berlin");
    const summer = instantLabel("2026-07-15T12:00:00Z", "Europe/Berlin");
    expect(winter).toContain("(UTC+01:00)");
    expect(summer).toContain("(UTC+02:00)");
    expect(winter).toBe("15.01.2026 13:00:00 (UTC+01:00)");
    expect(summer).toBe("15.07.2026 14:00:00 (UTC+02:00)");
  });

  it("names BOTH offsets when one UTC cell spans the change", () => {
    // Europe/Berlin springs forward at 2026-03-29 01:00Z. A UTC day containing that instant is
    // 23 local hours long, and one offset cannot describe it.
    expect(utcCellExtentLabel("2026-03-29T00:00:00Z", "2026-03-30T00:00:00Z", "Europe/Berlin")).toBe(
      "29.03 01:00 → 30.03 02:00 (UTC+01:00 → UTC+02:00)",
    );
  });
});

describe("utcCellExtentLabel", () => {
  it("renders a UTC day as its real local extent, never as the viewer's calendar day", () => {
    // The whole point: for a viewer at UTC+05 this UTC day begins at 05:00 their time. A label
    // reading '01.09' would be a boundary lie.
    const label = utcCellExtentLabel("2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z", "Asia/Yekaterinburg");
    expect(label).toBe("01.09 05:00 → 02.09 05:00 (UTC+05:00)");
    expect(label).toContain("→");
  });

  it("renders a sub-day cell — a boundary fragment — at its real minutes", () => {
    expect(utcCellExtentLabel("2026-09-01T02:30:00Z", "2026-09-01T02:34:00Z", "Asia/Yekaterinburg")).toBe(
      "01.09 07:30 → 01.09 07:34 (UTC+05:00)",
    );
  });

  it("is a dash when either end is absent, so a half-known extent is never drawn", () => {
    expect(utcCellExtentLabel(null, "2026-09-02T00:00:00Z")).toBe("—");
    expect(utcCellExtentLabel("2026-09-01T00:00:00Z", null)).toBe("—");
    expect(utcCellExtentLabel("bad", "worse")).toBe("—");
  });
});

describe("the UTC line", () => {
  it("carries the canonical instant for log correlation", () => {
    expect(utcInstantLabel("2026-09-01T00:00:00Z")).toBe("2026-09-01T00:00:00Z");
    expect(utcInstantLabel("2026-09-03T15:04:31.000Z")).toBe("2026-09-03T15:04:31Z");
    expect(utcInstantLabel(null)).toBe("—");
  });

  it("carries the canonical extent when the subject is a cell", () => {
    expect(utcExtentLabel("2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z")).toBe(
      "2026-09-01T00:00:00Z → 2026-09-02T00:00:00Z",
    );
    expect(utcExtentLabel("2026-09-01T00:00:00Z", null)).toBe("—");
  });
});

// §8's structural rule, asserted against the SOURCE rather than trusted: there is no generic
// formatter that could be handed a UTC bucket and produce a local calendar day, and the test seam
// is not reachable from the product.
describe("the mechanism's shape", () => {
  const SRC = resolve(__dirname, "..");

  function walk(dir: string): string[] {
    const out: string[] = [];
    for (const name of readdirSync(dir)) {
      const p = join(dir, name);
      if (statSync(p).isDirectory()) out.push(...walk(p));
      else if (/\.(ts|vue)$/.test(name)) out.push(p);
    }
    return out;
  }

  it("exports named instant and cell-extent functions and no generic date formatter", () => {
    const src = readFileSync(join(SRC, "lib/wallclock.ts"), "utf8");
    const exported = [...src.matchAll(/export function (\w+)/g)].map((m) => m[1]).sort();
    expect(exported).toEqual([
      "instantLabel", "instantLabelShort", "instantRangeLabel",
      "utcCellExtentLabel", "utcExtentLabel", "utcInstantLabel",
    ]);
    // A name like formatDate / formatTime is exactly what a caller reaches for when it has a
    // bucket and wants "a date"; there is deliberately nothing here to reach for.
    expect(src).not.toMatch(/export function format(Date|Time|Timestamp)?\b/);
  });

  // NFR-025b's enforcement, and the reason it stays closed rather than being closed once: a
  // product file may not render a timestamp with `toLocaleString` and friends, because that is
  // exactly how five call sites came to show a local time with no zone beside a card showing UTC.
  it("has no product file rendering a timestamp through toLocaleString and friends", () => {
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      if (file.endsWith("wallclock.ts") || file.endsWith("wallclock.spec.ts")) continue;
      const text = readFileSync(file, "utf8");
      for (const m of text.matchAll(/toLocale(?:Date|Time)?String\s*\(/g)) {
        offenders.push(`${file.split("/src/")[1]}: ${m[0]}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("has no product call site passing the test-only zone argument", () => {
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      if (file.endsWith("wallclock.ts") || file.endsWith("wallclock.spec.ts")) continue;
      const text = readFileSync(file, "utf8");
      for (const fn of ["instantLabel", "utcCellExtentLabel"]) {
        // a third argument to instantLabel, or a third to the extent label, is the zone
        const re = new RegExp(`${fn}\\(([^()]|\\([^()]*\\))*,\\s*["'\`]`, "g");
        for (const m of text.matchAll(re)) {
          const call = m[0];
          const commas = (call.match(/,/g) ?? []).length;
          if ((fn === "instantLabel" && commas >= 1) || (fn === "utcCellExtentLabel" && commas >= 2)) {
            offenders.push(`${file}: ${call.trim()}`);
          }
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});

// NFR-025b: the two renderings the five legacy call sites needed. Both name the offset, because a
// shorter rendering does not get to drop the part the requirement is about.
describe("instantLabelShort", () => {
  it("drops the seconds and keeps the offset", () => {
    expect(instantLabelShort("2026-09-03T12:55:31Z", "Asia/Yekaterinburg")).toBe("03.09.2026 17:55 (UTC+05:00)");
  });

  it("resolves the offset at the instant here too", () => {
    expect(instantLabelShort("2026-01-15T12:00:00Z", "Europe/Berlin")).toBe("15.01.2026 13:00 (UTC+01:00)");
    expect(instantLabelShort("2026-07-15T12:00:00Z", "Europe/Berlin")).toBe("15.07.2026 14:00 (UTC+02:00)");
  });

  it("is a dash for an absent instant", () => {
    expect(instantLabelShort(null)).toBe("—");
    expect(instantLabelShort("nope")).toBe("—");
  });
});

describe("instantRangeLabel", () => {
  it("names one offset and one date when both ends share them", () => {
    expect(instantRangeLabel("2026-09-03T12:55:00Z", "2026-09-03T13:55:00Z", "Asia/Yekaterinburg")).toBe(
      "03.09.2026 17:55 → 18:55 (UTC+05:00)",
    );
  });

  it("keeps the second date when the window crosses local midnight", () => {
    expect(instantRangeLabel("2026-09-03T18:55:00Z", "2026-09-03T19:55:00Z", "Asia/Yekaterinburg")).toBe(
      "03.09.2026 23:55 → 04.09.2026 00:55 (UTC+05:00)",
    );
  });

  it("names BOTH offsets when the window crosses a DST change, rather than picking one", () => {
    expect(instantRangeLabel("2026-03-29T00:55:00Z", "2026-03-29T01:55:00Z", "Europe/Berlin")).toBe(
      "29.03.2026 01:55 (UTC+01:00) → 29.03.2026 03:55 (UTC+02:00)",
    );
  });

  it("is a dash when either end is absent, so a half-known window is never drawn", () => {
    expect(instantRangeLabel(null, "2026-09-03T13:55:00Z")).toBe("—");
    expect(instantRangeLabel("2026-09-03T12:55:00Z", null)).toBe("—");
  });
});
