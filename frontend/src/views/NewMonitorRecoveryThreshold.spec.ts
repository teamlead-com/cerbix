import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import NewMonitorView from "@/views/NewMonitorView.vue";

// FR-043 against the REAL form (func-recovery-threshold.md §8, approved mock
// docs/design/mock-recovery-threshold.html sections 1–3): the "Recovery threshold" field sits in the
// Schedule card under Confirmations, 1..10, default 1; the long-soak warning appears at
// interval × (M − 1) ≥ 600 s; the dependency hint only with a parent AND M > 1; push and composite
// neither show nor send the field (D8), whatever it held before the type was switched; a multi-region
// set gives its regional children the form's value and its composite parent none.

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

const HINT_TEXT = 'Consecutive successful checks before "up" again. The monitor stays down until this many checks in a row succeed. 1–10.';
const DEP_HINT =
  "This monitor depends on other monitors. A parent with a lower recovery threshold recovers first — and from that moment stops suppressing this monitor's alerts, even while this monitor is still down.";

interface Setup {
  editMonitor?: Record<string, unknown>;
  projectMonitors?: Record<string, unknown>[];
  regions?: { name: string; live: boolean }[];
}

async function mountForm(setup: Setup = {}) {
  routeMock.params = setup.editMonitor ? { id: String(setup.editMonitor.id) } : {};
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/monitors/{monitorID}") return { data: setup.editMonitor };
    if (path === "/api/v1/projects/{projectID}/monitors") return { data: setup.projectMonitors ?? [] };
    if (path === "/api/v1/regions") return { data: { regions: setup.regions ?? [{ name: "core", live: true }] } };
    if (path === "/api/v1/monitor-defaults") return { data: {} };
    return { data: [] };
  });
  let n = 0;
  apiMock.POST.mockImplementation(async () => ({ data: { id: `new-${++n}` } }));
  apiMock.PATCH.mockImplementation(async () => ({ data: { id: setup.editMonitor?.id } }));
  apiMock.DELETE.mockResolvedValue({});
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  // loadForEdit releases its prefill guard on a zero timeout.
  await new Promise((r) => setTimeout(r, 0));
  await flushPromises();
  return w;
}
type W = Awaited<ReturnType<typeof mountForm>>;

const field = (w: W) => w.find('[data-testid="monitor-recovery-threshold"]');
const input = (w: W) => field(w).find("input");
const soak = (w: W) => w.find('[data-testid="recovery-soak-warning"]');
const depHint = (w: W) => w.find('[data-testid="recovery-dependency-hint"]');

async function pickType(w: W, label: string) {
  const card = w.findAll("button").find((b) => b.find("b").exists() && b.find("b").text() === label);
  expect(card, `the form offers a ${label} type card`).toBeTruthy();
  await card!.trigger("click");
  await flushPromises();
}
async function setName(w: W, v: string) {
  await w.find('input[placeholder="payments-callback"]').setValue(v);
}
async function setTarget(w: W, v: string) {
  await w.find('input[placeholder="https://api.internal/health"]').setValue(v);
}
async function setInterval(w: W, v: number) {
  await w.find('[data-testid="monitor-interval"]').setValue(String(v));
}
async function submit(w: W) {
  await flushPromises();
  await w.find("form").trigger("submit");
  await flushPromises();
}
const createBodies = () =>
  apiMock.POST.mock.calls.filter((c) => c[0] === "/api/v1/projects/{projectID}/monitors").map((c) => c[1].body as Record<string, unknown>);

describe("recovery threshold field (create)", () => {
  it("sits in the Schedule card directly under Confirmations, 1..10, unit ok, the mock's hint", async () => {
    const w = await mountForm();
    expect(field(w).exists(), "the field is on the http form").toBe(true);
    expect(field(w).text()).toContain("Recovery threshold");
    expect(field(w).text()).toContain("ok");
    expect(field(w).text()).toContain(HINT_TEXT);
    expect(input(w).attributes("type")).toBe("number");
    expect(input(w).attributes("min")).toBe("1");
    expect(input(w).attributes("max")).toBe("10");
    expect((input(w).element as HTMLInputElement).value).toBe("1");
    // Three per row: Interval·Timeout·Retries, Confirmations·Confirm interval·Re-notify, then this
    // field opens the third row — the same column as Confirmations.
    const grid = field(w).element.parentElement!;
    const cells = Array.from(grid.children);
    const confirmations = cells.findIndex((c) => c.textContent?.includes("Confirmations"));
    expect(confirmations).toBeGreaterThanOrEqual(0);
    expect(cells.indexOf(field(w).element)).toBe(confirmations + 3);
  });

  it("sends the default 1 on create for http", async () => {
    const w = await mountForm();
    await setName(w, "api");
    await setTarget(w, "https://api.internal/health");
    await submit(w);
    expect(createBodies()).toHaveLength(1);
    expect(createBodies()[0].recovery_threshold).toBe(1);
  });

  it("sends the edited value on create", async () => {
    const w = await mountForm();
    await setName(w, "api");
    await setTarget(w, "https://api.internal/health");
    await input(w).setValue("4");
    await submit(w);
    expect(createBodies()[0].recovery_threshold).toBe(4);
  });
});

describe("long-soak warning", () => {
  it("is shown at 300 s × 3 with the warning text", async () => {
    const w = await mountForm();
    await setInterval(w, 300);
    await input(w).setValue("3");
    await flushPromises();
    expect(soak(w).exists()).toBe(true);
    expect(soak(w).find("b").text()).toBe("Slow recovery.");
    expect(soak(w).text()).toBe(
      "Slow recovery. Recovery will be announced about 10 min after the first successful check (2 more checks × 5 min). Checks are not sped up during recovery.",
    );
  });

  it("is absent at 60 s × 3", async () => {
    const w = await mountForm();
    await setInterval(w, 60);
    await input(w).setValue("3");
    await flushPromises();
    expect(field(w).exists()).toBe(true);
    expect(soak(w).exists()).toBe(false);
  });
});

describe("dependency hint", () => {
  const parent = { id: "parent-1", name: "orders-db", type: "postgres", status: "up" };
  async function withParent() {
    const w = await mountForm({ projectMonitors: [parent] });
    const pick = w.findAll("button").find((b) => b.text().includes("orders-db"));
    expect(pick, "the dependency picker lists the parent").toBeTruthy();
    await pick!.trigger("click");
    await flushPromises();
    return w;
  }

  it("is shown with a parent and threshold 2, with the mock's text", async () => {
    const w = await withParent();
    await input(w).setValue("2");
    await flushPromises();
    expect(depHint(w).exists()).toBe(true);
    expect(depHint(w).text()).toBe(DEP_HINT);
    expect(depHint(w).find("b").text()).toBe("This monitor depends on other monitors.");
  });

  it("is hidden with a parent at threshold 1", async () => {
    const w = await withParent();
    await input(w).setValue("2");
    await input(w).setValue("1");
    await flushPromises();
    expect(depHint(w).exists()).toBe(false);
  });

  it("is hidden without a parent", async () => {
    const w = await mountForm({ projectMonitors: [parent] });
    await input(w).setValue("3");
    await flushPromises();
    expect(depHint(w).exists()).toBe(false);
  });

  it("follows the soak warning when both apply", async () => {
    const w = await withParent();
    await setInterval(w, 300);
    await input(w).setValue("3");
    await flushPromises();
    expect(soak(w).exists()).toBe(true);
    expect(depHint(w).exists()).toBe(true);
    const order = soak(w).element.compareDocumentPosition(depHint(w).element);
    expect(order & Node.DOCUMENT_POSITION_FOLLOWING, "the warning comes first").toBeTruthy();
  });
});

describe("push and composite carry no recovery threshold (D8)", () => {
  it("push: the field disappears and the request omits it after it was edited to 3", async () => {
    const w = await mountForm();
    await input(w).setValue("3");
    await pickType(w, "Push");
    expect(field(w).exists()).toBe(false);
    expect(soak(w).exists()).toBe(false);
    await setName(w, "heartbeat");
    await submit(w);
    expect(createBodies()).toHaveLength(1);
    expect(createBodies()[0].type).toBe("push");
    expect("recovery_threshold" in createBodies()[0]).toBe(false);
  });

  it("composite: the field disappears and the request omits it after it was edited to 3", async () => {
    const w = await mountForm({ projectMonitors: [{ id: "c1", name: "child-a", type: "http", status: "up" }] });
    await input(w).setValue("3");
    await pickType(w, "Group");
    expect(field(w).exists()).toBe(false);
    await setName(w, "group");
    const box = w.findAll("input[type='checkbox']").find((i) => i.element.parentElement?.textContent?.includes("child-a"));
    expect(box, "the composite member checkbox").toBeTruthy();
    await box!.setValue(true);
    await submit(w);
    expect(createBodies()).toHaveLength(1);
    expect(createBodies()[0].type).toBe("composite");
    expect("recovery_threshold" in createBodies()[0]).toBe(false);
  });

  it("switching back to http shows the field again", async () => {
    const w = await mountForm();
    await input(w).setValue("3");
    await pickType(w, "Push");
    await pickType(w, "HTTP(S)");
    expect(field(w).exists()).toBe(true);
  });
});

describe("multi-region set", () => {
  it("gives each regional child the form's value and omits it on the composite parent", async () => {
    const w = await mountForm({
      regions: [
        { name: "core", live: true },
        { name: "eu", live: true },
      ],
    });
    await setName(w, "api");
    await setTarget(w, "https://api.internal/health");
    await input(w).setValue("3");
    const section = w.findAll("section").find((s) => s.text().includes("Multi-region set"))!;
    await section.find('[role="switch"]').trigger("click");
    await flushPromises();
    for (const r of ["core", "eu"]) {
      const b = section.findAll("button").find((x) => x.text() === r);
      expect(b, `region button ${r}`).toBeTruthy();
      await b!.trigger("click");
    }
    await submit(w);
    const bodies = createBodies();
    expect(bodies).toHaveLength(3);
    const children = bodies.filter((b) => b.type === "http");
    const parent = bodies.find((b) => b.type === "composite")!;
    expect(children.map((c) => c.recovery_threshold)).toEqual([3, 3]);
    expect(parent, "the quorum composite is created").toBeTruthy();
    expect("recovery_threshold" in parent).toBe(false);
  });
});

describe("edit mode", () => {
  const stored = {
    id: "m9",
    name: "api",
    type: "http",
    target: "https://api.internal/health",
    method: "GET",
    interval_seconds: 60,
    timeout_seconds: 10,
    failure_threshold: 2,
    recovery_threshold: 4,
    depends_on: [],
  };

  it("loads the stored value and sends it on save", async () => {
    const w = await mountForm({ editMonitor: stored });
    expect((input(w).element as HTMLInputElement).value).toBe("4");
    await submit(w);
    expect(apiMock.PATCH).toHaveBeenCalledTimes(1);
    expect(apiMock.PATCH.mock.calls[0][1].body.recovery_threshold).toBe(4);
  });

  it("sends a changed value on save", async () => {
    const w = await mountForm({ editMonitor: stored });
    await input(w).setValue("6");
    await submit(w);
    expect(apiMock.PATCH.mock.calls[0][1].body.recovery_threshold).toBe(6);
  });

  it("an edited push monitor shows no field and sends none", async () => {
    const w = await mountForm({
      editMonitor: { id: "m8", name: "hb", type: "push", interval_seconds: 300, grace_seconds: 60, recovery_threshold: 1 },
    });
    expect(field(w).exists()).toBe(false);
    await submit(w);
    expect(apiMock.PATCH).toHaveBeenCalledTimes(1);
    expect("recovery_threshold" in apiMock.PATCH.mock.calls[0][1].body).toBe(false);
  });
});
