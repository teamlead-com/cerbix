import { defineStore } from "pinia";

// Small cross-view UI state: which create dialog (if any) is open. Any view can
// open it (the switcher, the dashboard empty states); AppShell renders it once.
// `incidentNotice` (FR-041a mock 5a-2): `/i/:ref` sets "Switched to <org> / <project> to open this
// incident." when it changed the workspace; the incident page takes it ONCE.
export const useUi = defineStore("ui", {
  state: () => ({ createKind: "" as "" | "org" | "project", incidentNotice: "" }),
  actions: {
    setIncidentNotice(s: string) {
      this.incidentNotice = s;
    },
    takeIncidentNotice(): string {
      const s = this.incidentNotice;
      this.incidentNotice = "";
      return s;
    },
    openCreate(kind: "org" | "project") {
      this.createKind = kind;
    },
    closeCreate() {
      this.createKind = "";
    },
  },
});
