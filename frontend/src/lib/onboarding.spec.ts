import { describe, expect, it } from "vitest";

import {
  createdMonitorDestination,
  dismissOnboarding,
  isOnboardingDismissed,
  onboardingDismissalKey,
  resolveOnboarding,
  type MonitorEvidence,
  type OnboardingInput,
} from "@/lib/onboarding";

const monitor = (overrides: Record<string, unknown> = {}) => ({
  id: "m1",
  name: "Checkout API",
  type: "http" as const,
  region: "core",
  enabled: true,
  interval_seconds: 60,
  timeout_seconds: 10,
  created_at: "2026-09-19T09:00:00Z",
  ...overrides,
});

const evidence = (heartbeat: MonitorEvidence["heartbeat"] = null, overrides: Record<string, unknown> = {}): MonitorEvidence[] => [{
  monitor: monitor(overrides),
  heartbeat,
}];

const base = (overrides: Partial<OnboardingInput> = {}): OnboardingInput => ({
  orgCount: 1,
  orgId: "o1",
  projectId: "p1",
  canCreateOrg: true,
  canCreateProject: true,
  canCreateMonitor: true,
  evidence: [],
  ...overrides,
});

describe("onboarding state machine", () => {
  it("gives failed reads precedence over apparent empty state", () => {
    const state = resolveOnboarding(base({ orgCount: 0, readError: "503 service unavailable" }));
    expect(state.kind).toBe("failed_read");
    expect(state.error).toContain("503");
  });

  it("separates authorized fresh states from permission handoff", () => {
    expect(resolveOnboarding(base({ orgCount: 0 })).kind).toBe("fresh_no_org");
    const denied = resolveOnboarding(base({ orgCount: 0, canCreateOrg: false }));
    expect(denied.kind).toBe("forbidden_handoff");
    expect(denied.handoffStep).toBe("org");

    expect(resolveOnboarding(base({ projectId: "" })).kind).toBe("org_no_project");
    expect(resolveOnboarding(base({ evidence: [] })).kind).toBe("project_no_monitor");
  });

  it("waits without claiming health and reports known worker absence", () => {
    const waiting = resolveOnboarding(base({ evidence: evidence(), now: Date.parse("2026-09-19T09:00:20Z") }));
    expect(waiting.kind).toBe("waiting_for_result");
    expect(waiting.done).toEqual(["org", "project", "monitor"]);

    const unavailable = resolveOnboarding(base({ evidence: evidence(), regionLive: false }));
    expect(unavailable.kind).toBe("worker_unavailable");

    const unknown = resolveOnboarding(base({ evidence: evidence(), regionLive: true, now: Date.parse("2026-09-19T09:02:00Z") }));
    expect(unknown.kind).toBe("scheduler_unknown");
  });

  it("treats the first DOWN as journey success and target failure", () => {
    const heartbeat = { monitor_id: "m1", ts: "2026-09-19T09:01:00Z", up: false, code: 503, msg: "expected 2xx" };
    const state = resolveOnboarding(base({ evidence: evidence(heartbeat), journeyActive: true }));
    expect(state.kind).toBe("first_result_down");
    expect(state.done).toEqual(["org", "project", "monitor", "result"]);
    expect(state.heartbeat?.msg).toBe("expected 2xx");
  });

  it("keeps an existing installation closed unless manually opened", () => {
    const heartbeat = { monitor_id: "m1", ts: "2026-09-19T09:01:00Z", up: true, latency_ms: 184 };
    expect(resolveOnboarding(base({ evidence: evidence(heartbeat) })).kind).toBe("complete_existing");
    expect(resolveOnboarding(base({ evidence: evidence(heartbeat), journeyActive: true })).kind).toBe("first_result_up");
  });

  it("prefers the requested candidate, otherwise the newest enabled monitor", () => {
    const candidates: MonitorEvidence[] = [
      { monitor: monitor({ id: "old", enabled: true, created_at: "2026-09-19T08:00:00Z" }), heartbeat: null },
      { monitor: monitor({ id: "new", enabled: true, created_at: "2026-09-19T09:00:00Z" }), heartbeat: null },
      { monitor: monitor({ id: "paused", enabled: false, created_at: "2026-09-19T10:00:00Z" }), heartbeat: null },
    ];
    expect(resolveOnboarding(base({ evidence: candidates, now: Date.parse("2026-09-19T09:00:10Z") })).candidate?.id).toBe("new");
    expect(resolveOnboarding(base({ evidence: candidates, candidateId: "old" })).candidate?.id).toBe("old");
  });

  it("does not complete a new candidate from another monitor's old heartbeat", () => {
    const state = resolveOnboarding(base({
      journeyActive: true,
      candidateId: "new",
      now: Date.parse("2026-09-19T09:00:10Z"),
      evidence: [
        {
          monitor: monitor({ id: "old", created_at: "2026-09-19T08:00:00Z" }),
          heartbeat: { monitor_id: "old", ts: "2026-09-19T08:01:00Z", up: true },
        },
        {
          monitor: monitor({ id: "new", created_at: "2026-09-19T09:00:00Z" }),
          heartbeat: null,
        },
      ],
    }));

    expect(state.kind).toBe("waiting_for_result");
    expect(state.candidate?.id).toBe("new");
  });
});

describe("onboarding dismissal", () => {
  it("scopes presentation preference by user and selected tenant scope", () => {
    expect(onboardingDismissalKey("u1", "o1", "p1")).not.toBe(onboardingDismissalKey("u2", "o1", "p1"));
    expect(onboardingDismissalKey("u1", "o1", "p1")).not.toBe(onboardingDismissalKey("u1", "o1", "p2"));
    expect(onboardingDismissalKey("u1", "o1", "")).toContain("org.o1");
  });

  it("stores only the boolean presentation preference", () => {
    const values = new Map<string, string>();
    const storage = {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => values.set(key, value),
    };
    const key = onboardingDismissalKey("u1", "o1", "p1");
    dismissOnboarding(key, storage);
    expect(isOnboardingDismissed(key, storage)).toBe(true);
    expect([...values.values()]).toEqual(["1"]);
  });
});

describe("onboarding create navigation", () => {
  it("returns pull monitors to the waiting guide and keeps push secrets on monitor detail", () => {
    expect(createdMonitorDestination("m1", "http", true)).toEqual({
      name: "dashboard",
      query: { onboarding: "1", monitor: "m1" },
    });
    expect(createdMonitorDestination("m2", "push", true)).toEqual({
      name: "monitor",
      params: { id: "m2" },
      query: { onboarding: "1" },
    });
    expect(createdMonitorDestination("m3", "dns", false)).toEqual({ name: "monitor", params: { id: "m3" } });
  });
});
