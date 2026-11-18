import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useSession } from "@/stores/session";
import SettingsView from "@/views/SettingsView.vue";

// FR-041a §4.2, mock §2: Settings → Account → Phone sits right after Security for every signed-in
// person (SSO included), opens from `/settings?tab=phone`, and is where the Telephony tab's
// "Add one under Account → Phone" leads (Task 2's link, inert until this tab existed).
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
const routeState = vi.hoisted(() => ({ query: {} as Record<string, string> }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => routeState,
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/workspace", async () => {
  const { reactive } = await vi.importActual<typeof import("vue")>("vue");
  const ws = reactive({
    orgId: "o1",
    projectId: "p1",
    orgName: "Acme",
    projectName: "API",
    projects: [{ id: "p1", name: "API", slug: "api" }],
    init: async () => {},
  });
  return { useWorkspace: () => ws };
});
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));

const stubs = { RouterLink: { template: "<a><slot /></a>" } };
const mounted: ReturnType<typeof mount>[] = [];
function mountView() {
  const w = mount(SettingsView, { global: { stubs } });
  mounted.push(w);
  return w;
}
afterEach(() => {
  while (mounted.length) mounted.pop()!.unmount();
});

function as(globalAdmin: boolean, sso = false) {
  const s = useSession();
  s.user = { id: "u", email: "u@example.com", is_global_admin: globalAdmin, ...(sso ? { oidc_sub: "kc-123" } : {}) } as never;
  s.memberships = [{ org_id: "o1", project_id: null, role: "viewer" }] as never;
  s.telephonyEnabled = true;
  s.phone = null;
}
function groupTabs(w: ReturnType<typeof mount>, label: string): string[] {
  const group = w.findAll("nav > div").find((g) => g.text().startsWith(label));
  return group ? group.findAll("button").map((b) => b.text()) : [];
}

beforeEach(() => {
  setActivePinia(createPinia());
  routeState.query = {};
  for (const f of Object.values(apiMock)) f.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/settings/telephony") {
      return {
        data: { configured: true, encryption_key_configured: true, enabled: true, provider: "twilio", twilio: { account_sid: "AC1", auth_token_set: true, from: "+15550100" } },
        response: { status: 200, ok: true },
      };
    }
    if (path === "/api/v1/settings/mail") return { data: { public_base_url: "" }, response: { status: 200, ok: true } };
    return { data: [], response: { status: 200, ok: true } };
  });
});

describe("Settings → Account → Phone", () => {
  it("the Account group is Security, then Phone", async () => {
    as(false);
    const w = mountView();
    await flushPromises();
    expect(groupTabs(w, "Account")).toEqual(["Security", "Phone"]);
  });

  it("is listed for an SSO account too", async () => {
    as(false, true);
    const w = mountView();
    await flushPromises();
    expect(groupTabs(w, "Account")).toContain("Phone");
  });

  it("opens from the deep link ?tab=phone", async () => {
    routeState.query = { tab: "phone" };
    as(false);
    const w = mountView();
    await flushPromises();
    expect(w.find('[data-testid="phone-panel"]').exists()).toBe(true);
  });

  it("the Telephony tab's 'Add one under Account → Phone' now opens the Phone tab", async () => {
    routeState.query = { tab: "telephony" };
    as(true);
    const w = mountView();
    await flushPromises();
    await w.get('[data-testid="telephony-test-phone-link"]').trigger("click");
    await flushPromises();
    expect(w.find('[data-testid="phone-panel"]').exists()).toBe(true);
    expect(w.find('[data-testid="telephony-panel"]').exists()).toBe(false);
  });

  it("the Phone tab's 'Turn them on under Administration → Telephony' opens the Telephony tab for a global admin", async () => {
    routeState.query = { tab: "phone" };
    as(true);
    useSession().telephonyEnabled = false;
    const w = mountView();
    await flushPromises();
    await w.get('[data-testid="phone-off-link"]').trigger("click");
    await flushPromises();
    expect(w.find('[data-testid="telephony-panel"]').exists()).toBe(true);
  });
});
