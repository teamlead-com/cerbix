import { flushPromises, mount } from "@vue/test-utils";
import { nextTick, reactive } from "vue";
import { beforeEach, describe, expect, it, vi } from "vitest";

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
  transitionPending: false,
  transitionError: "",
  init: vi.fn(() => Promise.resolve()),
}));
const reactiveWorkspace = reactive(workspaceMock);

vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", async () => {
  const actual =
    await vi.importActual<typeof import("vue-router")>("vue-router");
  return {
    ...actual,
    useRoute: () => routeMock,
    useRouter: () => routerMock,
    RouterLink: {
      props: ["to"],
      template: '<a data-testid="router-link"><slot /></a>',
    },
  };
});
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => reactiveWorkspace }));
vi.mock("@/stores/session", () => ({
  useSession: () => ({
    user: { id: "u1" },
    isGlobalAdmin: true,
    isOrgAdmin: () => true,
    canProjectWrite: () => true,
  }),
}));
vi.mock("@/stores/ui", () => ({ useUi: () => ({ openCreate: vi.fn() }) }));
vi.mock("@/stores/live", () => ({
  useLive: () => ({ connect: vi.fn(), statuses: {} }),
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: {
    template:
      '<main><div data-testid="shell-actions"><slot name="actions" /></div><slot /></main>',
  },
}));
vi.mock("@/components/Kpi.vue", () => ({
  default: {
    props: ["label", "value", "unit", "sub"],
    template:
      '<div data-testid="kpi"><span data-testid="kpi-label">{{ label }}</span><span data-testid="kpi-value">{{ value }}</span><span data-testid="kpi-unit">{{ unit }}</span><span data-testid="kpi-sub">{{ sub }}</span></div>',
  },
}));
vi.mock("@/components/MonitorCard.vue", () => ({
  default: {
    props: ["monitor", "uptime", "budgetLeft", "budgetMet"],
    template:
      '<div data-testid="monitor-card"><span data-testid="monitor-uptime">{{ uptime }}</span><span data-testid="monitor-budget-left">{{ budgetLeft }}</span><span data-testid="monitor-budget-met">{{ budgetMet }}</span></div>',
  },
}));

function response(path: string) {
  if (path.includes("/projects/{projectID}/monitors")) {
    return {
      data: [
        {
          id: "m1",
          project_id: "p1",
          name: "Checkout API",
          type: "http",
          region: "core",
          status: "up",
          enabled: true,
          created_at: "2026-09-19T09:00:00Z",
        },
      ],
    };
  }
  if (path.includes("/projects/{projectID}/availability")) return { data: [] };
  if (path.includes("/projects/{projectID}/sla"))
    return { data: { windows: [] } };
  if (path.includes("/monitors/{monitorID}/heartbeats"))
    return {
      data: [
        {
          monitor_id: "m1",
          ts: "2026-09-19T09:01:00Z",
          up: true,
          latency_ms: 184,
        },
      ],
    };
  if (path.includes("/monitors/{monitorID}/sla"))
    return { data: { windows: [] } };
  if (path === "/api/v1/regions")
    return { data: { regions: [{ name: "core", live: true }] } };
  return { data: [] };
}

describe("Dashboard onboarding", () => {
  beforeEach(() => {
    localStorage.clear();
    apiMock.GET.mockReset();
    workspaceMock.orgs = [{ id: "o1", name: "Acme" }];
    workspaceMock.orgId = "o1";
    workspaceMock.projectId = "p1";
    workspaceMock.orgName = "Acme";
    workspaceMock.projectName = "Payments";
    workspaceMock.transitionPending = false;
    workspaceMock.transitionError = "";
    workspaceMock.init.mockReset().mockResolvedValue(undefined);
  });

  it("keeps an existing Dashboard unchanged when Get started opens the compact guide", async () => {
    apiMock.GET.mockImplementation((path: string) =>
      Promise.resolve(response(path)),
    );
    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await flushPromises();

    expect(wrapper.find('[data-testid="onboarding-guide"]').exists()).toBe(
      false,
    );
    expect(wrapper.findAll('[data-testid="kpi"]')).toHaveLength(4);
    expect(wrapper.findAll('[data-testid="monitor-card"]')).toHaveLength(1);

    await wrapper.find('[data-testid="onboarding-entry"]').trigger("click");
    await flushPromises();

    expect(wrapper.find('[data-testid="onboarding-guide"]').exists()).toBe(
      true,
    );
    expect(wrapper.findAll('[data-testid="kpi"]')).toHaveLength(4);
    expect(wrapper.findAll('[data-testid="monitor-card"]')).toHaveLength(1);
  });

  it("discards a delayed project A load after project B becomes current", async () => {
    let resolveProjectA!: (value: ReturnType<typeof response>) => void;
    const projectAMonitors = new Promise<ReturnType<typeof response>>(
      (resolve) => {
        resolveProjectA = resolve;
      },
    );
    apiMock.GET.mockImplementation(
      (
        path: string,
        options?: { params?: { path?: { projectID?: string } } },
      ) => {
        if (path.includes("/projects/{projectID}/monitors")) {
          return options?.params?.path?.projectID === "p1"
            ? projectAMonitors
            : Promise.resolve({ data: [] });
        }
        return Promise.resolve(response(path));
      },
    );

    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await Promise.resolve();

    workspaceMock.projectId = "p2";
    workspaceMock.projectName = "Ledger";
    await wrapper.find('[data-testid="onboarding-entry"]').trigger("click");
    await flushPromises();

    resolveProjectA(response("/api/v1/projects/{projectID}/monitors"));
    await flushPromises();

    expect(wrapper.findAll('[data-testid="monitor-card"]')).toHaveLength(0);
    expect(wrapper.text()).toContain("Ledger");
    expect(wrapper.text()).toContain("0 monitors");
    expect(wrapper.text()).not.toContain("Checkout API");
  });

  it("does not continue monitor or region reads after unmount", async () => {
    let resolveMonitors!: (value: ReturnType<typeof response>) => void;
    const delayedMonitors = new Promise<ReturnType<typeof response>>(
      (resolve) => {
        resolveMonitors = resolve;
      },
    );
    apiMock.GET.mockImplementation((path: string) => {
      if (path.includes("/projects/{projectID}/monitors"))
        return delayedMonitors;
      return Promise.resolve(response(path));
    });

    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await Promise.resolve();
    wrapper.unmount();
    resolveMonitors(response("/api/v1/projects/{projectID}/monitors"));
    await flushPromises();

    const postProjectReads = apiMock.GET.mock.calls.filter(
      ([path]) =>
        String(path).includes("/monitors/{monitorID}/") ||
        path === "/api/v1/regions",
    );
    expect(postProjectReads).toHaveLength(0);
  });

  it("shows an explicit loading state before the initial workspace read resolves", async () => {
    let resolveInit!: () => void;
    workspaceMock.init.mockImplementation(
      () => new Promise<void>((resolve) => (resolveInit = resolve)),
    );

    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await nextTick();

    expect(wrapper.get('[data-testid="dashboard-loading"]').text()).toContain(
      "Loading",
    );
    expect(wrapper.get('[data-testid="dashboard-loading"]').attributes("aria-label")).toMatch(
      /loading/i,
    );
    expect(wrapper.text()).not.toContain("0 / 0");
    expect(wrapper.findAll('[data-testid="kpi"]')).toHaveLength(0);
    expect(wrapper.find('[data-testid="availability-strip"]').exists()).toBe(
      false,
    );
    expect(wrapper.text()).not.toContain("No monitors in this project yet");

    resolveInit();
  });

  it("names null availability buckets as no data without inventing uptime status", async () => {
    apiMock.GET.mockImplementation((path: string) => {
      if (path.includes("/projects/{projectID}/availability"))
        return Promise.resolve({
          data: [
            {
              day: "2026-09-30T00:00:00Z",
              total: 0,
              uptime_percent: 0,
            },
          ],
        });
      if (path.includes("/projects/{projectID}/sla"))
        return Promise.resolve({
          data: {
            windows: [
              {
                window: "30d",
                total: 0,
                uptime_percent: 0,
                error_budget: { objective: 99.9, burned_percent: 0, met: true },
              },
            ],
          },
        });
      if (path.includes("/monitors/{monitorID}/sla"))
        return Promise.resolve({
          data: {
            windows: [
              {
                window: "30d",
                total: 0,
                uptime_percent: 0,
                error_budget: { objective: 99.9, burned_percent: 0, met: true },
              },
            ],
          },
        });
      if (path.includes("/monitors/{monitorID}/heartbeats"))
        return Promise.resolve({ data: [] });
      return Promise.resolve(response(path));
    });

    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await flushPromises();

    const strip = wrapper.get('[data-testid="availability-strip"]');
    expect(
      wrapper.get('[data-testid="availability-no-data-legend"]').text(),
    ).toMatch(/No data|UNKNOWN/);
    expect(strip.attributes("aria-label")).toMatch(/No data|UNKNOWN/);
    expect(wrapper.get('[data-testid="timeline-uptime"]').text()).toBe("—");
    expect(strip.findAll('[data-state="no-data"]')).toHaveLength(90);
    for (const bucket of strip.findAll('[data-state="no-data"]')) {
      expect(bucket.attributes("class") ?? "").not.toMatch(
        /(^|\s)(up|down|degraded)(\s|$)/,
      );
      expect(bucket.attributes("style")).toContain("var(--inset)");
    }
    expect(wrapper.get('[data-testid="kpi"]').exists()).toBe(true);
    expect(wrapper.text()).toContain("1 monitors");
    const availabilityKpi = wrapper
      .findAll('[data-testid="kpi"]')
      .find((kpi) =>
        kpi.get('[data-testid="kpi-label"]').text().startsWith("Availability"),
      );
    const budgetKpi = wrapper
      .findAll('[data-testid="kpi"]')
      .find((kpi) =>
        kpi.get('[data-testid="kpi-label"]').text().startsWith("Error budget"),
      );
    expect(availabilityKpi?.get('[data-testid="kpi-value"]').text()).toBe("—");
    expect(budgetKpi?.get('[data-testid="kpi-value"]').text()).toBe("—");
    expect(wrapper.get('[data-testid="monitor-uptime"]').text()).toBe("—");
    expect(wrapper.get('[data-testid="monitor-budget-left"]').text()).toBe("");
    expect(wrapper.text()).not.toContain("0.00%");
    expect(wrapper.text()).not.toContain("100%");
  });

  it.each([
    {
      name: "monitor",
      path: "/projects/{projectID}/monitors",
      message: "Monitor list is unavailable.",
    },
    {
      name: "project SLA",
      path: "/projects/{projectID}/sla",
      message: "Project SLA is unavailable.",
    },
    {
      name: "availability",
      path: "/projects/{projectID}/availability",
      message: "Availability service is unavailable.",
    },
  ])(
    "renders an actionable error and recovers on Retry after a required $name read fails",
    async ({ path, message }) => {
      let failed = true;
      apiMock.GET.mockImplementation((requestPath: string) => {
        if (failed && requestPath.includes(path))
          return Promise.resolve({ error: { error: message } });
        return Promise.resolve(response(requestPath));
      });

      const wrapper = mount(DashboardView, {
        global: {
          stubs: {
            RouterLink: { props: ["to"], template: "<a><slot /></a>" },
          },
        },
      });
      await flushPromises();

      expect(wrapper.get('[data-testid="dashboard-error"]').text()).toContain(
        message,
      );
      expect(wrapper.get('[data-testid="dashboard-retry"]').text()).toMatch(
        /Retry/i,
      );
      expect(wrapper.text()).not.toContain("No monitors in this project yet");

      failed = false;
      await wrapper.get('[data-testid="dashboard-retry"]').trigger("click");
      await flushPromises();

      expect(wrapper.find('[data-testid="dashboard-error"]').exists()).toBe(
        false,
      );
      expect(wrapper.findAll('[data-testid="kpi"]')).toHaveLength(4);
      expect(wrapper.findAll('[data-testid="monitor-card"]')).toHaveLength(1);
      expect(workspaceMock.init).toHaveBeenCalledTimes(2);
    },
  );

  it("forces workspace initialization on Retry after a transition error without watcher loops", async () => {
    let initCalls = 0;
    workspaceMock.init.mockImplementation(async (force?: boolean) => {
      initCalls++;
      if (initCalls === 1) {
        reactiveWorkspace.transitionError =
          "Could not load projects for the selected organization.";
        throw new Error("first project read failed");
      }
      expect(force).toBe(true);
      reactiveWorkspace.transitionError = "";
    });
    apiMock.GET.mockImplementation((path: string) =>
      Promise.resolve(response(path)),
    );

    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await flushPromises();

    expect(wrapper.get('[data-testid="dashboard-error"]').text()).toContain(
      "first project read failed",
    );
    await wrapper.get('[data-testid="dashboard-retry"]').trigger("click");
    await flushPromises();
    await nextTick();

    expect(initCalls).toBe(2);
    expect(wrapper.find('[data-testid="dashboard-error"]').exists()).toBe(
      false,
    );
    expect(wrapper.findAll('[data-testid="kpi"]')).toHaveLength(4);
    expect(wrapper.findAll('[data-testid="monitor-card"]')).toHaveLength(1);
  });

  it("does not render an empty project state while an organization transition is pending", async () => {
    workspaceMock.projectId = "";
    workspaceMock.projectName = "";
    workspaceMock.transitionPending = true;
    workspaceMock.init.mockResolvedValue(undefined);
    apiMock.GET.mockImplementation((path: string) => Promise.resolve(response(path)));

    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await flushPromises();

    expect(wrapper.text()).not.toContain("Projects are the teams and apps inside");
    expect(wrapper.text()).not.toContain("No projects in");
  });

  it("keeps a cold project-read failure stable instead of retrying from its watcher", async () => {
    workspaceMock.orgs = [{ id: "org-b", name: "Org B" }];
    workspaceMock.orgId = "org-b";
    workspaceMock.projectId = "";
    workspaceMock.projectName = "";
    workspaceMock.transitionPending = false;
    workspaceMock.transitionError = "";
    let initCalls = 0;
    workspaceMock.init.mockImplementation(async () => {
      initCalls++;
      if (initCalls === 1) {
        reactiveWorkspace.transitionError = "Could not load projects for the selected organization.";
        throw new Error("first project read failed");
      }
    });

    const wrapper = mount(DashboardView, {
      global: {
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });
    await flushPromises();
    await Promise.resolve();

    expect(initCalls).toBe(1);
    expect(wrapper.text()).toContain("first project read failed");
  });
});
