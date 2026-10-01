import { defineStore } from "pinia";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

type Organization = components["schemas"]["Organization"];
type Project = components["schemas"]["Project"];
type ProjectState = "idle" | "loading" | "ready" | "empty" | "error" | "none";

type WorkspaceTransition = {
  pendingOrgId: string;
  generation: number;
  error: string;
  failedOrgId: string;
};

interface State {
  orgs: Organization[];
  projects: Project[];
  orgId: string;
  projectId: string;
  projectState: ProjectState;
  transition: WorkspaceTransition;
  loaded: boolean;
  loading: boolean;
  // serviceCounts is how the nav knows whether to show the Services adoption badge. It is a
  // per-project cache rather than a flag, so switching projects cannot leave a stale answer.
  serviceCounts: Record<string, number>;
}

type ContextSnapshot = Pick<State, "orgId" | "projects" | "projectId" | "projectState">;

function snapshotOf(state: ContextSnapshot): ContextSnapshot {
  return {
    orgId: state.orgId,
    projects: [...state.projects],
    projectId: state.projectId,
    projectState: state.projectState,
  };
}

function responseFailure(res: { error?: { error?: string }; response?: Response }) {
  const failure = res.error;
  if (failure || (res.response && !res.response.ok)) {
    return failure?.error || "Could not load projects.";
  }
  return "";
}

async function readProjects(orgId: string): Promise<Project[]> {
  const res = await api.GET("/api/v1/organizations/{orgID}/projects", {
    params: { path: { orgID: orgId } },
  });
  const failure = responseFailure(res as { error?: { error?: string }; response?: Response });
  if (failure) throw new Error(failure);
  return res.data ?? [];
}

function chooseProject(projects: Project[], rememberedId: string, currentId = "") {
  return (
    projects.find((p) => p.id === currentId) ??
    projects.find((p) => p.id === rememberedId) ??
    projects[0]
  );
}

type TransitionSignal = {
  promise: Promise<boolean>;
  resolve: (result: boolean) => void;
};

const transitionSignals = new WeakMap<object, Map<number, TransitionSignal>>();

function beginTransitionSignal(store: object, generation: number): TransitionSignal {
  let signals = transitionSignals.get(store);
  if (!signals) {
    signals = new Map();
    transitionSignals.set(store, signals);
  }
  let resolve!: (result: boolean) => void;
  const promise = new Promise<boolean>((done) => {
    resolve = done;
  });
  const signal = { promise, resolve };
  signals.set(generation, signal);
  return signal;
}

function settleTransitionSignal(store: object, generation: number, result: boolean) {
  const signals = transitionSignals.get(store);
  const signal = signals?.get(generation);
  if (!signal) return;
  signal.resolve(result);
  signals?.delete(generation);
}

function transitionSignal(store: object, generation: number) {
  return transitionSignals.get(store)?.get(generation);
}

const LAST_ORG = "cerbix.org";
const LAST_PROJECT = "cerbix.project";
const SELECTED_ORG_PROJECT_ERROR = "Could not load projects for the selected organization.";
const WORKSPACE_TRANSITION_ERROR = "The workspace is changing organizations. Try again after it finishes.";
const INITIAL_WORKSPACE_READ_PENDING = "__workspace_initial_read__";
const FORCED_REFRESH_PENDING = "__workspace_refresh__";

/**
 * Holds the org/project selection shared by every view. `init()` loads the
 * caller's organizations, restores the last selection when it is still visible,
 * and falls back to the first entry otherwise. Switching org reloads projects.
 */
export const useWorkspace = defineStore("workspace", {
  state: (): State => ({
    orgs: [],
    projects: [],
    orgId: "",
    projectId: "",
    projectState: "idle",
    transition: {
      pendingOrgId: "",
      generation: 0,
      error: "",
      failedOrgId: "",
    },
    loaded: false,
    loading: false,
    serviceCounts: {},
  }),
  getters: {
    currentOrg: (s) => s.orgs.find((o) => o.id === s.orgId) ?? null,
    currentProject: (s) => s.projects.find((p) => p.id === s.projectId) ?? null,
    orgName(): string {
      return this.currentOrg?.name ?? this.currentOrg?.slug ?? "";
    },
    projectName(): string {
      return this.currentProject?.name ?? this.currentProject?.slug ?? "";
    },
    transitionPending(): boolean {
      return !!this.transition.pendingOrgId && !this.transition.error;
    },
    transitionError(): string {
      return this.transition.error;
    },
    projectsEmpty(): boolean {
      return this.projectState === "empty";
    },
  },
  actions: {
    async init(force = false): Promise<void> {
      if (this.transitionPending) {
        const signal = transitionSignal(this, this.transition.generation);
        const succeeded = signal ? await signal.promise : false;
        if (!succeeded && !this.loaded && this.transitionError) {
          throw new Error(this.transitionError);
        }
        return;
      }
      if (this.loaded && !force) return;
      if (!force && this.transitionError) throw new Error(this.transitionError);

      const generation = this.transition.generation + 1;
      const snapshot = snapshotOf(this);
      beginTransitionSignal(this, generation);
      let orgs = this.orgs;
      let targetOrgId = "";
      this.$patch({
        loading: true,
        projects: [],
        projectId: "",
        projectState: "loading",
        transition: {
          pendingOrgId: force
            ? this.orgId || FORCED_REFRESH_PENDING
            : INITIAL_WORKSPACE_READ_PENDING,
          generation,
          error: "",
          failedOrgId: "",
        },
      });
      try {
        const res = await api.GET("/api/v1/organizations");
        if (this.transition.generation !== generation) {
          settleTransitionSignal(this, generation, false);
          return;
        }
        const failure = (res as { error?: { error?: string }; response?: Response }).error;
        if (failure || (res.response && !res.response.ok)) {
          const message = failure?.error || "Could not load organizations.";
          throw new Error(message);
        }

        orgs = res.data ?? [];
        const remembered = localStorage.getItem(LAST_ORG);
        const pick = orgs.find((o) => o.id === remembered) ?? orgs[0];
        targetOrgId = pick?.id ?? "";
        this.$patch({ orgs });

        if (!targetOrgId) {
          this.$patch({
            orgId: "",
            projects: [],
            projectId: "",
            projectState: "none",
            loaded: true,
            transition: { pendingOrgId: "", generation, error: "", failedOrgId: "" },
          });
          localStorage.removeItem(LAST_ORG);
          localStorage.removeItem(LAST_PROJECT);
          settleTransitionSignal(this, generation, true);
          return;
        }

        // Do not publish the newly selected organization until its projects are readable. The
        // old organization remains committed while the old project list is quarantined.
        this.$patch({
          projects: [],
          projectId: "",
          projectState: "loading",
          transition: {
            pendingOrgId: targetOrgId,
            generation,
            error: "",
            failedOrgId: "",
          },
        });
        const projects = await readProjects(targetOrgId);
        if (this.transition.generation !== generation || this.transition.pendingOrgId !== targetOrgId) {
          settleTransitionSignal(this, generation, false);
          return;
        }
        const currentId = targetOrgId === snapshot.orgId ? snapshot.projectId : "";
        const pickProject = chooseProject(projects, localStorage.getItem(LAST_PROJECT) ?? "", currentId);
        this.$patch({
          orgId: targetOrgId,
          projects,
          projectId: pickProject?.id ?? "",
          projectState: projects.length ? "ready" : "empty",
          loaded: true,
          transition: { pendingOrgId: "", generation, error: "", failedOrgId: "" },
        });
        localStorage.setItem(LAST_ORG, targetOrgId);
        if (pickProject?.id) localStorage.setItem(LAST_PROJECT, pickProject.id);
        else localStorage.removeItem(LAST_PROJECT);
        settleTransitionSignal(this, generation, true);
      } catch (error) {
        if (this.transition.generation !== generation) {
          settleTransitionSignal(this, generation, false);
          return;
        }
        const hasCommittedContext = !!snapshot.orgId;
        const message = targetOrgId
          ? SELECTED_ORG_PROJECT_ERROR
          : error instanceof Error
            ? error.message
            : "Could not load organizations.";
        if (targetOrgId && !hasCommittedContext) {
          this.$patch({
            orgs,
            orgId: targetOrgId,
            projects: [],
            projectId: "",
            projectState: "error",
            loaded: false,
            transition: {
              pendingOrgId: "",
              generation,
              error: message,
              failedOrgId: targetOrgId,
            },
          });
        } else {
          this.$patch({
            ...(hasCommittedContext ? snapshot : { orgId: "", projects: [], projectId: "", projectState: "error" as const }),
            ...(hasCommittedContext ? { orgs } : { orgs }),
            loaded: hasCommittedContext,
            transition: {
              pendingOrgId: "",
              generation,
              error: message,
              failedOrgId: targetOrgId,
            },
          });
        }
        settleTransitionSignal(this, generation, false);
        throw error;
      } finally {
        if (this.transition.generation === generation) this.loading = false;
      }
    },
    async loadProjects() {
      if (!this.orgId) {
        this.$patch({ projects: [], projectId: "", projectState: "none" });
        return true;
      }
      const generation = this.transition.generation + 1;
      const snapshot = snapshotOf(this);
      this.$patch({
        projects: [],
        projectId: "",
        projectState: "loading",
        transition: { pendingOrgId: "", generation, error: "", failedOrgId: "" },
      });
      try {
        const projects = await readProjects(this.orgId);
        if (this.transition.generation !== generation || this.transitionPending) return false;
        const pick = chooseProject(projects, localStorage.getItem(LAST_PROJECT) ?? "", snapshot.projectId);
        this.$patch({
          projects,
          projectId: pick?.id ?? "",
          projectState: projects.length ? "ready" : "empty",
          transition: { pendingOrgId: "", generation, error: "", failedOrgId: "" },
        });
        if (pick?.id) localStorage.setItem(LAST_PROJECT, pick.id);
        else localStorage.removeItem(LAST_PROJECT);
        return true;
      } catch (error) {
        if (this.transition.generation !== generation || this.transitionPending) return false;
        this.$patch({ ...snapshot, projectState: "error" });
        throw error;
      }
    },
    // noteServiceCount records what a view already loaded, so the nav does not re-fetch it.
    noteServiceCount(projectId: string, n: number) {
      if (projectId) this.serviceCounts[projectId] = n;
    },
    // ensureServiceCount probes once per project. The Services badge is an adoption
    // affordance that has to disappear once a project HAS services — including services a
    // bundle created, which the operator may never have opened the screen to see.
    async ensureServiceCount() {
      const id = this.projectId;
      if (!id || this.serviceCounts[id] !== undefined) return;
      try {
        const res = await api.GET("/api/v1/projects/{projectID}/services", {
          params: { path: { projectID: id } },
        });
        if (res.data) this.serviceCounts[id] = res.data.length;
      } catch {
        // A failed probe just leaves the badge undecided; it is not worth an error surface.
      }
    },
    async selectOrg(id: string): Promise<boolean> {
      const retryingFailedOrg =
        id === this.orgId &&
        !!this.transitionError &&
        this.transition.failedOrgId === id;
      if (!id || (id === this.orgId && !retryingFailedOrg) || this.transitionPending) return false;

      const generation = this.transition.generation + 1;
      const snapshot = snapshotOf(this);
      beginTransitionSignal(this, generation);
      this.$patch({
        projects: [],
        projectId: "",
        projectState: "loading",
        loading: false,
        transition: {
          pendingOrgId: id,
          generation,
          error: "",
          failedOrgId: "",
        },
      });

      try {
        const projects = await readProjects(id);
        if (this.transition.generation !== generation || this.transition.pendingOrgId !== id) {
          settleTransitionSignal(this, generation, false);
          return false;
        }
        const pick = chooseProject(projects, localStorage.getItem(LAST_PROJECT) ?? "");
        // One synchronous patch is the publication boundary: consumers never observe a new
        // organization with the previous organization's project selection.
        this.$patch({
          orgId: id,
          projects,
          projectId: pick?.id ?? "",
          projectState: projects.length ? "ready" : "empty",
          transition: { pendingOrgId: "", generation, error: "", failedOrgId: "" },
        });
        localStorage.setItem(LAST_ORG, id);
        if (pick?.id) localStorage.setItem(LAST_PROJECT, pick.id);
        else localStorage.removeItem(LAST_PROJECT);
        settleTransitionSignal(this, generation, true);
        return true;
      } catch {
        if (this.transition.generation !== generation || this.transition.pendingOrgId !== id) {
          settleTransitionSignal(this, generation, false);
          return false;
        }
        this.$patch({
          ...snapshot,
          projectState: "error",
          transition: {
            pendingOrgId: "",
            generation,
            error: SELECTED_ORG_PROJECT_ERROR,
            failedOrgId: id,
          },
        });
        settleTransitionSignal(this, generation, false);
        return false;
      }
    },
    selectProject(id: string): boolean {
      if (this.transitionPending || !id) return false;
      if (!this.projects.some((project) => project.id === id)) return false;
      this.projectId = id;
      localStorage.setItem(LAST_PROJECT, id);
      return true;
    },
    // createOrg creates an organization and switches to it. Returns "" on success
    // or a human-readable error. Requires a global admin (enforced by the API).
    async createOrg(name: string, slug: string): Promise<string> {
      if (this.transitionPending) return WORKSPACE_TRANSITION_ERROR;
      const res = await api.POST("/api/v1/organizations", { body: { slug, name } });
      if (res.error || !res.data) {
        return (res.error as { error?: string })?.error || "Could not create the organization.";
      }
      this.orgs.push(res.data);
      const switched = await this.selectOrg(res.data.id!); // sets orgId + loads its (empty) projects
      return switched ? "" : this.transitionError || SELECTED_ORG_PROJECT_ERROR;
    },
    // createProject creates a project in the current org and switches to it.
    // Requires org-admin on the current org (enforced by the API).
    async createProject(name: string, slug: string): Promise<string> {
      if (!this.orgId) return "Select an organization first.";
      if (this.transitionPending) return WORKSPACE_TRANSITION_ERROR;
      const orgAtStart = this.orgId;
      const generationAtStart = this.transition.generation;
      const res = await api.POST("/api/v1/organizations/{orgID}/projects", {
        params: { path: { orgID: orgAtStart } },
        body: { slug, name },
      });
      if (this.orgId !== orgAtStart || this.transition.generation !== generationAtStart || this.transitionPending) {
        return WORKSPACE_TRANSITION_ERROR;
      }
      if (res.error || !res.data) {
        return (res.error as { error?: string })?.error || "Could not create the project.";
      }
      const project = res.data;
      this.$patch({
        projects: [...this.projects, project],
        projectId: project.id!,
        projectState: "ready",
      });
      localStorage.setItem(LAST_PROJECT, project.id!);
      return "";
    },
    // deleteProject permanently deletes a project and switches away from it. Returns ""
    // on success or a human-readable error. Requires org-admin on the owning org
    // (enforced by the API); refused for file-provider-managed projects.
    async deleteProject(id: string): Promise<string> {
      if (this.transitionPending) return WORKSPACE_TRANSITION_ERROR;
      const res = await api.DELETE("/api/v1/projects/{projectID}", {
        params: { path: { projectID: id } },
      });
      if (res.error) {
        const code = (res.error as { error?: string })?.error;
        if (code === "managed_by_file") {
          return "This project is managed by a file provider — remove its config files to delete it.";
        }
        return code || "Could not delete the project.";
      }
      const projects = this.projects.filter((p) => p.id !== id);
      const nextProjectId = this.projectId === id ? projects[0]?.id ?? "" : this.projectId;
      this.$patch({
        projects,
        projectId: nextProjectId,
        projectState: projects.length ? "ready" : "empty",
      });
      if (nextProjectId) localStorage.setItem(LAST_PROJECT, nextProjectId);
      else localStorage.removeItem(LAST_PROJECT);
      return "";
    },
    // deleteOrg permanently deletes an organization and switches to another. Returns ""
    // on success or a human-readable error. Requires a global admin (enforced by the API);
    // refused for orgs that own file-provider-managed projects.
    async deleteOrg(id: string): Promise<string> {
      if (this.transitionPending) return WORKSPACE_TRANSITION_ERROR;
      const res = await api.DELETE("/api/v1/organizations/{orgID}", {
        params: { path: { orgID: id } },
      });
      if (res.error) {
        const code = (res.error as { error?: string })?.error;
        if (code === "managed_by_file") {
          return "This organization has file-provider-managed projects — remove their config files to delete it.";
        }
        return code || "Could not delete the organization.";
      }
      const wasCurrent = this.orgId === id;
      this.orgs = this.orgs.filter((o) => o.id !== id);
      if (!wasCurrent) return "";

      const replacement = this.orgs[0]?.id ?? "";
      localStorage.removeItem(LAST_ORG);
      localStorage.removeItem(LAST_PROJECT);
      this.$patch({
        orgId: "",
        projects: [],
        projectId: "",
        projectState: replacement ? "loading" : "none",
        loaded: false,
        transition: {
          pendingOrgId: "",
          generation: this.transition.generation,
          error: "",
          failedOrgId: "",
        },
      });
      if (!replacement) {
        this.loaded = true;
        return "";
      }

      const switched = await this.selectOrg(replacement);
      if (!switched) return this.transitionError || SELECTED_ORG_PROJECT_ERROR;
      this.loaded = true;
      return "";
    },
  },
});
