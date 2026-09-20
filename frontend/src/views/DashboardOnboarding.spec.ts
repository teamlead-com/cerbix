import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import DashboardView from "@/views/DashboardView.vue";

const apiMock = vi.hoisted(() => ({ GET: vi.fn() }));
const routerMock = vi.hoisted(() => ({ replace: vi.fn() }));
const routeMock = vi.hoisted(() => ({ query: {} as Record<string, string> }));
const workspaceMock = vi.hoisted(() => ({
  orgs: [{ id: "o1", name: "Acme" }],
  projects: [{ id: "p1", name: "Payments" }],
  orgId: "o1",
  projectId: "p1",
  orgName: "Acme",
  projectName: "Payments",
  init: vi.fn(() => Promise.resolve()),
}));

vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", async () => {
  const actual = await vi.importActual<typeof import("vue-router")>("vue-router");
  return {
    ...actual,
    useRoute: () => routeMock,
    useRouter: () => routerMock,
    RouterLink: { props: ["to"], template: '<a data-testid="router-link"><slot /></a>' },
  };
});
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => workspaceMock }));
vi.mock("@/stores/session", () => ({
  useSession: () => ({
    user: { id: "u1" },
    isGlobalAdmin: true,
    isOrgAdmin: () => true,
    canProjectWrite: () => true,
  }),
}));
vi.mock("@/stores/ui", () => ({ useUi: () => ({ openCreate: vi.fn() }) }));
vi.mock("@/stores/live", () => ({ useLive: () => ({ connect: vi.fn(), statuses: {} }) }));
vi.mock("@/components/AppShell.vue", () => ({
  default: { template: '<main><div data-testid="shell-actions"><slot name="actions" /></div><slot /></main>' },
}));
vi.mock("@/components/Kpi.vue", () => ({ default: { template: '<div data-testid="kpi" />' } }));
vi.mock("@/components/MonitorCard.vue", () => ({ default: { template: '<div data-testid="monitor-card" />' } }));

function response(path: string) {
  if (path.includes("/projects/{projectID}/monitors")) {
    return { data: [{ id: "m1", project_id: "p1", name: "Checkout API", type: "http", region: "core", status: "up", enabled: true, created_at: "2026-09-19T09:00:00Z" }] };
  }
  if (path.includes("/projects/{projectID}/availability")) return { data: [] };
  if (path.includes("/projects/{projectID}/sla")) return { data: { windows: [] } };
  if (path.includes("/monitors/{monitorID}/heartbeats")) return { data: [{ monitor_id: "m1", ts: "2026-09-19T09:01:00Z", up: true, latency_ms: 184 }] };
  if (path.includes("/monitors/{monitorID}/sla")) return { data: { windows: [] } };
  if (path === "/api/v1/regions") return { data: { regions: [{ name: "core", live: true }] } };
  return { data: [] };
}

describe("Dashboard onboarding", () => {
  it("keeps an existing Dashboard unchanged when Get started opens the compact guide", async () => {
    localStorage.clear();
    apiMock.GET.mockImplementation((path: string) => Promise.resolve(response(path)));
    const wrapper = mount(DashboardView, { global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } } });
    await flushPromises();

    expect(wrapper.find('[data-testid="onboarding-guide"]').exists()).toBe(false);
    expect(wrapper.findAll('[data-testid="kpi"]')).toHaveLength(4);
    expect(wrapper.findAll('[data-testid="monitor-card"]')).toHaveLength(1);

    await wrapper.find('[data-testid="onboarding-entry"]').trigger("click");
    await flushPromises();

    expect(wrapper.find('[data-testid="onboarding-guide"]').exists()).toBe(true);
    expect(wrapper.findAll('[data-testid="kpi"]')).toHaveLength(4);
    expect(wrapper.findAll('[data-testid="monitor-card"]')).toHaveLength(1);
  });
});
