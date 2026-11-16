import { describe, expect, it } from "vitest";

import {
  SOAK_WARN_AT,
  confirmingLabel,
  dependencyHintShown,
  formatSoakDuration,
  hasRecoveryThreshold,
  recoveringLabel,
  recoveringTooltip,
  soakWarning,
} from "./monitorRun";

// FR-043 §8 and the approved mock (docs/design/mock-recovery-threshold.html, 2026-11-16): the
// helpers the list, the monitor page and the form share, so the two labels and the warning are
// written once.

describe("recovering N/M", () => {
  it("is shown for a down monitor with a run, and is not gated on N < M (D10: '3/2')", () => {
    expect(recoveringLabel({ status: "down", consecutive_successes: 2, recovery_threshold: 3 })).toBe("recovering 2/3");
    expect(recoveringLabel({ status: "down", consecutive_successes: 3, recovery_threshold: 2 })).toBe("recovering 3/2");
    expect(recoveringLabel({ status: "down", consecutive_successes: 1, recovery_threshold: 3 })).toBe("recovering 1/3");
  });
  it("is absent without a run, and never on an up or pending monitor", () => {
    expect(recoveringLabel({ status: "down", consecutive_successes: 0, recovery_threshold: 3 })).toBeUndefined();
    expect(recoveringLabel({ status: "down", recovery_threshold: 3 })).toBeUndefined();
    expect(recoveringLabel({ status: "up", consecutive_successes: 2, recovery_threshold: 3 })).toBeUndefined();
    expect(recoveringLabel({ status: "pending", consecutive_successes: 2, recovery_threshold: 3 })).toBeUndefined();
  });
  it("explains itself in the mock's words, as of page load", () => {
    expect(recoveringTooltip({ status: "down", consecutive_successes: 2, recovery_threshold: 3 })).toBe(
      "2 of 3 consecutive successful checks. The monitor stays down until 3 in a row succeed. As of page load.",
    );
    expect(recoveringTooltip({ status: "down", consecutive_successes: 3, recovery_threshold: 2 })).toBe(
      "3 consecutive successful checks; the threshold was lowered to 2 during the run, so the next successful check confirms the recovery. As of page load.",
    );
    expect(recoveringTooltip({ status: "up", consecutive_successes: 0, recovery_threshold: 3 })).toBeUndefined();
  });
});

describe("confirming N/M", () => {
  it("keeps today's rule and text: up, failures counted, below the threshold", () => {
    expect(confirmingLabel({ status: "up", consecutive_failures: 1, failure_threshold: 3 })).toBe("Confirming 1/3…");
    expect(confirmingLabel({ status: "up", consecutive_failures: 3, failure_threshold: 3 })).toBeUndefined();
    expect(confirmingLabel({ status: "down", consecutive_failures: 1, failure_threshold: 3 })).toBeUndefined();
    expect(confirmingLabel({ status: "up", consecutive_failures: 0, failure_threshold: 3 })).toBeUndefined();
  });
});

describe("long-soak warning", () => {
  it("warns at interval × (M − 1) ≥ 600 s, with the mock's text", () => {
    expect(SOAK_WARN_AT).toBe(600);
    expect(soakWarning(300, 3)).toBe(
      "Recovery will be announced about 10 min after the first successful check (2 more checks × 5 min). Checks are not sped up during recovery.",
    );
    expect(soakWarning(600, 2)).toBe(
      "Recovery will be announced about 10 min after the first successful check (1 more check × 10 min). Checks are not sped up during recovery.",
    );
    expect(soakWarning(90, 8)).toContain("about 10 min 30 s after");
    expect(soakWarning(3600, 3)).toContain("about 2 h after");
  });
  it("is silent below the boundary and at threshold 1", () => {
    expect(soakWarning(60, 3)).toBeUndefined();
    expect(soakWarning(60, 10)).toBeUndefined(); // 540 s: a 60 s monitor can never trigger it
    expect(soakWarning(3600, 1)).toBeUndefined();
    expect(soakWarning(599, 2)).toBeUndefined();
  });
  it("formats durations in the largest whole units, as the mock does", () => {
    expect(formatSoakDuration(45)).toBe("45 s");
    expect(formatSoakDuration(600)).toBe("10 min");
    expect(formatSoakDuration(630)).toBe("10 min 30 s");
    expect(formatSoakDuration(7200)).toBe("2 h");
    expect(formatSoakDuration(3661)).toBe("1 h 1 min 1 s");
  });
});

describe("dependency hint and the field's presence", () => {
  it("shows the hint only with a parent AND a threshold above 1 (owner, 2026-11-16)", () => {
    expect(dependencyHintShown(["p1"], 2)).toBe(true);
    expect(dependencyHintShown(["p1"], 1)).toBe(false);
    expect(dependencyHintShown([], 3)).toBe(false);
    expect(dependencyHintShown(undefined, 3)).toBe(false);
  });
  it("has no recovery threshold for push and composite (D8)", () => {
    expect(hasRecoveryThreshold("http")).toBe(true);
    expect(hasRecoveryThreshold("tcp")).toBe(true);
    expect(hasRecoveryThreshold("push")).toBe(false);
    expect(hasRecoveryThreshold("composite")).toBe(false);
  });
});
