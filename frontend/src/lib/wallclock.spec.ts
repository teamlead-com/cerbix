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

  /** Split an argument or parameter list at TOP-LEVEL commas only. */
  function splitTopLevel(text: string): string[] {
    const out: string[] = [];
    let depth = 0, quote = "", cur = "";
    for (let i = 0; i < text.length; i++) {
      const c = text[i];
      if (quote) {
        if (c === quote && text[i - 1] !== "\\") quote = "";
      } else if (c === '"' || c === "'" || c === "`") quote = c;
      else if ("([{<".includes(c)) depth++;
      else if (")]}>".includes(c)) depth--;
      else if (c === "," && depth === 0) { out.push(cur); cur = ""; continue; }
      cur += c;
    }
    if (cur.trim()) out.push(cur);
    return out.filter((x) => x.trim().length > 0);
  }

  /** Every argument list passed to `fn(` in `text`, balanced across nesting and strings. */
  function callArgs(text: string, fn: string): string[] {
    const out: string[] = [];
    // `fn` may be dotted (`ns.instantLabel`), so every regex-special character is escaped. An
    // earlier version turned the dot into a LITERAL BACKSLASH and matched nothing — the failure
    // mode a guard must never have: it passed while guarding zero namespace calls.
    const lit = fn.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const re = new RegExp(`(?<![\\w.])${lit}\\s*\\(`, "g");
    for (const m of text.matchAll(re)) {
      let depth = 1, quote = "", buf = "";
      for (let i = m.index! + m[0].length; i < text.length && depth > 0; i++) {
        const c = text[i];
        if (quote) { if (c === quote && text[i - 1] !== "\\") quote = ""; }
        else if (c === '"' || c === "'" || c === "`") quote = c;
        else if (c === "(") depth++;
        else if (c === ")") { depth--; if (depth === 0) break; }
        buf += c;
      }
      out.push(buf);
    }
    return out;
  }

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

  // A RATCHET, not a claim of completeness. Reviewer P1 at party [195] found what the
  // `toLocaleString` guard above does not reach: timestamps rendered by hand out of a `Date` —
  // `toISOString().slice(0,10)`, `getUTC*`, `get*` — several of them user-visible with no zone at
  // all. NFR-025b substituted the five `toLocaleString` sites; these are a DIFFERENT and larger
  // surface (NFR-025c), and each one needs a decision rather than a substitution: a UTC date must
  // say UTC, a local one must name its offset, an input value must stay bare because the HTML
  // format demands it, and a map key is not rendered at all.
  //
  // So this test does not assert the surface is clean. It asserts the surface is BOUNDED: exactly
  // these files, with exactly these counts. A new file or a new call fails it, and so does a count
  // that shrinks without the list being updated — a stale allow-list is how a ratchet rots.
  it("keeps the hand-rolled date surface bounded, file by file (NFR-025c ratchet)", () => {
    const IDIOM = /toISOString\(\)\.(?:slice|substring)|getUTC(?:Date|Month|FullYear|Hours|Minutes|Seconds)\(|\.get(?:Hours|Minutes|Seconds|Date|Month|FullYear)\(/g;
    // file -> [count, why it is still here]
    const KNOWN: Record<string, [number, string]> = {
      "components/ServiceReliability.vue": [3, "dayLabel builds a segment range as a UTC date, unlabelled — that one needs a UTC LABEL, not a conversion"],
      "components/settings/AgentTokensPanel.vue": [1, "created/revoked as a bare UTC date"],
      "components/settings/MembersPanel.vue": [1, "added as a bare UTC date"],
      "components/settings/SecretsPanel.vue": [1, "created/rotated as a bare UTC date"],
      "lib/changes.ts": [14, "the change timeline's compact clock and date: one rendering already says ` Z`, the rest say nothing — and it defines its OWN `instantLabel(iso, now)`, a name collision with the mechanism that (c) should settle"],
      "lib/changesTimeline.ts": [1, "a same-day comparison whose branch returns a bare clock"],
      "lib/gate.ts": [6, "a bare UTC date, plus a datetime-local INPUT value that must stay offset-free by the HTML format"],
      "lib/gateLedger.ts": [4, "a bare UTC date, plus UTC day boundaries used only for comparison"],
      "views/DashboardView.vue": [2, "day-grid map keys — never rendered"],
      "views/MonitorDetailView.vue": [3, "two day-grid keys (not rendered) plus created/updated as a bare UTC date"],
      "views/PublicStatusView.vue": [4, "one rendering already says ` UTC`, one bare date, two day-grid keys"],
      "views/SettingsView.vue": [5, "a datetime-local INPUT value that must stay offset-free, and its helpers"],
      "views/StatusPagesView.vue": [1, "fmtSubDate: a subscriber date as a bare UTC date"],
    };
    const found: Record<string, number> = {};
    for (const file of walk(SRC)) {
      const rel = file.split("/src/")[1];
      if (rel.startsWith("lib/wallclock") || rel.endsWith(".spec.ts")) continue;
      const n = (readFileSync(file, "utf8").match(IDIOM) ?? []).length;
      if (n > 0) found[rel] = n;
    }
    const expected = Object.fromEntries(Object.entries(KNOWN).map(([f, [n]]) => [f, n]));
    expect(found).toEqual(expected);
  });

  it("has no product call site passing the test-only zone argument, on ANY export that takes one", () => {
    // The list is DERIVED from the module's own signatures, not written by hand. The first version
    // enumerated `instantLabel` and `utcCellExtentLabel`; two exports were added later that also
    // take `zone`, and the guard did not follow them — reviewer P2 at party [199]. A hand list is
    // what rots, so this one reads the parameter position out of the source and a future export
    // taking a `zone` is covered the moment it exists.
    const src = readFileSync(join(SRC, "lib/wallclock.ts"), "utf8");
    const zoneArg: Record<string, number> = {};
    for (const m of src.matchAll(/export function (\w+)\(([\s\S]*?)\):/g)) {
      const params = splitTopLevel(m[2]);
      const i = params.findIndex((prm) => /^zone\??\s*:/.test(prm.trim()));
      if (i >= 0) zoneArg[m[1]] = i;
    }
    // the derivation itself is asserted, or a broken regex would silently guard nothing
    expect(zoneArg).toEqual({
      instantLabel: 1, instantLabelShort: 1, utcCellExtentLabel: 2, instantRangeLabel: 2,
    });

    // IMPORT-AWARE, and it has to be twice over.
    //
    // First, because `lib/changes.ts` defines its OWN `instantLabel(iso, now)` — a different
    // function with the same name whose second argument is a clock reference, not a zone. The first
    // version of this scan flagged its call sites: a false positive AND a real hazard, recorded in
    // the NFR-025c ledger for that file, since the compact clock that shadows the mechanism's name
    // is one of the sites (c) has to decide.
    //
    // Second, because the LOCAL name is what a call site uses. Matching by the exported name let
    // `import { instantLabel as compact }` followed by `compact(ts, "UTC")` walk straight past, and
    // `import * as wallclock` past that — reviewer P2 at party [201]. Aliases are resolved into a
    // local -> exported map, and namespace imports are searched as `ns.fn(`. Neither form exists
    // today; the guard covers them so a later refactor cannot introduce one silently.
    //
    // ITS REMAINING BOUNDARY, stated rather than claimed away: a RE-EXPORT barrel
    // (`export { instantLabel } from "@/lib/wallclock"` in some other module) would put a second
    // hop between the import and the source, and this scan follows one hop. No such barrel exists —
    // every importer above reaches `lib/wallclock` directly — and if one is ever added, this is
    // where it has to be taught about.
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      if (file.endsWith("wallclock.ts") || file.endsWith("wallclock.spec.ts")) continue;
      const text = readFileSync(file, "utf8");
      const WC = /["'][^"']*lib\/wallclock["']/;
      /** what this file calls it -> what the module exports */
      const local: Record<string, string> = {};
      for (const im of text.matchAll(/import\s*(?:type\s+)?\{([^}]*)\}\s*from\s*(["'][^"']*["'])/g)) {
        if (!WC.test(im[2])) continue;
        for (const clause of im[1].split(",")) {
          const [exported, alias] = clause.trim().split(/\s+as\s+/).map((x) => x.trim());
          if (exported) local[alias || exported] = exported;
        }
      }
      for (const im of text.matchAll(/import\s*\*\s*as\s+(\w+)\s*from\s*(["'][^"']*["'])/g)) {
        if (!WC.test(im[2])) continue;
        for (const fn of Object.keys(zoneArg)) local[`${im[1]}.${fn}`] = fn;
      }
      for (const [callee, exported] of Object.entries(local)) {
        const idx = zoneArg[exported];
        if (idx === undefined) continue;
        for (const args of callArgs(text, callee)) {
          if (splitTopLevel(args).length > idx) {
            offenders.push(`${file.split("/src/")[1]}: ${callee}(${args})`);
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
