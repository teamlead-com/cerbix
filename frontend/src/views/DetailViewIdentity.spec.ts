import { flushPromises, mount } from "@vue/test-utils";
import { reactive } from "vue";
import { describe, expect, it, vi } from "vitest";

import IncidentDetailView from "@/views/IncidentDetailView.vue";
import MonitorDetailView from "@/views/MonitorDetailView.vue";

// Navigating between two things of the SAME kind.
//
// Vue Router reuses a component when only the params change, and both detail views read
// `route.params.id` once and loaded on mount. So a search hit for another monitor — or another
// incident — changed the URL and left the previous subject on screen, with every mutation on the
// page still pointing at it. Pause, retire and delete on the monitor page; acknowledge, resolve and
// postmortem on the incident page.
//
// Three layers fix it and each is tested here: the id is REACTIVE, a watch reloads on the route
// identity AND the workspace, and a load ticket stops a slower response for the previous subject
// from overwriting the current one. (App.vue also keys the RouterView by path, which is the layer
// that does not depend on a view remembering any of this.)

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

// A REACTIVE route, which is the whole point: a static mock cannot express the navigation that broke.
const route = vi.hoisted(() => ({ current: null as unknown as { params: { id: string } } }));
vi.mock("vue-router", () => ({
  useRoute: () => route.current,
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true, canOrgWrite: () => true }),
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", projects: [], orgs: [] }),
}));
vi.mock("@/stores/live", () => ({
  useLive: () => ({ connect: () => {}, statuses: {} as Record<string, string> }),
}));

describe("a detail view follows the id in the route", () => {
  it("loads the monitor the URL names after an A → B navigation", async () => {
    route.current = reactive({ params: { id: "mon-a" } });
    const asked: string[] = [];
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation(async (path: string, opts?: { params?: { path?: Record<string, string> } }) => {
      const mid = opts?.params?.path?.monitorID;
      if (path === "/api/v1/monitors/{monitorID}") {
        asked.push(mid ?? "");
        return { data: { id: mid, project_id: "p1", name: `monitor ${mid}`, type: "http", target: "https://x", status: "up", enabled: true, interval_seconds: 30, management: { source: "ui" } } };
      }
      return { data: undefined };
    });

    const w = mount(MonitorDetailView, {
      global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
    });
    await flushPromises();
    expect(w.text()).toContain("monitor mon-a");

    route.current.params.id = "mon-b";
    await flushPromises();

    expect(asked).toContain("mon-b");
    expect(w.text()).toContain("monitor mon-b");
    expect(w.text()).not.toContain("monitor mon-a");
  });

  it("does not let a slow response for the previous monitor overwrite the current one", async () => {
    route.current = reactive({ params: { id: "mon-a" } });
    let releaseA: (() => void) | undefined;
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation(async (path: string, opts?: { params?: { path?: Record<string, string> } }) => {
      const mid = opts?.params?.path?.monitorID;
      if (path === "/api/v1/monitors/{monitorID}") {
        const body = { data: { id: mid, project_id: "p1", name: `monitor ${mid}`, type: "http", target: "https://x", status: "up", enabled: true, interval_seconds: 30, management: { source: "ui" } } };
        if (mid === "mon-a") {
          // A's answer is still in flight when the user moves on.
          await new Promise<void>((res) => (releaseA = res));
        }
        return body;
      }
      return { data: undefined };
    });

    const w = mount(MonitorDetailView, {
      global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
    });
    route.current.params.id = "mon-b";
    await flushPromises();
    releaseA?.();
    await flushPromises();

    expect(w.text()).toContain("monitor mon-b");
    expect(w.text()).not.toContain("monitor mon-a");
  });

  it("loads the incident the URL names after an A → B navigation", async () => {
    route.current = reactive({ params: { id: "inc-a" } });
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation(async (path: string, opts?: { params?: { path?: Record<string, string> } }) => {
      const iid = opts?.params?.path?.incidentID;
      if (path === "/api/v1/incidents/{incidentID}") {
        return { data: { id: iid, project_id: "p1", title: `incident ${iid}`, status: "investigating", impact: "major", source: "manual", started_at: "2026-08-26T00:00:00Z" } };
      }
      if (path === "/api/v1/incidents/{incidentID}/updates") return { data: [] };
      if (path === "/api/v1/incidents/{incidentID}/postmortem") return { error: {} };
      return { data: undefined };
    });

    const w = mount(IncidentDetailView, {
      global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
    });
    await flushPromises();
    expect(w.text()).toContain("incident inc-a");

    route.current.params.id = "inc-b";
    await flushPromises();

    expect(w.text()).toContain("incident inc-b");
    expect(w.text()).not.toContain("incident inc-a");
  });
});
