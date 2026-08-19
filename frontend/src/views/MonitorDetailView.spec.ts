import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import MonitorDetailView from "@/views/MonitorDetailView.vue";

// FR-021 phase 5 (§16.1), against the approved mock's "A delegated monitor" screen.
//
// The screen exists because of one question an operator asks in the middle of an outage: this
// monitor is DOWN and nothing paged me — is that broken, or is somebody else covering it? So:
//
//   * the monitor keeps its REAL status pill. Dimming a delegated monitor would make the system show
//     something other than what it knows, which is what §11 and §14 exist to prevent;
//   * the chip names the OWNER, so "somebody else" has a name;
//   * the two signals are stated apart, because they are delegated apart — a service commonly covers
//     DOWN transitions while the monitor still alerts on its own error budget. "Delegated" without
//     saying WHICH would be worse than saying nothing.

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
// The live SSE store is machinery this screen's delegation block has no business exercising.
vi.mock("@/stores/live", () => ({
  useLive: () => ({ connect: () => {}, statuses: {} as Record<string, string> }),
}));

type Delegation = {
  live: { delegated: boolean; owners?: { id: string; slug: string; name: string }[]; reason?: string };
  burn: { delegated: boolean; owners?: { id: string; slug: string; name: string }[]; reason?: string };
};

function mountWith(delegation?: Delegation) {
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/monitors/{monitorID}") {
      return {
        data: {
          id: "mon1",
          project_id: "p1",
          name: "checkout-http",
          type: "http",
          target: "https://checkout.example.com/",
          status: "down",
          enabled: true,
          interval_seconds: 30,
          management: { source: "ui" },
          ...(delegation ? { delegation } : {}),
        },
      };
    }
    return { data: undefined };
  });
  return mount(MonitorDetailView, {
    global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
  });
}

const CHECKOUT = { id: "svc1", slug: "checkout", name: "Checkout" };

describe("MonitorDetailView delegation", () => {
  it("keeps the real status and names who pages instead", async () => {
    const w = mountWith({
      live: { delegated: true, owners: [CHECKOUT] },
      burn: { delegated: false, reason: "no_active_owner" },
    });
    await flushPromises();

    const chip = w.find('[data-testid="monitor-delegated"]');
    expect(chip.exists()).toBe(true);
    expect(chip.text()).toContain("Checkout");
    // The monitor is DOWN and must still READ as down — the chip is added beside the real pill,
    // never instead of it, and the pill is not dimmed away.
    expect(w.text().toLowerCase()).toContain("down");
    expect(w.findComponent({ name: "StatusPill" }).exists()).toBe(true);
  });

  it("states the two signals apart", async () => {
    const w = mountWith({
      live: { delegated: true, owners: [CHECKOUT] },
      burn: { delegated: false, reason: "no_active_owner" },
    });
    await flushPromises();

    expect(w.find('[data-testid="monitor-delegation-live"]').text()).toContain("delegated to Checkout");
    const burn = w.find('[data-testid="monitor-delegation-burn"]').text();
    expect(burn).toContain("still alerts for itself");
    expect(burn).toContain("no_active_owner");
  });

  it("says nothing about delegation when nothing is delegated", async () => {
    const w = mountWith({
      live: { delegated: false, reason: "no_active_owner" },
      burn: { delegated: false, reason: "no_active_owner" },
    });
    await flushPromises();

    expect(w.find('[data-testid="monitor-delegated"]').exists()).toBe(false);
    // ...but the per-signal statement is still there, because "why is nothing suppressed" is a
    // question with an answer.
    expect(w.find('[data-testid="monitor-delegation-live"]').text()).toContain("still alerts for itself");
  });

  it("omits the block entirely when the lookup could not be served", async () => {
    const w = mountWith(undefined);
    await flushPromises();

    // Absent is not "not delegated": a failed lookup must not be rendered as a claim.
    expect(w.find('[data-testid="monitor-delegation"]').exists()).toBe(false);
    expect(w.find('[data-testid="monitor-delegated"]').exists()).toBe(false);
  });
});
