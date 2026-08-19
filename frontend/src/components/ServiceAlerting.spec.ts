import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import ServiceAlerting from "@/components/ServiceAlerting.vue";

const apiMock = vi.hoisted(() => ({ PATCH: vi.fn(), GET: vi.fn(), PUT: vi.fn() }));
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
  // FR-023
  policies?: { id: string; name: string; steps?: unknown[] }[];
  escalationPolicyId?: string;
  putError?: string;
  putEcho?: string;
}) {
  apiMock.PATCH.mockReset();
  apiMock.GET.mockReset();
  // The panel re-reads coverage from the SERVER; `stateAfterRefresh` is what the server says the
  // next time it is asked, which is how the tests below age a lease or dis-arm a save.
  // The FIRST read answers what the page loaded with; every later read answers `stateAfterRefresh`.
  // That ordering is the point: it makes "the badge changed" provable as "the panel asked again",
  // rather than as "the fixture always said so".
  let reads = 0;
  apiMock.GET.mockImplementation(async (path: string) => {
    // TWO endpoints share this mock since FR-023: coverage and the escalation-policy list. Answering
    // by PATH matters — a single answer would hand the policy picker a coverage payload and any
    // assertion about it would then pass by accident.
    if (path.includes("escalation-policies")) return { data: opts.policies ?? [] };
    reads++;
    const later = opts.stateAfterRefresh && reads > 1 ? opts.stateAfterRefresh : undefined;
    return { data: later ?? opts.state ?? undefined };
  });
  apiMock.PUT.mockReset();
  apiMock.PUT.mockImplementation(async (_path: string, req: { body: { escalation_policy_id?: string } }) =>
    opts.putError
      ? { error: { error: opts.putError } }
      : // The real endpoint echoes what the DATABASE holds, which is not always what was sent — a
        // no-op echoes the stored value. `putEcho` is how a test says so.
        { data: { escalation_policy_id: opts.putEcho ?? req.body.escalation_policy_id } },
  );
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
      escalationPolicyId: opts.escalationPolicyId ?? "",
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

  // A refresh that FAILS must stop the panel claiming coverage, and this test drives the cadence
  // itself: the earlier version queued a rejection and never advanced the timer, so the rejection
  // was never consumed and deleting the interval left every test green.
  it("stops claiming ARMED when the cadence refresh fails, and recovers on the next success", async () => {
    vi.useFakeTimers();
    try {
      const w = mountWith({ alerting: owning, state: { live: { armed: true }, burn: { armed: true } } });
      await vi.advanceTimersByTimeAsync(0);
      expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("armed");
      const callsAtMount = apiMock.GET.mock.calls.length;

      // The next cadence read fails. Delivery may already have dis-armed on an expiry or an error,
      // so continuing to show green would be an assertion nobody can back.
      apiMock.GET.mockRejectedValueOnce(new Error("network"));
      await vi.advanceTimersByTimeAsync(15_000);
      expect(apiMock.GET.mock.calls.length,
        "the cadence must actually fire — without it this whole test is vacuous").toBeGreaterThan(callsAtMount);
      expect(w.find('[data-testid="alerting-badge-live"]').text()).not.toContain("armed");
      expect(w.find('[data-testid="alerting-state-unavailable"]').exists()).toBe(true);

      // ...and the next successful answer restores it: unavailable is a statement about now.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("armed");
      expect(w.find('[data-testid="alerting-state-unavailable"]').exists()).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  // The OTHER failure shape: a response that resolved carrying an error instead of a body. Treating
  // it as "nothing to do" is how a 500 every cadence reads as a healthy green badge.
  it("treats a resolved error response as a failed refresh", async () => {
    vi.useFakeTimers();
    try {
      const w = mountWith({ alerting: owning, state: { live: { armed: true }, burn: { armed: true } } });
      await vi.advanceTimersByTimeAsync(0);
      apiMock.GET.mockResolvedValueOnce({ error: { error: "not found" } });
      await vi.advanceTimersByTimeAsync(15_000);
      expect(w.find('[data-testid="alerting-badge-live"]').text()).not.toContain("armed");
      expect(w.find('[data-testid="alerting-state-unavailable"]').exists()).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("ServiceAlerting under a network blackhole", () => {
  // The third failure shape, and the one the two tests above cannot reach: a read that NEITHER
  // resolves NOR rejects. No error branch runs, so an earlier revision kept the last ARMED badge on
  // screen indefinitely while a new request piled up behind it every fifteen seconds.
  it("stops claiming ARMED when a refresh HANGS, and recovers on the next answer", async () => {
    vi.useFakeTimers();
    try {
      const armed = { live: { armed: true }, burn: { armed: true } };
      const w = mountWith({ alerting: owning, state: armed });
      await vi.advanceTimersByTimeAsync(0);
      expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("armed");

      // Every later read hangs forever, and the signal is deliberately ignored — a transport that
      // does not honour the abort must not be able to hold the panel hostage either.
      apiMock.GET.mockImplementation(() => new Promise(() => {}));
      await vi.advanceTimersByTimeAsync(15_000); // the cadence fires the hung read
      expect(w.find('[data-testid="alerting-badge-live"]').text(),
        "before its deadline the panel may still show the last answer").toContain("armed");

      await vi.advanceTimersByTimeAsync(10_000); // ...and its deadline expires
      expect(w.find('[data-testid="alerting-badge-live"]').text()).not.toContain("armed");
      expect(w.find('[data-testid="alerting-state-unavailable"]').exists()).toBe(true);

      // The next cadence answers: unavailable is a statement about now, not a scar.
      apiMock.GET.mockImplementation(async () => ({ data: armed }));
      await vi.advanceTimersByTimeAsync(15_000);
      expect(w.find('[data-testid="alerting-badge-live"]').text()).toContain("armed");
      expect(w.find('[data-testid="alerting-state-unavailable"]').exists()).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  // At most one NON-ABORTED read, and unmount cancels the one still running.
  //
  // The claim is deliberately narrow. A transport that ignores its signal cannot be cancelled by
  // anybody, so its underlying promise stays pending however many times it is aborted — what the
  // deadline bounds is the UI's truth, asserted by the test above. What THIS test pins is that the
  // component leaves at most one signal un-aborted, which is the whole of what it controls, and that
  // unmount aborts rather than abandoning.
  it("leaves at most one non-aborted read, and aborts the running one on unmount", async () => {
    vi.useFakeTimers();
    try {
      // mountWith installs its own implementation, so the collecting one goes in AFTER the mount
      // read — which is what we want anyway: the reads under test are the cadence reads.
      const w = mountWith({ alerting: owning, state: { live: { armed: true }, burn: { armed: true } } });
      await vi.advanceTimersByTimeAsync(0);

      const signals: AbortSignal[] = [];
      apiMock.GET.mockImplementation((_p: string, req: { signal?: AbortSignal }) => {
        if (req?.signal) signals.push(req.signal);
        return new Promise(() => {}) as never;
      });
      await vi.advanceTimersByTimeAsync(15_000);
      await vi.advanceTimersByTimeAsync(15_000);

      expect(signals.length, "every read must carry a signal").toBeGreaterThanOrEqual(2);
      const live = signals.filter((s) => !s.aborted);
      expect(live.length, "at most one signal may still be un-aborted").toBeLessThanOrEqual(1);

      w.unmount();
      expect(signals.every((s) => s.aborted),
        "unmount must abort the read still running, not only the next one").toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});

// The abort at the TOP of refreshState has its own test, because the cadence cannot reach it: the
// deadline (10s) fires before the next cadence (15s), so a cadence read is always already aborted by
// the time the following one starts. The line matters for the OTHER caller — a save, which refreshes
// immediately — and without it a successful PATCH would fire a second read while a hung cadence read
// was still outstanding.
it("a forced refresh after a save cancels the cadence read it replaces", async () => {
  vi.useFakeTimers();
  try {
    const w = mountWith({ alerting: owning, state: { live: { armed: true }, burn: { armed: true } } });
    await vi.advanceTimersByTimeAsync(0);

    const signals: AbortSignal[] = [];
    apiMock.GET.mockImplementation((_p: string, req: { signal?: AbortSignal }) => {
      if (req?.signal) signals.push(req.signal);
      return new Promise(() => {}) as never;
    });
    await vi.advanceTimersByTimeAsync(15_000); // a cadence read starts and hangs
    expect(signals.length, "the cadence read must have started").toBe(1);
    const cadenceRead = signals[0];
    expect(cadenceRead.aborted, "and it must still be running when the save happens").toBe(false);

    // The save's own refresh starts EARLY — before that read's deadline — so it is the one caller
    // for which "abort whatever is still running" is not already true.
    await w.find('[data-testid="alerting-confirm"]').setValue("6");
    await w.find('[data-testid="alerting-save"]').trigger("click");
    await vi.advanceTimersByTimeAsync(0);

    expect(signals.length, "the save must trigger its own read").toBeGreaterThan(1);
    expect(cadenceRead.aborted,
      "the forced refresh must cancel the cadence read it replaces").toBe(true);
  } finally {
    vi.useRealTimers();
  }
});

// FR-023 (approved mock, panel 1): the ladder's policy lives in this panel and saves SEPARATELY.
// What these pin is the pair of ways the control could lie — a picker that preselects nothing while a
// policy is attached, and a save whose local state believes the request instead of the response.
describe("ServiceAlerting escalation policy", () => {
  const POLICIES = [
    { id: "pol-1", name: "payments rota", steps: [{}, {}, {}] },
    { id: "pol-2", name: "night shift", steps: [{}] },
  ];

  it("lists the project's policies, preselects the attached one, and offers — none —", async () => {
    const w = mountWith({ policies: POLICIES, escalationPolicyId: "pol-2" });
    await flushPromises();
    const select = w.get('[data-testid="alerting-escalation-select"]');
    const options = select.findAll("option").map((o) => o.attributes("value"));
    expect(options).toEqual(["", "pol-1", "pol-2"]);
    expect((select.element as HTMLSelectElement).value).toBe("pol-2");
    // The step count is shown, because "which of these ladders" is the question an operator has.
    expect(select.text()).toContain("3 step(s)");
  });

  it("saves separately from the declaration and trusts the ECHO, not the request", async () => {
    const w = mountWith({ policies: POLICIES, escalationPolicyId: "", putEcho: "pol-1" });
    await flushPromises();
    // Nothing to save until something changes: the button is a statement about dirtiness.
    expect(w.get('[data-testid="alerting-escalation-save"]').attributes("disabled")).toBeDefined();

    await w.get('[data-testid="alerting-escalation-select"]').setValue("pol-1");
    await w.get('[data-testid="alerting-escalation-save"]').trigger("click");
    await flushPromises();

    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    const [path, req] = apiMock.PUT.mock.calls[0] as [string, { body: { escalation_policy_id: string } }];
    expect(path).toContain("/escalation-policy");
    expect(req.body).toEqual({ escalation_policy_id: "pol-1" });
    // The DECLARATION was not touched: two writes, two buttons, and this one must not send the other.
    expect(apiMock.PATCH).not.toHaveBeenCalled();
    expect(w.emitted("policy-saved")).toEqual([["pol-1"]]);
    expect(w.emitted("saved")).toBeUndefined();
  });

  it("emits what the server stored even when that differs from what was sent", async () => {
    // A no-op echoes the STORED value. A component that assumed its own draft had won would then
    // tell the parent something the database does not hold.
    const w = mountWith({ policies: POLICIES, escalationPolicyId: "pol-1", putEcho: "pol-1" });
    await flushPromises();
    await w.get('[data-testid="alerting-escalation-select"]').setValue("");
    await w.get('[data-testid="alerting-escalation-save"]').trigger("click");
    await flushPromises();
    expect(w.emitted("policy-saved")).toEqual([["pol-1"]]);
    expect((w.get('[data-testid="alerting-escalation-select"]').element as HTMLSelectElement).value).toBe("pol-1");
  });

  it("renders a refusal as ITSELF and emits nothing", async () => {
    const w = mountWith({ policies: POLICIES, escalationPolicyId: "", putError: "owner_not_in_project" });
    await flushPromises();
    await w.get('[data-testid="alerting-escalation-select"]').setValue("pol-1");
    await w.get('[data-testid="alerting-escalation-save"]').trigger("click");
    await flushPromises();
    expect(w.get('[data-testid="alerting-escalation-error"]').text()).toContain("owner_not_in_project");
    expect(w.emitted("policy-saved")).toBeUndefined();
    // The declaration's own error line stays empty: two writes, two error surfaces, or a reader
    // cannot tell which half failed.
    expect(w.find('[data-testid="alerting-error"]').exists()).toBe(false);
  });

  it("says a project has no policies instead of offering a picker that implies one", async () => {
    const w = mountWith({ policies: [], escalationPolicyId: "" });
    await flushPromises();
    expect(w.get('[data-testid="alerting-escalation-empty"]').text()).toContain("no escalation policy yet");
    // — none — is still there: the control is not hidden, because the field exists either way.
    expect(w.get('[data-testid="alerting-escalation-select"]').findAll("option")).toHaveLength(1);
  });

  it("is read-only for a file-managed service — the file's desired state includes the policy", async () => {
    const w = mountWith({ policies: POLICIES, escalationPolicyId: "pol-1", managedBy: "file:ops.yaml" });
    await flushPromises();
    expect(w.get('[data-testid="alerting-escalation-select"]').attributes("disabled")).toBeDefined();
    expect(w.find('[data-testid="alerting-escalation-save"]').exists()).toBe(false);
  });
});
