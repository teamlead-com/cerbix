import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "@/api/client";
import { useSession } from "@/stores/session";

vi.mock("@/api/client", () => ({ api: { GET: vi.fn() } }));

// FR-024 D-0207 item 3: `canProjectAdmin` is the ONE predicate that separates editor from
// project_admin — the override controls render on its answer and nowhere is a role string compared.
// `canProjectWrite` (editor+) is unchanged by its arrival.

type Membership = { org_id: string; project_id?: string | null; role: string };

function sessionWith(memberships: Membership[], globalAdmin = false) {
  const s = useSession();
  s.user = { id: "u1", email: "u@example.com", is_global_admin: globalAdmin } as typeof s.user;
  s.memberships = memberships as typeof s.memberships;
  return s;
}

describe("session.canProjectAdmin", () => {
  beforeEach(() => setActivePinia(createPinia()));

  it("is true for a global admin regardless of memberships", () => {
    const s = sessionWith([], true);
    expect(s.canProjectAdmin("o1", "p1")).toBe(true);
    expect(s.canProjectWrite("o1", "p1")).toBe(true);
  });

  it("is true for an org-level org_admin of that org", () => {
    const s = sessionWith([{ org_id: "o1", project_id: null, role: "org_admin" }]);
    expect(s.canProjectAdmin("o1", "p1")).toBe(true);
    expect(s.canProjectAdmin("o1", "p2"), "org-level: every project of the org").toBe(true);
    expect(s.canProjectAdmin("o2", "p1"), "not another org").toBe(false);
  });

  it("is true for a project_admin of THAT project only", () => {
    const s = sessionWith([{ org_id: "o1", project_id: "p1", role: "project_admin" }]);
    expect(s.canProjectAdmin("o1", "p1")).toBe(true);
    expect(s.canProjectAdmin("o1", "p2"), "another project of the same org").toBe(false);
    expect(s.canProjectAdmin("o2", "p1")).toBe(false);
  });

  it("is false for an editor and a viewer — who may still WRITE (editor) but never override", () => {
    const editor = sessionWith([{ org_id: "o1", project_id: "p1", role: "editor" }]);
    expect(editor.canProjectAdmin("o1", "p1")).toBe(false);
    expect(editor.canProjectWrite("o1", "p1"), "canProjectWrite is unchanged: an editor writes").toBe(true);

    setActivePinia(createPinia());
    const orgEditor = sessionWith([{ org_id: "o1", project_id: null, role: "editor" }]);
    expect(orgEditor.canProjectAdmin("o1", "p1")).toBe(false);
    expect(orgEditor.canProjectWrite("o1", "p1")).toBe(true);

    setActivePinia(createPinia());
    const viewer = sessionWith([{ org_id: "o1", project_id: "p1", role: "viewer" }]);
    expect(viewer.canProjectAdmin("o1", "p1")).toBe(false);
    expect(viewer.canProjectWrite("o1", "p1")).toBe(false);
  });

  it("is false with no user at all", () => {
    const s = useSession();
    expect(s.canProjectAdmin("o1", "p1")).toBe(false);
    expect(s.canProjectWrite("o1", "p1")).toBe(false);
  });
});

// FR-041a §4.2 (revision 6/8): a non-admin learns the instance's telephony state, their own masked
// phone and the instance's voice language from GET /api/v1/me — held here so every surface reads one
// answer, and cleared with the session.
describe("session.fetchMe — phone state", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.mocked(api.GET).mockReset();
  });

  function answer(status: number, data: unknown) {
    vi.mocked(api.GET).mockResolvedValueOnce({ data, response: new Response(null, { status }) } as never);
  }

  it("stores phone, telephony_enabled and telephony_voice_language", async () => {
    answer(200, {
      user: { id: "u1", email: "u@example.com" },
      memberships: [],
      phone: { number_masked: "+90•••••6869", verified: true, language: "tr-TR" },
      telephony_enabled: true,
      telephony_can_call: true,
      telephony_voice_language: "en-GB",
    });
    const s = useSession();
    expect(await s.fetchMe()).toBe(true);
    expect(vi.mocked(api.GET)).toHaveBeenCalledWith("/api/v1/me");
    expect(s.phone).toEqual({ number_masked: "+90•••••6869", verified: true, language: "tr-TR" });
    expect(s.telephonyEnabled).toBe(true);
    expect(s.telephonyCanCall, "revision 9: a code can be spoken by a call").toBe(true);
    expect(s.telephonyVoiceLanguage).toBe("en-GB");
  });

  it("missing fields read as no phone, telephony off, no language", async () => {
    answer(200, { user: { id: "u1", email: "u@example.com" }, memberships: [] });
    const s = useSession();
    await s.fetchMe();
    expect(s.phone).toBeNull();
    expect(s.telephonyEnabled).toBe(false);
    expect(s.telephonyCanCall).toBe(false);
    expect(s.telephonyVoiceLanguage).toBeNull();
  });

  it("only a literal true turns telephony on, or offers a call", async () => {
    answer(200, { user: { id: "u1", email: "u@example.com" }, telephony_enabled: "yes", telephony_can_call: "yes", phone: null });
    const s = useSession();
    await s.fetchMe();
    expect(s.telephonyEnabled).toBe(false);
    expect(s.telephonyCanCall).toBe(false);
    expect(s.phone).toBeNull();
  });

  it("a 401 clears what an earlier answer stored", async () => {
    const s = useSession();
    answer(200, {
      user: { id: "u1", email: "u@example.com" },
      phone: { number_masked: "+90•••••6869", verified: false, language: null },
      telephony_enabled: true,
      telephony_can_call: true,
      telephony_voice_language: "en-US",
    });
    await s.fetchMe();
    expect(s.telephonyEnabled).toBe(true);
    expect(s.telephonyCanCall).toBe(true);

    answer(401, undefined);
    expect(await s.fetchMe()).toBe(false);
    expect(s.user).toBeNull();
    expect(s.phone).toBeNull();
    expect(s.telephonyEnabled).toBe(false);
    expect(s.telephonyCanCall).toBe(false);
    expect(s.telephonyVoiceLanguage).toBeNull();
  });

  it("a later answer that can no longer call takes the call away (telephony on, telephony_can_call false)", async () => {
    const s = useSession();
    answer(200, { user: { id: "u1", email: "u@example.com" }, telephony_enabled: true, telephony_can_call: true });
    await s.fetchMe();
    expect(s.telephonyCanCall).toBe(true);
    answer(200, { user: { id: "u1", email: "u@example.com" }, telephony_enabled: true, telephony_can_call: false });
    await s.fetchMe();
    expect(s.telephonyEnabled).toBe(true);
    expect(s.telephonyCanCall).toBe(false);
  });

  it("setPhone replaces the held phone without a request", () => {
    const s = useSession();
    s.setPhone({ number_masked: "+44•••••0199", verified: false, language: null });
    expect(s.phone?.number_masked).toBe("+44•••••0199");
    s.setPhone(null);
    expect(s.phone).toBeNull();
    expect(vi.mocked(api.GET)).not.toHaveBeenCalled();
  });
});

// Revision 9, review m-2: the way the last code went from Account → Phone ("sms", "voice", or
// "undelivered" after a provider error) lives in this in-memory store, so a settings tab switch keeps
// it. It is never written to browser storage — after a reload the page does not know it and says so —
// and it is dropped whenever the number it belonged to goes.
describe("session.phoneCodeSend — the way of the last code, in memory only", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.mocked(api.GET).mockReset();
    sessionStorage.clear();
    localStorage.clear();
  });
  const PHONE = { number_masked: "+90•••••6869", verified: false, language: null };

  it("starts unknown, holds what it is given, and touches no browser storage", () => {
    const s = useSession();
    expect(s.phoneCodeSend).toBeNull();
    s.setPhone(PHONE);
    s.setPhoneCodeSend("voice");
    expect(s.phoneCodeSend).toBe("voice");
    s.setPhoneCodeSend("undelivered");
    expect(s.phoneCodeSend).toBe("undelivered");
    expect(sessionStorage.length).toBe(0);
    expect(localStorage.length).toBe(0);
  });

  it("stays with the same number, and goes with a different one or none", () => {
    const s = useSession();
    s.setPhone(PHONE);
    s.setPhoneCodeSend("sms");
    s.setPhone({ ...PHONE, language: "de-DE" });
    expect(s.phoneCodeSend, "same number: kept").toBe("sms");
    s.setPhone({ ...PHONE, number_masked: "+49•••••3302" });
    expect(s.phoneCodeSend, "another number").toBeNull();
    s.setPhoneCodeSend("voice");
    s.setPhone(null);
    expect(s.phoneCodeSend, "no number").toBeNull();
  });

  it("a re-read of /me that brings another number drops it; the same number keeps it; a 401 clears it", async () => {
    const s = useSession();
    s.setPhone(PHONE);
    s.setPhoneCodeSend("voice");
    answer(200, { user: { id: "u1", email: "u@example.com" }, phone: PHONE, telephony_enabled: true });
    await s.fetchMe();
    expect(s.phoneCodeSend).toBe("voice");
    answer(200, { user: { id: "u1", email: "u@example.com" }, phone: { ...PHONE, number_masked: "+44•••••0199" }, telephony_enabled: true });
    await s.fetchMe();
    expect(s.phoneCodeSend).toBeNull();
    s.setPhoneCodeSend("sms");
    answer(401, undefined);
    await s.fetchMe();
    expect(s.phoneCodeSend).toBeNull();
  });

  function answer(status: number, data: unknown) {
    vi.mocked(api.GET).mockResolvedValueOnce({ data, response: new Response(null, { status }) } as never);
  }
});
