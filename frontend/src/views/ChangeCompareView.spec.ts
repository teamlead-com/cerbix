import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { reactive } from "vue";

import ChangeCompareView from "@/views/ChangeCompareView.vue";
import { CHANGE_ERROR_TEXT } from "@/lib/changes";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const routeMock = vi.hoisted(() => ({
  route: { query: {} as Record<string, string>, params: {} as Record<string, string> },
  replaced: [] as unknown[],
}));
vi.mock("vue-router", () => ({
  useRoute: () => routeMock.route,
  useRouter: () => ({
    push: vi.fn(),
    // `router.replace` is what the horizon control calls; here it writes the reactive query, so the
    // view's own route watcher re-reads exactly as it does in the app.
    replace: (to: { query?: Record<string, string> }) => {
      routeMock.replaced.push(to);
      routeMock.route.query = { ...(to.query ?? {}) };
      return Promise.resolve();
    },
  }),
  RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' },
}));
vi.mock("@/components/AppShell.vue", () => ({ default: { name: "AppShell", template: "<div><slot /></div>" } }));

type Ws = { init: () => Promise<void>; orgId: string; projectId: string; orgName: string; projectName: string };
const wsMock = vi.hoisted(() => ({ ws: null as unknown as Ws }));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => wsMock.ws }));

// FR-025 D-0210 item 3, mock screen 3: the comparison view, addressed by `(source, external_id,
// horizon)` — there is NO by-identity group route. What is proven:
//
//   * the query drives the request, verbatim, with `1h` when the link named no horizon;
//   * each side renders in EXACTLY one of the three shapes (D8, D-0211): a figure with its bar and
//     its durations line, `withheld` with the page's own reason word, `pending` with `sealed_through`
//     STATED and no partial number;
//   * Δ exists only when both sides are figures;
//   * the horizon control writes the URL and the view re-reads at the new horizon;
//   * 404 `no_terminal_phase` is that code's sentence and a bare `not found` is this view's own;
//   * a link with no `source` is refused HERE, before any comparison is asked;
//   * an unmount aborts what is in flight.

const SERVICE = "0191c2a4-7f3e-4c1b-9a2d-00000000000f";

type Res = { data?: unknown; error?: unknown; response?: Response };
type Answer = Res | Promise<Res> | ((opts: any) => Res | Promise<Res>);
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

const DETAIL = { service: { id: SERVICE, slug: "checkout", name: "Checkout" } };

const figure = (availability: number, over: Record<string, unknown> = {}) => ({
  from: "2026-08-30T13:05:00Z",
  to: "2026-08-30T14:05:00Z",
  availability,
  good_seconds: 3597,
  bad_seconds: 3,
  unknown_seconds: 0,
  excluded_seconds: 0,
  buckets: 60,
  ...over,
});
const withheld = (reason: string, detail?: string) => ({ from: "2026-08-30T13:05:00Z", to: "2026-08-30T14:05:00Z", withheld: reason, detail });
const pending = (sealed_through?: string) => ({ from: "2026-08-30T14:05:00Z", to: "2026-08-30T15:05:00Z", pending: true, sealed_through });

function compareBody(over: Record<string, unknown> = {}) {
  return {
    source: "github-actions",
    external_id: "1234",
    kind: "deploy",
    ref: "v4.2.1",
    change_id: "0191c2a4-7f3e-4c1b-9a2d-0000000000b1",
    terminal_phase: "succeeded",
    t: "2026-08-30T14:05:00Z",
    horizon: "1h",
    sealed_through: "2026-08-30T16:00:00Z",
    as_of: "2026-08-30T16:01:00Z",
    before: figure(99.94),
    after: figure(98.28, { good_seconds: 3538, bad_seconds: 62 }),
    delta: -1.66,
    ...over,
  };
}

function serve(server: { service?: Answer; compare?: Answer }) {
  const pick = (a: Answer | undefined, fallback: Res, opts: unknown) => {
    if (a === undefined) return Promise.resolve(fallback);
    if (typeof a === "function") return Promise.resolve(a(opts));
    return Promise.resolve(a);
  };
  apiMock.GET.mockImplementation((path: string, opts: unknown) => {
    if (path.endsWith("/changes/compare")) return pick(server.compare, ok(compareBody()), opts);
    if (path.endsWith("/services/{serviceID}")) return pick(server.service, ok(DETAIL), opts);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function workspace(over: Partial<Ws> = {}): Ws {
  return reactive({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API", ...over });
}

const LINK = { source: "github-actions", external_id: "1234" };

function mountView(opts: { query?: Record<string, string>; ws?: Ws } = {}) {
  routeMock.route = reactive({ query: { ...(opts.query ?? LINK) }, params: { id: SERVICE } });
  routeMock.replaced = [];
  wsMock.ws = opts.ws ?? workspace();
  return mount(ChangeCompareView, {
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
const compareCalls = () => apiMock.GET.mock.calls.filter((c) => String(c[0]).endsWith("/changes/compare"));
const lastQuery = () => compareCalls().at(-1)![1].params.query;

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
  apiMock.PUT.mockReset();
  apiMock.DELETE.mockReset();
});

// The comparison is a READ (D-0210 item 7): nothing on this page writes, and every read it makes
// names the identity it is for and rides an AbortSignal.
afterEach(() => {
  expect(apiMock.POST, "the comparison view writes nothing").not.toHaveBeenCalled();
  expect(apiMock.PUT).not.toHaveBeenCalled();
  expect(apiMock.DELETE).not.toHaveBeenCalled();
  for (const c of compareCalls()) {
    const q = c[1].params.query;
    expect(q.source, "a comparison is addressed by identity, never by a group id").toBeTruthy();
    expect(q.external_id).toBeTruthy();
    expect(q.horizon).toBeTruthy();
    expect(c[1].signal).toBeInstanceOf(AbortSignal);
  }
});

describe("ChangeCompareView — the query drives the request", () => {
  it("source, external_id and the default horizon 1h when the link named none", async () => {
    serve({});
    const w = mountView();
    await settle();
    expect(compareCalls()).toHaveLength(1);
    expect(lastQuery()).toEqual({ source: "github-actions", external_id: "1234", horizon: "1h" });
    expect(compareCalls()[0][1].params.path).toEqual({ projectID: "p1", serviceID: SERVICE });
    expect(t(w, "change-compare").attributes("data-horizon")).toBe("1h");
    expect(t(w, "change-compare").attributes("data-source")).toBe("github-actions");
    expect(t(w, "change-compare").attributes("data-external-id")).toBe("1234");
    expect(t(w, "compare-horizon").attributes("data-horizon")).toBe("1h");
  });

  it("the link's horizon travels VERBATIM — even one this SPA does not know, so the server's 400 is what renders", async () => {
    serve({ compare: refused(400, "horizon_invalid (horizon): one of 15m, 1h, 6h, 24h") });
    const w = mountView({ query: { ...LINK, horizon: "3h" } });
    await settle();
    expect(lastQuery().horizon, "never silently corrected into a comparison the link did not ask for").toBe("3h");
    expect(t(w, "compare-error").text()).toContain(CHANGE_ERROR_TEXT.horizon_invalid);
    expect(t(w, "compare-error").attributes("data-status")).toBe("400");
  });

  it("the header quotes the group the comparison came back with: kind, ref, terminal phase and T", async () => {
    serve({});
    const w = mountView();
    await settle();
    expect(t(w, "compare-kind").attributes("data-kind")).toBe("deploy");
    expect(t(w, "compare-kind").text()).toBe("deploy");
    expect(t(w, "compare-ref").text()).toBe("v4.2.1");
    expect(t(w, "compare-terminal").attributes("data-phase")).toBe("succeeded");
    expect(t(w, "compare-t").text()).toBe("T = 2026-08-30 14:05:00Z");
    expect(t(w, "compare-identity").text()).toBe("github-actions · 1234");
  });
});

describe("ChangeCompareView — the three side shapes (D8, D-0211)", () => {
  it("a figure: the percent, the bar and the durations line", async () => {
    serve({});
    const w = mountView();
    await settle();
    expect(t(w, "compare-before").attributes("data-kind")).toBe("figure");
    expect(t(w, "compare-before-figure").text()).toContain("99.94 %");
    expect(t(w, "compare-before-figure").text()).toContain("60 min");
    expect(has(w, "compare-before-bar")).toBe(true);
    expect(t(w, "compare-before-durations").text()).toBe("good 59m 57s · bad 3s · unknown 0 · excluded 0");
    expect(t(w, "compare-before-bar").attributes("aria-label")).toBe("good 59m 57s · bad 3s · unknown 0 · excluded 0");
    expect(t(w, "compare-after").attributes("data-kind")).toBe("figure");
    expect(t(w, "compare-after-figure").text()).toContain("98.28 %");
  });

  it("withheld: the page's own reason word and its explanation, and no number at all", async () => {
    serve({ compare: ok(compareBody({ before: withheld("definition_changed"), delta: undefined })) });
    const w = mountView();
    await settle();
    const side = t(w, "compare-before");
    expect(side.attributes("data-kind")).toBe("withheld");
    expect(side.attributes("data-withheld")).toBe("definition_changed");
    expect(t(w, "compare-before-figure").text()).toContain("withheld · definition changed");
    expect(t(w, "compare-before-figure").text()).not.toMatch(/\d/);
    expect(has(w, "compare-before-bar"), "nothing quoted: nothing drawn").toBe(false);
    expect(side.text()).toContain("a declaration revision or epoch boundary sits inside this side");
  });

  it("withheld `undecidable` keeps the reliability page's own detail when it sent one", async () => {
    serve({ compare: ok(compareBody({ after: withheld("undecidable", "coverage below the page's floor for 41 % of the range"), delta: undefined })) });
    const w = mountView();
    await settle();
    expect(t(w, "compare-after").attributes("data-withheld")).toBe("undecidable");
    expect(t(w, "compare-after-figure").text()).toContain("withheld · undecidable");
    expect(t(w, "compare-after").text()).toContain("coverage below the page's floor for 41 % of the range");
  });

  it("pending: `sealed_through` is STATED and no partial number is shown — on either side", async () => {
    serve({ compare: ok(compareBody({ before: pending("2026-08-30T14:00:00Z"), after: pending("2026-08-30T14:00:00Z"), sealed_through: "2026-08-30T14:00:00Z", delta: undefined })) });
    const w = mountView();
    await settle();
    for (const key of ["before", "after"]) {
      expect(t(w, `compare-${key}`).attributes("data-kind"), key).toBe("pending");
      expect(t(w, `compare-${key}`).attributes("data-pending")).toBe("true");
      const fig = t(w, `compare-${key}-figure`).text();
      expect(fig).toContain("pending");
      expect(fig).toContain("sealed through 2026-08-30 14:00:00Z");
      expect(fig, "no partial figure, ever").not.toMatch(/\d+(\.\d+)? %/);
      expect(has(w, `compare-${key}-bar`)).toBe(false);
    }
    expect(t(w, "compare-info").text()).toContain("a pending side is quoted once it has passed that side's end");
  });

  it("Δ is present only with TWO figures, and is coloured by its sign", async () => {
    serve({});
    const w = mountView();
    await settle();
    expect(t(w, "compare-delta").attributes("data-present")).toBe("true");
    expect(t(w, "compare-delta").attributes("data-sign")).toBe("-1");
    expect(t(w, "compare-delta").text()).toContain("−1.66");
    expect(t(w, "compare-info").text()).toContain("Both sides are fully sealed");

    apiMock.GET.mockReset();
    serve({ compare: ok(compareBody({ after: pending("2026-08-30T14:00:00Z"), delta: undefined })) });
    const w2 = mountView();
    await settle();
    expect(t(w2, "compare-delta").attributes("data-present")).toBe("false");
    expect(t(w2, "compare-delta").text()).toContain("both sides must be figures");
  });
});

describe("ChangeCompareView — the horizon control", () => {
  it("writes the horizon into the query and re-reads at the new one", async () => {
    serve({ compare: (o: any) => ok(compareBody({ horizon: o.params.query.horizon })) });
    const w = mountView();
    await settle();
    expect(compareCalls()).toHaveLength(1);

    await t(w, "compare-horizon-6h").trigger("click");
    await settle();
    expect(routeMock.replaced).toEqual([{ query: { source: "github-actions", external_id: "1234", horizon: "6h" } }]);
    expect(compareCalls()).toHaveLength(2);
    expect(lastQuery().horizon).toBe("6h");
    expect(t(w, "change-compare").attributes("data-horizon")).toBe("6h");
    expect(t(w, "compare-horizon-6h").attributes("aria-pressed")).toBe("true");

    // The horizon already shown asks nothing: no URL write, no read.
    await t(w, "compare-horizon-6h").trigger("click");
    await settle();
    expect(routeMock.replaced).toHaveLength(1);
    expect(compareCalls()).toHaveLength(2);
  });
});

describe("ChangeCompareView — the refusals", () => {
  it("404 `no_terminal_phase` is that code's sentence", async () => {
    serve({ compare: refused(404, "no_terminal_phase") });
    const w = mountView();
    await settle();
    expect(t(w, "compare-error").text()).toContain(CHANGE_ERROR_TEXT.no_terminal_phase);
    expect(t(w, "compare-error").attributes("data-status")).toBe("404");
    expect(has(w, "compare-sides"), "no figure without a comparison").toBe(false);
  });

  it("a bare `not found` on the comparison is THIS view's own sentence — the change, not the service", async () => {
    serve({ compare: refused(404, "not found") });
    const w = mountView();
    await settle();
    expect(t(w, "compare-error").text()).toContain("This change does not exist on this service, or you cannot see it.");
  });

  it("a service that is gone is the SERVICE's sentence, and the comparison is not quoted under it", async () => {
    serve({ service: refused(404, "not found"), compare: ok(compareBody()) });
    const w = mountView();
    await settle();
    expect(t(w, "compare-error").text()).toContain("This service does not exist, or you cannot see it.");
    expect(has(w, "compare-sides")).toBe(false);
  });

  it("a link with no `source` is refused HERE, before any comparison is asked", async () => {
    serve({});
    const w = mountView({ query: { external_id: "1234" } });
    await settle();
    expect(compareCalls(), "nothing is asked for a link that names no change").toHaveLength(0);
    expect(t(w, "compare-error").text()).toContain("This link names no change — it needs a source and an external_id.");
    expect(t(w, "compare-error").attributes("data-status")).toBe("client");
    expect(t(w, "compare-identity").text(), "the half the link did name is still shown").toBe("? · 1234");
  });

  it("a link with no `external_id` is refused the same way", async () => {
    serve({});
    const w = mountView({ query: { source: "github-actions" } });
    await settle();
    expect(compareCalls()).toHaveLength(0);
    expect(t(w, "compare-error").attributes("data-status")).toBe("client");
  });

  it("401 and 403 are one line; 429 names the Retry-After seconds; a network failure keeps its own words", async () => {
    serve({ compare: refused(403, "") });
    const w = mountView();
    await settle();
    expect(t(w, "compare-error").text()).toContain("You cannot read this service's changes.");

    apiMock.GET.mockReset();
    serve({ compare: refused(429, "process_inflight", { "Retry-After": "7" }) });
    const w2 = mountView();
    await settle();
    expect(t(w2, "compare-error").text()).toContain("The change reads are busy right now. Try again in 7 s.");

    apiMock.GET.mockReset();
    serve({ compare: () => Promise.reject(new Error("Failed to fetch")) });
    const w3 = mountView();
    await settle();
    expect(t(w3, "compare-error").text()).toContain("Could not reach the server: Failed to fetch");
    expect(t(w3, "compare-error").attributes("data-status")).toBe("0");
  });
});

describe("ChangeCompareView — concurrency (D-0210 item 7)", () => {
  it("unmount aborts the reads in flight and the late answer is never applied", async () => {
    const gate = deferred<Res>();
    const signals: AbortSignal[] = [];
    serve({
      service: (o: any) => {
        signals.push(o.signal);
        return ok(DETAIL);
      },
      compare: (o: any) => {
        signals.push(o.signal);
        return gate.promise;
      },
    });
    const w = mountView();
    await flushPromises();
    expect(signals).toHaveLength(2);
    expect(signals.some((s) => s.aborted)).toBe(false);

    w.unmount();
    // The service read had already RESOLVED, so its controller left the scope's in-flight set and
    // there is nothing there to abort; the comparison is what is still open. Until the shared
    // `requestScope` this view used ONE controller for both reads, which made `every` the right
    // check; now each request owns its own, and asserting on the settled one would be bookkeeping
    // rather than behaviour.
    expect(signals[1].aborted, "the read still in flight is aborted").toBe(true);
    gate.resolve(ok(compareBody()));
    await settle();
    expect(compareCalls(), "nothing new is asked after unmount").toHaveLength(1);
  });

  it("a change of identity in the query aborts the previous read and asks for the new one", async () => {
    const signals: AbortSignal[] = [];
    const gates: ReturnType<typeof deferred<Res>>[] = [];
    serve({
      compare: (o: any) => {
        signals.push(o.signal);
        const d = deferred<Res>();
        gates.push(d);
        return d.promise;
      },
    });
    const w = mountView();
    await flushPromises();
    expect(compareCalls()).toHaveLength(1);

    routeMock.route.query = { source: "argo", external_id: "9999" };
    await settle();
    expect(signals[0].aborted, "the previous identity's read is aborted").toBe(true);
    expect(compareCalls()).toHaveLength(2);
    expect(lastQuery()).toEqual({ source: "argo", external_id: "9999", horizon: "1h" });

    gates[0].resolve(ok(compareBody({ ref: "STALE" })));
    await settle();
    expect(w.text(), "the previous identity's answer is dropped").not.toContain("STALE");
  });


  // Review [31]: the comparison is ANCILLARY and must never suppress an answer about the SUBJECT.
  // The view used to `await Promise.all([service, compare])`, so a comparison that never settles —
  // a blackholed transport, not merely a slow one — held the page on "Loading…" forever while the
  // service's own 404 was already in hand. `deferred()` here is never resolved on purpose: that is
  // the whole condition, and a test that resolves it late would prove something weaker.
  it("renders the service's own refusal even when the comparison never settles", async () => {
    const stuck = deferred<Res>();
    serve({ service: refused(404, "not found"), compare: stuck.promise });
    const w = mountView();
    await settle();

    expect(has(w, "compare-loading")).toBe(false);
    expect(t(w, "compare-error").text()).toContain("This service does not exist");
    expect(t(w, "compare-error").attributes("data-status")).toBe("404");

    // And a late arrival cannot overwrite the decided page: nothing rendered from `cmp` appears.
    // (`compare-ref` is NOT the check — it falls back to the link's own external_id, so it reads
    // "1234" whether or not a comparison ever arrived. The `v-if="cmp"` chips are the honest ones.)
    stuck.resolve(ok(compareBody()));
    await settle();
    expect(t(w, "compare-error").text()).toContain("This service does not exist");
    expect(has(w, "compare-kind")).toBe(false);
    expect(has(w, "compare-terminal")).toBe(false);
    expect(has(w, "compare-t")).toBe(false);
  });

  it("does the same for a denied service, and abandons the comparison rather than waiting on it", async () => {
    const stuck = deferred<Res>();
    serve({ service: refused(403, "forbidden"), compare: stuck.promise });
    const w = mountView();
    await settle();

    expect(has(w, "compare-loading")).toBe(false);
    expect(t(w, "compare-error").text()).toContain("You cannot see this service.");
    expect(t(w, "compare-error").attributes("data-status")).toBe("403");
    // The comparison was issued — the two go out together — and then abandoned.
    expect(compareCalls().length).toBe(1);
    expect(compareCalls()[0][1].signal.aborted).toBe(true);
  });


  // Review [42]: [31] made the SUBJECT decisive, which fixed the case where the service refuses. It
  // left the other half — service 200 and a comparison that never settles — and the commit that
  // moved `requestScope` into `lib/changes.ts` claimed every comparison was bounded in time while
  // this view, its third caller, was still outside. It is on the scope now, so the 10 s deadline
  // applies here too: fake timers, and the page ends in a visible transport error, not a spinner.
  it("a blackholed comparison under a healthy service ends at the deadline, not never", async () => {
    vi.useFakeTimers();
    try {
      serve({ service: ok(DETAIL), compare: () => new Promise<Res>(() => {}) });
      const w = mountView();
      await settle();

      // The subject arrived; the comparison has not, and the page is honestly still loading.
      expect(has(w, "compare-loading")).toBe(true);
      expect(has(w, "compare-error")).toBe(false);

      await vi.advanceTimersByTimeAsync(10_000);
      await settle();

      expect(has(w, "compare-loading")).toBe(false);
      expect(has(w, "compare-error")).toBe(true);
      expect(t(w, "compare-error").attributes("data-status")).toBe("0");
      // The header still shows the subject that DID arrive — the deadline ends the comparison, not
      // the page.
      expect(t(w, "compare-identity").text()).toContain("github-actions");
    } finally {
      vi.useRealTimers();
    }
  });
});
