import { type DOMWrapper, type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import SettingsView from "@/views/SettingsView.vue";

// FR-041a §4.3, D28, D31, mock 3e–3g: Settings → Notification channels as a list of phone channels.
// What each role is offered on a row is what the server lets it do — "update and delete follow the
// same table" (D28) — so no visible control leads to a 403. The route state reads from the channel's
// own `deliverable` and `disabled_reason`; the only-route warning from `only_route_for` (§14 point 5).

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: { tab: "channels" } }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
const sessionFlags = vi.hoisted(() => ({
  telephonyEnabled: true,
  projectAdmin: true,
  projectWrite: true,
  userId: "u-admin",
  fetchMe: null as null | ReturnType<typeof vi.fn>,
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({
    canProjectWrite: () => sessionFlags.projectWrite,
    isOrgAdmin: () => sessionFlags.projectAdmin,
    canProjectAdmin: () => sessionFlags.projectAdmin,
    isGlobalAdmin: false,
    totpEnabled: false,
    memberships: [],
    get user() {
      return { id: sessionFlags.userId, email: `${sessionFlags.userId}@example.com`, display_name: sessionFlags.userId };
    },
    get telephonyEnabled() {
      return sessionFlags.telephonyEnabled;
    },
    fetchMe: (...a: unknown[]) => sessionFlags.fetchMe!(...a),
  }),
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({
    init: () => Promise.resolve(),
    orgId: "o1",
    projectId: "p1",
    orgName: "Acme",
    projectName: "prod",
    projects: [{ id: "p1", name: "prod", slug: "prod" }],
  }),
}));
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));

// ── The rows of mock 3e (project prod, Twilio, phone alerts on) ──
const person = (id: string, name: string, userId: string, displayName: string | null, over: Record<string, unknown> = {}) => ({
  id,
  project_id: "p1",
  type: "sms",
  name,
  config: {},
  enabled: true,
  recipient: "user",
  recipient_user_id: userId,
  recipient_display_name: displayName,
  deliverable: { route: true },
  only_route_for: [],
  only_route_for_total: 0,
  ...over,
});
const SLACK = { id: "nc-slack", project_id: "p1", type: "slack", name: "oncall-slack", config: { webhook_url: "" }, enabled: true };
const MAIL = { id: "nc-mail", project_id: "p1", type: "email", name: "ops-mail", config: { to: "ops@example.com" }, enabled: false };
const SMS_ADA = person("nc-sms-ada", "SMS — Ada", "u-ada", "Ada Park");
const CALL_ADA = person("nc-call-ada", "Call — Ada", "u-ada", "Ada Park", { type: "voice" });
const CALL_BO = person("nc-call-bo", "Call — Bo", "u-bo", "Bo Lindqvist", {
  type: "voice",
  deliverable: { route: false, reason: "no_verified_number" },
});
const SMS_ELI = person("nc-sms-eli", "SMS — Eli", "u-eli", "Eli Moreau", { deliverable: { route: false, reason: "recipient_not_eligible" } });
const NOC = {
  id: "nc-noc",
  project_id: "p1",
  type: "sms",
  name: "NOC duty phone",
  config: {},
  enabled: false,
  recipient: "number",
  number_masked: "+44•••••0199",
  number_verified: false,
  disabled_reason: "number_unverified",
  deliverable: { route: false, reason: "disabled" },
  only_route_for: [],
  only_route_for_total: 0,
};
const NOC_VERIFIED = { ...NOC, id: "nc-noc-ok", name: "NOC verified", enabled: true, number_verified: true, disabled_reason: undefined, deliverable: { route: true } };
const SMS_FAY = person("nc-sms-fay", "SMS — Fay", "", null, {
  recipient_user_id: undefined,
  enabled: false,
  disabled_reason: "recipient_deleted",
  deliverable: { route: false, reason: "disabled" },
});
const MOCK_3E = [SLACK, SMS_ADA, CALL_ADA, CALL_BO, SMS_ELI, NOC, SMS_FAY, MAIL];

const handlerErrors: unknown[] = [];

async function mountList(list: unknown[], deliveries: unknown[] = []) {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  handlerErrors.length = 0;
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/notification-channels/{channelID}/phone-deliveries") {
      return { data: { deliveries }, response: { ok: true, status: 200 } };
    }
    if (path === "/api/v1/projects/{projectID}/phone-recipients") return { data: [], response: { ok: true, status: 200 } };
    return { data: list, response: { ok: true, status: 200 } };
  });
  const w = mount(SettingsView, {
    global: {
      stubs: { RouterLink: { template: "<a><slot /></a>" } },
      config: { errorHandler: (err: unknown) => void handlerErrors.push(err) },
    },
  });
  await flushPromises();
  expect(w.text(), "the channels tab is the one that rendered").toContain("channel(s)");
  return w;
}

const norm = (s: string) => s.replace(/\s+/g, " ").trim();
function row(w: VueWrapper, name: string): DOMWrapper<Element> {
  const r = w.findAll('[data-testid="channel-row"]').find((x) => x.attributes("data-name") === name);
  expect(r, `a row named ${name}`).toBeTruthy();
  return r!;
}
/** A row's controls: the switch by its label, every other button by its text. */
const controls = (w: VueWrapper, name: string) =>
  row(w, name)
    .findAll("button")
    .filter((b) => !(b.element as HTMLButtonElement).disabled)
    .map((b) => b.attributes("aria-label") ?? norm(b.text()));
/** The sentence in the table row right under the channel's row, or null (review M-2: under THAT row). */
function errorUnder(w: VueWrapper, name: string): string | null {
  const trs = w.findAll("tbody tr");
  const at = trs.findIndex((t) => t.attributes("data-name") === name);
  expect(at, `a row named ${name}`).toBeGreaterThanOrEqual(0);
  const next = trs[at + 1]?.find('[data-testid="channel-row-error"]');
  return next?.exists() ? norm(next.text()) : null;
}
const hasWorkingSwitch = (w: VueWrapper, name: string) =>
  row(w, name)
    .findAll("button")
    .some((b) => /^(Disable|Enable) channel$/.test(b.attributes("aria-label") ?? "") && !(b.element as HTMLButtonElement).disabled);

beforeEach(() => {
  vi.clearAllMocks();
  sessionFlags.telephonyEnabled = true;
  sessionFlags.projectAdmin = true;
  sessionFlags.projectWrite = true;
  sessionFlags.userId = "u-admin";
  sessionFlags.fetchMe = vi.fn(async () => true);
});

describe("3e — project admin", () => {
  it("names each phone channel's recipient under its name", async () => {
    const w = await mountList(MOCK_3E);
    const to = (name: string) => norm(row(w, name).get('[data-testid="channel-recipient"]').text());
    expect(to("SMS — Ada")).toBe("to Ada Park");
    expect(to("Call — Bo")).toBe("to Bo Lindqvist");
    expect(to("NOC duty phone")).toBe("to +44•••••0199");
    expect(row(w, "NOC duty phone").get('[data-testid="channel-recipient"] .font-mono').text()).toBe("+44•••••0199");
    expect(to("SMS — Fay")).toBe("to a deleted account");
    expect(row(w, "oncall-slack").find('[data-testid="channel-recipient"]').exists()).toBe(false);
  });

  it("shows the route chips of 3e: solid for a disabled reason, dashed for not a route, none for a route", async () => {
    const w = await mountList(MOCK_3E);
    const chip = (name: string) => row(w, name).find('[data-testid="channel-route-chip"]');
    expect(chip("SMS — Ada").exists()).toBe(false);
    expect(chip("Call — Ada").exists()).toBe(false);
    expect(norm(chip("Call — Bo").text())).toBe("Not a route · Bo Lindqvist has no verified number");
    expect(norm(chip("SMS — Eli").text())).toBe("Not a route · Eli Moreau can no longer open this project");
    expect(norm(chip("NOC duty phone").text())).toBe("Disabled · number not confirmed");
    expect(chip("NOC duty phone").attributes("data-kind")).toBe("disabled");
    expect(norm(chip("SMS — Fay").text())).toBe("Disabled · the recipient's account was deleted");
    expect(chip("ops-mail").exists()).toBe(false);
  });

  it("offers Verify only on the unverified number row, Deliveries on every phone row, Edit and Delete everywhere", async () => {
    const w = await mountList(MOCK_3E);
    const actions = (name: string) =>
      row(w, name)
        .findAll('[data-testid="channel-actions"] button')
        .map((b) => norm(b.text()));
    expect(actions("oncall-slack")).toEqual(["Edit", "Delete"]);
    expect(actions("SMS — Ada")).toEqual(["Deliveries", "Edit", "Delete"]);
    expect(actions("Call — Bo")).toEqual(["Deliveries", "Edit", "Delete"]);
    expect(actions("NOC duty phone")).toEqual(["Verify", "Deliveries", "Edit", "Delete"]);
    expect(actions("SMS — Fay")).toEqual(["Deliveries", "Edit", "Delete"]);
    expect(actions("ops-mail")).toEqual(["Edit", "Delete"]);
    expect(w.findAll("button").filter((b) => norm(b.text()) === "Verify")).toHaveLength(1);
  });

  it("shows no Verify on a verified number channel", async () => {
    const w = await mountList([NOC_VERIFIED]);
    expect(w.findAll("button").some((b) => norm(b.text()) === "Verify")).toBe(false);
  });

  it("locks the switch while the channel carries a disabled reason, with the tooltip that says what unlocks it", async () => {
    const w = await mountList(MOCK_3E);
    for (const [name, title] of [
      ["NOC duty phone", "Confirm the number to enable this channel"],
      ["SMS — Fay", "The recipient was deleted — pick another person or delete the channel"],
    ] as const) {
      const lock = row(w, name).get('[data-testid="channel-toggle-locked"]');
      expect((lock.element as HTMLButtonElement).disabled, `${name}: the switch is locked`).toBe(true);
      expect(lock.attributes("title")).toBe(title);
      expect(hasWorkingSwitch(w, name)).toBe(false);
      await lock.trigger("click");
    }
    expect(apiMock.PATCH).not.toHaveBeenCalled();
  });

  it("keeps the channel's own switch when it is enabled but not a route (3e′)", async () => {
    const off = (c: Record<string, unknown>) => ({ ...c, deliverable: { route: false, reason: "telephony_disabled" } });
    const w = await mountList([off(SMS_ADA), off(CALL_ADA)]);
    expect(hasWorkingSwitch(w, "SMS — Ada")).toBe(true);
    expect(hasWorkingSwitch(w, "Call — Ada")).toBe(true);
    expect(norm(row(w, "Call — Ada").get('[data-testid="channel-route-chip"]').text())).toBe(
      "Not a route · phone alerts are off on this instance",
    );
  });

  it("puts the only-route warning under the row, from the channel's own fields (3e″)", async () => {
    const onCall = person("nc-oncall", "SMS — on-call", "u-chen", "Chen Wei", {
      only_route_for: [
        { project: "prod", service: "Checkout", slug: "checkout" },
        { project: "prod", service: "Payments", slug: "payments" },
      ],
      only_route_for_total: 2,
    });
    const w = await mountList([onCall, SMS_ADA]);
    const warn = row(w, "SMS — on-call").get('[data-testid="phone-only-route"]');
    expect(warn.findAll('[data-testid="phone-only-route-service"]').map((s) => norm(s.text()))).toEqual([
      "service checkout",
      "service payments",
    ]);
    expect(row(w, "SMS — Ada").find('[data-testid="phone-only-route"]').exists()).toBe(false);
  });
});

describe("3e‴ / 3e⁗ — editor and viewer (D28)", () => {
  // The Task 5 acceptance addition (Task 4 review I-1): the server refuses each of these with 403.
  it("D28: an editor sees no Edit, no Delete and no working toggle on another person's phone channel or on any number channel", async () => {
    sessionFlags.projectAdmin = false;
    sessionFlags.userId = "u-bo";
    const w = await mountList([SLACK, SMS_ADA, CALL_BO, NOC, NOC_VERIFIED, SMS_FAY]);

    expect(controls(w, "oncall-slack"), "a text channel").toEqual(["Disable channel", "Edit", "Delete"]);
    expect(controls(w, "Call — Bo"), "the editor's own phone channel").toEqual(["Disable channel", "Deliveries", "Edit", "Delete"]);
    for (const name of ["SMS — Ada", "NOC duty phone", "NOC verified", "SMS — Fay"]) {
      expect(controls(w, name), `${name}: Deliveries only`).toEqual(["Deliveries"]);
      expect(hasWorkingSwitch(w, name), `${name}: no working switch`).toBe(false);
      expect(row(w, name).find('[data-testid="channel-toggle-locked"]').exists(), `${name}: not even a locked one`).toBe(false);
    }
    expect(norm(row(w, "SMS — Ada").get('[data-testid="channel-status"]').text())).toBe("enabled");
    expect(norm(row(w, "NOC duty phone").get('[data-testid="channel-status"]').text())).toBe("disabled");
    expect(w.findAll("button").some((b) => norm(b.text()) === "Verify"), "Verify is project:manage").toBe(false);
  });

  it("3e‴: the editor's own channel reads “to you”, with “add one” in its chip", async () => {
    sessionFlags.projectAdmin = false;
    sessionFlags.userId = "u-bo";
    const w = await mountList([CALL_BO]);
    expect(norm(row(w, "Call — Bo").get('[data-testid="channel-recipient"]').text())).toBe("to you");
    expect(norm(row(w, "Call — Bo").get('[data-testid="channel-route-chip"]').text())).toBe(
      "Not a route · you have no verified number — add one",
    );
  });

  it("3e⁗: a viewer sees Deliveries on phone rows and nothing else, and no Add channel", async () => {
    sessionFlags.projectAdmin = false;
    sessionFlags.projectWrite = false;
    const w = await mountList([SLACK, SMS_ADA, CALL_BO, NOC]);
    expect(w.findAll("button").some((b) => norm(b.text()) === "Add channel")).toBe(false);
    expect(controls(w, "oncall-slack")).toEqual([]);
    expect(controls(w, "SMS — Ada")).toEqual(["Deliveries"]);
    expect(controls(w, "Call — Bo")).toEqual(["Deliveries"]);
    expect(controls(w, "NOC duty phone")).toEqual(["Deliveries"]);
    expect(norm(row(w, "oncall-slack").get('[data-testid="channel-status"]').text())).toBe("enabled");
    expect(norm(row(w, "Call — Bo").get('[data-testid="channel-route-chip"]').text())).toBe(
      "Not a route · Bo Lindqvist has no verified number",
    );
  });
});

describe("the switch on a phone row", () => {
  it.each([
    ["telephony_disabled", "phone alerts are turned off on this instance", true],
    ["number_unverified", "the number is not verified; verify it before enabling the channel", false],
    ["voice_unsupported_by_provider", "the telephony provider in force sends SMS only; a voice channel cannot be created or enabled", false],
  ])("a refusal (%s) reads the server's sentence under the row and keeps the list", async (reason, sentence, refetch) => {
    const w = await mountList([SMS_ADA, { ...CALL_ADA, enabled: false, deliverable: { route: false, reason: "disabled" } }, SMS_ELI, SLACK]);
    apiMock.PATCH.mockResolvedValue({ data: undefined, error: { error: sentence, reason }, response: { ok: false, status: 409 } });
    await row(w, "Call — Ada").get('button[aria-label="Enable channel"]').trigger("click");
    await flushPromises();
    expect(errorUnder(w, "Call — Ada"), "under the refused row").toBe(sentence);
    expect(errorUnder(w, "SMS — Ada"), "not under the row above").toBeNull();
    expect(errorUnder(w, "SMS — Eli"), "not under the row below").toBeNull();
    expect(w.findAll('[data-testid="channel-row-error"]')).toHaveLength(1);
    expect(w.find('[data-testid="channel-row"][data-name="oncall-slack"]').exists(), "the list stays").toBe(true);
    expect(sessionFlags.fetchMe!.mock.calls.length > 0, "a telephony_disabled refusal re-reads /me").toBe(refetch);
  });

  it("a refused delete reads the server's sentence under that row and keeps the row", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    try {
      const w = await mountList([SMS_ADA, NOC, SMS_ELI]);
      const sentence = "notification channel: in use";
      apiMock.DELETE.mockResolvedValue({ data: undefined, error: { error: sentence }, response: { ok: false, status: 409 } });
      await row(w, "NOC duty phone").findAll("button").find((b) => norm(b.text()) === "Delete")!.trigger("click");
      await flushPromises();
      expect(apiMock.DELETE).toHaveBeenCalledWith("/api/v1/notification-channels/{channelID}", {
        params: { path: { channelID: "nc-noc" } },
      });
      expect(errorUnder(w, "NOC duty phone")).toBe(sentence);
      expect(errorUnder(w, "SMS — Ada")).toBeNull();
      expect(errorUnder(w, "SMS — Eli")).toBeNull();
      expect(w.find('[data-testid="channel-row"][data-name="NOC duty phone"]').exists(), "the row stays").toBe(true);
    } finally {
      confirm.mockRestore();
    }
  });

  it("a row's sentence goes once the form saves that channel (review M-6)", async () => {
    const w = await mountList([{ ...CALL_ADA, enabled: false, deliverable: { route: false, reason: "disabled" } }, SMS_ELI]);
    apiMock.PATCH.mockResolvedValueOnce({
      data: undefined,
      error: { error: "the number is not verified", reason: "number_unverified" },
      response: { ok: false, status: 409 },
    });
    await row(w, "Call — Ada").get('button[aria-label="Enable channel"]').trigger("click");
    await flushPromises();
    expect(errorUnder(w, "Call — Ada")).toBe("the number is not verified");

    await row(w, "Call — Ada").findAll("button").find((b) => norm(b.text()) === "Edit")!.trigger("click");
    await flushPromises();
    apiMock.PATCH.mockResolvedValueOnce({ data: { ...CALL_ADA, name: "Call — Ada" }, error: undefined, response: { ok: true, status: 200 } });
    await w.get('[data-testid="phone-channel-submit"]').trigger("click");
    await flushPromises();
    expect(apiMock.PATCH).toHaveBeenLastCalledWith("/api/v1/notification-channels/{channelID}", {
      params: { path: { channelID: "nc-call-ada" } },
      body: { name: "Call — Ada" },
    });
    expect(errorUnder(w, "Call — Ada")).toBeNull();
    expect(w.findAll('[data-testid="channel-row-error"]')).toHaveLength(0);
  });

  it("a successful switch replaces the row with the server's answer, route state included", async () => {
    const w = await mountList([{ ...SMS_ADA, enabled: false, deliverable: { route: false, reason: "disabled" } }]);
    apiMock.PATCH.mockResolvedValue({ data: { ...SMS_ADA, enabled: true, deliverable: { route: false, reason: "no_verified_number" } } });
    await row(w, "SMS — Ada").get('button[aria-label="Enable channel"]').trigger("click");
    await flushPromises();
    expect(apiMock.PATCH).toHaveBeenCalledWith("/api/v1/notification-channels/{channelID}", {
      params: { path: { channelID: "nc-sms-ada" } },
      body: { enabled: true },
    });
    expect(row(w, "SMS — Ada").find('button[aria-label="Disable channel"]').exists()).toBe(true);
    expect(norm(row(w, "SMS — Ada").get('[data-testid="channel-route-chip"]').text())).toBe(
      "Not a route · Ada Park has no verified number",
    );
  });
});

describe("3g — Deliveries from the row", () => {
  it("clicking Deliveries GETs that channel's phone deliveries and opens the panel under the table; ✕ Close closes it", async () => {
    const w = await mountList(MOCK_3E);
    await row(w, "Call — Bo").findAll("button").find((b) => norm(b.text()) === "Deliveries")!.trigger("click");
    await flushPromises();
    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/notification-channels/{channelID}/phone-deliveries", {
      params: { path: { channelID: "nc-call-bo" } },
    });
    const panel = w.get('[data-testid="channel-deliveries"]');
    expect(norm(panel.get('[data-testid="deliveries-title"]').text())).toBe("Deliveries · Call — Bo");
    expect(norm(panel.get('[data-testid="deliveries-empty"]').text())).toBe("No alert has been sent through this channel yet.");
    // Under the table (decision 15), not above it like the form.
    const html = w.html();
    expect(html.indexOf('data-testid="channel-deliveries"')).toBeGreaterThan(html.indexOf("</table>"));

    await panel.get('[data-testid="deliveries-close"]').trigger("click");
    expect(w.find('[data-testid="channel-deliveries"]').exists()).toBe(false);
  });

  it("a viewer can open Deliveries (project:read)", async () => {
    sessionFlags.projectAdmin = false;
    sessionFlags.projectWrite = false;
    const w = await mountList([SMS_ADA]);
    await row(w, "SMS — Ada").findAll("button").find((b) => norm(b.text()) === "Deliveries")!.trigger("click");
    await flushPromises();
    expect(w.find('[data-testid="channel-deliveries"]').exists()).toBe(true);
  });
});

describe("3e — the row's Verify (3b-2 for an existing channel)", () => {
  it("opens the verification steps on that channel, sends the code to it, and verifies it", async () => {
    const w = await mountList(MOCK_3E);
    await row(w, "NOC duty phone").findAll("button").find((b) => norm(b.text()) === "Verify")!.trigger("click");
    await flushPromises();
    expect(handlerErrors).toEqual([]);
    const form = w.get('[data-testid="channel-form"]');
    expect(form.find('[data-testid="channel-type"]').exists(), "the steps replace Type and Name").toBe(false);
    expect(form.find('[data-testid="channel-edit-note"]').exists(), "not the edit form").toBe(false);
    expect(norm(form.get('[data-testid="phone-verify-created"]').text())).toBe("NOC duty phone — disabled until the number is confirmed.");
    expect(norm(form.get('[data-testid="phone-verify-step-1"]').text())).toContain("1 · Send a code to +44•••••0199");
    expect(apiMock.POST, "nothing is sent by opening Verify").not.toHaveBeenCalled();

    apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { ok: true, status: 204 } });
    await form.get('[data-testid="phone-verify-send"]').trigger("click");
    await flushPromises();
    expect(apiMock.POST).toHaveBeenLastCalledWith("/api/v1/notification-channels/{channelID}/verify/send", {
      params: { path: { channelID: "nc-noc" } },
    });

    const verified = { ...NOC, number_verified: true, disabled_reason: undefined };
    apiMock.POST.mockResolvedValueOnce({ data: verified, error: undefined, response: { ok: true, status: 200 } });
    await form.get('[data-testid="phone-verify-code"]').setValue("123456");
    await form.get('[data-testid="phone-verify-submit"]').trigger("click");
    await flushPromises();
    expect(apiMock.POST).toHaveBeenLastCalledWith("/api/v1/notification-channels/{channelID}/verify", {
      params: { path: { channelID: "nc-noc" } },
      body: { code: "123456" },
    });
    expect(w.find('[data-testid="phone-enable"]').exists(), "3b-3: Enable channel / Keep disabled").toBe(true);
    // The row took the server's answer: no Verify left, and the switch unlocked.
    expect(row(w, "NOC duty phone").findAll("button").some((b) => norm(b.text()) === "Verify")).toBe(false);
    expect(row(w, "NOC duty phone").find('[data-testid="channel-toggle-locked"]').exists()).toBe(false);
  });

  // The owner's manual check (2026-11-18): with the steps already open above the list, a second Verify
  // changed nothing on screen. Every Verify now brings the form into view and focuses its first action.
  it("every Verify scrolls to the form and focuses its first action, also when the steps are already open", async () => {
    const scrolled: Element[] = [];
    const original = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = function (this: Element) {
      scrolled.push(this);
    };
    const host = document.createElement("div");
    document.body.appendChild(host);
    try {
      for (const fn of Object.values(apiMock)) fn.mockReset();
      apiMock.GET.mockResolvedValue({ data: [NOC], response: { ok: true, status: 200 } });
      const w = mount(SettingsView, { attachTo: host, global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
      await flushPromises();
      const clickVerify = async () => {
        await row(w, "NOC duty phone").findAll("button").find((b) => norm(b.text()) === "Verify")!.trigger("click");
        await flushPromises();
      };

      await clickVerify();
      const form = w.get('[data-testid="channel-form"]').element;
      expect(scrolled, "the first Verify brings the form into view").toEqual([form]);
      expect(document.activeElement).toBe(w.get('[data-testid="phone-verify-send"]').element);

      // A code is sent, then the focus moves away, as when the person scrolls back down to the list.
      apiMock.POST.mockResolvedValueOnce({ data: undefined, error: undefined, response: { ok: true, status: 204 } });
      await w.get('[data-testid="phone-verify-send"]').trigger("click");
      await flushPromises();
      (document.activeElement as HTMLElement | null)?.blur();
      scrolled.length = 0;

      await clickVerify();
      expect(scrolled, "a second Verify brings the open form into view again").toEqual([w.get('[data-testid="channel-form"]').element]);
      const send = w.get('[data-testid="phone-verify-send"]');
      expect(norm(send.text()), "the open steps keep the code already sent").toBe("Send a new code");
      expect(document.activeElement, "the form's first action has the focus").toBe(send.element);
      w.unmount();
    } finally {
      Element.prototype.scrollIntoView = original;
      host.remove();
    }
  });

  it("Edit after Verify on the same channel opens the edit form, not the steps", async () => {
    const w = await mountList([NOC]);
    await row(w, "NOC duty phone").findAll("button").find((b) => norm(b.text()) === "Verify")!.trigger("click");
    await flushPromises();
    await row(w, "NOC duty phone").findAll("button").find((b) => norm(b.text()) === "Edit")!.trigger("click");
    await flushPromises();
    expect(w.find('[data-testid="phone-verify-step-1"]').exists()).toBe(false);
    expect(w.find('[data-testid="phone-number-input"]').exists()).toBe(true);
    expect(w.find('[data-testid="channel-edit-note"]').exists()).toBe(true);
  });
});
