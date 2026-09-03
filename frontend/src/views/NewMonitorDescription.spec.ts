import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import NewMonitorView from "@/views/NewMonitorView.vue";
import { MAX_MONITOR_DESCRIPTION } from "@/lib/monitorBounds";

// FR-030 against the REAL form: the field exists under Name, the count is live and counts code points,
// the refusal is met at the field, Create stays disabled while it stands, and the description leaves the
// page trimmed in the create body.

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

async function mountForm() {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockResolvedValue({ data: [] });
  apiMock.POST.mockResolvedValue({ data: { id: "m1" } });
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  return w;
}

const createButton = (w: ReturnType<typeof mount>) =>
  w.findAll("button").find((b) => /create monitor/i.test(b.text()))!;

describe("monitor description field", () => {
  it("counts code points live and shows the limit", async () => {
    const w = await mountForm();
    const field = w.find('[data-testid="monitor-description"]');
    expect(field.exists(), "the field sits in the form").toBe(true);
    await field.setValue("я".repeat(120));
    await flushPromises();
    expect(w.find('[data-testid="monitor-description-count"]').text()).toBe(`120 / ${MAX_MONITOR_DESCRIPTION}`);
    expect(w.find('[data-testid="monitor-description-error"]').exists()).toBe(false);
  });

  it("refuses at the field one past the limit and disables Create until it fits", async () => {
    const w = await mountForm();
    await w.find('[data-testid="monitor-description"]').setValue("x".repeat(MAX_MONITOR_DESCRIPTION + 1));
    await flushPromises();
    expect(w.find('[data-testid="monitor-description-error"]').text()).toContain(`${MAX_MONITOR_DESCRIPTION}`);
    expect(w.find('[data-testid="monitor-description-count"]').text()).toBe(`${MAX_MONITOR_DESCRIPTION + 1} / ${MAX_MONITOR_DESCRIPTION}`);
    expect(createButton(w).attributes("disabled"), "Create must be disabled while over").toBeDefined();

    await w.find('[data-testid="monitor-description"]').setValue("x".repeat(MAX_MONITOR_DESCRIPTION));
    await flushPromises();
    expect(w.find('[data-testid="monitor-description-error"]').exists()).toBe(false);
  });

  it("sends the description trimmed in the create body", async () => {
    const w = await mountForm();
    // A push monitor needs no target, so the only thing between the form and Create is the name.
    const push = w.findAll("button").find((b) => b.text().includes("Push"));
    expect(push, "the form offers a Push type card").toBeTruthy();
    await push!.trigger("click");
    await flushPromises();
    await w.find('input[placeholder="payments-callback"]').setValue("payments-callback");
    await w.find('[data-testid="monitor-description"]').setValue("  Confirms the provider reaches our callback.  ");
    await flushPromises();
    expect(createButton(w).attributes("disabled"), "a valid monitor must be submittable").toBeUndefined();
    // The page submits through the FORM (jsdom does not turn a click on a submit button into a
    // submission), the same way the canary and scenario-binding specs drive it.
    await w.find("form").trigger("submit");
    await flushPromises();
    expect(apiMock.POST).toHaveBeenCalled();
    const body = apiMock.POST.mock.calls.at(-1)![1].body as any;
    expect(body.description).toBe("Confirms the provider reaches our callback.");
  });
});
