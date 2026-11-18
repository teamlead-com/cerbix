import { type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import SettingsView from "@/views/SettingsView.vue";

// Editing a notification channel, as the operator meets it. The rule this file exists
// for cannot be proven anywhere else: the API blanks secret config values
// (internal/domain/notification.go SecretChannelConfigKeys), so the form is never given
// the bot token or the hook URL — and a form that posted back what it was shown would
// send an empty secret and blank a working channel. The server treats a blank secret as
// "keep the stored one", and the assertions below are on the BODY that leaves the page:
// a secret the operator did not retype must be ABSENT from it, and a non-secret field
// must travel as typed, including empty (that is how an optional field is cleared).
//
// Everything the view reaches for outside itself is faked: the API client, the route,
// AppShell and the three stores. Entry point is the view's own deep link,
// `/settings?tab=channels`.

const apiMock = vi.hoisted(() => ({
  GET: vi.fn(),
  POST: vi.fn(),
  PUT: vi.fn(),
  PATCH: vi.fn(),
  DELETE: vi.fn(),
}));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: { tab: "channels" } }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
// Mutable per test: FR-041a's phone types read `telephonyEnabled`, `user.id` and `canProjectAdmin`
// (D28), and a viewer is `canProjectWrite: false`.
const sessionFlags = vi.hoisted(() => ({ telephonyEnabled: true, projectAdmin: false, projectWrite: true }));
vi.mock("@/stores/session", () => ({
  useSession: () => ({
    canProjectWrite: () => sessionFlags.projectWrite,
    isOrgAdmin: () => true,
    canProjectAdmin: () => sessionFlags.projectAdmin, // iter-0206: SettingsView reads it for the project Members tab
    isGlobalAdmin: false,
    totpEnabled: false,
    user: { id: "u-admin", email: "admin@cerbix.local", display_name: "Admin" },
    get telephonyEnabled() {
      return sessionFlags.telephonyEnabled;
    },
    fetchMe: vi.fn(async () => true),
  }),
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({
    init: () => Promise.resolve(),
    orgId: "o1",
    projectId: "p1",
    orgName: "Acme",
    projectName: "API",
    projects: [{ id: "p1", name: "API", slug: "api" }],
  }),
}));
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));

// A telegram channel as the LIST returns it: chat_id survives, bot_token is blanked.
const telegram = {
  id: "nc-tg",
  project_id: "p1",
  type: "telegram" as const,
  name: "oncall-tg",
  config: { bot_token: "", chat_id: "42" },
  enabled: true,
};

type PatchCall = { params: { path: { channelID: string } }; body: { name: string; config: Record<string, string> } };

function lastPatch(): PatchCall {
  const calls = apiMock.PATCH.mock.calls;
  expect(calls.length, "the form issued a PATCH").toBeGreaterThan(0);
  return calls[calls.length - 1][1] as PatchCall;
}

// Errors thrown inside the view's handlers (an Edit click) land here instead of only in a warning.
const handlerErrors: unknown[] = [];

async function mountChannels(channel: Record<string, unknown> | Record<string, unknown>[] = telegram, recipients: unknown[] = []) {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  handlerErrors.length = 0;
  const list = Array.isArray(channel) ? channel : [channel];
  apiMock.GET.mockImplementation(async (path: string) =>
    path === "/api/v1/projects/{projectID}/phone-recipients" ? { data: recipients, response: { ok: true, status: 200 } } : { data: list },
  );
  apiMock.PATCH.mockResolvedValue({ data: { ...channel, name: "renamed" } });
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

async function openEdit(w: VueWrapper) {
  const edit = w.findAll("button").find((b) => b.text() === "Edit");
  expect(edit, "an editor is offered Edit on the channel row").toBeTruthy();
  await edit!.trigger("click");
  expect(w.find('[data-testid="channel-form"]').exists(), "the form opened").toBe(true);
}

function field(w: VueWrapper, key: string) {
  const el = w.find(`[data-testid="channel-field-${key}"]`);
  expect(el.exists(), `the form renders the ${key} field`).toBe(true);
  return el;
}

async function save(w: VueWrapper) {
  const btn = w.findAll("button").find((b) => b.text() === "Save");
  expect(btn, "the edit form's submit says Save, not Create").toBeTruthy();
  await btn!.trigger("click");
  await flushPromises();
}

describe("editing a notification channel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    sessionFlags.telephonyEnabled = true;
    sessionFlags.projectAdmin = false;
    sessionFlags.projectWrite = true;
  });

  it("prefills the name and non-secret config, and leaves the secret blank", async () => {
    const w = await mountChannels();
    await openEdit(w);
    expect((field(w, "chat_id").element as HTMLInputElement).value).toBe("42");
    expect((field(w, "bot_token").element as HTMLInputElement).value, "a secret is never prefilled").toBe("");
    expect(
      field(w, "bot_token").attributes("placeholder"),
      "the blank secret says what leaving it blank means",
    ).toContain("unchanged");
    expect(
      (w.find('[data-testid="channel-type"]').element as HTMLSelectElement).disabled,
      "the type is frozen while editing",
    ).toBe(true);
  });

  it("omits a secret the operator did not retype", async () => {
    const w = await mountChannels();
    await openEdit(w);
    await field(w, "chat_id").setValue("77");
    await save(w);

    const call = lastPatch();
    expect(call.params.path.channelID).toBe("nc-tg");
    expect(call.body.name).toBe("oncall-tg");
    expect(call.body.config.chat_id).toBe("77");
    expect(
      Object.hasOwn(call.body.config, "bot_token"),
      "a blank secret must not travel — the server would read it as a new empty value",
    ).toBe(false);
  });

  it("sends a secret the operator did retype", async () => {
    const w = await mountChannels();
    await openEdit(w);
    await field(w, "bot_token").setValue("NEW-TOKEN");
    await save(w);
    expect(lastPatch().body.config.bot_token).toBe("NEW-TOKEN");
  });

  it("sends a cleared non-secret field, so an optional value can be removed", async () => {
    const email = {
      id: "nc-mail",
      project_id: "p1",
      type: "email" as const,
      name: "ops-mail",
      config: { to: "oncall@x", from: "cerbix@x", smtp_host: "mail", smtp_username: "u", smtp_password: "" },
      enabled: true,
    };
    const w = await mountChannels(email);
    await openEdit(w);
    await field(w, "smtp_username").setValue("");
    await save(w);

    const cfg = lastPatch().body.config;
    expect(cfg.smtp_username, "an emptied non-secret travels as empty").toBe("");
    expect(Object.hasOwn(cfg, "smtp_password"), "the untouched password stays out of the body").toBe(false);
    expect(cfg.to).toBe("oncall@x");
  });

  it("keeps the form open and shows the server's word when the edit is refused", async () => {
    const w = await mountChannels();
    apiMock.PATCH.mockResolvedValue({ error: { error: "notification channel: telegram requires config.bot_token and config.chat_id" } });
    await openEdit(w);
    await field(w, "chat_id").setValue("");
    await save(w);

    expect(w.find('[data-testid="channel-form"]').exists(), "a refusal does not discard the edit").toBe(true);
    expect(w.text()).toContain("requires config.bot_token");
  });

  it("replaces the row on success and does not reopen as a create form", async () => {
    const w = await mountChannels();
    apiMock.PATCH.mockResolvedValue({ data: { ...telegram, name: "oncall-tg-2" } });
    await openEdit(w);
    await field(w, "chat_id").setValue("99");
    await save(w);

    expect(w.find('[data-testid="channel-form"]').exists(), "a saved edit closes the form").toBe(false);
    expect(w.text(), "the list shows the saved name without a reload").toContain("oncall-tg-2");
    expect(apiMock.GET.mock.calls.length, "success needs no re-list").toBe(1);

    // Add channel after an edit starts clean: a stale id would turn a create into a save.
    const add = w.findAll("button").find((b) => b.text() === "Add channel");
    await add!.trigger("click");
    expect(w.findAll("button").some((b) => b.text() === "Create"), "the form is back in create mode").toBe(true);
    expect((w.find('[data-testid="channel-type"]').element as HTMLSelectElement).disabled).toBe(false);
  });
});

// FR-041a: the channel form with the two phone types. The Edit crash this block pins: before the
// fix, Edit on an `sms` or `voice` channel iterated `channelFields[c.type]` — undefined for a phone
// type — and threw "is not iterable"; a type assertion hid it from vue-tsc.
describe("the channel form with SMS and Voice call (FR-041a)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    sessionFlags.telephonyEnabled = true;
    sessionFlags.projectAdmin = true;
    sessionFlags.projectWrite = true;
  });

  const smsChannel = {
    id: "nc-sms",
    project_id: "p1",
    type: "sms" as const,
    name: "SMS — Ada",
    config: {},
    enabled: true,
    recipient: "user" as const,
    recipient_user_id: "u-ada",
    recipient_display_name: "Ada Park",
    deliverable: { route: true },
  };
  const voiceChannel = {
    id: "nc-voice",
    project_id: "p1",
    type: "voice" as const,
    name: "NOC duty phone",
    config: {},
    enabled: true,
    recipient: "number" as const,
    number_masked: "+44•••••0199",
    number_verified: true,
    deliverable: { route: true },
  };
  const recipients = [{ user_id: "u-ada", display_name: "Ada Park", email: "ada@example.com", phone_verified: true }];

  it.each([
    ["an sms channel", smsChannel],
    ["a voice channel", voiceChannel],
  ])("Edit on %s does not throw, opens the phone editor, and saves without a config", async (_label, ch) => {
    const w = await mountChannels(ch, recipients);
    await w.findAll("button").find((b) => b.text() === "Edit")!.trigger("click");
    await flushPromises();
    expect(handlerErrors.map(String), "Edit threw inside the view").toEqual([]);
    expect(w.find('[data-testid="channel-form"]').exists(), "the form opened").toBe(true);
    expect(w.find('[data-testid="phone-channel-form"]').exists(), "the phone editor opened").toBe(true);
    expect(w.find('[data-testid^="channel-field-"]').exists(), "no config field for a phone channel").toBe(false);
    expect(w.text()).toContain(
      `Editing ${ch.name} — the type and who it sends to cannot be changed; another kind of recipient is a new channel.`,
    );
    expect((w.find('[data-testid="channel-type"]').element as HTMLSelectElement).value).toBe(ch.type);

    await save(w);
    expect(handlerErrors).toEqual([]);
    const body = lastPatch().body as unknown as Record<string, unknown>;
    expect(Object.hasOwn(body, "config"), "a phone channel's PATCH carries no config").toBe(false);
    expect(body).toEqual({ name: ch.name });
    expect(w.find('[data-testid="channel-form"]').exists(), "a saved edit closes the form").toBe(false);
  });

  it("offers SMS and Voice call in the type select, and replaces the config fields with the recipient block", async () => {
    const w = await mountChannels(telegram, recipients);
    await w.findAll("button").find((b) => b.text() === "Add channel")!.trigger("click");
    const opts = w.findAll('[data-testid="channel-type"] option');
    expect(opts.map((o) => o.text())).toEqual(["Webhook", "Slack", "Telegram", "Email", "SMS", "Voice call"]);
    expect(opts.every((o) => !(o.element as HTMLOptionElement).disabled)).toBe(true);

    await w.find('[data-testid="channel-type"]').setValue("sms");
    await flushPromises();
    expect(w.find('[data-testid="phone-channel-form"]').exists()).toBe(true);
    expect(w.find('[data-testid^="channel-field-"]').exists()).toBe(false);
  });

  it("disables both phone types while phone alerts are off, with the reason in their label (3f)", async () => {
    sessionFlags.telephonyEnabled = false;
    const w = await mountChannels(telegram);
    await w.findAll("button").find((b) => b.text() === "Add channel")!.trigger("click");
    const opts = w.findAll('[data-testid="channel-type"] option');
    const sms = opts.find((o) => (o.element as HTMLOptionElement).value === "sms")!;
    const voice = opts.find((o) => (o.element as HTMLOptionElement).value === "voice")!;
    expect(sms.text()).toBe("SMS — phone alerts are off");
    expect(voice.text()).toBe("Voice call — phone alerts are off");
    expect((sms.element as HTMLOptionElement).disabled).toBe(true);
    expect((voice.element as HTMLOptionElement).disabled).toBe(true);
    expect((opts[0].element as HTMLOptionElement).disabled).toBe(false);
  });

  it("puts the only-route warning at the top of the edit form, from the channel's own fields (3e″)", async () => {
    const only = {
      ...smsChannel,
      only_route_for: [
        { project: "API", service: "Checkout", slug: "checkout" },
        { project: "API", service: "Payments", slug: "payments" },
      ],
      only_route_for_total: 3,
    };
    const w = await mountChannels(only, recipients);
    await openEdit(w);
    const warn = w.find('[data-testid="phone-only-route"]');
    expect(warn.exists()).toBe(true);
    const t = warn.text().replace(/\s+/g, " ");
    expect(t).toContain("Only route for");
    expect(warn.findAll('[data-testid="phone-only-route-service"]').map((c) => c.text().replace(/\s+/g, " "))).toEqual([
      "service checkout",
      "service payments",
    ]);
    expect(t).toContain("… and 1 more.");
    expect(t).toContain(
      "If a message to it is not sent — storm limit, refused by the provider, outcome unknown — nobody hears about those services' incidents, and their members' own alerts stay held. Add a second channel, or set re-notify on the members.",
    );
    const form = w.find('[data-testid="channel-form"]');
    expect(form.element.firstElementChild, "the warning is the form's first element").toBe(warn.element);
  });

  it("has no only-route warning for a channel that is nobody's only route", async () => {
    const w = await mountChannels({ ...smsChannel, only_route_for: [], only_route_for_total: 0 }, recipients);
    await openEdit(w);
    expect(w.find('[data-testid="phone-only-route"]').exists()).toBe(false);
  });

  it("keeps the form open on a new number channel and adds its row (3b-2)", async () => {
    const w = await mountChannels(telegram, recipients);
    await w.findAll("button").find((b) => b.text() === "Add channel")!.trigger("click");
    await w.find('[data-testid="channel-type"]').setValue("sms");
    await w.find('[data-testid="channel-name"]').setValue("NOC duty phone");
    await flushPromises();
    await w.find('[data-testid="phone-send-to-number"]').setValue(true);
    await w.find('[data-testid="phone-number-input"]').setValue("+44 7700 900199");
    apiMock.POST.mockResolvedValue({
      data: { ...voiceChannel, id: "nc-new", type: "sms", enabled: false, number_verified: false, disabled_reason: "number_unverified" },
      response: { ok: true, status: 201 },
    });
    await w.find('[data-testid="phone-channel-submit"]').trigger("click");
    await flushPromises();

    expect(w.find('[data-testid="channel-form"]').exists(), "the form stays open").toBe(true);
    expect(w.find('[data-testid="phone-verify-created"]').exists()).toBe(true);
    expect(w.find('[data-testid="channel-type"]').exists(), "type and name give way to the verification steps").toBe(false);
    expect(w.text(), "the new row is listed").toContain("2 channel(s)");

    await w.find('[data-testid="phone-verify-later"]').trigger("click");
    expect(w.find('[data-testid="channel-form"]').exists(), "Later closes the form").toBe(false);
  });

  // D28 (review I-1): an editor holds a phone channel only when it phones themselves. Edit, Delete and
  // the switch are offered on that one and on every text channel, never on another person's channel or
  // on a number channel — the server would refuse each with 403.
  it("gives an editor Edit, Delete and the switch only on text channels and their own phone channel", async () => {
    sessionFlags.projectAdmin = false;
    const rows = [
      { ...telegram, name: "oncall-tg" },
      { ...smsChannel, id: "nc-own", name: "Call — me", recipient_user_id: "u-admin", recipient_display_name: "Admin" },
      { ...smsChannel, id: "nc-ada", name: "SMS — Ada" },
      { ...voiceChannel, id: "nc-noc", name: "NOC duty phone" },
    ];
    const w = await mountChannels(rows);
    const row = (name: string) => w.findAll("tbody tr").find((r) => r.text().includes(name))!;
    const buttons = (name: string) => row(name).findAll("button").map((b) => b.attributes("aria-label") ?? b.text());
    expect(buttons("oncall-tg")).toEqual(["Disable channel", "Edit", "Delete"]);
    // Task 5 (mock 3e‴): every phone row also offers Deliveries, which is project:read.
    expect(buttons("Call — me")).toEqual(["Disable channel", "Deliveries", "Edit", "Delete"]);
    expect(buttons("SMS — Ada"), "someone else's person channel").toEqual(["Deliveries"]);
    expect(buttons("NOC duty phone"), "a number channel").toEqual(["Deliveries"]);
    expect(row("SMS — Ada").text(), "the state is still readable").toContain("enabled");
  });

  it("gives a project admin Edit on every phone channel", async () => {
    const rows = [
      { ...smsChannel, id: "nc-ada", name: "SMS — Ada" },
      { ...voiceChannel, id: "nc-noc", name: "NOC duty phone" },
    ];
    const w = await mountChannels(rows);
    expect(w.findAll("button").filter((b) => b.text() === "Edit")).toHaveLength(2);
  });

  it("a person channel closes the form once created, and the next form shows Type again", async () => {
    const w = await mountChannels(telegram, recipients);
    await w.findAll("button").find((b) => b.text() === "Add channel")!.trigger("click");
    await w.find('[data-testid="channel-type"]').setValue("sms");
    await w.find('[data-testid="channel-name"]').setValue("SMS — Ada");
    await flushPromises();
    apiMock.POST.mockResolvedValue({ data: { ...smsChannel, id: "nc-new" }, response: { ok: true, status: 201 } });
    await w.find('[data-testid="phone-channel-submit"]').trigger("click");
    await flushPromises();
    expect(w.find('[data-testid="channel-form"]').exists()).toBe(false);
    expect(w.text()).toContain("2 channel(s)");
    await w.findAll("button").find((b) => b.text() === "Add channel")!.trigger("click");
    expect(w.find('[data-testid="channel-type"]').exists()).toBe(true);
  });

  it("gives a viewer no Add channel and no Edit", async () => {
    sessionFlags.projectWrite = false;
    const w = await mountChannels(smsChannel);
    expect(w.findAll("button").some((b) => b.text() === "Add channel")).toBe(false);
    expect(w.findAll("button").some((b) => b.text() === "Edit")).toBe(false);
  });
});
