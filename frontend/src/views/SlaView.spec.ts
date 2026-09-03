import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { nextTick } from "vue";
import { beforeEach, describe, expect, it, vi } from "vitest";

import SlaView from "@/views/SlaView.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

const apiMock = vi.hoisted(() => ({
  GET: vi.fn(),
  POST: vi.fn(),
  PATCH: vi.fn(),
  PUT: vi.fn(),
  DELETE: vi.fn(),
}));

vi.mock("@/api/client", () => ({ api: apiMock }));

// AppShell drags in theme/live-SSE machinery that has no business in this test; the module is
// mocked (not merely stubbed) because its import side effects run before any stub applies.
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot /><slot name='actions' /></div>" },
}));

type Deferred<T> = { promise: Promise<T>; resolve: (value: T) => void };

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function mountView() {
  const pinia = createPinia();
  setActivePinia(pinia);
  const ws = useWorkspace();
  ws.orgId = "org-a";
  ws.projectId = "project-a";
  ws.loaded = true; // ws.init() becomes a no-op: the test controls the project directly
  const session = useSession();
  session.user = { id: "user-a", is_global_admin: true } as typeof session.user;
  const wrapper = mount(SlaView, {
    global: { plugins: [pinia] },
  });
  return { wrapper, ws };
}

function maintWindow(id: string, reason: string) {
  const now = new Date();
  return {
    id,
    reason,
    monitor_id: "",
    starts_at: new Date(now.getTime() + 3600_000).toISOString(),
    ends_at: new Date(now.getTime() + 7200_000).toISOString(),
  };
}

// The tenant race the store cannot see: it refuses a cross-tenant CONFIRM, but a screen
// rendering project A's windows under project B's name misleads the operator with no write
// ever happening. The guard is a generation captured before the first await and checked
// before every state write — success, error and finally alike.
describe("SlaView tenant context", () => {
  beforeEach(() => {
    for (const method of Object.values(apiMock)) method.mockReset();
  });

  it("ignores a deferred project-A load after switching to project B", async () => {
    const maintA = deferred<{ data: unknown[] }>();
    apiMock.GET.mockImplementation((path: string, options: { params: { path: { projectID?: string } } }) => {
      const project = options.params.path.projectID;
      if (path.endsWith("/maintenance")) {
        if (project === "project-a") return maintA.promise; // A's slowest response
        return Promise.resolve({ data: [maintWindow("mw-b", "b-window")] });
      }
      if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
      return Promise.resolve({ data: [] }); // monitors
    });

    const { wrapper, ws } = mountView();
    await nextTick();

    // Switch while A's maintenance list is still in flight.
    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();
    expect(wrapper.text()).toContain("b-window");

    // A's response lands late. It must change NOTHING: not the list, not the error, not the
    // loading flag of the view that now belongs to B.
    maintA.resolve({ data: [maintWindow("mw-a", "a-window")] });
    await flushPromises();
    expect(wrapper.text()).toContain("b-window");
    expect(wrapper.text()).not.toContain("a-window");
  });

  it("renders a maintenance window's times with their zone named (NFR-025b)", async () => {
    // The source guard in `wallclock.spec.ts` proves no product file renders a timestamp through
    // `toLocaleString` any more. This reaches the SURFACE: what an operator actually reads on this
    // screen now carries the offset, where it used to read `03.09.2026, 17:55` with nothing saying
    // whose 17:55 — beside a reliability card rendering UTC dates.
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/maintenance")) return Promise.resolve({ data: [maintWindow("mw-1", "e2e-window")] });
      if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
      return Promise.resolve({ data: [] });
    });
    const { wrapper } = mountView();
    await flushPromises();
    const text = wrapper.text();
    expect(text).toContain("e2e-window");
    expect(text).toMatch(/UTC[+-]\d{2}:\d{2}/);
    // and the old bare rendering, which had a comma between date and time and no zone, is gone
    expect(text).not.toMatch(/\d{2}\.\d{2}\.\d{4},\s\d{2}:\d{2}(?!\s\()/);
  });

  it("drops a deferred mutation error from the previous project", async () => {
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
      return Promise.resolve({ data: [] });
    });
    const createA = deferred<never>();
    apiMock.POST.mockImplementation(() => createA.promise);

    const { wrapper, ws } = mountView();
    await flushPromises();

    // Start a create in project A…
    const vm = wrapper.vm as unknown as {
      maintForm: { starts_at: string; ends_at: string };
      addMaintenance: () => Promise<void>;
    };
    vm.maintForm.starts_at = new Date(Date.now() + 3600_000).toISOString().slice(0, 16);
    vm.maintForm.ends_at = new Date(Date.now() + 7200_000).toISOString().slice(0, 16);
    const pending = vm.addMaintenance();

    // …switch to B while it is in flight, then let A's request FAIL late.
    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();
    createA.resolve(Promise.reject(new Error("network")) as never);
    await pending.catch(() => {});
    await flushPromises();

    // The stale failure must not surface as an error in project B's context.
    expect(wrapper.text()).not.toContain("Could not schedule maintenance.");
  });

  it("drops a deferred report toggle from the previous project", async () => {
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [], sla_report_weekly: false } });
      return Promise.resolve({ data: [] });
    });
    const toggleA = deferred<{ data: { sla_report_weekly: boolean } }>();
    apiMock.PUT.mockImplementation(() => toggleA.promise);

    const { wrapper, ws } = mountView();
    await flushPromises();

    const vm = wrapper.vm as unknown as {
      toggleReport: () => Promise<void>;
      reportEnabled: boolean;
      reportSaving: boolean;
    };
    const pending = vm.toggleReport();

    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();

    // A's toggle result lands late: project B's switch must not flip ON, and the busy flag
    // A's finally would have cleared must not be touched by a dead generation either way.
    toggleA.resolve({ data: { sla_report_weekly: true } });
    await pending;
    await flushPromises();
    expect(vm.reportEnabled).toBe(false);
  });

  it("keeps archived windows out of the scheduler list across reloads", async () => {
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/maintenance"))
        return Promise.resolve({
          data: [
            maintWindow("mw-live", "live-window"),
            { ...maintWindow("mw-arch", "archived-window"), archived_at: new Date().toISOString() },
          ],
        });
      if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
      return Promise.resolve({ data: [] });
    });

    // The reload IS the regression: archiving removed the row locally, but the next load
    // brought it back because the list carried no archive state to filter on.
    const { wrapper } = mountView();
    await flushPromises();
    expect(wrapper.text()).toContain("live-window");
    expect(wrapper.text()).not.toContain("archived-window");
  });

  it("drops a deferred maintenance archive from the previous project", async () => {
    apiMock.GET.mockImplementation((path: string, options: { params: { path: { projectID?: string } } }) => {
      const project = options.params.path.projectID;
      if (path.endsWith("/maintenance")) {
        if (project === "project-b") return Promise.resolve({ data: [maintWindow("mw-b", "b-window")] });
        return Promise.resolve({ data: [maintWindow("mw-b", "a-copy")] });
      }
      if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
      return Promise.resolve({ data: [] });
    });
    const deleteA = deferred<{ error?: unknown }>();
    apiMock.DELETE.mockImplementation(() => deleteA.promise);

    const { wrapper, ws } = mountView();
    await flushPromises();

    const vm = wrapper.vm as unknown as { deleteMaintenance: (id: string) => Promise<void> };
    const pending = vm.deleteMaintenance("mw-b");

    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();
    expect(wrapper.text()).toContain("b-window");

    // A's archive succeeds late. B's window shares the id in this construction — precisely
    // the case where an ungated filter would silently remove B's row.
    deleteA.resolve({});
    await pending;
    await flushPromises();
    expect(wrapper.text()).toContain("b-window");
  });
});

describe("SlaView objective rule (D-0165, iter-0143)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("canonicalizes the objective on the client: post-round bounds reject before any request, the canonical value is what gets sent", async () => {
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/monitors"))
        return Promise.resolve({ data: [{ id: "mon-1", name: "api", type: "http", interval_seconds: 30 }] });
      if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
      return Promise.resolve({ data: [] });
    });
    apiMock.PUT.mockResolvedValue({ data: {} });

    const { wrapper } = mountView();
    await flushPromises();

    const vm = wrapper.vm as unknown as {
      rows: Array<{ monitor: { id?: string } }>;
      editSlo: (r: unknown) => void;
      saveSlo: (m: unknown) => Promise<void>;
      draft: string | number;
      rowError: string;
    };
    expect(vm.rows.length).toBe(1);
    const row = vm.rows[0];
    vm.editSlo(row as never);

    // The two boundary values [203] named: both pass a raw-only check and died as server
    // 400s before this rule reached the client. Now they are rejected HERE, with no PUT.
    for (const bad of ["99.99995", "0.00001"]) {
      vm.draft = bad;
      await vm.saveSlo(row.monitor as never);
      expect(vm.rowError, `draft=${bad}`).toContain("above 0 and below 100");
      expect(apiMock.PUT, `draft=${bad}`).not.toHaveBeenCalled();
    }

    // The acceptance case: 99.99994 canonicalizes to 99.9999 and THAT is the wire value.
    vm.draft = "99.99994";
    await vm.saveSlo(row.monitor as never);
    await flushPromises();
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    const putArgs = apiMock.PUT.mock.calls[0] as [string, { body: { objective: number } }];
    expect(putArgs[0]).toBe("/api/v1/monitors/{monitorID}/sla-target");
    expect(putArgs[1].body.objective).toBe(99.9999);
  });
});

// The project objective (iter-0155, mock approved by the owner). What is worth pinning is what the mock
// decided, not the markup: the card states the promise about the WHOLE and is distinct from the mean
// across monitors; a project without one shows "not set" and no invented number; the client rejects a
// value the server would refuse, so the operator reads why instead of a 400; and Clear takes the budget
// with it.
describe("SlaView project objective", () => {
  const projectSla = (objective?: number) => ({
    project_id: "project-a",
    sla_report_weekly: false,
    windows: [
      { window: "24h", total: 100, up: 100, uptime_percent: 100, avg_latency_ms: 5, p95_latency_ms: 9 },
      {
        window: "30d",
        total: 1000,
        up: 999,
        uptime_percent: 99.9,
        avg_latency_ms: 5,
        p95_latency_ms: 9,
        ...(objective != null
          ? { objective, error_budget: { met: true, burned_percent: 26, allowed_downtime_seconds: 100, actual_downtime_seconds: 26 } }
          : {}),
      },
    ],
  });

  function wire(objective?: number) {
    apiMock.GET.mockReset();
    apiMock.PUT.mockReset();
    apiMock.DELETE.mockReset();
    apiMock.GET.mockImplementation(async (path: string) => {
      if (path === "/api/v1/projects/{projectID}/sla") return { data: projectSla(objective) };
      if (path === "/api/v1/projects/{projectID}/monitors") return { data: [] };
      if (path === "/api/v1/projects/{projectID}/maintenance") return { data: [] };
      return { data: [] };
    });
    apiMock.PUT.mockResolvedValue({ data: { objective: 99.9, window: "30d" } });
    apiMock.DELETE.mockResolvedValue({ data: undefined });
  }

  it("states the project's own promise, and says nothing when there is none", async () => {
    wire();
    const { wrapper } = mountView();
    await flushPromises();

    expect(wrapper.find('[data-testid="project-objective-unset"]').text()).toBe("not set");
    // No budget without an objective: a percentage here would be a promise nobody made.
    expect(wrapper.find('[data-testid="project-objective-budget"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="project-objective-clear"]').exists()).toBe(false);

    wire(99.9);
    const second = mountView();
    await flushPromises();
    expect(second.wrapper.find('[data-testid="project-objective-value"]').text()).toContain("99.9");
    expect(second.wrapper.find('[data-testid="project-objective-budget"]').text()).toContain("74%");
  });

  it("does not carry a typed draft across a project switch (P0)", async () => {
    // D-0235 decision 11 / reviewer P0: `resetMaintenanceState` reset seven pieces of editor and
    // busy state and its own comment claimed to cover EVERY one, but not this card's. A value
    // typed for project A was still in the box under project B, where Save wrote it as a
    // perfectly legitimate write the store cannot refuse — not the one the operator meant.
    wire(99.9);
    const { wrapper, ws } = mountView();
    await flushPromises();

    await wrapper.find('[data-testid="project-objective-edit"]').trigger("click");
    await wrapper.find('[data-testid="project-objective-input"]').setValue("95");
    expect(wrapper.find('[data-testid="project-objective-dirty"]').text()).toContain("95");

    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();

    // the editor is closed and the draft is gone BEFORE any of B's data renders
    expect(wrapper.find('[data-testid="project-objective-input"]').exists()).toBe(false);
    expect(wrapper.text()).not.toContain("unsaved draft 95");
    // and reopening under B shows B's own stored value, never A's typed one
    await wrapper.find('[data-testid="project-objective-edit"]').trigger("click");
    expect((wrapper.find('[data-testid="project-objective-input"]').element as HTMLInputElement).value).not.toBe("95");
  });

  it("drops a deferred objective save from the previous project (P0)", async () => {
    // The card's two writers took no load generation while every neighbour in this file does and
    // one of them says why: "the project moved under this response". A save fired under A that
    // returns after the switch wrote A's error into B's screen and cleared B's busy flag.
    wire(99.9);
    const putA = deferred<{ error: { error: string } }>();
    apiMock.PUT.mockImplementation(() => putA.promise);

    const { wrapper, ws } = mountView();
    await flushPromises();
    await wrapper.find('[data-testid="project-objective-edit"]').trigger("click");
    await wrapper.find('[data-testid="project-objective-input"]').setValue("95");
    await wrapper.find('[data-testid="project-objective-save"]').trigger("click");

    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();

    // A's save fails, late. It must change NOTHING about the screen that now belongs to B.
    putA.resolve({ error: { error: "boom" } });
    await flushPromises();
    expect(wrapper.find('[data-testid="project-objective-error"]').exists()).toBe(false);
    expect(wrapper.text()).not.toContain("Could not save the project objective");
    // B's editor is still closed and untouched by A's response
    expect(wrapper.find('[data-testid="project-objective-input"]').exists()).toBe(false);
  });

  it("meets an impossible objective at the field, live, with Save unable to send it", async () => {
    // FR-031 §7: the card is read-only until Edit, and the refusal is met AT THE FIELD as the
    // operator types — requiring a click to learn why would be worse than the form this replaced.
    wire();
    const { wrapper } = mountView();
    await flushPromises();

    // closed by default: a closed editor cannot hold a stale draft
    expect(wrapper.find('[data-testid="project-objective-input"]').exists()).toBe(false);
    await wrapper.find('[data-testid="project-objective-edit"]').trigger("click");

    await wrapper.find('[data-testid="project-objective-input"]').setValue("100");
    expect(wrapper.find('[data-testid="project-objective-invalid"]').text()).toContain("below 100");
    // and a control that cannot do anything is not offered
    expect(wrapper.find('[data-testid="project-objective-save"]').attributes("disabled")).toBeDefined();
    await wrapper.find('[data-testid="project-objective-save"]').trigger("click");
    await flushPromises();
    expect(apiMock.PUT).not.toHaveBeenCalled();
  });

  it("sends the canonical value, closes on success and says so, then clears through the API", async () => {
    wire(99.9);
    const { wrapper } = mountView();
    await flushPromises();

    await wrapper.find('[data-testid="project-objective-edit"]').trigger("click");
    // opening prefills with the STORED value, which is therefore not a change yet
    expect((wrapper.find('[data-testid="project-objective-input"]').element as HTMLInputElement).value).toBe("99.9");
    expect(wrapper.find('[data-testid="project-objective-clean"]').text()).toContain("nothing to save");
    expect(wrapper.find('[data-testid="project-objective-save"]').attributes("disabled")).toBeDefined();

    await wrapper.find('[data-testid="project-objective-input"]').setValue("99.99994");
    // the card can now SAY which state it is in — the defect was that it could not
    expect(wrapper.find('[data-testid="project-objective-dirty"]').text()).toContain("unsaved draft 99.9999%");
    expect(wrapper.find('[data-testid="project-objective-save"]').attributes("disabled")).toBeUndefined();

    await wrapper.find('[data-testid="project-objective-save"]').trigger("click");
    await flushPromises();
    expect(apiMock.PUT).toHaveBeenCalled();
    const body = apiMock.PUT.mock.calls[0][1].body;
    expect(body.objective).toBe(99.9999);
    expect(body.window).toBe("30d");
    // a successful save CLOSES the editor, clears the draft and confirms itself: an unsent draft
    // can no longer render identically to a stored fact
    expect(wrapper.find('[data-testid="project-objective-input"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="project-objective-saved"]').text()).toContain("saved");

    await wrapper.find('[data-testid="project-objective-edit"]').trigger("click");
    await wrapper.find('[data-testid="project-objective-clear"]').trigger("click");
    await flushPromises();
    expect(apiMock.DELETE).toHaveBeenCalled();
    expect(apiMock.DELETE.mock.calls[0][1].params.query.window).toBe("30d");
    expect(wrapper.find('[data-testid="project-objective-input"]').exists()).toBe(false);
  });
});
