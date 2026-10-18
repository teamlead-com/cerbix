import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import ProjectMembersPanel from "@/components/settings/ProjectMembersPanel.vue";
import { useSession } from "@/stores/session";
import SettingsView from "@/views/SettingsView.vue";

// iter-0206 (FR-040): who sees Settings → Project → Members. The REAL session store decides
// (canProjectAdmin), so this pins the gate the operator actually meets, not a stub's answer.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
const routeState = vi.hoisted(() => ({ query: {} as Record<string, string> }));
// The workspace starts EMPTY and gets its project only when init() runs — the way a deep link meets
// it — so a tab that depends on the current project is not in the list at mount.
const wsState = vi.hoisted(() => ({
  ws: null as null | { projectId: string; orgId: string },
  loadProject: true,
  initCalls: [] as boolean[],
}));
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
  return {
    useWorkspace: () => {
      if (!wsState.ws) {
        const ws = reactive({
          orgId: "o1",
          projectId: wsState.loadProject ? "p1" : "",
          orgName: "Acme",
          projectName: "API",
          projects: [{ id: "p1", name: "API", slug: "api" }],
          init: async (force = false) => {
            wsState.initCalls.push(force);
            ws.projectId = "p1";
          },
        });
        wsState.ws = ws;
      }
      return wsState.ws;
    },
  };
});
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));

beforeEach(() => {
  setActivePinia(createPinia());
  routeState.query = {};
  wsState.ws = null;
  wsState.loadProject = true;
  wsState.initCalls = [];
  apiMock.GET.mockReset();
  apiMock.GET.mockResolvedValue({ data: [] });
});

function projectTabs(w: ReturnType<typeof mount>): string[] {
  const group = w.findAll("nav > div").find((g) => g.text().startsWith("Project"));
  expect(group, "the Project group renders").toBeTruthy();
  return group!.findAll("button").map((b) => b.text());
}

const cases: { name: string; globalAdmin: boolean; memberships: object[]; sees: boolean }[] = [
  { name: "org admin", globalAdmin: false, memberships: [{ org_id: "o1", role: "org_admin" }], sees: true },
  { name: "global admin", globalAdmin: true, memberships: [], sees: true },
  { name: "project admin of this project", globalAdmin: false, memberships: [{ org_id: "o1", project_id: "p1", role: "project_admin" }], sees: true },
  { name: "project admin of another project", globalAdmin: false, memberships: [{ org_id: "o1", project_id: "p2", role: "project_admin" }, { org_id: "o1", project_id: "p1", role: "viewer" }], sees: false },
  { name: "org editor", globalAdmin: false, memberships: [{ org_id: "o1", role: "editor" }], sees: false },
  { name: "project viewer", globalAdmin: false, memberships: [{ org_id: "o1", project_id: "p1", role: "viewer" }], sees: false },
];

describe("Settings → Project → Members visibility", () => {
  for (const c of cases) {
    it(`${c.sees ? "shows" : "hides"} the tab for a ${c.name}`, async () => {
      const s = useSession();
      s.user = { id: "u", email: "u@example.com", is_global_admin: c.globalAdmin } as never;
      s.memberships = c.memberships as never;
      const w = mount(SettingsView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
      await flushPromises();
      expect(projectTabs(w).includes("Members")).toBe(c.sees);
    });
  }
});

describe("deep link /settings?tab=projectmembers", () => {
  it("opens the tab once the workspace has its project, not the first tab", async () => {
    routeState.query = { tab: "projectmembers" };
    wsState.loadProject = false; // the project arrives with init(), after mount
    const s = useSession();
    s.user = { id: "u", email: "u@example.com", is_global_admin: false } as never;
    s.memberships = [{ org_id: "o1", project_id: "p1", role: "project_admin" }] as never;
    apiMock.GET.mockImplementation(async (path: string) =>
      path === "/api/v1/projects/{projectID}/members" ? { data: { members: [], inherited: [] } } : { data: [] },
    );
    const w = mount(SettingsView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
    await flushPromises();
    expect(w.find('[data-testid="project-members"]').exists(), "the deep-linked panel rendered").toBe(true);
  });
});

describe("after the user changes their own project membership", () => {
  async function mountAsProjectAdmin(meAfter: object[]) {
    routeState.query = { tab: "projectmembers" };
    const s = useSession();
    s.user = { id: "u", email: "u@example.com", is_global_admin: false } as never;
    s.memberships = [{ org_id: "o1", project_id: "p1", role: "project_admin" }] as never;
    apiMock.GET.mockImplementation(async (path: string) => {
      if (path === "/api/v1/me") return { data: { user: s.user, memberships: meAfter }, response: { status: 200 } };
      if (path === "/api/v1/projects/{projectID}/members") return { data: { members: [], inherited: [] } };
      return { data: [] };
    });
    const w = mount(SettingsView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
    await flushPromises();
    wsState.initCalls = [];
    w.findComponent(ProjectMembersPanel).vm.$emit("self-changed");
    await flushPromises();
    return w;
  }

  it("re-reads the workspace and says access is gone when nothing reaches the project", async () => {
    const w = await mountAsProjectAdmin([]);
    expect(wsState.initCalls, "the workspace is re-read with force").toEqual([true]);
    expect(w.get('[data-testid="self-notice"]').text()).toContain("You no longer have access to API.");
    expect(projectTabs(w).includes("Members")).toBe(false);
  });

  it("keeps the workspace and says other access is unchanged when the project is still visible", async () => {
    const w = await mountAsProjectAdmin([{ org_id: "o1", project_id: "p1", role: "viewer" }]);
    expect(wsState.initCalls).toEqual([]);
    expect(w.get('[data-testid="self-notice"]').text()).toContain("Your other access is unchanged.");
    expect(projectTabs(w).includes("Members")).toBe(false);
  });
});
