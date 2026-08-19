import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import InstanceAuditPanel from "@/components/settings/InstanceAuditPanel.vue";

// The instance audit panel (iter-0155, mock approved by the owner). What is worth pinning is not the
// markup — that is AuditRows, shared with the org trail on purpose — but the four judgements the mock
// records: it reads the ADMIN endpoint (never the org listing with a wider filter), an unresolvable
// actor reads as "machine" instead of borrowing a human's name, a failed read says so instead of
// rendering an empty history, and the window widens on demand rather than cutting silently at 30.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const entry = (over: Record<string, unknown> = {}) => ({
  id: "a1",
  org_id: "",
  action: "user.global_admin",
  target: "ada@example.com",
  actor_name: "Grace Hopper",
  via_token: false,
  created_at: new Date().toISOString(),
  ...over,
});

beforeEach(() => {
  apiMock.GET.mockReset();
});

describe("InstanceAuditPanel", () => {
  it("reads the instance endpoint and never an org listing", async () => {
    apiMock.GET.mockResolvedValue({ data: [entry()] });
    const w = mount(InstanceAuditPanel);
    await flushPromises();

    const paths = apiMock.GET.mock.calls.map((c) => c[0]);
    expect(paths).toContain("/api/v1/admin/audit");
    expect(paths.some((p: string) => p.includes("organizations"))).toBe(false);
    expect(w.findAll('[data-testid="audit-row"]')).toHaveLength(1);
    expect(w.text()).toContain("granted or revoked global admin");
    expect(w.find('[data-testid="instance-chip"]').text()).toBe("instance");
  });

  it("renders an unresolvable actor as machine, not as a borrowed name", async () => {
    apiMock.GET.mockResolvedValue({
      data: [entry({ id: "a2", action: "outbox.replay", target: "e-4471", actor_name: "", actor_email: "" })],
    });
    const w = mount(InstanceAuditPanel);
    await flushPromises();

    const row = w.find('[data-testid="audit-row"]').text();
    expect(row).toContain("machine");
    expect(row).toContain("replayed a dead outbox event");
  });

  it("states a failed read instead of showing an empty history", async () => {
    apiMock.GET.mockResolvedValue({ error: { error: "forbidden" } });
    const w = mount(InstanceAuditPanel);
    await flushPromises();

    expect(w.find('[data-testid="instance-audit-error"]').exists()).toBe(true);
    // An empty trail and an unreadable one are different facts and must not share a rendering.
    expect(w.find('[data-testid="audit-empty"]').exists()).toBe(false);
  });

  it("says the trail is empty when it is", async () => {
    apiMock.GET.mockResolvedValue({ data: [] });
    const w = mount(InstanceAuditPanel);
    await flushPromises();

    expect(w.find('[data-testid="audit-empty"]').text()).toContain("No instance-level actions recorded yet.");
    expect(w.find('[data-testid="audit-more"]').exists()).toBe(false);
  });

  it("widens the window on demand instead of cutting at 30", async () => {
    apiMock.GET.mockResolvedValue({ data: Array.from({ length: 30 }, (_, i) => entry({ id: `a${i}` })) });
    const w = mount(InstanceAuditPanel);
    await flushPromises();

    const first = apiMock.GET.mock.calls[0][1].params.query.limit;
    expect(first).toBe(30);
    expect(w.find('[data-testid="audit-more"]').exists()).toBe(true);

    await w.find('[data-testid="audit-more"]').trigger("click");
    await flushPromises();
    expect(apiMock.GET.mock.calls[1][1].params.query.limit).toBe(100);
  });
});
