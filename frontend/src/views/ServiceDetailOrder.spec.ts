import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import ServiceDetailView from "@/views/ServiceDetailView.vue";

// FR-025, D-0210 item 1 (approved mock, service detail): the `Changes` card sits BETWEEN the release
// gate and the dependency graph — facts, then who may deploy, then what a pipeline said it changed,
// then the blast radius. Until now that order lived only in the parent's template, where any edit
// (or a merge that reorders the block) moves it silently: every other spec stubs the three cards and
// passes whatever the sequence is. This file asserts the placement itself, by DOCUMENT POSITION of
// the rendered markers — not by the order of a props array or of the `stubs` object, neither of which
// the template has to honour.

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
  useWorkspace: () => ({
    init: () => Promise.resolve(),
    orgId: "o1",
    projectId: "p1",
    orgName: "Acme",
    projectName: "API",
  }),
}));

const RouterLink = { props: ["to"], template: '<a :data-to="to.params?.id"><slot /></a>' };
const CHILD = { template: "<div />" };
// Distinguishable markers, one per card, so the three are told apart in the rendered DOM.
const marker = (id: string) => ({ template: `<div data-testid="${id}" />` });

// The whole ServiceDetail shape — a half-fixture renders an empty body and every assertion below
// would then fail (or pass) for the wrong reason.
const SERVICE = {
  service: { id: "svc1", slug: "checkout", name: "Checkout", created_at: "", updated_at: "" },
  declaration: null,
  epoch: null,
  materialization: { repairing: [] },
  reliability: null,
  supersedes: [],
  sla_targets: [],
};

describe("ServiceDetailView card order (D-0210 item 1)", () => {
  it("the Changes card sits between Release gate and Dependencies", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation((path: string) => {
      const p = String(path ?? "");
      if (p.endsWith("/incidents")) return Promise.resolve({ data: [] });
      if (p.endsWith("/monitors")) return Promise.resolve({ data: [] });
      return Promise.resolve({ data: SERVICE });
    });
    const w = mount(ServiceDetailView, {
      global: {
        stubs: {
          RouterLink,
          ServiceReliability: CHILD,
          ServiceAlerting: CHILD,
          ServiceGate: marker("order-gate"),
          ServiceChanges: marker("order-changes"),
          ServiceDependencies: marker("order-deps"),
        },
      },
    });
    await flushPromises();
    await flushPromises();

    // Vacuity guard: the page itself rendered, and all three cards are on it.
    expect(w.text(), "the fixture must render the service page, or the order below is an empty claim").toContain("checkout");
    const gate = w.get('[data-testid="order-gate"]').element;
    const changes = w.get('[data-testid="order-changes"]').element;
    const deps = w.get('[data-testid="order-deps"]').element;

    // DOCUMENT_POSITION_FOLLOWING (4): the other node comes after this one in document order.
    const FOLLOWING = Node.DOCUMENT_POSITION_FOLLOWING;
    expect(
      gate.compareDocumentPosition(changes) & FOLLOWING,
      "the Changes card must be rendered AFTER the Release gate",
    ).toBe(FOLLOWING);
    expect(
      changes.compareDocumentPosition(deps) & FOLLOWING,
      "the Changes card must be rendered BEFORE Dependencies",
    ).toBe(FOLLOWING);

    // The same claim read off the serialized markup, so a failure prints the order that was rendered.
    const html = w.html();
    const at = (id: string) => html.indexOf(`data-testid="${id}"`);
    expect([at("order-gate"), at("order-changes"), at("order-deps")]).toEqual(
      [at("order-gate"), at("order-changes"), at("order-deps")].slice().sort((a, b) => a - b),
    );
  });
});
