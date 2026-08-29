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
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API" }),
}));

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
