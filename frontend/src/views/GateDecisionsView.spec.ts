import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { reactive } from "vue";

import GateDecisionsView from "@/views/GateDecisionsView.vue";
import { RANGE_TOO_WIDE_TEXT } from "@/lib/gateLedger";

// FR-024 D-0207 items 1 and 5, mock screen 4: the project's decision LEDGER. What is proven here:
//
//   * EVERY request — the first page and each "Show 50 more" — carries an explicit RFC3339
//     half-open range of at most 31 days, frozen at Apply, plus `limit: 50`; paging reuses the SAME
//     pair with the cursor. A range the server would refuse is refused before any request.
//   * every refusal renders: 400 `range_too_wide` (the same sentence as the client's), 401/403 (one
//     line, no table), 429 (Retry-After seconds), a network failure verbatim, an empty page.
//   * `?service=` pre-selects — on a COLD load too, when `ws.init()` picks the project only after the
//     view mounted (P1 [88]); a LATER project switch resets the filters.
//   * the state filter is the SERVER's (iter-0164): a picked state travels as a one-element `state`
//     array on the first page AND on "Show 50 more", nothing is filtered here and no hint says
//     otherwise; a deleted service is a chip; a page that lands after the filters moved is dropped.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const routeMock = vi.hoisted(() => ({ route: { query: {} as Record<string, string>, params: {} as Record<string, string> } }));
vi.mock("vue-router", () => ({
  useRoute: () => routeMock.route,
  useRouter: () => ({ push: vi.fn() }),
  RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));

type Ws = { init: () => Promise<void>; orgId: string; projectId: string; orgName: string; projectName: string };
const wsMock = vi.hoisted(() => ({ ws: null as unknown as Ws }));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => wsMock.ws }));

const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;
const DAY_MS = 86_400_000;
const SVC_A = "6d1f0b1e-0000-4000-8000-00000000000a";
const SVC_B = "6d1f0b1e-0000-4000-8000-00000000000b";

type Res = { data?: unknown; error?: unknown; response?: Response };
const ok = (data: unknown): Res => ({ data, response: new Response(null, { status: 200 }) });
const refused = (status: number, code: string, headers?: Record<string, string>): Res => ({
  error: { error: code },
  response: new Response(null, { status, headers }),
});

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => (resolve = res));
  return { promise, resolve };
}

function row(id: string, over: Record<string, unknown> = {}) {
  return {
    schema_version: 1,
    decision_id: id,
    evaluated_at: "2026-08-29T14:03:02Z",
    service_id: SVC_A,
    service_slug: "checkout",
    service_name: "Checkout",
    state: "ALLOW",
    action: "ALLOW",
    reasons: [],
    policy_revision: 3,
    ...over,
  };
}
const page = (items: unknown[], next_cursor: string | null = null) => ok({ items, next_cursor });
const SERVICES = [
  { service: { id: SVC_A, slug: "checkout", name: "Checkout" } },
  { service: { id: SVC_B, slug: "api", name: "API" } },
];

type Answer = Res | Promise<Res> | ((opts: any) => Res | Promise<Res>);
function serve(list: Answer, services: Answer = ok(SERVICES)) {
  const pick = (a: Answer, opts: unknown) => Promise.resolve(typeof a === "function" ? a(opts) : a);
  apiMock.GET.mockImplementation((path: string, opts: unknown) => {
    if (path.endsWith("/gate/decisions")) return pick(list, opts);
    if (path.endsWith("/services")) return pick(services, opts);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function workspace(over: Partial<Ws> = {}): Ws {
  return reactive({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API", ...over });
}

function mountView(opts: { query?: Record<string, string>; ws?: Ws } = {}) {
  routeMock.route = reactive({ query: opts.query ?? {}, params: {} });
  wsMock.ws = opts.ws ?? workspace();
  return mount(GateDecisionsView, {
    global: { stubs: { RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' } } },
  });
}

async function settle() {
  await flushPromises();
  await flushPromises();
}

type W = ReturnType<typeof mountView>;
const t = (w: W, id: string) => w.find(`[data-testid="${id}"]`);
const has = (w: W, id: string) => t(w, id).exists();
const ledgerCalls = () => apiMock.GET.mock.calls.filter((c) => String(c[0]).endsWith("/gate/decisions"));
const lastQuery = () => ledgerCalls().at(-1)![1].params.query;
const rows = (w: W) => w.findAll('[data-testid="gate-decision-row"]');

async function applyRange(w: W, from: string, to: string) {
  await t(w, "gate-decisions-from").setValue(from);
  await t(w, "gate-decisions-to").setValue(to);
  await t(w, "gate-decisions-apply").trigger("click");
  await settle();
}

// Braces matter: a function RETURNED from beforeEach is registered as a cleanup hook, and
// `mockReset()` returns the mock itself — which would then be called as `api.GET()` after every test.
beforeEach(() => {
  apiMock.GET.mockReset();
});
afterEach(() => {
  expect(apiMock.POST, "opening the ledger writes nothing").not.toHaveBeenCalled();
  for (const c of ledgerCalls()) {
    const q = c[1].params.query;
    expect(q.from, "every ledger request names its range").toMatch(RFC3339);
    expect(q.to).toMatch(RFC3339);
    expect(Date.parse(q.to) - Date.parse(q.from)).toBeGreaterThan(0);
    expect(Date.parse(q.to) - Date.parse(q.from), "never wider than 31 days").toBeLessThanOrEqual(31 * DAY_MS);
    expect(q.limit).toBe(50);
    // `state` is the server's set filter: absent for "Any state", otherwise exactly the one picked.
    if ("state" in q) {
      expect(Array.isArray(q.state), "state travels as an array (repeatable query param)").toBe(true);
      expect(q.state).toHaveLength(1);
      expect(["ALLOW", "WARN", "BLOCK", "UNKNOWN", "NOT_CONFIGURED"]).toContain(q.state[0]);
    }
    expect(c[1].signal).toBeInstanceOf(AbortSignal);
  }
});

describe("GateDecisionsView — the range (check 3)", () => {
  it("the first request carries the default 30 calendar days as an RFC3339 half-open pair, limit 50, no cursor", async () => {
    serve(page([row("d1")]));
    const w = mountView();
    await settle();
    expect(ledgerCalls()).toHaveLength(1);
    const q = lastQuery();
    expect(q.from).toMatch(RFC3339);
    expect(q.to).toMatch(RFC3339);
    expect(q.from.endsWith("T00:00:00Z"), "day boundaries").toBe(true);
    expect(q.to.endsWith("T00:00:00Z")).toBe(true);
    expect(Date.parse(q.to) - Date.parse(q.from), "30 inclusive calendar days = 30 whole days").toBe(30 * DAY_MS);
    expect(Date.parse(q.to) > Date.now(), "`to` is 00:00Z of TOMORROW: today is inside the range").toBe(true);
    expect(q.limit).toBe(50);
    expect(q.cursor).toBeUndefined();
    expect(q.service_id).toBeUndefined();
    expect(ledgerCalls()[0][1].params.path).toEqual({ projectID: "p1" });
    expect(rows(w)).toHaveLength(1);
    expect(has(w, "gate-decisions-more")).toBe(false);
  });

  it("`to` is 00:00Z of the day AFTER the last picked day, and 'Show 50 more' reuses the SAME pair with the cursor", async () => {
    let cursorSeen: string | undefined;
    serve((opts: { params: { query: { cursor?: string } } }) => {
      cursorSeen = opts.params.query.cursor;
      return cursorSeen ? page([row("d3")]) : page([row("d1"), row("d2")], "cur-1");
    });
    const w = mountView();
    await settle();
    await applyRange(w, "2026-08-01", "2026-08-31");
    expect(ledgerCalls()).toHaveLength(2);
    expect(lastQuery()).toEqual({ from: "2026-08-01T00:00:00Z", to: "2026-09-01T00:00:00Z", limit: 50 });
    expect(rows(w)).toHaveLength(2);
    expect(t(w, "gate-decisions-more").text()).toBe("Show 50 more");

    await t(w, "gate-decisions-more").trigger("click");
    await settle();
    expect(ledgerCalls()).toHaveLength(3);
    expect(lastQuery()).toEqual({ from: "2026-08-01T00:00:00Z", to: "2026-09-01T00:00:00Z", limit: 50, cursor: "cur-1" });
    expect(cursorSeen).toBe("cur-1");
    expect(rows(w).map((r) => r.attributes("data-id"))).toEqual(["d1", "d2", "d3"]);
    expect(has(w, "gate-decisions-more"), "a null cursor ends the traversal").toBe(false);
  });

  it("32 days are refused BEFORE any request with the server's sentence; 31 are asked for", async () => {
    serve(page([row("d1")]));
    const w = mountView();
    await settle();
    expect(ledgerCalls()).toHaveLength(1);
    await applyRange(w, "2026-08-01", "2026-09-01");
    expect(ledgerCalls(), "no request for a refused range").toHaveLength(1);
    const err = t(w, "gate-decisions-error");
    expect(err.attributes("data-status")).toBe("client");
    expect(err.text()).toBe(RANGE_TOO_WIDE_TEXT);
    expect(rows(w), "the old rows answered a different question").toHaveLength(0);
    expect(has(w, "gate-decisions-empty"), "a refusal is not an empty page").toBe(false);

    await applyRange(w, "2026-08-01", "2026-08-31");
    expect(ledgerCalls()).toHaveLength(2);
    expect(Date.parse(lastQuery().to) - Date.parse(lastQuery().from)).toBe(31 * DAY_MS);
    expect(has(w, "gate-decisions-error")).toBe(false);
    expect(rows(w)).toHaveLength(1);
  });

  it("an end before the start, and a missing date, are refused too", async () => {
    serve(page([]));
    const w = mountView();
    await settle();
    await applyRange(w, "2026-08-10", "2026-08-01");
    expect(t(w, "gate-decisions-error").text()).toBe("The end must not be before the start.");
    expect(t(w, "gate-decisions-error").attributes("data-status")).toBe("client");
    await applyRange(w, "", "2026-08-01");
    expect(t(w, "gate-decisions-error").text()).toBe("Pick both dates.");
    expect(ledgerCalls()).toHaveLength(1);
  });
});

describe("GateDecisionsView — refusals render as themselves (check 3)", () => {
  it("the server's 400 range_too_wide is the same sentence, with its status", async () => {
    serve(refused(400, "range_too_wide"));
    const w = mountView();
    await settle();
    expect(t(w, "gate-decisions-error").text()).toBe(RANGE_TOO_WIDE_TEXT);
    expect(t(w, "gate-decisions-error").attributes("data-status")).toBe("400");
    expect(has(w, "gate-decisions-table")).toBe(false);
  });

  it("401: one line, no table", async () => {
    serve(refused(401, "unauthorized"));
    const w = mountView();
    await settle();
    expect(t(w, "gate-decisions-error").text()).toBe("Your session has ended — sign in again.");
    expect(t(w, "gate-decisions-error").attributes("data-status")).toBe("401");
    expect(has(w, "gate-decisions-table")).toBe(false);
    expect(has(w, "gate-decisions-empty")).toBe(false);
  });

  it("403: one line in the view's own words, no table", async () => {
    serve(refused(403, "forbidden"));
    const w = mountView();
    await settle();
    expect(t(w, "gate-decisions-error").text()).toBe("You cannot see this project's gate decisions.");
    expect(t(w, "gate-decisions-error").attributes("data-status")).toBe("403");
    expect(has(w, "gate-decisions-table")).toBe(false);
  });

  it("429: the Retry-After seconds", async () => {
    serve(refused(429, "principal_inflight", { "Retry-After": "12" }));
    const w = mountView();
    await settle();
    expect(t(w, "gate-decisions-error").text()).toBe("You already have as many ledger reads in flight as one principal may. Try again in 12 s.");
    expect(t(w, "gate-decisions-error").attributes("data-status")).toBe("429");
  });

  it("a network failure: the transport's words, verbatim, status 0", async () => {
    serve(() => Promise.reject(new Error("Failed to fetch")));
    const w = mountView();
    await settle();
    expect(t(w, "gate-decisions-error").text()).toBe("Could not reach the server: Failed to fetch");
    expect(t(w, "gate-decisions-error").attributes("data-status")).toBe("0");
  });

  it("an empty page says so, naming the applied range", async () => {
    serve(page([]));
    const w = mountView();
    await settle();
    expect(has(w, "gate-decisions-empty")).toBe(true);
    expect(t(w, "gate-decisions-empty").text()).toContain("No decisions between");
    expect(t(w, "gate-decisions-empty").text()).toContain("opening this page writes nothing");
    expect(has(w, "gate-decisions-error")).toBe(false);
  });
});

describe("GateDecisionsView — filters", () => {
  it("`?service=` pre-selects the picker and filters the FIRST request", async () => {
    serve(page([row("d1")]));
    const w = mountView({ query: { service: SVC_A } });
    await settle();
    expect(ledgerCalls()).toHaveLength(1);
    expect(lastQuery().service_id).toBe(SVC_A);
    expect((t(w, "gate-decisions-service").element as HTMLSelectElement).value).toBe(SVC_A);
  });

  it("P1 [88]: a COLD load of ?service= survives the workspace picking its project after mount; a LATER switch resets the filter", async () => {
    const init = deferred<void>();
    const ws = workspace({ projectId: "", init: () => init.promise });
    serve(page([row("d1")]));
    const w = mountView({ query: { service: SVC_A }, ws });
    await settle();
    expect(ledgerCalls(), "no project yet: nothing asked").toHaveLength(0);

    // init() picks the project: the watcher fires for the FIRST time — this is startup, not a switch.
    ws.projectId = "p1";
    init.resolve();
    await settle();
    expect(ledgerCalls(), "exactly one first page — the watcher and onMounted do not both start").toHaveLength(1);
    expect(lastQuery().service_id, "the route's pre-filter reaches the first request").toBe(SVC_A);
    expect(ledgerCalls()[0][1].params.path).toEqual({ projectID: "p1" });
    expect((t(w, "gate-decisions-service").element as HTMLSelectElement).value).toBe(SVC_A);

    // A later project switch is a real switch: the route's service belonged to the previous project.
    ws.projectId = "p2";
    await settle();
    expect(ledgerCalls()).toHaveLength(2);
    expect(lastQuery().service_id).toBeUndefined();
    expect(ledgerCalls()[1][1].params.path).toEqual({ projectID: "p2" });
    expect((t(w, "gate-decisions-service").element as HTMLSelectElement).value, "back to All services").toBe("");
  });

  it("the workspace already initialised: onMounted starts once with the route's service", async () => {
    serve(page([row("d1")]));
    const ws = workspace();
    mountView({ query: { service: SVC_B }, ws });
    await settle();
    expect(ledgerCalls()).toHaveLength(1);
    expect(lastQuery().service_id).toBe(SVC_B);
  });

  it("a pre-selected service the picker does not list is still shown, shortened", async () => {
    serve(page([]), ok([]));
    const w = mountView({ query: { service: SVC_A } });
    await settle();
    const opts = t(w, "gate-decisions-service").findAll("option");
    expect(opts.map((o) => o.attributes("value"))).toEqual(["", SVC_A]);
    expect(opts[1].text()).toBe("6d1f0b1e…000a");
  });

  it("the state filter is server-side: `state` travels as a one-element array on the first page AND on 'Show 50 more', nothing is filtered here, no hint", async () => {
    // The server answers what it is asked: the rows it returns for `state=BLOCK` are all BLOCK, and
    // the view renders EVERY row it gets — a client that still filtered would be invisible here, so
    // the second page deliberately carries a row of another state to prove nothing is dropped.
    serve((opts: { params: { query: { state?: string[]; cursor?: string } } }) => {
      const { state, cursor } = opts.params.query;
      if (!state) return page([row("d1", { state: "BLOCK", action: "BLOCK" }), row("d2"), row("d3", { state: "WARN", action: "WARN" })]);
      return cursor ? page([row("d6", { state: "WARN", action: "WARN" })]) : page([row("d4", { state: "BLOCK", action: "BLOCK" }), row("d5", { state: "BLOCK", action: "BLOCK" })], "cur-b");
    });
    const w = mountView();
    await settle();
    expect(rows(w)).toHaveLength(3);
    expect("state" in lastQuery(), "Any state: no `state` key at all").toBe(false);
    expect(has(w, "gate-decisions-state-hint"), "the client-side hint is gone").toBe(false);

    await t(w, "gate-decisions-state").setValue("BLOCK");
    await t(w, "gate-decisions-apply").trigger("click");
    await settle();
    expect(ledgerCalls()).toHaveLength(2);
    expect(lastQuery().state, "the first filtered page carries the state").toEqual(["BLOCK"]);
    expect(lastQuery().cursor).toBeUndefined();
    expect(rows(w).map((r) => r.attributes("data-id")), "the server's rows, as returned").toEqual(["d4", "d5"]);
    expect(has(w, "gate-decisions-state-hint")).toBe(false);

    await t(w, "gate-decisions-more").trigger("click");
    await settle();
    expect(ledgerCalls()).toHaveLength(3);
    expect(lastQuery(), "the next page reuses the SAME state with the cursor").toMatchObject({ state: ["BLOCK"], cursor: "cur-b", limit: 50 });
    expect(rows(w).map((r) => r.attributes("data-id")), "appended verbatim — no client-side filtering").toEqual(["d4", "d5", "d6"]);
    expect(has(w, "gate-decisions-more")).toBe(false);

    // Back to "Any state": the key disappears again.
    await t(w, "gate-decisions-state").setValue("");
    await t(w, "gate-decisions-apply").trigger("click");
    await settle();
    expect(ledgerCalls()).toHaveLength(4);
    expect("state" in lastQuery()).toBe(false);
  });

  it("an empty filtered page names the state it asked for", async () => {
    serve((opts: { params: { query: { state?: string[] } } }) => (opts.params.query.state ? page([]) : page([row("d1")])));
    const w = mountView();
    await settle();
    expect(rows(w)).toHaveLength(1);
    await t(w, "gate-decisions-state").setValue("NOT_CONFIGURED");
    await t(w, "gate-decisions-apply").trigger("click");
    await settle();
    expect(lastQuery().state).toEqual(["NOT_CONFIGURED"]);
    expect(rows(w)).toHaveLength(0);
    expect(t(w, "gate-decisions-empty").text()).toContain("No not configured decisions between");
    expect(t(w, "gate-decisions-empty").text()).not.toContain("loaded decisions");
    expect(has(w, "gate-decisions-state-hint")).toBe(false);
  });

  it("rows: the state pill, the reason chips with their attrs, a deleted service's chip, the link by id", async () => {
    serve(
      page([
        row("d1", {
          state: "UNKNOWN",
          action: "WARN",
          reasons: [{ code: "seal_stale", clause: "budget_exhausted", assignment: "block" }],
          override_id: "ov-1234567890abcdef",
        }),
        row("d2", { service_id: null, service_slug: "gone", state: "NOT_CONFIGURED", reasons: [{ code: "not_configured" }] }),
      ]),
    );
    const w = mountView();
    await settle();
    const [r1, r2] = rows(w);
    expect(r1.attributes("data-state")).toBe("UNKNOWN");
    expect(r1.text()).toContain("UNKNOWN");
    expect(r1.text()).toContain("rev 3");
    const chip = r1.find('[data-testid="gate-reason-chip"]');
    expect(chip.text()).toBe("seal_stale");
    expect(chip.attributes()).toMatchObject({ "data-code": "seal_stale", "data-clause": "budget_exhausted", "data-assignment": "block" });
    expect(chip.attributes("class")).toContain("border-dashed");
    expect(r1.find('[data-testid="gate-decision-link"]').attributes("data-to")).toContain('"id":"d1"');
    expect(r1.find('[data-testid="gate-decision-link"]').text()).toBe("d1");
    expect(has(w, "gate-decision-service-deleted")).toBe(true);
    expect(r2.find('[data-testid="gate-decision-service-deleted"]').exists()).toBe(true);
    expect(r1.find('[data-testid="gate-decision-service-deleted"]').exists()).toBe(false);
    expect(r2.text()).toContain("not configured");
    expect(r2.text()).toContain("—");
    expect(has(w, "gate-decisions-live-note")).toBe(true);
  });
});

describe("GateDecisionsView — one guard for every read (check 5)", () => {
  it("a 'Show 50 more' page that lands after the filters moved is dropped, and its request was aborted", async () => {
    const more = deferred<Res>();
    const signals: AbortSignal[] = [];
    serve((opts: { params: { query: { cursor?: string } }; signal: AbortSignal }) => {
      signals.push(opts.signal);
      if (opts.params.query.cursor) return more.promise;
      return page([row("d1")], "cur-1");
    });
    const w = mountView();
    await settle();
    await t(w, "gate-decisions-more").trigger("click");
    await settle();
    expect(t(w, "gate-decisions-more").text()).toBe("Loading…");
    // The filters move under the pending page.
    await t(w, "gate-decisions-state").setValue("ALLOW");
    await t(w, "gate-decisions-apply").trigger("click");
    await settle();
    expect(signals[1].aborted, "the stale page's request is aborted").toBe(true);
    expect(rows(w).map((r) => r.attributes("data-id"))).toEqual(["d1"]);
    more.resolve(page([row("d2"), row("d3")]));
    await settle();
    expect(rows(w).map((r) => r.attributes("data-id")), "the stale page never lands").toEqual(["d1"]);
    expect(t(w, "gate-decisions-more").text(), "the new traversal's own state, not the stale page's").toBe("Show 50 more");
  });

  it("unmount aborts the pending first page and its late answer is dropped without error", async () => {
    const first = deferred<Res>();
    const signals: AbortSignal[] = [];
    serve((opts: { signal: AbortSignal }) => {
      signals.push(opts.signal);
      return first.promise;
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const w = mountView();
      await settle();
      expect(signals).toHaveLength(1);
      w.unmount();
      expect(signals[0].aborted).toBe(true);
      first.resolve(page([row("d1")]));
      await settle();
      expect(errors).not.toHaveBeenCalled();
    } finally {
      errors.mockRestore();
    }
  });

  it("a client refusal while a 'Show 50 more' page is pending aborts it and drops its answer", async () => {
    const more = deferred<Res>();
    const signals: AbortSignal[] = [];
    serve((opts: { params: { query: { cursor?: string } }; signal: AbortSignal }) => {
      signals.push(opts.signal);
      return opts.params.query.cursor ? more.promise : page([row("d1")], "cur-1");
    });
    const w = mountView();
    await settle();
    // Apply is disabled while the FIRST page loads, so a refusal can only race a "Show 50 more".
    await t(w, "gate-decisions-more").trigger("click");
    await settle();
    expect(signals).toHaveLength(2);
    await applyRange(w, "2026-08-01", "2026-09-01");
    expect(ledgerCalls(), "a refused range sends nothing").toHaveLength(2);
    expect(signals[1].aborted, "the pending page is aborted by the refusal").toBe(true);
    expect(rows(w)).toHaveLength(0);
    more.resolve(page([row("d2")]));
    await settle();
    expect(rows(w), "its late answer never lands").toHaveLength(0);
    expect(t(w, "gate-decisions-error").attributes("data-status")).toBe("client");
    expect(has(w, "gate-decisions-more"), "no traversal survives a refusal").toBe(false);
  });
});
