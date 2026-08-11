import { defineStore } from "pinia";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

type Organization = components["schemas"]["Organization"];
type Project = components["schemas"]["Project"];

interface State {
  orgs: Organization[];
  projects: Project[];
  orgId: string;
  projectId: string;
  loaded: boolean;
  loading: boolean;
}

const LAST_ORG = "cerbix.org";
const LAST_PROJECT = "cerbix.project";

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
    loaded: false,
    loading: false,
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
  },
  actions: {
    async init(force = false) {
      if (this.loaded && !force) return;
      this.loading = true;
      try {
        this.orgs = (await api.GET("/api/v1/organizations")).data ?? [];
        const remembered = localStorage.getItem(LAST_ORG);
        const pick = this.orgs.find((o) => o.id === remembered) ?? this.orgs[0];
        this.orgId = pick?.id ?? "";
        await this.loadProjects();
        this.loaded = true;
      } finally {
        this.loading = false;
      }
    },
    async loadProjects() {
      if (!this.orgId) {
        this.projects = [];
        this.projectId = "";
        return;
      }
      this.projects =
        (
          await api.GET("/api/v1/organizations/{orgID}/projects", {
            params: { path: { orgID: this.orgId } },
          })
        ).data ?? [];
      const remembered = localStorage.getItem(LAST_PROJECT);
      const stillHere = this.projects.find((p) => p.id === this.projectId);
      const pick =
        stillHere ??
        this.projects.find((p) => p.id === remembered) ??
        this.projects[0];
      this.selectProject(pick?.id ?? "");
    },
    async selectOrg(id: string) {
      if (id === this.orgId) return;
      this.orgId = id;
      localStorage.setItem(LAST_ORG, id);
      this.projectId = "";
      await this.loadProjects();
    },
    selectProject(id: string) {
      this.projectId = id;
      if (id) localStorage.setItem(LAST_PROJECT, id);
    },
    // createOrg creates an organization and switches to it. Returns "" on success
    // or a human-readable error. Requires a global admin (enforced by the API).
    async createOrg(name: string, slug: string): Promise<string> {
      const res = await api.POST("/api/v1/organizations", { body: { slug, name } });
      if (res.error || !res.data) {
        return (res.error as { error?: string })?.error || "Could not create the organization.";
      }
      this.orgs.push(res.data);
      await this.selectOrg(res.data.id!); // sets orgId + loads its (empty) projects
      return "";
    },
    // createProject creates a project in the current org and switches to it.
    // Requires org-admin on the current org (enforced by the API).
    async createProject(name: string, slug: string): Promise<string> {
      if (!this.orgId) return "Select an organization first.";
      const res = await api.POST("/api/v1/organizations/{orgID}/projects", {
        params: { path: { orgID: this.orgId } },
        body: { slug, name },
      });
      if (res.error || !res.data) {
        return (res.error as { error?: string })?.error || "Could not create the project.";
      }
      this.projects.push(res.data);
      this.selectProject(res.data.id!);
      return "";
    },
    // deleteProject permanently deletes a project and switches away from it. Returns ""
    // on success or a human-readable error. Requires org-admin on the owning org
    // (enforced by the API); refused for file-provider-managed projects.
    async deleteProject(id: string): Promise<string> {
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
      this.projects = this.projects.filter((p) => p.id !== id);
      if (this.projectId === id) {
        localStorage.removeItem(LAST_PROJECT);
        this.selectProject(this.projects[0]?.id ?? "");
      }
      return "";
    },
    // deleteOrg permanently deletes an organization and switches to another. Returns ""
    // on success or a human-readable error. Requires a global admin (enforced by the API);
    // refused for orgs that own file-provider-managed projects.
    async deleteOrg(id: string): Promise<string> {
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
      this.orgs = this.orgs.filter((o) => o.id !== id);
      if (this.orgId === id) {
        this.orgId = this.orgs[0]?.id ?? "";
        if (this.orgId) localStorage.setItem(LAST_ORG, this.orgId);
        else localStorage.removeItem(LAST_ORG);
        localStorage.removeItem(LAST_PROJECT);
        await this.loadProjects();
      }
      return "";
    },
  },
});
