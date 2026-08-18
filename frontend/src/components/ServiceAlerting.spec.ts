import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import ServiceAlerting from "@/components/ServiceAlerting.vue";

const apiMock = vi.hoisted(() => ({ PATCH: vi.fn(), GET: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

// FR-021 phase 5 (§16.6a/§16.1), against the approved mock. Three properties, each of which the
// component would otherwise get wrong in a way an operator would believe:
//
//   * the DECLARATION is not coverage. `owns_paging: true` with nothing armed must read as "members
//     page for themselves", with the server's reason — not as a service that is covering them.
//   * only edited fields travel. A PATCH that restated everything would overwrite whatever somebody
//     else changed between load and save, and the server merges under its own lock.
//   * a refusal renders as ITSELF, from the payload's own message.

type Alerting = { owns_paging: boolean; page_on: string[]; page_on_unknown: boolean; confirm_evaluations: number };

function mountWith(opts: {
  alerting?: Alerting | null;
  state?: { live: { armed: boolean; reason?: string; last_error?: string }; burn: { armed: boolean; reason?: string; last_error?: string } } | null;
  canWrite?: boolean;
  managedBy?: string;
  patchError?: string;
  stateAfterRefresh?: { live: { armed: boolean; reason?: string; last_error?: string }; burn: { armed: boolean; reason?: string; last_error?: string } };
}) {
  apiMock.PATCH.mockReset();
  apiMock.GET.mockReset();
  // The panel re-reads coverage from the SERVER; `stateAfterRefresh` is what the server says the
  // next time it is asked, which is how the tests below age a lease or dis-arm a save.
  // The FIRST read answers what the page loaded with; every later read answers `stateAfterRefresh`.
  // That ordering is the point: it makes "the badge changed" provable as "the panel asked again",
  // rather than as "the fixture always said so".
  let reads = 0;
  apiMock.GET.mockImplementation(async () => {
    reads++;
    const later = opts.stateAfterRefresh && reads > 1 ? opts.stateAfterRefresh : undefined;
    return { data: later ?? opts.state ?? undefined };
  });
  apiMock.PATCH.mockImplementation(async (_path: string, req: { body: Record<string, unknown> }) =>
    opts.patchError
      ? { error: { error: opts.patchError } }
      : { data: { ...(opts.alerting as Alerting), ...req.body } },
  );
  return mount(ServiceAlerting, {
    props: {
      projectId: "p1",
      serviceId: "s1",
      canWrite: opts.canWrite ?? true,
      alerting: opts.alerting ?? null,
      state: opts.state ?? null,
      managedBy: opts.managedBy ?? "",
    },
  });
}

const owning: Alerting = { owns_paging: true, page_on: ["down"], page_on_unknown: false, confirm_evaluations: 2 };

describe("ServiceAlerting", () => {
  it("reports coverage from the server, not from the switch", async () => {
    const w = mountWith({
      alerting: owning,
      state: { live: { armed: false, reason: "never_evaluated" }, burn: { armed: false, reason: "unroutable" } },
    });
    await flushPromises();

    expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("pending");
    expect(w.find('[data-testid="alerting-badge-burn"]').text()).toContain("degraded");
    expect(w.find('[data-testid="alerting-signal-live"]').text()).toContain("no evaluation yet");
    expect(w.find('[data-testid="alerting-signal-burn"]').text()).toContain("nothing to notify");
  });

  it("says a service is delegating only when something is armed", async () => {
    const w = mountWith({
      alerting: owning,
      state: { live: { armed: true }, burn: { armed: false, reason: "held" } },
    });
    await flushPromises();
    expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("armed");
    // The armed signal must NOT hide the other one's reason — the mandatory held-burn case.
    expect(w.find('[data-testid="alerting-signal-burn"]').text()).toContain("cannot be quoted");
    expect(w.find('[data-testid="alerting-burn-warning"]').exists()).toBe(true);
  });

  it("sends ONLY the fields that changed", async () => {
    const w = mountWith({ alerting: owning, state: { live: { armed: true }, burn: { armed: true } } });
    await flushPromises();

    await w.find('[data-testid="alerting-confirm"]').setValue("5");
    await w.find('[data-testid="alerting-save"]').trigger("click");
    await flushPromises();

    expect(apiMock.PATCH).toHaveBeenCalledTimes(1);
    expect(apiMock.PATCH.mock.calls[0][1].body).toEqual({ confirm_evaluations: 5 });
  });

  it("keeps Save disabled until something actually changes", async () => {
    const w = mountWith({ alerting: owning, state: { live: { armed: true }, burn: { armed: true } } });
    await flushPromises();
    expect(w.find('[data-testid="alerting-save"]').attributes("disabled")).toBeDefined();

    await w.find('[data-testid="alerting-page-on-degraded"]').trigger("click");
    expect(w.find('[data-testid="alerting-save"]').attributes("disabled")).toBeUndefined();
    await w.find('[data-testid="alerting-page-on-degraded"]').trigger("click"); // back to where it was
    expect(w.find('[data-testid="alerting-save"]').attributes("disabled")).toBeDefined();
  });

  it("warns when the declaration would page for nothing at all", async () => {
    const w = mountWith({
      alerting: { ...owning, page_on: [] },
      state: { live: { armed: false, reason: "policy_pages_nothing" }, burn: { armed: true } },
    });
    await flushPromises();
    expect(w.find('[data-testid="alerting-pages-nothing"]').exists()).toBe(true);
  });

  it("renders a file-managed service read-only and never sends", async () => {
    const w = mountWith({
      alerting: owning,
      state: { live: { armed: true }, burn: { armed: true } },
      managedBy: "payments-bundle",
    });
    await flushPromises();

    expect(w.find('[data-testid="alerting-save"]').exists()).toBe(false);
    expect(w.find('[data-testid="alerting-read-only"]').exists()).toBe(true);
    expect(w.find('[data-testid="alerting-owns-paging"]').attributes("disabled")).toBeDefined();
    expect(apiMock.PATCH).not.toHaveBeenCalled();
  });

  it("renders the server's own refusal", async () => {
    const w = mountWith({
      alerting: owning,
      state: { live: { armed: true }, burn: { armed: true } },
      patchError: "service alert policy: confirm_evaluations must be 1..10, got 44",
    });
    await flushPromises();

    await w.find('[data-testid="alerting-confirm"]').setValue("44");
    await w.find('[data-testid="alerting-save"]').trigger("click");
    await flushPromises();

    expect(w.find('[data-testid="alerting-error"]').text()).toContain("must be 1..10");
  });

  it("says so when the declaration could not be read, instead of showing a default", async () => {
    const w = mountWith({ alerting: null, state: null });
    await flushPromises();
    expect(w.find('[data-testid="alerting-unavailable"]').exists()).toBe(true);
    expect(w.find('[data-testid="alerting-owns-paging"]').exists()).toBe(false);
  });
});

describe("ServiceAlerting coverage is the server's, and it is CURRENT", () => {
  it("re-reads coverage after a save, because a paging edit dis-arms the service", async () => {
    const w = mountWith({
      alerting: owning,
      state: { live: { armed: true }, burn: { armed: true } },
      // What the server says once the PATCH has bumped the generation.
      stateAfterRefresh: {
        live: { armed: false, reason: "generation_changed" },
        burn: { armed: false, reason: "generation_changed" },
      },
    });
    await flushPromises();
    // Before the save the server still says armed, and the panel says armed.
    expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("armed");

    await w.find('[data-testid="alerting-confirm"]').setValue("5");
    await w.find('[data-testid="alerting-save"]').trigger("click");
    await flushPromises();

    expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("pending");
    expect(w.find('[data-testid="alerting-signal-live"]').text()).toContain("re-arming");
  });

  it("shows the evaluator's own error rather than summarizing it", async () => {
    const w = mountWith({
      alerting: owning,
      state: {
        live: { armed: false, reason: "evaluation_error", last_error: "projection failed: epoch 7 missing" },
        burn: { armed: true },
      },
    });
    await flushPromises();
    expect(w.find('[data-testid="alerting-error-live"]').text()).toContain("epoch 7 missing");
  });

  it("keeps the last server answer when a refresh fails, rather than inventing one", async () => {
    const w = mountWith({ alerting: owning, state: { live: { armed: true }, burn: { armed: true } } });
    await flushPromises();
    apiMock.GET.mockRejectedValueOnce(new Error("network"));
    await flushPromises();
    expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("armed");
  });
});
