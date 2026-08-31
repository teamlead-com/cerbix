import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { reactive } from "vue";

import ServiceChangesView from "@/views/ServiceChangesView.vue";
import { RANGE_TOO_WIDE_TEXT } from "@/lib/changesTimeline";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const routeMock = vi.hoisted(() => ({ route: { query: {} as Record<string, unknown>, params: {} as Record<string, string> } }));
vi.mock("vue-router", () => ({
  useRoute: () => routeMock.route,
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' },
}));
vi.mock("@/components/AppShell.vue", () => ({ default: { name: "AppShell", template: "<div><slot /></div>" } }));

type Ws = { init: () => Promise<void>; orgId: string; projectId: string; orgName: string; projectName: string };
const wsMock = vi.hoisted(() => ({ ws: null as unknown as Ws }));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => wsMock.ws }));

// FR-025 D-0210 item 5, mock screen 5: the per-service timeline. What is proven:
//
//   * EVERY request — the first page and each "Show 50 more" — carries an EXPLICIT RFC3339 half-open
//     range of at most 92 days (D6) and the page size, never a server default; paging reuses the
//     range, the kind set and the source FROZEN at Apply, with the cursor;
//   * a range the server would refuse is refused BEFORE any request, in the mock's sentence — and
//     the server's own `range_too_wide` renders identically if it ever comes back;
//   * the kind chips travel as a SET (repeatable `kind=`), and `?kind=` survives a COLD load where
//     the workspace picks its project only after the view mounted (P1 [88]); a LATER project switch
//     is a real switch and resets the filters;
//   * a page that lands after the filters moved is dropped, never appended;
//   * the empty state and the live-traversal note say what this page can and cannot promise.

const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,3})?Z$/;
const DAY_MS = 86_400_000;
const PAGE = 50;
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

const DETAIL = { service: { id: SERVICE, slug: "checkout", name: "Checkout" } };
const PHASE_BASE = { ref: "v4.2.1", url: "", actor_label: "token:ci", actor_user_id: null, via_token: true, recorded_at: "2026-08-30T14:05:01Z" };
const phase = (id: string, name: string, at: string, over: Record<string, unknown> = {}) => ({ ...PHASE_BASE, id, phase: name, occurred_at: at, ...over });

function done(external_id: string, over: Record<string, unknown> = {}) {
  const phases = [phase(`${external_id}-a`, "started", "2026-08-30T14:00:00Z"), phase(`${external_id}-b`, "succeeded", "2026-08-30T14:05:00Z")];
  return { source: "github-actions", external_id, kind: "deploy", ref: "v4.2.1", url: "", latest_occurred_at: "2026-08-30T14:05:00Z", phases, incidents: [], ...over };
}
function running(external_id: string, over: Record<string, unknown> = {}) {
  const phases = [phase(`${external_id}-a`, "started", "2026-08-30T15:00:00Z", { ref: "v4.3.0" })];
  return { source: "argo", external_id, kind: "flag", ref: "", url: "", latest_occurred_at: "2026-08-30T15:00:00Z", phases, incidents: [], ...over };
}

const figure = (availability: number) => ({
  from: "2026-08-30T13:05:00Z",
  to: "2026-08-30T14:05:00Z",
  availability,
  good_seconds: 3597,
  bad_seconds: 3,
  unknown_seconds: 0,
  excluded_seconds: 0,
  buckets: 60,
});
const compareBody = (over: Record<string, unknown> = {}) => ({
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
  before: figure(99.94),
  after: figure(98.28),
  delta: -1.66,
  ...over,
});

function serve(server: { list?: Answer; service?: Answer; compare?: Answer }) {
  const pick = (a: Answer | undefined, fallback: Res, opts: unknown) => {
    if (a === undefined) return Promise.resolve(fallback);
    if (typeof a === "function") return Promise.resolve(a(opts));
    return Promise.resolve(a);
  };
  apiMock.GET.mockImplementation((path: string, opts: unknown) => {
    if (path.endsWith("/changes/compare")) return pick(server.compare, ok(compareBody()), opts);
    if (path.endsWith("/changes")) return pick(server.list, page([]), opts);
    if (path.endsWith("/services/{serviceID}")) return pick(server.service, ok(DETAIL), opts);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function workspace(over: Partial<Ws> = {}): Ws {
  return reactive({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API", ...over });
}

function mountView(opts: { query?: Record<string, unknown>; ws?: Ws } = {}) {
  routeMock.route = reactive({ query: { ...(opts.query ?? {}) }, params: { id: SERVICE } });
  wsMock.ws = opts.ws ?? workspace();
  return mount(ServiceChangesView, {
    global: { stubs: { RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' } } },
  });
}

async function settle() {
  await flushPromises();
  await flushPromises();
  await flushPromises();
}

type W = ReturnType<typeof mountView>;
const t = (w: W, id: string) => w.find(`[data-testid="${id}"]`);
const has = (w: W, id: string) => t(w, id).exists();
const listCalls = () => apiMock.GET.mock.calls.filter((c) => String(c[0]).endsWith("/changes"));
const compareCalls = () => apiMock.GET.mock.calls.filter((c) => String(c[0]).endsWith("/changes/compare"));
const lastQuery = () => listCalls().at(-1)![1].params.query;
const rows = (w: W) => w.findAll('[data-testid="changes-row"]');

async function applyRange(w: W, from: string, to: string) {
  await t(w, "changes-from").setValue(from);
  await t(w, "changes-to").setValue(to);
  await t(w, "changes-apply").trigger("click");
  await settle();
}

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
  apiMock.PUT.mockReset();
  apiMock.DELETE.mockReset();
});

// D14 + D6, over EVERY test in this file: the timeline is a read, and no request leaves without an
// explicit range within the bound, the page size and an AbortSignal.
afterEach(() => {
  expect(apiMock.POST, "the timeline records nothing — the record is the pipeline's").not.toHaveBeenCalled();
  expect(apiMock.PUT).not.toHaveBeenCalled();
  expect(apiMock.DELETE).not.toHaveBeenCalled();
  for (const c of listCalls()) {
    const q = c[1].params.query;
    expect(q.from, "every request names its range").toMatch(RFC3339);
    expect(q.to).toMatch(RFC3339);
    expect(Date.parse(q.to) - Date.parse(q.from)).toBeGreaterThan(0);
    expect(Date.parse(q.to) - Date.parse(q.from), "never wider than D6's 92 days").toBeLessThanOrEqual(92 * DAY_MS);
    expect(q.limit).toBe(PAGE);
    if ("kind" in q) {
      expect(Array.isArray(q.kind), "kind travels as a repeatable set").toBe(true);
      for (const k of q.kind) expect(["deploy", "rollback", "flag"]).toContain(k);
    }
    expect(c[1].signal).toBeInstanceOf(AbortSignal);
  }
});

describe("ServiceChangesView — the range (D6)", () => {
  it("the first request is an explicit RFC3339 half-open pair of 30 calendar days with limit 50 and no cursor", async () => {
    serve({ list: page([done("1")]) });
    const w = mountView();
    await settle();
    expect(listCalls()).toHaveLength(1);
    const q = lastQuery();
    expect(q.from.endsWith("T00:00:00Z"), "day boundaries").toBe(true);
    expect(q.to.endsWith("T00:00:00Z")).toBe(true);
    expect(Date.parse(q.to) - Date.parse(q.from)).toBe(30 * DAY_MS);
    expect(Date.parse(q.to) > Date.now(), "`to` is 00:00Z of tomorrow: today is inside the range").toBe(true);
    expect(q.cursor).toBeUndefined();
    expect(q.kind).toBeUndefined();
    expect(q.source).toBeUndefined();
    expect(listCalls()[0][1].params.path).toEqual({ projectID: "p1", serviceID: SERVICE });
    expect(rows(w)).toHaveLength(1);
  });

  it("92 days are asked for; 93 are refused BEFORE any request, in the mock's own sentence", async () => {
    serve({ list: page([done("1")]) });
    const w = mountView();
    await settle();
    expect(listCalls()).toHaveLength(1);

    await applyRange(w, "2026-05-02", "2026-08-01");
    expect(listCalls(), "92 days: asked").toHaveLength(2);
    expect(Date.parse(lastQuery().to) - Date.parse(lastQuery().from)).toBe(92 * DAY_MS);
    expect(t(w, "changes-days").text()).toContain("92 days");

    await applyRange(w, "2026-05-01", "2026-08-01");
    expect(listCalls(), "93 days: NOT asked").toHaveLength(2);
    expect(t(w, "changes-view-error").text()).toBe(RANGE_TOO_WIDE_TEXT);
    expect(t(w, "changes-view-error").attributes("data-status")).toBe("client");
    expect(has(w, "changes-table"), "the rows answered a different question and go with the refusal").toBe(false);
  });

  it("the server's own `range_too_wide` renders with the SAME sentence", async () => {
    serve({ list: refused(400, "range_too_wide: at most 92 days a page") });
    const w = mountView();
    await settle();
    expect(t(w, "changes-view-error").text()).toBe(RANGE_TOO_WIDE_TEXT);
    expect(t(w, "changes-view-error").attributes("data-status")).toBe("400");
  });

  it("an end before the start is refused before asking, and so is a source that is not a slug", async () => {
    serve({ list: page([done("1")]) });
    const w = mountView();
    await settle();
    await applyRange(w, "2026-08-10", "2026-08-01");
    expect(listCalls()).toHaveLength(1);
    expect(t(w, "changes-view-error").text()).toBe("The end must not be before the start.");

    await t(w, "changes-from").setValue("2026-08-01");
    await t(w, "changes-to").setValue("2026-08-10");
    await t(w, "changes-source-filter").setValue("GitHub Actions");
    await t(w, "changes-apply").trigger("click");
    await settle();
    expect(listCalls(), "an invalid source never leaves the browser").toHaveLength(1);
    expect(t(w, "changes-view-error").text()).toBe("The source is not a valid slug.");
  });
});

describe("ServiceChangesView — the filters", () => {
  it("the kind chips travel as a SET, in the catalogue's order, on the first page and on every next one", async () => {
    serve({ list: (o: any) => (o.params.query.cursor ? page([done("3")]) : page([done("1")], "cur-1")) });
    const w = mountView();
    await settle();
    await t(w, "changes-kind-flag").trigger("click");
    await t(w, "changes-kind-deploy").trigger("click");
    await t(w, "changes-apply").trigger("click");
    await settle();
    expect(listCalls()).toHaveLength(2);
    expect(lastQuery().kind, "the mock's order, not the click order").toEqual(["deploy", "flag"]);
    expect(t(w, "changes-kind-deploy").attributes("data-on")).toBe("true");
    expect(t(w, "changes-kind-rollback").attributes("data-on")).toBeUndefined();

    await t(w, "changes-more").trigger("click");
    await settle();
    expect(lastQuery().kind, "the frozen set travels with the cursor too").toEqual(["deploy", "flag"]);
    expect(lastQuery().cursor).toBe("cur-1");

    // Toggling a chip off again narrows the set; nothing is sent when none is picked.
    await t(w, "changes-kind-flag").trigger("click");
    await t(w, "changes-kind-deploy").trigger("click");
    await t(w, "changes-apply").trigger("click");
    await settle();
    expect(lastQuery().kind).toBeUndefined();
  });

  it("a source slug travels and is frozen with the rest", async () => {
    serve({ list: page([done("1")], "cur-1") });
    const w = mountView();
    await settle();
    await t(w, "changes-source-filter").setValue("github-actions");
    await t(w, "changes-apply").trigger("click");
    await settle();
    expect(lastQuery().source).toBe("github-actions");
    await t(w, "changes-more").trigger("click");
    await settle();
    expect(lastQuery().source).toBe("github-actions");
    expect(lastQuery().cursor).toBe("cur-1");
  });

  it("P1 [88]: `?kind=` survives a COLD load where the workspace picks its project after mount; a LATER switch resets it", async () => {
    const init = deferred<void>();
    const ws = workspace({ projectId: "", init: () => init.promise });
    serve({ list: page([done("1")]) });
    const w = mountView({ query: { kind: "deploy,flag" }, ws });
    await settle();
    expect(listCalls(), "no project yet: nothing asked").toHaveLength(0);

    ws.projectId = "p1";
    init.resolve();
    await settle();
    expect(listCalls(), "exactly one first page — the watcher and onMounted do not both start").toHaveLength(1);
    expect(lastQuery().kind, "the route's pre-filter reaches the FIRST request").toEqual(["deploy", "flag"]);
    expect(listCalls()[0][1].params.path).toEqual({ projectID: "p1", serviceID: SERVICE });
    expect(t(w, "changes-kind-deploy").attributes("data-on")).toBe("true");
    expect(t(w, "changes-kind-flag").attributes("data-on")).toBe("true");
    expect(t(w, "changes-kind-rollback").attributes("data-on")).toBeUndefined();

    // A later project switch is a real switch: the route's kinds belonged to the previous project.
    ws.projectId = "p2";
    await settle();
    expect(listCalls()).toHaveLength(2);
    expect(lastQuery().kind, "the filters reset").toBeUndefined();
    expect(listCalls()[1][1].params.path).toEqual({ projectID: "p2", serviceID: SERVICE });
    expect(t(w, "changes-kind-deploy").attributes("data-on")).toBeUndefined();
  });

  it("an unknown `?kind=` value is dropped rather than sent — the server's closed set is the SPA's too", async () => {
    serve({ list: page([]) });
    mountView({ query: { kind: "deploy,config,flagged" } });
    await settle();
    expect(lastQuery().kind).toEqual(["deploy"]);
  });
});

describe("ServiceChangesView — paging and staleness", () => {
  it("'Show 50 more' reuses the FROZEN range with the cursor and appends", async () => {
    serve({ list: (o: any) => (o.params.query.cursor ? page([done("3")]) : page([done("1"), running("2")], "cur-1")) });
    const w = mountView();
    await settle();
    const first = lastQuery();
    expect(rows(w)).toHaveLength(2);
    expect(t(w, "changes-more").text()).toBe("Show 50 more");

    await t(w, "changes-more").trigger("click");
    await settle();
    expect(listCalls()).toHaveLength(2);
    expect(lastQuery()).toEqual({ from: first.from, to: first.to, limit: PAGE, cursor: "cur-1" });
    expect(rows(w).map((r) => r.attributes("data-external-id"))).toEqual(["1", "2", "3"]);
    expect(has(w, "changes-more"), "a null cursor ends the traversal").toBe(false);
  });

  it("a 'Show 50 more' page that lands AFTER Apply moved the filters is dropped, never appended", async () => {
    const more = deferred<Res>();
    const signals: AbortSignal[] = [];
    let n = 0;
    serve({
      list: (o: any) => {
        signals.push(o.signal);
        n += 1;
        if (n === 1) return page([done("1")], "cur-1");
        if (n === 2) return more.promise; // "Show 50 more" — left in flight on purpose
        return page([done("fresh")]);
      },
    });
    const w = mountView();
    await settle();
    await t(w, "changes-more").trigger("click");
    await flushPromises();
    expect(listCalls()).toHaveLength(2);

    // Apply is reachable while a NEXT page is in flight (only the first page disables it).
    await applyRange(w, "2026-08-01", "2026-08-10");
    expect(listCalls()).toHaveLength(3);
    expect(signals[1].aborted, "the previous traversal's next page is aborted").toBe(true);
    expect(rows(w).map((r) => r.attributes("data-external-id"))).toEqual(["fresh"]);

    more.resolve(page([done("stale")], "cur-2"));
    await settle();
    expect(rows(w).map((r) => r.attributes("data-external-id")), "the old traversal's page is dropped, not appended").toEqual(["fresh"]);
    expect(has(w, "changes-more"), "and its cursor is not adopted either").toBe(false);
  });

  it("a first page that lands after the ROUTE's pre-filter moved is dropped too", async () => {
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
    const w = mountView();
    await flushPromises();
    expect(listCalls()).toHaveLength(1);

    routeMock.route.query = { kind: "rollback" };
    await settle();
    expect(listCalls()).toHaveLength(2);
    expect(signals[0].aborted, "the previous filters' read is aborted").toBe(true);
    expect(lastQuery().kind).toEqual(["rollback"]);

    gates[0].resolve(page([done("stale")]));
    await settle();
    expect(rows(w), "the answer to the question the operator moved away from is dropped").toHaveLength(0);

    gates[1].resolve(page([done("fresh")]));
    await settle();
    expect(rows(w).map((r) => r.attributes("data-external-id"))).toEqual(["fresh"]);
  });

  it("an unmount aborts what is in flight", async () => {
    const gate = deferred<Res>();
    const signals: AbortSignal[] = [];
    serve({
      list: (o: any) => {
        signals.push(o.signal);
        return gate.promise;
      },
    });
    const w = mountView();
    await flushPromises();
    expect(signals[0].aborted).toBe(false);
    w.unmount();
    expect(signals[0].aborted).toBe(true);
    gate.resolve(page([done("1")]));
    await settle();
  });
});

describe("ServiceChangesView — the rows and the before/after column", () => {
  it("the row carries its identity and terminal phase; the started-only row's cell is `no-terminal` and is never asked", async () => {
    serve({ list: page([done("1"), running("2")]) });
    const w = mountView();
    await settle();
    const r = rows(w);
    expect(r[0].attributes("data-terminal")).toBe("succeeded");
    expect(r[1].attributes("data-terminal")).toBeUndefined();
    expect(r[1].find('[data-testid="changes-compare"]').attributes("data-state")).toBe("no-terminal");
    expect(r[1].find('[data-testid="changes-compare"]').text()).toBe("before/after unavailable until a terminal phase");
    expect(compareCalls(), "only the terminal group is compared").toHaveLength(1);
    expect(compareCalls()[0][1].params.query).toEqual({ source: "github-actions", external_id: "1", horizon: "1h" });
    expect(r[0].find('[data-testid="changes-compare-before"]').text()).toBe("99.94 %");
    expect(r[0].find('[data-testid="changes-compare-after"]').text()).toBe("98.28 %");
    expect(r[0].find('[data-testid="changes-compare-delta"]').text()).toBe("−1.66");
  });

  it("moving the header's horizon re-issues every comparison and does NOT re-read the page", async () => {
    serve({ list: page([done("1")]), compare: (o: any) => ok(compareBody({ horizon: o.params.query.horizon })) });
    const w = mountView();
    await settle();
    expect(compareCalls()).toHaveLength(1);
    await t(w, "changes-horizon-24h").trigger("click");
    await settle();
    expect(listCalls(), "the horizon is not a filter").toHaveLength(1);
    expect(compareCalls()).toHaveLength(2);
    expect(compareCalls()[1][1].params.query.horizon).toBe("24h");
    expect(t(w, "changes-compare-header").text()).toContain("24 h");
  });

  it("the decision cell is the ledger's live state or `aged out`; `preceded` carries the lag and links the incident", async () => {
    serve({
      list: page([
        done("1", {
          decision: { decision_id: DECISION, state: "WARN", action: "ALLOW", overridden: true },
          incidents: [{ incident_id: INCIDENT, opened_at: "2026-08-30T14:31:00Z", role: "own_service", lag_seconds: 1560, change_id: "1-b" }],
        }),
        done("2", { decision: { decision_id: DECISION, aged_out: true } }),
      ]),
    });
    const w = mountView();
    await settle();
    const cells = w.findAll('[data-testid="changes-decision"]');
    expect(cells[0].attributes("data-state")).toBe("WARN");
    expect(cells[0].find('[data-testid="changes-decision-override"]').text()).toBe("→ ALLOW");
    expect(cells[1].attributes("data-state")).toBe("aged-out");
    expect(cells[1].text()).toContain("aged out");

    const preceded = t(w, "changes-preceded");
    expect(preceded.attributes("data-role")).toBe("own_service");
    expect(t(w, "changes-preceded-lag").text()).toBe("−26 m");
    expect(JSON.parse(t(w, "changes-preceded-link").attributes("data-to")!)).toEqual({ name: "incident", params: { id: INCIDENT } });
    expect(w.text()).not.toContain("caused");
  });
});

describe("ServiceChangesView — the empty state and the notes", () => {
  it("an empty page names the applied range and points at the pipeline; the live-traversal note stands", async () => {
    serve({ list: page([]) });
    const w = mountView();
    await settle();
    expect(has(w, "changes-view-empty")).toBe(true);
    expect(t(w, "changes-view-empty").text()).toContain("No change was recorded on this service between");
    expect(t(w, "changes-view-empty").text()).toContain("cerbix change record");
    expect(has(w, "changes-table")).toBe(false);
    expect(has(w, "changes-view-error")).toBe(false);
    expect(t(w, "changes-live-note").text()).toContain("a group never appears twice");
    expect(t(w, "changes-aged-out-note").text()).toContain("a group without a terminal phase has none");
  });

  it("an empty page under filters says which ones were applied", async () => {
    serve({ list: page([]) });
    const w = mountView();
    await settle();
    await t(w, "changes-kind-rollback").trigger("click");
    await t(w, "changes-source-filter").setValue("argo");
    await t(w, "changes-apply").trigger("click");
    await settle();
    expect(t(w, "changes-view-empty").text()).toContain("(rollback · source argo)");
  });

  it("401/403/404 leave no table behind, and the transport keeps its own words", async () => {
    for (const [status, text] of [
      [401, "Your session has ended — sign in again."],
      [403, "You cannot see this service's changes."],
      [404, "This service does not exist, or you cannot see it."],
    ] as [number, string][]) {
      apiMock.GET.mockReset();
      serve({ list: refused(status, status === 404 ? "not found" : ""), service: refused(status, status === 404 ? "not found" : "") });
      const w = mountView();
      await settle();
      expect(t(w, "changes-view-error").text(), `HTTP ${status}`).toBe(text);
      expect(has(w, "changes-table")).toBe(false);
    }

    apiMock.GET.mockReset();
    serve({ list: () => Promise.reject(new Error("Failed to fetch")) });
    const w = mountView();
    await settle();
    expect(t(w, "changes-view-error").text()).toBe("Could not reach the server: Failed to fetch");
    expect(t(w, "changes-view-error").attributes("data-status")).toBe("0");
  });


  // Review [32]: the timeline asked one comparison per terminal row with nothing holding them back.
  // A page is up to 50 groups and `change.read_inflight_process` is configurable down to 1, so a
  // normal page could saturate the instance's read permits, manufacture its own 429s and crowd out
  // every other read. The card already used a pool of four; the loop is now owned once in
  // `lib/changes.ts` and both screens go through it.
  //
  // The assertion is on MAX CONCURRENCY, measured, not on the pool constant: the comparison answers
  // are deferred and released in waves, so a run that opened everything at once cannot pass by
  // finishing quickly.
  it("never opens more than four comparisons at once, however many rows the page has", async () => {
    const rows = Array.from({ length: 12 }, (_, i) => done(`run-${i}`));
    let open = 0;
    let peak = 0;
    const gates: Array<(v: Res) => void> = [];
    serve({
      list: page(rows),
      compare: () => {
        open++;
        peak = Math.max(peak, open);
        const d = deferred<Res>();
        gates.push((v) => {
          open--;
          d.resolve(v);
        });
        return d.promise;
      },
    });
    mountView();
    await settle();

    expect(compareCalls().length).toBe(4);
    expect(peak).toBe(4);

    // Release one: exactly one more starts, so the pool refills rather than bursting.
    gates.shift()!(ok(compareBody()));
    await settle();
    expect(compareCalls().length).toBe(5);
    expect(peak).toBe(4);

    // Drain the rest; every row is asked exactly once and the ceiling never moved.
    while (gates.length) {
      gates.shift()!(ok(compareBody()));
      await settle();
    }
    expect(compareCalls().length).toBe(12);
    expect(peak).toBe(4);
  });



  // Review [37]: the bound of [32] introduced a way to stall. `inPool` holds a slot until its work
  // resolves, so four comparisons that never settle would occupy the whole pool and leave every
  // queued row loading forever — where the UNBOUNDED fan-out at least asked every row. The 10 s
  // deadline in `requestScope` is what makes the bound safe: it aborts, frees the slot, and leaves
  // an error cell a reader can see.
  //
  // Fake timers, so this is deterministic rather than a race against a real clock.
  it("releases a pool slot when a comparison outlives its deadline, and the queue moves on", async () => {
    vi.useFakeTimers();
    try {
      const rows = Array.from({ length: 6 }, (_, i) => done(`run-${i}`));
      serve({ list: page(rows), compare: () => new Promise<Res>(() => {}) }); // never settles, ever
      const w = mountView();
      await settle();

      // Four in flight, two queued behind them, nothing resolved.
      expect(compareCalls().length).toBe(4);
      expect(w.findAll('[data-testid="changes-compare"][data-state="error"]').length).toBe(0);

      // Cross the deadline: the four abort, their cells become errors, and the two queued start.
      await vi.advanceTimersByTimeAsync(10_000);
      await settle();
      expect(compareCalls().length).toBe(6);
      expect(w.findAll('[data-testid="changes-compare"][data-state="error"]').length).toBe(4);

      // And the last two are not stuck either — they hold their own slots for one deadline more.
      await vi.advanceTimersByTimeAsync(10_000);
      await settle();
      expect(w.findAll('[data-testid="changes-compare"][data-state="error"]').length).toBe(6);
      expect(w.findAll('[data-testid="changes-compare"][data-state="loading"]').length).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });
});
