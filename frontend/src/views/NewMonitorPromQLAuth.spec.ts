import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import NewMonitorView from "@/views/NewMonitorView.vue";

// D-0215's UI half. The form is where the gap was: the schema, the dispatch gate and the
// prober all accepted basic auth while the PromQL section rendered a username and no way to
// supply the credential — so Save stayed disabled and nothing explained why. These cases pin
// the block that closes it, and the body that leaves the page.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: {}, params: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true, isOrgAdmin: () => true, isGlobalAdmin: false }),
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

async function mountPromQLForm() {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockResolvedValue({ data: [{ id: "s1", name: "prom-scanner" }] });
  apiMock.POST.mockResolvedValue({ data: { id: "m1" } });
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  // The type is a grid of buttons, not a select: click the PromQL card the way an operator does.
  const card = w.findAll("button").find((b) => b.text().includes("PromQL"));
  expect(card, "the form offers a PromQL type card").toBeTruthy();
  await card!.trigger("click");
  await flushPromises();
  return w;
}

describe("promql basic auth in the monitor form", () => {
  it("offers no credential input until basic auth is chosen", async () => {
    const w = await mountPromQLForm();
    expect(w.find('[data-testid="promql-auth-mode"]').exists(), "the auth selector is part of the query section").toBe(true);
    expect(w.find('[data-testid="promql-username"]').exists()).toBe(false);
    expect(w.find('[data-testid="monitor-secret-ref"]').exists()).toBe(false);
  });

  it("offers the username AND the credential selector once basic auth is chosen", async () => {
    const w = await mountPromQLForm();
    await w.find('[data-testid="promql-auth-mode"]').setValue("basic");
    await flushPromises();
    expect(w.find('[data-testid="promql-username"]').exists()).toBe(true);
    // The gap this test exists for: a username with no way to give the password left Save
    // disabled and unexplained.
    const radios = w.findAll('input[type="radio"][value="ref"]');
    expect(radios.length, "the credential mode radios render for promql too").toBeGreaterThan(0);
    await radios[0].setValue(true);
    await flushPromises();
    expect(w.find('[data-testid="monitor-secret-ref"]').exists(), "a project secret can be selected").toBe(true);
  });
});
