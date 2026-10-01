import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useWorkspace } from "@/stores/workspace";
import IncidentDetailView from "@/views/IncidentDetailView.vue";

const apiMock = vi.hoisted(() => ({
  GET: vi.fn(),
  POST: vi.fn(),
  PUT: vi.fn(),
}));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: { id: "inc1" } }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true }),
}));

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const INCIDENT = {
  id: "inc1",
  project_id: "p1",
  title: "Checkout incident",
  status: "investigating",
  impact: "major",
  source: "manual",
  started_at: "2026-10-01T00:00:00Z",
  impacts: [],
};

describe("IncidentDetailView with the production workspace store", () => {
  beforeEach(() => {
    localStorage.clear();
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
    apiMock.PUT.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("opens one cold workspace generation and loads the incident once after that context is published", async () => {
    const organizations = deferred<{ data: Array<{ id: string; name: string }> }>();
    const projects = deferred<{ data: Array<{ id: string; name: string }> }>();
    const firstOrganizationRead = deferred<void>();
    let organizationReads = 0;
    let incidentReads = 0;

    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/organizations") {
        organizationReads += 1;
        firstOrganizationRead.resolve();
        return organizations.promise;
      }
      if (path === "/api/v1/organizations/{orgID}/projects") return projects.promise;
      if (path === "/api/v1/incidents/{incidentID}") {
        incidentReads += 1;
        return Promise.resolve({ data: INCIDENT });
      }
      if (path === "/api/v1/incidents/{incidentID}/updates") return Promise.resolve({ data: [] });
      if (path === "/api/v1/incidents/{incidentID}/postmortem") {
        return Promise.resolve({ error: { error: "not found" } });
      }
      return Promise.resolve({ data: undefined });
    });

    const pinia = createPinia();
    setActivePinia(pinia);
    const wrapper = mount(IncidentDetailView, {
      attachTo: document.body,
      global: {
        plugins: [pinia],
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });

    try {
      await firstOrganizationRead.promise;
      await Promise.resolve();
      await wrapper.vm.$nextTick();

      expect(organizationReads).toBe(1);

      organizations.resolve({ data: [{ id: "o1", name: "Org A" }] });
      await Promise.resolve();
      projects.resolve({ data: [{ id: "p1", name: "Project A" }] });
      await flushPromises();

      expect(organizationReads).toBe(1);
      expect(incidentReads).toBe(1);
      expect(wrapper.text()).toContain(INCIDENT.title);
    } finally {
      wrapper.unmount();
    }
  });

  it("settles a cold workspace failure without opening retry generations", async () => {
    const organizations = deferred<{ error: { error: string }; response: { ok: boolean } }>();
    const firstOrganizationRead = deferred<void>();
    let organizationReads = 0;
    let incidentReads = 0;

    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/organizations") {
        organizationReads += 1;
        firstOrganizationRead.resolve();
        if (organizationReads === 1) return organizations.promise;
        return new Promise(() => {});
      }
      if (path === "/api/v1/incidents/{incidentID}") incidentReads += 1;
      return Promise.resolve({ data: undefined });
    });

    const pinia = createPinia();
    setActivePinia(pinia);
    const wrapper = mount(IncidentDetailView, {
      attachTo: document.body,
      global: {
        plugins: [pinia],
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });

    try {
      await firstOrganizationRead.promise;
      organizations.resolve({ error: { error: "database unavailable" }, response: { ok: false } });
      await flushPromises();
      await wrapper.vm.$nextTick();

      expect(organizationReads).toBe(1);
      expect(incidentReads).toBe(0);
      expect(wrapper.get('[data-testid="incident-load-error"]').text()).toContain(
        "Could not load the incident.",
      );
    } finally {
      wrapper.unmount();
    }
  });

  it("fails closed when the incident belongs to another project", async () => {
    const pinia = createPinia();
    setActivePinia(pinia);
    const workspace = useWorkspace();
    workspace.$patch({
      orgs: [{ id: "o1", name: "Org A" }],
      projects: [{ id: "p1", name: "Project A" }],
      orgId: "o1",
      projectId: "p1",
      projectState: "ready",
      loaded: true,
      transition: { pendingOrgId: "", generation: 7, error: "", failedOrgId: "" },
    });
    const mismatched = {
      ...INCIDENT,
      project_id: "p2",
      title: "Foreign incident",
      service_id: "svc-foreign",
    };
    let secondaryReads = 0;

    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/incidents/{incidentID}") return Promise.resolve({ data: mismatched });
      if (path === "/api/v1/incidents/{incidentID}/updates") {
        return Promise.resolve({
          data: [{ status: "identified", body: "Foreign update", created_at: "2026-10-01T00:01:00Z" }],
        });
      }
      if (path === "/api/v1/incidents/{incidentID}/postmortem") {
        return Promise.resolve({
          data: { body: "## Summary\nForeign postmortem", author: "operator", published_at: "2026-10-01T00:02:00Z" },
        });
      }
      if (path.includes("/services/") || path.endsWith("/changes")) {
        secondaryReads += 1;
      }
      return Promise.resolve({ data: undefined });
    });

    const wrapper = mount(IncidentDetailView, {
      attachTo: document.body,
      global: {
        plugins: [pinia],
        stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } },
      },
    });

    try {
      await flushPromises();

      expect(wrapper.text()).not.toContain("Foreign incident");
      expect(wrapper.text()).not.toContain("Foreign update");
      expect(wrapper.text()).not.toContain("Foreign postmortem");
      expect(wrapper.find('[data-testid="incident-load-error"]').text()).toContain(
        "This incident does not belong to the selected project, or you cannot see it.",
      );
      expect(wrapper.text()).not.toContain("No updates yet.");
      expect(wrapper.findAll("button").some((button) => ["Acknowledge", "Resolve", "Post update"].includes(button.text()))).toBe(false);
      expect(secondaryReads).toBe(0);
    } finally {
      wrapper.unmount();
    }
  });
});
