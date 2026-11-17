import { flushPromises, mount } from "@vue/test-utils";
import { reactive } from "vue";
import { beforeEach, describe, expect, it, vi } from "vitest";

import IncidentDetailView from "@/views/IncidentDetailView.vue";

// FR-021 phase 3 ([301] P1-1): the impact enrichment must SURVIVE an acknowledge. The
// acknowledge endpoint answers with the BASE incident — no impacts, no
// impacts_unavailable — so assigning its response wholesale would silently erase the
// chips the detail was loaded with. This is the regression the [298] fix pass claimed and
// did not have.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: { id: "inc1" } }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
// AppShell drags in the theme/live-SSE machinery (matchMedia) that has no business in this
// test; the module is replaced the way SlaView.spec.ts does it.
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true }),
}));
const workspaceMock = vi.hoisted(() => ({
  init: vi.fn(() => Promise.resolve()),
  orgId: "o1",
  projectId: "p1",
  orgName: "Org A",
  projectName: "Project A",
  transitionPending: false,
  transitionError: "",
  transition: { generation: 0, pendingOrgId: "", error: "", failedOrgId: "" },
}));
const reactiveWorkspace = reactive(workspaceMock);
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => reactiveWorkspace }));

const RouterLink = { props: ["to"], template: "<a><slot /></a>" };

const BASE_INCIDENT = {
  id: "inc1",
  project_id: "p1",
  title: "checkout-http is down",
  status: "investigating",
  impact: "major",
  source: "auto",
  started_at: "2026-08-17T10:14:07Z",
  monitor_id: "mon1",
};

const LINK = {
  service_id: "svc-pay",
  slug: "payments",
  name: "Payments",
  role: "probable_root",
  path: ["payments", "checkout"],
  computed_at: "2026-08-17T10:14:12Z",
};

function mountWith(detail: Record<string, unknown>) {
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/updates")) return Promise.resolve({ data: [] });
    if (path.endsWith("/postmortem")) return Promise.resolve({ error: { error: "not found" } });
    return Promise.resolve({ data: detail });
  });
  // The acknowledge response is the BASE incident: this is the wire contract, and the
  // whole point of the regression.
  apiMock.POST.mockImplementation(() =>
    Promise.resolve({ data: { ...BASE_INCIDENT, acknowledged_at: "2026-08-17T10:20:00Z", acknowledged_by: "u1" } }),
  );
  return mount(IncidentDetailView, { global: { stubs: { RouterLink } } });
}

beforeEach(() => {
  workspaceMock.init.mockReset();
  workspaceMock.init.mockResolvedValue(undefined);
  workspaceMock.orgId = "o1";
  workspaceMock.projectId = "p1";
  workspaceMock.orgName = "Org A";
  workspaceMock.projectName = "Project A";
  workspaceMock.transitionPending = false;
  workspaceMock.transitionError = "";
  workspaceMock.transition = { generation: 0, pendingOrgId: "", error: "", failedOrgId: "" };
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("IncidentDetailView workspace fencing", () => {
  it("does not read or render an incident after cold workspace initialization fails", async () => {
    workspaceMock.init.mockRejectedValueOnce(new Error("project read failed"));
    const wrapper = mountWith(BASE_INCIDENT);

    await flushPromises();

    expect(apiMock.GET.mock.calls.some(([path]) => path === "/api/v1/incidents/{incidentID}")).toBe(false);
    expect(wrapper.text()).not.toContain(BASE_INCIDENT.title);
  });

  it("clears the old incident during a workspace transition and drops every late read", async () => {
    const lateReads: Array<{
      promise: Promise<{ data: Record<string, unknown> }>;
      resolve: (value: { data: Record<string, unknown> }) => void;
    }> = [];
    let incidentReads = 0;
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/incidents/{incidentID}") {
        incidentReads += 1;
        if (incidentReads === 1) return Promise.resolve({ data: BASE_INCIDENT });
        const late = deferred<{ data: Record<string, unknown> }>();
        lateReads.push(late);
        return late.promise;
      }
      if (path.endsWith("/updates")) return Promise.resolve({ data: [] });
      if (path.endsWith("/postmortem")) return Promise.resolve({ error: { error: "not found" } });
      return Promise.resolve({ data: undefined });
    });

    const wrapper = mount(IncidentDetailView, { global: { stubs: { RouterLink } } });
    await flushPromises();
    expect(wrapper.text()).toContain(BASE_INCIDENT.title);

    reactiveWorkspace.transition = { generation: 1, pendingOrgId: "", error: "", failedOrgId: "" };
    await vi.waitFor(() => expect(incidentReads).toBeGreaterThan(1));
    const readsBeforeTransition = incidentReads;

    reactiveWorkspace.transitionPending = true;
    reactiveWorkspace.projectId = "";
    reactiveWorkspace.transition = { generation: 2, pendingOrgId: "org-b", error: "", failedOrgId: "" };
    await flushPromises();

    expect(wrapper.text()).not.toContain(BASE_INCIDENT.title);
    expect(incidentReads).toBe(readsBeforeTransition);

    for (const late of lateReads) late.resolve({ data: BASE_INCIDENT });
    await flushPromises();
    expect(wrapper.text()).not.toContain(BASE_INCIDENT.title);
  });
});

// FR-022, mock panel 1: the header says what the incident is an incident OF. The name comes from a
// SECOND request, so the interesting cases are the ones where that request has not answered or has
// nothing to answer about.
describe("IncidentDetailView subject chip", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
  });

  function mountAnchored(anchor: Record<string, unknown>, subject: Record<string, unknown> | null) {
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/updates")) return Promise.resolve({ data: [] });
      if (path.endsWith("/postmortem")) return Promise.resolve({ error: { error: "not found" } });
      if (path.includes("/services/")) return Promise.resolve({ data: subject ? { service: subject } : undefined });
      if (path.includes("/monitors/")) return Promise.resolve({ data: subject ?? undefined });
      return Promise.resolve({ data: { ...BASE_INCIDENT, monitor_id: undefined, ...anchor, impacts: [] } });
    });
    return mount(IncidentDetailView, { global: { stubs: { RouterLink } } });
  }

  it("names the service a service incident belongs to", async () => {
    const w = mountAnchored({ service_id: "svc1" }, { slug: "checkout", name: "Checkout" });
    await flushPromises();
    const chip = w.get('[data-testid="incident-subject"]');
    expect(chip.text()).toContain("service");
    expect(chip.text()).toContain("checkout");
  });

  it("names the monitor for a monitor incident — the anchor changed, the grammar did not", async () => {
    const w = mountAnchored({ monitor_id: "mon1" }, { name: "checkout-http" });
    await flushPromises();
    expect(w.get('[data-testid="incident-subject"]').text()).toContain("checkout-http");
  });

  it("still renders the incident when its subject is GONE, stating the kind alone", async () => {
    const w = mountAnchored({ service_id: "svc-deleted" }, null);
    await flushPromises();
    // A timeline is a record of something that happened; deleting the subject does not unhappen it.
    expect(w.text()).toContain(BASE_INCIDENT.title);
    expect(w.get('[data-testid="incident-subject"]').text()).toContain("service");
  });

  it("shows NO chip on a project-level incident", async () => {
    const w = mountAnchored({}, null);
    await flushPromises();
    expect(w.find('[data-testid="incident-subject"]').exists()).toBe(false);
  });
});

describe("IncidentDetailView impact enrichment", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
  });

  it("keeps populated impact chips across an acknowledge", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: [LINK] });
    await flushPromises();

    expect(wrapper.findAll('[data-testid="impact-root"]')).toHaveLength(1);
    expect(wrapper.get('[data-testid="impact-root"]').text()).toContain("payments");

    const ack = wrapper.findAll("button").find((b) => b.text() === "Acknowledge");
    expect(ack, "the acknowledge button must be present for an unacknowledged incident").toBeTruthy();
    await ack!.trigger("click");
    await flushPromises();

    // The acknowledge landed…
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    expect(wrapper.text()).toContain("acknowledged");
    // …and the enrichment the base response knows nothing about survived it.
    expect(wrapper.findAll('[data-testid="impact-root"]')).toHaveLength(1);
    expect(wrapper.get('[data-testid="impact-root"]').text()).toContain("payments");
  });

  it("keeps the impacts-unavailable state across an acknowledge", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: null, impacts_unavailable: true });
    await flushPromises();
    expect(wrapper.find('[data-testid="impact-unavailable"]').exists()).toBe(true);

    const ack = wrapper.findAll("button").find((b) => b.text() === "Acknowledge");
    await ack!.trigger("click");
    await flushPromises();

    // A degraded read stays degraded — it must not silently become "no impact".
    expect(wrapper.find('[data-testid="impact-unavailable"]').exists()).toBe(true);
    expect(wrapper.findAll('[data-testid="impact-root"]')).toHaveLength(0);
  });

  it("renders no impact block at all when the incident honestly has no links", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: [] });
    await flushPromises();
    expect(wrapper.find('[data-testid="incident-impacts"]').exists()).toBe(false);
  });

  it("ranks probable roots nearest-first, exactly as the 🕸 note orders them", async () => {
    const far = { ...LINK, service_id: "svc-db", slug: "billing-db", path: ["billing-db", "payments", "checkout"] };
    const wrapper = mountWith({ ...BASE_INCIDENT, impacts: [far, LINK] });
    await flushPromises();
    const chips = wrapper.findAll('[data-testid="impact-root"]');
    expect(chips).toHaveLength(2);
    expect(chips[0].text()).toContain("payments");
    expect(chips[1].text()).toContain("billing-db");
    // The stored path renders verbatim, root first.
    expect(chips[1].text()).toContain("billing-db → payments → checkout");
  });
});

// The API contract has always said "omit to keep the current status (a plain comment)". This screen
// echoed the status it had last loaded instead, so a comment written while somebody else moved the
// incident forward carried a stale opinion about the lifecycle — and the server, which used to take
// that opinion at face value, reverted the transition.
describe("IncidentDetailView plain comment", () => {
  it("sends no status at all when the composer only carries a body", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, status: "identified" });
    await flushPromises();

    const box = wrapper.find('[data-testid="update-body"]');
    await box.setValue("adding a note");
    await wrapper.find('[data-testid="update-post"]').trigger("click");
    await flushPromises();

    const call = apiMock.POST.mock.calls.find(
      (c) => String(c[0]).endsWith("/updates"),
    ) as [string, { body: Record<string, unknown> }] | undefined;
    if (!call) throw new Error("the update was never posted");
    expect(call[1].body).toEqual({ body: "adding a note" });
    expect("status" in call[1].body).toBe(false);
  });

  it("still sends the status when the operator picked one", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, status: "investigating" });
    await flushPromises();

    await wrapper.find('[data-testid="update-body"]').setValue("cause found");
    const picker = wrapper.find('[data-testid="update-status-identified"]');
    await picker.trigger("click");
    await wrapper.find('[data-testid="update-post"]').trigger("click");
    await flushPromises();

    const call = apiMock.POST.mock.calls.find(
      (c) => String(c[0]).endsWith("/updates"),
    ) as [string, { body: Record<string, unknown> }] | undefined;
    if (!call) throw new Error("the update was never posted");
    expect(call[1].body).toEqual({ status: "identified", body: "cause found" });
  });
});

// The server refuses a backward transition, so a button offering one can only fail. Both screens now
// offer the current status and the ones after it, and resolving stays its own action.
describe("IncidentDetailView status picker", () => {
  it("offers no step the server would refuse", async () => {
    const wrapper = mountWith({ ...BASE_INCIDENT, status: "monitoring" });
    await flushPromises();

    expect(wrapper.find('[data-testid="update-status-monitoring"]').exists()).toBe(true);
    for (const backward of ["investigating", "identified"]) {
      expect(wrapper.find(`[data-testid="update-status-${backward}"]`).exists()).toBe(false);
    }
    // Resolving is its own action, not a rung on this picker.
    expect(wrapper.find('[data-testid="update-status-resolved"]').exists()).toBe(false);
  });
});

// FR-042 / NFR-035, mock §3 and spec §4.6: the basis line, in its six forms, under the meta row.
describe("IncidentDetailView impact basis", () => {
  // A link stub that SAYS where it goes, so "a link only while service_id is set" is checkable.
  const LinkStub = {
    props: ["to"],
    template: `<a data-testid="link" :data-to="JSON.stringify(to)"><slot /></a>`,
  };
  const MAPPING = { degraded: "minor", down: "major", unknown: "none" };

  function mountDetail(detail: Record<string, unknown>, updates: unknown[] = []) {
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
    apiMock.GET.mockImplementation((path: string) => {
      if (path.endsWith("/updates")) return Promise.resolve({ data: updates });
      if (path.endsWith("/postmortem")) return Promise.resolve({ error: { error: "not found" } });
      if (path.includes("/services/") || path.includes("/monitors/")) return Promise.resolve({ data: undefined });
      return Promise.resolve({ data: detail });
    });
    // The acknowledge response is the BASE incident: no basis, no unavailable flag (D15, I11).
    apiMock.POST.mockImplementation(() =>
      Promise.resolve({ data: { ...BASE_INCIDENT, ...pickBase(detail), acknowledged_at: "2026-08-17T10:20:00Z" } }),
    );
    return mount(IncidentDetailView, { global: { stubs: { RouterLink: LinkStub } } });
  }
  function pickBase(detail: Record<string, unknown>) {
    const { impact_basis: _b, impact_basis_unavailable: _u, impacts: _i, impacts_unavailable: _iu, ...base } = detail;
    return base;
  }
  const basis = (w: ReturnType<typeof mountDetail>) => w.get('[data-testid="incident-impact-basis"]');

  it("service_mapping, service exists: the name links to the service, the state and frozen mapping follow", async () => {
    const w = mountDetail({
      ...BASE_INCIDENT,
      monitor_id: undefined,
      service_id: "svc1",
      impact_basis: { source: "service_mapping", mapping: MAPPING, state: "down", service: { name: "Checkout", slug: "checkout" } },
    });
    await flushPromises();
    const line = basis(w);
    // The mapping is its own block line under the sentence (D-11); the sentence is what precedes it.
    expect(line.text()).toMatch(/^Major impact — declared by service Checkout for state down/);
    // The mapping is a block line visually, but copy/paste and screen readers read textContent:
    // the state and the mapping must not run together as "downmapping".
    expect(line.element.textContent).toMatch(/for state down\s+mapping frozen at open:/);
    const link = line.get('[data-testid="link"]');
    expect(link.text()).toBe("Checkout");
    expect(JSON.parse(link.attributes("data-to")!)).toEqual({ name: "service", params: { id: "svc1" } });
    expect(line.get('[data-testid="incident-impact-basis-mapping"]').text()).toBe(
      "mapping frozen at open: down → major · degraded → minor · unknown → not specified",
    );
  });

  it("service_mapping, service deleted: the snapshot name as text, then a muted (deleted)", async () => {
    const w = mountDetail({
      ...BASE_INCIDENT,
      impact: "minor",
      monitor_id: undefined,
      service_id: undefined,
      impact_basis: {
        source: "service_mapping",
        mapping: { degraded: "minor", down: "critical", unknown: "none" },
        state: "degraded",
        service: { name: "Legacy search", slug: "legacy-search" },
      },
    });
    await flushPromises();
    const line = basis(w);
    expect(line.find('[data-testid="link"]').exists()).toBe(false);
    expect(line.text()).toContain("Minor impact — declared by service Legacy search (deleted) for state degraded");
    expect(line.get('[data-testid="incident-impact-basis-deleted"]').text()).toBe("(deleted)");
  });

  it("service_mapping with impact none starts with the phrase, never a bare None", async () => {
    const w = mountDetail({
      ...BASE_INCIDENT,
      impact: "none",
      service_id: "svc1",
      impact_basis: { source: "service_mapping", mapping: MAPPING, state: "unknown", service: { name: "Checkout", slug: "checkout" } },
    });
    await flushPromises();
    expect(basis(w).text()).toMatch(/^Impact not specified — declared by service Checkout for state unknown/);
  });

  it("project_default links 'project default' to Settings → Incidents", async () => {
    const w = mountDetail({ ...BASE_INCIDENT, impact_basis: { source: "project_default" } });
    await flushPromises();
    expect(basis(w).text()).toBe("Major impact — project default for monitor incidents");
    const link = basis(w).get('[data-testid="link"]');
    expect(link.text()).toBe("project default");
    expect(JSON.parse(link.attributes("data-to")!)).toEqual({ name: "settings", query: { tab: "incidents" } });
  });

  it("manual and alertmanager name their source", async () => {
    let w = mountDetail({ ...BASE_INCIDENT, impact: "minor", source: "manual", impact_basis: { source: "manual" } });
    await flushPromises();
    expect(basis(w).text()).toBe("Minor impact — set by the author when the incident was created");
    w.unmount();

    w = mountDetail({ ...BASE_INCIDENT, impact: "critical", source: "api", impact_basis: { source: "alertmanager" } });
    await flushPromises();
    expect(basis(w).text()).toBe("Critical impact — from the alert's severity label");
  });

  it("null says the basis was never recorded", async () => {
    const w = mountDetail({ ...BASE_INCIDENT, impact_basis: null });
    await flushPromises();
    expect(basis(w).text()).toBe("Major impact — basis not recorded (opened before impact classification)");
    expect(basis(w).classes()).not.toContain("border-dashed");
  });

  it("null with impact_basis_unavailable says the read failed, in a dashed outline", async () => {
    const w = mountDetail({ ...BASE_INCIDENT, impact_basis: null, impact_basis_unavailable: true });
    await flushPromises();
    expect(basis(w).text()).toBe("Major impact — basis unavailable");
    expect(basis(w).classes()).toContain("border-dashed");
  });

  it("sits under the meta row, above the impact-link chips", async () => {
    const w = mountDetail({ ...BASE_INCIDENT, impact_basis: { source: "project_default" }, impacts: [LINK] });
    await flushPromises();
    const html = w.html();
    expect(html.indexOf("started")).toBeLessThan(html.indexOf('data-testid="incident-impact-basis"'));
    expect(html.indexOf('data-testid="incident-impact-basis"')).toBeLessThan(html.indexOf('data-testid="incident-impacts"'));
  });

  it("acknowledge() keeps impact_basis — the line does not change after a click (§4.6)", async () => {
    const w = mountDetail({
      ...BASE_INCIDENT,
      service_id: "svc1",
      impact_basis: { source: "service_mapping", mapping: MAPPING, state: "down", service: { name: "Checkout", slug: "checkout" } },
    });
    await flushPromises();
    const before = basis(w).text();
    await w.findAll("button").find((b) => b.text() === "Acknowledge")!.trigger("click");
    await flushPromises();
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    expect(w.text()).toContain("acknowledged");
    expect(basis(w).text()).toBe(before);
  });

  it("acknowledge() keeps impact_basis_unavailable", async () => {
    const w = mountDetail({ ...BASE_INCIDENT, impact_basis: null, impact_basis_unavailable: true });
    await flushPromises();
    await w.findAll("button").find((b) => b.text() === "Acknowledge")!.trigger("click");
    await flushPromises();
    expect(basis(w).text()).toBe("Major impact — basis unavailable");
  });

  it("prints impact none as 'Not specified' on the badge", async () => {
    const w = mountDetail({ ...BASE_INCIDENT, impact: "none", impact_basis: null });
    await flushPromises();
    const badge = w.get('[data-testid="incident-impact-badge"]');
    expect(badge.text()).toBe("Not specified");
    expect(w.text()).not.toMatch(/\bNone\b/);
  });

  it("renders an Impact raised note like the other system notes", async () => {
    const w = mountDetail({ ...BASE_INCIDENT, impact_basis: null }, [
      {
        id: "u1",
        status: "investigating",
        body: "⬆ Impact raised: none → minor — the service is degraded.",
        created_at: "2026-08-17T10:30:00Z",
      },
    ]);
    await flushPromises();
    const note = w.get('[data-testid="incident-update-system-note"]');
    expect(note.attributes("data-note")).toBe("impact_raised");
    expect(note.find("p.font-mono").text()).toBe("⬆ Impact raised: none → minor — the service is degraded.");
  });
});
