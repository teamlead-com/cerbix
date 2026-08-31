import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import ServiceChanges from "@/components/ServiceChanges.vue";
import { CARD_PAGE_SIZE, NO_TERMINAL_TEXT, groupKey } from "@/lib/changes";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), PUT: vi.fn(), POST: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: {}, query: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' },
}));

// FR-025 / AC-0165-7, mock screen 1 as D-0210 item 1 reads it: the `Changes` card. What is proven:
//
//   * it is a RECORD, not a control (D14): no test in this file ever sees a POST/PUT/DELETE — the
//     file-wide afterEach below asserts it, so a future "record here" button breaks every test;
//   * the list is ONE explicit RFC3339 range fixed at load with `limit: 10`; "Show 10 more" reuses
//     the SAME pair with the cursor (D6) — the afterEach checks every request;
//   * a comparison is asked for TERMINAL groups only, at the header's horizon, by identity; a
//     started-only group is never asked and says `before/after unavailable until a terminal phase`;
//   * moving the horizon ABORTS what is in flight and re-issues every row at the new horizon;
//   * the marks are EMITTED (one per terminal phase) and cleared on unmount — the card draws none;
//   * the decision cell is the ledger's live state/action or `aged out` (D11); `preceded`, with the
//     lag and the incident link, never "caused" (D7);
//   * every refusal renders: 401/403 as one line with no rows, 429 with the Retry-After seconds, a
//     network failure in the transport's own words; an unmount aborts the reads in flight.

const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,3})?Z$/;
const DAY_MS = 86_400_000;
const PROJECT = "0191c2a4-7f3e-4c1b-9a2d-000000000001";
const SERVICE = "0191c2a4-7f3e-4c1b-9a2d-00000000000f";
const INCIDENT = "0191dddd-1111-4000-8000-0000000000ab";
const DECISION = "0191c2a4-7f3e-4c1b-9a2d-000000005b04";

type Res = { data?: unknown; error?: unknown; response?: Response };
type Answer = Res | Promise<Res> | ((opts: any) => Res | Promise<Res>);
const ok = (data: unknown): Res => ({ data, response: new Response(null, { status: 200 }) });
const refused = (status: number, code: string, headers?: Record<string, string>): Res => ({
  error: { error: code },
  response: new Response(null, { status, headers }),
});
const page = (items: unknown[], next_cursor: string | null = null) => ok({ items, next_cursor });

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => (resolve = res));
  return { promise, resolve };
}

const PHASE_BASE = { ref: "v4.2.1", url: "", actor_label: "token:ci", actor_user_id: null, via_token: true, recorded_at: "2026-08-30T14:05:01Z" };
const phase = (id: string, name: string, at: string, over: Record<string, unknown> = {}) => ({ ...PHASE_BASE, id, phase: name, occurred_at: at, ...over });

/** A terminal group: `started` then `succeeded`. */
function done(external_id: string, over: Record<string, unknown> = {}) {
  const phases = [phase(`${external_id}-a`, "started", "2026-08-30T14:00:00Z"), phase(`${external_id}-b`, "succeeded", "2026-08-30T14:05:00Z")];
  return { source: "github-actions", external_id, kind: "deploy", ref: "v4.2.1", url: "", latest_occurred_at: "2026-08-30T14:05:00Z", phases, incidents: [], ...over };
}
/** A started-only group: no mark, no comparison. */
function running(external_id: string, over: Record<string, unknown> = {}) {
  const phases = [phase(`${external_id}-a`, "started", "2026-08-30T15:00:00Z", { ref: "v4.3.0" })];
  return { source: "github-actions", external_id, kind: "rollback", ref: "v4.3.0", url: "", latest_occurred_at: "2026-08-30T15:00:00Z", phases, incidents: [], ...over };
}

const sideFigure = (availability: number, buckets = 60) => ({
  from: "2026-08-30T13:05:00Z",
  to: "2026-08-30T14:05:00Z",
  availability,
  good_seconds: 3597,
  bad_seconds: 3,
  unknown_seconds: 0,
  excluded_seconds: 0,
  buckets,
});
const sidePending = () => ({ from: "2026-08-30T14:05:00Z", to: "2026-08-30T15:05:00Z", pending: true, sealed_through: "2026-08-30T14:20:00Z" });

function compareBody(over: Record<string, unknown> = {}) {
  return {
    source: "github-actions",
    external_id: "1",
    kind: "deploy",
    ref: "v4.2.1",
    change_id: "1-b",
    terminal_phase: "succeeded",
    t: "2026-08-30T14:05:00Z",
    horizon: "1h",
    sealed_through: "2026-08-30T16:00:00Z",
    as_of: "2026-08-30T16:01:00Z",
    before: sideFigure(99.94),
    after: sideFigure(98.28),
    delta: -1.66,
    ...over,
  };
}

interface Server {
  list?: Answer;
  compare?: Answer;
}
/** Answers by PATH: the list and the comparison are different endpoints and must not share an answer. */
function serve(server: Server) {
  const pick = (a: Answer | undefined, fallback: Res, opts: unknown) => {
    if (a === undefined) return Promise.resolve(fallback);
    if (typeof a === "function") return Promise.resolve(a(opts));
    return Promise.resolve(a);
  };
  apiMock.GET.mockImplementation((path: string, opts: unknown) => {
    if (path.endsWith("/changes/compare")) return pick(server.compare, ok(compareBody()), opts);
    if (path.endsWith("/changes")) return pick(server.list, page([]), opts);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function mountCard(props: Record<string, unknown> = {}) {
  return mount(ServiceChanges, {
    props: { projectId: PROJECT, serviceId: SERVICE, serviceSlug: "checkout", ...props },
    global: { stubs: { RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' } } },
  });
}

async function settle() {
  await flushPromises();
  await flushPromises();
  await flushPromises();
}

type W = ReturnType<typeof mountCard>;
const t = (w: W, id: string) => w.find(`[data-testid="${id}"]`);
const all = (w: W, id: string) => w.findAll(`[data-testid="${id}"]`);
const has = (w: W, id: string) => t(w, id).exists();
const listCalls = () => apiMock.GET.mock.calls.filter((c) => String(c[0]).endsWith("/changes"));
const compareCalls = () => apiMock.GET.mock.calls.filter((c) => String(c[0]).endsWith("/changes/compare"));
const rows = (w: W) => all(w, "changes-group");
const cellOf = (row: ReturnType<typeof rows>[number]) => row.find('[data-testid="changes-compare"]');
const marksOf = (w: W) => (w.emitted("marks") ?? []) as unknown[][];
const lastMarks = (w: W) => (marksOf(w).at(-1)?.[0] ?? []) as { at: string; kind: string; key?: string }[];

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.PUT.mockReset();
  apiMock.POST.mockReset();
  apiMock.DELETE.mockReset();
});

// D14, over EVERY test in this file: the card is a record, not a control. And every read it makes
// names its range explicitly, carries the card's page size and rides an AbortSignal.
afterEach(() => {
  expect(apiMock.POST, "the card never records a change — the record is the pipeline's (D14)").not.toHaveBeenCalled();
  expect(apiMock.PUT).not.toHaveBeenCalled();
  expect(apiMock.DELETE).not.toHaveBeenCalled();
  for (const c of listCalls()) {
    const q = c[1].params.query;
    expect(q.from, "every list request names its range").toMatch(RFC3339);
    expect(q.to).toMatch(RFC3339);
    expect(Date.parse(q.to) - Date.parse(q.from)).toBeGreaterThan(0);
    expect(Date.parse(q.to) - Date.parse(q.from), "never wider than D6's 92 days").toBeLessThanOrEqual(92 * DAY_MS);
    expect(q.limit).toBe(CARD_PAGE_SIZE);
    expect(c[1].signal).toBeInstanceOf(AbortSignal);
  }
  for (const c of compareCalls()) expect(c[1].signal).toBeInstanceOf(AbortSignal);
});

describe("ServiceChanges — the empty state (mock screen 1)", () => {
  it("nothing recorded: the sentence names the service and the range, and the CLI line names canonical ids and the LITERAL token placeholder", async () => {
    serve({ list: page([]) });
    const w = mountCard();
    await settle();
    expect(has(w, "changes-empty")).toBe(true);
    expect(t(w, "changes-empty").text()).toContain("checkout");
    expect(t(w, "changes-empty").text()).toContain("last 30 days");
    const cli = t(w, "changes-cli").text();
    expect(cli).toContain("cerbix change record");
    expect(cli).toContain(`--project ${PROJECT}`);
    expect(cli).toContain(`--service ${SERVICE}`);
    expect(/CERBIX_TOKEN=(\S+)/.exec(cli)![1], "the placeholder, never a value from the session").toBe("…");
    expect(has(w, "changes-group"), "no rows").toBe(false);
    expect(t(w, "changes-count").text()).toContain("· 0");
    expect(compareCalls(), "nothing to compare").toHaveLength(0);
    expect(lastMarks(w), "no marks").toEqual([]);
  });

  it("the first request is the default 30 days ending NOW, limit 10, no cursor", async () => {
    serve({ list: page([]) });
    mountCard();
    await settle();
    expect(listCalls()).toHaveLength(1);
    const q = listCalls()[0][1].params.query;
    expect(Date.parse(q.to) - Date.parse(q.from)).toBe(30 * DAY_MS);
    expect(Math.abs(Date.parse(q.to) - Date.now()), "`to` is now").toBeLessThan(5_000);
    expect(q.cursor).toBeUndefined();
    expect(listCalls()[0][1].params.path).toEqual({ projectID: PROJECT, serviceID: SERVICE });
  });
});

describe("ServiceChanges — the rows (D-0210 item 1)", () => {
  it("a terminal group and a started-only one: the phases, the identity, the actor, and `data-terminal` only where there is one", async () => {
    serve({ list: page([done("1"), running("2")]) });
    const w = mountCard();
    await settle();
    const r = rows(w);
    expect(r).toHaveLength(2);
    expect(r[0].attributes("data-terminal")).toBe("succeeded");
    expect(r[0].attributes("data-source")).toBe("github-actions");
    expect(r[0].attributes("data-external-id")).toBe("1");
    expect(r[0].attributes("data-kind")).toBe("deploy");
    expect(r[1].attributes("data-terminal"), "a started-only group has no terminal phase").toBeUndefined();
    expect(r[1].attributes("data-kind")).toBe("rollback");

    expect(r[0].findAll('[data-testid="changes-phase"]').map((p) => p.attributes("data-phase"))).toEqual(["started", "succeeded"]);
    expect(r[1].findAll('[data-testid="changes-phase"]').map((p) => p.attributes("data-phase"))).toEqual(["started"]);
    expect(r[0].find('[data-testid="changes-source"]').text()).toBe("github-actions · 1");
    expect(r[0].find('[data-testid="changes-actor"]').text()).toBe("token:ci");
    expect(r[0].find('[data-testid="changes-actor"]').attributes("title")).toBe("an API token");
    expect(r[0].find('[data-testid="changes-kind"]').text()).toBe("deploy");
    expect(t(w, "changes-count").attributes("data-count")).toBe("2");
  });

  it("the actor is the LATEST phase's, and phases by different people are named in the title", async () => {
    const g = done("1", {
      phases: [
        phase("1-a", "started", "2026-08-30T14:00:00Z", { actor_label: "alice@example.com", via_token: false }),
        phase("1-b", "succeeded", "2026-08-30T14:05:00Z", { actor_label: "token:ci" }),
      ],
    });
    serve({ list: page([g]) });
    const w = mountCard();
    await settle();
    expect(t(w, "changes-actor").text()).toBe("token:ci");
    expect(t(w, "changes-actor").attributes("title")).toBe("phases by alice@example.com, token:ci");
  });
});

describe("ServiceChanges — the decision a change rested on (D11)", () => {
  it("no decision: no cell at all", async () => {
    serve({ list: page([done("1")]) });
    const w = mountCard();
    await settle();
    expect(has(w, "changes-decision")).toBe(false);
  });

  it("a live decision: the ledger's state, and the override chip only when it was overridden", async () => {
    serve({ list: page([done("1", { decision: { decision_id: DECISION, state: "BLOCK", action: "ALLOW", overridden: true } }), done("2", { decision: { decision_id: DECISION, state: "ALLOW", action: "ALLOW" } })]) });
    const w = mountCard();
    await settle();
    const cells = all(w, "changes-decision");
    expect(cells).toHaveLength(2);
    expect(cells[0].attributes("data-state")).toBe("BLOCK");
    expect(cells[0].attributes("data-decision-id")).toBe(DECISION);
    expect(cells[0].attributes("data-aged-out")).toBeUndefined();
    expect(cells[0].find('[data-testid="changes-decision-override"]').text()).toBe("override → ALLOW");
    expect(cells[1].attributes("data-state")).toBe("ALLOW");
    expect(cells[1].find('[data-testid="changes-decision-override"]').exists(), "not overridden: no chip").toBe(false);
  });

  it("a decision the ledger has aged out is shown by id and said so — no invented state", async () => {
    serve({ list: page([done("1", { decision: { decision_id: DECISION, aged_out: true } })]) });
    const w = mountCard();
    await settle();
    const cell = t(w, "changes-decision");
    expect(cell.attributes("data-aged-out")).toBe("true");
    expect(cell.attributes("data-state")).toBeUndefined();
    expect(cell.text()).toContain("aged out");
    expect(cell.text()).toContain("0191c2a4…5b04");
  });
});

describe("ServiceChanges — `preceded`, never caused (D7)", () => {
  it("the lag, the role and a link to the incident", async () => {
    const g = done("1", { incidents: [{ incident_id: INCIDENT, opened_at: "2026-08-30T14:31:00Z", role: "own_service", lag_seconds: 1560, change_id: "1-b" }] });
    serve({ list: page([g]) });
    const w = mountCard();
    await settle();
    const p = t(w, "changes-preceded");
    expect(p.attributes("data-incident-id")).toBe(INCIDENT);
    expect(p.attributes("data-role")).toBe("own_service");
    expect(p.attributes("data-lag")).toBe("1560");
    expect(p.text()).toContain("preceded");
    expect(p.text()).toContain("−26 m");
    expect(p.text()).not.toContain("caused");
    expect(JSON.parse(p.find("a").attributes("data-to")!)).toEqual({ name: "incident", params: { id: INCIDENT } });
  });

  it("an upstream link is marked a probable root", async () => {
    const g = done("1", { incidents: [{ incident_id: INCIDENT, opened_at: "2026-08-30T14:05:30Z", role: "upstream", lag_seconds: 30, change_id: "1-b" }] });
    serve({ list: page([g]) });
    const w = mountCard();
    await settle();
    expect(t(w, "changes-preceded").text()).toContain("upstream · probable root");
    expect(t(w, "changes-preceded").text()).toContain("−30 s");
  });
});

describe("ServiceChanges — before/after: terminal groups only, at the header's horizon (D8)", () => {
  it("exactly one comparison per TERMINAL group; the started-only row is never asked and says so", async () => {
    const items = [done("1"), running("2"), done("3"), running("4"), done("5")];
    const terminals = items.filter((g) => g.phases.some((p) => p.phase !== "started"));
    serve({ list: page(items), compare: (o: any) => ok(compareBody({ external_id: o.params.query.external_id })) });
    const w = mountCard();
    await settle();
    expect(compareCalls(), "one per terminal group, none for the rest").toHaveLength(terminals.length);
    expect(compareCalls().map((c) => c[1].params.query.external_id).sort()).toEqual(["1", "3", "5"]);

    const r = rows(w);
    expect(cellOf(r[1]).attributes("data-state")).toBe("no-terminal");
    expect(cellOf(r[1]).text()).toBe(NO_TERMINAL_TEXT);
    expect(cellOf(r[1]).text(), "no partial number on a group that has none").not.toMatch(/\d/);
    expect(cellOf(r[1]).find('[data-testid="changes-compare-link"]').exists(), "and no link to a comparison that would 404").toBe(false);
    expect(cellOf(r[0]).attributes("data-state")).toBe("ok");
  });

  it("the request is by IDENTITY at the default horizon 1h; the figures, the Δ and the link render", async () => {
    serve({ list: page([done("1")]), compare: ok(compareBody()) });
    const w = mountCard();
    await settle();
    expect(compareCalls()).toHaveLength(1);
    expect(compareCalls()[0][1].params.query).toEqual({ source: "github-actions", external_id: "1", horizon: "1h" });
    expect(compareCalls()[0][1].params.path).toEqual({ projectID: PROJECT, serviceID: SERVICE });
    expect(t(w, "changes-compare").attributes("data-horizon")).toBe("1h");
    expect(t(w, "changes-compare-before").text()).toBe("99.94 %");
    expect(t(w, "changes-compare-after").text()).toBe("98.28 %");
    expect(t(w, "changes-compare-delta").text()).toBe("−1.66");
    expect(t(w, "changes-compare-delta").attributes("data-sign")).toBe("-1");
    expect(JSON.parse(t(w, "changes-compare-link").attributes("data-to")!)).toEqual({
      name: "service-change-compare",
      params: { id: SERVICE },
      query: { source: "github-actions", external_id: "1", horizon: "1h" },
    });
  });

  it("a pending side states the seal and shows no partial number; no Δ without two figures", async () => {
    serve({ list: page([done("1")]), compare: ok(compareBody({ after: sidePending(), delta: undefined })) });
    const w = mountCard();
    await settle();
    expect(t(w, "changes-compare-after").attributes("data-kind")).toBe("pending");
    expect(t(w, "changes-compare-after").text()).toBe("pending");
    expect(has(w, "changes-compare-delta"), "one side is not a figure: no Δ").toBe(false);
  });

  it("a withheld side is the page's own reason word", async () => {
    serve({ list: page([done("1")]), compare: ok(compareBody({ before: { from: "x", to: "y", withheld: "definition_changed" }, delta: undefined })) });
    const w = mountCard();
    await settle();
    expect(t(w, "changes-compare-before").attributes("data-kind")).toBe("withheld");
    expect(t(w, "changes-compare-before").text()).toBe("withheld: definition changed");
  });

  it("a comparison that fails leaves its row's cell alone — the other rows still get their figures", async () => {
    serve({
      list: page([done("1"), done("2")]),
      compare: (o: any) => (o.params.query.external_id === "1" ? refused(404, "no_terminal_phase") : ok(compareBody({ external_id: "2" }))),
    });
    const w = mountCard();
    await settle();
    const r = rows(w);
    expect(cellOf(r[0]).attributes("data-state")).toBe("failed");
    expect(cellOf(r[0]).find('[data-testid="changes-compare-error"]').text()).toBe(
      "This change has no terminal phase yet — before/after is unavailable until one is recorded.",
    );
    expect(cellOf(r[1]).attributes("data-state")).toBe("ok");
  });

  it("moving the horizon ABORTS the comparisons in flight and re-issues EVERY row at the new one", async () => {
    const gates: Record<string, ReturnType<typeof deferred<Res>>> = {};
    const signals: { horizon: string; signal: AbortSignal }[] = [];
    serve({
      list: page([done("1"), done("2")]),
      compare: (o: any) => {
        const { external_id, horizon } = o.params.query;
        signals.push({ horizon, signal: o.signal });
        const d = deferred<Res>();
        gates[`${external_id}@${horizon}`] = d;
        return d.promise;
      },
    });
    const w = mountCard();
    await settle();
    expect(compareCalls()).toHaveLength(2);
    expect(signals.every((s) => s.horizon === "1h")).toBe(true);
    expect(rows(w).map((r) => cellOf(r).attributes("data-state"))).toEqual(["loading", "loading"]);

    await t(w, "changes-horizon-6h").trigger("click");
    await settle();
    expect(signals.filter((s) => s.horizon === "1h").every((s) => s.signal.aborted), "the 1 h reads are aborted").toBe(true);
    expect(compareCalls()).toHaveLength(4);
    expect(compareCalls().slice(2).map((c) => c[1].params.query.horizon)).toEqual(["6h", "6h"]);
    expect(compareCalls().slice(2).map((c) => c[1].params.query.external_id).sort()).toEqual(["1", "2"]);
    expect(t(w, "changes-horizon").attributes("data-horizon")).toBe("6h");
    expect(listCalls(), "the horizon does not re-read the list").toHaveLength(1);

    // The aborted 1 h answers land late: they are for a column that no longer exists and are dropped.
    gates["1@1h"].resolve(ok(compareBody({ before: sideFigure(11.11), after: sideFigure(22.22), delta: 11.11 })));
    gates["2@1h"].resolve(ok(compareBody({ before: sideFigure(11.11), after: sideFigure(22.22), delta: 11.11 })));
    await settle();
    expect(w.text(), "a stale horizon's figure is never applied").not.toContain("11.11 %");

    gates["1@6h"].resolve(ok(compareBody({ before: sideFigure(99.9), after: sideFigure(99.1), delta: -0.8 })));
    gates["2@6h"].resolve(ok(compareBody({ before: sideFigure(99.9), after: sideFigure(99.1), delta: -0.8 })));
    await settle();
    expect(all(w, "changes-compare-before").map((s) => s.text())).toEqual(["99.90 %", "99.90 %"]);
    expect(all(w, "changes-compare").every((c) => c.attributes("data-horizon") === "6h")).toBe(true);
  });
});

describe("ServiceChanges — the marks are EMITTED, never drawn here (D14, invariant 19)", () => {
  it("one mark per terminal phase, at the terminal's instant; the started-only group contributes none", async () => {
    const items = [done("1"), running("2"), done("3")];
    serve({ list: page(items) });
    const w = mountCard();
    await settle();
    const marks = lastMarks(w);
    expect(marks).toHaveLength(2);
    expect(marks.map((m) => m.at)).toEqual(["2026-08-30T14:05:00Z", "2026-08-30T14:05:00Z"]);
    expect(marks.map((m) => m.key)).toEqual([groupKey(items[0]), groupKey(items[2])]);
    expect(t(w, "changes-marks-note").attributes("data-marks")).toBe("2");
    expect(w.findAll("svg, canvas"), "the card draws no strip of its own").toHaveLength(0);
  });

  it("the marks are CLEARED on unmount — the strip above must not keep a dead series", async () => {
    serve({ list: page([done("1")]) });
    const w = mountCard();
    await settle();
    expect(lastMarks(w)).toHaveLength(1);
    w.unmount();
    expect(lastMarks(w)).toEqual([]);
  });

  it("a started-only page emits nothing and says so in the note", async () => {
    serve({ list: page([running("2")]) });
    const w = mountCard();
    await settle();
    expect(lastMarks(w)).toEqual([]);
    expect(t(w, "changes-marks-note").text()).toContain("no mark on the reliability timeline above yet");
  });
});

describe("ServiceChanges — paging reuses the FROZEN range (D6)", () => {
  it("'Show 10 more' sends the SAME from/to with the cursor, and appends", async () => {
    serve({
      list: (o: any) => (o.params.query.cursor ? page([done("3")]) : page([done("1"), done("2")], "cur-1")),
    });
    const w = mountCard();
    await settle();
    expect(rows(w)).toHaveLength(2);
    const first = listCalls()[0][1].params.query;
    expect(t(w, "changes-more").text()).toBe("Show 10 more");

    await t(w, "changes-more").trigger("click");
    await settle();
    expect(listCalls()).toHaveLength(2);
    const second = listCalls()[1][1].params.query;
    expect(second).toEqual({ from: first.from, to: first.to, limit: CARD_PAGE_SIZE, cursor: "cur-1" });
    expect(rows(w).map((r) => r.attributes("data-external-id"))).toEqual(["1", "2", "3"]);
    expect(has(w, "changes-more"), "a null cursor ends the traversal").toBe(false);
    expect(compareCalls().map((c) => c[1].params.query.external_id).sort(), "the fresh group is compared too").toEqual(["1", "2", "3"]);
    expect(lastMarks(w)).toHaveLength(3);
  });

  it("a range change opens a NEW traversal: a new range, no cursor, the rows replaced", async () => {
    serve({ list: page([done("1")], "cur-1") });
    const w = mountCard();
    await settle();
    await t(w, "changes-range-7d").trigger("click");
    await settle();
    expect(listCalls()).toHaveLength(2);
    const q = listCalls()[1][1].params.query;
    expect(Date.parse(q.to) - Date.parse(q.from)).toBe(7 * DAY_MS);
    expect(q.cursor).toBeUndefined();
    expect(t(w, "changes-range").attributes("data-days")).toBe("7");
    expect(t(w, "changes-count").text()).toContain("last 7 days");
  });
});

describe("ServiceChanges — every refusal renders", () => {
  it("401 and 403 are one line with no rows and no comparison asked", async () => {
    for (const status of [401, 403]) {
      apiMock.GET.mockReset();
      serve({ list: refused(status, "") });
      const w = mountCard();
      await settle();
      expect(has(w, "changes-unavailable"), `HTTP ${status}`).toBe(true);
      expect(t(w, "changes-unavailable").attributes("data-status")).toBe(String(status));
      expect(has(w, "changes-group")).toBe(false);
      expect(has(w, "changes-error")).toBe(false);
      expect(compareCalls()).toHaveLength(0);
      expect(t(w, "changes-unavailable").text()).toBe(status === 401 ? "Your session has ended — sign in again." : "You cannot read this service's changes.");
    }
  });

  it("429 names the Retry-After seconds and offers Retry", async () => {
    serve({ list: refused(429, "principal_inflight", { "Retry-After": "12" }) });
    const w = mountCard();
    await settle();
    expect(t(w, "changes-error").text()).toBe("You already have as many change reads in flight as one principal may. Try again in 12 s.");
    expect(t(w, "changes-error").attributes("data-status")).toBe("429");
    expect(has(w, "changes-retry")).toBe(true);
  });

  it("a network failure keeps the transport's own words, verbatim, at status 0", async () => {
    serve({ list: () => Promise.reject(new Error("Failed to fetch")) });
    const w = mountCard();
    await settle();
    expect(t(w, "changes-error").text()).toBe("Could not reach the server: Failed to fetch");
    expect(t(w, "changes-error").attributes("data-status")).toBe("0");
  });

  it("a refusal on 'Show 10 more' keeps the rows already shown and says why beside the button", async () => {
    serve({ list: (o: any) => (o.params.query.cursor ? refused(400, "cursor_invalid") : page([done("1")], "cur-1")) });
    const w = mountCard();
    await settle();
    await t(w, "changes-more").trigger("click");
    await settle();
    expect(rows(w), "the page already read is still true").toHaveLength(1);
    expect(t(w, "changes-more-error").text()).toContain("This page marker is no longer valid");
  });
});

describe("ServiceChanges — concurrency (D-0210 item 7)", () => {
  it("unmount aborts BOTH scopes: the list while it is in flight, and a comparison after it", async () => {
    // (1) unmounted while the LIST is in flight.
    const listGate = deferred<Res>();
    const listSignals: AbortSignal[] = [];
    serve({
      list: (o: any) => {
        listSignals.push(o.signal);
        return listGate.promise;
      },
    });
    const w1 = mountCard();
    await flushPromises();
    expect(listSignals[0].aborted).toBe(false);
    w1.unmount();
    expect(listSignals[0].aborted, "the list scope aborts on unmount").toBe(true);
    listGate.resolve(page([done("1")]));
    await settle();
    expect(compareCalls(), "an aborted list never schedules a comparison").toHaveLength(0);

    // (2) unmounted while a COMPARISON is in flight — the second scope, aborted by the same hook.
    apiMock.GET.mockReset();
    const cmpGate = deferred<Res>();
    const cmpSignals: AbortSignal[] = [];
    serve({
      list: page([done("1")]),
      compare: (o: any) => {
        cmpSignals.push(o.signal);
        return cmpGate.promise;
      },
    });
    const w2 = mountCard();
    await settle();
    expect(cmpSignals).toHaveLength(1);
    expect(cmpSignals[0].aborted).toBe(false);
    w2.unmount();
    expect(cmpSignals[0].aborted, "the comparison scope aborts on unmount").toBe(true);
    cmpGate.resolve(ok(compareBody()));
    await settle();
    expect(compareCalls(), "no new read after unmount").toHaveLength(1);
  });

  it("a prop change aborts the previous service's read; its late answer is DROPPED, never applied", async () => {
    const OTHER = "0191c2a4-7f3e-4c1b-9a2d-0000000000aa";
    const gates: ReturnType<typeof deferred<Res>>[] = [];
    const signals: AbortSignal[] = [];
    serve({
      list: (o: any) => {
        signals.push(o.signal);
        const d = deferred<Res>();
        gates.push(d);
        return d.promise;
      },
    });
    const w = mountCard();
    await flushPromises();
    expect(listCalls()).toHaveLength(1);

    await w.setProps({ serviceId: OTHER });
    await settle();
    expect(listCalls()).toHaveLength(2);
    expect(signals[0].aborted, "the previous service's read is aborted").toBe(true);
    expect(listCalls()[1][1].params.path.serviceID).toBe(OTHER);

    gates[0].resolve(page([done("stale")]));
    await settle();
    expect(rows(w), "the previous service's page never reaches this service's card").toHaveLength(0);
    gates[1].resolve(page([done("fresh")]));
    await settle();
    expect(rows(w).map((r) => r.attributes("data-external-id"))).toEqual(["fresh"]);
  });
});
