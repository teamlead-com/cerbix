import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import ServiceGate from "@/components/ServiceGate.vue";
import { GATE_ERROR_TEXT, RANGE_TOO_WIDE_TEXT, SEAL_LAG_MIN_MESSAGE, THRESHOLD_MESSAGE, WINDOW_LOST_MESSAGE, cliCommand } from "@/lib/gate";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), PUT: vi.fn(), POST: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: {}, query: {} }),
  useRouter: () => ({ push: vi.fn() }),
  RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' },
}));

// FR-024 / D-0207: the `Release gate` card, against the approved mock and the reviewer's five checks.
//
//   1. the override controls exist ONLY on `canOverride` (session.canProjectAdmin's answer, passed
//      in by the view); an editor is told who can, a viewer sees no control at all;
//   2. the window selector offers the `slaTargets` inventory and nothing else; a stored window that
//      left the inventory renders readably and Save is refused;
//   3. the latest decision is TWO LEDGER READS — an explicit RFC3339 half-open 30-day range with
//      `limit: 1`, then the record by id — and the card never POSTs `…/gate`; every refusal renders;
//   4. the CLI command names canonical ids and the LITERAL `CERBIX_TOKEN=…`;
//   5. after a 409 the draft is preserved and every mutation is blocked until the explicit Reload —
//      Discard and the closed delete dialog do not unblock (P1 [86]); every read and write is
//      guarded by a generation and an AbortSignal, so an answer landing after unmount or a prop
//      change is never applied.

const RouterLinkStub = { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' };
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,3})?Z$/;
const DAY_MS = 86_400_000;
const PAST = "2026-08-20T10:00:00Z";
const FUTURE = new Date(Date.now() + 5 * 3600_000).toISOString();

const TARGETS = [
  { window: "7d", objective: 99.5, updated_at: PAST },
  { window: "30d", objective: 99.9, updated_at: PAST },
];
const CLAUSES = {
  budget_exhausted: "block",
  budget_consumed: "warn",
  page_burn_firing: "block",
  ticket_burn_firing: "warn",
  service_incident_open: "warn",
};
const POLICY = {
  schema_version: 1,
  window: "30d",
  clauses: CLAUSES,
  budget_consumed_percent: 90,
  max_seal_lag_seconds: 900,
  unknown_behavior: "warn",
  revision: 3,
  updated_at: PAST,
  updated_by: "alice@example.com",
};
const OVERRIDE = {
  id: "ov1",
  reason: "hotfix for the 14:00 release",
  expires_at: FUTURE,
  created_at: PAST,
  actor_label: "alice@example.com",
  policy_revision: 3,
};
const BUDGET_WARN = { code: "budget_consumed", clause: "budget_consumed", assignment: "warn", value: 96.4, source: "materializer" };
const INCIDENT_BLOCK = { code: "service_incident_open", clause: "service_incident_open", assignment: "block", value: "inc-1" };
const SUMMARY = {
  schema_version: 1,
  decision_id: "0191c2a4-7f3e-4c1b-9a2d-000000005b04",
  evaluated_at: "2026-08-29T14:03:02.417Z",
  service_id: "s1",
  service_slug: "checkout",
  service_name: "Checkout",
  state: "BLOCK",
  action: "BLOCK",
  reasons: [INCIDENT_BLOCK, BUDGET_WARN],
  policy_revision: 3,
};
const DECISION = {
  ...SUMMARY,
  window: "30d",
  unknown_behavior: "warn",
  max_seal_lag_seconds: 900,
  objective: 99.9,
  objective_updated_at: PAST,
  sealed_through: "2026-08-29T14:00:00Z",
  seal_lag: 182,
  facts_fresh_until: FUTURE,
};

type Res = { data?: unknown; error?: unknown; response?: Response };
type Answer = Res | Promise<Res> | ((opts: any) => Res | Promise<Res>);
const ok = (data: unknown, status = 200): Res => ({ data, response: new Response(null, { status }) });
const refused = (status: number, code: string, headers?: Record<string, string>): Res => ({
  error: { error: code },
  response: new Response(null, { status, headers }),
});
const notFound = (code: string) => refused(404, code);
const empty = () => ok({ items: [], next_cursor: null });
const listOf = (...items: unknown[]) => ok({ items, next_cursor: null });

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

interface Server {
  policy?: Answer;
  list?: Answer;
  record?: Answer;
  override?: Answer;
}

/** Answers by PATH — one answer for every endpoint would hand the ledger a policy and let assertions pass by accident. */
function serve(server: Server) {
  const pick = (a: Answer | undefined, fallback: Res, opts: unknown) => {
    if (a === undefined) return Promise.resolve(fallback);
    if (typeof a === "function") return Promise.resolve(a(opts));
    return Promise.resolve(a);
  };
  apiMock.GET.mockImplementation((path: string, opts: unknown) => {
    if (path.endsWith("/gate/policy")) return pick(server.policy, notFound("not_configured"), opts);
    if (path.endsWith("/gate/decisions")) return pick(server.list, empty(), opts);
    if (path.endsWith("/gate/decisions/{decisionID}")) return pick(server.record, notFound("not found"), opts);
    if (path.endsWith("/gate/override")) return pick(server.override, notFound("none_active"), opts);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function mountGate(props: Record<string, unknown> = {}) {
  return mount(ServiceGate, {
    props: {
      projectId: "p1",
      serviceId: "s1",
      serviceSlug: "checkout",
      slaTargets: TARGETS,
      managedBy: "",
      canPolicyWrite: true,
      canOverride: true,
      ...props,
    },
    global: { stubs: { RouterLink: RouterLinkStub } },
  });
}

async function settle() {
  await flushPromises();
  await flushPromises();
}

const t = (w: ReturnType<typeof mountGate>, id: string) => w.find(`[data-testid="${id}"]`);
const has = (w: ReturnType<typeof mountGate>, id: string) => t(w, id).exists();
const disabled = (w: ReturnType<typeof mountGate>, id: string) => t(w, id).attributes("disabled") !== undefined;
const value = (w: ReturnType<typeof mountGate>, id: string) => (t(w, id).element as HTMLInputElement).value;
const calls = (fn: ReturnType<typeof vi.fn>, suffix: string) => fn.mock.calls.filter((c) => String(c[0]).endsWith(suffix));

async function openEditor(w: ReturnType<typeof mountGate>) {
  await t(w, "gate-configure").trigger("click");
  await settle();
  expect(has(w, "gate-policy-form"), "the editor must be open").toBe(true);
}
async function submitPolicy(w: ReturnType<typeof mountGate>) {
  await t(w, "gate-policy-form").trigger("submit");
  await settle();
}

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.PUT.mockReset();
  apiMock.POST.mockReset();
  apiMock.DELETE.mockReset();
});

// Reviewer check 3, enforced over EVERY test in this file: the card never asks the gate.
afterEach(() => {
  const decides = apiMock.POST.mock.calls.filter((c) => /\/gate$/.test(String(c[0])));
  expect(decides, "the SPA never POSTs …/gate — a decision is the pipeline's to make").toEqual([]);
});

describe("ServiceGate — who sees which controls (check 1)", () => {
  it("viewer, unconfigured: the empty state without Configure; the inventory chips with their objectives", async () => {
    serve({});
    const w = mountGate({ canPolicyWrite: false, canOverride: false });
    await settle();
    expect(has(w, "gate-empty")).toBe(true);
    expect(t(w, "gate-state").text()).toBe("not configured");
    expect(t(w, "gate-policy-chip").text()).toBe("no policy");
    expect(has(w, "gate-configure"), "a viewer never sees a button that can only 403").toBe(false);
    const chips = w.findAll('[data-testid="gate-window-chip"]');
    expect(chips.map((c) => c.attributes("data-window"))).toEqual(["7d", "30d"]);
    expect(chips[1].text()).toBe("30d · 99.90 %");
    expect(has(w, "gate-override-panel"), "no policy: no override panel").toBe(false);
    expect(has(w, "gate-latest-empty")).toBe(true);
  });

  it("editor, unconfigured: Configure is present; with no target it is disabled and says why", async () => {
    serve({});
    const w = mountGate({ slaTargets: [] });
    await settle();
    expect(has(w, "gate-configure")).toBe(true);
    expect(disabled(w, "gate-configure")).toBe(true);
    expect(has(w, "gate-windows-none")).toBe(true);
    await w.setProps({ slaTargets: TARGETS });
    expect(disabled(w, "gate-configure"), "the inventory arrives: Configure enables").toBe(false);
  });

  it("viewer with a policy: the read-only rendering, no form, and no override control even with one active", async () => {
    serve({ policy: ok(POLICY), override: ok(OVERRIDE) });
    const w = mountGate({ canPolicyWrite: false, canOverride: false });
    await settle();
    expect(has(w, "gate-policy-readonly")).toBe(true);
    expect(t(w, "gate-readonly-window").text()).toBe("30d");
    expect(t(w, "gate-readonly-clause-budget_exhausted").attributes("data-assignment")).toBe("block");
    expect(has(w, "gate-policy-form")).toBe(false);
    expect(has(w, "gate-configure")).toBe(false);
    expect(has(w, "gate-delete")).toBe(false);
    expect(has(w, "gate-override-active"), "the active override is a fact every role may read").toBe(true);
    expect(has(w, "gate-override-revoke")).toBe(false);
    expect(has(w, "gate-override-form")).toBe(false);
    expect(has(w, "gate-override-create")).toBe(false);
    expect(has(w, "gate-override-absent"), "a viewer is not told who can — only an editor is").toBe(false);
  });

  it("editor who is not a project admin: the policy form, but no override control — told who can", async () => {
    serve({ policy: ok(POLICY), override: ok(OVERRIDE) });
    const w = mountGate({ canPolicyWrite: true, canOverride: false });
    await settle();
    expect(has(w, "gate-configure")).toBe(true);
    expect(has(w, "gate-override-absent")).toBe(true);
    expect(t(w, "gate-override-absent").text()).toContain("project admins");
    expect(has(w, "gate-override-form")).toBe(false);
    expect(has(w, "gate-override-create")).toBe(false);
    expect(has(w, "gate-override-revoke")).toBe(false);
  });

  it("project admin: the override controls are present", async () => {
    serve({ policy: ok(POLICY), override: ok(OVERRIDE) });
    const w = mountGate({ canPolicyWrite: true, canOverride: true });
    await settle();
    expect(has(w, "gate-override-form")).toBe(true);
    expect(has(w, "gate-override-create")).toBe(true);
    expect(has(w, "gate-override-revoke")).toBe(true);
    expect(has(w, "gate-override-absent")).toBe(false);
  });

  it("a file-managed service does NOT make the gate read-only (D13)", async () => {
    serve({ policy: ok(POLICY) });
    const w = mountGate({ managedBy: "payments-bundle" });
    await settle();
    expect(has(w, "gate-configure")).toBe(true);
    expect(w.text()).toContain("file-managed service");
  });
});

describe("ServiceGate — the editor and the inventory (check 2)", () => {
  it("is prefilled from the policy and offers ONLY the inventory's windows, with their objectives", async () => {
    serve({ policy: ok(POLICY) });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-policy-chip").text()).toBe("revision 3");
    await openEditor(w);
    const options = t(w, "gate-window").findAll("option");
    expect(options.map((o) => o.attributes("value"))).toEqual(["7d", "30d"]);
    expect(options[0].text()).toBe("7d · objective 99.50 %");
    expect(options[1].text()).toBe("30d · objective 99.90 %");
    expect(value(w, "gate-window")).toBe("30d");
    expect(value(w, "gate-seal-lag-minutes"), "900 s as whole minutes").toBe("15");
    expect(value(w, "gate-threshold")).toBe("90");
    expect(value(w, "gate-unknown-behavior")).toBe("warn");
    expect(t(w, "gate-clause-budget_exhausted-block").attributes("aria-pressed")).toBe("true");
    expect(t(w, "gate-clause-budget_consumed-warn").attributes("aria-pressed")).toBe("true");
    expect(t(w, "gate-clause-service_incident_open-block").attributes("aria-pressed")).toBe("false");
    expect(disabled(w, "gate-save"), "a valid prefilled draft may be saved").toBe(false);
  });

  it("a stored window that LEFT the inventory renders readably, disabled, and Save is refused until a window with a target is picked", async () => {
    serve({ policy: ok({ ...POLICY, window: "90d" }) });
    const w = mountGate({ slaTargets: [TARGETS[1]] });
    await settle();
    await openEditor(w);
    const options = t(w, "gate-window").findAll("option");
    expect(options.map((o) => o.attributes("value"))).toEqual(["30d", "90d"]);
    const lost = options.find((o) => o.attributes("value") === "90d")!;
    expect(lost.text()).toBe("90d · no target any more");
    expect(lost.attributes("disabled")).toBeDefined();
    expect(value(w, "gate-window"), "the field still READS the stored window").toBe("90d");
    expect(has(w, "gate-window-stale")).toBe(true);
    expect(t(w, "gate-window-stale").text()).toContain("window_target_missing");
    expect(t(w, "gate-field-error-window").text()).toBe(WINDOW_LOST_MESSAGE);
    expect(disabled(w, "gate-save")).toBe(true);
    await submitPolicy(w);
    expect(apiMock.PUT, "a refused draft never travels").not.toHaveBeenCalled();

    await t(w, "gate-window").setValue("30d");
    expect(has(w, "gate-field-error-window")).toBe(false);
    expect(has(w, "gate-window-stale")).toBe(false);
    expect(disabled(w, "gate-save")).toBe(false);
  });

  it("client validation mirrors the server: the seal-lag floor sentence, the threshold rule", async () => {
    serve({ policy: ok(POLICY) });
    const w = mountGate();
    await settle();
    await openEditor(w);
    await t(w, "gate-seal-lag-minutes").setValue("4");
    expect(t(w, "gate-field-error-seal-lag").text()).toBe(SEAL_LAG_MIN_MESSAGE);
    expect(disabled(w, "gate-save")).toBe(true);
    await t(w, "gate-seal-lag-minutes").setValue("15");
    expect(has(w, "gate-field-error-seal-lag")).toBe(false);
    await t(w, "gate-threshold").setValue("0");
    expect(t(w, "gate-field-error-threshold").text()).toBe(THRESHOLD_MESSAGE);
    expect(disabled(w, "gate-save")).toBe(true);
    await t(w, "gate-threshold").setValue("95");
    expect(has(w, "gate-field-error-threshold")).toBe(false);
    expect(disabled(w, "gate-save")).toBe(false);
    expect(apiMock.PUT).not.toHaveBeenCalled();
  });

  it("Save sends the WHOLE document with expected_revision = the revision the operator saw, then re-reads", async () => {
    serve({ policy: ok(POLICY) });
    apiMock.PUT.mockResolvedValue(ok({ revision: 4 }));
    const w = mountGate();
    await settle();
    await openEditor(w);
    const policyReadsBefore = calls(apiMock.GET, "/gate/policy").length;
    await t(w, "gate-threshold").setValue("95");
    await t(w, "gate-clause-service_incident_open-block").trigger("click");
    await t(w, "gate-unknown-behavior").setValue("block");
    await submitPolicy(w);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    const [path, req] = apiMock.PUT.mock.calls[0];
    expect(path).toMatch(/\/gate\/policy$/);
    expect(req.params.path).toEqual({ projectID: "p1", serviceID: "s1" });
    expect(req.body).toEqual({
      expected_revision: 3,
      schema_version: 1,
      window: "30d",
      clauses: { ...CLAUSES, service_incident_open: "block" },
      budget_consumed_percent: 95,
      max_seal_lag_seconds: 900,
      unknown_behavior: "block",
    });
    expect(req.signal, "a write carries its AbortSignal too").toBeInstanceOf(AbortSignal);
    expect(calls(apiMock.GET, "/gate/policy").length, "the echo is not trusted: the policy is re-read").toBeGreaterThan(policyReadsBefore);
    expect(has(w, "gate-policy-form"), "the editor closes on success").toBe(false);
  });

  it("creating: the template picks 30d from the inventory and expected_revision is null", async () => {
    serve({});
    apiMock.PUT.mockResolvedValue(ok({ revision: 1 }));
    const w = mountGate();
    await settle();
    await openEditor(w);
    expect(value(w, "gate-window")).toBe("30d");
    expect(value(w, "gate-threshold")).toBe("90");
    expect(value(w, "gate-seal-lag-minutes")).toBe("15");
    await submitPolicy(w);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(apiMock.PUT.mock.calls[0][1].body).toMatchObject({ expected_revision: null, window: "30d", clauses: CLAUSES });
  });

  it("creating with an inventory that has no 30d: the canonical first window", async () => {
    serve({});
    const w = mountGate({ slaTargets: [{ window: "90d", objective: 99, updated_at: PAST }, { window: "7d", objective: 99.5, updated_at: PAST }] });
    await settle();
    await openEditor(w);
    expect(value(w, "gate-window")).toBe("7d");
  });
});

describe("ServiceGate — the 409 discipline (check 5, P1 [86])", () => {
  it("P1 [86]: after a 409 the draft is preserved and EVERY mutation is blocked; Discard does NOT unblock; only Reload does, and the next Save carries the fresh revision", async () => {
    let served = POLICY;
    serve({ policy: () => ok(served), override: ok(OVERRIDE) });
    apiMock.PUT.mockResolvedValueOnce(refused(409, "revision_conflict")).mockResolvedValue(ok({ revision: 5 }));
    const w = mountGate();
    await settle();
    await openEditor(w);
    expect(disabled(w, "gate-override-revoke"), "before the 409 Revoke is live").toBe(false);
    await t(w, "gate-threshold").setValue("95");
    await submitPolicy(w);

    // The refusal, verbatim; the draft, preserved; every mutation, disabled.
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(t(w, "gate-policy-error").text()).toBe(GATE_ERROR_TEXT.revision_conflict);
    expect(t(w, "gate-policy-error").text()).toContain("changed while you were editing");
    expect(has(w, "gate-reload")).toBe(true);
    expect(value(w, "gate-threshold"), "what was typed is the operator's, and stays").toBe("95");
    expect(disabled(w, "gate-save")).toBe(true);
    expect(disabled(w, "gate-delete")).toBe(true);
    expect(disabled(w, "gate-override-create")).toBe(true);
    expect(disabled(w, "gate-override-revoke")).toBe(true);
    // Attempting each mutation changes nothing on the wire.
    await submitPolicy(w);
    await t(w, "gate-save").trigger("click");
    await t(w, "gate-delete").trigger("click");
    await t(w, "gate-override-revoke").trigger("click");
    await t(w, "gate-override-form").trigger("submit");
    await settle();
    expect(has(w, "gate-delete-dialog"), "Delete does not even open its dialog while blocked").toBe(false);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(apiMock.DELETE).not.toHaveBeenCalled();
    expect(apiMock.POST).not.toHaveBeenCalled();

    // Discard: the editor closes, the banner and its Reload stay, every mutation stays blocked.
    await t(w, "gate-discard").trigger("click");
    await settle();
    expect(has(w, "gate-policy-form")).toBe(false);
    expect(has(w, "gate-policy-error"), "the banner survives Discard").toBe(true);
    expect(has(w, "gate-reload"), "the way out survives Discard").toBe(true);
    expect(disabled(w, "gate-configure"), "re-opening the editor from the stale policy is refused").toBe(true);
    expect(disabled(w, "gate-override-revoke")).toBe(true);
    expect(disabled(w, "gate-override-create")).toBe(true);
    await t(w, "gate-configure").trigger("click");
    await t(w, "gate-override-form").trigger("submit");
    await settle();
    expect(has(w, "gate-policy-form"), "Configure does nothing while blocked").toBe(false);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(apiMock.POST).not.toHaveBeenCalled();

    // Reload: the server's CURRENT policy is re-read, the block ends, and a Save rests on it.
    served = { ...POLICY, revision: 4, budget_consumed_percent: 80 };
    const readsBefore = calls(apiMock.GET, "/gate/policy").length;
    await t(w, "gate-reload").trigger("click");
    await settle();
    expect(calls(apiMock.GET, "/gate/policy").length, "Reload re-reads the policy").toBe(readsBefore + 1);
    expect(has(w, "gate-reload")).toBe(false);
    expect(has(w, "gate-policy-error")).toBe(false);
    expect(t(w, "gate-policy-chip").text()).toBe("revision 4");
    expect(disabled(w, "gate-configure")).toBe(false);
    expect(disabled(w, "gate-override-revoke")).toBe(false);
    await openEditor(w);
    expect(value(w, "gate-threshold"), "the reopened editor holds the server's policy, not the stale draft").toBe("80");
    expect(disabled(w, "gate-save")).toBe(false);
    await submitPolicy(w);
    expect(apiMock.PUT).toHaveBeenCalledTimes(2);
    expect(apiMock.PUT.mock.calls[1][1].body.expected_revision).toBe(4);
  });

  it("P1 [86]: a 409 with the editor left open — Reload re-prefills the open editor with the current policy and Save carries the fresh revision", async () => {
    let served = POLICY;
    serve({ policy: () => ok(served) });
    apiMock.PUT.mockResolvedValueOnce(refused(409, "revision_conflict")).mockResolvedValue(ok({ revision: 5 }));
    const w = mountGate({ canOverride: false });
    await settle();
    await openEditor(w);
    await t(w, "gate-threshold").setValue("95");
    await submitPolicy(w);
    expect(disabled(w, "gate-save")).toBe(true);
    served = { ...POLICY, revision: 4 };
    await t(w, "gate-reload").trigger("click");
    await settle();
    expect(has(w, "gate-policy-form"), "the editor stays open across Reload").toBe(true);
    expect(value(w, "gate-threshold"), "re-prefilled from the server").toBe("90");
    expect(disabled(w, "gate-save")).toBe(false);
    await t(w, "gate-threshold").setValue("95");
    await submitPolicy(w);
    expect(apiMock.PUT).toHaveBeenCalledTimes(2);
    expect(apiMock.PUT.mock.calls[1][1].body).toMatchObject({ expected_revision: 4, budget_consumed_percent: 95 });
    expect(has(w, "gate-policy-form")).toBe(false);
  });

  it("a failed Reload leaves no controls at all — nothing is mutable until a re-read succeeds", async () => {
    let policyAnswer: Res = ok(POLICY);
    serve({ policy: () => policyAnswer });
    apiMock.PUT.mockResolvedValue(refused(409, "revision_conflict"));
    const w = mountGate({ canOverride: false });
    await settle();
    await openEditor(w);
    await submitPolicy(w);
    expect(has(w, "gate-reload")).toBe(true);
    policyAnswer = refused(503, "unavailable");
    await t(w, "gate-reload").trigger("click");
    await settle();
    expect(has(w, "gate-unavailable"), "nothing re-read: one line, no controls").toBe(true);
    expect(has(w, "gate-save")).toBe(false);
    // The server comes back: the NEXT Reload is what ends the block, and only then.
    policyAnswer = ok({ ...POLICY, revision: 4 });
    await w.setProps({ serviceId: "s1" }); // no-op: the prop did not change, so nothing reloads by itself
    await settle();
    expect(has(w, "gate-unavailable")).toBe(true);
  });

  it("Delete: the dialog quotes the tombstone and the next revision, DELETE carries the revision seen, and the card re-reads to 'not configured'", async () => {
    let policyAnswer: Res = ok(POLICY);
    serve({ policy: () => policyAnswer });
    apiMock.DELETE.mockImplementation(async () => {
      policyAnswer = notFound("not_configured");
      return { data: undefined, response: new Response(null, { status: 204 }) };
    });
    const w = mountGate();
    await settle();
    await openEditor(w);
    await t(w, "gate-delete").trigger("click");
    await settle();
    const dialog = t(w, "gate-delete-dialog");
    expect(dialog.exists()).toBe(true);
    expect(dialog.text()).toContain("checkout");
    expect(dialog.text()).toContain("revision 4");
    expect(dialog.text()).toContain("revision 5");
    expect(dialog.text()).toContain("No override is active");
    expect(dialog.text()).toContain("NOT_CONFIGURED");
    await t(w, "gate-delete-confirm").trigger("click");
    await settle();
    expect(apiMock.DELETE).toHaveBeenCalledTimes(1);
    const [path, req] = apiMock.DELETE.mock.calls[0];
    expect(path).toMatch(/\/gate\/policy$/);
    expect(req.params.query).toEqual({ expected_revision: 3 });
    expect(has(w, "gate-delete-dialog")).toBe(false);
    expect(t(w, "gate-state").text()).toBe("not configured");
    expect(has(w, "gate-empty")).toBe(true);
  });

  it("P1 [86]: a 409 inside the delete dialog keeps blocking after the dialog is closed, until Reload", async () => {
    let served = POLICY;
    serve({ policy: () => ok(served), override: ok(OVERRIDE) });
    apiMock.DELETE.mockResolvedValue(refused(409, "revision_conflict"));
    const w = mountGate();
    await settle();
    await openEditor(w);
    await t(w, "gate-delete").trigger("click");
    await settle();
    await t(w, "gate-delete-confirm").trigger("click");
    await settle();
    expect(apiMock.DELETE).toHaveBeenCalledTimes(1);
    expect(apiMock.DELETE.mock.calls[0][1].params.query).toEqual({ expected_revision: 3 });
    expect(t(w, "gate-delete-error").text()).toContain("changed while this dialog was open");
    expect(w.findAll('[data-testid="gate-reload"]').length, "exactly one Reload on screen: the dialog's").toBe(1);
    expect(disabled(w, "gate-delete-confirm")).toBe(true);

    // Close the dialog: the block and the way out both stay.
    await t(w, "gate-delete-cancel").trigger("click");
    await settle();
    expect(has(w, "gate-delete-dialog")).toBe(false);
    expect(t(w, "gate-policy-error").text()).toContain("changed while this dialog was open");
    expect(w.findAll('[data-testid="gate-reload"]').length, "the Reload moved to the card's banner").toBe(1);
    expect(disabled(w, "gate-save")).toBe(true);
    expect(disabled(w, "gate-delete")).toBe(true);
    expect(disabled(w, "gate-override-revoke")).toBe(true);
    expect(disabled(w, "gate-override-create")).toBe(true);
    await submitPolicy(w);
    await t(w, "gate-delete").trigger("click");
    await settle();
    expect(has(w, "gate-delete-dialog")).toBe(false);
    expect(apiMock.PUT).not.toHaveBeenCalled();
    expect(apiMock.DELETE).toHaveBeenCalledTimes(1);

    // Discard from here keeps it too.
    await t(w, "gate-discard").trigger("click");
    await settle();
    expect(has(w, "gate-reload")).toBe(true);
    expect(disabled(w, "gate-configure")).toBe(true);

    served = { ...POLICY, revision: 4 };
    await t(w, "gate-reload").trigger("click");
    await settle();
    expect(has(w, "gate-reload")).toBe(false);
    expect(has(w, "gate-policy-error")).toBe(false);
    expect(disabled(w, "gate-configure")).toBe(false);
    expect(disabled(w, "gate-override-revoke")).toBe(false);
    await openEditor(w);
    expect(disabled(w, "gate-delete")).toBe(false);
    apiMock.PUT.mockResolvedValue(ok({ revision: 5 }));
    await submitPolicy(w);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(apiMock.PUT.mock.calls[0][1].body.expected_revision).toBe(4);
  });

  it("a non-409 refusal on Save is shown verbatim and does NOT block — Discard clears it", async () => {
    serve({ policy: ok(POLICY) });
    apiMock.PUT.mockResolvedValue(refused(400, "clauses.budget_consumed: unknown assignment"));
    const w = mountGate({ canOverride: false });
    await settle();
    await openEditor(w);
    await submitPolicy(w);
    expect(t(w, "gate-policy-error").text()).toBe("clauses.budget_consumed: unknown assignment");
    expect(has(w, "gate-reload")).toBe(false);
    expect(disabled(w, "gate-save"), "no conflict: Save stays live").toBe(false);
    await t(w, "gate-discard").trigger("click");
    await settle();
    expect(has(w, "gate-policy-error")).toBe(false);
  });
});

describe("ServiceGate — the latest decision is the ledger, never the gate (check 3)", () => {
  it("reads the newest row over an explicit RFC3339 half-open 30-day range with limit 1, then the record by id", async () => {
    serve({ policy: ok(POLICY), list: listOf(SUMMARY), record: ok(DECISION) });
    const w = mountGate();
    await settle();
    const list = calls(apiMock.GET, "/gate/decisions");
    expect(list).toHaveLength(1);
    const q = list[0][1].params.query;
    expect(q.from).toMatch(RFC3339);
    expect(q.to).toMatch(RFC3339);
    expect(Date.parse(q.to) - Date.parse(q.from)).toBe(30 * DAY_MS);
    expect(Date.parse(q.to)).toBeLessThanOrEqual(Date.now());
    expect(q.service_id).toBe("s1");
    expect(q.limit).toBe(1);
    expect(list[0][1].params.path).toEqual({ projectID: "p1" });
    expect(list[0][1].signal).toBeInstanceOf(AbortSignal);
    const record = calls(apiMock.GET, "/gate/decisions/{decisionID}");
    expect(record).toHaveLength(1);
    expect(record[0][1].params.path).toEqual({ projectID: "p1", decisionID: SUMMARY.decision_id });
    expect(apiMock.POST).not.toHaveBeenCalled();
    expect(t(w, "gate-latest-state").text()).toBe("BLOCK");
    expect(t(w, "gate-state").text(), "the header pill is the latest decision's state").toBe("BLOCK");
  });

  it("renders the record: the id, the revision, the seal lag within bound, fresh-until ahead, the budget the decision quoted, the reasons", async () => {
    serve({ policy: ok(POLICY), list: listOf(SUMMARY), record: ok(DECISION) });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-id").text()).toBe("0191c2a4…5b04");
    expect(t(w, "gate-latest-id").attributes("title")).toBe(SUMMARY.decision_id);
    expect(t(w, "gate-latest-revision").text()).toBe("policy rev 3");
    expect(t(w, "gate-latest-evaluated").text()).toBe("2026-08-29 14:03:02.417Z");
    expect(has(w, "gate-latest-action"), "state === action and no override: no chip").toBe(false);
    expect(t(w, "gate-latest-seal-lag").attributes("data-stale")).toBe("false");
    expect(t(w, "gate-latest-seal-lag").text()).toContain("seal lag 3m 02s");
    expect(t(w, "gate-latest-seal-lag").text()).toContain("of 15m allowed");
    expect(t(w, "gate-latest-fresh-until").attributes("data-past")).toBe("false");
    expect(t(w, "gate-latest-window").text()).toContain("30d · objective 99.90 %");
    expect(t(w, "gate-latest-budget").text().replace(/\s+/g, " ")).toMatch(/^96\.4 % ?burned · 3\.6 % remaining$/);
    expect(has(w, "gate-latest-budget-withheld")).toBe(false);
    expect(has(w, "gate-latest-never-sealed")).toBe(false);
    expect(has(w, "gate-latest-override")).toBe(false);
    const rows = w.findAll('[data-testid="gate-reason"]');
    expect(rows).toHaveLength(2);
    expect(rows[0].attributes()).toMatchObject({ "data-code": "service_incident_open", "data-clause": "service_incident_open", "data-assignment": "block" });
    expect(rows[0].text()).toContain("incident inc-1");
    expect(rows[1].attributes()).toMatchObject({ "data-code": "budget_consumed", "data-clause": "budget_consumed", "data-assignment": "warn" });
    expect(rows[1].text()).toContain("96.4 % burned");
    expect(rows[1].text()).toContain("in revision 3");
    expect(has(w, "gate-latest-stale-warning")).toBe(false);
  });

  it("an override applied: the action chip, and the info naming the unoverridden_action", async () => {
    const overridden = {
      ...DECISION,
      action: "ALLOW",
      unoverridden_action: "BLOCK",
      override_id: "ov1",
      override: { id: "ov1", actor_label: "alice@example.com", reason: "hotfix", expires_at: FUTURE },
    };
    serve({ policy: ok(POLICY), list: listOf({ ...SUMMARY, action: "ALLOW", override_id: "ov1" }), record: ok(overridden), override: ok(OVERRIDE) });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-state").text(), "the state is what was OBSERVED").toBe("BLOCK");
    expect(t(w, "gate-latest-action").text()).toBe("override applied → action ALLOW");
    expect(has(w, "gate-latest-override")).toBe(true);
    expect(t(w, "gate-latest-unoverridden").text()).toBe("BLOCK");
    expect(t(w, "gate-latest-override").text()).toContain("unoverridden_action");
    expect(t(w, "gate-latest-override").text()).toContain("alice@example.com");
    expect(t(w, "gate-latest-override").text()).toContain("hotfix");
  });

  it("UNKNOWN on a fresh service: never sealed, an action that differs from the state carries its exit code", async () => {
    const unknown = {
      ...DECISION,
      state: "UNKNOWN",
      action: "BLOCK",
      unknown_behavior: "block",
      reasons: [{ code: "never_sealed", clause: "budget_exhausted", assignment: "block" }],
      sealed_through: undefined,
      seal_lag: undefined,
      facts_fresh_until: undefined,
    };
    delete (unknown as any).sealed_through;
    delete (unknown as any).seal_lag;
    delete (unknown as any).facts_fresh_until;
    serve({ policy: ok(POLICY), list: listOf({ ...SUMMARY, state: "UNKNOWN", reasons: unknown.reasons }), record: ok(unknown) });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-state").text()).toBe("UNKNOWN");
    expect(t(w, "gate-latest-action").text()).toBe("action BLOCK · exit 2");
    expect(has(w, "gate-latest-never-sealed")).toBe(true);
    expect(has(w, "gate-latest-seal-lag")).toBe(false);
    expect(has(w, "gate-latest-fresh-until")).toBe(false);
    const row = t(w, "gate-reason");
    expect(row.attributes("data-code")).toBe("never_sealed");
    expect(row.attributes("data-clause")).toBe("budget_exhausted");
    expect(row.text()).toContain("unavailable");
    expect(row.text()).toContain("assigned block");
    // A budget clause is PRESENT but unavailable: the KPI is withheld, never invented.
    expect(has(w, "gate-latest-budget")).toBe(false);
    expect(t(w, "gate-latest-budget-withheld").text()).toContain("withheld · never_sealed");
  });

  it("seal_stale: the lag over the bound is marked stale, the budget is withheld, the warning names the lag", async () => {
    const stale = {
      ...DECISION,
      state: "UNKNOWN",
      action: "WARN",
      seal_lag: 1500,
      reasons: [{ code: "seal_stale", clause: "budget_exhausted", assignment: "block" }, { code: "seal_stale", clause: "budget_consumed", assignment: "warn" }],
      facts_fresh_until: "2026-08-29T14:10:00Z",
    };
    serve({ policy: ok(POLICY), list: listOf({ ...SUMMARY, state: "UNKNOWN", action: "WARN" }), record: ok(stale) });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-seal-lag").attributes("data-stale")).toBe("true");
    expect(t(w, "gate-latest-seal-lag").text()).toContain("over the 15m allowed");
    expect(t(w, "gate-latest-fresh-until").attributes("data-past"), "a fresh-until behind the clock").toBe("true");
    expect(t(w, "gate-latest-fresh-until").text()).toContain("already past");
    expect(t(w, "gate-latest-budget-withheld").text()).toContain("withheld · seal_stale");
    expect(has(w, "gate-latest-budget")).toBe(false);
    expect(t(w, "gate-latest-stale-warning").text()).toContain("Facts are 25m behind.");
    expect(t(w, "gate-latest-action").text()).toBe("action WARN · exit 0");
  });

  it("no budget clause among the reasons: no KPI at all; an empty reasons list says so", async () => {
    serve({ policy: ok(POLICY), list: listOf(SUMMARY), record: ok({ ...DECISION, reasons: [INCIDENT_BLOCK] }) });
    const w = mountGate();
    await settle();
    expect(has(w, "gate-latest-budget")).toBe(false);
    expect(has(w, "gate-latest-budget-withheld")).toBe(false);
    serve({ policy: ok(POLICY), list: listOf({ ...SUMMARY, state: "ALLOW", action: "ALLOW", reasons: [] }), record: ok({ ...DECISION, state: "ALLOW", action: "ALLOW", reasons: [] }) });
    await w.setProps({ serviceId: "s2" });
    await settle();
    expect(t(w, "gate-latest-state").text()).toBe("ALLOW");
    expect(has(w, "gate-latest-no-reasons")).toBe(true);
  });

  it("an empty ledger: 'no decision yet'; the header pill has nothing to say beyond the policy", async () => {
    serve({ policy: ok(POLICY), list: empty() });
    const w = mountGate();
    await settle();
    expect(has(w, "gate-latest-empty")).toBe(true);
    expect(t(w, "gate-latest-empty").text()).toContain("never asks the gate itself");
    expect(calls(apiMock.GET, "/gate/decisions/{decisionID}"), "no row: no record read").toHaveLength(0);
    expect(has(w, "gate-state"), "a policy without a decision: no state pill").toBe(false);
  });

  it("429 with Retry-After renders the seconds", async () => {
    serve({ policy: ok(POLICY), list: refused(429, "process_inflight", { "Retry-After": "7" }) });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-error").text()).toBe("The ledger is busy right now. Try again in 7 s.");
  });

  it("400 range_too_wide renders the mock's sentence", async () => {
    serve({ policy: ok(POLICY), list: refused(400, "range_too_wide") });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-error").text()).toBe(RANGE_TOO_WIDE_TEXT);
  });

  it("a network failure renders the transport's words verbatim", async () => {
    serve({ policy: ok(POLICY), list: () => Promise.reject(new Error("Failed to fetch")) });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-error").text()).toBe("Could not reach the server: Failed to fetch");
  });

  it("a record that failed to read after the row was found is its own error", async () => {
    serve({ policy: ok(POLICY), list: listOf(SUMMARY), record: refused(401, "unauthorized") });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-latest-error").text()).toBe("Your session has ended — sign in again.");
  });

  it("a 403 on the policy: one line, no controls, and the ledger is not even asked", async () => {
    serve({ policy: refused(403, "forbidden") });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-unavailable").text()).toBe("You cannot read this service's gate.");
    expect(has(w, "gate-empty")).toBe(false);
    expect(has(w, "gate-latest")).toBe(false);
    expect(has(w, "gate-configure")).toBe(false);
    expect(calls(apiMock.GET, "/gate/decisions")).toHaveLength(0);
    expect(calls(apiMock.GET, "/gate/override")).toHaveLength(0);
  });

  it("a 401 on the policy, and a network failure on it, render the same way", async () => {
    serve({ policy: refused(401, "unauthorized") });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-unavailable").text()).toBe("Your session has ended — sign in again.");
    serve({ policy: () => Promise.reject(new Error("ECONNRESET")) });
    await w.setProps({ serviceId: "s2" });
    await settle();
    expect(t(w, "gate-unavailable").text()).toBe("Could not reach the server: ECONNRESET");
  });
});

describe("ServiceGate — the override panel", () => {
  it("the active override renders its facts; Revoke asks first, then DELETEs by the override's OWN id and re-reads", async () => {
    let active: Res = ok(OVERRIDE);
    serve({ policy: ok(POLICY), override: () => active });
    apiMock.DELETE.mockImplementation(async () => {
      active = notFound("none_active");
      return { data: undefined, response: new Response(null, { status: 204 }) };
    });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-override-reason").text()).toBe(OVERRIDE.reason);
    expect(t(w, "gate-override-actor").text()).toBe("alice@example.com");
    expect(t(w, "gate-override-created").text()).toContain("2026-08-20 10:00:00Z");
    expect(t(w, "gate-override-expires").text()).toMatch(/· in [45]h/);
    expect(has(w, "gate-override-blocked")).toBe(true);
    expect(disabled(w, "gate-override-create"), "one at a time: Create is disabled while one is active").toBe(true);

    await t(w, "gate-override-revoke").trigger("click");
    await settle();
    expect(apiMock.DELETE, "asking is not doing").not.toHaveBeenCalled();
    expect(has(w, "gate-override-revoke-confirm")).toBe(true);
    await t(w, "gate-override-revoke-cancel").trigger("click");
    await settle();
    expect(has(w, "gate-override-revoke-confirm")).toBe(false);
    await t(w, "gate-override-revoke").trigger("click");
    await t(w, "gate-override-revoke-confirm").trigger("click");
    await settle();
    expect(apiMock.DELETE).toHaveBeenCalledTimes(1);
    const [path, req] = apiMock.DELETE.mock.calls[0];
    expect(path).toMatch(/\/gate\/overrides\/\{overrideID\}$/);
    expect(req.params.path).toEqual({ projectID: "p1", serviceID: "s1", overrideID: "ov1" });
    expect(req.signal).toBeInstanceOf(AbortSignal);
    expect(has(w, "gate-override-active"), "re-read: the slot is released").toBe(false);
    expect(has(w, "gate-override-blocked")).toBe(false);
  });

  it("409 override_not_active on revoke: the sentence, Reload, every mutation blocked", async () => {
    serve({ policy: ok(POLICY), override: ok(OVERRIDE) });
    apiMock.DELETE.mockResolvedValue(refused(409, "override_not_active"));
    const w = mountGate();
    await settle();
    await t(w, "gate-override-revoke").trigger("click");
    await t(w, "gate-override-revoke-confirm").trigger("click");
    await settle();
    expect(t(w, "gate-override-error").text()).toBe(GATE_ERROR_TEXT.override_not_active);
    expect(has(w, "gate-reload")).toBe(true);
    expect(disabled(w, "gate-override-revoke")).toBe(true);
    expect(disabled(w, "gate-override-create")).toBe(true);
    expect(disabled(w, "gate-configure")).toBe(true);
    await t(w, "gate-override-revoke").trigger("click");
    await settle();
    expect(apiMock.DELETE).toHaveBeenCalledTimes(1);
    serve({ policy: ok(POLICY), override: notFound("none_active") });
    await t(w, "gate-reload").trigger("click");
    await settle();
    expect(has(w, "gate-override-error")).toBe(false);
    expect(has(w, "gate-override-active")).toBe(false);
    expect(disabled(w, "gate-configure")).toBe(false);
  });

  it("Create: the reason and the expiry are validated on screen, then POST carries {policy_revision, reason, expires_at}", async () => {
    let active: Res = notFound("none_active");
    serve({ policy: ok(POLICY), override: () => active });
    apiMock.POST.mockImplementation(async () => {
      active = ok(OVERRIDE);
      return ok({ id: "ov1" }, 201);
    });
    const w = mountGate();
    await settle();
    expect(has(w, "gate-override-blocked")).toBe(false);
    expect(disabled(w, "gate-override-create"), "an empty reason cannot be sent").toBe(true);
    await t(w, "gate-override-form").trigger("submit");
    await settle();
    expect(apiMock.POST).not.toHaveBeenCalled();
    expect(t(w, "gate-override-field-error-reason").text()).toContain("A reason is required");

    await t(w, "gate-override-input-reason").setValue("  hotfix for the 14:00 release  ");
    expect(has(w, "gate-override-field-error-reason")).toBe(false);
    await t(w, "gate-override-input-until").setValue("2020-01-01T10:00");
    expect(t(w, "gate-override-field-error-until").text()).toBe("Pick a time in the future.");
    expect(disabled(w, "gate-override-create")).toBe(true);
    const until = new Date(Date.now() + 20 * 3600_000);
    const pad = (n: number) => String(n).padStart(2, "0");
    const local = `${until.getFullYear()}-${pad(until.getMonth() + 1)}-${pad(until.getDate())}T${pad(until.getHours())}:${pad(until.getMinutes())}`;
    await t(w, "gate-override-input-until").setValue(local);
    expect(has(w, "gate-override-field-error-until")).toBe(false);
    expect(disabled(w, "gate-override-create")).toBe(false);

    await t(w, "gate-override-form").trigger("submit");
    await settle();
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    const [path, req] = apiMock.POST.mock.calls[0];
    expect(path).toMatch(/\/gate\/override$/);
    expect(req.params.path).toEqual({ projectID: "p1", serviceID: "s1" });
    expect(req.body).toEqual({ policy_revision: 3, reason: "hotfix for the 14:00 release", expires_at: new Date(local).toISOString() });
    expect(req.signal).toBeInstanceOf(AbortSignal);
    expect(has(w, "gate-override-active"), "re-read after the write").toBe(true);
    expect(value(w, "gate-override-input-reason"), "the form resets after success").toBe("");
  });

  it("409 override_active on create: the sentence, Reload, Create blocked, the typed reason preserved", async () => {
    serve({ policy: ok(POLICY), override: notFound("none_active") });
    apiMock.POST.mockResolvedValue(refused(409, "override_active"));
    const w = mountGate();
    await settle();
    await t(w, "gate-override-input-reason").setValue("hotfix");
    await t(w, "gate-override-form").trigger("submit");
    await settle();
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    expect(t(w, "gate-override-error").text()).toBe(GATE_ERROR_TEXT.override_active);
    expect(has(w, "gate-reload")).toBe(true);
    expect(disabled(w, "gate-override-create")).toBe(true);
    expect(value(w, "gate-override-input-reason"), "the operator's words stay").toBe("hotfix");
    await t(w, "gate-override-form").trigger("submit");
    await settle();
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    serve({ policy: ok(POLICY), override: ok(OVERRIDE) });
    await t(w, "gate-reload").trigger("click");
    await settle();
    expect(has(w, "gate-override-error")).toBe(false);
    expect(has(w, "gate-override-active"), "Reload shows the override somebody else created").toBe(true);
    expect(value(w, "gate-override-input-reason"), "Reload keeps the operator's words").toBe("hotfix");
  });

  it("the override read failing is its own line; the panel is absent without a policy", async () => {
    serve({ policy: ok(POLICY), override: refused(503, "") });
    const w = mountGate();
    await settle();
    expect(t(w, "gate-override-read-error").text()).toBe("The server answered HTTP 503.");
    serve({});
    await w.setProps({ serviceId: "s2" });
    await settle();
    expect(has(w, "gate-override-panel")).toBe(false);
  });
});

describe("ServiceGate — the CLI card (check 4)", () => {
  it("prints the exact command for THIS project and service, by canonical id, with the literal token placeholder", async () => {
    serve({});
    const w = mountGate({ projectId: "6d1f0b1e-0000-4000-8000-000000000001", serviceId: "6d1f0b1e-0000-4000-8000-000000000002" });
    await settle();
    const text = t(w, "gate-cli-command").text();
    expect(text).toBe(cliCommand(window.location.origin, "6d1f0b1e-0000-4000-8000-000000000001", "6d1f0b1e-0000-4000-8000-000000000002"));
    expect(text).toBe(
      `CERBIX_URL=${window.location.origin} CERBIX_TOKEN=… cerbix gate check --project 6d1f0b1e-0000-4000-8000-000000000001 --service 6d1f0b1e-0000-4000-8000-000000000002`,
    );
    expect(text).toContain("CERBIX_TOKEN=… ");
    expect(text).not.toContain("checkout");
    expect(t(w, "gate-cli").text()).toContain("exit 4");
    await w.setProps({ serviceId: "6d1f0b1e-0000-4000-8000-000000000003" });
    await settle();
    expect(t(w, "gate-cli-command").text()).toContain("--service 6d1f0b1e-0000-4000-8000-000000000003");
  });
});

describe("ServiceGate — abort and generation guards (check 5)", () => {
  it("every read carries an AbortSignal; unmount aborts the pending one and its late answer is dropped without error", async () => {
    const pending = deferred<Res>();
    const signals: AbortSignal[] = [];
    serve({
      policy: (opts: { signal?: AbortSignal }) => {
        if (opts?.signal) signals.push(opts.signal);
        return pending.promise;
      },
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const warns = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const w = mountGate();
      await settle();
      expect(signals).toHaveLength(1);
      expect(signals[0].aborted).toBe(false);
      expect(has(w, "gate-loading")).toBe(true);
      w.unmount();
      expect(signals[0].aborted, "unmount aborts what is in flight").toBe(true);
      pending.resolve(ok(POLICY));
      await settle();
      expect(calls(apiMock.GET, "/gate/decisions"), "a dropped answer starts no dependent read").toHaveLength(0);
      expect(errors).not.toHaveBeenCalled();
      expect(warns).not.toHaveBeenCalled();
    } finally {
      errors.mockRestore();
      warns.mockRestore();
    }
  });

  it("a serviceId change while the policy read is pending drops the first answer — the DOM reflects the second", async () => {
    const first = deferred<Res>();
    const signals: Record<string, AbortSignal> = {};
    serve({
      policy: (opts: { params: { path: { serviceID: string } }; signal: AbortSignal }) => {
        const id = opts.params.path.serviceID;
        signals[id] = opts.signal;
        return id === "s1" ? first.promise : ok({ ...POLICY, revision: 7 });
      },
    });
    const w = mountGate();
    await settle();
    expect(has(w, "gate-loading")).toBe(true);
    await w.setProps({ serviceId: "s2" });
    await settle();
    expect(signals.s1.aborted, "the superseded read is aborted, not merely ignored").toBe(true);
    expect(signals.s2.aborted).toBe(false);
    expect(t(w, "gate-policy-chip").text()).toBe("revision 7");
    first.resolve(ok({ ...POLICY, revision: 1 }));
    await settle();
    expect(t(w, "gate-policy-chip").text(), "the late first answer never lands").toBe("revision 7");
    expect(calls(apiMock.GET, "/gate/override").every((c) => c[1].params.path.serviceID === "s2"), "no dependent read for the dropped context").toBe(true);
  });

  it("a late second-step record read (by id) after a prop change is dropped too", async () => {
    const record = deferred<Res>();
    serve({
      policy: ok(POLICY),
      list: (opts: { params: { query: { service_id: string } } }) => (opts.params.query.service_id === "s1" ? listOf(SUMMARY) : empty()),
      record: () => record.promise,
    });
    const w = mountGate();
    await settle();
    expect(has(w, "gate-latest-loading")).toBe(true);
    await w.setProps({ serviceId: "s2" });
    await settle();
    expect(has(w, "gate-latest-empty")).toBe(true);
    record.resolve(ok(DECISION));
    await settle();
    expect(has(w, "gate-latest-empty"), "s1's record must not appear on s2's card").toBe(true);
    expect(has(w, "gate-latest-state")).toBe(false);
  });

  it("a write landing after unmount is dropped without error", async () => {
    const put = deferred<Res>();
    serve({ policy: ok(POLICY) });
    let signal: AbortSignal | undefined;
    apiMock.PUT.mockImplementation((_p: string, req: { signal: AbortSignal }) => {
      signal = req.signal;
      return put.promise;
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const w = mountGate({ canOverride: false });
      await settle();
      await openEditor(w);
      await submitPolicy(w);
      expect(apiMock.PUT).toHaveBeenCalledTimes(1);
      expect(t(w, "gate-save").text()).toBe("Saving…");
      const readsBefore = calls(apiMock.GET, "/gate/policy").length;
      w.unmount();
      expect(signal!.aborted).toBe(true);
      put.resolve(ok({ revision: 4 }));
      await settle();
      expect(calls(apiMock.GET, "/gate/policy").length, "no re-read after the context is gone").toBe(readsBefore);
      expect(errors).not.toHaveBeenCalled();
    } finally {
      errors.mockRestore();
    }
  });

  it("a write landing after a serviceId change is dropped — the new context is untouched", async () => {
    const put = deferred<Res>();
    serve({ policy: (opts: { params: { path: { serviceID: string } } }) => ok({ ...POLICY, revision: opts.params.path.serviceID === "s1" ? 3 : 9 }) });
    apiMock.PUT.mockImplementation(() => put.promise);
    const w = mountGate({ canOverride: false });
    await settle();
    await openEditor(w);
    await submitPolicy(w);
    await w.setProps({ serviceId: "s2" });
    await settle();
    expect(t(w, "gate-policy-chip").text()).toBe("revision 9");
    expect(has(w, "gate-policy-form"), "a new context opens closed").toBe(false);
    put.resolve(refused(409, "revision_conflict"));
    await settle();
    expect(has(w, "gate-policy-error"), "s1's 409 must not block s2").toBe(false);
    expect(disabled(w, "gate-configure")).toBe(false);
  });
});
