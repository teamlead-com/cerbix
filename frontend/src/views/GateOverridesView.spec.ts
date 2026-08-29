import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";
import { reactive } from "vue";

import GateOverridesView from "@/views/GateOverridesView.vue";

// FR-024 D-0207 item 1, mock screen 5: a service's override history, read-only. `status` is the
// server's function of facts (D13a) and is printed as given; the "Closed" cell reads per
// `revoked_reason`; there is deliberately no Revoke button here (revocation is by id, on the card).

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: { id: "svc1" }, query: {} }),
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

const SERVICE = ok({ service: { id: "svc1", slug: "checkout", name: "Checkout" }, sla_targets: [] });
const closed = { revoked_at: "2026-08-29T12:00:00Z", revoked_by_user_id: null, revoked_via_token: false };
function record(id: string, over: Record<string, unknown>) {
  return {
    id,
    reason: `reason ${id}`,
    expires_at: "2026-08-30T10:00:00Z",
    created_at: "2026-08-29T10:00:00Z",
    policy_revision: 3,
    actor_label: "alice@example.com",
    actor_user_id: "u1",
    via_token: false,
    status: "active",
    revoked_at: null,
    revoked_reason: null,
    revoked_by_label: null,
    revoked_by_user_id: null,
    revoked_via_token: null,
    ...over,
  };
}

function mountView(service: Res, overrides: Res) {
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/gate/overrides")) return Promise.resolve(overrides);
    if (path.endsWith("/services/{serviceID}")) return Promise.resolve(service);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
  return mount(GateOverridesView, {
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

describe("GateOverridesView", () => {
  it("rows carry their status, and the Closed cell reads per revoked_reason", async () => {
    const w = mountView(
      SERVICE,
      ok({
        items: [
          record("ov-active", {}),
          record("ov-manual", { status: "revoked", revoked_reason: "manual", revoked_by_label: "bob@example.com", ...closed }),
          record("ov-expired", { status: "expired", revoked_reason: "expired", ...closed }),
          record("ov-changed", { status: "inert", revoked_reason: "policy_changed", ...closed }),
          record("ov-deleted", { status: "inert", revoked_reason: "policy_deleted", ...closed }),
          record("ov-token", { actor_label: "token:ci-deploy", actor_user_id: null, via_token: true }),
        ],
      }),
    );
    await settle();
    const [, req] = apiMock.GET.mock.calls.find((c) => String(c[0]).endsWith("/gate/overrides"))!;
    expect(req.params.path).toEqual({ projectID: "p1", serviceID: "svc1" });
    expect(req.signal).toBeInstanceOf(AbortSignal);
    expect(w.text()).toContain("Checkout · override history");
    expect(t(w, "gate-overrides-back").attributes("data-to")).toContain('"id":"svc1"');

    const rows = w.findAll('[data-testid="gate-override-row"]');
    expect(rows.map((r) => r.attributes("data-status"))).toEqual(["active", "revoked", "expired", "inert", "inert", "active"]);
    expect(rows.map((r) => r.attributes("data-id"))).toEqual(["ov-active", "ov-manual", "ov-expired", "ov-changed", "ov-deleted", "ov-token"]);
    const closedCell = (r: (typeof rows)[number]) => r.findAll("td")[5].text();
    expect(closedCell(rows[0]), "unclosed: the dash").toBe("—");
    expect(closedCell(rows[1])).toBe("2026-08-29 12:00:00Z · by bob@example.com");
    expect(closedCell(rows[2])).toBe("2026-08-29 12:00:00Z · expired");
    expect(closedCell(rows[3])).toBe("2026-08-29 12:00:00Z · policy changed");
    expect(closedCell(rows[4])).toBe("2026-08-29 12:00:00Z · policy deleted");
    expect(rows[1].text()).toContain("reason ov-manual");
    expect(rows[1].text()).toContain("rev 3");
    expect(rows[5].findAll("td")[2].attributes("title")).toBe("an API token");
    expect(rows[0].findAll("td")[2].attributes("title")).toBe("a user");
    expect(w.text(), "no Revoke button in the history").not.toContain("Revoke");
    expect(apiMock.DELETE).not.toHaveBeenCalled();
  });

  it("no override ever: the empty sentence", async () => {
    const w = mountView(SERVICE, ok({ items: [] }));
    await settle();
    expect(has(w, "gate-overrides-table")).toBe(false);
    expect(t(w, "gate-overrides-empty").text()).toContain("No override has ever been added");
  });

  it("404 on the service: one sentence with its status, no table", async () => {
    const w = mountView(refused(404, "not found"), ok({ items: [record("x", {})] }));
    await settle();
    expect(t(w, "gate-overrides-error").text()).toBe("This service does not exist, or you cannot see it.");
    expect(t(w, "gate-overrides-error").attributes("data-status")).toBe("404");
    expect(has(w, "gate-overrides-table")).toBe(false);
    expect(has(w, "gate-overrides-empty")).toBe(false);
  });

  it("403 on the overrides, and a network failure, render as themselves", async () => {
    let w = mountView(SERVICE, refused(403, "forbidden"));
    await settle();
    expect(t(w, "gate-overrides-error").text()).toBe("You cannot see this service's overrides.");
    expect(t(w, "gate-overrides-error").attributes("data-status")).toBe("403");
    w = mountView(SERVICE, Promise.reject(new Error("Failed to fetch")) as never);
    await settle();
    expect(t(w, "gate-overrides-error").text()).toBe("Could not reach the server: Failed to fetch");
  });

  it("unmount aborts the pending reads", async () => {
    const signals: AbortSignal[] = [];
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementation((_p: string, opts: { signal: AbortSignal }) => {
      signals.push(opts.signal);
      return new Promise(() => {});
    });
    const w = mount(GateOverridesView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
    await settle();
    expect(signals).toHaveLength(2);
    w.unmount();
    expect(signals.every((s) => s.aborted)).toBe(true);
    expect(reactive({}), "reactive import kept for parity with the other view specs").toBeTruthy();
  });
});
