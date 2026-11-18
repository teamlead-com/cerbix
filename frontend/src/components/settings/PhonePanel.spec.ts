import { type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import PhonePanel from "@/components/settings/PhonePanel.vue";

// FR-041a §4.2, mock §2 (2a–2j, 2j′): Settings → Account → Phone. Every state is drawn from the
// session's copy of GET /api/v1/me alone — the panel makes no request on mount. The assertions are
// on what the person sees and on the body that leaves the page: the typed number goes out in the
// PUT exactly as typed and is never rendered back (D35, P3); "Send a new code" is the body-less
// POST /me/phone/verify/send (ruling 7), so it works after a reload; answers the mock has no text
// for show the server's sentence under the action that failed (ruling 9).

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

type Phone = { number_masked: string; verified: boolean; language: string | null };
interface FakeSession {
  isGlobalAdmin: boolean;
  user: { id: string; email: string };
  phone: Phone | null;
  telephonyEnabled: boolean;
  telephonyCanCall: boolean;
  telephonyVoiceLanguage: string | null;
  phoneCodeSend: "sms" | "voice" | "undelivered" | null;
  setPhone: ReturnType<typeof vi.fn>;
  setPhoneCodeSend: ReturnType<typeof vi.fn>;
  fetchMe: ReturnType<typeof vi.fn>;
}

const sessionState = vi.hoisted(() => ({ value: null as null | Record<string, unknown> }));
vi.mock("@/stores/session", () => ({ useSession: () => sessionState.value }));

const MASK = "+90•••••6869";
// Not the field's placeholder (+90 532 000 0000): typedNumberGone must be able to fail where a phone field shows.
const TYPED = "+90 533 111 2233";
const unverified = (over: Partial<Phone> = {}): Phone => ({ number_masked: MASK, verified: false, language: null, ...over });
const verified = (over: Partial<Phone> = {}): Phone => ({ number_masked: MASK, verified: true, language: null, ...over });

async function seed(over: Partial<FakeSession> = {}) {
  const { reactive } = await import("vue");
  const s = reactive({
    isGlobalAdmin: false,
    user: { id: "u1", email: "ada@example.com" },
    phone: null as Phone | null,
    telephonyEnabled: true,
    telephonyCanCall: false,
    telephonyVoiceLanguage: "en-US" as string | null,
    phoneCodeSend: null as string | null,
    setPhone: vi.fn(),
    setPhoneCodeSend: vi.fn(),
    fetchMe: vi.fn(async () => true),
    ...over,
  });
  // As the store does: the way of the last code goes with the number it belonged to.
  s.setPhone.mockImplementation((p: Phone | null) => {
    if ((p?.number_masked ?? null) !== (s.phone?.number_masked ?? null)) s.phoneCodeSend = null;
    s.phone = p;
  });
  s.setPhoneCodeSend.mockImplementation((v: string | null) => {
    s.phoneCodeSend = v;
  });
  sessionState.value = s as unknown as Record<string, unknown>;
  return s as unknown as FakeSession;
}

function ok<T>(data: T, status = 200) {
  return { data, error: undefined, response: { status, ok: true } };
}
function fail(status: number, error: unknown) {
  return { data: undefined, error, response: { status, ok: false } };
}

const mounted: VueWrapper[] = [];
async function mountPanel(): Promise<VueWrapper> {
  const w = mount(PhonePanel);
  mounted.push(w);
  await flushPromises();
  return w;
}
afterEach(() => {
  while (mounted.length) mounted.pop()!.unmount();
});

beforeEach(() => {
  for (const f of Object.values(apiMock)) f.mockReset();
  sessionState.value = null;
});

const norm = (s: string) => s.replace(/\s+/g, " ").trim();
const tid = (w: VueWrapper, id: string) => w.get(`[data-testid="${id}"]`);
const has = (w: VueWrapper, id: string) => w.find(`[data-testid="${id}"]`).exists();
const text = (w: VueWrapper, id: string) => norm(tid(w, id).text());
const disabled = (w: VueWrapper, id: string) => (tid(w, id).element as HTMLButtonElement | HTMLInputElement).disabled;
async function click(w: VueWrapper, id: string) {
  await tid(w, id).trigger("click");
  await flushPromises();
}
function noRequests() {
  for (const f of Object.values(apiMock)) expect(f).not.toHaveBeenCalled();
}
/** The typed number appears nowhere: not in the markup, not as any field's value. */
function typedNumberGone(w: VueWrapper, number: string) {
  const digits = number.replace(/\D/g, "");
  expect(w.html()).not.toContain(number);
  expect(w.html().replace(/\D/g, "")).not.toContain(digits);
  for (const el of w.findAll("input")) expect((el.element as HTMLInputElement).value).not.toBe(number);
}

const OFF_SENTENCE = "Phone alerts are not enabled on this instance, so a number cannot be confirmed now.";
const OFF_ADMIN_TAIL = "An administrator turns them on under Settings → Administration → Telephony.";
const SWITCHED_OFF =
  "No code sent: phone alerts were switched off on this instance. An administrator turns them on under Settings → Administration → Telephony.";
const CODE_INCORRECT = "That code is not right. Check the most recent SMS and try again.";
const CODE_EXPIRED = "This code has expired — codes work for 10 minutes. Send a new one.";
const CODE_EXHAUSTED = "Too many wrong codes — this code no longer works. Send a new one.";

// ── 1 · every state from the session alone ─────────────────────────────────────────────────────
describe("PhonePanel · states from GET /api/v1/me alone (no request on mount)", () => {
  it("2a — no number, phone alerts on: the explanation, the field and Send code", async () => {
    await seed({ phone: null, telephonyEnabled: true });
    const w = await mountPanel();
    noRequests();
    expect(text(w, "phone-title")).toBe("Phone for alerts");
    expect(text(w, "phone-explanation")).toBe(
      "Phone channels can text or call you at this number. cerbix sends nothing to it until you confirm it with a code.",
    );
    const input = tid(w, "phone-number-input");
    expect(input.attributes("placeholder")).toBe("+90 532 000 0000");
    expect(text(w, "phone-number-hint")).toBe("International format with the country code (E.164).");
    expect(w.text()).toContain("Mobile number");
    expect(text(w, "phone-send")).toBe("Send code");
    expect(disabled(w, "phone-number-input")).toBe(false);
    await input.setValue(TYPED);
    expect(disabled(w, "phone-send")).toBe(false);
    expect(has(w, "phone-off-notice")).toBe(false);
  });

  it("2b — stored unverified (also what a reload shows): mask, Not verified, the numbered steps", async () => {
    await seed({ phone: unverified() });
    const w = await mountPanel();
    noRequests();
    expect(text(w, "phone-masked")).toBe(MASK);
    expect(text(w, "phone-pill")).toBe("Not verified");
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code sent by SMS");
    expect(text(w, "phone-step-1")).toContain("It works for 10 minutes.");
    expect(text(w, "phone-step-2")).toContain("2 · Enter the 6-digit code");
    const code = tid(w, "phone-code-input");
    expect(code.attributes("inputmode")).toBe("numeric");
    expect(code.attributes("placeholder")).toBe("123456");
    expect(code.classes()).toContain("w-[180px]");
    expect(code.classes()).toContain("font-mono");
    expect(text(w, "phone-verify")).toBe("Verify");
    expect(text(w, "phone-resend")).toBe("Send a new code");
    expect(text(w, "phone-use-another")).toBe("Use another number");
    expect(has(w, "phone-number-input")).toBe(false);
  });

  it("2g — verified: Replace / Remove and the voice language, the instance default named with its value", async () => {
    await seed({ phone: verified(), telephonyVoiceLanguage: "en-US" });
    const w = await mountPanel();
    noRequests();
    expect(text(w, "phone-masked")).toBe(MASK);
    expect(text(w, "phone-pill")).toBe("Verified");
    expect(text(w, "phone-replace")).toBe("Replace number");
    expect(text(w, "phone-remove")).toBe("Remove");
    expect(disabled(w, "phone-replace")).toBe(false);
    const sel = tid(w, "phone-language").element as HTMLSelectElement;
    expect(sel.value).toBe("");
    expect(sel.options[0].text).toBe("Instance default (en-US)");
    expect(Array.from(sel.options).map((o) => o.value)).toContain("de-DE");
    expect(text(w, "phone-language-hint")).toBe(
      "The language cerbix speaks when it calls you. SMS text is always in English. Saved as soon as you pick it.",
    );
    expect(has(w, "phone-off-kept")).toBe(false);
  });

  it("2g — the instance language unknown (null on /me): 'Instance default' with no value", async () => {
    await seed({ phone: verified(), telephonyVoiceLanguage: null });
    const w = await mountPanel();
    const sel = tid(w, "phone-language").element as HTMLSelectElement;
    expect(sel.options[0].text).toBe("Instance default");
  });

  it("2g — a stored language outside the fixed list is still offered and selected", async () => {
    await seed({ phone: verified({ language: "az-AZ" }) });
    const w = await mountPanel();
    const sel = tid(w, "phone-language").element as HTMLSelectElement;
    expect(sel.value).toBe("az-AZ");
    expect(Array.from(sel.options).filter((o) => o.value === "az-AZ")).toHaveLength(1);
  });

  it("2j — no number, phone alerts off (a non-admin): said on load, field and Send code disabled, no link", async () => {
    await seed({ phone: null, telephonyEnabled: false, isGlobalAdmin: false });
    const w = await mountPanel();
    noRequests();
    expect(text(w, "phone-off-notice")).toBe(`${OFF_SENTENCE} ${OFF_ADMIN_TAIL}`);
    expect(has(w, "phone-off-link")).toBe(false);
    expect(disabled(w, "phone-number-input")).toBe(true);
    expect(disabled(w, "phone-send")).toBe(true);
    expect(has(w, "phone-explanation")).toBe(false);
  });

  it("2j — the same state for a global admin: the way to fix it links to Administration → Telephony", async () => {
    await seed({ phone: null, telephonyEnabled: false, isGlobalAdmin: true });
    const w = await mountPanel();
    expect(text(w, "phone-off-notice")).toBe(`${OFF_SENTENCE} Turn them on under Administration → Telephony.`);
    const link = tid(w, "phone-off-link");
    expect(norm(link.text())).toBe("Turn them on under Administration → Telephony");
    expect(link.attributes("href")).toBe("/settings?tab=telephony");
    await link.trigger("click");
    expect(w.emitted("open-tab")).toEqual([["telephony"]]);
  });

  it("2j — a degraded /me (phone: null, telephony_enabled: false) is drawn exactly as the API says", async () => {
    await seed({ phone: null, telephonyEnabled: false, telephonyVoiceLanguage: null });
    const w = await mountPanel();
    expect(text(w, "phone-off-notice")).toContain(OFF_SENTENCE);
    expect(disabled(w, "phone-send")).toBe(true);
  });

  it("2j′ — verified, phone alerts off: the number stays; Replace disabled with its reason, Remove and the language work", async () => {
    await seed({ phone: verified(), telephonyEnabled: false });
    const w = await mountPanel();
    noRequests();
    expect(text(w, "phone-pill")).toBe("Verified");
    expect(text(w, "phone-off-kept")).toBe(
      "Phone alerts are switched off on this instance, so nothing is sent to this number for now. It stays verified.",
    );
    expect(disabled(w, "phone-replace")).toBe(true);
    expect(text(w, "phone-replace-reason")).toContain("Replacing needs a code, and no SMS can be sent, so Replace is disabled.");
    expect(disabled(w, "phone-remove")).toBe(false);
    expect(disabled(w, "phone-language")).toBe(false);
  });

  it("2b with phone alerts off: the off sentence, and no code action (a number cannot be confirmed now)", async () => {
    await seed({ phone: unverified(), telephonyEnabled: false });
    const w = await mountPanel();
    expect(text(w, "phone-off-notice")).toContain(OFF_SENTENCE);
    expect(disabled(w, "phone-verify")).toBe(true);
    expect(disabled(w, "phone-resend")).toBe(true);
    expect(disabled(w, "phone-use-another")).toBe(true);
  });
});

// ── 2 · Send code ───────────────────────────────────────────────────────────────────────────────
describe("PhonePanel · Send code (2a → 2b)", () => {
  it("PUTs the number exactly as typed; the 200 is 2b with the mask, and the typed number is gone from the page", async () => {
    const s = await seed({ phone: null });
    apiMock.PUT.mockResolvedValue(ok(unverified()));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(apiMock.PUT).toHaveBeenCalledWith("/api/v1/me/phone", { body: { number: TYPED } });
    expect(s.setPhone).toHaveBeenCalledWith(unverified());
    expect(text(w, "phone-masked")).toBe(MASK);
    expect(text(w, "phone-pill")).toBe("Not verified");
    typedNumberGone(w, TYPED);
  });

  it("a refused number (400 number_invalid) shows the server's sentence under Send code and keeps the field to correct", async () => {
    await seed({ phone: null });
    apiMock.PUT.mockResolvedValue(fail(400, { error: "the number must be in international format, e.g. +14155550100", reason: "number_invalid" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue("0532 000");
    await click(w, "phone-send");
    expect(text(w, "phone-send-error")).toBe("the number must be in international format, e.g. +14155550100");
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value).toBe("0532 000");
  });

  it.each([
    [409, "number_in_use", "this number is registered to another user"],
    [409, "encryption_key_missing", "phone numbers need security.encryption_key to be configured"],
    [503, undefined, "the telephony provider's secret cannot be decrypted"],
  ])("an unmocked %i %s reads the server's sentence, no new layout", async (status, reason, sentence) => {
    await seed({ phone: null });
    apiMock.PUT.mockResolvedValue(fail(status, reason ? { error: sentence, reason } : { error: sentence }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(text(w, "phone-send-error")).toBe(sentence);
  });

  it("a 501 with no body still says no code was sent", async () => {
    await seed({ phone: null });
    apiMock.PUT.mockResolvedValue(fail(501, undefined));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(text(w, "phone-send-error")).toBe("No code sent.");
  });

  it("502 provider_rejected — the number IS stored unverified: re-read /me (→ 2b) and show the server's sentence", async () => {
    const s = await seed({ phone: null });
    s.fetchMe.mockImplementation(async () => {
      s.phone = unverified();
      return true;
    });
    apiMock.PUT.mockResolvedValue(fail(502, { error: "the telephony provider refused to send the code", reason: "provider_rejected" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(s.fetchMe).toHaveBeenCalledTimes(1);
    expect(text(w, "phone-pill")).toBe("Not verified");
    expect(text(w, "phone-send-error")).toBe("the telephony provider refused to send the code");
    typedNumberGone(w, TYPED);
  });

  it("429 send_limited on the first send: the server's sentence; nothing was stored, the field stays", async () => {
    await seed({ phone: null });
    apiMock.PUT.mockResolvedValue(fail(429, { error: "too many verification codes were requested; try again later", reason: "send_limited" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(text(w, "phone-send-error")).toBe("too many verification codes were requested; try again later");
    expect(has(w, "phone-number-input")).toBe(true);
  });

  // One rule for `telephony_disabled` on every phone surface (Task 4 review M-1, ruling 9): the
  // server's sentence; the mock's "switched off meanwhile" line only when the server sent none.
  it("409 telephony_disabled — switched off meanwhile: the server's sentence, then GET /me is re-read and 2j shows", async () => {
    const s = await seed({ phone: null, telephonyEnabled: true });
    s.fetchMe.mockImplementation(async () => {
      s.telephonyEnabled = false;
      return true;
    });
    apiMock.PUT.mockResolvedValue(fail(409, { error: "phone alerts are turned off on this instance", reason: "telephony_disabled" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(text(w, "phone-send-error")).toBe("phone alerts are turned off on this instance");
    expect(s.fetchMe).toHaveBeenCalledTimes(1);
    expect(text(w, "phone-off-notice")).toContain(OFF_SENTENCE);
    expect(disabled(w, "phone-send")).toBe(true);
    // Nothing was stored, and the number is cleared anyway: the field is disabled and empty (P3).
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value).toBe("");
    typedNumberGone(w, TYPED);
  });
});

// ── P3: the typed number is cleared once it is stored ──────────────────────────────────────────
// 2a's and 2h's fields are bound to the component's one `typed` ref, so any way back to a phone
// field after a stored PUT would show the full number again if the ref were not cleared.
describe("PhonePanel · a stored number is not held after the PUT (P3)", () => {
  it("Send code 200, then Send a new code 409 no_number and /me says phone: null → 2a's field is empty", async () => {
    const s = await seed({ phone: null });
    s.fetchMe.mockImplementation(async () => {
      s.phone = null;
      return true;
    });
    apiMock.PUT.mockResolvedValue(ok(unverified()));
    apiMock.POST.mockResolvedValue(fail(409, { error: "no phone number is stored; enter your number first", reason: "no_number" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(text(w, "phone-pill")).toBe("Not verified");
    await click(w, "phone-resend");
    expect(s.fetchMe).toHaveBeenCalledTimes(1);
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value).toBe("");
    typedNumberGone(w, TYPED);
  });

  it("Replace 200 (2h → 2b), then the number gone elsewhere (phone: null) → 2a's field is empty", async () => {
    const s = await seed({ phone: verified() });
    apiMock.PUT.mockResolvedValue(ok(unverified({ number_masked: "+90•••••2233" })));
    const w = await mountPanel();
    await click(w, "phone-replace");
    await tid(w, "phone-replace-input").setValue(TYPED);
    await click(w, "phone-replace-send");
    expect(text(w, "phone-pill")).toBe("Not verified");
    s.phone = null;
    await flushPromises();
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value).toBe("");
    typedNumberGone(w, TYPED);
  });
});

// ── 3 · the code states ─────────────────────────────────────────────────────────────────────────
describe("PhonePanel · code answers (2c–2f)", () => {
  async function typeCode(w: VueWrapper, code = "482193") {
    await tid(w, "phone-code-input").setValue(code);
  }

  it("2c — 400 code_incorrect: the line, the field marked, Verify stays", async () => {
    await seed({ phone: unverified() });
    apiMock.POST.mockResolvedValue(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
    const w = await mountPanel();
    await typeCode(w);
    await click(w, "phone-verify");
    expect(apiMock.POST).toHaveBeenCalledWith("/api/v1/me/phone/verify", { body: { code: "482193" } });
    expect(text(w, "phone-code-error")).toBe(CODE_INCORRECT);
    expect(tid(w, "phone-code-input").classes()).toContain("border-down");
    expect(disabled(w, "phone-verify")).toBe(false);
    expect(disabled(w, "phone-resend")).toBe(false);
  });

  it.each([
    ["code_expired", "the verification code has expired; request a new one", CODE_EXPIRED],
    ["code_attempts_exhausted", "too many incorrect attempts; request a new code", CODE_EXHAUSTED],
  ])("410 %s: the line, Verify disabled, Send a new code possible — and a new code enables Verify again", async (reason, error, line) => {
    await seed({ phone: unverified() });
    apiMock.POST.mockImplementation(async (path: string) => {
      if (path === "/api/v1/me/phone/verify") return fail(410, { error, reason });
      return ok(undefined, 204);
    });
    const w = await mountPanel();
    await typeCode(w);
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe(line);
    expect(disabled(w, "phone-verify")).toBe(true);
    expect(disabled(w, "phone-resend")).toBe(false);
    await click(w, "phone-resend");
    expect(apiMock.POST).toHaveBeenLastCalledWith("/api/v1/me/phone/verify/send");
    expect(has(w, "phone-code-error")).toBe(false);
    await typeCode(w, "123456");
    expect(disabled(w, "phone-verify")).toBe(false);
  });

  it("2f — 429 send_limited on Send a new code: the server's sentence; a code already received and Use another number stay possible", async () => {
    await seed({ phone: unverified() });
    apiMock.POST.mockResolvedValue(fail(429, { error: "too many verification codes were requested; try again later", reason: "send_limited" }));
    const w = await mountPanel();
    await click(w, "phone-resend");
    expect(text(w, "phone-send-error")).toBe("too many verification codes were requested; try again later");
    await typeCode(w, "123456");
    expect(disabled(w, "phone-verify")).toBe(false);
    expect(disabled(w, "phone-use-another")).toBe(false);
  });

  it("409 telephony_disabled with no sentence from the server falls back to the mock's 'switched off meanwhile' line", async () => {
    const s = await seed({ phone: null, telephonyEnabled: true });
    apiMock.PUT.mockResolvedValue(fail(409, { reason: "telephony_disabled" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(text(w, "phone-send-error")).toBe(SWITCHED_OFF);
    expect(s.fetchMe).toHaveBeenCalledTimes(1);
  });

  it("409 telephony_disabled on Send a new code: the server's sentence, and GET /me re-read", async () => {
    const s = await seed({ phone: unverified() });
    s.fetchMe.mockImplementation(async () => {
      s.telephonyEnabled = false;
      return true;
    });
    apiMock.POST.mockResolvedValue(fail(409, { error: "phone alerts are turned off on this instance", reason: "telephony_disabled" }));
    const w = await mountPanel();
    await click(w, "phone-resend");
    expect(text(w, "phone-send-error")).toBe("phone alerts are turned off on this instance");
    expect(s.fetchMe).toHaveBeenCalledTimes(1);
    expect(text(w, "phone-off-notice")).toContain(OFF_SENTENCE);
  });

  it("an unmocked verify refusal (409 encryption_key_missing) reads the server's sentence", async () => {
    await seed({ phone: unverified() });
    apiMock.POST.mockResolvedValue(fail(409, { error: "phone numbers need security.encryption_key to be configured", reason: "encryption_key_missing" }));
    const w = await mountPanel();
    await typeCode(w, "123456");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe("phone numbers need security.encryption_key to be configured");
  });
});

// ── 4 · Verify ──────────────────────────────────────────────────────────────────────────────────
describe("PhonePanel · Verify (2b → 2g)", () => {
  it("a 200 holds the verified phone in the session and shows 2g", async () => {
    const s = await seed({ phone: unverified() });
    apiMock.POST.mockResolvedValue(ok(verified()));
    const w = await mountPanel();
    await tid(w, "phone-code-input").setValue("123456");
    await click(w, "phone-verify");
    expect(apiMock.POST).toHaveBeenCalledWith("/api/v1/me/phone/verify", { body: { code: "123456" } });
    expect(s.setPhone).toHaveBeenCalledWith(verified());
    expect(text(w, "phone-pill")).toBe("Verified");
    expect(has(w, "phone-replace")).toBe(true);
    expect(has(w, "phone-code-input")).toBe(false);
  });
});

// ── 5 · the voice language ──────────────────────────────────────────────────────────────────────
describe("PhonePanel · Voice language (PATCH …/preferences)", () => {
  it("a tag PATCHes {language: tag}; 'Instance default' PATCHes {language: null}; the number is never touched", async () => {
    const s = await seed({ phone: verified() });
    apiMock.PATCH.mockImplementation(async (_p: string, init: { body: { language: string | null } }) =>
      ok(verified({ language: init.body.language })),
    );
    const w = await mountPanel();
    await tid(w, "phone-language").setValue("de-DE");
    await flushPromises();
    expect(apiMock.PATCH).toHaveBeenLastCalledWith("/api/v1/me/phone/preferences", { body: { language: "de-DE" } });
    expect(s.setPhone).toHaveBeenLastCalledWith(verified({ language: "de-DE" }));
    expect(text(w, "phone-language-saved")).toBe("✓ Saved");

    await tid(w, "phone-language").setValue("");
    await flushPromises();
    expect(apiMock.PATCH).toHaveBeenLastCalledWith("/api/v1/me/phone/preferences", { body: { language: null } });
    expect(apiMock.PUT).not.toHaveBeenCalled();
    expect(apiMock.POST).not.toHaveBeenCalled();
    expect(apiMock.DELETE).not.toHaveBeenCalled();
  });

  it("a refused change reads the server's sentence and the select returns to the stored value", async () => {
    await seed({ phone: verified({ language: "fr-FR" }) });
    apiMock.PATCH.mockResolvedValue(fail(400, { error: "language must be a BCP-47 tag such as en-US, or null" }));
    const w = await mountPanel();
    await tid(w, "phone-language").setValue("de-DE");
    await flushPromises();
    expect(text(w, "phone-language-error")).toBe("language must be a BCP-47 tag such as en-US, or null");
    expect((tid(w, "phone-language").element as HTMLSelectElement).value).toBe("fr-FR");
    expect(has(w, "phone-language-saved")).toBe(false);
  });

  it("works with phone alerts off (2j′)", async () => {
    await seed({ phone: verified(), telephonyEnabled: false });
    apiMock.PATCH.mockResolvedValue(ok(verified({ language: "tr-TR" })));
    const w = await mountPanel();
    await tid(w, "phone-language").setValue("tr-TR");
    await flushPromises();
    expect(apiMock.PATCH).toHaveBeenCalledWith("/api/v1/me/phone/preferences", { body: { language: "tr-TR" } });
  });
});

// ── 6 · Replace and Remove ──────────────────────────────────────────────────────────────────────
describe("PhonePanel · Replace (2h) and Remove (2i)", () => {
  it("Replace: the 2h warning, PUT of the new number, then 2b for it; the typed number is gone", async () => {
    const s = await seed({ phone: verified() });
    apiMock.PUT.mockResolvedValue(ok(unverified({ number_masked: "+90•••••0042" })));
    const w = await mountPanel();
    await click(w, "phone-replace");
    expect(w.text()).toContain("New mobile number");
    expect(text(w, "phone-replace-warning")).toBe(
      "Your current number stops working for alerts now. Until you confirm the new one, phone channels that alert you are not routes, and nothing is sent to either number.",
    );
    expect(text(w, "phone-replace-send")).toBe("Send code to the new number");
    await tid(w, "phone-replace-input").setValue("+90 533 000 0042");
    await click(w, "phone-replace-send");
    expect(apiMock.PUT).toHaveBeenCalledWith("/api/v1/me/phone", { body: { number: "+90 533 000 0042" } });
    expect(s.setPhone).toHaveBeenCalledWith(unverified({ number_masked: "+90•••••0042" }));
    expect(text(w, "phone-masked")).toBe("+90•••••0042");
    expect(text(w, "phone-pill")).toBe("Not verified");
    typedNumberGone(w, "+90 533 000 0042");
  });

  it("Replace → Cancel sends nothing and returns to 2g", async () => {
    await seed({ phone: verified() });
    const w = await mountPanel();
    await click(w, "phone-replace");
    await tid(w, "phone-replace-input").setValue("+90 533 000 0042");
    await click(w, "phone-replace-cancel");
    noRequests();
    expect(has(w, "phone-replace-input")).toBe(false);
    expect(has(w, "phone-replace")).toBe(true);
    typedNumberGone(w, "+90 533 000 0042");
  });

  it("Remove: the inline confirmation, DELETE, then 2a", async () => {
    const s = await seed({ phone: verified() });
    apiMock.DELETE.mockResolvedValue(ok(undefined, 204));
    const w = await mountPanel();
    await click(w, "phone-remove");
    expect(text(w, "phone-remove-warning")).toBe(
      "Remove this number? Phone channels that alert you stay, but they are not routes until you add and confirm a number again.",
    );
    expect(text(w, "phone-remove-confirm")).toBe("Remove number");
    await click(w, "phone-remove-confirm");
    expect(apiMock.DELETE).toHaveBeenCalledWith("/api/v1/me/phone");
    expect(s.setPhone).toHaveBeenCalledWith(null);
    expect(has(w, "phone-number-input")).toBe(true);
    expect(has(w, "phone-explanation")).toBe(true);
  });

  it("Remove → Cancel sends nothing", async () => {
    await seed({ phone: verified() });
    const w = await mountPanel();
    await click(w, "phone-remove");
    await click(w, "phone-remove-cancel");
    noRequests();
    expect(has(w, "phone-remove-warning")).toBe(false);
    expect(text(w, "phone-pill")).toBe("Verified");
  });

  it("Remove works with phone alerts off (2j′)", async () => {
    await seed({ phone: verified(), telephonyEnabled: false });
    apiMock.DELETE.mockResolvedValue(ok(undefined, 204));
    const w = await mountPanel();
    await click(w, "phone-remove");
    await click(w, "phone-remove-confirm");
    expect(apiMock.DELETE).toHaveBeenCalledWith("/api/v1/me/phone");
    expect(has(w, "phone-off-notice")).toBe(true);
  });

  it("a refused DELETE reads the server's sentence under the confirmation", async () => {
    await seed({ phone: verified() });
    apiMock.DELETE.mockResolvedValue(fail(404, { error: "no phone number is stored" }));
    const w = await mountPanel();
    await click(w, "phone-remove");
    await click(w, "phone-remove-confirm");
    expect(text(w, "phone-remove-error")).toBe("no phone number is stored");
  });

  // The owner's decision of 2026-11-18, a deviation from mock 2b: an unverified number can be
  // removed, whether phone alerts are on or off — the same DELETE, confirmation and wording as 2i.
  it("2b, phone alerts on: Remove asks the 2i confirmation, DELETEs, then 2a; the running code and its way are dropped", async () => {
    const s = await seed({ phone: unverified(), telephonyEnabled: true, phoneCodeSend: "voice" });
    apiMock.DELETE.mockResolvedValue(ok(undefined, 204));
    const w = await mountPanel();
    await tid(w, "phone-code-input").setValue("123456");
    expect(text(w, "phone-remove")).toBe("Remove");
    expect(disabled(w, "phone-remove")).toBe(false);
    await click(w, "phone-remove");
    expect(text(w, "phone-remove-warning")).toBe(
      "Remove this number? Phone channels that alert you stay, but they are not routes until you add and confirm a number again.",
    );
    expect(text(w, "phone-remove-confirm")).toBe("Remove number");
    await click(w, "phone-remove-confirm");
    expect(apiMock.DELETE).toHaveBeenCalledWith("/api/v1/me/phone");
    expect(s.setPhone).toHaveBeenCalledWith(null);
    expect(s.setPhoneCodeSend).toHaveBeenCalledWith(null);
    expect(s.phoneCodeSend).toBeNull();
    expect(has(w, "phone-explanation")).toBe(true);
    expect(has(w, "phone-number-input")).toBe(true);
    expect(has(w, "phone-code-input")).toBe(false);
    // A number added again starts with an empty code field and no way named from the removed one.
    s.setPhone(unverified({ number_masked: "+49•••••3302" }));
    await flushPromises();
    expect((tid(w, "phone-code-input").element as HTMLInputElement).value).toBe("");
    expect(disabled(w, "phone-verify")).toBe(true);
  });

  it("2b, phone alerts off: Remove is offered and works, then 2j", async () => {
    const s = await seed({ phone: unverified(), telephonyEnabled: false });
    apiMock.DELETE.mockResolvedValue(ok(undefined, 204));
    const w = await mountPanel();
    expect(disabled(w, "phone-verify")).toBe(true);
    expect(disabled(w, "phone-remove")).toBe(false);
    await click(w, "phone-remove");
    expect(text(w, "phone-remove-warning")).toContain("Remove this number?");
    await click(w, "phone-remove-confirm");
    expect(apiMock.DELETE).toHaveBeenCalledWith("/api/v1/me/phone");
    expect(s.setPhone).toHaveBeenCalledWith(null);
    expect(text(w, "phone-off-notice")).toContain(OFF_SENTENCE);
    expect(disabled(w, "phone-number-input")).toBe(true);
    expect(has(w, "phone-pill")).toBe(false);
  });

  it("2b: a refused Remove reads the server's sentence under the confirmation and keeps the number", async () => {
    const s = await seed({ phone: unverified() });
    apiMock.DELETE.mockResolvedValue(fail(409, { error: "the number cannot be removed now" }));
    const w = await mountPanel();
    await click(w, "phone-remove");
    await click(w, "phone-remove-confirm");
    expect(text(w, "phone-remove-error")).toBe("the number cannot be removed now");
    expect(s.setPhone).not.toHaveBeenCalled();
    expect(text(w, "phone-pill")).toBe("Not verified");
    await click(w, "phone-remove-cancel");
    expect(has(w, "phone-code-input")).toBe(true);
  });

  it("Use another number (2b): an empty Mobile number field, Send code PUTs it, Cancel returns to the code", async () => {
    await seed({ phone: unverified() });
    apiMock.PUT.mockResolvedValue(ok(unverified({ number_masked: "+49•••••3302" })));
    const w = await mountPanel();
    await click(w, "phone-use-another");
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value).toBe("");
    await click(w, "phone-another-cancel");
    noRequests();
    expect(has(w, "phone-code-input")).toBe(true);
    await click(w, "phone-use-another");
    await tid(w, "phone-number-input").setValue("+49 151 0000 3302");
    await click(w, "phone-send");
    expect(apiMock.PUT).toHaveBeenCalledWith("/api/v1/me/phone", { body: { number: "+49 151 0000 3302" } });
    expect(text(w, "phone-masked")).toBe("+49•••••3302");
    expect(has(w, "phone-code-input")).toBe(true);
    typedNumberGone(w, "+49 151 0000 3302");
  });
});

// ── 7 · after a reload (ruling 7) ───────────────────────────────────────────────────────────────
describe("PhonePanel · Send a new code after a reload (POST /me/phone/verify/send)", () => {
  it("a remount in 2b holds no number, and Send a new code is the body-less verify/send", async () => {
    await seed({ phone: unverified() });
    const first = await mountPanel();
    first.unmount();
    mounted.length = 0;
    apiMock.POST.mockResolvedValue(ok(undefined, 204));
    const w = await mountPanel();
    await click(w, "phone-resend");
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    expect(apiMock.POST.mock.calls[0]).toEqual(["/api/v1/me/phone/verify/send"]);
    expect(apiMock.PUT).not.toHaveBeenCalled();
    expect(has(w, "phone-send-error")).toBe(false);
  });

  it.each([
    ["no_number", "no phone number is stored; enter your number first"],
    ["number_already_verified", "this phone number is already verified"],
  ])("409 %s reads the server's sentence and re-reads GET /me", async (reason, error) => {
    const s = await seed({ phone: unverified() });
    apiMock.POST.mockResolvedValue(fail(409, { error, reason }));
    const w = await mountPanel();
    await click(w, "phone-resend");
    expect(text(w, "phone-send-error")).toBe(error);
    expect(s.fetchMe).toHaveBeenCalledTimes(1);
  });

  it.each([
    [502, "provider_unreachable", "the telephony provider could not be reached; try again"],
    [503, undefined, "the telephony provider's secret cannot be decrypted"],
  ])("an unmocked %i on Send a new code reads the server's sentence", async (status, reason, error) => {
    await seed({ phone: unverified() });
    apiMock.POST.mockResolvedValue(fail(status, reason ? { error, reason } : { error }));
    const w = await mountPanel();
    await click(w, "phone-resend");
    expect(text(w, "phone-send-error")).toBe(error);
  });
});

// ── Revision 9 · the code by a call ─────────────────────────────────────────────────────────────
// Beside every send of a code, "Call me with the code": the same request with `via: "voice"`,
// offered only while /me says `telephony_can_call`. No fallback between the ways — a refused call
// reads the server's sentence and nothing else is sent; the code-state lines follow the way the
// last code went, and after a call they never say SMS.
describe("PhonePanel · Call me with the code (revision 9)", () => {
  const CALL_INCORRECT = "That code is not right. Check the code from the most recent call and try again.";

  it("is offered beside each send only while /me says telephony_can_call", async () => {
    for (const canCall of [false, true]) {
      // 2a: the first send.
      await seed({ phone: null, telephonyCanCall: canCall });
      let w = await mountPanel();
      expect(has(w, "phone-send"), "Send code is always there").toBe(true);
      expect(has(w, "phone-send-call"), `2a, can call: ${canCall}`).toBe(canCall);
      if (canCall) expect(text(w, "phone-send-call")).toBe("Call me with the code");

      // 2b: Send a new code, and Use another number's field.
      await seed({ phone: unverified(), telephonyCanCall: canCall });
      w = await mountPanel();
      expect(has(w, "phone-resend")).toBe(true);
      expect(has(w, "phone-resend-call"), `2b, can call: ${canCall}`).toBe(canCall);
      if (canCall) expect(text(w, "phone-resend-call")).toBe("Call me with a new code");
      await click(w, "phone-use-another");
      expect(has(w, "phone-send-call"), `Use another number, can call: ${canCall}`).toBe(canCall);

      // 2h: Replace's send to the new number.
      await seed({ phone: verified(), telephonyCanCall: canCall });
      w = await mountPanel();
      await click(w, "phone-replace");
      expect(has(w, "phone-replace-send-call"), `2h, can call: ${canCall}`).toBe(canCall);
      if (canCall) expect(text(w, "phone-replace-send-call")).toBe("Call me with the code");
      noRequests();
    }
  });

  it("the first send by a call PUTs {number, via: \"voice\"}; then 2b and its lines say call, never SMS", async () => {
    const s = await seed({ phone: null, telephonyCanCall: true });
    apiMock.PUT.mockResolvedValue(ok(unverified()));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send-call");
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(apiMock.PUT).toHaveBeenCalledWith("/api/v1/me/phone", { body: { number: TYPED, via: "voice" } });
    expect(s.setPhone).toHaveBeenCalledWith(unverified());
    expect(text(w, "phone-masked")).toBe(MASK);
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code spoken by a call");
    typedNumberGone(w, TYPED);

    apiMock.POST.mockResolvedValue(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe(CALL_INCORRECT);
    expect(norm(w.text())).not.toMatch(/SMS/);
  });

  it("Send code by SMS still PUTs the number alone, and the lines say SMS", async () => {
    await seed({ phone: null, telephonyCanCall: true });
    apiMock.PUT.mockResolvedValue(ok(unverified()));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send");
    expect(apiMock.PUT).toHaveBeenCalledWith("/api/v1/me/phone", { body: { number: TYPED } });
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code sent by SMS");
    apiMock.POST.mockResolvedValue(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe(CODE_INCORRECT);
  });

  it("Call me with a new code POSTs verify/send {via: \"voice\"}; the way follows the last send, each way on its own", async () => {
    await seed({ phone: unverified(), telephonyCanCall: true });
    apiMock.POST.mockImplementation(async (path: string) => {
      if (path === "/api/v1/me/phone/verify") return fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" });
      return ok(undefined, 204);
    });
    const w = await mountPanel();
    await click(w, "phone-resend-call");
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    expect(apiMock.POST).toHaveBeenLastCalledWith("/api/v1/me/phone/verify/send", { body: { via: "voice" } });
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code spoken by a call");
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe(CALL_INCORRECT);
    expect(norm(w.text())).not.toMatch(/SMS/);

    // Back to SMS: the body-less send, and the SMS lines again.
    await click(w, "phone-resend");
    expect(apiMock.POST).toHaveBeenLastCalledWith("/api/v1/me/phone/verify/send");
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code sent by SMS");
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe(CODE_INCORRECT);
  });

  it("Replace by a call PUTs the new number with via voice, then 2b says call", async () => {
    await seed({ phone: verified(), telephonyCanCall: true });
    apiMock.PUT.mockResolvedValue(ok(unverified({ number_masked: "+49•••••3302" })));
    const w = await mountPanel();
    await click(w, "phone-replace");
    await tid(w, "phone-replace-input").setValue("+49 151 0000 3302");
    await click(w, "phone-replace-send-call");
    expect(apiMock.PUT).toHaveBeenCalledWith("/api/v1/me/phone", { body: { number: "+49 151 0000 3302", via: "voice" } });
    expect(text(w, "phone-masked")).toBe("+49•••••3302");
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code spoken by a call");
  });

  it("Use another number by a call PUTs it with via voice", async () => {
    await seed({ phone: unverified(), telephonyCanCall: true });
    apiMock.PUT.mockResolvedValue(ok(unverified({ number_masked: "+49•••••3302" })));
    const w = await mountPanel();
    await click(w, "phone-use-another");
    await tid(w, "phone-number-input").setValue("+49 151 0000 3302");
    await click(w, "phone-send-call");
    expect(apiMock.PUT).toHaveBeenCalledWith("/api/v1/me/phone", { body: { number: "+49 151 0000 3302", via: "voice" } });
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code spoken by a call");
  });

  it("a call refused with voice_unsupported_by_provider reads the server's sentence; no SMS is sent instead, the field stays, /me is re-read", async () => {
    const s = await seed({ phone: null, telephonyCanCall: true });
    const SENTENCE = "the telephony provider in force sends SMS only; a code cannot be spoken by a call";
    apiMock.PUT.mockResolvedValue(fail(400, { error: SENTENCE, reason: "voice_unsupported_by_provider" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send-call");
    expect(text(w, "phone-send-error")).toBe(SENTENCE);
    expect(apiMock.PUT, "no fallback to SMS").toHaveBeenCalledTimes(1);
    expect(apiMock.POST).not.toHaveBeenCalled();
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value, "nothing was stored: the field stays").toBe(TYPED);
    expect(s.fetchMe, "/me said a call could be made: re-read it").toHaveBeenCalledTimes(1);
  });

  it("a call refused on Send a new code (voice_unsupported_by_provider, via_invalid) reads the server's sentence and sends nothing else", async () => {
    for (const [reason, error] of [
      ["voice_unsupported_by_provider", "the telephony provider in force sends SMS only; a code cannot be spoken by a call"],
      ["via_invalid", "via must be sms or voice"],
    ]) {
      await seed({ phone: unverified(), telephonyCanCall: true });
      apiMock.POST.mockReset();
      apiMock.POST.mockResolvedValue(fail(400, { error, reason }));
      const w = await mountPanel();
      await click(w, "phone-resend-call");
      expect(text(w, "phone-send-error"), reason).toBe(error);
      expect(apiMock.POST, `${reason}: no fallback`).toHaveBeenCalledTimes(1);
      expect(apiMock.PUT).not.toHaveBeenCalled();
    }
  });

  it("a call refused with no sentence falls back to “No code sent.”", async () => {
    await seed({ phone: unverified(), telephonyCanCall: true });
    apiMock.POST.mockResolvedValue(fail(400, { reason: "voice_unsupported_by_provider" }));
    const w = await mountPanel();
    await click(w, "phone-resend-call");
    expect(text(w, "phone-send-error")).toBe("No code sent.");
  });

  it("after a reload with calls possible, the way of the running code is unknown: the lines name neither way", async () => {
    await seed({ phone: unverified(), telephonyCanCall: true });
    apiMock.POST.mockResolvedValue(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
    const w = await mountPanel();
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code sent");
    expect(text(w, "phone-step-1")).not.toMatch(/SMS|call/);
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe("That code is not right. Check the most recent code and try again.");
  });
});

// ── Revision 9 · review fixes ───────────────────────────────────────────────────────────────────
describe("PhonePanel · revision 9 review fixes", () => {
  const PROVIDER = "the telephony provider refused to send the code";
  const NEUTRAL_INCORRECT = "That code is not right. Check the most recent code and try again.";

  // I-1: the backend stores the new code (ending the previous one) before it calls the provider, so
  // after a 502 nobody received the running code and any earlier code is dead. Step 1 is the mock's
  // own "not sent" state, the server's sentence is under the action, and no line points at a code.
  it("I-1 — a provider error never claims a code: a failed first send (either way), and a call that failed after an SMS that went", async () => {
    for (const [button, reason] of [
      ["phone-send", "provider_rejected"],
      ["phone-send-call", "provider_unreachable"],
    ]) {
      const s = await seed({ phone: null, telephonyCanCall: true });
      s.fetchMe.mockImplementation(async () => {
        s.phone = unverified();
        return true;
      });
      apiMock.PUT.mockResolvedValue(fail(502, { error: PROVIDER, reason }));
      const w = await mountPanel();
      await tid(w, "phone-number-input").setValue(TYPED);
      await click(w, button);
      expect(text(w, "phone-pill"), button).toBe("Not verified");
      expect(text(w, "phone-step-1"), button).toBe(`1 · Send a code to ${MASK}`);
      expect(tid(w, "phone-step-1").find(".text-up").exists(), `${button}: no green tick`).toBe(false);
      expect(text(w, "phone-send-error"), button).toBe(PROVIDER);
    }

    // An SMS went, then a call got 502: the SMS code is dead, and nothing points at it.
    await seed({ phone: unverified(), telephonyCanCall: true });
    apiMock.POST.mockImplementation(async (path: string, init?: { body?: { via?: string } }) => {
      if (path === "/api/v1/me/phone/verify") return fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" });
      if (init?.body?.via === "voice") return fail(502, { error: PROVIDER, reason: "provider_rejected" });
      return ok(undefined, 204);
    });
    const w = await mountPanel();
    await click(w, "phone-resend");
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code sent by SMS");
    await click(w, "phone-resend-call");
    expect(text(w, "phone-step-1")).toBe(`1 · Send a code to ${MASK}`);
    expect(text(w, "phone-send-error")).toBe(PROVIDER);
    expect(norm(w.text())).not.toMatch(/SMS|Code sent|spoken/);
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error"), "no pointer to the dead SMS").toBe(NEUTRAL_INCORRECT);
    // The next send that goes is claimed again.
    await click(w, "phone-resend");
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code sent by SMS");
  });

  it("m-2 — the way of the last code survives a remount (a settings tab switch); it is held in the session store", async () => {
    const s = await seed({ phone: null, telephonyCanCall: true });
    apiMock.PUT.mockResolvedValue(ok(unverified()));
    const first = await mountPanel();
    await tid(first, "phone-number-input").setValue(TYPED);
    await click(first, "phone-send-call");
    expect(s.setPhoneCodeSend).toHaveBeenLastCalledWith("voice");
    first.unmount();
    mounted.length = 0;
    const w = await mountPanel();
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code spoken by a call");
    apiMock.POST.mockResolvedValue(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe("That code is not right. Check the code from the most recent call and try again.");
  });

  it("m-1/m-3 — a refused call on Send a new code re-reads /me; when it says no call, the action goes, the sentence stays, and the unknown way stays unknown", async () => {
    const s = await seed({ phone: unverified(), telephonyCanCall: true });
    s.fetchMe.mockImplementation(async () => {
      s.telephonyCanCall = false;
      return true;
    });
    const SENTENCE = "the telephony provider in force cannot place calls; ask for the code by SMS";
    apiMock.POST.mockImplementation(async (path: string) => {
      if (path === "/api/v1/me/phone/verify") return fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" });
      return fail(400, { error: SENTENCE, reason: "voice_unsupported_by_provider" });
    });
    const w = await mountPanel();
    expect(text(w, "phone-step-1")).toContain("✓ 1 · Code sent");
    await click(w, "phone-resend-call");
    expect(s.fetchMe).toHaveBeenCalledTimes(1);
    expect(has(w, "phone-resend-call"), "the call action goes with telephony_can_call").toBe(false);
    expect(text(w, "phone-send-error")).toBe(SENTENCE);
    expect(text(w, "phone-step-1"), "the running code's way did not change").not.toMatch(/SMS|call/);
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-verify");
    expect(text(w, "phone-code-error")).toBe(NEUTRAL_INCORRECT);
  });

  it("m-3 — a refused first call: when /me then says no call, the action goes, the typed number and the sentence stay", async () => {
    const s = await seed({ phone: null, telephonyCanCall: true });
    s.fetchMe.mockImplementation(async () => {
      s.telephonyCanCall = false;
      return true;
    });
    apiMock.PUT.mockResolvedValue(fail(400, { error: "no calls here", reason: "voice_unsupported_by_provider" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send-call");
    expect(has(w, "phone-send-call")).toBe(false);
    expect(has(w, "phone-send")).toBe(true);
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value).toBe(TYPED);
    expect(text(w, "phone-send-error")).toBe("no calls here");
  });

  it("m-3 — via_invalid reads the sentence and does not re-read /me", async () => {
    const s = await seed({ phone: null, telephonyCanCall: true });
    apiMock.PUT.mockResolvedValue(fail(400, { error: "via must be sms or voice", reason: "via_invalid" }));
    apiMock.POST.mockResolvedValue(fail(400, { error: "via must be sms or voice", reason: "via_invalid" }));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send-call");
    expect(text(w, "phone-send-error")).toBe("via must be sms or voice");
    s.phone = unverified();
    await flushPromises();
    await click(w, "phone-resend-call");
    expect(text(w, "phone-send-error")).toBe("via must be sms or voice");
    expect(s.fetchMe).not.toHaveBeenCalled();
  });

  it("m-3/m-4 — while a call is in flight: “Calling…”, the other sends and Verify are disabled, and Verify does not say “Verifying…”", async () => {
    await seed({ phone: null, telephonyCanCall: true });
    let release!: (v: unknown) => void;
    apiMock.PUT.mockReturnValue(new Promise((r) => (release = r)));
    const w = await mountPanel();
    await tid(w, "phone-number-input").setValue(TYPED);
    await click(w, "phone-send-call");
    expect(text(w, "phone-send-call")).toBe("Calling…");
    expect(text(w, "phone-send")).toBe("Send code");
    expect(disabled(w, "phone-send")).toBe(true);
    expect(disabled(w, "phone-send-call")).toBe(true);
    release(ok(unverified()));
    await flushPromises();

    apiMock.POST.mockReturnValue(new Promise((r) => (release = r)));
    await tid(w, "phone-code-input").setValue("482193");
    await click(w, "phone-resend-call");
    expect(text(w, "phone-resend-call")).toBe("Calling…");
    expect(text(w, "phone-resend")).toBe("Send a new code");
    expect(disabled(w, "phone-resend")).toBe(true);
    expect(disabled(w, "phone-verify")).toBe(true);
    expect(text(w, "phone-verify"), "a send is not a verify").toBe("Verify");
    expect(disabled(w, "phone-use-another")).toBe(true);
    release(ok(undefined, 204));
    await flushPromises();
    expect(text(w, "phone-resend-call")).toBe("Call me with a new code");
  });
});
