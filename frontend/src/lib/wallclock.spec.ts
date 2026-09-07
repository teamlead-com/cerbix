import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, resolve } from "node:path";

import {
  instantLabel, instantLabelShort, instantRangeLabel,
  utcCellExtentLabel, utcClockLabel, utcClockRangeLabel,
  utcCompactInstantLabel, utcDayClockLabel, utcDayLabel, utcDayRangeLabel,
  localInputRangeZoneHint, localInputZoneHint, utcDayInputHint,
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

  /**
   * Strip JS comments from CODE, tracking string context so a `//` inside a literal survives.
   *
   * A regex cannot do this. The first version stripped `//` only at line start, to protect
   * `https://` — and a TRAILING comment then kept a helper's name alive in the stripped text while
   * the element was gone from the DOM: `const noop = 0; // localInputZoneHint(x)` satisfied the
   * inventory with nothing rendered. Reviewer finding N5, and he is right that it is the same class
   * as N4 rather than a style point: the guard reported a hint an operator cannot see.
   *
   * KNOWN BOUNDARY, stated rather than claimed away: a backtick is treated as opening a template
   * literal until the next backtick, so a `${...}` holding another backtick is read
   * conservatively — it keeps MORE text, never less, so it can only cost a false positive in a
   * guard and never hide a missing hint. Regex literals containing `//` are the same shape.
   */
  function stripScriptComments(code: string): string {
    let out = "";
    let i = 0;
    let quote: string | null = null;
    while (i < code.length) {
      const c = code[i];
      const n = code[i + 1];
      if (quote) {
        if (c === "\\") { out += c + (n ?? ""); i += 2; continue; }
        if (c === quote) quote = null;
        out += c; i++; continue;
      }
      if (c === '"' || c === "'" || c === "`") { quote = c; out += c; i++; continue; }
      if (c === "/" && n === "/") { while (i < code.length && code[i] !== "\n") i++; out += " "; continue; }
      if (c === "/" && n === "*") {
        i += 2;
        while (i < code.length && !(code[i] === "*" && code[i + 1] === "/")) i++;
        i += 2; out += " "; continue;
      }
      out += c; i++;
    }
    return out;
  }

  /**
   * `text` with COMMENTS removed, for every scan that reads source.
   *
   * `<!-- … -->` goes everywhere. JS comments go only inside `<script>` blocks, because in a Vue
   * TEMPLATE `//` is not a comment at all — a bare `https://` in visible text would otherwise take
   * the rest of its line with it, and a filter that eats real markup makes every scan quietly
   * weaker.
   */
  function stripComments(text: string): string {
    const noHtml = text.replace(/<!--[\s\S]*?-->/g, " ");
    // A single-file component is decided by EITHER block. Keying only on `<script>` sent a
    // template-only fixture down the code path and ate a bare URL in visible text — the exact
    // damage this split exists to prevent, found by the fixture written for it.
    const isSFC = /<template[\s>]/.test(noHtml) || /<script[\s>]/.test(noHtml);
    if (isSFC) {
      return noHtml.replace(/(<script[^>]*>)([\s\S]*?)(<\/script>)/g,
        (_m, open: string, body: string, close: string) => open + stripScriptComments(body) + close);
    }
    return stripScriptComments(noHtml);
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
    const src = stripComments(readFileSync(join(SRC, "lib/wallclock.ts"), "utf8"));
    const exported = [...src.matchAll(/export function (\w+)/g)].map((m) => m[1]).sort();
    expect(exported).toEqual([
      "instantLabel", "instantLabelShort", "instantRangeLabel",
      "localInputRangeZoneHint", "localInputValue", "localInputZoneHint",
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
        const text = stripComments(readFileSync(f, "utf8")).replace(/import[\s\S]*?from\s*["'][^"']*["'];/g, "");
        return new RegExp(`\\b${fn}\\s*\\(`).test(text);
      });
      expect(callers.length, `${fn} is exported and nothing calls it`).toBeGreaterThan(0);
    }
    // A name like formatDate / formatTime is exactly what a caller reaches for when it has a
    // bucket and wants "a date"; there is deliberately nothing here to reach for.
    expect(src).not.toMatch(/export function format(Date|Time|Timestamp)?\b/);
  });

  // E6 — the extent a per-segment label actually has, to the precision that distinguishes two.
  //
  // The label truncated to whole days, so two reliability segments split by a revision boundary
  // hours apart on one day rendered the identical string and the label could not say which segment
  // a reader was looking at. Definition changes minutes apart are the ordinary case here — the
  // strip beside this label clusters them for exactly that reason.
  //
  // The mutation that must kill this: truncate to days again.
  it("distinguishes two boundaries inside one day, and leaves a whole-day extent alone", () => {
    // Unchanged where nothing is lost: both ends at midnight read as they always did.
    expect(utcDayRangeLabel("2026-09-01T00:00:00Z", "2026-09-03T00:00:00Z"))
      .toBe("01.09.2026 → 03.09.2026 UTC");

    // Two segments of one day, split at 09:30. Under the old label both read
    // "05.09.2026 → 05.09.2026 UTC".
    const first = utcDayRangeLabel("2026-09-05T00:00:00Z", "2026-09-05T09:30:00Z");
    const second = utcDayRangeLabel("2026-09-05T09:30:00Z", "2026-09-06T00:00:00Z");
    expect(first).not.toBe(second);
    expect(first).toContain("09:30");
    expect(second).toContain("09:30");
    // The date is stated once inside a single day, and the zone once for the pair.
    expect(first).toBe("05.09.2026 00:00 → 09:30 UTC");
    expect(second).toBe("05.09.2026 09:30 → 06.09.2026 00:00 UTC");
  });

  // NFR-025b's enforcement, and the reason it stays closed rather than being closed once: a
  // product file may not render a timestamp with `toLocaleString` and friends, because that is
  // exactly how five call sites came to show a local time with no zone beside a card showing UTC.
  it("has no product file rendering a timestamp through toLocaleString and friends", () => {
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      if (file.endsWith("wallclock.ts") || file.endsWith("wallclock.spec.ts")) continue;
      const text = stripComments(readFileSync(file, "utf8"));
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
  // E11 widened this from a CALL SHAPE to the HAZARD.
  //
  // The list above banned the ISO serializer and the field getters, and E1 walked straight past it
  // with a slice of an API string: `sc.anchor_at.slice(0, 16)` produced a `datetime-local` value in
  // no zone at all, which the save path then read as local and moved an on-call rotation anchor by
  // the viewer's offset. Also unbanned and reachable when this was written: the offset getter, the
  // UTC and date string forms, and a direct `Intl.DateTimeFormat`.
  //
  // What is NOT banned, and why: `getTime()`. It yields a NUMBER for arithmetic — a duration, a
  // comparison, a sort key — and nine product modules use it that way. Banning it would ban
  // subtraction, which is not the hazard; deriving a DISPLAYED value from an instant is.
  //
  // The slice rule is narrow on purpose and its boundary is stated rather than implied: it fires
  // when a receiver NAMED like an instant is cut at an RFC-3339 field boundary. A slice with
  // computed bounds, or one whose receiver is named something else, is not caught here — those are
  // what the per-view surface specs and the round-trip assertions exist for, and a guard that
  // pretended otherwise would be the over-claim this package is about.
  it("has no product file building a date by hand — two modules own every one (NFR-025c)", () => {
    const IDIOM = new RegExp([
      /toISOString\s*\(/,
      /getUTC(?:Date|Month|FullYear|Hours|Minutes|Seconds|Day)\s*\(/,
      /\.get(?:Hours|Minutes|Seconds|Date|Month|FullYear|Day)\s*\(/,
      // E11: the routes the shape-based list never named.
      /getTimezoneOffset\s*\(/,
      /\.to(?:UTC|Date|Time)String\s*\(/,
      // WITHOUT the `new`, deliberately. `Intl.DateTimeFormat(...)` is callable as a plain
      // function and returns the same formatter — `Intl.DateTimeFormat("en-GB", { timeZone: "UTC" })
      // .format(d)` prints a wall clock, verified by running it, and the first version of this rule
      // matched only the constructor form (reviewer P1, party [326]). That was a rule about a
      // SPELLING wearing the clothes of a rule about a hazard, which is the defect this guard's own
      // item exists to remove. `Intl.DateTimeFormatOptions` is a TYPE and stays uncaught: the word
      // boundary does not fall between `DateTimeFormat` and `Options`.
      /\bIntl\.DateTimeFormat\b/,
      // and the one E1 used: an instant cut at an RFC-3339 field boundary.
      // The receiver alternatives are anchored at a word boundary on purpose: a bare `ts` is an
      // instant, while the `ts` inside `heartbeats` is the end of a plural and slicing an ARRAY is
      // not this hazard.
      /(?:\b\w*(?:_at|At|[Ii]so|ISO|imestamp)|\bts|\bTs)\s*(?:\?\.)?\.slice\s*\(\s*(?:0\s*,\s*(?:10|16|19)|11\s*,\s*(?:16|19))\s*\)/,
    ].map((r) => r.source).join("|"), "g");
    const OWNERS = ["lib/wallclock.ts", "lib/datekeys.ts"];
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      const rel = file.split("/src/")[1];
      if (OWNERS.includes(rel) || rel.endsWith(".spec.ts")) continue;
      // Comments are stripped here too: a commented-out `toISOString` is not a call, and a
      // guard that reports one teaches its reader to ignore it.
      for (const m of stripComments(readFileSync(file, "utf8")).matchAll(IDIOM)) offenders.push(`${rel}: ${m[0]}`);
    }
    expect(offenders).toEqual([]);

    // The widened rule is pinned against FIXTURES as well as against the tree, because a tree that
    // is already clean cannot tell a working guard from a broken pattern. Each of these is a route
    // that walked past the old list.
    const caught = (src: string) => [...src.matchAll(new RegExp(IDIOM.source, "g"))].length;
    expect(caught('scheduleForm.anchor_at = sc.anchor_at.slice(0, 16);'), "E1's own route").toBe(1);
    expect(caught("const d = ts.slice(0, 19);")).toBe(1);
    expect(caught("const off = new Date().getTimezoneOffset();")).toBe(1);
    expect(caught("el.textContent = d.toUTCString();")).toBe(1);
    expect(caught("el.textContent = d.toDateString();")).toBe(1);
    expect(caught('new Intl.DateTimeFormat("en-GB").format(d);')).toBe(1);
    expect(
      caught('Intl.DateTimeFormat("en-GB", { timeZone: "UTC" }).format(d);'),
      "the same formatter without `new` — executed and it prints a wall clock",
    ).toBe(1);
    // And what it must NOT catch, or it would ban arithmetic and array work rather than rendering.
    expect(caught("const ms = new Date(b).getTime() - new Date(a).getTime();"), "arithmetic").toBe(0);
    expect(caught("heartbeats.slice(0, 12)"), "an array slice").toBe(0);
    expect(caught('digest.slice(0, 16)'), "a digest, not an instant").toBe(0);
    expect(caught("byDay.set(d.day.slice(0, 10), d)"), "a UTC day used as a map key").toBe(0);
    expect(
      caught("const opts: Intl.DateTimeFormatOptions = { hour: \"2-digit\" };"),
      "a type annotation is not a call",
    ).toBe(0);
  });

  // E11's second half: the control inventory matches a LITERAL attribute, so a control whose type
  // is bound would vanish from it — and an inventory that silently shrinks is worse than no
  // inventory, because the exact-map assertion beside it would still pass.
  //
  // The hole is closed by refusing the shape rather than by teaching the matcher to evaluate a
  // binding: a `<input :type="…">` in a product template fails here by file name, and whoever
  // wants one has to decide what the inventory should say about it. None exists today, which is
  // what makes this a guard against a future edit rather than a repair.
  it("has no input whose TYPE is bound, so the control inventory cannot be walked past", () => {
    const offenders: string[] = [];
    for (const file of walk(SRC).filter((f) => f.endsWith(".vue"))) {
      const text = stripComments(readFileSync(file, "utf8"));
      for (const m of text.matchAll(/<input[^>]*?\s(?::type|v-bind:type)\s*=/g)) {
        offenders.push(`${file.split("/src/")[1]}: ${m[0].trim()}`);
      }
    }
    expect(
      offenders,
      "a bound input type is invisible to the control-surface inventory, which counts a literal attribute",
    ).toEqual([]);
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
    const src = stripComments(readFileSync(join(SRC, "lib/wallclock.ts"), "utf8"));
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
      const text = stripComments(readFileSync(join(SRC, rel), "utf8"));
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
    const src = stripComments(readFileSync(join(SRC, "lib/wallclock.ts"), "utf8"));
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
  // says which zone it is typing in" — and every one of them carried nothing but "Starts", "Ends",
  // "Until", "from", "From" or "To", including the one the specification named. The only mention of
  // a zone anywhere near them was a source comment. Reviewer P1 on the NFR-025 contract audit.
  // No count is written in this comment: the first one said six and there were eight.
  //
  // So the exemption is a CHECK now: a file that renders one of these controls must render the
  // matching hint at least as many times as it has controls. A seventh input added tomorrow with
  // no hint fails here by file name, which is what an allow-list of views would not do.
  // The comment filter is itself a mechanism, so it is pinned rather than trusted. Two dangers,
  // opposite in direction: it must remove what an operator cannot see, and it must NOT remove code
  // that only LOOKS like a comment — a `https://` in a template, a glob in a doc comment — because
  // a filter that eats real source makes every scan above quietly weaker.
  it("removes comments and nothing else", () => {
    expect(stripComments('<!-- {{ localInputZoneHint(x) }} -->')).not.toContain("localInputZoneHint");
    expect(stripComments("/* localInputZoneHint(x) */")).not.toContain("localInputZoneHint");
    expect(stripComments("  // localInputZoneHint(x)")).not.toContain("localInputZoneHint");
    // A TRAILING comment is a comment. The first version kept it, to protect `https://`, and that
    // left `const noop = 0; // localInputZoneHint(x)` satisfying the inventory with nothing in the
    // DOM — reviewer finding N5.
    expect(stripComments("const noop = 0; // localInputZoneHint(x)")).not.toContain("localInputZoneHint");
    expect(stripComments("const a = 1; // trailing")).toContain("const a = 1;");
    // NOT a comment: a URL inside a string literal, in code...
    expect(stripComments('const u = "https://example.test/x";')).toContain("https://example.test/x");
    expect(stripComments("const u = 'https://example.test/x';")).toContain("https://example.test/x");
    // ...and a bare URL in TEMPLATE text, where `//` is not a comment marker at all.
    expect(stripComments("<template><a>https://example.test/x</a>{{ localInputZoneHint(v) }}</template>"))
      .toContain("localInputZoneHint");
    // Applied to the real tree it must change NO count that any scan above depends on. This is
    // the assertion that a stricter filter cannot quietly shrink the inventory.
    const PATTERNS: [string, RegExp][] = [
      ["datetime-local", /type="datetime-local"/g],
      ["date", /type="date"/g],
      ["hint", /(?<!Range)localInputZoneHint\(/g],
      ["range", /localInputRangeZoneHint\(/g],
      ["utcDay", /utcDayInputHint\(/g],
    ];
    const changed: string[] = [];
    for (const file of walk(SRC).filter((f) => f.endsWith(".vue"))) {
      const raw = readFileSync(file, "utf8");
      const stripped = stripComments(raw);
      for (const [name, pat] of PATTERNS) {
        const a = (raw.match(pat) ?? []).length;
        const b = (stripped.match(pat) ?? []).length;
        if (a !== b) changed.push(`${file.split("/src/")[1]}: ${name} ${a} -> ${b}`);
      }
    }
    expect(changed, "the comment filter changed a count on the real tree").toEqual([]);
  });

  it("gives every zone-free INPUT a surface that names the zone it is read in (NFR-025)", () => {
    const CONTROLS: [RegExp, RegExp, string][] = [
      [/type="datetime-local"/g, /(?<!Range)localInputZoneHint\(/g, "localInputZoneHint"],
      [/type="date"/g, /utcDayInputHint\(/g, "utcDayInputHint"],
    ];
    const offenders: string[] = [];
    const inventory: Record<string, Record<string, number>> = {};
    let controls = 0;
    // Only `.vue` files RENDER a control; a `.ts` module that quotes `type="date"` in a comment
    // explaining the contract is documentation, and flagging it would teach a reader that the
    // guard cries wolf.
    for (const file of walk(SRC).filter((f) => f.endsWith(".vue"))) {
      const rel = file.split("/src/")[1];
      const text = stripComments(readFileSync(file, "utf8"));
      // Controls live in the TEMPLATE and so must their hints — counted inside `{{ … }}`
      // interpolations rather than anywhere in the file. A helper's NAME in a live string literal
      // satisfied the file-wide count while the DOM had no hint at all (reviewer N6): the lexer
      // correctly preserves that string, and a name preserved is not a call rendered. This is a
      // narrowing of what the SOURCE guard claims; what an operator actually sees is proven by the
      // per-view surface specs, which is the only evidence that can settle it.
      const template = (/<template[^>]*>([\s\S]*)<\/template>/.exec(text) ?? ["", ""])[1];
      const rendered = [...template.matchAll(/\{\{([\s\S]*?)\}\}/g)].map((m) => m[1]).join(" ");
      // A RANGE hint covers exactly two controls — `starts → ends` is one subject, and two
      // identical offsets in one inline row is noise rather than honesty. It counts for two
      // because it READS both values and names both offsets when they differ; the first version
      // of this rule accepted a single-instant hint beside a `data-covers="2"` attribute, which
      // is a declaration that one label covers two controls and no evidence that it says the
      // right thing about the second. Reviewer P1: `data-covers` was a permission slip.
      const ranges = (rendered.match(/localInputRangeZoneHint\(/g) ?? []).length;
      for (const [control, hint, name] of CONTROLS) {
        const inputs = (template.match(control) ?? []).length;
        if (inputs === 0) continue;
        controls += inputs;
        const kind = name === "utcDayInputHint" ? "date" : "datetime-local";
        inventory[rel] = { ...(inventory[rel] ?? {}), [kind]: inputs };
        // a range hint reads two values, so it answers for two controls
        const hints = (rendered.match(hint) ?? []).length + (name === "localInputZoneHint" ? ranges * 2 : 0);
        if (hints < inputs) {
          offenders.push(`${rel}: ${inputs} control(s), ${hints} covered by ${name}() (a range hint counts for two)`);
        }
      }
    }
    expect(offenders).toEqual([]);

    // THE EXACT INVENTORY, not a floor. `toBeGreaterThan(8)` stood here and it is the same evasion
    // as a hand-written total: it stays green while the prose beside it claims a number the scan
    // never checked — which is precisely how "six controls across five views" survived when there
    // were eight. Reviewer P1, twice in two commits, on the same class.
    //
    // A per-file map rather than one number, because one number cannot say WHERE it changed. A new
    // control fails this even when it is correctly hinted: adding one is a decision, and a decision
    // that no one has to make is one nobody records.
    expect(inventory).toEqual({
      "components/ServiceGate.vue": { "datetime-local": 1 },
      "views/EscalationView.vue": { "datetime-local": 3 },
      "views/GateDecisionsView.vue": { date: 2 },
      "views/ServiceChangesView.vue": { date: 2 },
      "views/ServiceDeclarationView.vue": { "datetime-local": 1 },
      "views/SettingsView.vue": { "datetime-local": 1 },
      "views/SlaView.vue": { "datetime-local": 2 },
    });
    expect(controls, "the totals disagree with the inventory above").toBe(12);

    // EVERY control file has a SURFACE assertion, named here, and the map is checked against the
    // inventory rather than written beside it. A source guard cannot prove a render — reviewer N6
    // reached it with the helper's name in a live string literal — so the guarantee rests on these
    // specs, and this is what stops a new control file shipping without one.
    const SURFACES: Record<string, string> = {
      "components/ServiceGate.vue": "components/ServiceGate.spec.ts",
      "views/EscalationView.vue": "views/EscalationZoneHints.spec.ts",
      "views/GateDecisionsView.vue": "views/GateDecisionsView.spec.ts",
      "views/ServiceChangesView.vue": "views/ServiceChangesView.spec.ts",
      "views/ServiceDeclarationView.vue": "views/ZoneFreeControlSurfaces.spec.ts",
      "views/SettingsView.vue": "views/ZoneFreeControlSurfaces.spec.ts",
      "views/SlaView.vue": "views/SlaView.spec.ts",
    };
    expect(Object.keys(SURFACES).sort()).toEqual(Object.keys(inventory).sort());
    for (const [control, spec] of Object.entries(SURFACES)) {
      const text = readFileSync(join(SRC, spec), "utf8");
      expect(text, `${spec} does not assert a zone for ${control}`).toMatch(/-zone"|local time \(UTC|UTC days/);
    }
  });

  // A RANGE hint counts for two controls because it READS two values — so every call site must
  // actually pass two DIFFERENT ones. Passing the start twice restores exactly the state the
  // reviewer refused (`data-covers` beside a single-instant hint) while still satisfying the
  // counting guard above, and no surface test can catch it in a zone with no DST in the window
  // under test — which is most zones, most of the year. This one does, in every zone.
  it("passes two distinct values to every range zone hint", () => {
    const offenders: string[] = [];
    let calls = 0;
    for (const file of walk(SRC).filter((f) => f.endsWith(".vue"))) {
      const rel = file.split("/src/")[1];
      const text = stripComments(readFileSync(file, "utf8"));
      // `callArgs` walks balanced parentheses; a regex stops at the first `)` of a nested call,
      // and every real call site here contains one (`ovDraft(s.id ?? '').starts_at`).
      for (const raw of callArgs(text, "localInputRangeZoneHint")) {
        calls++;
        const args = splitTopLevel(raw).map((a) => a.trim());
        if (args.length !== 2) {
          offenders.push(`${rel}: localInputRangeZoneHint takes two ends, got ${args.length}`);
        } else if (args[0] === args[1]) {
          offenders.push(`${rel}: both ends are \`${args[0]}\` — the second control is labelled with the first one's instant`);
        }
      }
    }
    expect(calls, "no range hint call site found; the scan is looking wrong").toBeGreaterThan(0);
    expect(offenders).toEqual([]);
  });

  it("names two owner modules that exist and that really do build dates", () => {
    for (const rel of ["lib/wallclock.ts", "lib/datekeys.ts"]) {
      const text = stripComments(readFileSync(join(SRC, rel), "utf8"));
      expect(text, `${rel} is not where dates are built`).toMatch(/toISOString\s*\(|getUTC\w+\s*\(|\.get(?:Hours|Minutes|Date|Month|FullYear)\s*\(/);
    }
  });

  it("has no product call site passing the test-only zone argument, on ANY export that takes one", () => {
    // The list is DERIVED from the module's own signatures, not written by hand. The first version
    // enumerated `instantLabel` and `utcCellExtentLabel`; two exports were added later that also
    // take `zone`, and the guard did not follow them — reviewer P2 at party [199]. A hand list is
    // what rots, so this one reads the parameter position out of the source and a future export
    // taking a `zone` is covered the moment it exists.
    const src = stripComments(readFileSync(join(SRC, "lib/wallclock.ts"), "utf8"));
    const zoneArg: Record<string, number> = {};
    for (const m of src.matchAll(/export function (\w+)\(([\s\S]*?)\):/g)) {
      const params = splitTopLevel(m[2]);
      const i = params.findIndex((prm) => /^zone\??\s*:/.test(prm.trim()));
      if (i >= 0) zoneArg[m[1]] = i;
    }
    // the derivation itself is asserted, or a broken regex would silently guard nothing
    expect(zoneArg).toEqual({
      instantLabel: 1, instantLabelShort: 1, utcCellExtentLabel: 2, instantRangeLabel: 2,
      localInputValue: 1, localInputZoneHint: 1, localInputRangeZoneHint: 2,
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
      const text = stripComments(readFileSync(file, "utf8"));
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

  it("names BOTH offsets when a range crosses a DST change, and one when it does not", () => {
    // The case `data-covers="2"` could not answer: End is entered at a different offset from
    // Start, and a hint computed from Start alone tells the operator the wrong thing about it.
    expect(localInputRangeZoneHint("2026-03-28T12:00", "2026-03-30T12:00", "Europe/Berlin"))
      .toBe("local time (UTC+01:00 → UTC+02:00)");
    expect(localInputRangeZoneHint("2026-03-28T09:00", "2026-03-28T17:00", "Europe/Berlin"))
      .toBe("local time (UTC+01:00)");
    // A half-entered range still answers, for the end that exists and for now on the one that
    // does not — a field with no value has no instant, and guessing one would be the same lie.
    expect(localInputRangeZoneHint("", "", "Europe/Berlin"))
      .toMatch(/^local time \(UTC[+-]\d{2}:\d{2}\)$/);
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
