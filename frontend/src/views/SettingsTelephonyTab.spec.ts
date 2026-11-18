import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useSession } from "@/stores/session";
import SettingsView from "@/views/SettingsView.vue";

// FR-041a §4.1, mock §1: Settings → Administration → Telephony sits directly after Email, for a
// global admin only (the REAL session store decides), and opens from `/settings?tab=telephony`.
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

function as(globalAdmin: boolean) {
  const s = useSession();
  s.user = { id: "u", email: "u@example.com", is_global_admin: globalAdmin } as never;
  s.memberships = [{ org_id: "o1", project_id: null, role: "org_admin" }] as never;
}
function adminTabs(w: ReturnType<typeof mount>): string[] {
  const group = w.findAll("nav > div").find((g) => g.text().startsWith("Administration"));
  return group ? group.findAll("button").map((b) => b.text()) : [];
}

beforeEach(() => {
  setActivePinia(createPinia());
  routeState.query = {};
  for (const f of Object.values(apiMock)) f.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/settings/telephony") {
      return { data: { configured: false, encryption_key_configured: true, enabled: false, provider: "" }, response: { status: 200, ok: true } };
    }
    if (path === "/api/v1/settings/mail") return { data: { public_base_url: "" }, response: { status: 200, ok: true } };
    return { data: [], response: { status: 200, ok: true } };
  });
});

describe("Settings → Administration → Telephony", () => {
  it("is listed directly after Email for a global admin", async () => {
    as(true);
    const w = mountView();
    await flushPromises();
    const tabs = adminTabs(w);
    expect(tabs[tabs.indexOf("Email") + 1]).toBe("Telephony");
  });

  it("is not listed for anyone else", async () => {
    as(false);
    const w = mountView();
    await flushPromises();
    expect(adminTabs(w)).not.toContain("Telephony");
    expect(w.text()).not.toContain("Telephony");
  });

  it("a non-admin's deep link ?tab=telephony opens no panel and reads nothing", async () => {
    routeState.query = { tab: "telephony" };
    as(false);
    const w = mountView();
    await flushPromises();
    expect(w.find('[data-testid="telephony-panel"]').exists()).toBe(false);
    const paths = apiMock.GET.mock.calls.map((c) => c[0]);
    expect(paths).not.toContain("/api/v1/settings/telephony");
    expect(paths).not.toContain("/api/v1/settings/mail");
  });

  it("opens from the deep link ?tab=telephony", async () => {
    routeState.query = { tab: "telephony" };
    as(true);
    const w = mountView();
    await flushPromises();
    expect(w.find('[data-testid="telephony-panel"]').exists()).toBe(true);
    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/settings/telephony");
  });
});
