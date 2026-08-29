import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { reactive } from "vue";

import GateDecisionView from "@/views/GateDecisionView.vue";

// FR-024 D-0207 item 1, mock screen 4 "one decision, by id": presence says what happened (D7). A
// row renders ONLY when its field is present; `service_id: null` is "the referent is gone", not
// "never applied"; the raw record keeps the two apart exactly as the server sent them.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
const routeMock = vi.hoisted(() => ({ route: { params: { id: "d1" } as Record<string, string>, query: {} } }));
vi.mock("vue-router", () => ({
  useRoute: () => routeMock.route,
  useRouter: () => ({ push: vi.fn() }),
  RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API" }),
}));

type Res = { data?: unknown; error?: unknown; response?: Response };
const ok = (data: unknown): Res => ({ data, response: new Response(null, { status: 200 }) });
const refused = (status: number, code: string): Res => ({ error: { error: code }, response: new Response(null, { status }) });

const NOT_CONFIGURED = {
  schema_version: 1,
  decision_id: "d1",
  evaluated_at: "2026-08-29T14:03:02Z",
  service_id: "s1",
  service_slug: "checkout",
  service_name: "Checkout",
  state: "NOT_CONFIGURED",
  reasons: [{ code: "not_configured", docs: "https://docs.example/gate" }],
};
const FULL = {
  ...NOT_CONFIGURED,
  state: "BLOCK",
  action: "ALLOW",
  unoverridden_action: "BLOCK",
  policy_revision: 3,
  window: "30d",
  unknown_behavior: "warn",
  max_seal_lag_seconds: 900,
  override_id: "ov1",
  override: { id: "ov1", actor_label: "alice@example.com", reason: "hotfix", expires_at: "2026-08-30T10:00:00Z" },
  target_id: "t1",
  objective: 99.9,
  sealed_through: "2026-08-29T14:00:00Z",
  seal_lag: 182,
  facts_fresh_until: "2026-08-29T14:10:00Z",
  reasons: [
    { code: "service_incident_open", clause: "service_incident_open", assignment: "block", value: "inc-1", source: "incidents" },
    { code: "seal_stale", clause: "budget_exhausted", assignment: "block" },
  ],
  burn_leases: [{ rule_key: "30d/page/fast", severity: "page", firing: false, last_verdict: "ok", evaluated_at: "2026-08-29T14:02:00Z", lease_until: "2026-08-29T14:07:00Z", fresh: true }],
  coverage_state: { live: { armed: true }, burn: { armed: false, reason: "held" } },
};

function mountView(answer: Res | Promise<Res>, id = "d1") {
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/gate/decisions/{decisionID}")) return Promise.resolve(answer);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
  routeMock.route = reactive({ params: { id }, query: {} });
  return mount(GateDecisionView, {
    global: { stubs: { RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' } } },
  });
}
async function settle() {
  await flushPromises();
  await flushPromises();
}
type W = ReturnType<typeof mountView>;
const t = (w: W, id: string) => w.find(`[data-testid="${id}"]`);
const has = (w: W, id: string) => t(w, id).exists();

beforeEach(() => {
  apiMock.POST.mockReset();
});

describe("GateDecisionView", () => {
  it("reads the record by id under the project, and never writes", async () => {
    const w = mountView(ok(FULL));
    await settle();
    expect(apiMock.GET).toHaveBeenCalledTimes(1);
    const [, req] = apiMock.GET.mock.calls[0];
    expect(req.params.path).toEqual({ projectID: "p1", decisionID: "d1" });
    expect(req.signal).toBeInstanceOf(AbortSignal);
    expect(apiMock.POST).not.toHaveBeenCalled();
    expect(t(w, "gate-decision-state").text()).toBe("BLOCK");
    expect(t(w, "gate-decision-back").attributes("data-to")).toContain("gate-decisions");
  });

  it("NOT_CONFIGURED: no action, policy or override row — presence is the contract (D7)", async () => {
    const w = mountView(ok(NOT_CONFIGURED));
    await settle();
    expect(t(w, "gate-decision-state").text()).toBe("not configured");
    expect(has(w, "gate-decision-action")).toBe(false);
    expect(has(w, "gate-decision-override")).toBe(false);
    expect(has(w, "gate-decision-override-applied")).toBe(false);
    const kv = t(w, "gate-decision-kv").text();
    expect(kv).not.toContain("Policy");
    expect(kv).not.toContain("Action");
    expect(kv).not.toContain("Window");
    expect(kv).not.toContain("null");
    expect(has(w, "gate-decision-burn-leases")).toBe(false);
    expect(has(w, "gate-decision-service-link"), "the service exists: it is a link").toBe(true);
    expect(has(w, "gate-decision-service-deleted")).toBe(false);
    const reasons = w.findAll('[data-testid="gate-reason-chip"]');
    expect(reasons).toHaveLength(1);
    expect(reasons[0].attributes("data-code")).toBe("not_configured");
    expect(reasons[0].find("a").attributes("href")).toBe("https://docs.example/gate");
    // The raw record is the server's own shape: no `action` key at all.
    const json = JSON.parse(t(w, "gate-decision-json").text());
    expect(json).toEqual(NOT_CONFIGURED);
    expect("action" in json).toBe(false);
  });

  it("a full record: every carried row is present, the override chip, the reasons with their attrs, the raw JSON", async () => {
    const w = mountView(ok(FULL));
    await settle();
    expect(t(w, "gate-decision-action").text()).toBe("ALLOW");
    expect(t(w, "gate-decision-override-applied").text()).toBe("override applied → action ALLOW");
    const kv = t(w, "gate-decision-kv").text();
    expect(kv).toContain("Without the override");
    expect(kv).toContain("BLOCK");
    expect(kv).toContain("rev 3");
    expect(kv).toContain("30d");
    expect(kv).toContain("15m 0s");
    expect(kv).toContain("99.9%");
    expect(kv).toContain("Facts fresh until");
    expect(kv).toContain("live · armed");
    expect(kv).toContain("burn · not armed · held");
    const ov = t(w, "gate-decision-override");
    expect(ov.text()).toContain("alice@example.com");
    expect(ov.text()).toContain("hotfix");
    expect(ov.text()).toContain("2026-08-30 10:00:00Z");
    expect(has(w, "gate-decision-burn-leases")).toBe(true);
    expect(t(w, "gate-decision-burn-leases").text()).toContain("30d/page/fast");
    const reasons = w.findAll('[data-testid="gate-reason-chip"]');
    expect(reasons).toHaveLength(2);
    expect(reasons[0].attributes()).toMatchObject({ "data-code": "service_incident_open", "data-clause": "service_incident_open", "data-assignment": "block" });
    expect(reasons[0].text()).toContain("value inc-1");
    expect(reasons[0].text()).toContain("from incidents");
    expect(reasons[1].attributes()).toMatchObject({ "data-code": "seal_stale", "data-clause": "budget_exhausted" });
    expect(reasons[1].text()).toContain("clause budget_exhausted");
    expect(t(w, "gate-decision-json").text()).toContain('"decision_id": "d1"');
    expect(JSON.parse(t(w, "gate-decision-json").text())).toEqual(FULL);
  });

  it("service_id null: the row keeps the slug and wears the deleted chip; the raw record keeps the null", async () => {
    const w = mountView(ok({ ...FULL, service_id: null }));
    await settle();
    expect(has(w, "gate-decision-service-deleted")).toBe(true);
    expect(has(w, "gate-decision-service-link")).toBe(false);
    expect(t(w, "gate-decision-kv").text()).toContain("checkout");
    expect(t(w, "gate-decision-json").text()).toContain('"service_id": null');
  });

  it("404 is one sentence with its status; 400 is the server's message verbatim; a network failure its words", async () => {
    let w = mountView(refused(404, "not found"));
    await settle();
    expect(t(w, "gate-decision-error").text()).toBe("This decision does not exist in this project, or you cannot see it.");
    expect(t(w, "gate-decision-error").attributes("data-status")).toBe("404");
    expect(has(w, "gate-decision-json")).toBe(false);
    expect(has(w, "gate-decision-state")).toBe(false);

    w = mountView(refused(400, "decision id must be a UUID"), "nope");
    await settle();
    expect(t(w, "gate-decision-error").text()).toBe("decision id must be a UUID");

    w = mountView(Promise.reject(new Error("Failed to fetch")));
    await settle();
    expect(t(w, "gate-decision-error").text()).toBe("Could not reach the server: Failed to fetch");
    expect(t(w, "gate-decision-error").attributes("data-status")).toBe("0");
  });

  it("a route change re-reads under a new generation; the old read is aborted", async () => {
    const signals: AbortSignal[] = [];
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation((_p: string, opts: { params: { path: { decisionID: string } }; signal: AbortSignal }) => {
      signals.push(opts.signal);
      return opts.params.path.decisionID === "d1" ? new Promise(() => {}) : Promise.resolve(ok({ ...FULL, decision_id: "d2" }));
    });
    routeMock.route = reactive({ params: { id: "d1" }, query: {} });
    const w = mount(GateDecisionView, {
      global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
    });
    await settle();
    routeMock.route.params.id = "d2";
    await settle();
    expect(signals[0].aborted).toBe(true);
    expect(t(w, "gate-decision-json").text()).toContain('"decision_id": "d2"');
  });
});
