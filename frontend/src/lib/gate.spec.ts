import { afterEach, describe, expect, it, vi } from "vitest";

import {
  CHIP_DORM,
  CHIP_DOWN,
  CHIP_WARN,
  CLAUSES_MESSAGE,
  GATE_ERROR_TEXT,
  LEDGER_MAX_RANGE_DAYS,
  RANGE_TOO_WIDE_TEXT,
  SEAL_LAG_MAX_MESSAGE,
  SEAL_LAG_MIN_MESSAGE,
  SEAL_LAG_WHOLE_MESSAGE,
  THRESHOLD_MESSAGE,
  WINDOW_LOST_MESSAGE,
  WINDOW_MESSAGE,
  budgetOfDecision,
  cliCommand,
  createTemplate,
  describeFailure,
  draftToBody,
  failureOf,
  isConflict,
  ledgerRange,
  parseRetryAfter,
  reasonTone,
  reasonToneClass,
  shortId,
  statePill,
  templateWindow,
  transportFailureOf,
  validateClauses,
  validateDraft,
  validateSealLagMinutes,
  validateThreshold,
  validateWindow,
  type GateReason,
} from "@/lib/gate";

// FR-024 / D-0207: the ONE owner of every rendering rule the gate surfaces share. These are the
// rules the reviewer's five checks rest on, provable without mounting anything: the validators
// mirror the server's refusals; the ledger range is explicit and half-open; the CLI command names
// canonical ids and a literal token placeholder; a refusal is spelled from one code→message map.

const DAY_MS = 86_400_000;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,3})?Z$/;

describe("gate validators mirror the server's rules", () => {
  it("seal lag: the 300 s floor, the 24 h ceiling, whole minutes only", () => {
    expect(validateSealLagMinutes(4), "4 min = 240 s is below the floor").toBe(SEAL_LAG_MIN_MESSAGE);
    expect(SEAL_LAG_MIN_MESSAGE).toContain("300 s");
    expect(validateSealLagMinutes(5), "5 min = 300 s is the floor itself").toBe("");
    expect(validateSealLagMinutes("15")).toBe("");
    expect(validateSealLagMinutes(1440), "1440 min = 86400 s is the ceiling itself").toBe("");
    expect(validateSealLagMinutes(1441), "over 86400 s").toBe(SEAL_LAG_MAX_MESSAGE);
    expect(validateSealLagMinutes("2.5"), "a fraction of a minute").toBe(SEAL_LAG_WHOLE_MESSAGE);
    expect(validateSealLagMinutes("abc")).toBe(SEAL_LAG_WHOLE_MESSAGE);
    expect(validateSealLagMinutes(""), "an empty field reads as below the floor, with the floor sentence").toBe(SEAL_LAG_MIN_MESSAGE);
  });

  it("threshold: a whole number from 1 to 100", () => {
    expect(validateThreshold(0)).toBe(THRESHOLD_MESSAGE);
    expect(validateThreshold(101)).toBe(THRESHOLD_MESSAGE);
    expect(validateThreshold(1.5)).toBe(THRESHOLD_MESSAGE);
    expect(validateThreshold("90")).toBe("");
    expect(validateThreshold(1)).toBe("");
    expect(validateThreshold(100)).toBe("");
    expect(validateThreshold("")).toBe(THRESHOLD_MESSAGE);
  });

  it("clauses: every fact must be assigned", () => {
    const full = createTemplate(["30d"]).clauses;
    expect(validateClauses(full)).toBe("");
    const { service_incident_open: _dropped, ...missing } = full;
    expect(validateClauses(missing)).toBe(CLAUSES_MESSAGE);
    expect(validateClauses({ ...full, budget_consumed: "maybe" as never })).toBe(CLAUSES_MESSAGE);
  });

  it("window: only the inventory; a stored window that LEFT the inventory is its own message", () => {
    expect(validateWindow("30d", ["30d", "7d"])).toBe("");
    expect(validateWindow("7d", ["30d"]), "a window the service has no target for").toBe(WINDOW_MESSAGE);
    expect(validateWindow("", ["30d"]), "nothing picked").toBe(WINDOW_MESSAGE);
    expect(validateWindow("7d", ["30d"], "7d"), "the stored window lost its target").toBe(WINDOW_LOST_MESSAGE);
    expect(validateWindow("24h", ["30d"], "7d"), "a different missing window is the ordinary message").toBe(WINDOW_MESSAGE);
    expect(validateWindow("30d", [], "30d"), "an empty inventory with the stored window is the lost message").toBe(WINDOW_LOST_MESSAGE);
  });

  it("validateDraft reports every field at once and is empty for the template", () => {
    const ok = createTemplate(["30d"]);
    expect(validateDraft(ok, ["30d"])).toEqual({});
    const bad = { ...ok, window: "7d" as const, threshold: "0", sealLagMinutes: "4" };
    expect(validateDraft(bad, ["30d"], "7d")).toEqual({
      window: WINDOW_LOST_MESSAGE,
      threshold: THRESHOLD_MESSAGE,
      "seal-lag": SEAL_LAG_MIN_MESSAGE,
    });
  });
});

describe("the create template", () => {
  it("prefers 30d when the service has a target for it", () => {
    expect(templateWindow(["7d", "30d", "90d"])).toBe("30d");
    expect(createTemplate(["90d", "30d"]).window).toBe("30d");
  });
  it("falls back to the canonical FIRST window with a target, never one without", () => {
    expect(templateWindow(["90d", "7d"]), "canonical order is 24h, 7d, 30d, 90d").toBe("7d");
    expect(templateWindow(["90d"])).toBe("90d");
    expect(templateWindow([]), "no inventory: nothing picked, so the form cannot save").toBe("");
  });
  it("ships the mock's defaults and turns into the WHOLE document", () => {
    const d = createTemplate(["30d"]);
    expect(d.clauses).toEqual({
      budget_exhausted: "block",
      budget_consumed: "warn",
      page_burn_firing: "block",
      ticket_burn_firing: "warn",
      service_incident_open: "warn",
    });
    expect(draftToBody(d, null)).toEqual({
      expected_revision: null,
      schema_version: 1,
      window: "30d",
      clauses: d.clauses,
      budget_consumed_percent: 90,
      max_seal_lag_seconds: 900,
      unknown_behavior: "warn",
    });
    expect(draftToBody({ ...d, sealLagMinutes: "15", threshold: 95 }, 3)).toMatchObject({
      expected_revision: 3,
      max_seal_lag_seconds: 900,
      budget_consumed_percent: 95,
    });
  });
});

describe("ledgerRange", () => {
  it("is an explicit RFC3339 pair spanning exactly 30 days, `to` = now", () => {
    const now = new Date("2026-08-29T12:34:56.789Z");
    const { from, to } = ledgerRange(now);
    expect(from).toMatch(RFC3339);
    expect(to).toMatch(RFC3339);
    expect(to).toBe(now.toISOString());
    expect(Date.parse(to) - Date.parse(from)).toBe(30 * DAY_MS);
    expect(Date.parse(from) < Date.parse(to), "half-open: from strictly before to").toBe(true);
  });
  it("never asks for more than the server's 31-day cap", () => {
    const now = new Date("2026-08-29T00:00:00Z");
    const { from, to } = ledgerRange(now, 90);
    expect(Date.parse(to) - Date.parse(from)).toBe(LEDGER_MAX_RANGE_DAYS * DAY_MS);
  });
});

describe("cliCommand", () => {
  it("names the canonical ids and the LITERAL token placeholder, and carries no token text", () => {
    const cmd = cliCommand("https://cerbix.example", "p-123", "s-456");
    expect(cmd).toBe("CERBIX_URL=https://cerbix.example CERBIX_TOKEN=… cerbix gate check --project p-123 --service s-456");
    expect(cmd).toMatch(/CERBIX_TOKEN=… cerbix gate check/);
    expect(cmd).not.toMatch(/Bearer|token:|eyJ/);
  });
});

describe("statePill", () => {
  it("says the five answers, dashed for deficient evidence and for 'a different kind of answer'", () => {
    expect(statePill("ALLOW")).toMatchObject({ label: "ALLOW", dashed: false });
    expect(statePill("WARN")).toMatchObject({ label: "WARN", dashed: false });
    expect(statePill("BLOCK")).toMatchObject({ label: "BLOCK", dashed: false });
    expect(statePill("UNKNOWN")).toMatchObject({ label: "UNKNOWN", dashed: true });
    expect(statePill("NOT_CONFIGURED")).toMatchObject({ label: "not configured", dashed: true });
    expect(statePill("UNKNOWN").cls).toContain("border-dashed");
    expect(statePill("NOT_CONFIGURED").cls).toContain("border-dashed");
    expect(statePill("ALLOW").cls).not.toContain("border-dashed");
  });
  it("renders an unfamiliar state as deficient evidence with its own label — never as ALLOW", () => {
    const p = statePill("SOMETHING_NEW");
    expect(p.label).toBe("SOMETHING_NEW");
    expect(p.dashed).toBe(true);
    expect(p.cls).toBe(statePill("UNKNOWN").cls);
  });
});

describe("shortId", () => {
  it("keeps the first eight and the last four of a uuid, and a short id whole", () => {
    expect(shortId("0191c2a4-7f3e-4c1b-9a2d-000000005b04")).toBe("0191c2a4…5b04");
    expect(shortId("abc")).toBe("abc");
    expect(shortId("1234567890123"), "13 characters is the last length left whole").toBe("1234567890123");
  });
});

describe("failureOf / describeFailure", () => {
  afterEach(() => vi.useRealTimers());

  it("spells the three 409 codes with the mock's sentences, in and out of their contexts", () => {
    const f409 = (code: string) => failureOf({ error: { error: code }, response: new Response(null, { status: 409 }) });
    expect(describeFailure(f409("revision_conflict"), { context: "save" })).toBe(GATE_ERROR_TEXT.revision_conflict);
    expect(GATE_ERROR_TEXT.revision_conflict).toContain("changed while you were editing");
    expect(describeFailure(f409("override_active"), { context: "override" })).toBe(GATE_ERROR_TEXT.override_active);
    expect(GATE_ERROR_TEXT.override_active).toContain("One override at a time");
    expect(describeFailure(f409("override_not_active"), { context: "revoke" })).toBe(GATE_ERROR_TEXT.override_not_active);
    expect(GATE_ERROR_TEXT.override_not_active).toContain("no longer active");
    // The delete dialog's and the override form's 409 are their own sentences (mock screens 2/3).
    expect(describeFailure(f409("revision_conflict"), { context: "delete" })).toContain("while this dialog was open");
    expect(describeFailure(f409("revision_conflict"), { context: "override" })).toContain("cannot be created");
    for (const code of ["revision_conflict", "override_active", "override_not_active"]) {
      expect(isConflict(f409(code)), `${code} blocks until Reload`).toBe(true);
    }
    expect(isConflict(failureOf({ error: { error: "not_configured" }, response: new Response(null, { status: 404 }) }))).toBe(false);
  });

  it("reads a 429's Retry-After in seconds and says so", () => {
    const f = failureOf({
      error: { error: "process_inflight" },
      response: new Response(null, { status: 429, headers: { "Retry-After": "7" } }),
    });
    expect(f).toEqual({ status: 429, code: "process_inflight", retryAfter: 7 });
    expect(describeFailure(f, { context: "ledger" })).toBe("The ledger is busy right now. Try again in 7 s.");
    const bare = failureOf({ error: { error: "" }, response: new Response(null, { status: 429 }) });
    expect(describeFailure(bare)).toBe("Too many requests at once. Try again shortly.");
  });

  it("reads an HTTP-date Retry-After as the seconds until then", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-29T12:00:00Z"));
    expect(parseRetryAfter("Sat, 29 Aug 2026 12:00:30 GMT")).toBe(30);
    expect(parseRetryAfter("Sat, 29 Aug 2026 11:00:00 GMT"), "a date already past is at least one second").toBe(1);
    expect(parseRetryAfter("not a date")).toBeNull();
    expect(parseRetryAfter(null)).toBeNull();
    const f = failureOf({
      error: { error: "principal_inflight" },
      response: new Response(null, { status: 429, headers: { "Retry-After": "Sat, 29 Aug 2026 12:00:30 GMT" } }),
    });
    expect(describeFailure(f)).toBe(`${GATE_ERROR_TEXT.principal_inflight} Try again in 30 s.`);
  });

  it("401 and 403 are one line each, 403 taking the caller's words", () => {
    const f401 = failureOf({ error: { error: "unauthorized" }, response: new Response(null, { status: 401 }) });
    expect(describeFailure(f401)).toBe("Your session has ended — sign in again.");
    const f403 = failureOf({ error: { error: "forbidden" }, response: new Response(null, { status: 403 }) });
    expect(describeFailure(f403)).toBe("You cannot see this.");
    expect(describeFailure(f403, { denied: "You cannot read this service's gate." })).toBe("You cannot read this service's gate.");
  });

  it("a network failure is the transport's own words, verbatim", () => {
    const f = transportFailureOf(new Error("ECONNREFUSED 127.0.0.1:8080"));
    expect(f.status).toBe(0);
    expect(describeFailure(f)).toBe("Could not reach the server: ECONNREFUSED 127.0.0.1:8080");
    expect(describeFailure(transportFailureOf(new Error("")))).toBe("Could not reach the server: network error");
  });

  it("range_too_wide is the mock's sentence; an unknown code is shown verbatim, never swallowed", () => {
    const wide = failureOf({ error: { error: "range_too_wide" }, response: new Response(null, { status: 400 }) });
    expect(describeFailure(wide, { context: "ledger" })).toBe(RANGE_TOO_WIDE_TEXT);
    const odd = failureOf({ error: { error: "something_the_client_never_heard_of" }, response: new Response(null, { status: 400 }) });
    expect(describeFailure(odd)).toBe("something_the_client_never_heard_of");
    const silent = failureOf({ error: {}, response: new Response(null, { status: 503 }) });
    expect(describeFailure(silent)).toBe("The server answered HTTP 503.");
  });

  it("tolerates a missing response (a test double) without throwing", () => {
    expect(failureOf({ error: { error: "x" } })).toEqual({ status: 0, code: "x", retryAfter: null });
  });
});

describe("the reason tone rule (D4)", () => {
  const matchedBlock: GateReason = { code: "budget_exhausted", clause: "budget_exhausted", assignment: "block", value: 100 };
  const matchedWarn: GateReason = { code: "budget_consumed", clause: "budget_consumed", assignment: "warn", value: 96.4 };
  const staleWithAssignment: GateReason = { code: "seal_stale", clause: "budget_exhausted", assignment: "block" };
  const whole: GateReason = { code: "not_configured" };
  const ignored: GateReason = { code: "service_incident_open", clause: "service_incident_open", assignment: "ignore", value: "inc1" };

  it("a matched clause under block reads down, under warn reads degraded", () => {
    expect(reasonTone(matchedBlock)).toBe("down");
    expect(reasonToneClass(matchedBlock)).toBe(CHIP_DOWN);
    expect(reasonTone(matchedWarn)).toBe("warn");
    expect(reasonToneClass(matchedWarn)).toBe(CHIP_WARN);
  });
  it("an unavailability keeps its clause's assignment but is drawn dashed — it decided nothing", () => {
    expect(reasonTone(staleWithAssignment)).toBe("dorm");
    expect(reasonToneClass(staleWithAssignment)).toBe(CHIP_DORM);
    expect(CHIP_DORM).toContain("border-dashed");
    expect(reasonTone(whole)).toBe("dorm");
    expect(reasonTone(ignored)).toBe("dorm");
  });
  it("the budget KPI is the value the decision quoted, or its withholding, or nothing", () => {
    expect(budgetOfDecision([matchedWarn])).toEqual({ reason: matchedWarn, percent: 96.4 });
    expect(budgetOfDecision([staleWithAssignment])).toEqual({ reason: staleWithAssignment });
    expect(budgetOfDecision([whole])).toBeNull();
    expect(budgetOfDecision([])).toBeNull();
  });
});
