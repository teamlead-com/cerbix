import { defineStore } from "pinia";

// Small cross-view UI state: which create dialog (if any) is open. Any view can
// open it (the switcher, the dashboard empty states); AppShell renders it once.
export const useUi = defineStore("ui", {
  state: () => ({ createKind: "" as "" | "org" | "project" }),
  actions: {
    openCreate(kind: "org" | "project") {
      this.createKind = kind;
    },
    closeCreate() {
      this.createKind = "";
    },
  },
});
