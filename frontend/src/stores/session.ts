import { defineStore } from "pinia";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { useLive } from "@/stores/live";

type User = components["schemas"]["User"];
type Membership = components["schemas"]["Membership"];

interface State {
  user: User | null;
  memberships: Membership[];
  totpEnabled: boolean;
  loaded: boolean;
  version: string;
  commit: string;
}

export const useSession = defineStore("session", {
  state: (): State => ({ user: null, memberships: [], totpEnabled: false, loaded: false, version: "", commit: "" }),
  getters: {
    isAuthed: (s) => s.user !== null,
    isGlobalAdmin: (s) => s.user?.is_global_admin === true,
    // Whether the caller may manage the given org (create projects, members):
    // a global admin, or an org-level org_admin membership.
    isOrgAdmin: (s) => (orgID: string): boolean =>
      s.user?.is_global_admin === true ||
      s.memberships.some((m) => m.org_id === orgID && !m.project_id && m.role === "org_admin"),
    // Whether the caller may write in a project (monitors, incidents, maintenance,
    // channels): a global admin, an org-level org_admin/editor, or a project-level
    // project_admin/editor. Mirrors backend ActionProjectWrite.
    canProjectWrite:
      (s) =>
      (orgID: string, projectID: string): boolean => {
        if (s.user?.is_global_admin === true) return true;
        const writeRoles = ["org_admin", "project_admin", "editor"];
        return s.memberships.some(
          (m) =>
            m.org_id === orgID &&
            (!m.project_id || m.project_id === projectID) &&
            writeRoles.includes(m.role ?? ""),
        );
      },
    initials: (s) => {
      const name = s.user?.display_name || s.user?.email || "";
      return name
        .split(/[\s@.]+/)
        .filter(Boolean)
        .slice(0, 2)
        .map((w) => w[0]?.toUpperCase())
        .join("");
    },
  },
  actions: {
    async fetchMe() {
      const { data, response } = await api.GET("/api/v1/me");
      this.loaded = true;
      if (response.status === 401 || !data) {
        this.user = null;
        this.memberships = [];
        this.totpEnabled = false;
        return false;
      }
      this.user = data.user ?? null;
      this.memberships = data.memberships ?? [];
      this.totpEnabled = data.totp_enabled ?? false;
      return this.user !== null;
    },
    // Build info for the sidebar footer — fetched once per session.
    async fetchVersion() {
      if (this.version) return;
      const { data } = await api.GET("/api/v1/version");
      this.version = data?.version ?? "";
      this.commit = data?.commit ?? "";
    },
    async logout() {
      useLive().disconnect(); // close the SSE stream before the session ends
      await fetch("/auth/logout", { method: "POST", credentials: "include" }).catch(() => {});
      this.user = null;
      this.memberships = [];
    },
  },
});
