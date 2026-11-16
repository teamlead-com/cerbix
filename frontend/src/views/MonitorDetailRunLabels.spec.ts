import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import * as liveModule from "@/stores/live";
import MonitorDetailView from "@/views/MonitorDetailView.vue";

// FR-043 §8, approved mock docs/design/mock-recovery-threshold.html §5 variant 1, §6 and §7
// (2026-11-16).
//
//   * the header reads exactly like the list row: "Confirming N/M…" on the degraded pill for an up
//     monitor with failures counted, and the "recovering N/M" chip beside the Down pill for a down
//     monitor with a run — a notification link must not land on "Operational" one click after the
//     list said "Confirming 1/3…";
//   * the page applies live `status` events to the loaded monitor, and both counters are as fresh as
//     the last load (D13). Every transition resets the run, so applying one zeroes
//     consecutive_successes, and a move into up zeroes consecutive_failures too — otherwise a stale
//     label from the first run survives the transition (owner's decision 2026-11-16);
//   * the Configuration list gains "Recovery threshold" directly after "Failure threshold", for the
//     types that have the field — not push, not composite.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: { id: "mon1" } }),
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
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", projects: [] }),
}));
// A REACTIVE statuses map: the live store is what delivers a status event to the page, and a plain
// object could not express one arriving after the load.
vi.mock("@/stores/live", async () => {
  const { reactive } = await vi.importActual<typeof import("vue")>("vue");
  const statuses = reactive({} as Record<string, { status: string; latency_ms: number; ts: string }>);
  return { useLive: () => ({ connect: () => {}, statuses }), __statuses: statuses };
});

const statuses = (liveModule as unknown as { __statuses: Record<string, { status: string; latency_ms: number; ts: string }> })
  .__statuses;

async function mountWith(monitor: Record<string, unknown>): Promise<VueWrapper> {
  for (const k of Object.keys(statuses)) delete statuses[k];
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/monitors/{monitorID}") {
      return {
        data: {
          id: "mon1",
          project_id: "p1",
          name: "api-gateway",
          type: "http",
          target: "https://api.example.internal/healthz",
          enabled: true,
          interval_seconds: 60,
          timeout_seconds: 10,
          retries: 0,
          failure_threshold: 3,
          recovery_threshold: 3,
          consecutive_failures: 0,
          consecutive_successes: 0,
          management: { source: "ui" },
          ...monitor,
        },
      };
    }
    return { data: undefined };
  });
  const w = mount(MonitorDetailView, {
    global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
  });
  await flushPromises();
  return w;
}

async function liveStatus(status: string) {
  statuses.mon1 = { status, latency_ms: 12, ts: new Date().toISOString() };
  await flushPromises();
}

const headerPill = (w: VueWrapper) => w.findComponent({ name: "StatusPill" });
const recoveringChip = (w: VueWrapper) => w.find('[data-testid="monitor-recovering"]');

describe("monitor page header — both labels (mock §5 variant 1)", () => {
  it("shows Confirming N/M… on the degraded pill, exactly as the list does", async () => {
    const w = await mountWith({ status: "up", failure_threshold: 3, consecutive_failures: 1 });
    expect(headerPill(w).text()).toBe("Confirming 1/3…");
    expect(headerPill(w).props("status")).toBe("degraded");
    expect(recoveringChip(w).exists()).toBe(false);
  });

  it("shows the recovering chip beside a Down pill that keeps saying Down", async () => {
    const w = await mountWith({ status: "down", recovery_threshold: 3, consecutive_successes: 2 });
    expect(headerPill(w).text()).toBe("Down");
    expect(headerPill(w).props("status")).toBe("down");
    const c = recoveringChip(w);
    expect(c.exists(), "no recovering chip in the header").toBe(true);
    expect(c.text()).toBe("recovering 2/3");
    expect(c.attributes("title")).toBe(
      "2 of 3 consecutive successful checks. The monitor stays down until 3 in a row succeed. As of page load.",
    );
  });

  it("reads 3/2 after the threshold was lowered during the run", async () => {
    const w = await mountWith({ status: "down", recovery_threshold: 2, consecutive_successes: 3 });
    expect(recoveringChip(w).text()).toBe("recovering 3/2");
  });

  it("shows neither label on a settled up monitor or a down monitor without a run", async () => {
    let w = await mountWith({ status: "up" });
    expect(headerPill(w).text()).toBe("Operational");
    expect(recoveringChip(w).exists()).toBe(false);
    w = await mountWith({ status: "down" });
    expect(headerPill(w).text()).toBe("Down");
    expect(recoveringChip(w).exists()).toBe(false);
  });
});

describe("monitor page — a live status event resets the counters (mock §7)", () => {
  it("clears consecutive_successes, so a stale recovering label does not survive up → down", async () => {
    const w = await mountWith({ status: "down", recovery_threshold: 3, consecutive_successes: 2 });
    expect(recoveringChip(w).text()).toBe("recovering 2/3");

    await liveStatus("up");
    expect(headerPill(w).text()).toBe("Operational");
    // Down again with the counter at 0: the first run's "2/3" is gone for good.
    await liveStatus("down");
    expect(headerPill(w).text()).toBe("Down");
    expect(recoveringChip(w).exists(), "the first run's label survived the transition").toBe(false);
  });

  it("clears consecutive_successes on any status event, even one into down", async () => {
    const w = await mountWith({ status: "down", recovery_threshold: 3, consecutive_successes: 2 });
    await liveStatus("down");
    expect(recoveringChip(w).exists()).toBe(false);
  });

  it("clears consecutive_failures on a move into up, so Confirming does not survive down → up", async () => {
    const w = await mountWith({ status: "up", failure_threshold: 3, consecutive_failures: 1 });
    expect(headerPill(w).text()).toBe("Confirming 1/3…");

    await liveStatus("down");
    expect(headerPill(w).text()).toBe("Down");
    await liveStatus("up");
    expect(headerPill(w).text(), "the first episode's Confirming label survived the recovery").toBe("Operational");
    expect(headerPill(w).props("status")).toBe("up");
  });
});

describe("monitor page Configuration — Recovery threshold row (mock §6)", () => {
  function configRows(w: VueWrapper): string[] {
    const section = w.findAll("section").find((s) => s.find("h3").exists() && s.find("h3").text() === "Configuration");
    expect(section, "no Configuration card").toBeDefined();
    return section!.findAll("dt").map((d) => d.text());
  }
  function value(w: VueWrapper, label: string): string {
    const dt = w.findAll("dt").find((d) => d.text() === label);
    expect(dt, `no ${label} row`).toBeDefined();
    return (dt!.element.nextElementSibling as HTMLElement).textContent!.trim();
  }

  it("lists it directly after Failure threshold for http, in checks", async () => {
    const w = await mountWith({ status: "up", type: "http", failure_threshold: 3, recovery_threshold: 3 });
    const rows = configRows(w);
    const i = rows.indexOf("Failure threshold");
    expect(i).toBeGreaterThanOrEqual(0);
    expect(rows[i + 1]).toBe("Recovery threshold");
    expect(value(w, "Recovery threshold")).toBe("3 checks");
  });

  it("shows it at 1 too — the first success recovers is a statement, not an absence", async () => {
    const w = await mountWith({ status: "up", type: "tcp", recovery_threshold: 1 });
    expect(value(w, "Recovery threshold")).toBe("1 checks");
  });

  it("is absent for push", async () => {
    const w = await mountWith({ status: "up", type: "push", recovery_threshold: 1, grace_seconds: 60 });
    expect(configRows(w)).not.toContain("Recovery threshold");
  });

  it("is absent for composite, which keeps its Failure threshold row", async () => {
    const w = await mountWith({ status: "up", type: "composite", recovery_threshold: 1, target: "" });
    const rows = configRows(w);
    expect(rows).toContain("Failure threshold");
    expect(rows).not.toContain("Recovery threshold");
  });
});
