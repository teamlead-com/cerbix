import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { reactive } from "vue";

import ServiceDetailView from "@/views/ServiceDetailView.vue";

// FR-022, mock panel 3: the service page LINKS its open incident and does not embed it — one
// timeline, one owner. The link is a claim about THIS service RIGHT NOW, so the cases that matter
// are the ones where a nearby incident must not produce it: another service's, and a resolved one.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
// The route is replaceable per test: the stale-navigation test below needs a REACTIVE one so the
// view's own `watch([route.params.id, ws.projectId])` fires the way it does under the real router.
const routeMock = vi.hoisted(() => ({ route: { params: { id: "svc1" } } as { params: { id: string } } }));
vi.mock("vue-router", () => ({
  useRoute: () => routeMock.route,
  useRouter: () => ({ push: vi.fn() }),
  RouterLink: { props: ["to"], template: '<a :data-to="to.params?.id"><slot /></a>' },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true, canProjectAdmin: () => true }),
}));
// Swappable per test: the cold-load test below needs a REACTIVE store whose `projectId` is empty until
// `init()` has answered, which is what the real one looks like on a fresh page.
const wsMock = vi.hoisted(() => {
  const STATIC = { init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API" };
  return { STATIC, current: STATIC as Record<string, unknown> };
});
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => wsMock.current }));

const RouterLink = { props: ["to"], template: '<a :data-to="to.params?.id"><slot /></a>' };
const CHILD = { template: "<div />" };

// The whole ServiceDetail shape, not a convenient subset: the template reads `materialization`
// and `supersedes` unguarded, and a half-fixture renders an empty body that would make every
// negative assertion below pass for the wrong reason.
const SERVICE = {
  service: { id: "svc1", slug: "checkout", name: "Checkout", created_at: "", updated_at: "" },
  declaration: null,
  epoch: null,
  materialization: { repairing: [] },
  reliability: null,
  supersedes: [],
  sla_targets: [],
};

function mountWith(incidents: Record<string, unknown>[]) {
  routeMock.route = { params: { id: "svc1" } };
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    const p = String(path ?? "");
    if (p.endsWith("/incidents")) return Promise.resolve({ data: incidents });
    if (p.endsWith("/monitors")) return Promise.resolve({ data: [] });
    return Promise.resolve({ data: SERVICE });
  });
  return mount(ServiceDetailView, {
    global: {
      stubs: {
        RouterLink,
        ServiceReliability: CHILD,
        ServiceAlerting: CHILD,
        ServiceGate: CHILD,
        ServiceDependencies: CHILD,
      },
    },
  });
}

describe("ServiceDetailView open incident", () => {
  beforeEach(() => apiMock.GET.mockReset());

  it("links the service's OPEN incident by title", async () => {
    const w = mountWith([
      { id: "inc1", service_id: "svc1", status: "investigating", title: "Checkout degraded" },
    ]);
    await flushPromises();
    await flushPromises();
    expect(w.text(), "the fixture must render the service page itself, or every assertion below passes vacuously").toContain("checkout");
    const link = w.get('[data-testid="service-open-incident"]');
    expect(link.text()).toContain("Checkout degraded");
    expect(link.attributes("data-to")).toBe("inc1");
  });

  it("shows nothing when the service's incident is RESOLVED", async () => {
    const w = mountWith([
      { id: "inc1", service_id: "svc1", status: "resolved", title: "Checkout degraded" },
    ]);
    await flushPromises();
    await flushPromises();
    expect(w.text(), "the fixture must render the service page itself, or every assertion below passes vacuously").toContain("checkout");
    expect(w.find('[data-testid="service-open-incident"]').exists()).toBe(false);
  });

  it("does not borrow ANOTHER service's open incident", async () => {
    const w = mountWith([
      { id: "inc2", service_id: "svc-other", status: "investigating", title: "Payments degraded" },
      { id: "inc3", monitor_id: "mon1", status: "investigating", title: "api-http down" },
    ]);
    await flushPromises();
    await flushPromises();
    expect(w.text(), "the fixture must render the service page itself, or every assertion below passes vacuously").toContain("checkout");
    expect(w.find('[data-testid="service-open-incident"]').exists()).toBe(false);
    expect(w.text()).not.toContain("Payments degraded");
  });
});

// P1 [90] (iter-0163 §1.13 review): the parent's `load()` had no generation guard. Navigate A→B while
// A's GET is delayed and A's answer landed LAST — so `detail` was A's while the route said B, and the
// `ServiceGate` below received `serviceId=B` with A's `slaTargets`, slug and managed marker: it could
// offer A's windows for B. The order of resolution here is the adversarial one, deliberately.
describe("ServiceDetailView stale navigation (P1 [90])", () => {
  function deferred<T>() {
    let resolve!: (v: T) => void;
    const promise = new Promise<T>((res) => (resolve = res));
    return { promise, resolve };
  }
  const detailOf = (id: string, slug: string, name: string, windows: string[], managed = "") => ({
    ...SERVICE,
    service: { id, slug, name, created_at: "", updated_at: "", ...(managed ? { managed_by: managed } : {}) },
    sla_targets: windows.map((w) => ({ window: w, objective: 99.9, updated_at: "" })),
  });
  // A stub that REPORTS the props it received — the assertion is about what the card is handed.
  const GateProbe = {
    props: ["projectId", "serviceId", "serviceSlug", "slaTargets", "managedBy"],
    template:
      '<div data-testid="gate-probe" :data-service-id="serviceId" :data-slug="serviceSlug" :data-managed="managedBy" :data-windows="(slaTargets || []).map((t) => t.window).join(\',\')" />',
  };

  it("P1 [90]: A's late answer never lands on B's screen — detail, and the gate card's props, are B's; A's request is aborted", async () => {
    const pending: Record<string, ReturnType<typeof deferred<{ data: unknown }>>> = {};
    const signals: Record<string, AbortSignal> = {};
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation((path: string, opts: { params: { path: { serviceID?: string } }; signal?: AbortSignal }) => {
      const p = String(path ?? "");
      if (p.endsWith("/services/{serviceID}")) {
        const id = opts.params.path.serviceID!;
        signals[id] = opts.signal!;
        pending[id] ??= deferred();
        return pending[id].promise;
      }
      return Promise.resolve({ data: [] }); // monitors, incidents
    });
    routeMock.route = reactive({ params: { id: "svcA" } });
    const w = mount(ServiceDetailView, {
      global: { stubs: { RouterLink, ServiceReliability: CHILD, ServiceAlerting: CHILD, ServiceGate: GateProbe, ServiceDependencies: CHILD } },
    });
    await flushPromises();
    await flushPromises();
    expect(signals.svcA, "A's read is in flight and carries a signal").toBeInstanceOf(AbortSignal);
    expect(signals.svcA.aborted).toBe(false);

    // Navigate A → B while A is still pending.
    routeMock.route.params.id = "svcB";
    await flushPromises();
    await flushPromises();
    expect(signals.svcA.aborted, "the superseded load is aborted, not merely ignored").toBe(true);
    expect(signals.svcB).toBeInstanceOf(AbortSignal);
    expect(signals.svcB.aborted).toBe(false);

    // B answers first…
    pending.svcB.resolve({ data: detailOf("svcB", "payments", "Payments", ["7d"], "payments-bundle") });
    await flushPromises();
    await flushPromises();
    expect(w.find("h1").text()).toBe("Payments");
    const probe = () => w.get('[data-testid="gate-probe"]').attributes();
    expect(probe()).toMatchObject({ "data-service-id": "svcB", "data-slug": "payments", "data-windows": "7d", "data-managed": "payments-bundle" });

    // …and A answers LAST. Nothing may move.
    pending.svcA.resolve({ data: detailOf("svcA", "checkout", "Checkout", ["30d", "90d"]) });
    await flushPromises();
    await flushPromises();
    expect(w.find("h1").text(), "detail is still B's").toBe("Payments");
    expect(w.text()).not.toContain("Checkout");
    expect(probe(), "the gate card is handed B's inventory, slug and managed marker — never A's").toMatchObject({
      "data-service-id": "svcB",
      "data-slug": "payments",
      "data-windows": "7d",
      "data-managed": "payments-bundle",
    });
  });

  it("unmount aborts the pending load and its late answer is dropped without error", async () => {
    const d = deferred<{ data: unknown }>();
    let signal: AbortSignal | undefined;
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation((path: string, opts: { signal?: AbortSignal }) => {
      if (String(path).endsWith("/services/{serviceID}")) {
        signal = opts.signal;
        return d.promise;
      }
      return Promise.resolve({ data: [] });
    });
    routeMock.route = reactive({ params: { id: "svcA" } });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const w = mount(ServiceDetailView, {
        global: { stubs: { RouterLink, ServiceReliability: CHILD, ServiceAlerting: CHILD, ServiceGate: CHILD, ServiceDependencies: CHILD } },
      });
      await flushPromises();
      w.unmount();
      expect(signal!.aborted).toBe(true);
      d.resolve({ data: detailOf("svcA", "checkout", "Checkout", ["30d"]) });
      await flushPromises();
      await flushPromises();
      expect(errors).not.toHaveBeenCalled();
    } finally {
      errors.mockRestore();
    }
  });
});

// iter-0164: where the LATE `alerting` prop that clobbered the typed cadence came from. On a cold load
// `ws.projectId` is "" until `ws.init()` — awaited INSIDE load() — sets it, and the view's own
// `watch([route.params.id, ws.projectId])` then starts a SECOND load() (whose init runs in full again,
// because the first has not marked the store loaded yet). Before P1 [90] both loads assigned `detail`,
// so the panel was handed the declaration TWICE: the second one two round trips (orgs, projects)
// after the first, a new object with the same values, which rebuilt the draft under whatever the
// operator had typed in between. With the guard the first load is superseded before it reads, and the
// panel is handed the declaration exactly once. Run against the pre-[90] view this test fails on both
// of the last two assertions (2 reads, 2 deliveries) — that is the reproduction.
describe("ServiceDetailView cold load (iter-0164)", () => {
  it("hands the panel ONE declaration although ws.init() re-triggers load()", async () => {
    const deliveries: unknown[] = [];
    const AlertingProbe = {
      props: ["alerting"],
      template: '<div data-testid="alerting-probe" />',
      watch: { alerting: { immediate: true, handler(v: unknown) { if (v) deliveries.push(v); } } },
    };
    const reads: string[] = [];
    let inits = 0;
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation((path: string) => {
      const p = String(path ?? "");
      reads.push(p);
      if (p.endsWith("/organizations")) return Promise.resolve({ data: [{ id: "o1", slug: "acme" }] });
      if (p.endsWith("/projects")) return Promise.resolve({ data: [{ id: "p1", slug: "api" }] });
      if (p.endsWith("/services/{serviceID}")) {
        // A FRESH object per read, as JSON parsing gives: identity is what the panel's watcher sees.
        return Promise.resolve({
          data: { ...SERVICE, alerting: { owns_paging: false, page_on: ["down"], page_on_unknown: false, confirm_evaluations: 2, renotify_seconds: 0 } },
        });
      }
      return Promise.resolve({ data: [] }); // monitors, incidents
    });
    // The real store's shape, reduced to what matters: `projectId` lands only after two awaited reads,
    // it is set INSIDE an awaited `loadProjects()` — so the watcher it triggers runs before `loaded` is
    // marked — and an init that starts before the first has finished therefore runs the whole thing
    // again. (Set `loaded` in the same tick as `projectId` and the second init is a no-op: the tick
    // boundary is what makes the double load real.)
    wsMock.current = reactive({
      orgId: "", projectId: "", loaded: false, orgName: "", projectName: "",
      async init() {
        if (this.loaded) return;
        inits++;
        const orgs = (await apiMock.GET("/api/v1/organizations")) as { data: { id: string }[] };
        this.orgId = orgs.data[0].id;
        await this.loadProjects();
        this.loaded = true;
      },
      async loadProjects() {
        const projects = (await apiMock.GET("/api/v1/organizations/{orgID}/projects")) as { data: { id: string }[] };
        this.projectId = projects.data[0].id;
      },
    });
    routeMock.route = reactive({ params: { id: "svc1" } });
    try {
      const w = mount(ServiceDetailView, {
        global: { stubs: { RouterLink, ServiceReliability: CHILD, ServiceAlerting: AlertingProbe, ServiceGate: CHILD, ServiceDependencies: CHILD } },
      });
      for (let i = 0; i < 8; i++) await flushPromises();

      expect(w.text(), "the page rendered").toContain("checkout");
      expect(inits, "the projectId watcher fires from inside the first init, so load() runs twice").toBe(2);
      expect(reads.filter((p) => p.endsWith("/services/{serviceID}")).length,
        "…but only the surviving load reads the service").toBe(1);
      expect(deliveries.length, "and the panel is handed the declaration exactly once").toBe(1);
    } finally {
      wsMock.current = wsMock.STATIC;
    }
  });
});
