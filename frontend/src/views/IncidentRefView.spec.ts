import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import IncidentRefView from "@/views/IncidentRefView.vue";

// FR-041a D21, mock §5: `/i/:ref` — the link in a phone message. The view normalises the reference
// on the client (5d without a request when malformed), resolves it through
// `GET /api/v1/incidents/by-ref/{ref}`, switches the workspace to the answer's organization and
// project, and REPLACES the URL with the incident's (5a-2). 403 → 5b, workspace untouched; 404 → 5d.

const apiMock = vi.hoisted(() => ({ GET: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const routerMock = vi.hoisted(() => ({ replace: vi.fn() }));
const routeState = vi.hoisted(() => ({ params: { ref: "" as string | string[] } }));
vi.mock("vue-router", async () => {
  // The real isNavigationFailure / NavigationFailureType: the view tells a failed replace apart.
  const actual = await vi.importActual<typeof import("vue-router")>("vue-router");
  const { reactive: r } = await import("vue");
  const route = r(routeState);
  return {
    isNavigationFailure: actual.isNavigationFailure,
    NavigationFailureType: actual.NavigationFailureType,
    useRoute: () => route,
    useRouter: () => routerMock,
  };
});
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot /></div>" },
}));

const uiMock = vi.hoisted(() => ({ setIncidentNotice: vi.fn(), takeIncidentNotice: vi.fn() }));
vi.mock("@/stores/ui", () => ({ useUi: () => uiMock }));

type Fn = ReturnType<typeof vi.fn>;
type Ws = {
  init: Fn;
  selectOrg: Fn;
  selectProject: Fn;
  loadProjects: Fn;
  orgs: { id: string; name: string }[];
  projects: string[];
  orgId: string;
  projectId: string;
  orgName: string;
  projectName: string;
  transitionPending: boolean;
  transitionError: string;
  transition: { failedOrgId: string };
};
const wsState = vi.hoisted(() => ({}) as Ws);
vi.mock("@/stores/workspace", async () => {
  const { reactive: r } = await import("vue");
  const ws = r(wsState);
  return { useWorkspace: () => ws };
});

const NAMES: Record<string, string> = { o1: "Home Org", o2: "Acme", p1: "home", p2: "prod", p3: "staging", "p-first": "first" };
// What each organization's project list holds, as the server would answer it.
const PROJECTS: Record<string, string[]> = { o1: ["p1", "p3"], o2: ["p-first", "p2"] };

function resetWorkspace() {
  Object.assign(wsState, {
    init: vi.fn(() => Promise.resolve()),
    orgs: [
      { id: "o1", name: "Home Org" },
      { id: "o2", name: "Acme" },
    ],
    projects: PROJECTS.o1,
    orgId: "o1",
    projectId: "p1",
    orgName: "Home Org",
    projectName: "home",
    transitionPending: false,
    transitionError: "",
    transition: { failedOrgId: "" },
    selectOrg: vi.fn(async (id: string) => {
      wsState.orgId = id;
      wsState.orgName = NAMES[id] ?? "";
      wsState.projects = PROJECTS[id] ?? [];
      wsState.projectId = wsState.projects[0] ?? "";
      wsState.projectName = NAMES[wsState.projectId] ?? "";
      wsState.transitionError = "";
      wsState.transition = { failedOrgId: "" };
      return true;
    }),
    selectProject: vi.fn((id: string) => {
      if (!wsState.projects.includes(id)) return false;
      wsState.projectId = id;
      wsState.projectName = NAMES[id] ?? id;
      return true;
    }),
    loadProjects: vi.fn(async () => {
      wsState.projects = PROJECTS[wsState.orgId] ?? [];
      return true;
    }),
  });
}

function answer(status: number, body: unknown) {
  return status >= 200 && status < 300
    ? { data: body, response: { status, ok: true } }
    : { error: body, response: { status, ok: false } };
}

function mountAt(ref: string | string[]) {
  routeState.params.ref = ref;
  return mount(IncidentRefView, {
    global: { stubs: { RouterLink: { props: ["to"], template: "<a data-testid='router-link' :data-to='JSON.stringify(to)'><slot /></a>" } } },
  });
}

describe("IncidentRefView (/i/:ref)", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    routerMock.replace.mockReset();
    uiMock.setIncidentNotice.mockReset();
    uiMock.takeIncidentNotice.mockReset();
    PROJECTS.o1 = ["p1", "p3"];
    resetWorkspace();
  });

  it.each([
    ["abc", "too short"],
    ["X7K2P9QMA", "nine characters"],
    ["X7K2P9QU", "contains U, outside Crockford base32"],
  ])("a malformed reference %s (%s) shows 5d with the raw text and sends no request", async (raw) => {
    const w = mountAt(raw);
    await flushPromises();
    expect(apiMock.GET).not.toHaveBeenCalled();
    expect(w.find('[data-testid="incident-ref-not-found"]').exists()).toBe(true);
    expect(w.text()).toContain("No incident found");
    expect(w.text()).toContain(`Nothing matches ${raw}. Check the link in the message — a reference is 8 letters and digits.`);
    expect(w.text()).toContain("Go to incidents");
    expect(routerMock.replace).not.toHaveBeenCalled();
    expect(wsState.selectOrg).not.toHaveBeenCalled();
  });

  it("normalises the reference before the request: x7k2p9qm asks for X7K2P9QM", async () => {
    apiMock.GET.mockResolvedValue(answer(404, { error: "not found" }));
    mountAt("x7k2p9qm");
    await flushPromises();
    expect(apiMock.GET).toHaveBeenCalledTimes(1);
    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/incidents/by-ref/{ref}", { params: { path: { ref: "X7K2P9QM" } } });
  });

  it("the aliases I, L and O are read as 1, 1 and 0 (i7l2p9qo → 1712P9Q0 is a request, not 5d)", async () => {
    apiMock.GET.mockResolvedValue(answer(404, { error: "not found" }));
    mountAt("i7l2p9qo");
    await flushPromises();
    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/incidents/by-ref/{ref}", { params: { path: { ref: "1712P9Q0" } } });
  });

  it("shows 5a-1 while the reference resolves", async () => {
    apiMock.GET.mockReturnValue(new Promise(() => {}));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    const card = w.find('[data-testid="incident-ref-resolving"]');
    expect(card.exists()).toBe(true);
    expect(card.text()).toBe("Opening incident X7K2P9QM…");
  });

  it("200 in the active workspace: replaces the URL with the incident's and sets no notice", async () => {
    apiMock.GET.mockResolvedValue(answer(200, { incident_id: "inc-9", project_id: "p1", org_id: "o1" }));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.init).toHaveBeenCalled();
    expect(wsState.selectOrg).not.toHaveBeenCalled();
    expect(wsState.selectProject).not.toHaveBeenCalled();
    expect(uiMock.setIncidentNotice).not.toHaveBeenCalled();
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
  });

  it("200 in another organization: selectOrg, selectProject, the 5a-2 notice, then replace", async () => {
    apiMock.GET.mockResolvedValue(answer(200, { incident_id: "inc-9", project_id: "p2", org_id: "o2" }));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.selectOrg).toHaveBeenCalledWith("o2");
    expect(wsState.selectProject).toHaveBeenCalledWith("p2");
    expect(uiMock.setIncidentNotice).toHaveBeenCalledWith("Switched to Acme / prod to open this incident.");
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
    // The notice is set BEFORE the navigation, so the incident page can take it on mount.
    expect(uiMock.setIncidentNotice.mock.invocationCallOrder[0]).toBeLessThan(routerMock.replace.mock.invocationCallOrder[0]);
  });

  it("200 in another project of the same organization: only selectProject, and the notice", async () => {
    apiMock.GET.mockResolvedValue(answer(200, { incident_id: "inc-9", project_id: "p3", org_id: "o1" }));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.selectOrg).not.toHaveBeenCalled();
    expect(wsState.selectProject).toHaveBeenCalledWith("p3");
    expect(uiMock.setIncidentNotice).toHaveBeenCalledWith("Switched to Home Org / staging to open this incident.");
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
  });

  it("200 whose organization cannot be selected: a failure line, no navigation, no notice", async () => {
    apiMock.GET.mockResolvedValue(answer(200, { incident_id: "inc-9", project_id: "p2", org_id: "o2" }));
    wsState.selectOrg = vi.fn(async () => {
      wsState.transitionError = "Could not load projects for the selected organization.";
      return false;
    });
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    expect(routerMock.replace).not.toHaveBeenCalled();
    expect(uiMock.setIncidentNotice).not.toHaveBeenCalled();
    const card = w.find('[data-testid="incident-ref-failed"]');
    expect(card.exists()).toBe(true);
    expect(card.text()).toContain("Could not load projects for the selected organization.");
    expect(card.text()).toContain("Go to incidents");
  });

  it("403 → 5b; the workspace is not switched", async () => {
    apiMock.GET.mockResolvedValue(answer(403, { error: "forbidden" }));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    const card = w.find('[data-testid="incident-ref-forbidden"]');
    expect(card.exists()).toBe(true);
    expect(card.text()).toContain("You can't open this incident");
    expect(card.text()).toContain(
      "Incident X7K2P9QM is in a project you are not a member of. Ask an admin of that project to add you.",
    );
    expect(card.find('[data-testid="router-link"]').attributes("data-to")).toBe(JSON.stringify({ name: "incidents" }));
    expect(card.text()).toContain("Go to incidents");
    expect(wsState.selectOrg).not.toHaveBeenCalled();
    expect(wsState.selectProject).not.toHaveBeenCalled();
    expect(routerMock.replace).not.toHaveBeenCalled();
  });

  it("404 → 5d with the normalised reference", async () => {
    apiMock.GET.mockResolvedValue(answer(404, { error: "not found" }));
    const w = mountAt("x7k2p9qn");
    await flushPromises();
    const card = w.find('[data-testid="incident-ref-not-found"]');
    expect(card.exists()).toBe(true);
    expect(card.text()).toContain("No incident found");
    expect(card.text()).toContain("Nothing matches X7K2P9QN. Check the link in the message — a reference is 8 letters and digits.");
    expect(card.find('[data-testid="router-link"]').attributes("data-to")).toBe(JSON.stringify({ name: "incidents" }));
    expect(wsState.selectOrg).not.toHaveBeenCalled();
    expect(routerMock.replace).not.toHaveBeenCalled();
  });

  it("another failure (500) shows the server's sentence, not 5d", async () => {
    apiMock.GET.mockResolvedValue(answer(500, { error: "database unavailable" }));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    expect(w.find('[data-testid="incident-ref-not-found"]').exists()).toBe(false);
    const card = w.find('[data-testid="incident-ref-failed"]');
    expect(card.text()).toContain("database unavailable");
  });

  it("a transport failure shows a failure line, not 5d", async () => {
    apiMock.GET.mockRejectedValue(new Error("offline"));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    expect(w.find('[data-testid="incident-ref-failed"]').text()).toContain("Could not reach the server");
    expect(routerMock.replace).not.toHaveBeenCalled();
  });

  it("an answer that arrives after the page was left navigates nowhere", async () => {
    let resolve!: (v: unknown) => void;
    apiMock.GET.mockReturnValue(new Promise((r) => (resolve = r)));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    w.unmount();
    resolve(answer(200, { incident_id: "inc-9", project_id: "p2", org_id: "o2" }));
    await flushPromises();
    expect(wsState.selectOrg).not.toHaveBeenCalled();
    expect(routerMock.replace).not.toHaveBeenCalled();
  });
});

// Review fixes (M1–M4): the paths where the loaded workspace is not yet, or no longer, the truth.
describe("IncidentRefView — the workspace it finds", () => {
  const TARGET = { incident_id: "inc-9", project_id: "p2", org_id: "o2" };

  beforeEach(() => {
    apiMock.GET.mockReset();
    routerMock.replace.mockReset();
    routerMock.replace.mockResolvedValue(undefined);
    uiMock.setIncidentNotice.mockReset();
    uiMock.takeIncidentNotice.mockReset();
    PROJECTS.o1 = ["p1", "p3"];
    resetWorkspace();
  });

  function freshTab(remembered: { org: string; project: string }) {
    Object.assign(wsState, { orgs: [], projects: [], orgId: "", projectId: "", orgName: "", projectName: "" });
    wsState.init = vi.fn(async () => {
      await Promise.resolve();
      wsState.orgs = [
        { id: "o1", name: "Home Org" },
        { id: "o2", name: "Acme" },
      ];
      wsState.orgId = remembered.org;
      wsState.orgName = NAMES[remembered.org];
      wsState.projects = PROJECTS[remembered.org];
      wsState.projectId = remembered.project;
      wsState.projectName = NAMES[remembered.project];
    });
  }

  it("M3: a fresh tab whose remembered workspace IS the incident's — the read is awaited, nothing switches, no notice", async () => {
    freshTab({ org: "o2", project: "p2" });
    apiMock.GET.mockResolvedValue(answer(200, TARGET));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.init).toHaveBeenCalledTimes(1);
    expect(wsState.init).toHaveBeenCalledWith();
    expect(wsState.selectOrg).not.toHaveBeenCalled();
    expect(wsState.selectProject).not.toHaveBeenCalled();
    expect(uiMock.setIncidentNotice).not.toHaveBeenCalled();
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
  });

  it("M3: a fresh tab that remembered another organization switches from IT, with the notice", async () => {
    freshTab({ org: "o1", project: "p1" });
    apiMock.GET.mockResolvedValue(answer(200, TARGET));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.init).toHaveBeenCalledTimes(1);
    expect(wsState.selectOrg).toHaveBeenCalledWith("o2");
    expect(uiMock.setIncidentNotice).toHaveBeenCalledWith("Switched to Acme / prod to open this incident.");
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
  });

  it("M1: a project missing from the loaded list is read again once, then opened", async () => {
    PROJECTS.o1 = ["p1", "p3", "p-new"]; // the server's list now has it; the loaded one does not
    apiMock.GET.mockResolvedValue(answer(200, { incident_id: "inc-9", project_id: "p-new", org_id: "o1" }));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.loadProjects).toHaveBeenCalledTimes(1);
    expect(wsState.projectId).toBe("p-new");
    expect(w.find('[data-testid="incident-ref-failed"]').exists()).toBe(false);
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
  });

  it("M1: still missing after the reload → the failure line; Try again recovers once the list has it", async () => {
    apiMock.GET.mockResolvedValue(answer(200, { incident_id: "inc-9", project_id: "p-new", org_id: "o1" }));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.loadProjects).toHaveBeenCalledTimes(1);
    expect(routerMock.replace).not.toHaveBeenCalled();
    expect(uiMock.setIncidentNotice).not.toHaveBeenCalled();
    const card = w.find('[data-testid="incident-ref-failed"]');
    expect(card.text()).toContain("Could not switch to the incident's project.");

    PROJECTS.o1 = ["p1", "p3", "p-new"];
    await card.findAll("button").find((b) => b.text() === "Try again")!.trigger("click");
    await flushPromises();
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
  });

  it("M1: an organization missing from the loaded list re-reads the list once, so the notice names it", async () => {
    wsState.orgs = [{ id: "o1", name: "Home Org" }];
    wsState.init = vi.fn(async (force?: boolean) => {
      if (force) wsState.orgs = [...wsState.orgs, { id: "o2", name: "Acme" }];
    });
    apiMock.GET.mockResolvedValue(answer(200, TARGET));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.init).toHaveBeenCalledWith(true);
    expect(wsState.init).toHaveBeenCalledTimes(2);
    expect(wsState.selectOrg).toHaveBeenCalledWith("o2");
    expect(uiMock.setIncidentNotice).toHaveBeenCalledWith("Switched to Acme / prod to open this incident.");
  });

  it("M2: the incident's own organization failed to load — its real error, and Try again retries it through selectOrg", async () => {
    const ERR = "Could not load projects for the selected organization.";
    // What the store holds after init() failed reading o2's projects with no committed context;
    // init() keeps throwing that standing error.
    wsState.init = vi.fn(async () => {
      Object.assign(wsState, { orgId: "o2", orgName: "Acme", projects: [], projectId: "", projectName: "" });
      wsState.transitionError = ERR;
      wsState.transition = { failedOrgId: "o2" };
      throw new Error(ERR);
    });
    const succeed = wsState.selectOrg.getMockImplementation()!;
    wsState.selectOrg = vi.fn(async () => false);
    apiMock.GET.mockResolvedValue(answer(200, TARGET));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    expect(wsState.selectOrg).toHaveBeenCalledWith("o2");
    const card = w.find('[data-testid="incident-ref-failed"]');
    expect(card.text()).toContain(ERR);
    expect(card.text()).not.toContain("Could not switch to the incident's project.");
    expect(routerMock.replace).not.toHaveBeenCalled();

    wsState.selectOrg = vi.fn(succeed);
    await card.findAll("button").find((b) => b.text() === "Try again")!.trigger("click");
    await flushPromises();
    expect(wsState.selectOrg).toHaveBeenCalledWith("o2");
    expect(routerMock.replace).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-9" } });
    // There was no working context before, so nothing was "switched".
    expect(uiMock.setIncidentNotice).not.toHaveBeenCalled();
  });

  it("M4: a refused navigation takes the notice back, so the next incident page does not show it", async () => {
    const { createRouter, createMemoryHistory } = await vi.importActual<typeof import("vue-router")>("vue-router");
    const real = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: "/", component: { template: "<div />" } },
        { path: "/incidents/:id", name: "incident", component: { template: "<div />" } },
      ],
    });
    real.beforeEach((to) => to.name !== "incident");
    await real.push("/");
    const failure = await real.replace({ name: "incident", params: { id: "inc-9" } });
    expect(failure).toBeTruthy();
    routerMock.replace.mockResolvedValue(failure);
    apiMock.GET.mockResolvedValue(answer(200, TARGET));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(uiMock.setIncidentNotice).toHaveBeenCalledTimes(1);
    expect(uiMock.takeIncidentNotice).toHaveBeenCalledTimes(1);
  });

  it("M4: a navigation that throws takes the notice back and says the page could not be opened", async () => {
    routerMock.replace.mockRejectedValue(new Error("chunk failed to load"));
    apiMock.GET.mockResolvedValue(answer(200, TARGET));
    const w = mountAt("X7K2P9QM");
    await flushPromises();
    expect(uiMock.takeIncidentNotice).toHaveBeenCalledTimes(1);
    expect(w.find('[data-testid="incident-ref-failed"]').text()).toContain("Could not open the incident page.");
  });

  it("M4: a navigation that happens leaves the notice for the incident page", async () => {
    apiMock.GET.mockResolvedValue(answer(200, TARGET));
    mountAt("X7K2P9QM");
    await flushPromises();
    expect(uiMock.setIncidentNotice).toHaveBeenCalledTimes(1);
    expect(uiMock.takeIncidentNotice).not.toHaveBeenCalled();
  });
});
