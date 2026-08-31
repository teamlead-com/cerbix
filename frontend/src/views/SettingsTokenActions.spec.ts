import { type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ROLE_GRANTS } from "@/lib/tokenActions";
import SettingsView from "@/views/SettingsView.vue";

// FR-025 D12 / D-0210 item 6: the CI token's optional `actions` allow-list, as the OPERATOR meets it.
// `lib/tokenActions.spec.ts` proves the table and the three helpers; it cannot prove that the form on
// the API-tokens tab is wired to them — a chip grid built from the wrong list, a role change that
// leaves a stale selection standing, or an `actions: []` sent on an empty selection (which would mean
// "allow nothing", not "the role decides") would all pass that spec untouched. So this file mounts the
// real SettingsView, opens the real form, clicks the real chips, and reads the body that reaches the
// API — the helpers are imported here only as the expectation for what a role's grants ARE.
//
// Entry point: `/settings?tab=tokens` — the view's own deep link (`route.query.tab`), so the tab is the
// one the operator lands on, not a ref poked from outside. Everything the view reaches for outside
// itself is faked: the API client, the route, AppShell, and the three stores.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: { tab: "tokens" } }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
// Org admin: the whole form is behind `canManageOrg`, and a viewer would render no chips at all.
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

type Body = { name: string; role: string; project_id?: string; actions?: string[] };

/** Mounts the settings page on its API-tokens tab and opens the "Issue token" form. */
async function mountTokenForm() {
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
  apiMock.GET.mockResolvedValue({ data: [] }); // the org has no tokens yet
  apiMock.POST.mockResolvedValue({ data: { api_token: { id: "t1", name: "ci-deploy-bot" }, token: "cbx_secret" } });
  const w = mount(SettingsView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  expect(w.text(), "the API-tokens tab is the one that rendered").toContain("token(s)");
  await openForm(w);
  return w;
}

async function openForm(w: VueWrapper) {
  const issue = w.findAll("button").find((b) => b.text() === "Issue token");
  expect(issue, "the org admin is offered the Issue token button").toBeTruthy();
  await issue!.trigger("click");
  expect(w.find('[data-testid="token-actions"]').exists(), "the chip grid is part of the form").toBe(true);
}

/** The role <select> — identified by the options it carries, not by its position in the form. */
function roleSelect(w: VueWrapper) {
  const sel = w.findAll("select").find((s) => s.findAll("option").some((o) => o.attributes("value") === "editor"));
  expect(sel, "the form offers a role picker").toBeTruthy();
  return sel!;
}

const group = (w: VueWrapper) => w.get('[data-testid="token-actions"]');
const chips = (w: VueWrapper) => group(w).findAll('[data-testid^="token-action-"]');
const nameOf = (testid: string) => testid.replace("token-action-", "");
const offered = (w: VueWrapper) =>
  chips(w).filter((c) => c.attributes("data-lacking") !== "true").map((c) => nameOf(c.attributes("data-testid")!));
const dormant = (w: VueWrapper) =>
  chips(w).filter((c) => c.attributes("data-lacking") === "true").map((c) => nameOf(c.attributes("data-testid")!));
const selected = (w: VueWrapper) =>
  chips(w).filter((c) => c.attributes("data-on") === "true").map((c) => nameOf(c.attributes("data-testid")!));

async function pick(w: VueWrapper, action: string) {
  await group(w).get(`[data-testid="token-action-${action}"]`).trigger("click");
}

async function issue(w: VueWrapper, name = "ci-deploy-bot"): Promise<Body> {
  await w.get('input[placeholder="e.g. ci-deploy-bot"]').setValue(name);
  await w.get('[data-testid="token-create"]').trigger("click");
  await flushPromises();
  expect(apiMock.POST, "the form posted the token").toHaveBeenCalledTimes(1);
  return apiMock.POST.mock.calls[0][1].body as Body;
}

describe("SettingsView API-token allow-list (FR-025 D12)", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
  });

  it("offers exactly the role's grants and shows the rest dormant with the role they need", async () => {
    const w = await mountTokenForm();
    await roleSelect(w).setValue("editor");

    // The editor's grants, in the catalogue order the grid lays them out. Hard-coded on purpose: this
    // mirrors `roleGrants["editor"]` in internal/authz/authz.go, so a drift in either table fails here.
    expect(offered(w)).toEqual(["project:read", "project:write", "gate:evaluate", "gate:policy:write", "change:record", "org:read"]);
    expect([...offered(w)].sort(), "…and that is precisely the role's grant set").toEqual([...ROLE_GRANTS.editor].sort());
    expect(dormant(w), "what the editor lacks is shown, not hidden and not offered").toEqual([
      "project:manage",
      "gate:override",
      "org:manage",
    ]);

    // A lacking action is INERT and names the role that would grant it — never a chip that can only 400.
    const override = group(w).get('[data-testid="token-action-gate:override"]');
    expect(override.element.tagName, "the dormant entry is not a button").toBe("SPAN");
    expect(override.attributes("aria-disabled")).toBe("true");
    expect(override.attributes("title")).toBe("needs project_admin");
    expect(w.get('[data-testid="token-actions-hint"]').text()).toContain("gate:override needs project_admin");
    await override.trigger("click");
    expect(selected(w), "clicking a dormant entry selects nothing").toEqual([]);
  });

  it("sends no `actions` key when nothing is picked, and exactly the picked ones when they are", async () => {
    const bare = await mountTokenForm();
    await roleSelect(bare).setValue("editor");
    expect(selected(bare)).toEqual([]);
    const empty = await issue(bare);
    expect(empty, "an untouched grid means the ROLE decides — an empty list would allow nothing").not.toHaveProperty("actions");
    expect(Object.keys(empty).sort()).toEqual(["name", "role"]);
    expect(empty.role).toBe("editor");

    const picked = await mountTokenForm();
    await roleSelect(picked).setValue("editor");
    await pick(picked, "project:read");
    await pick(picked, "change:record");
    expect(selected(picked)).toEqual(["project:read", "change:record"]);
    expect((await issue(picked)).actions).toEqual(["project:read", "change:record"]);
  });

  it("drops a selection the new role does not grant, in the UI and in the next POST body", async () => {
    const w = await mountTokenForm();
    await roleSelect(w).setValue("editor");
    await pick(w, "gate:policy:write");
    await pick(w, "project:read");
    expect(selected(w)).toEqual(["project:read", "gate:policy:write"]);

    // Viewer grants project:read but not gate:policy:write.
    await roleSelect(w).setValue("viewer");
    expect(selected(w), "the narrowed role prunes what it cannot grant").toEqual(["project:read"]);
    expect(dormant(w), "the pruned action is now shown dormant").toContain("gate:policy:write");
    expect(group(w).find('[data-testid="token-action-gate:policy:write"]').attributes("data-on")).toBeUndefined();

    const body = await issue(w);
    expect(body.role).toBe("viewer");
    expect(body.actions, "the stale selection reaches neither the UI nor the wire").toEqual(["project:read"]);
  });

  it("warns while a non-empty list leaves project:read out, and stops warning once it is in", async () => {
    const w = await mountTokenForm();
    await roleSelect(w).setValue("editor");
    expect(w.find('[data-testid="token-actions-warning"]').exists(), "an empty list is the role's full set — nothing to warn about").toBe(false);

    await pick(w, "change:record");
    const warning = w.find('[data-testid="token-actions-warning"]');
    expect(warning.exists(), "a narrowed token without project:read cannot read the service page").toBe(true);
    expect(warning.text()).toContain("will not be able to read the service page");

    await pick(w, "project:read");
    expect(w.find('[data-testid="token-actions-warning"]').exists()).toBe(false);

    await pick(w, "project:read"); // …and back off again
    expect(w.find('[data-testid="token-actions-warning"]').exists()).toBe(true);
  });
});
