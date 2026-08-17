import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import ServiceDependencies from "@/components/ServiceDependencies.vue";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), PUT: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

// FR-021 phase 3 (§14.2/§14.4), against the approved mock: the two edge lists with the
// two-layer neighbour health, and the editor's THREE honesty states — a cycle, a stale
// `graph_generation` (409, with the Reload that keeps the other writer's set) and the
// count bound — each rendered as ITSELF from the API's own message, never as a generic
// "could not save".

type Edge = {
  id: string;
  slug: string;
  name: string;
  managed_by?: string;
  health?: { sli: string; diagnostics?: string; failing_monitors?: string[] } | null;
};

const RouterLink = { props: ["to"], template: "<a><slot /></a>" };

function mountWith(opts: {
  graph?: { graph_generation: number; depends_on?: Edge[]; depended_on_by?: Edge[] };
  graphFails?: boolean;
  graphRejects?: boolean;
  services?: { service: { id: string; slug: string; name: string } }[];
  putError?: string;
  putRejects?: boolean;
  canWrite?: boolean;
  managedBy?: string;
}) {
  apiMock.GET.mockReset();
  apiMock.PUT.mockReset();
  let reads = 0;
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/dependencies")) {
      reads++;
      if (opts.graphRejects) return Promise.reject(new TypeError("fetch failed"));
      if (opts.graphFails) return Promise.resolve({ error: { error: "boom" } });
      return Promise.resolve({
        data: opts.graph ?? { graph_generation: 0, depends_on: [], depended_on_by: [] },
      });
    }
    if (path.endsWith("/services")) return Promise.resolve({ data: opts.services ?? [] });
    return Promise.resolve({ data: {} });
  });
  apiMock.PUT.mockImplementation(() => {
    if (opts.putRejects) return Promise.reject(new TypeError("fetch failed"));
    if (opts.putError) return Promise.resolve({ error: { error: opts.putError } });
    return Promise.resolve({ data: { graph_generation: 9, depends_on: [], depended_on_by: [] } });
  });
  const wrapper = mount(ServiceDependencies, {
    props: {
      projectId: "p1",
      serviceId: "svc-checkout",
      canWrite: opts.canWrite ?? true,
      managedBy: opts.managedBy ?? "",
    },
    global: { stubs: { RouterLink } },
  });
  return { wrapper, reads: () => reads };
}

describe("ServiceDependencies", () => {
  it("renders both directions with the neighbour health and the edge count", async () => {
    const { wrapper } = mountWith({
      graph: {
        graph_generation: 3,
        depends_on: [
          { id: "svc-pay", slug: "payments", name: "Payments", health: { sli: "down" } },
          { id: "svc-redis", slug: "session-redis", name: "Redis", health: { sli: "healthy" } },
        ],
        depended_on_by: [{ id: "svc-store", slug: "storefront", name: "Storefront", health: { sli: "degraded" } }],
      },
    });
    await flushPromises();

    const up = wrapper.findAll('[data-testid="svc-dep-upstream"]');
    expect(up).toHaveLength(2);
    expect(up[0].text()).toContain("payments");
    expect(wrapper.findAll('[data-testid="svc-dep-downstream"]')).toHaveLength(1);
    expect(wrapper.get('[data-testid="svc-dep-count"]').text()).toContain("2 / 20");

    const pills = wrapper.findAll('[data-testid="svc-dep-health"]');
    expect(pills[0].text()).toBe("Down");
    expect(pills[0].classes()).toContain("text-down");
    expect(pills[1].text()).toBe("Operational");
  });

  it("renders a neighbour whose health is ABSENT as unknown, never as operational", async () => {
    const { wrapper } = mountWith({
      graph: {
        graph_generation: 1,
        depends_on: [{ id: "svc-pay", slug: "payments", name: "Payments", health: null }],
        depended_on_by: [],
      },
    });
    await flushPromises();
    const pill = wrapper.get('[data-testid="svc-dep-health"]');
    expect(pill.text()).toBe("unknown");
    expect(pill.classes()).toContain("text-ink-3");
  });

  it("renders a rejected read as an error, not as an empty graph", async () => {
    const { wrapper } = mountWith({ graphRejects: true });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-dep-error"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="svc-dep-upstream-empty"]').exists()).toBe(false);
  });

  it("hides the editor from a reader", async () => {
    const { wrapper } = mountWith({ canWrite: false });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-dep-edit"]').exists()).toBe(false);
  });

  it("saves the replace-set with the token that was read with the list", async () => {
    const { wrapper } = mountWith({
      graph: { graph_generation: 4, depends_on: [], depended_on_by: [] },
      services: [
        { service: { id: "svc-pay", slug: "payments", name: "Payments" } },
        { service: { id: "svc-checkout", slug: "checkout", name: "Checkout" } },
      ],
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();

    // The service itself is never offered as its own dependency.
    const options = wrapper.findAll('[data-testid="svc-dep-option"]');
    expect(options).toHaveLength(1);
    expect(options[0].text()).toContain("payments");
    expect(wrapper.get('[data-testid="svc-dep-token"]').text()).toContain("4");

    await options[0].get("input").setValue(true);
    await wrapper.get('[data-testid="svc-dep-save"]').trigger("click");
    await flushPromises();

    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    const body = apiMock.PUT.mock.calls[0][1].body;
    expect(body).toEqual({ depends_on: ["svc-pay"], graph_generation: 4 });
    // A successful save closes the editor and adopts the returned set.
    expect(wrapper.find('[data-testid="svc-dep-editor"]').exists()).toBe(false);
  });

  it("renders a CYCLE rejection as itself and keeps the editor open", async () => {
    const { wrapper } = mountWith({
      graph: { graph_generation: 1, depends_on: [], depended_on_by: [] },
      services: [{ service: { id: "svc-store", slug: "storefront", name: "Storefront" } }],
      putError: "dependency_cycle: the graph is a DAG",
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-save"]').trigger("click");
    await flushPromises();

    const err = wrapper.get('[data-testid="svc-dep-save-error"]');
    expect(err.text()).toContain("dependency_cycle");
    expect(wrapper.find('[data-testid="svc-dep-editor"]').exists()).toBe(true);
    // A cycle is the author's own edit to fix — no Reload affordance.
    expect(wrapper.find('[data-testid="svc-dep-reload"]').exists()).toBe(false);
  });

  it("renders a STALE token as a 409 with a Reload that adopts the other writer's set", async () => {
    const { wrapper, reads } = mountWith({
      graph: { graph_generation: 1, depends_on: [], depended_on_by: [] },
      services: [{ service: { id: "svc-pay", slug: "payments", name: "Payments" } }],
      putError: "graph_generation_stale: dependencies changed concurrently — reload and retry",
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-save"]').trigger("click");
    await flushPromises();

    expect(wrapper.get('[data-testid="svc-dep-save-error"]').text()).toContain("graph_generation_stale");
    const reload = wrapper.get('[data-testid="svc-dep-reload"]');
    const before = reads();
    await reload.trigger("click");
    await flushPromises();
    // The Reload re-reads rather than re-submitting over the other writer.
    expect(reads()).toBe(before + 1);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(wrapper.find('[data-testid="svc-dep-save-error"]').exists()).toBe(false);
  });

  it("renders the count BOUND rejection as itself", async () => {
    const { wrapper } = mountWith({
      graph: { graph_generation: 1, depends_on: [], depended_on_by: [] },
      services: [{ service: { id: "svc-pay", slug: "payments", name: "Payments" } }],
      putError: "too_many_dependencies: at most 20 direct dependencies",
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-save"]').trigger("click");
    await flushPromises();
    expect(wrapper.get('[data-testid="svc-dep-save-error"]').text()).toContain("too_many_dependencies");
  });

  it("renders a REJECTED save (network failure) exactly like an error payload", async () => {
    const { wrapper } = mountWith({
      graph: { graph_generation: 1, depends_on: [], depended_on_by: [] },
      services: [{ service: { id: "svc-pay", slug: "payments", name: "Payments" } }],
      putRejects: true,
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-save"]').trigger("click");
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-dep-save-error"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="svc-dep-editor"]').exists()).toBe(true);
  });

  it("shows the ownership notice on the READ surface, where a managed service can see it", async () => {
    // Production prop combination ([298] P2-2): the parent computes canWrite as
    // role-write AND !managed, so a managed service has NO Edit button — a notice living
    // inside the editor would be unreachable.
    const { wrapper } = mountWith({
      graph: { graph_generation: 1, depends_on: [], depended_on_by: [] },
      managedBy: "file:shop.yaml",
      canWrite: false,
    });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-dep-edit"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="svc-dep-managed"]').text()).toContain("file:shop.yaml");
  });

  it("ignores a deferred read from a previous service after the context changed", async () => {
    let resolveFirst: ((v: unknown) => void) | null = null;
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementationOnce(() => new Promise((r) => (resolveFirst = r)));
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/dependencies")) {
        return Promise.resolve({
          data: {
            graph_generation: 2,
            depends_on: [{ id: "svc-b", slug: "b-service", name: "B" }],
            depended_on_by: [],
          },
        });
      }
      return Promise.resolve({ data: [] });
    });
    const wrapper = mount(ServiceDependencies, {
      props: { projectId: "p1", serviceId: "svc-a", canWrite: true },
      global: { stubs: { RouterLink } },
    });
    await wrapper.setProps({ serviceId: "svc-b" });
    await flushPromises();
    // The first (stale) response lands only now.
    resolveFirst?.({
      data: { graph_generation: 99, depends_on: [{ id: "svc-a", slug: "a-service", name: "A" }], depended_on_by: [] },
    });
    await flushPromises();

    const up = wrapper.findAll('[data-testid="svc-dep-upstream"]');
    expect(up).toHaveLength(1);
    expect(up[0].text()).toContain("b-service");
  });
});

describe("ServiceDependencies — [298] fix pass", () => {
  // P1-3: the two layers are independent. A neighbour whose SLI is healthy while its
  // diagnostics are failing must SAY so; one merged word would hide the operational
  // failure the two-layer card exists to surface.
  it("renders BOTH health layers, including a healthy SLI over failing diagnostics", async () => {
    const { wrapper } = mountWith({
      graph: {
        graph_generation: 1,
        depends_on: [
          {
            id: "svc-pay",
            slug: "payments",
            name: "Payments",
            health: { sli: "healthy", diagnostics: "failing", failing_monitors: ["payments-db"] },
          },
        ],
        depended_on_by: [],
      },
    });
    await flushPromises();
    expect(wrapper.get('[data-testid="svc-dep-health"]').text()).toBe("Operational");
    const diag = wrapper.get('[data-testid="svc-dep-diagnostics"]');
    expect(diag.text()).toContain("diagnostics failing");
    expect(diag.text()).toContain("payments-db");
    expect(diag.classes()).toContain("text-down");
  });

  // [301] P1-2: an absent health payload must render BOTH layers as unknown — a blank
  // second line reads as a rendered fact rather than a missing one.
  it("renders an absent health as unknown on BOTH layers, upstream and downstream", async () => {
    const { wrapper } = mountWith({
      graph: {
        graph_generation: 1,
        depends_on: [{ id: "svc-pay", slug: "payments", name: "Payments", health: null }],
        depended_on_by: [{ id: "svc-store", slug: "storefront", name: "Storefront", health: null }],
      },
    });
    await flushPromises();
    expect(wrapper.get('[data-testid="svc-dep-health"]').text()).toBe("unknown");
    expect(wrapper.get('[data-testid="svc-dep-diagnostics"]').text()).toBe("diagnostics unknown");
    expect(wrapper.get('[data-testid="svc-dep-downstream-health"]').text()).toBe("unknown");
    // The downstream row carries its own diagnostics line, equally non-blank.
    const downstreamDiag = wrapper.get('[data-testid="svc-dep-downstream"]').findAll("span").at(-1);
    expect(downstreamDiag?.text()).toBe("diagnostics unknown");
  });

  // [301] P2-1: a cancelled open must not have its deferred candidate list land in the
  // reopen that followed it.
  it("discards a cancelled open's deferred candidate list when the editor is reopened", async () => {
    const resolvers: ((v: unknown) => void)[] = [];
    apiMock.GET.mockReset();
    apiMock.PUT.mockReset();
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/dependencies")) {
        return Promise.resolve({ data: { graph_generation: 1, depends_on: [], depended_on_by: [] } });
      }
      return new Promise((resolve) => resolvers.push(resolve));
    });
    const wrapper = mount(ServiceDependencies, {
      props: { projectId: "p1", serviceId: "svc-checkout", canWrite: true },
      global: { stubs: { RouterLink } },
    });
    await flushPromises();

    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click"); // open A
    await wrapper.get('[data-testid="svc-dep-cancel"]').trigger("click");
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click"); // reopen B
    // B answers first, then the abandoned A answers last.
    resolvers[1]({ data: [{ service: { id: "svc-b", slug: "b-current", name: "B" } }] });
    await flushPromises();
    resolvers[0]({ data: [{ service: { id: "svc-a", slug: "a-stale", name: "A" } }] });
    await flushPromises();

    const options = wrapper.findAll('[data-testid="svc-dep-option"]');
    expect(options).toHaveLength(1);
    expect(options[0].text()).toContain("b-current");
  });

  // P1-4: a file-owned DEPENDENT pins this service — the reader must be able to predict
  // the 409 on delete, which is what the approved mock's chip is for.
  it("marks a file-owned neighbour on both directions", async () => {
    const { wrapper } = mountWith({
      graph: {
        graph_generation: 1,
        depends_on: [{ id: "svc-pay", slug: "payments", name: "Payments", health: { sli: "healthy" } }],
        depended_on_by: [
          { id: "svc-store", slug: "storefront", name: "Storefront", managed_by: "file:shop.yaml", health: { sli: "healthy" } },
        ],
      },
    });
    await flushPromises();
    const chips = wrapper.findAll('[data-testid="svc-dep-managed-chip"]');
    expect(chips).toHaveLength(1);
    expect(chips[0].text()).toContain("file:shop.yaml");
    // The UI-owned upstream row carries no chip.
    expect(wrapper.get('[data-testid="svc-dep-upstream"]').find('[data-testid="svc-dep-managed-chip"]').exists()).toBe(false);
  });

  // P1-2: a failed candidate list is not the honest empty-project state, and the editor
  // must not be able to submit a set it never saw.
  it("shows a candidate-list failure as itself and disables Save", async () => {
    apiMock.GET.mockReset();
    apiMock.PUT.mockReset();
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/dependencies")) {
        return Promise.resolve({ data: { graph_generation: 2, depends_on: [], depended_on_by: [] } });
      }
      return Promise.reject(new TypeError("fetch failed"));
    });
    const wrapper = mount(ServiceDependencies, {
      props: { projectId: "p1", serviceId: "svc-checkout", canWrite: true },
      global: { stubs: { RouterLink } },
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();

    expect(wrapper.get('[data-testid="svc-dep-candidates-error"]').exists()).toBe(true);
    expect(wrapper.text()).not.toContain("No other services in this project");
    expect(wrapper.get('[data-testid="svc-dep-save"]').attributes("disabled")).toBeDefined();
    await wrapper.get('[data-testid="svc-dep-save"]').trigger("click");
    await flushPromises();
    expect(apiMock.PUT).not.toHaveBeenCalled();
  });

  it("shows an HTTP-error candidate list the same way as a rejected one", async () => {
    const { wrapper } = mountWith({
      graph: { graph_generation: 1, depends_on: [], depended_on_by: [] },
    });
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/dependencies")) {
        return Promise.resolve({ data: { graph_generation: 1, depends_on: [], depended_on_by: [] } });
      }
      return Promise.resolve({ error: { error: "services list exploded" } });
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();
    expect(wrapper.get('[data-testid="svc-dep-candidates-error"]').text()).toContain("services list exploded");
    expect(wrapper.get('[data-testid="svc-dep-save"]').attributes("disabled")).toBeDefined();
  });

  // P2-1: the draft belongs to the context it was typed in.
  it("discards an OPEN editor's draft when the service context changes", async () => {
    apiMock.GET.mockReset();
    apiMock.PUT.mockReset();
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/dependencies")) {
        return Promise.resolve({ data: { graph_generation: 5, depends_on: [], depended_on_by: [] } });
      }
      return Promise.resolve({ data: [{ service: { id: "svc-pay", slug: "payments", name: "Payments" } }] });
    });
    const wrapper = mount(ServiceDependencies, {
      props: { projectId: "p1", serviceId: "svc-a", canWrite: true },
      global: { stubs: { RouterLink } },
    });
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-edit"]').trigger("click");
    await flushPromises();
    await wrapper.get('[data-testid="svc-dep-option"] input').setValue(true);

    await wrapper.setProps({ serviceId: "svc-b" });
    await flushPromises();

    // The editor is closed and the draft is gone — no PUT can carry A's selection to B.
    expect(wrapper.find('[data-testid="svc-dep-editor"]').exists()).toBe(false);
    expect(apiMock.PUT).not.toHaveBeenCalled();
  });
});
