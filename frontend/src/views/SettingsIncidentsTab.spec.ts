import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import SettingsView from "@/views/SettingsView.vue";

// FR-042 / NFR-035 (D19; mock §2, D-8): Settings → Project → Incidents. The tab is in the Project
// group right after Release gate and is shown to EVERYONE who sees the project's settings; only
// `project:manage` on this project (the REAL session store's canProjectAdmin decides) can edit it.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
const routeState = vi.hoisted(() => ({ query: {} as Record<string, string> }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => routeState,
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/workspace", async () => {
  const { reactive } = await vi.importActual<typeof import("vue")>("vue");
  const ws = reactive({
    orgId: "o1",
    projectId: "p1",
    orgName: "Acme",
    projectName: "API",
    projects: [{ id: "p1", name: "API", slug: "api" }],
    init: async () => {},
  });
  return { useWorkspace: () => ws };
});
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));

const stubs = { RouterLink: { template: "<a><slot /></a>" } };

function as(memberships: object[]) {
  const s = useSession();
  s.user = { id: "u", email: "u@example.com", is_global_admin: false } as never;
  s.memberships = memberships as never;
}
function projectTabs(w: ReturnType<typeof mount>): string[] {
  const group = w.findAll("nav > div").find((g) => g.text().startsWith("Project"));
  return group!.findAll("button").map((b) => b.text());
}

// The workspace mock is one shared reactive object, so a wrapper left mounted by an earlier test
// would react to this test's projectId change and add loads of its own.
const mounted: ReturnType<typeof mount>[] = [];
function mountView() {
  const w = mount(SettingsView, { global: { stubs } });
  mounted.push(w);
  return w;
}
afterEach(() => {
  while (mounted.length) mounted.pop()!.unmount();
});

beforeEach(() => {
  setActivePinia(createPinia());
  useWorkspace().projectId = "p1";
  routeState.query = {};
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation(async (path: string) =>
    path === "/api/v1/projects/{projectID}"
      ? { data: { id: "p1", default_incident_impact: "major" } }
      : { data: [] },
  );
});

describe("Settings → Project → Incidents", () => {
  it("sits right after Release gate, for a project admin", async () => {
    as([{ org_id: "o1", project_id: "p1", role: "project_admin" }]);
    const w = mountView();
    await flushPromises();
    const tabs = projectTabs(w);
    expect(tabs[tabs.indexOf("Release gate") + 1]).toBe("Incidents");
  });

  it("is shown to a project viewer as well", async () => {
    as([{ org_id: "o1", project_id: "p1", role: "viewer" }]);
    const w = mountView();
    await flushPromises();
    const tabs = projectTabs(w);
    expect(tabs[tabs.indexOf("Release gate") + 1]).toBe("Incidents");
  });

  it("opens from the deep link ?tab=incidents, editable with project:manage", async () => {
    routeState.query = { tab: "incidents" };
    as([{ org_id: "o1", project_id: "p1", role: "project_admin" }]);
    const w = mountView();
    await flushPromises();
    expect(w.find('[data-testid="project-incident-settings"]').exists()).toBe(true);
    expect(w.find('[data-testid="project-incident-impact-select"]').exists()).toBe(true);
  });

  it("opens from the deep link read-only without project:manage", async () => {
    routeState.query = { tab: "incidents" };
    as([{ org_id: "o1", project_id: "p1", role: "editor" }]);
    const w = mountView();
    await flushPromises();
    expect(w.find('[data-testid="project-incident-impact-select"]').exists()).toBe(false);
    expect(w.get('[data-testid="project-incident-impact-badge"]').text()).toBe("Major");
    expect(w.get('[data-testid="project-incident-read-only"]').text()).toBe("Only a project admin can change this.");
  });

  it("makes no request and shows no error while the project id is empty, then loads p1 once when it appears", async () => {
    routeState.query = { tab: "incidents" };
    as([{ org_id: "o1", project_id: "p1", role: "project_admin" }]);
    const ws = useWorkspace();
    ws.projectId = "";
    const w = mountView();
    await flushPromises();
    const projectCalls = () =>
      apiMock.GET.mock.calls.filter((c) => c[0] === "/api/v1/projects/{projectID}");
    expect(projectCalls()).toHaveLength(0);
    expect(w.find('[data-testid="project-incident-unavailable"]').exists()).toBe(false);
    expect(w.text()).not.toContain("Could not load");

    ws.projectId = "p1";
    await flushPromises();
    expect(projectCalls()).toHaveLength(1);
    expect(projectCalls()[0][1]).toEqual({ params: { path: { projectID: "p1" } } });
    expect(w.find('[data-testid="project-incident-unavailable"]').exists()).toBe(false);
    expect((w.get('[data-testid="project-incident-impact-select"]').element as HTMLSelectElement).value).toBe("major");
  });

  it("an organization with no projects shows no read error on the card", async () => {
    routeState.query = { tab: "incidents" };
    as([{ org_id: "o1", project_id: null, role: "org_admin" }]);
    useWorkspace().projectId = "";
    const w = mountView();
    await flushPromises();
    expect(apiMock.GET.mock.calls.some((c) => c[0] === "/api/v1/projects/{projectID}")).toBe(false);
    expect(w.find('[data-testid="project-incident-unavailable"]').exists()).toBe(false);
    expect(w.text()).not.toContain("Could not load the project's incident settings");
  });
});
