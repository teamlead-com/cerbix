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
vi.mock("@/stores/session", () => ({
  useSession: () => ({
    canProjectWrite: () => true,
    isOrgAdmin: () => true,
    isGlobalAdmin: false,
    totpEnabled: false,
    user: { email: "admin@cerbix.local" },
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

async function mountChannels(channel = telegram) {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockResolvedValue({ data: [channel] });
  apiMock.PATCH.mockResolvedValue({ data: { ...channel, name: "renamed" } });
  const w = mount(SettingsView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
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
  beforeEach(() => vi.clearAllMocks());

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
