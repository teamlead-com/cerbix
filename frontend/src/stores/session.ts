import { defineStore } from "pinia";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import type { PhoneCodeSend } from "@/lib/phone";
import { useLive } from "@/stores/live";

type User = components["schemas"]["User"];
type Membership = components["schemas"]["Membership"];
type UserPhone = components["schemas"]["UserPhone"];

interface State {
  user: User | null;
  memberships: Membership[];
  totpEnabled: boolean;
  loaded: boolean;
  version: string;
  commit: string;
  // FR-041a §4.2 (revision 6/8): the signed-in person's masked phone, whether the instance's
  // telephony is on, and the instance's voice language — every phone surface reads them here, so
  // a non-admin learns that phone alerts are off without meeting a refusal. Re-read with fetchMe()
  // after a telephony save and after any `telephony_disabled` refusal.
  phone: UserPhone | null;
  telephonyEnabled: boolean;
  // Revision 9: whether a verification code can be spoken by a call (telephony on and a provider
  // that places calls) — the "call me with the code" actions are offered only while it is true.
  telephonyCanCall: boolean;
  telephonyVoiceLanguage: string | null;
  // Revision 9 (review m-2): what the last code sent from Account → Phone did, held here in memory
  // so a settings tab switch keeps it — never in browser storage: after a reload the page does not
  // know it, and a code sent from another tab or device would make a stored one a lie. It goes with
  // the number it belonged to.
  phoneCodeSend: PhoneCodeSend | null;
}

export const useSession = defineStore("session", {
  state: (): State => ({
    user: null,
    memberships: [],
    totpEnabled: false,
    loaded: false,
    version: "",
    commit: "",
    phone: null,
    telephonyEnabled: false,
    telephonyCanCall: false,
    telephonyVoiceLanguage: null,
    phoneCodeSend: null,
  }),
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
    // Whether the caller may ADMINISTER a project (FR-024 D12 `gate:override`; D-0207 item 3):
    // a global admin, an org-level org_admin, or a project-level project_admin. The ONE
    // predicate that separates editor from project_admin — components take its answer and
    // never compare role strings themselves. Mirrors backend ActionProjectManage (which also
    // gates a project's member management, iter-0206).
    canProjectAdmin:
      (s) =>
      (orgID: string, projectID: string): boolean => {
        if (s.user?.is_global_admin === true) return true;
        const adminRoles = ["org_admin", "project_admin"];
        return s.memberships.some(
          (m) =>
            m.org_id === orgID &&
            (!m.project_id || m.project_id === projectID) &&
            adminRoles.includes(m.role ?? ""),
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
        this.clearPhoneState();
        return false;
      }
      this.user = data.user ?? null;
      this.memberships = data.memberships ?? [];
      this.totpEnabled = data.totp_enabled ?? false;
      this.setPhone(data.phone ?? null);
      this.telephonyEnabled = data.telephony_enabled === true;
      this.telephonyCanCall = data.telephony_can_call === true;
      this.telephonyVoiceLanguage = data.telephony_voice_language ?? null;
      return this.user !== null;
    },
    // Account → Phone holds the answer of a phone write here, so the other surfaces see it at once.
    // Another number (or none) drops the way of the last code, which belonged to the old one.
    setPhone(p: UserPhone | null) {
      if ((p?.number_masked ?? null) !== (this.phone?.number_masked ?? null)) this.phoneCodeSend = null;
      this.phone = p;
    },
    setPhoneCodeSend(v: PhoneCodeSend | null) {
      this.phoneCodeSend = v;
    },
    clearPhoneState() {
      this.phone = null;
      this.phoneCodeSend = null;
      this.telephonyEnabled = false;
      this.telephonyCanCall = false;
      this.telephonyVoiceLanguage = null;
    },
    // Build info for the sidebar footer — fetched once per session.
    async fetchVersion() {
      if (this.version) return;
      const { data } = await api.GET("/api/v1/version");
      this.version = data?.version ?? "";
      this.commit = data?.commit ?? "";
    },
    // Sign out, then leave the SPA for the destination the server names (sec-hardening-2 H-9b).
    // POST /auth/logout answers 200 {"redirect_to": …}: fetch must not follow a redirect to an
    // external post_logout_url (`connect-src 'self'` blocks it), so the SPA navigates itself, with a
    // full page load that also drops every in-memory store. The server validates the destination;
    // without one in the answer — an error, an older server's 302 — the SPA goes to "/".
    async logout() {
      useLive().disconnect(); // close the SSE stream before the session ends
      let to = "/";
      try {
        const res = await fetch("/auth/logout", { method: "POST", credentials: "include", redirect: "manual" });
        if (res.ok) {
          const body = (await res.json()) as { redirect_to?: unknown };
          if (typeof body.redirect_to === "string" && body.redirect_to !== "") to = body.redirect_to;
        }
      } catch {
        // signed out locally either way; the server's session expires on its own
      }
      this.user = null;
      this.memberships = [];
      this.clearPhoneState();
      window.location.assign(to);
    },
  },
});
