import { describe, expect, it } from "vitest";

import {
  CHANGE_ERROR_TEXT,
  CHANGE_KINDS,
  CHANGE_RANGE_MAX_DAYS,
  DEFAULT_HORIZON,
  HORIZONS,
  KIND_CLIP,
  NO_TERMINAL_TEXT,
  PHASE_ORDER,
  RANGE_TOO_WIDE_TEXT,
  TERMINAL_PHASES,
  WITHHELD_TEXT,
  changeCode,
  cliRecordLine,
  defaultRange,
  deltaChip,
  deltaText,
  describeChangeFailure,
  formatCompareSide,
  groupKey,
  groupLatest,
  isHorizon,
  isTerminal,
  kindClip,
  kindGlyph,
  kindLabel,
  stripMarksOf,
  terminalOf,
  type ChangeCompareSide,
  type ChangeGroup,
  type ChangePhase,
} from "@/lib/changes";
import type { GateFailure } from "@/lib/gate";
import { transportFailureOf } from "@/lib/gate";

// FR-025 / D-0210: the PURE rules every change surface shares, proven without mounting anything.
// Each block is one of the readings D-0210 makes into the product:
//
//   * a kind is SHAPE + text (item 1) — never a hue, never hidden when the server is newer;
//   * the phases' domain order and the group's identity (items 1, 3): a terminal alone is a valid
//     group, `(source, external_id)` is the only key;
//   * a comparison side is EXACTLY one of three shapes (item 3, D8/D-0211) — a figure, withheld
//     with the page's own reason word, or pending with `sealed_through` STATED and never a partial
//     number;
//   * the Δ chip is coloured by its sign and by nothing else, and is absent when the API sent none;
//   * the horizon vocabulary is closed and the card's range is EXPLICIT RFC3339 (items 1, 5);
//   * a refusal is spelled once (items 1, 3, 5): the code split off `code: rule`, the closed set,
//     401/403, a 429's Retry-After, the transport's own words;
//   * the empty state's CLI line names canonical ids and the LITERAL `CERBIX_TOKEN=…`;
//   * the strip marks are one per TERMINAL phase (item 1, invariant 19).

const PHASE_DEFAULTS = {
  ref: "v4.2.1",
  url: "",
  actor_label: "token:ci",
  actor_user_id: null,
  via_token: true,
  recorded_at: "2026-08-30T14:05:01Z",
};

function phase(id: string, name: ChangePhase["phase"], at: string, over: Partial<ChangePhase> = {}): ChangePhase {
  return { ...PHASE_DEFAULTS, id, phase: name, occurred_at: at, ...over };
}

function group(over: Partial<ChangeGroup> = {}): ChangeGroup {
  const phases = over.phases ?? [phase("p1", "started", "2026-08-30T14:00:00Z"), phase("p2", "succeeded", "2026-08-30T14:05:00Z")];
  return {
    source: "github-actions",
    external_id: "1234",
    kind: "deploy",
    ref: "v4.2.1",
    url: "",
    latest_occurred_at: phases[phases.length - 1]?.occurred_at ?? "2026-08-30T14:05:00Z",
    phases,
    incidents: [],
    ...over,
    ...(over.phases ? { phases: over.phases } : {}),
  };
}

const side = (over: Partial<ChangeCompareSide> = {}): ChangeCompareSide => ({
  from: "2026-08-30T13:05:00Z",
  to: "2026-08-30T14:05:00Z",
  ...over,
});

const failure = (over: Partial<GateFailure> = {}): GateFailure => ({ status: 400, code: "", retryAfter: null, ...over });

describe("changes — a kind is shape and text, never a hue", () => {
  it("the three kinds carry their glyph, their word and their clip-path", () => {
    expect([...CHANGE_KINDS]).toEqual(["deploy", "rollback", "flag"]);
    expect(CHANGE_KINDS.map(kindGlyph)).toEqual(["▲", "▼", "◆"]);
    expect(CHANGE_KINDS.map(kindLabel)).toEqual(["deploy", "rollback", "flag"]);
    for (const k of CHANGE_KINDS) expect(kindClip(k)).toBe(KIND_CLIP[k]);
    // The shapes are distinct: the strip's marks must be told apart without colour.
    expect(new Set(CHANGE_KINDS.map(kindClip)).size).toBe(3);
  });

  it("a kind this SPA does not know (a newer server) is still shown: a square, a plain word", () => {
    expect(kindGlyph("config")).toBe("■");
    expect(kindClip("config")).toBe("none");
    expect(kindLabel("config")).toBe("config");
    expect(kindLabel("")).toBe("change");
  });
});

describe("changes — the phases' domain order and the group's identity", () => {
  it("`started` is not terminal; the three terminals are", () => {
    expect([...PHASE_ORDER]).toEqual(["started", "succeeded", "failed", "cancelled"]);
    expect([...TERMINAL_PHASES]).toEqual(["succeeded", "failed", "cancelled"]);
    expect(isTerminal("started")).toBe(false);
    for (const p of TERMINAL_PHASES) expect(isTerminal(p)).toBe(true);
    expect(isTerminal("rolled-back"), "an unknown phase is not treated as terminal").toBe(false);
  });

  it("terminalOf finds the ONE terminal; a started-only group has none (no mark, no comparison)", () => {
    expect(terminalOf(group())!.phase).toBe("succeeded");
    expect(terminalOf(group({ phases: [phase("p1", "started", "2026-08-30T14:00:00Z")] }))).toBeUndefined();
    // A terminal alone is a valid group (D3, D6).
    expect(terminalOf(group({ phases: [phase("p9", "failed", "2026-08-30T14:00:00Z")] }))!.phase).toBe("failed");
  });

  it("groupLatest is the latest by occurred_at, and the domain's order breaks a tie", () => {
    const g = group({
      phases: [phase("p2", "succeeded", "2026-08-30T14:05:00Z", { actor_label: "token:ci" }), phase("p1", "started", "2026-08-30T14:00:00Z", { actor_label: "alice@example.com" })],
    });
    expect(groupLatest(g)!.id).toBe("p2");
    const tied = group({ phases: [phase("a", "started", "2026-08-30T14:00:00Z"), phase("b", "cancelled", "2026-08-30T14:00:00Z")] });
    expect(groupLatest(tied)!.id, "same instant: the later phase in the domain's order wins").toBe("b");
    expect(groupLatest(group({ phases: [] }))).toBeUndefined();
  });

  it("groupKey is `(source, external_id)` and nothing else — and two identities cannot collide", () => {
    expect(groupKey({ source: "github-actions", external_id: "1234" })).toBe(groupKey({ source: "github-actions", external_id: "1234" }));
    expect(groupKey({ source: "github-actions", external_id: "1234" })).not.toBe(groupKey({ source: "github-actions", external_id: "12345" }));
    // The separator is outside the slug grammar, so `a` + `b-c` never keys the same as `a-b` + `c`.
    expect(groupKey({ source: "a", external_id: "b-c" })).not.toBe(groupKey({ source: "a-b", external_id: "c" }));
  });
});

describe("changes — a comparison side is EXACTLY one of three shapes (D8, D-0211)", () => {
  it("a figure: the percent, the six numbers, and nothing invented", () => {
    const v = formatCompareSide(side({ availability: 99.94, good_seconds: 3597, bad_seconds: 3, unknown_seconds: 0, excluded_seconds: 0, buckets: 60 }));
    expect(v.kind).toBe("figure");
    if (v.kind !== "figure") throw new Error("unreachable");
    expect(v.text).toBe("99.94 %");
    expect([v.good, v.bad, v.unknown, v.excluded, v.buckets]).toEqual([3597, 3, 0, 0, 60]);
  });

  it("every withheld reason is quoted with the PAGE's own word, never a number", () => {
    for (const reason of ["definition_changed", "undecidable", "no_facts"] as const) {
      const v = formatCompareSide(side({ withheld: reason, detail: reason === "undecidable" ? "the page would not decide 41 % of the range" : undefined }));
      expect(v.kind).toBe("withheld");
      if (v.kind !== "withheld") throw new Error("unreachable");
      expect(v.reason).toBe(reason);
      expect(v.text).toBe(`withheld: ${WITHHELD_TEXT[reason]}`);
      expect(v.text).not.toMatch(/\d/);
    }
    expect(formatCompareSide(side({ withheld: "undecidable", detail: "41 % undecided" })).kind).toBe("withheld");
  });

  it("a side that fits no shape (a newer server) is withheld as `unquoted` — a number is never invented", () => {
    const v = formatCompareSide(side({}));
    expect(v).toEqual({ kind: "withheld", text: "withheld: unquoted", reason: "unquoted", detail: "" });
    // A non-finite availability is not a figure either.
    expect(formatCompareSide(side({ availability: Number.NaN }))).toMatchObject({ kind: "withheld", reason: "unquoted" });
  });

  it("pending STATES sealed_through and shows NO partial number — on either side (D-0211)", () => {
    const v = formatCompareSide(side({ pending: true, sealed_through: "2026-08-30T14:00:00Z" }));
    expect(v).toEqual({ kind: "pending", text: "pending", sealedThrough: "2026-08-30T14:00:00Z" });
    expect(v.text).not.toMatch(/\d/);
    // pending wins over a partial figure the server may still have carried: never a partial number.
    const partial = formatCompareSide(side({ pending: true, sealed_through: "2026-08-30T14:00:00Z", availability: 62.5, buckets: 37 }));
    expect(partial.kind).toBe("pending");
    expect(partial.text).toBe("pending");
    // and over `withheld`, so a not-yet-sealed side is never called undecidable (D-0211's whole point).
    expect(formatCompareSide(side({ pending: true, withheld: "undecidable" })).kind).toBe("pending");
    // pending without a watermark says so rather than inventing one.
    expect(formatCompareSide(side({ pending: true }))).toEqual({ kind: "pending", text: "pending", sealedThrough: null });
  });
});

describe("changes — the Δ chip is its SIGN and nothing else", () => {
  it("a rise, a fall, a wash — two decimals, the typographic minus", () => {
    expect(deltaChip(1.664)).toMatchObject({ sign: 1, text: "+1.66" });
    expect(deltaChip(-1.671)).toMatchObject({ sign: -1, text: "−1.67" });
    expect(deltaChip(0)).toMatchObject({ sign: 0, text: "0.00" });
    // Rounds to zero: "0.00", never "−0.00" and never a sign hue.
    expect(deltaChip(-0.001)).toMatchObject({ sign: 0, text: "0.00" });
    expect(deltaText(-0.004)).toBe("0.00");
    const up = deltaChip(2)!;
    const down = deltaChip(-2)!;
    const flat = deltaChip(0)!;
    expect(new Set([up.cls, down.cls, flat.cls]).size, "three signs, three chips").toBe(3);
  });

  it("no delta at all when the API carried none — a withheld or pending side has no Δ", () => {
    expect(deltaChip(undefined)).toBeNull();
    expect(deltaChip(null)).toBeNull();
    expect(deltaChip(Number.NaN)).toBeNull();
    expect(deltaChip(Number.POSITIVE_INFINITY)).toBeNull();
  });
});

describe("changes — the horizons and the card's range", () => {
  it("the vocabulary is closed, ordered, and `1h` is the default", () => {
    expect([...HORIZONS]).toEqual(["15m", "1h", "6h", "24h"]);
    expect(DEFAULT_HORIZON).toBe("1h");
    for (const h of HORIZONS) expect(isHorizon(h)).toBe(true);
    for (const bad of ["30m", "1H", "", "1h ", 3600, null, undefined, ["1h"]]) expect(isHorizon(bad)).toBe(false);
  });

  it("defaultRange is an EXPLICIT half-open RFC3339 pair ending now, of exactly the days asked", () => {
    const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;
    const now = new Date("2026-08-30T14:05:00.000Z");
    const r = defaultRange(30, now);
    expect(r.from).toMatch(RFC3339);
    expect(r.to).toMatch(RFC3339);
    expect(r.to).toBe(now.toISOString());
    expect(Date.parse(r.to) - Date.parse(r.from), "exactly 30 days").toBe(30 * 86_400_000);
    expect(Date.parse(defaultRange(7, now).to) - Date.parse(defaultRange(7, now).from)).toBe(7 * 86_400_000);
    expect(Date.parse(defaultRange(1, now).to) - Date.parse(defaultRange(1, now).from)).toBe(86_400_000);
  });

  it("the range never exceeds D6's 92 days and never runs backwards", () => {
    const now = new Date("2026-08-30T14:05:00.000Z");
    expect(CHANGE_RANGE_MAX_DAYS).toBe(92);
    const wide = defaultRange(365, now);
    expect(Date.parse(wide.to) - Date.parse(wide.from)).toBe(92 * 86_400_000);
    const back = defaultRange(-5, now);
    expect(Date.parse(back.to) - Date.parse(back.from)).toBe(0);
  });
});

describe("changes — one refusal reader (D6, D8, §5a)", () => {
  it("changeCode takes the code off `code`, `code: rule` and `code (field): rule`", () => {
    expect(changeCode("range_too_wide")).toBe("range_too_wide");
    expect(changeCode("range_too_wide: at most 92 days a page")).toBe("range_too_wide");
    expect(changeCode("horizon_invalid (horizon): one of 15m, 1h, 6h, 24h")).toBe("horizon_invalid");
    expect(changeCode("not found")).toBe("not");
    expect(changeCode("")).toBe("");
    expect(changeCode(undefined as unknown as string)).toBe("");
  });

  it("every code of the closed set has its own sentence, and the rendering carries the rule off", () => {
    const codes = Object.keys(CHANGE_ERROR_TEXT);
    expect(codes).toEqual([
      "range_required",
      "range_invalid",
      "range_too_wide",
      "limit_invalid",
      "cursor_invalid",
      "kind_invalid",
      "source_invalid",
      "external_id_invalid",
      "horizon_invalid",
      "no_terminal_phase",
      "process_inflight",
      "principal_inflight",
      "principal_rate",
      "process_rate",
      "change_not_wired",
    ]);
    for (const code of codes) {
      const status = code === "no_terminal_phase" ? 404 : code.endsWith("_rate") || code.endsWith("_inflight") ? 429 : code === "change_not_wired" ? 503 : 400;
      const rendered = describeChangeFailure(failure({ status, code: `${code}: the server's own rule` }));
      expect(rendered, code).toContain(CHANGE_ERROR_TEXT[code]);
      expect(rendered, `${code} must not leak the server's rule text`).not.toContain("the server's own rule");
    }
    expect(CHANGE_ERROR_TEXT.range_too_wide).toBe(RANGE_TOO_WIDE_TEXT);
    expect(NO_TERMINAL_TEXT).toBe("before/after unavailable until a terminal phase");
  });

  it("401 and 403 are one line, the caller's own", () => {
    expect(describeChangeFailure(failure({ status: 401, code: "" }))).toBe("Your session has ended — sign in again.");
    expect(describeChangeFailure(failure({ status: 403, code: "" }), { denied: "You cannot read this service's changes." })).toBe("You cannot read this service's changes.");
    expect(describeChangeFailure(failure({ status: 403, code: "" }))).toBe("You cannot see this.");
  });

  it("429 carries Retry-After in seconds; without one it says 'shortly'", () => {
    expect(describeChangeFailure(failure({ status: 429, code: "principal_inflight", retryAfter: 12 }))).toBe(
      "You already have as many change reads in flight as one principal may. Try again in 12 s.",
    );
    expect(describeChangeFailure(failure({ status: 429, code: "process_rate", retryAfter: null }))).toBe(
      "The server is recording changes faster than the limit allows. Try again shortly.",
    );
    expect(describeChangeFailure(failure({ status: 429, code: "", retryAfter: 3 }))).toBe("Too many requests at once. Try again in 3 s.");
  });

  it("a 404: the closed code's sentence, or the caller's own words for a bare `not found`", () => {
    expect(describeChangeFailure(failure({ status: 404, code: "no_terminal_phase" }))).toBe(CHANGE_ERROR_TEXT.no_terminal_phase);
    expect(describeChangeFailure(failure({ status: 404, code: "not found" }), { notFound: "This change is no longer on the timeline." })).toBe("This change is no longer on the timeline.");
    expect(describeChangeFailure(failure({ status: 404, code: "not found" }))).toBe("not found");
  });

  it("an unfamiliar code is shown verbatim — never swallowed; the transport keeps its own words", () => {
    expect(describeChangeFailure(failure({ status: 400, code: "brand_new_code: a rule this SPA has never seen" }))).toBe("brand_new_code: a rule this SPA has never seen");
    const net = describeChangeFailure(transportFailureOf(new Error("Failed to fetch")));
    expect(net).toBe("Could not reach the server: Failed to fetch");
    expect(describeChangeFailure(transportFailureOf(new Error("")))).toBe("Could not reach the server: network error");
    expect(describeChangeFailure(failure({ status: 500, code: "" }))).toBe("The server answered HTTP 500.");
  });
});

describe("changes — the empty state's CLI line", () => {
  const PROJECT = "0191c2a4-7f3e-4c1b-9a2d-000000000001";
  const SERVICE = "0191c2a4-7f3e-4c1b-9a2d-00000000000f";

  it("names the canonical ids and the LITERAL placeholder — never a token", () => {
    const line = cliRecordLine(PROJECT, SERVICE);
    expect(line).toContain(`--project ${PROJECT}`);
    expect(line).toContain(`--service ${SERVICE}`);
    expect(line).toContain("cerbix change record");
    expect(line).toContain("CERBIX_TOKEN=…");
    // The placeholder is the ONLY thing that follows CERBIX_TOKEN=.
    expect(/CERBIX_TOKEN=(\S+)/.exec(line)![1]).toBe("…");
    expect(line).not.toMatch(/CERBIX_TOKEN=[A-Za-z0-9_.-]/);
    expect(line).not.toContain("CERBIX_URL=");
  });

  it("an origin is prefixed as CERBIX_URL=, before the token placeholder", () => {
    const line = cliRecordLine(PROJECT, SERVICE, "https://cerbix.example.com");
    expect(line.startsWith("CERBIX_URL=https://cerbix.example.com CERBIX_TOKEN=… cerbix change record")).toBe(true);
    expect(/CERBIX_TOKEN=(\S+)/.exec(line)![1]).toBe("…");
  });
});

describe("changes — the strip marks: one per TERMINAL phase (D14, invariant 19)", () => {
  const succeeded = group({ source: "github-actions", external_id: "1", phases: [phase("a1", "started", "2026-08-30T14:00:00Z"), phase("a2", "succeeded", "2026-08-30T14:05:00Z")] });
  const startedOnly = group({ source: "github-actions", external_id: "2", kind: "rollback", ref: "v4.2.0", phases: [phase("b1", "started", "2026-08-30T15:00:00Z")] });
  const failedOnly = group({ source: "argo", external_id: "3", kind: "flag", ref: "", phases: [phase("c1", "failed", "2026-08-30T16:30:00Z")] });

  it("a started-only group contributes NO mark; a terminal one is marked at the TERMINAL's occurred_at", () => {
    const marks = stripMarksOf([succeeded, startedOnly, failedOnly], "", new Date("2026-08-30T17:00:00Z"));
    expect(marks).toHaveLength(2);
    expect(marks.map((m) => m.at)).toEqual(["2026-08-30T14:05:00Z", "2026-08-30T16:30:00Z"]);
    expect(marks.map((m) => m.kind)).toEqual(["deploy", "flag"]);
    expect(marks.map((m) => m.key)).toEqual([groupKey(succeeded), groupKey(failedOnly)]);
    expect(stripMarksOf([startedOnly]), "no terminal phase anywhere: no mark at all").toEqual([]);
    expect(stripMarksOf([])).toEqual([]);
  });

  it("the mark's label names the kind, the ref (or the external id) and the terminal phase", () => {
    const [deploy, flag] = stripMarksOf([succeeded, failedOnly], "", new Date("2026-08-30T17:00:00Z"));
    expect(deploy.label).toContain("deploy · v4.2.1 · succeeded");
    expect(flag.label, "no ref: the external id stands in").toContain("flag · 3 · failed");
  });

  it("exactly the selected key carries the flag; nothing is selected when none is named", () => {
    const marks = stripMarksOf([succeeded, failedOnly], groupKey(failedOnly), new Date("2026-08-30T17:00:00Z"));
    expect(marks.map((m) => m.selected)).toEqual([false, true]);
    expect(stripMarksOf([succeeded, failedOnly], "").every((m) => m.selected === false)).toBe(true);
    expect(stripMarksOf([succeeded, failedOnly], groupKey(startedOnly)).every((m) => m.selected === false)).toBe(true);
  });
});
