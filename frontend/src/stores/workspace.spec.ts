import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useWorkspace } from "@/stores/workspace";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

beforeEach(() => {
  setActivePinia(createPinia());
  localStorage.clear();
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
  apiMock.DELETE.mockReset();
});

describe("workspace read failures", () => {
  it("does not render a failed organization read as an empty tenant", async () => {
    apiMock.GET.mockResolvedValue({ error: { error: "database unavailable" }, response: { ok: false } });
    const workspace = useWorkspace();
    await expect(workspace.init()).rejects.toThrow("database unavailable");
    expect(workspace.loaded).toBe(false);
  });

  it("does not render a failed project read as an empty organization", async () => {
    apiMock.GET
      .mockResolvedValueOnce({ data: [{ id: "o1", name: "Acme" }], response: { ok: true } })
      .mockResolvedValueOnce({ error: { error: "project read failed" }, response: { ok: false } });
    const workspace = useWorkspace();
    await expect(workspace.init()).rejects.toThrow("project read failed");
    expect(workspace.orgId).toBe("o1");
    expect(workspace.projects).toEqual([]);
    expect(workspace.loaded).toBe(false);
  });
});

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason?: unknown) => void;
};

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function seededWorkspace() {
  const workspace = useWorkspace();
  workspace.$patch({
    orgs: [
      { id: "org-a", name: "Org A" },
      { id: "org-b", name: "Org B" },
      { id: "org-c", name: "Org C" },
    ],
    projects: [{ id: "old-project", name: "Old project" }],
    orgId: "org-a",
    projectId: "old-project",
    loaded: true,
  });
  return workspace;
}

describe("atomic organization transitions", () => {
  it("quarantines old projects and blocks competing selections while pending", async () => {
    const first = deferred<{ data: Array<{ id: string; name: string }> }>();
    apiMock.GET.mockImplementationOnce(() => first.promise);
    const workspace = seededWorkspace();

    const pending = workspace.selectOrg("org-b");

    expect(workspace.transitionPending).toBe(true);
    expect(workspace.projects).toEqual([]);
    expect(workspace.projectId).toBe("");
    expect(workspace.selectProject("old-project")).toBe(false);
    expect(await workspace.selectOrg("org-c")).toBe(false);

    first.resolve({ data: [{ id: "new-project", name: "New project" }] });
    expect(await pending).toBe(true);
    expect(workspace.orgId).toBe("org-b");
    expect(workspace.projects).toEqual([{ id: "new-project", name: "New project" }]);
    expect(workspace.projectId).toBe("new-project");
    expect(workspace.transitionPending).toBe(false);
  });

  it("uses the remembered project only when it belongs to the fresh organization", async () => {
    const workspace = seededWorkspace();
    localStorage.setItem("cerbix.project", "new-project");
    apiMock.GET.mockResolvedValueOnce({
      data: [
        { id: "first-project", name: "First project" },
        { id: "new-project", name: "Remembered project" },
      ],
    });

    expect(await workspace.selectOrg("org-b")).toBe(true);
    expect(workspace.projectId).toBe("new-project");

    localStorage.setItem("cerbix.project", "old-project");
    apiMock.GET.mockResolvedValueOnce({
      data: [{ id: "replacement-project", name: "Replacement project" }],
    });
    expect(await workspace.selectOrg("org-c")).toBe(true);
    expect(workspace.projectId).toBe("replacement-project");
  });

  it("commits a successful empty project response as an explicit empty state", async () => {
    const workspace = seededWorkspace();
    apiMock.GET.mockResolvedValueOnce({ data: [] });

    expect(await workspace.selectOrg("org-b")).toBe(true);
    expect(workspace.orgId).toBe("org-b");
    expect(workspace.projects).toEqual([]);
    expect(workspace.projectId).toBe("");
    expect(workspace.projectState).toBe("empty");
    expect(workspace.transitionError).toBe("");
  });

  it("restores the old committed context and exposes a recoverable error on failure", async () => {
    const workspace = seededWorkspace();
    apiMock.GET.mockResolvedValueOnce({
      error: { error: "project read failed" },
      response: { ok: false },
    });

    expect(await workspace.selectOrg("org-b")).toBe(false);
    expect(workspace.orgId).toBe("org-a");
    expect(workspace.projects).toEqual([{ id: "old-project", name: "Old project" }]);
    expect(workspace.projectId).toBe("old-project");
    expect(workspace.transitionPending).toBe(false);
    expect(workspace.transitionError).toBe("Could not load projects for the selected organization.");
    expect(workspace.projectState).toBe("error");
  });

  it("ignores a late project response from an older generation that was actually requested", async () => {
    const oldProjects = deferred<{ data: Array<{ id: string; name: string }> }>();
    const freshProjects = deferred<{ data: Array<{ id: string; name: string }> }>();
    const requestedOrganizations: string[] = [];
    apiMock.GET.mockImplementation((_path: string, options?: { params?: { path?: { orgID?: string } } }) => {
      const orgID = options?.params?.path?.orgID ?? "";
      requestedOrganizations.push(orgID);
      return orgID === "org-b" ? freshProjects.promise : oldProjects.promise;
    });
    const workspace = seededWorkspace();

    const staleLoad = workspace.loadProjects();
    const staleGeneration = workspace.transition.generation;
    const switching = workspace.selectOrg("org-b");
    const currentGeneration = workspace.transition.generation;

    expect(requestedOrganizations).toEqual(["org-a", "org-b"]);
    expect(currentGeneration).toBeGreaterThan(staleGeneration);

    freshProjects.resolve({ data: [{ id: "fresh-project", name: "Fresh project" }] });
    expect(await switching).toBe(true);

    oldProjects.resolve({ data: [{ id: "stale-project", name: "Stale project" }] });
    expect(await staleLoad).toBe(false);
    expect(workspace.orgId).toBe("org-b");
    expect(workspace.projects).toEqual([{ id: "fresh-project", name: "Fresh project" }]);
    expect(workspace.projectId).toBe("fresh-project");
  });

  it("makes force init wait for an explicit organization transition without opening another request", async () => {
    const switchingProjects = deferred<{ data: Array<{ id: string; name: string }> }>();
    apiMock.GET.mockImplementation(() => switchingProjects.promise);
    const workspace = seededWorkspace();

    const switching = workspace.selectOrg("org-b");
    const generation = workspace.transition.generation;
    const refreshing = workspace.init(true);

    expect(apiMock.GET).toHaveBeenCalledTimes(1);
    expect(workspace.transition.generation).toBe(generation);
    expect(workspace.projects).toEqual([]);
    expect(workspace.projectId).toBe("");

    switchingProjects.resolve({ data: [{ id: "new-project", name: "New project" }] });
    expect(await switching).toBe(true);
    await refreshing;

    expect(workspace.orgId).toBe("org-b");
    expect(workspace.projects).toEqual([{ id: "new-project", name: "New project" }]);
    expect(workspace.projectId).toBe("new-project");
  });

  it("quarantines the committed project and blocks selection and mutations during a forced organization read", async () => {
    const orgRead = deferred<{ data: Array<{ id: string; name: string }> }>();
    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/organizations") return orgRead.promise;
      return Promise.resolve({ data: [{ id: "refreshed-project", name: "Refreshed project" }] });
    });
    const workspace = seededWorkspace();
    localStorage.setItem("cerbix.org", "org-a");

    const refreshing = workspace.init(true);

    expect(workspace.transitionPending).toBe(true);
    expect(workspace.projects).toEqual([]);
    expect(workspace.projectId).toBe("");
    expect(workspace.selectProject("old-project")).toBe(false);
    expect(await workspace.selectOrg("org-b")).toBe(false);
    expect(await workspace.createProject("Raced project", "raced-project")).toBe(
      "The workspace is changing organizations. Try again after it finishes.",
    );
    expect(await workspace.deleteProject("old-project")).toBe(
      "The workspace is changing organizations. Try again after it finishes.",
    );
    expect(apiMock.POST).not.toHaveBeenCalled();
    expect(apiMock.DELETE).not.toHaveBeenCalled();

    orgRead.resolve({ data: workspace.orgs });
    await expect(refreshing).resolves.toBeUndefined();
    expect(workspace.orgId).toBe("org-a");
    expect(workspace.projects).toEqual([{ id: "refreshed-project", name: "Refreshed project" }]);
    expect(workspace.projectId).toBe("refreshed-project");
  });

  it("does not publish a late project creation response into a newer organization", async () => {
    const created = deferred<{ data: { id: string; name: string } }>();
    const replacement = deferred<{ data: Array<{ id: string; name: string }> }>();
    apiMock.POST.mockImplementationOnce(() => created.promise);
    apiMock.GET.mockImplementationOnce(() => replacement.promise);
    const workspace = seededWorkspace();

    const creating = workspace.createProject("Created in A", "created-in-a");
    const switching = workspace.selectOrg("org-b");
    expect(workspace.projects).toEqual([]);

    created.resolve({ data: { id: "project-a-created", name: "Created in A" } });
    expect(await creating).not.toBe("");
    expect(workspace.projects).toEqual([]);

    replacement.resolve({ data: [{ id: "project-b", name: "Project B" }] });
    expect(await switching).toBe(true);
    expect(workspace.orgId).toBe("org-b");
    expect(workspace.projects).toEqual([{ id: "project-b", name: "Project B" }]);
    expect(workspace.projectId).toBe("project-b");
  });

  it("reports a failed organization creation transition instead of claiming success", async () => {
    const workspace = seededWorkspace();
    apiMock.POST.mockResolvedValueOnce({ data: { id: "org-c", name: "Org C" } });
    apiMock.GET.mockResolvedValueOnce({
      error: { error: "project read failed" },
      response: { ok: false },
    });

    await expect(workspace.createOrg("Org C", "org-c")).resolves.toBe(
      "Could not load projects for the selected organization.",
    );
    expect(workspace.orgId).toBe("org-a");
  });

  it("does not restore a deleted organization's project after replacement loading fails", async () => {
    const workspace = seededWorkspace();
    localStorage.setItem("cerbix.org", "org-a");
    localStorage.setItem("cerbix.project", "old-project");
    apiMock.DELETE.mockResolvedValueOnce({});
    apiMock.GET.mockResolvedValueOnce({
      error: { error: "replacement project read failed" },
      response: { ok: false },
    });

    await expect(workspace.deleteOrg("org-a")).resolves.toBe(
      "Could not load projects for the selected organization.",
    );
    expect(workspace.orgId).toBe("");
    expect(workspace.projects).toEqual([]);
    expect(workspace.projectId).toBe("");
    expect(workspace.projectState).toBe("error");
    expect(workspace.transitionError).toBe("Could not load projects for the selected organization.");
    expect(localStorage.getItem("cerbix.org")).toBeNull();
    expect(localStorage.getItem("cerbix.project")).toBeNull();
  });

  it("keeps forced initialization atomic and restores the committed context on project-read failure", async () => {
    const projects = deferred<
      | { data: Array<{ id: string; name: string }> }
      | { error: { error: string }; response: { ok: boolean } }
    >();
    localStorage.setItem("cerbix.org", "org-b");
    apiMock.GET
      .mockResolvedValueOnce({ data: [{ id: "org-a", name: "Org A" }, { id: "org-b", name: "Org B" }] })
      .mockImplementationOnce(() => projects.promise);
    const workspace = seededWorkspace();

    const refreshing = workspace.init(true);
    await Promise.resolve();
    await Promise.resolve();
    expect(workspace.orgId).toBe("org-a");
    expect(workspace.projects).toEqual([]);
    expect(workspace.projectId).toBe("");
    expect(workspace.transitionPending).toBe(true);

    projects.resolve({ error: { error: "replacement project read failed" }, response: { ok: false } });
    await expect(refreshing).rejects.toThrow("replacement project read failed");
    expect(workspace.orgId).toBe("org-a");
    expect(workspace.projects).toEqual([{ id: "old-project", name: "Old project" }]);
  });

  it("waits for an explicit organization transition when init is called during it", async () => {
    const replacement = deferred<{ data: Array<{ id: string; name: string }> }>();
    apiMock.GET.mockImplementationOnce(() => replacement.promise);
    const workspace = seededWorkspace();
    const switching = workspace.selectOrg("org-b");
    const initializing = workspace.init();
    let settled = false;
    void initializing.then(() => { settled = true; });
    await Promise.resolve();
    expect(settled).toBe(false);

    replacement.resolve({ data: [{ id: "project-b", name: "Project B" }] });
    expect(await switching).toBe(true);
    await initializing;
    expect(settled).toBe(true);
  });

  it("keeps projectState truthful across create and delete, and rejects the empty project ID", async () => {
    const workspace = seededWorkspace();
    workspace.$patch({ projects: [], projectId: "", projectState: "empty" });
    apiMock.POST.mockResolvedValueOnce({ data: { id: "created", name: "Created" } });
    expect(await workspace.createProject("Created", "created")).toBe("");
    expect(workspace.projectState).toBe("ready");
    expect(workspace.projects).toEqual([{ id: "created", name: "Created" }]);
    apiMock.DELETE.mockResolvedValueOnce({});
    expect(await workspace.deleteProject("created")).toBe("");
    expect(workspace.projects).toEqual([]);
    expect(workspace.projectState).toBe("empty");
    expect(workspace.selectProject("")).toBe(false);
  });

  it("allows retrying the failed cold-start organization with the same ID", async () => {
    const workspace = useWorkspace();
    apiMock.GET
      .mockResolvedValueOnce({ data: [{ id: "org-b", name: "Org B" }] })
      .mockResolvedValueOnce({ error: { error: "first project read failed" }, response: { ok: false } });

    await expect(workspace.init()).rejects.toThrow("first project read failed");
    expect(workspace.orgId).toBe("org-b");
    expect(workspace.transition.failedOrgId).toBe("org-b");
    expect(workspace.transitionError).toBe("Could not load projects for the selected organization.");

    apiMock.GET.mockResolvedValueOnce({ data: [{ id: "project-b", name: "Project B" }] });
    expect(await workspace.selectOrg("org-b")).toBe(true);
    expect(workspace.orgId).toBe("org-b");
    expect(workspace.projectId).toBe("project-b");
    expect(workspace.transitionError).toBe("");
  });

  it("allows retrying a failed forced refresh of the current organization", async () => {
    const workspace = seededWorkspace();
    localStorage.setItem("cerbix.org", "org-a");
    apiMock.GET
      .mockResolvedValueOnce({ data: [{ id: "org-a", name: "Org A" }] })
      .mockResolvedValueOnce({ error: { error: "refresh project read failed" }, response: { ok: false } });

    await expect(workspace.init(true)).rejects.toThrow("refresh project read failed");
    expect(workspace.orgId).toBe("org-a");
    expect(workspace.transition.failedOrgId).toBe("org-a");

    apiMock.GET.mockResolvedValueOnce({ data: [{ id: "project-a", name: "Project A" }] });
    expect(await workspace.selectOrg("org-a")).toBe(true);
    expect(workspace.projectId).toBe("project-a");
    expect(workspace.transitionError).toBe("");
  });

  it("marks cold initialization pending before the organization read and makes concurrent init wait", async () => {
    const organizations = deferred<{ data: Array<{ id: string; name: string }> }>();
    const projects = deferred<{ data: Array<{ id: string; name: string }> }>();
    let organizationReads = 0;
    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/organizations") {
        organizationReads += 1;
        return organizations.promise;
      }
      return projects.promise;
    });
    const workspace = useWorkspace();

    const first = workspace.init();
    const firstGeneration = workspace.transition.generation;
    const second = workspace.init();
    const pendingBeforeResponse = workspace.transitionPending;
    const pendingMarker = workspace.transition.pendingOrgId;
    const readsBeforeResponse = organizationReads;
    const generationAfterSecondInit = workspace.transition.generation;

    organizations.resolve({ data: [{ id: "org-a", name: "Org A" }] });
    await Promise.resolve();
    await Promise.resolve();
    projects.resolve({ data: [{ id: "project-a", name: "Project A" }] });
    await Promise.allSettled([first, second]);

    expect(pendingBeforeResponse).toBe(true);
    expect(pendingMarker).not.toBe("");
    expect(readsBeforeResponse).toBe(1);
    expect(generationAfterSecondInit).toBe(firstGeneration);
    expect(workspace.orgId).toBe("org-a");
    expect(workspace.projectId).toBe("project-a");
    expect(workspace.transitionPending).toBe(false);
  });

  it("settles concurrent cold initialization with the same failure and does not open another generation", async () => {
    const organizations = deferred<{ error: { error: string }; response: { ok: boolean } }>();
    let organizationReads = 0;
    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/organizations") {
        organizationReads += 1;
        return organizations.promise;
      }
      return Promise.resolve({ data: [] });
    });
    const workspace = useWorkspace();

    const first = workspace.init();
    const generation = workspace.transition.generation;
    const second = workspace.init();
    const firstFailure = expect(first).rejects.toThrow("database unavailable");
    const secondFailure = expect(second).rejects.toThrow("database unavailable");

    organizations.resolve({ error: { error: "database unavailable" }, response: { ok: false } });
    await firstFailure;
    await secondFailure;

    expect(organizationReads).toBe(1);
    expect(workspace.transition.generation).toBe(generation);
    expect(workspace.transitionPending).toBe(false);
    expect(workspace.transitionError).toBe("database unavailable");
  });
});
