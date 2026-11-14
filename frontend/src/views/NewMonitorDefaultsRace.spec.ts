import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import NewMonitorView from "@/views/NewMonitorView.vue";

// The instance defaults (Settings → Monitor defaults) prefill a NEW monitor. They arrive after the
// workspace and five lists load — late enough for a person, or e2e/tests/canary-ui.spec.ts:22, to
// have typed a timeout and an interval already. A default fills a field only while the person has
// not changed it; a value they typed is never overwritten. (Owner ruling for iter-0212, outside
// sec-hardening-2; the form's appearance does not change.)

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

const DEFAULTS = {
  interval_seconds: 120,
  timeout_seconds: 20,
  retries: 2,
  failure_threshold: 3,
  renotify_seconds: 600,
  auto_incident: false,
};

// Mounts the create form with the defaults request held open; returns the release.
function mountWithHeldDefaults() {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/monitor-defaults") {
      await held;
      return { data: DEFAULTS };
    }
    return { data: [] };
  });
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  return { w, release };
}

const input = (w: ReturnType<typeof mount>, id: string) => w.find(`[data-testid="${id}"]`).element as HTMLInputElement;

describe("instance defaults never overwrite what the person typed", () => {
  it("keeps a timeout and an interval typed before the defaults arrive", async () => {
    const { w, release } = mountWithHeldDefaults();
    await flushPromises();
    await w.find('[data-testid="monitor-timeout"]').setValue("300");
    await w.find('[data-testid="monitor-interval"]').setValue("300");
    release();
    await flushPromises();
    expect(input(w, "monitor-timeout").value, "typed timeout").toBe("300");
    expect(input(w, "monitor-interval").value, "typed interval").toBe("300");
  });

  it("still prefills every field the person did not touch", async () => {
    const { w, release } = mountWithHeldDefaults();
    await flushPromises();
    await w.find('[data-testid="monitor-timeout"]').setValue("45");
    release();
    await flushPromises();
    expect(input(w, "monitor-timeout").value, "typed timeout").toBe("45");
    expect(input(w, "monitor-interval").value, "untouched interval takes the default").toBe("120");
  });

  it("prefills from the defaults when nothing was typed", async () => {
    const { w, release } = mountWithHeldDefaults();
    release();
    await flushPromises();
    expect(input(w, "monitor-timeout").value).toBe("20");
    expect(input(w, "monitor-interval").value).toBe("120");
  });
});
