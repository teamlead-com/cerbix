import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import NewMonitorView from "@/views/NewMonitorView.vue";

// sec-hardening-2 H-2 (approved mock docs/design/mock-websocket-tls.html): a WebSocket monitor's TLS
// is verified by default; the one control — "Skip certificate verification" — shows only for a
// wss:// or https:// endpoint and is sent as config.tls_skip_verify.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
// The edit cases mount the view at /monitors/:id/edit; the create cases with no id.
const routeMock = vi.hoisted(() => ({ params: {} as Record<string, string> }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: {}, params: routeMock.params }),
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

async function mountWebSocketForm() {
  routeMock.params = {};
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockResolvedValue({ data: [] });
  apiMock.POST.mockResolvedValue({ data: { id: "m1" } });
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  const card = w.findAll("button").find((b) => b.text().includes("WebSocket"));
  expect(card, "the form offers a WebSocket type card").toBeTruthy();
  await card!.trigger("click");
  await flushPromises();
  return w;
}

async function setTarget(w: Awaited<ReturnType<typeof mountWebSocketForm>>, value: string) {
  const input = w.findAll("input[type='text']").find((i) => i.attributes("placeholder") === "wss://api.internal/ws");
  expect(input, "the endpoint input").toBeTruthy();
  await input!.setValue(value);
  await flushPromises();
}

describe("WebSocket TLS verification control", () => {
  it("is absent for a plaintext ws:// endpoint", async () => {
    const w = await mountWebSocketForm();
    await setTarget(w, "ws://chat.internal/ws");
    expect(w.find('[data-testid="ws-skip-verify"]').exists()).toBe(false);
  });

  it("appears for wss:// and https://, unchecked by default", async () => {
    const w = await mountWebSocketForm();
    for (const target of ["wss://chat.internal/ws", "https://chat.internal/ws"]) {
      await setTarget(w, target);
      const box = w.find('[data-testid="ws-skip-verify"]');
      expect(box.exists(), target).toBe(true);
      expect((box.element as HTMLInputElement).checked).toBe(false);
    }
  });

  it("sends config.tls_skip_verify when checked", async () => {
    const w = await mountWebSocketForm();
    await w.find('input[placeholder="payments-callback"]').setValue("e2e-ws");
    await setTarget(w, "wss://chat.internal/ws");
    await w.find('[data-testid="ws-skip-verify"]').setValue(true);
    await flushPromises();
    // jsdom does not turn a click on a submit button into a submission; submit the form itself.
    await w.find("form").trigger("submit");
    await flushPromises();
    const call = apiMock.POST.mock.calls.find((c) => c[0] === "/api/v1/projects/{projectID}/monitors");
    expect(call, "the monitor was created").toBeTruthy();
    expect(call![1].body.config).toEqual({ tls_skip_verify: "true" });
  });
});

// External review of iter-0210 (P3-1): the edit path. loadForEdit prefills the box from the stored
// config, and unchecking it sends an empty config, which clears the opt-out.
async function mountWebSocketEdit(config: Record<string, string>) {
  routeMock.params = { id: "m9" };
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path === "/api/v1/monitors/{monitorID}") {
      return Promise.resolve({
        data: {
          id: "m9", project_id: "p1", name: "e2e-ws", type: "websocket", target: "wss://chat.internal/ws",
          interval_seconds: 60, timeout_seconds: 10, retries: 0, enabled: true, region: "core", tags: [], config,
        },
      });
    }
    return Promise.resolve({ data: [] });
  });
  apiMock.PATCH.mockResolvedValue({ data: { id: "m9" } });
  apiMock.PUT.mockResolvedValue({ data: {} });
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  return w;
}

async function submitEdit(w: Awaited<ReturnType<typeof mountWebSocketEdit>>) {
  await w.find("form").trigger("submit");
  await flushPromises();
  const call = apiMock.PATCH.mock.calls.find((c) => c[0] === "/api/v1/monitors/{monitorID}");
  expect(call, "the monitor was updated").toBeTruthy();
  return call![1].body;
}

describe("WebSocket TLS verification control, editing", () => {
  it("is prefilled checked from a stored opt-out, and unchecking it clears the opt-out", async () => {
    const w = await mountWebSocketEdit({ tls_skip_verify: "true" });
    const box = w.find('[data-testid="ws-skip-verify"]');
    expect(box.exists()).toBe(true);
    expect((box.element as HTMLInputElement).checked).toBe(true);
    await box.setValue(false);
    await flushPromises();
    const body = await submitEdit(w);
    expect(body.config).toEqual({});
  });

  it("keeps a stored opt-out when the box is left checked", async () => {
    const w = await mountWebSocketEdit({ tls_skip_verify: "true" });
    const body = await submitEdit(w);
    expect(body.config).toEqual({ tls_skip_verify: "true" });
  });

  it("is prefilled unchecked when nothing was stored", async () => {
    const w = await mountWebSocketEdit({});
    const box = w.find('[data-testid="ws-skip-verify"]');
    expect(box.exists()).toBe(true);
    expect((box.element as HTMLInputElement).checked).toBe(false);
    const body = await submitEdit(w);
    expect(body.config).toEqual({});
  });
});
