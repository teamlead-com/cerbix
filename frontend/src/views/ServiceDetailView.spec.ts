import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import ServiceDetailView from "@/views/ServiceDetailView.vue";

// FR-022, mock panel 3: the service page LINKS its open incident and does not embed it — one
// timeline, one owner. The link is a claim about THIS service RIGHT NOW, so the cases that matter
// are the ones where a nearby incident must not produce it: another service's, and a resolved one.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: { id: "svc1" } }),
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
