import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, resolve } from "node:path";

import {
  instantLabel, instantLabelShort, instantRangeLabel,
  utcCellExtentLabel, utcClockLabel, utcClockRangeLabel,
  utcCompactInstantLabel, utcDayClockLabel, utcDayLabel, utcDayRangeLabel,
  localInputZoneHint, utcDayInputHint,
  utcExtentLabel, utcInstantLabel, utcMillisLabel, utcSecondsLabel,
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
      "instantLabel", "instantLabelShort", "instantRangeLabel", "localInputZoneHint",
      "utcCellExtentLabel", "utcClockLabel", "utcClockRangeLabel",
      "utcCompactInstantLabel", "utcDayClockLabel", "utcDayInputHint", "utcDayLabel",
      "utcDayRangeLabel", "utcExtentLabel", "utcInstantLabel", "utcMillisLabel",
      "utcSecondsLabel",
    ]);
    // Every one of them has a caller. `clockSecondsLabel` in `lib/changes.ts` was exported,
    // documented as the honest one because it ended in ` Z`, and called by nothing in the
    // repository; its replacement here was deleted rather than kept for symmetry.
    for (const fn of exported) {
      const callers = walk(SRC).filter((f) => {
        if (f.endsWith("wallclock.ts") || f.endsWith(".spec.ts")) return false;
        // Import statements are stripped FIRST. Matching the bare name counted an unused import
        // as a caller, which is exactly the state a dropped call site leaves behind — the
        // mutation that removed `utcMillisLabel`'s only call survived on its own import line.
        const text = readFileSync(f, "utf8").replace(/import[\s\S]*?from\s*["'][^"']*["'];/g, "");
        return new RegExp(`\\b${fn}\\s*\\(`).test(text);
      });
      expect(callers.length, `${fn} is exported and nothing calls it`).toBeGreaterThan(0);
    }
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

  // NFR-025c's enforcement, and it is a GUARD now rather than a ratchet.
  //
  // The previous version was an allow-list: thirteen files with a count and a reason each,
  // failing on a new file, a new call, or a count that shrank without the list being updated. It
  // was honest about being a bound rather than a fix — and it had a hole exactly where an
  // allow-list has one. Its idiom required the slice to be CHAINED onto the call, so
  // `phaseInstantLabel` in `lib/changesTimeline.ts`, which did
  //
  //     const s = d.toISOString(); ... s.slice(11, 16)
  //
  // and rendered `08-28 16:40` to an operator with no zone at all, was never counted. It was
  // found by reading, not by the guard that existed to make reading unnecessary.
  //
  // So the rule is absolute and needs no list: OUTSIDE `lib/wallclock.ts` (renderings) and
  // `lib/datekeys.ts` (keys, wire values and control values), no product file calls
  // `toISOString` or pulls a field off a `Date` at all. There is nothing left to enumerate, and
  // an indirection cannot walk past it because the call itself is what is banned.
  it("has no product file building a date by hand — two modules own every one (NFR-025c)", () => {
    const IDIOM = /toISOString\s*\(|getUTC(?:Date|Month|FullYear|Hours|Minutes|Seconds|Day)\s*\(|\.get(?:Hours|Minutes|Seconds|Date|Month|FullYear|Day)\s*\(/g;
    const OWNERS = ["lib/wallclock.ts", "lib/datekeys.ts"];
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      const rel = file.split("/src/")[1];
      if (OWNERS.includes(rel) || rel.endsWith(".spec.ts")) continue;
      for (const m of readFileSync(file, "utf8").matchAll(IDIOM)) offenders.push(`${rel}: ${m[0]}`);
    }
    expect(offenders).toEqual([]);
  });

  // The guard above is only worth its comment if the two owners really are reachable — a typo in
  // OWNERS would exempt nothing and the test would still be green while the product was clean by
  // accident. This asserts the opposite direction: both owners DO contain the banned idiom, so
  // the exemption is load-bearing and the paths are real.
  // The UTC subjects take no `zone` parameter ON PURPOSE — their subject IS UTC — which means
  // nothing in a behaviour test can distinguish "pinned to UTC" from "the runner happens to sit at
  // UTC". So the property is asserted at the SOURCE, and the selector is the SIGNATURE rather than
  // the name: a renderer that takes `zone` is local by contract (`utcCellExtentLabel` renders a UTC
  // cell in the viewer's zone, which is its entire point), and a renderer that does not take one
  // may never hand `partsAt` anything but the literal "UTC". Dropping that argument — the obvious
  // refactor, since `partsAt`'s zone is optional — fails here by name.
  it("pins UTC in every zone-less renderer, so none of them inherits the runner's zone", () => {
    const src = readFileSync(join(SRC, "lib/wallclock.ts"), "utf8");
    const bodies = [...src.matchAll(/export function (\w+)\(([\s\S]*?)\):[\s\S]*?\n\}/g)];
    const offenders: string[] = [];
    let checked = 0;
    for (const m of bodies) {
      if (/\bzone\??\s*:/.test(m[2])) continue; // local by contract
      const calls = [...m[0].matchAll(/partsAt\(([^)]*)\)/g)];
      if (calls.length === 0) continue; // delegates, or reads the ISO string directly
      checked++;
      for (const c of calls) {
        if (!/,\s*"UTC"\s*,/.test(c[1])) offenders.push(`${m[1]}: partsAt(${c[1]})`);
      }
    }
    expect(checked, "the scan checked no function — it is looking at the wrong shape").toBeGreaterThan(4);
    expect(offenders, "a zone-less renderer that does not pin UTC renders in whatever zone the viewer has").toEqual([]);
  });

  // The three 90-day strips shared one shape and one defect: `label: key`, where `key` is a bare
  // `YYYY-MM-DD` used both to look a row up and to fill a tooltip. The guard above cannot see it —
  // `utcDayKey` is a legitimate call — so the three builders are held to the split explicitly, and
  // `MonitorDetailView.spec.ts` proves the rendering reaches a reader on one of them.
  it("splits the day strips' lookup key from what their tooltip shows", () => {
    for (const rel of ["views/DashboardView.vue", "views/MonitorDetailView.vue", "views/PublicStatusView.vue"]) {
      const text = readFileSync(join(SRC, rel), "utf8");
      expect(text, `${rel} no longer builds a day strip — this guard has to move`).toContain("utcDayBefore(today, i)");
      expect(text, `${rel} shows its lookup key instead of a labelled day`).toMatch(/label:\s*utcDayLabel\(/);
    }
  });

  // THE SUFFIX RULE, as a call rather than as a sentence.
  //
  // Every zone-less renderer is called with one real instant and must end in ` UTC` (a phrase a
  // person reads) or in `Z` (a canonical instant an engineer pastes into a log query). Which
  // bucket each one is in is ENUMERATED, so a new renderer cannot be added without landing in one
  // — and the enumeration is checked against the module's own exports in both directions, so a
  // rename cannot leave a stale entry behind.
  //
  // The prose in `wallclock.ts` claimed a single ` UTC` suffix and carved out `utcInstantLabel`
  // alone; `utcSecondsLabel` and `utcMillisLabel` were added beside it and the sentence did not
  // follow. Reviewer finding on `0a1557c..d0a8fed`. It is a test now.
  it("gives every UTC renderer one of exactly two suffixes, and says which", () => {
    const A = "2026-08-28T14:03:02.417Z";
    const B = "2026-08-29T15:04:03.000Z";
    // Each renderer is CALLED, with the arguments its own signature takes, and placed in a bucket.
    const PHRASE: Record<string, () => string> = {
      utcClockLabel: () => utcClockLabel(A),
      utcClockRangeLabel: () => utcClockRangeLabel(A, B),
      utcCompactInstantLabel: () => utcCompactInstantLabel(A, new Date(B)),
      utcDayClockLabel: () => utcDayClockLabel(A),
      utcDayLabel: () => utcDayLabel(A),
      utcDayRangeLabel: () => utcDayRangeLabel(A, B),
    };
    const CANONICAL: Record<string, () => string> = {
      utcExtentLabel: () => utcExtentLabel(A, B),
      utcInstantLabel: () => utcInstantLabel(A),
      utcMillisLabel: () => utcMillisLabel(A),
      utcSecondsLabel: () => utcSecondsLabel(A),
    };
    // A third form, and it is a form rather than an exception: a CONTROL hint renders no time at
    // all. It names the zone an `<input>` whose value carries none is read in, so what it must
    // contain is the zone — not a suffix on a rendered instant.
    const CONTROL: Record<string, () => string> = {
      utcDayInputHint: () => utcDayInputHint(),
    };

    // The two buckets ARE the module's zone-less exports, no more and no less — so a new renderer
    // fails here until somebody decides which form it is in, and a rename cannot leave a stale
    // entry behind.
    const src = readFileSync(join(SRC, "lib/wallclock.ts"), "utf8");
    const zoneless = [...src.matchAll(/export function (\w+)\(([\s\S]*?)\):/g)]
      .filter((m) => !/\bzone\??\s*:/.test(m[2]))
      .map((m) => m[1])
      .sort();
    expect(zoneless).toEqual(
      [...Object.keys(PHRASE), ...Object.keys(CANONICAL), ...Object.keys(CONTROL)].sort());

    for (const [name, call] of Object.entries(PHRASE)) {
      const out = call();
      expect(out, `${name} -> ${out}`).toMatch(/ UTC$/);
    }
    for (const [name, call] of Object.entries(CANONICAL)) {
      const out = call();
      expect(out, `${name} -> ${out}`).toMatch(/Z$/);
      expect(out, `${name} must not claim both forms`).not.toMatch(/ UTC$/);
    }
    for (const [name, call] of Object.entries(CONTROL)) {
      const out = call();
      expect(out, `${name} -> ${out} names no zone`).toMatch(/UTC/);
    }
  });

  // THE CONTROL-SURFACE CONTRACT, which §9 asserted and no surface implemented.
  //
  // An `<input type="datetime-local">` value is local and offset-free, and an `<input type="date">`
  // value is read by `lib/gateLedger.ts` as a UTC calendar day. Neither can carry a suffix. §9
  // recorded that as a documented exemption with the justification "the surface that owns the input
  // says which zone it is typing in" — and SIX datetime-local controls across five views, plus FOUR
  // date controls across two, carried nothing but "Starts", "Ends", "Until", "from", "From", "To".
  // Including the one the specification named. The only mention of a zone anywhere near them was a
  // source comment. Reviewer P1 on the NFR-025 contract audit.
  //
  // So the exemption is a CHECK now: a file that renders one of these controls must render the
  // matching hint at least as many times as it has controls. A seventh input added tomorrow with
  // no hint fails here by file name, which is what an allow-list of five views would not do.
  it("gives every zone-free INPUT a surface that names the zone it is read in (NFR-025)", () => {
    const CONTROLS: [RegExp, RegExp, string][] = [
      [/type="datetime-local"/g, /localInputZoneHint\(/g, "localInputZoneHint"],
      [/type="date"/g, /utcDayInputHint\(/g, "utcDayInputHint"],
    ];
    const offenders: string[] = [];
    let controls = 0;
    // Only `.vue` files RENDER a control; a `.ts` module that quotes `type="date"` in a comment
    // explaining the contract is documentation, and flagging it would teach a reader that the
    // guard cries wolf.
    for (const file of walk(SRC).filter((f) => f.endsWith(".vue"))) {
      const rel = file.split("/src/")[1];
      const text = readFileSync(file, "utf8");
      // One hint may honestly cover a RANGE — `starts → ends` is one subject, and two identical
      // offsets in one inline row is noise, not honesty. A hint that covers more than its own
      // control says so with `data-covers="N"`, which keeps the count strict: a seventh input
      // still needs either its own hint or an explicit, visible decision to be covered.
      const covers = [...text.matchAll(/data-covers="(\d+)"/g)]
        .reduce((n, m) => n + Number(m[1]) - 1, 0);
      for (const [control, hint, name] of CONTROLS) {
        const inputs = (text.match(control) ?? []).length;
        if (inputs === 0) continue;
        controls += inputs;
        const hints = (text.match(hint) ?? []).length + covers;
        if (hints < inputs) {
          offenders.push(`${rel}: ${inputs} control(s), ${hints} ${name}() call(s) incl. data-covers`);
        }
      }
    }
    expect(controls, "the scan found no zone-free controls at all; it is looking wrong")
      .toBeGreaterThan(8);
    expect(offenders).toEqual([]);
  });

  it("names two owner modules that exist and that really do build dates", () => {
    for (const rel of ["lib/wallclock.ts", "lib/datekeys.ts"]) {
      const text = readFileSync(join(SRC, rel), "utf8");
      expect(text, `${rel} is not where dates are built`).toMatch(/toISOString\s*\(|getUTC\w+\s*\(|\.get(?:Hours|Minutes|Date|Month|FullYear)\s*\(/);
    }
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
      localInputZoneHint: 1,
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

// ── The UTC subjects (NFR-025c) ─────────────────────────────────────────────────────────────────
//
// These render subjects whose identity IS UTC. The rule the specification states is that such a
// site SAYS UTC rather than being converted to the viewer's zone — converting it is the boundary
// lie, and leaving it silent, which is what thirteen files did, is that lie with the evidence
// removed. So every case below asserts the SUFFIX as much as the digits, and each one is checked
// from a non-UTC runner zone would give the same answer because the zone is pinned in the call.

describe("the UTC subjects say UTC", () => {
  it("renders a UTC calendar day as a fact, and says whose day it is", () => {
    expect(utcDayLabel("2026-09-05T12:04:31Z")).toBe("05.09.2026 UTC");
  });

  it("keeps the UTC day even for an instant that is already the NEXT day in a viewer's zone", () => {
    // 23:30Z on the 5th is 04:30 on the 6th at UTC+05. The old code sliced the ISO string and got
    // the UTC day right by accident while saying nothing; this gets it right and says so.
    expect(utcDayLabel("2026-09-05T23:30:00Z")).toBe("05.09.2026 UTC");
  });

  it("names the suffix once for a range of UTC days, not twice", () => {
    expect(utcDayRangeLabel("2026-08-01T00:00:00Z", "2026-08-05T00:00:00Z")).toBe(
      "01.08.2026 → 05.08.2026 UTC",
    );
  });

  it("renders a UTC clock and a UTC clock range", () => {
    expect(utcClockLabel("2026-09-05T14:05:12Z")).toBe("14:05 UTC");
    expect(utcClockRangeLabel("2026-09-05T13:05:00Z", "2026-09-05T14:05:00Z")).toBe("13:05 → 14:05 UTC");
  });

  it("renders a UTC day-and-clock without the year, for a phase read against the one before it", () => {
    expect(utcDayClockLabel("2026-08-28T16:40:00Z")).toBe("28.08 16:40 UTC");
  });

  it("quotes a seal to the second and a snapshot to the millisecond, both canonical", () => {
    expect(utcSecondsLabel("2026-08-28T14:03:02.417Z")).toBe("2026-08-28 14:03:02Z");
    expect(utcMillisLabel("2026-08-28T14:03:02.417Z")).toBe("2026-08-28 14:03:02.417Z");
  });

  it("gives the compact instant three branches against a UTC now, and a zone in all three", () => {
    const now = new Date("2026-08-28T20:00:00Z");
    expect(utcCompactInstantLabel("2026-08-28T16:40:00Z", now)).toBe("16:40 UTC");
    expect(utcCompactInstantLabel("2026-08-27T16:40:00Z", now)).toBe("27.08 16:40 UTC");
    expect(utcCompactInstantLabel("2025-08-27T16:40:00Z", now)).toBe("27.08.2025 16:40 UTC");
  });

  it("compares the compact instant's day in UTC, not in the runner's zone", () => {
    // 23:00Z and 01:00Z the next day are one UTC day apart. A local-day comparison at UTC+05
    // would call them the same day and drop the date from the label.
    const now = new Date("2026-08-29T01:00:00Z");
    expect(utcCompactInstantLabel("2026-08-28T23:00:00Z", now)).toBe("28.08 23:00 UTC");
  });

  it("tells an operator which zone a datetime-local control is read in, AT the typed instant", () => {
    // Late March in Europe/Berlin: the same control, two offsets, depending on what was typed.
    expect(localInputZoneHint("2026-03-28T12:00", "Europe/Berlin")).toBe("local time (UTC+01:00)");
    expect(localInputZoneHint("2026-03-30T12:00", "Europe/Berlin")).toBe("local time (UTC+02:00)");
    // An empty or half-typed control has no instant, so it answers for now rather than guessing.
    expect(localInputZoneHint("", "Europe/Berlin")).toMatch(/^local time \(UTC[+-]\d{2}:\d{2}\)$/);
    expect(localInputZoneHint("not-a-date", "Europe/Berlin")).toMatch(/^local time \(UTC[+-]\d{2}:\d{2}\)$/);
  });

  it("says a date control is read in UTC days, the same answer for every viewer", () => {
    expect(utcDayInputHint()).toBe("UTC days");
  });

  it("returns the absent marker rather than a fabricated date for every UTC subject", () => {
    for (const v of [null, undefined, "", "not-a-date"]) {
      expect(utcDayLabel(v)).toBe("—");
      expect(utcClockLabel(v)).toBe("—");
      expect(utcDayClockLabel(v)).toBe("—");
      expect(utcSecondsLabel(v)).toBe("—");
      expect(utcMillisLabel(v)).toBe("—");
      expect(utcCompactInstantLabel(v)).toBe("—");
    }
    expect(utcDayRangeLabel("2026-08-01T00:00:00Z", null)).toBe("—");
    expect(utcClockRangeLabel(null, "2026-08-01T00:00:00Z")).toBe("—");
  });
});
