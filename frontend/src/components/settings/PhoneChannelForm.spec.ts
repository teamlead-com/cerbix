import { type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import PhoneChannelForm from "@/components/settings/PhoneChannelForm.vue";

// FR-041a §4.2/§4.3, D28, mock §3 (3a, 3a′, 3b-1..3, 3c, 3d, 3f′): the part of the channel form
// that replaces the config fields for an SMS or Voice call channel. The assertions are on what the
// operator sees and on the bodies that leave the page:
// - a phone channel carries typed fields, never `config` (§4.3), and `enabled` is left to the server
//   (a number channel is created disabled whatever the request says, §4.2);
// - D28: a project admin names any eligible recipient or a fixed number; an editor only themselves;
// - the fixed-number flow sends nothing on Create (decision 11): "Send code" is its own step, and a
//   confirmed number does not enable the channel by itself (decision 12);
// - a refusal reads the server's sentence under the action that failed (§14 point 9).

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

interface FakeSession {
  user: { id: string; email: string; display_name?: string };
  admin: boolean;
  telephonyEnabled: boolean;
  telephonyCanCall: boolean;
  canProjectWrite: (o: string, p: string) => boolean;
  canProjectAdmin: (o: string, p: string) => boolean;
  fetchMe: ReturnType<typeof vi.fn>;
}
const sessionState = vi.hoisted(() => ({ value: null as null | Record<string, unknown> }));
vi.mock("@/stores/session", () => ({ useSession: () => sessionState.value }));

async function seed(over: Partial<FakeSession> = {}): Promise<FakeSession> {
  const { reactive } = await import("vue");
  const s = reactive({
    user: { id: "u-ada", email: "ada@example.com", display_name: "Ada Park" },
    admin: true,
    telephonyEnabled: true,
    telephonyCanCall: false,
    fetchMe: vi.fn(async () => true),
    ...over,
  }) as unknown as FakeSession;
  s.canProjectWrite = () => true;
  s.canProjectAdmin = () => s.admin;
  sessionState.value = s as unknown as Record<string, unknown>;
  return s;
}

const RECIPIENTS = [
  { user_id: "u-ada", display_name: "Ada Park", email: "ada@example.com", phone_verified: true },
  { user_id: "u-bo", display_name: "Bo Lindqvist", email: "bo@example.com", phone_verified: false },
  { user_id: "u-chen", display_name: "Chen Wei", email: "chen@example.com", phone_verified: true },
];

function ok<T>(data: T, status = 200) {
  return { data, error: undefined, response: { status, ok: true } };
}
function fail(status: number, error: unknown) {
  return { data: undefined, error, response: { status, ok: false } };
}

const TYPED = "+44 7700 900199";
const MASK = "+44•••••0199";

const created = (over: Record<string, unknown> = {}) => ({
  id: "nc-noc",
  project_id: "p1",
  type: "sms",
  name: "NOC duty phone",
  config: {},
  enabled: false,
  recipient: "number",
  number_masked: MASK,
  number_verified: false,
  disabled_reason: "number_unverified",
  deliverable: { route: false, reason: "disabled" },
  ...over,
});

const mounted: VueWrapper[] = [];
async function mountForm(props: Record<string, unknown> = {}, recipients: unknown = RECIPIENTS): Promise<VueWrapper> {
  apiMock.GET.mockResolvedValue(ok(recipients));
  const w = mount(PhoneChannelForm, {
    props: { orgId: "o1", projectId: "p1", type: "sms", name: "SMS — Ada", channel: null, ...props },
  });
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
async function click(w: VueWrapper, id: string) {
  await tid(w, id).trigger("click");
  await flushPromises();
}
function lastCall(fn: ReturnType<typeof vi.fn>) {
  const calls = fn.mock.calls;
  expect(calls.length, "a request was sent").toBeGreaterThan(0);
  return calls[calls.length - 1] as [string, { params?: { path?: Record<string, string> }; body?: unknown }];
}
function optionTexts(w: VueWrapper, id: string) {
  return tid(w, id)
    .findAll("option")
    .map((o) => norm(o.text()));
}

describe("PhoneChannelForm — creating a person channel (3a, 3a′, 3c)", () => {
  it("lists every eligible recipient for a project admin, with the verified mark, and creates with typed fields only", async () => {
    await seed();
    const w = await mountForm();

    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/projects/{projectID}/phone-recipients", { params: { path: { projectID: "p1" } } });
    expect(text(w, "phone-send-to-hint")).toBe("Fixed when the channel is created.");
    expect(optionTexts(w, "phone-recipient-select")).toEqual([
      "Ada Park · ada@example.com — ✓ verified number",
      "Bo Lindqvist · bo@example.com — no verified number",
      "Chen Wei · chen@example.com — ✓ verified number",
    ]);
    expect(text(w, "phone-recipient-hint")).toBe(
      "People who can open this project — members, organization members and global admins. No number is shown, not even masked.",
    );
    expect((tid(w, "phone-send-to-number").element as HTMLInputElement).disabled, "an admin may pick a fixed number").toBe(false);
    expect(has(w, "phone-recipient-unverified"), "Ada has a verified number").toBe(false);

    apiMock.POST.mockResolvedValue(ok(created({ recipient: "user", recipient_user_id: "u-ada", enabled: true }), 201));
    await click(w, "phone-channel-submit");

    const [path, init] = lastCall(apiMock.POST);
    expect(path).toBe("/api/v1/projects/{projectID}/notification-channels");
    expect(init.body).toEqual({ type: "sms", name: "SMS — Ada", recipient: "user", recipient_user_id: "u-ada" });
    expect(Object.hasOwn(init.body as object, "config"), "a phone channel carries no config").toBe(false);
    expect(w.emitted("created")?.[0]?.[0]).toMatchObject({ id: "nc-noc" });
    expect(w.emitted("cancel"), "a person channel closes the form once created").toBeTruthy();
  });

  it("warns that a person without a verified number is not a route (3a′), and still creates", async () => {
    await seed();
    const w = await mountForm({ type: "voice", name: "Call — Bo" });
    await tid(w, "phone-recipient-select").setValue("u-bo");

    expect(text(w, "phone-recipient-unverified")).toBe(
      "Bo Lindqvist has no verified number. The channel is created, but it is not a route — nothing is sent through it — until Bo Lindqvist confirms a number under Account → Phone.",
    );
    expect((tid(w, "phone-channel-submit").element as HTMLButtonElement).disabled, "Create is allowed").toBe(false);

    apiMock.POST.mockResolvedValue(ok(created({ type: "voice", recipient: "user", recipient_user_id: "u-bo" }), 201));
    await click(w, "phone-channel-submit");
    expect(lastCall(apiMock.POST)[1].body).toEqual({ type: "voice", name: "Call — Bo", recipient: "user", recipient_user_id: "u-bo" });
  });

  it("gives an editor only themselves and no fixed number (3c)", async () => {
    await seed({ user: { id: "u-bo", email: "bo@example.com", display_name: "Bo Lindqvist" }, admin: false });
    const w = await mountForm({ name: "SMS — Bo" }, [RECIPIENTS[1]]);

    expect(has(w, "phone-recipient-select"), "an editor has no picker").toBe(false);
    expect(text(w, "phone-recipient-self")).toBe("You (Bo Lindqvist) — no verified number");
    expect(text(w, "phone-recipient-hint")).toBe("As an editor you can create a phone channel only for yourself.");
    expect((tid(w, "phone-send-to-number").element as HTMLInputElement).disabled).toBe(true);
    expect(text(w, "phone-send-to-hint")).toBe("A fixed number can be added by a project admin.");

    apiMock.POST.mockResolvedValue(ok(created({ recipient: "user", recipient_user_id: "u-bo" }), 201));
    await click(w, "phone-channel-submit");
    expect(lastCall(apiMock.POST)[1].body).toEqual({ type: "sms", name: "SMS — Bo", recipient: "user", recipient_user_id: "u-bo" });
  });

  it("names the editor with the verified mark when they have a verified number", async () => {
    await seed({ user: { id: "u-chen", email: "chen@example.com", display_name: "Chen Wei" }, admin: false });
    const w = await mountForm({}, [RECIPIENTS[2]]);
    expect(text(w, "phone-recipient-self")).toBe("You (Chen Wei) — ✓ verified number");
    expect(has(w, "phone-recipient-unverified")).toBe(false);
  });

  it("shows the server's sentence when the recipients cannot be read", async () => {
    await seed();
    apiMock.GET.mockResolvedValue(fail(403, { error: "forbidden" }));
    const w = mount(PhoneChannelForm, { props: { orgId: "o1", projectId: "p1", type: "sms", name: "x", channel: null } });
    mounted.push(w);
    await flushPromises();
    expect(text(w, "phone-recipients-error")).toBe("forbidden");
    expect((tid(w, "phone-channel-submit").element as HTMLButtonElement).disabled, "no recipient, no Create").toBe(true);
  });
});

describe("PhoneChannelForm — a fixed number (3b-1, 3b-2, 3b-3)", () => {
  async function createNumberChannel(over: Partial<FakeSession> = {}): Promise<VueWrapper> {
    await seed(over);
    const w = await mountForm({ name: "NOC duty phone" });
    await tid(w, "phone-send-to-number").setValue(true);
    expect(text(w, "phone-number-hint")).toBe("E.164. Never shown again in full.");
    expect(text(w, "phone-number-explain")).toBe(
      "A fixed-number channel is created disabled. Confirm the number with a code to enable it. A shared phone receives the incident link only — it can never acknowledge, because it names nobody.",
    );
    await tid(w, "phone-number-input").setValue(TYPED);
    apiMock.POST.mockResolvedValueOnce(ok(created(), 201));
    await click(w, "phone-channel-submit");
    return w;
  }

  it("creates with the number and nothing else, then stays open on the new channel without sending a code", async () => {
    const w = await createNumberChannel();
    const [path, init] = lastCall(apiMock.POST);
    expect(path).toBe("/api/v1/projects/{projectID}/notification-channels");
    expect(init.body, "no config, no enabled, no recipient_user_id").toEqual({ type: "sms", name: "NOC duty phone", recipient: "number", number: TYPED });

    expect(w.emitted("created")?.[0]?.[0]).toMatchObject({ id: "nc-noc" });
    expect(w.emitted("cancel"), "the form stays open on the new channel").toBeFalsy();
    expect(text(w, "phone-verify-created")).toBe("Created NOC duty phone — disabled until the number is confirmed.");
    expect(text(w, "phone-verify-step-1")).toContain(`1 · Send a code to ${MASK}`);
    expect((tid(w, "phone-verify-code").element as HTMLInputElement).disabled, "no code was sent yet").toBe(true);
    expect((tid(w, "phone-verify-submit").element as HTMLButtonElement).disabled).toBe(true);
    expect(apiMock.POST, "Create sends no code (decision 11)").toHaveBeenCalledTimes(1);
    expect(w.html(), "the typed number is never rendered back").not.toContain("7700 900199");
    expect(has(w, "phone-number-input"), "the number field is gone").toBe(false);
  });

  it("sends the code only on Send code, then verifies; a confirmed number is not enabled by itself", async () => {
    const w = await createNumberChannel();
    apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { status: 204, ok: true } });
    await click(w, "phone-verify-send");
    const [sendPath, sendInit] = lastCall(apiMock.POST);
    expect(sendPath).toBe("/api/v1/notification-channels/{channelID}/verify/send");
    expect(sendInit.params?.path).toEqual({ channelID: "nc-noc" });
    expect(sendInit.body, "verify/send has no body").toBeUndefined();
    expect(text(w, "phone-verify-step-1")).toContain(`✓ 1 · Code sent to ${MASK}`);
    expect((tid(w, "phone-verify-code").element as HTMLInputElement).disabled).toBe(false);

    await tid(w, "phone-verify-code").setValue("123456");
    apiMock.POST.mockResolvedValueOnce(ok(created({ number_verified: true, disabled_reason: undefined })));
    await click(w, "phone-verify-submit");
    const [vPath, vInit] = lastCall(apiMock.POST);
    expect(vPath).toBe("/api/v1/notification-channels/{channelID}/verify");
    expect(vInit.params?.path).toEqual({ channelID: "nc-noc" });
    expect(vInit.body).toEqual({ code: "123456" });

    expect(text(w, "phone-verified-step-1")).toBe(`✓ 1 · Code sent to ${MASK}`);
    expect(text(w, "phone-verified-step-2")).toBe("✓ 2 · Number confirmed");
    expect(w.emitted("saved")?.[0]?.[0], "the row learns the number is confirmed").toMatchObject({ number_verified: true });
    expect(apiMock.PATCH, "verifying does not enable (decision 12)").not.toHaveBeenCalled();

    apiMock.PATCH.mockResolvedValueOnce(ok(created({ number_verified: true, disabled_reason: undefined, enabled: true })));
    await click(w, "phone-enable");
    const [pPath, pInit] = lastCall(apiMock.PATCH);
    expect(pPath).toBe("/api/v1/notification-channels/{channelID}");
    expect(pInit.params?.path).toEqual({ channelID: "nc-noc" });
    expect(pInit.body).toEqual({ enabled: true });
    expect(w.emitted("saved")?.[1]?.[0]).toMatchObject({ enabled: true });
    expect(w.emitted("cancel")).toBeTruthy();
  });

  it("“Keep disabled” closes without a PATCH", async () => {
    const w = await createNumberChannel();
    apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { status: 204, ok: true } });
    await click(w, "phone-verify-send");
    await tid(w, "phone-verify-code").setValue("123456");
    apiMock.POST.mockResolvedValueOnce(ok(created({ number_verified: true })));
    await click(w, "phone-verify-submit");
    await click(w, "phone-keep-disabled");
    expect(apiMock.PATCH).not.toHaveBeenCalled();
    expect(w.emitted("cancel")).toBeTruthy();
  });

  it("“Later” closes and leaves the channel disabled", async () => {
    const w = await createNumberChannel();
    await click(w, "phone-verify-later");
    expect(apiMock.PATCH).not.toHaveBeenCalled();
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    expect(w.emitted("cancel")).toBeTruthy();
  });

  it("reads the code states of §2 (2c, 2d, 2e) and lets a new code revive Verify", async () => {
    const w = await createNumberChannel();
    apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { status: 204, ok: true } });
    await click(w, "phone-verify-send");

    await tid(w, "phone-verify-code").setValue("000000");
    apiMock.POST.mockResolvedValueOnce(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
    await click(w, "phone-verify-submit");
    expect(text(w, "phone-verify-code-error")).toBe("That code is not right. Check the most recent SMS and try again.");
    expect((tid(w, "phone-verify-submit").element as HTMLButtonElement).disabled, "a wrong code leaves Verify usable").toBe(false);

    apiMock.POST.mockResolvedValueOnce(fail(410, { error: "the verification code has expired", reason: "code_expired" }));
    await click(w, "phone-verify-submit");
    expect(text(w, "phone-verify-code-error")).toBe("This code has expired — codes work for 10 minutes. Send a new one.");
    expect((tid(w, "phone-verify-submit").element as HTMLButtonElement).disabled).toBe(true);

    apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { status: 204, ok: true } });
    await click(w, "phone-verify-send");
    await tid(w, "phone-verify-code").setValue("111111");
    apiMock.POST.mockResolvedValueOnce(fail(410, { error: "too many", reason: "code_attempts_exhausted" }));
    await click(w, "phone-verify-submit");
    expect(text(w, "phone-verify-code-error")).toBe("Too many wrong codes — this code no longer works. Send a new one.");
  });

  it("reads the server's sentence when a code send is refused (send_limited, verified 409, telephony_disabled)", async () => {
    const w = await createNumberChannel();
    apiMock.POST.mockResolvedValueOnce(fail(429, { error: "too many verification codes were requested; try again later", reason: "send_limited" }));
    await click(w, "phone-verify-send");
    expect(text(w, "phone-verify-send-error")).toBe("too many verification codes were requested; try again later");

    apiMock.POST.mockResolvedValueOnce(fail(429, { reason: "send_limited" }));
    await click(w, "phone-verify-send");
    expect(text(w, "phone-verify-send-error"), "no sentence: the mock's §2 send-limit line").toBe(
      "No code sent: too many codes were requested. You can get 3 codes an hour, and a number gets at most 10 a day. Try again later.",
    );

    apiMock.POST.mockResolvedValueOnce(fail(409, { error: "this phone number is already verified", reason: "number_already_verified" }));
    await click(w, "phone-verify-send");
    expect(text(w, "phone-verify-send-error")).toBe("this phone number is already verified");

    apiMock.POST.mockResolvedValueOnce(fail(409, { error: "phone alerts are turned off on this instance", reason: "telephony_disabled" }));
    await click(w, "phone-verify-send");
    expect(text(w, "phone-verify-send-error")).toBe("phone alerts are turned off on this instance");
    expect((sessionState.value as unknown as FakeSession).fetchMe).toHaveBeenCalled();
  });

  it("reads the server's sentence when enabling is refused", async () => {
    const w = await createNumberChannel();
    apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { status: 204, ok: true } });
    await click(w, "phone-verify-send");
    await tid(w, "phone-verify-code").setValue("123456");
    apiMock.POST.mockResolvedValueOnce(ok(created({ number_verified: true })));
    await click(w, "phone-verify-submit");
    apiMock.PATCH.mockResolvedValueOnce(fail(409, { error: "verify the channel's number before enabling it", reason: "number_unverified" }));
    await click(w, "phone-enable");
    expect(text(w, "phone-enable-error")).toBe("verify the channel's number before enabling it");
    expect(w.emitted("cancel"), "a refused enable keeps the form open").toBeFalsy();
  });

  // Revision 9: beside Send code, "Call with the code" — the same verify/send with `via: "voice"`,
  // offered only while /me says `telephony_can_call`. No fallback between the ways; a refused call
  // reads the server's sentence; after a call the lines never say SMS.
  describe("Call with the code (revision 9)", () => {
    const NO_BODY = { data: undefined, error: undefined, response: { status: 204, ok: true } };

    it("is offered beside Send code only while /me says telephony_can_call — on Create's steps and on the row's Verify", async () => {
      let w = await createNumberChannel({ telephonyCanCall: false });
      expect(has(w, "phone-verify-send")).toBe(true);
      expect(has(w, "phone-verify-call"), "no call while the instance cannot place one").toBe(false);

      w = await createNumberChannel({ telephonyCanCall: true });
      expect(text(w, "phone-verify-call")).toBe("Call with the code");
      expect(apiMock.POST.mock.calls.filter(([p]) => String(p).endsWith("/verify/send")), "nothing is sent on Create").toHaveLength(0);

      for (const canCall of [false, true]) {
        await seed({ telephonyCanCall: canCall });
        const v = await mountForm({ name: "NOC duty phone", channel: created(), verify: true });
        expect(has(v, "phone-verify-call"), `row's Verify, can call: ${canCall}`).toBe(canCall);
      }
    });

    it("POSTs verify/send with {via: \"voice\"}; the steps and the code lines then say call, never SMS", async () => {
      const w = await createNumberChannel({ telephonyCanCall: true });
      apiMock.POST.mockResolvedValueOnce(NO_BODY);
      await click(w, "phone-verify-call");
      const [path, init] = lastCall(apiMock.POST);
      expect(path).toBe("/api/v1/notification-channels/{channelID}/verify/send");
      expect(init.params?.path).toEqual({ channelID: "nc-noc" });
      expect(init.body).toEqual({ via: "voice" });
      expect(text(w, "phone-verify-step-1")).toContain(`✓ 1 · Code spoken by a call to ${MASK}`);
      expect(text(w, "phone-verify-call")).toBe("Call with a new code");

      await tid(w, "phone-verify-code").setValue("000000");
      apiMock.POST.mockResolvedValueOnce(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
      await click(w, "phone-verify-submit");
      expect(text(w, "phone-verify-code-error")).toBe("That code is not right. Check the code from the most recent call and try again.");
      expect(norm(w.text())).not.toMatch(/SMS/);

      apiMock.POST.mockResolvedValueOnce(ok(created({ number_verified: true, disabled_reason: undefined })));
      await click(w, "phone-verify-submit");
      expect(text(w, "phone-verified-step-1")).toBe(`✓ 1 · Code spoken by a call to ${MASK}`);
      expect(norm(w.text())).not.toMatch(/SMS/);
    });

    it("Send code after a call goes back to the body-less send and the SMS line — each send its own way", async () => {
      const w = await createNumberChannel({ telephonyCanCall: true });
      apiMock.POST.mockResolvedValueOnce(NO_BODY);
      await click(w, "phone-verify-call");
      apiMock.POST.mockResolvedValueOnce(NO_BODY);
      await click(w, "phone-verify-send");
      expect(lastCall(apiMock.POST)[1].body, "an SMS send has no body").toBeUndefined();
      expect(text(w, "phone-verify-step-1")).toContain(`✓ 1 · Code sent to ${MASK}`);
      await tid(w, "phone-verify-code").setValue("000000");
      apiMock.POST.mockResolvedValueOnce(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
      await click(w, "phone-verify-submit");
      expect(text(w, "phone-verify-code-error")).toBe("That code is not right. Check the most recent SMS and try again.");
    });

    it("a refused call reads the server's sentence under the action and sends no SMS instead (voice_unsupported_by_provider, via_invalid)", async () => {
      for (const [reason, error] of [
        ["voice_unsupported_by_provider", "the telephony provider in force sends SMS only; a code cannot be spoken by a call"],
        ["via_invalid", "via must be sms or voice"],
      ]) {
        const w = await createNumberChannel({ telephonyCanCall: true });
        apiMock.POST.mockClear();
        apiMock.POST.mockResolvedValueOnce(fail(400, { error, reason }));
        await click(w, "phone-verify-call");
        expect(text(w, "phone-verify-send-error"), reason).toBe(error);
        expect(apiMock.POST, `${reason}: one request, no fallback`).toHaveBeenCalledTimes(1);
        expect((tid(w, "phone-verify-code").element as HTMLInputElement).disabled, "no code was sent").toBe(true);
        if (reason === "voice_unsupported_by_provider") {
          expect((sessionState.value as unknown as FakeSession).fetchMe, "/me said a call could be made: re-read it").toHaveBeenCalled();
        }
      }
    });

    // Review I-1: the backend stores the new code (ending the previous one) before it calls the
    // provider, so after a 502 nobody received the running code. The form goes back to the mock's
    // own "1 · Send a code to <mask>", with the server's sentence under the action.
    it("I-1 — a provider error never claims a code: a failed first send, and a call that failed after an SMS that went", async () => {
      const PROVIDER = "the telephony provider could not be reached; try again";
      const w = await createNumberChannel({ telephonyCanCall: true });
      apiMock.POST.mockResolvedValueOnce(fail(502, { error: PROVIDER, reason: "provider_unreachable" }));
      await click(w, "phone-verify-call");
      expect(text(w, "phone-verify-step-1")).toContain(`1 · Send a code to ${MASK}`);
      expect(text(w, "phone-verify-send-error")).toBe(PROVIDER);
      expect((tid(w, "phone-verify-code").element as HTMLInputElement).disabled).toBe(true);

      apiMock.POST.mockResolvedValueOnce(NO_BODY);
      await click(w, "phone-verify-send");
      expect(text(w, "phone-verify-step-1")).toContain(`✓ 1 · Code sent to ${MASK}`);
      await tid(w, "phone-verify-code").setValue("000000");
      apiMock.POST.mockResolvedValueOnce(fail(400, { error: "the verification code is incorrect", reason: "code_incorrect" }));
      await click(w, "phone-verify-submit");
      expect(text(w, "phone-verify-code-error")).toContain("most recent SMS");

      apiMock.POST.mockResolvedValueOnce(fail(502, { error: "the telephony provider refused to send the code", reason: "provider_rejected" }));
      await click(w, "phone-verify-call");
      expect(text(w, "phone-verify-step-1")).not.toMatch(/✓|Code sent|spoken/);
      expect(text(w, "phone-verify-step-1")).toContain(`1 · Send a code to ${MASK}`);
      expect(text(w, "phone-verify-send")).toBe("Send code");
      expect(text(w, "phone-verify-call")).toBe("Call with the code");
      expect(text(w, "phone-verify-send-error")).toBe("the telephony provider refused to send the code");
      expect(has(w, "phone-verify-code-error"), "no pointer to the dead SMS").toBe(false);
      expect(norm(w.text())).not.toMatch(/SMS/);
      expect((tid(w, "phone-verify-code").element as HTMLInputElement).disabled).toBe(true);
      expect((tid(w, "phone-verify-code").element as HTMLInputElement).value).toBe("");
      expect((tid(w, "phone-verify-submit").element as HTMLButtonElement).disabled).toBe(true);
    });

    it("m-3 — a refused call re-reads /me; when it says no call, the action goes and the sentence stays; via_invalid re-reads nothing", async () => {
      const w = await createNumberChannel({ telephonyCanCall: true });
      const s = sessionState.value as unknown as FakeSession;
      apiMock.POST.mockResolvedValueOnce(fail(400, { error: "via must be sms or voice", reason: "via_invalid" }));
      await click(w, "phone-verify-call");
      expect(s.fetchMe, "via_invalid: nothing to re-read").not.toHaveBeenCalled();

      s.fetchMe.mockImplementation(async () => {
        (s as { telephonyCanCall: boolean }).telephonyCanCall = false;
        return true;
      });
      apiMock.POST.mockResolvedValueOnce(fail(400, { error: "no calls here", reason: "voice_unsupported_by_provider" }));
      await click(w, "phone-verify-call");
      expect(s.fetchMe).toHaveBeenCalledTimes(1);
      expect(has(w, "phone-verify-call")).toBe(false);
      expect(has(w, "phone-verify-send")).toBe(true);
      expect(text(w, "phone-verify-send-error")).toBe("no calls here");
    });

    it("m-3/m-4 — while a call is in flight: “Calling…”, Send code, Verify and Later are disabled, and Verify does not say “Verifying…”", async () => {
      const w = await createNumberChannel({ telephonyCanCall: true });
      apiMock.POST.mockResolvedValueOnce(NO_BODY);
      await click(w, "phone-verify-send");
      await tid(w, "phone-verify-code").setValue("123456");
      let release!: (v: unknown) => void;
      apiMock.POST.mockReturnValueOnce(new Promise((r) => (release = r)));
      await click(w, "phone-verify-call");
      expect(text(w, "phone-verify-call")).toBe("Calling…");
      expect((tid(w, "phone-verify-send").element as HTMLButtonElement).disabled).toBe(true);
      expect((tid(w, "phone-verify-submit").element as HTMLButtonElement).disabled).toBe(true);
      expect(text(w, "phone-verify-submit"), "a send is not a verify").toBe("Verify");
      expect((tid(w, "phone-verify-later").element as HTMLButtonElement).disabled).toBe(true);
      release(NO_BODY);
      await flushPromises();
      expect(text(w, "phone-verify-call")).toBe("Call with a new code");
    });

    it("a refused call with no sentence falls back to “No code sent.”", async () => {
      const w = await createNumberChannel({ telephonyCanCall: true });
      apiMock.POST.mockResolvedValueOnce(fail(400, { reason: "voice_unsupported_by_provider" }));
      await click(w, "phone-verify-call");
      expect(text(w, "phone-verify-send-error")).toBe("No code sent.");
    });
  });
});

describe("PhoneChannelForm — the row's Verify on an existing number channel (3e → 3b-2)", () => {
  it("starts at 3b-2 on that channel: no recipient read, no Create, nothing sent until Send code", async () => {
    await seed();
    const w = await mountForm({ name: "NOC duty phone", channel: created(), verify: true });
    expect(apiMock.GET, "a number channel reads no recipients").not.toHaveBeenCalled();
    expect(has(w, "phone-channel-submit")).toBe(false);
    expect(has(w, "phone-number-input")).toBe(false);
    expect(text(w, "phone-verify-created")).toBe("NOC duty phone — disabled until the number is confirmed.");
    expect(text(w, "phone-verify-step-1")).toContain(`1 · Send a code to ${MASK}`);
    expect((tid(w, "phone-verify-submit").element as HTMLButtonElement).disabled).toBe(true);
    expect(apiMock.POST).not.toHaveBeenCalled();

    apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { status: 204, ok: true } });
    await click(w, "phone-verify-send");
    expect(lastCall(apiMock.POST)[1].params?.path).toEqual({ channelID: "nc-noc" });
    await tid(w, "phone-verify-code").setValue("123456");
    apiMock.POST.mockResolvedValueOnce(ok(created({ number_verified: true, disabled_reason: undefined })));
    await click(w, "phone-verify-submit");
    expect(text(w, "phone-verified-step-2")).toBe("✓ 2 · Number confirmed");
    expect(w.emitted("saved")?.[0]?.[0]).toMatchObject({ number_verified: true });
  });

  it("is ignored for a person channel, which has no number to confirm", async () => {
    await seed();
    const person = created({ recipient: "user", recipient_user_id: "u-ada", number_masked: undefined, disabled_reason: undefined });
    const w = await mountForm({ channel: person, verify: true });
    expect(has(w, "phone-verify-step-1")).toBe(false);
    expect(has(w, "phone-channel-submit")).toBe(true);
  });
});

describe("PhoneChannelForm — refusals of Create (3f, 3f′, ruling 9)", () => {
  it("telephony switched off meanwhile: the server's sentence, and /me is re-read", async () => {
    const s = await seed();
    const w = await mountForm();
    apiMock.POST.mockResolvedValue(fail(409, { error: "phone alerts are turned off on this instance", reason: "telephony_disabled" }));
    await click(w, "phone-channel-submit");
    expect(text(w, "phone-channel-error")).toBe("phone alerts are turned off on this instance");
    expect(s.fetchMe).toHaveBeenCalled();
    expect(w.emitted("created")).toBeFalsy();
  });

  it("telephony switched off with no sentence from the server falls back to the mock's 3f line", async () => {
    await seed();
    const w = await mountForm();
    apiMock.POST.mockResolvedValue(fail(409, { reason: "telephony_disabled" }));
    await click(w, "phone-channel-submit");
    expect(text(w, "phone-channel-error")).toBe("Not created: phone alerts are not enabled on this instance.");
  });

  it("a voice channel the provider cannot place: the server's sentence (generic HTTP, or Twilio with a Messaging Service)", async () => {
    await seed();
    const w = await mountForm({ type: "voice", name: "Call — Ada" });
    apiMock.POST.mockResolvedValue(
      fail(400, {
        error: "the telephony provider in force sends SMS only; a voice channel cannot be created or enabled",
        reason: "voice_unsupported_by_provider",
      }),
    );
    await click(w, "phone-channel-submit");
    expect(text(w, "phone-channel-error")).toBe("the telephony provider in force sends SMS only; a voice channel cannot be created or enabled");
  });

  it("a voice refusal with no sentence falls back to the mock's 3f′ line", async () => {
    await seed();
    const w = await mountForm({ type: "voice" });
    apiMock.POST.mockResolvedValue(fail(400, { reason: "voice_unsupported_by_provider" }));
    await click(w, "phone-channel-submit");
    expect(text(w, "phone-channel-error")).toBe(
      "Not created: this instance sends SMS through a generic HTTP gateway, which cannot place calls. Create an SMS channel instead.",
    );
  });

  it.each([
    ["number_invalid", 400, "not a valid phone number"],
    ["recipient_not_eligible", 400, "the recipient must be a person with access to the channel's project"],
    ["recipient_fixed", 400, "a phone channel's recipient mode is fixed at creation; create another channel"],
    ["encryption_key_missing", 409, "phone numbers need security.encryption_key to be configured"],
  ])("%s on Create reads the server's sentence under Create", async (reason, status, sentence) => {
    await seed();
    const w = await mountForm({ name: "NOC duty phone" });
    await tid(w, "phone-send-to-number").setValue(true);
    await tid(w, "phone-number-input").setValue(TYPED);
    apiMock.POST.mockResolvedValue(fail(status, { error: sentence, reason }));
    await click(w, "phone-channel-submit");
    expect(text(w, "phone-channel-error")).toBe(sentence);
    expect((tid(w, "phone-number-input").element as HTMLInputElement).value, "the form keeps the typed number for a retry").toBe(TYPED);
    expect(w.emitted("created")).toBeFalsy();
  });
});

describe("PhoneChannelForm — editing (3d)", () => {
  const numberChannel = created({ number_verified: true, disabled_reason: undefined, enabled: true });
  const personChannel = {
    id: "nc-ada",
    project_id: "p1",
    type: "sms",
    name: "SMS — Ada",
    config: {},
    enabled: true,
    recipient: "user",
    recipient_user_id: "u-ada",
    recipient_display_name: "Ada Park",
    deliverable: { route: true },
  };

  it("a number channel: the mask stands in the field; blank sends only the name, a typed number replaces it", async () => {
    await seed();
    const w = await mountForm({ channel: numberChannel, name: "NOC duty phone" });
    expect(apiMock.GET, "a number channel needs no recipient list").not.toHaveBeenCalled();
    expect(tid(w, "phone-number-input").attributes("placeholder")).toBe(`${MASK} — type a new number to replace it`);
    expect(text(w, "phone-number-hint")).toBe("A new number clears the confirmation and disables the channel until it is confirmed again.");
    expect(has(w, "phone-send-to-user"), "who it sends to cannot be changed").toBe(false);
    expect(text(w, "phone-channel-submit")).toBe("Save");

    apiMock.PATCH.mockResolvedValue(ok(numberChannel));
    await click(w, "phone-channel-submit");
    let [path, init] = lastCall(apiMock.PATCH);
    expect(path).toBe("/api/v1/notification-channels/{channelID}");
    expect(init.params?.path).toEqual({ channelID: "nc-noc" });
    expect(init.body).toEqual({ name: "NOC duty phone" });
    expect(w.emitted("saved")?.[0]?.[0]).toMatchObject({ id: "nc-noc" });
    expect(w.emitted("cancel")).toBeTruthy();

    const w2 = await mountForm({ channel: numberChannel, name: "NOC duty phone" });
    await tid(w2, "phone-number-input").setValue(TYPED);
    await click(w2, "phone-channel-submit");
    [path, init] = lastCall(apiMock.PATCH);
    expect(init.body).toEqual({ name: "NOC duty phone", number: TYPED });
  });

  it("a person channel, project admin: the select starts on the recipient; a rename sends no recipient, a new pick does", async () => {
    await seed();
    const w = await mountForm({ channel: personChannel });
    expect((tid(w, "phone-recipient-select").element as HTMLSelectElement).value).toBe("u-ada");
    apiMock.PATCH.mockResolvedValue(ok(personChannel));
    await click(w, "phone-channel-submit");
    expect(lastCall(apiMock.PATCH)[1].body).toEqual({ name: "SMS — Ada" });

    const w2 = await mountForm({ channel: personChannel });
    await tid(w2, "phone-recipient-select").setValue("u-chen");
    await click(w2, "phone-channel-submit");
    expect(lastCall(apiMock.PATCH)[1].body).toEqual({ name: "SMS — Ada", recipient_user_id: "u-chen" });
  });

  it("a person channel whose recipient was deleted: no current pick, the admin may pick another", async () => {
    await seed();
    const orphan = { ...personChannel, recipient_user_id: undefined, recipient_display_name: null, enabled: false, disabled_reason: "recipient_deleted" };
    const w = await mountForm({ channel: orphan });
    const sel = tid(w, "phone-recipient-select");
    expect((sel.element as HTMLSelectElement).value).toBe("");
    expect(optionTexts(w, "phone-recipient-select")[0]).toBe("A deleted account");
    apiMock.PATCH.mockResolvedValue(ok(personChannel));
    await click(w, "phone-channel-submit");
    expect(lastCall(apiMock.PATCH)[1].body, "a rename leaves the recipient alone").toEqual({ name: "SMS — Ada" });
    await sel.setValue("u-ada");
    await click(w, "phone-channel-submit");
    expect(lastCall(apiMock.PATCH)[1].body).toEqual({ name: "SMS — Ada", recipient_user_id: "u-ada" });
  });

  it("a person channel, editor on their own channel: only themselves, no retargeting", async () => {
    await seed({ user: { id: "u-bo", email: "bo@example.com", display_name: "Bo Lindqvist" }, admin: false });
    const own = { ...personChannel, id: "nc-bo", name: "Call — Bo", recipient_user_id: "u-bo", recipient_display_name: "Bo Lindqvist" };
    const w = await mountForm({ channel: own, name: "Call — Bo" }, [RECIPIENTS[1]]);
    expect(has(w, "phone-recipient-select")).toBe(false);
    expect(text(w, "phone-recipient-self")).toBe("You (Bo Lindqvist) — no verified number");
    apiMock.PATCH.mockResolvedValue(ok(own));
    await click(w, "phone-channel-submit");
    expect(lastCall(apiMock.PATCH)[1].body).toEqual({ name: "Call — Bo" });
  });

  it("a recipient who lost access and has no name stays the current pick, not “a deleted account”", async () => {
    await seed();
    const lost = { ...personChannel, recipient_user_id: "u-gone", recipient_display_name: "" };
    const w = await mountForm({ channel: lost });
    expect((tid(w, "phone-recipient-select").element as HTMLSelectElement).value).toBe("u-gone");
    expect(optionTexts(w, "phone-recipient-select")[0]).toBe("The current recipient");
    apiMock.PATCH.mockResolvedValue(ok(lost));
    await click(w, "phone-channel-submit");
    expect(lastCall(apiMock.PATCH)[1].body, "an unchanged recipient is not re-judged").toEqual({ name: "SMS — Ada" });
  });

  // D28: an editor holds only a person channel that phones themselves, judged against the channel as
  // stored (handlers_phonechannels.go mayHold). The view hides Edit for the rest; the form, if reached,
  // offers no Save the server would refuse with 403 (review I-1).
  it.each([
    ["someone else's person channel", { ...personChannel, recipient_user_id: "u-ada", recipient_display_name: "Ada Park" }],
    ["a number channel", numberChannel],
  ])("an editor on %s gets no Save", async (_l, ch) => {
    await seed({ user: { id: "u-bo", email: "bo@example.com", display_name: "Bo Lindqvist" }, admin: false });
    const w = await mountForm({ channel: ch, name: String(ch.name) }, [RECIPIENTS[1]]);
    expect(has(w, "phone-channel-submit"), "no Save is offered").toBe(false);
    expect(text(w, "phone-edit-forbidden")).toBe("As an editor you can edit only your own phone channels.");
    if (has(w, "phone-number-input")) expect((tid(w, "phone-number-input").element as HTMLInputElement).disabled).toBe(true);
    expect(apiMock.PATCH).not.toHaveBeenCalled();
  });

  it("a refused edit reads the server's sentence and keeps the form", async () => {
    await seed();
    const w = await mountForm({ channel: personChannel });
    await tid(w, "phone-recipient-select").setValue("u-bo");
    apiMock.PATCH.mockResolvedValue(fail(400, { error: "the recipient must be a person with access to the channel's project", reason: "recipient_not_eligible" }));
    await click(w, "phone-channel-submit");
    expect(text(w, "phone-channel-error")).toBe("the recipient must be a person with access to the channel's project");
    expect(w.emitted("cancel")).toBeFalsy();
  });
});
