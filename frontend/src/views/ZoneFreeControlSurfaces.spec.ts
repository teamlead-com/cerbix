import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { describe, expect, it, vi } from "vitest";

import ServiceDeclarationView from "@/views/ServiceDeclarationView.vue";
import SettingsView from "@/views/SettingsView.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

// NFR-025, the control-surface half, for the two views that had no component spec at all.
//
// The reviewer's N6 is why these exist: a SOURCE guard cannot prove a render. It was satisfied by
// the helper's name inside a live string literal while the DOM had no hint — and he chose
// `ServiceDeclarationView` for every one of his mutations precisely because nothing here reached
// it. The guard is narrowed to template interpolations now, and that is still a claim about
// SOURCE. These read the DOM.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("vue-router", () => ({
  // `?tab=alerting` is how an operator reaches the instance-silence control; the view reads the
  // tab from the query and loads that section only.
  useRoute: () => ({ params: { id: "svc-1" }, query: { tab: "alerting" } }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));

function prepare(get: (path: string) => Promise<unknown> = () => Promise.resolve({ data: [] })) {
  const pinia = createPinia();
  setActivePinia(pinia);
  const ws = useWorkspace();
  ws.orgId = "org-a";
  ws.projectId = "project-a";
  ws.loaded = true;
  const session = useSession();
  session.user = { id: "user-a", is_global_admin: true } as typeof session.user;
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockImplementation((path: string) => get(path));
  return pinia;
}

const ZONE = /local time \(UTC[+-]\d{2}:\d{2}\)/;

describe("every zone-free control names its zone where an operator reads it", () => {
  it("ServiceDeclarationView: the backfill instant", async () => {
    // The view reads `detail.service.name` for its breadcrumb, and `expectedRevision === 0` is
    // what makes the backfill section exist at all — a retroactive first revision is the only
    // one that may adopt history.
    const pinia = prepare((path) => {
      if (path.includes("/services/")) {
        return Promise.resolve({ data: { revision: 0, service: { id: "svc-1", slug: "checkout", name: "Checkout" }, members: [] } });
      }
      return Promise.resolve({ data: [] });
    });
    const w = mount(ServiceDeclarationView, { global: { plugins: [pinia] } });
    await flushPromises();
    const hint = w.find('[data-testid="backfill-zone"]');
    expect(hint.exists(), "the backfill zone hint is missing").toBe(true);
    expect(hint.text()).toMatch(ZONE);
  });

  it("SettingsView: the instance-silence deadline", async () => {
    const pinia = prepare((path) => {
      // The silence control is behind `alerting.enabled`, which is how an operator meets it: you
      // only choose an end when you have switched silence on.
      if (path.endsWith("/settings/alerting")) {
        return Promise.resolve({ data: { global_silence: { enabled: true, until: null } } });
      }
      return Promise.resolve({ data: {} });
    });
    const w = mount(SettingsView, { global: { plugins: [pinia] } });
    await flushPromises();
    await flushPromises();
    const label = w.findAll("span").find((el) => el.text().startsWith("Until"));
    expect(label, "the silence Until label is gone").toBeTruthy();
    expect(label!.text()).toMatch(ZONE);
  });
});
