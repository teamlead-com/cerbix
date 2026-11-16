import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import MonitorsView from "@/views/MonitorsView.vue";

// FR-043 §8, approved mock docs/design/mock-recovery-threshold.html §4, variant 2 (2026-11-16).
//
// A down monitor with a run of successes in progress keeps its Down pill — it IS down: notifications,
// the auto-incident, composites and dependency suppression all still treat it so — and gains a
// neutral qualifier chip "recovering N/M" beside it, in the same row of chips as "⏸ suppressed by".
// The chip is NOT gated on N < M: a threshold lowered during a run (D10) reads "3/2" until the next
// check. Today's "Confirming N/M…" on an up monitor stays exactly what it was.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: {}, query: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true, canOrgWrite: () => true }),
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", projectName: "Prod", projects: [] }),
}));

type Row = Record<string, unknown> & { id: string; name: string };

function base(id: string, name: string, extra: Record<string, unknown>): Row {
  return {
    id,
    name,
    project_id: "p1",
    type: "http",
    target: `https://${name}.example.internal/healthz`,
    enabled: true,
    interval_seconds: 60,
    failure_threshold: 1,
    recovery_threshold: 1,
    consecutive_failures: 0,
    consecutive_successes: 0,
    management: { source: "ui" },
    ...extra,
  };
}

// The mock's rows, plus the states that must carry no chip.
const ROWS: Row[] = [
  base("m1", "api-gateway", { status: "down", recovery_threshold: 3, consecutive_successes: 2 }),
  // Threshold lowered from 4 to 2 during a run of 3: still down until its next check (D10).
  base("m2", "checkout-api", { status: "down", recovery_threshold: 2, consecutive_successes: 3 }),
  base("m3", "search-indexer", { status: "down", recovery_threshold: 3, consecutive_successes: 1, depends_on: ["m1"] }),
  base("m4", "worker-health", { status: "down", recovery_threshold: 3, consecutive_successes: 0 }),
  base("m5", "orders-db", { status: "up", failure_threshold: 3, consecutive_failures: 1 }),
  // A non-zero counter on an up or pending monitor is never a run the list may announce.
  base("m6", "billing-up", { status: "up", recovery_threshold: 3, consecutive_successes: 2 }),
  base("m7", "fresh-pending", { status: "pending", recovery_threshold: 3, consecutive_successes: 1 }),
];

async function mountList(): Promise<VueWrapper> {
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/projects/{projectID}/monitors") return { data: ROWS };
    return { data: undefined };
  });
  const w = mount(MonitorsView, {
    global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
  });
  await flushPromises();
  return w;
}

function rowOf(w: VueWrapper, name: string) {
  const tr = w.findAll("tbody tr").find((r) => r.text().includes(name));
  expect(tr, `no row for ${name}`).toBeDefined();
  return tr!;
}

function pillText(w: VueWrapper, name: string): string {
  return rowOf(w, name).findComponent({ name: "StatusPill" }).text();
}

function chip(w: VueWrapper, name: string) {
  return rowOf(w, name).find('[data-testid="monitor-recovering"]');
}

describe("monitor list — recovering N/M (FR-043 §8, mock §4 variant 2)", () => {
  it("shows the chip beside a Down pill that keeps saying Down", async () => {
    const w = await mountList();
    expect(pillText(w, "api-gateway")).toBe("Down");
    const c = chip(w, "api-gateway");
    expect(c.exists(), "no recovering chip on a down monitor with a run").toBe(true);
    expect(c.text()).toBe("recovering 2/3");
    expect(c.attributes("title")).toBe(
      "2 of 3 consecutive successful checks. The monitor stays down until 3 in a row succeed. As of page load.",
    );
  });

  it("reads 3/2 after the threshold was lowered during the run — not hidden", async () => {
    const w = await mountList();
    expect(pillText(w, "checkout-api")).toBe("Down");
    const c = chip(w, "checkout-api");
    expect(c.exists(), "a run longer than the lowered threshold must still be shown").toBe(true);
    expect(c.text()).toBe("recovering 3/2");
    expect(c.attributes("title")).toBe(
      "3 consecutive successful checks; the threshold was lowered to 2 during the run, so the next successful check confirms the recovery. As of page load.",
    );
  });

  it("composes with the suppressed-by chip, recovering first", async () => {
    const w = await mountList();
    const row = rowOf(w, "search-indexer");
    expect(chip(w, "search-indexer").text()).toBe("recovering 1/3");
    expect(row.text()).toContain("⏸ suppressed by api-gateway");
    const text = row.text();
    expect(text.indexOf("recovering 1/3")).toBeLessThan(text.indexOf("suppressed by"));
  });

  it("shows no chip for a down monitor without a run, nor on up or pending", async () => {
    const w = await mountList();
    expect(pillText(w, "worker-health")).toBe("Down");
    expect(chip(w, "worker-health").exists()).toBe(false);
    expect(chip(w, "billing-up").exists()).toBe(false);
    expect(pillText(w, "billing-up")).toBe("Operational");
    expect(chip(w, "fresh-pending").exists()).toBe(false);
    expect(pillText(w, "fresh-pending")).toBe("Pending");
    expect(w.findAll('[data-testid="monitor-recovering"]')).toHaveLength(3);
  });

  it("leaves today's Confirming N/M… on an up monitor unchanged", async () => {
    const w = await mountList();
    expect(pillText(w, "orders-db")).toBe("Confirming 1/3…");
    const pill = rowOf(w, "orders-db").findComponent({ name: "StatusPill" });
    expect(pill.props("status")).toBe("degraded");
    expect(chip(w, "orders-db").exists()).toBe(false);
  });
});
