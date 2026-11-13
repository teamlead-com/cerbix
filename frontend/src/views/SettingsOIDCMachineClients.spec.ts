import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useSession } from "@/stores/session";
import SettingsView from "@/views/SettingsView.vue";

// sec-hardening-2 H-1: Settings → Authentication edits `machine_clients` — the client ids whose
// client-credentials JWTs the API bearer path accepts. It loads and saves the list, and flags this
// app's own Client ID before the server refuses it (Save stays enabled: the server is the authority).
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
const routeState = vi.hoisted(() => ({ query: { tab: "authentication" } as Record<string, string> }));
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
    orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API",
    projects: [{ id: "p1", name: "API", slug: "api" }],
    init: async () => {},
  });
  return { useWorkspace: () => ws };
});
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));

const view = {
  configured: true, active: true, enabled: true, issuer: "https://idp/x", client_id: "cerbix",
  client_secret_set: true, redirect_url: "https://c/auth/callback", scopes: ["openid"], post_logout_url: "",
  button_label: "", bootstrap_admins: [], machine_clients: ["ci-deployer"],
};

beforeEach(() => {
  setActivePinia(createPinia());
  const s = useSession();
  s.user = { id: "ga", email: "root@example.com", is_global_admin: true } as never;
  for (const f of Object.values(apiMock)) f.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/settings/oidc") return { data: view };
    return { data: [] };
  });
  apiMock.PUT.mockResolvedValue({ data: { ...view, active: true } });
});

async function mounted() {
  const w = mount(SettingsView);
  await flushPromises();
  return w;
}

describe("Settings → Authentication: machine clients", () => {
  it("loads the list and saves it back", async () => {
    const w = await mounted();
    expect(w.findAll('[data-testid="machine-client-chip"]').map((c) => c.text().replace("×", ""))).toEqual(["ci-deployer"]);
    const input = w.get('[data-testid="machine-client-input"]');
    await input.setValue("terraform-runner");
    await input.trigger("keydown", { key: "Enter" });
    await w.findAll("button").find((b) => b.text() === "Save & apply")!.trigger("click");
    await flushPromises();
    const call = apiMock.PUT.mock.calls.find((c) => c[0] === "/api/v1/settings/oidc");
    expect(call, "the OIDC settings were saved").toBeTruthy();
    expect(call![1].body.machine_clients).toEqual(["ci-deployer", "terraform-runner"]);
  });

  it("flags this app's own Client ID", async () => {
    const w = await mounted();
    expect(w.find('[data-testid="machine-client-error"]').exists()).toBe(false);
    const input = w.get('[data-testid="machine-client-input"]');
    await input.setValue(" cerbix ");
    await input.trigger("keydown", { key: "Enter" });
    expect(w.get('[data-testid="machine-client-error"]').text()).toContain("cerbix");
  });
});
