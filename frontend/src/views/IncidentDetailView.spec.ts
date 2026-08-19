import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import IncidentDetailView from "@/views/IncidentDetailView.vue";

// FR-021 phase 3 ([301] P1-1): the impact enrichment must SURVIVE an acknowledge. The
// acknowledge endpoint answers with the BASE incident — no impacts, no
// impacts_unavailable — so assigning its response wholesale would silently erase the
// chips the detail was loaded with. This is the regression the [298] fix pass claimed and
// did not have.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: { id: "inc1" } }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
// AppShell drags in the theme/live-SSE machinery (matchMedia) that has no business in this
// test; the module is replaced the way SlaView.spec.ts does it.
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true }),
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1" }),
}));

const RouterLink = { props: ["to"], template: "<a><slot /></a>" };

const BASE_INCIDENT = {
  id: "inc1",
  project_id: "p1",
  title: "checkout-http is down",
  status: "investigating",
  impact: "major",
  source: "auto",
  started_at: "2026-08-17T10:14:07Z",
  monitor_id: "mon1",
};

const LINK = {
  service_id: "svc-pay",
  slug: "payments",
  name: "Payments",
  role: "probable_root",
  path: ["payments", "checkout"],
  computed_at: "2026-08-17T10:14:12Z",
};

function mountWith(detail: Record<string, unknown>) {
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/updates")) return Promise.resolve({ data: [] });
    if (path.endsWith("/postmortem")) return Promise.resolve({ error: { error: "not found" } });
    return Promise.resolve({ data: detail });
  });
  // The acknowledge response is the BASE incident: this is the wire contract, and the
  // whole point of the regression.
  apiMock.POST.mockImplementation(() =>
    Promise.resolve({ data: { ...BASE_INCIDENT, acknowledged_at: "2026-08-17T10:20:00Z", acknowledged_by: "u1" } }),
  );
  return mount(IncidentDetailView, { global: { stubs: { RouterLink } } });
}

// FR-022, mock panel 1: the header says what the incident is an incident OF. The name comes from a
// SECOND request, so the interesting cases are the ones where that request has not answered or has
// nothing to answer about.
describe("IncidentDetailView subject chip", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
  });

  function mountAnchored(anchor: Record<string, unknown>, subject: Record<string, unknown> | null) {
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/updates")) return Promise.resolve({ data: [] });
      if (path.endsWith("/postmortem")) return Promise.resolve({ error: { error: "not found" } });
      if (path.includes("/services/")) return Promise.resolve({ data: subject ? { service: subject } : undefined });
      if (path.includes("/monitors/")) return Promise.resolve({ data: subject ?? undefined });
      return Promise.resolve({ data: { ...BASE_INCIDENT, monitor_id: undefined, ...anchor, impacts: [] } });
    });
    return mount(IncidentDetailView, { global: { stubs: { RouterLink } } });
  }

  it("names the service a service incident belongs to", async () => {
    const w = mountAnchored({ service_id: "svc1" }, { slug: "checkout", name: "Checkout" });
    await flushPromises();
    const chip = w.get('[data-testid="incident-subject"]');
    expect(chip.text()).toContain("service");
    expect(chip.text()).toContain("checkout");
  });

  it("names the monitor for a monitor incident — the anchor changed, the grammar did not", async () => {
    const w = mountAnchored({ monitor_id: "mon1" }, { name: "checkout-http" });
    await flushPromises();
    expect(w.get('[data-testid="incident-subject"]').text()).toContain("checkout-http");
  });

  it("still renders the incident when its subject is GONE, stating the kind alone", async () => {
    const w = mountAnchored({ service_id: "svc-deleted" }, null);
    await flushPromises();
    // A timeline is a record of something that happened; deleting the subject does not unhappen it.
    expect(w.text()).toContain(BASE_INCIDENT.title);
    expect(w.get('[data-testid="incident-subject"]').text()).toContain("service");
  });

  it("shows NO chip on a project-level incident", async () => {
    const w = mountAnchored({}, null);
    await flushPromises();
    expect(w.find('[data-testid="incident-subject"]').exists()).toBe(false);
  });
});

describe("IncidentDetailView impact enrichment", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
  });

  it("keeps populated impact chips across an acknowledge", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: [LINK] });
    await flushPromises();

    expect(wrapper.findAll('[data-testid="impact-root"]')).toHaveLength(1);
    expect(wrapper.get('[data-testid="impact-root"]').text()).toContain("payments");

    const ack = wrapper.findAll("button").find((b) => b.text() === "Acknowledge");
    expect(ack, "the acknowledge button must be present for an unacknowledged incident").toBeTruthy();
    await ack!.trigger("click");
    await flushPromises();

    // The acknowledge landed…
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    expect(wrapper.text()).toContain("acknowledged");
    // …and the enrichment the base response knows nothing about survived it.
    expect(wrapper.findAll('[data-testid="impact-root"]')).toHaveLength(1);
    expect(wrapper.get('[data-testid="impact-root"]').text()).toContain("payments");
  });

  it("keeps the impacts-unavailable state across an acknowledge", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: null, impacts_unavailable: true });
    await flushPromises();
    expect(wrapper.find('[data-testid="impact-unavailable"]').exists()).toBe(true);

    const ack = wrapper.findAll("button").find((b) => b.text() === "Acknowledge");
    await ack!.trigger("click");
    await flushPromises();

    // A degraded read stays degraded — it must not silently become "no impact".
    expect(wrapper.find('[data-testid="impact-unavailable"]').exists()).toBe(true);
    expect(wrapper.findAll('[data-testid="impact-root"]')).toHaveLength(0);
  });

  it("renders no impact block at all when the incident honestly has no links", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: [] });
    await flushPromises();
    expect(wrapper.find('[data-testid="incident-impacts"]').exists()).toBe(false);
  });

  it("ranks probable roots nearest-first, exactly as the 🕸 note orders them", async () => {
    const far = { ...LINK, service_id: "svc-db", slug: "billing-db", path: ["billing-db", "payments", "checkout"] };
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: [far, LINK] });
    await flushPromises();
    const chips = wrapper.findAll('[data-testid="impact-root"]');
    expect(chips).toHaveLength(2);
    expect(chips[0].text()).toContain("payments");
    expect(chips[1].text()).toContain("billing-db");
    // The stored path renders verbatim, root first.
    expect(chips[1].text()).toContain("billing-db → payments → checkout");
  });
});
