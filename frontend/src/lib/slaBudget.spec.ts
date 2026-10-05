import { describe, expect, it } from "vitest";

import { budgetRemaining, sloStatus } from "@/lib/slaBudget";

// Budgets below are exactly what internal/sla.ErrorBudget returns: remaining_ratio is
// allowed − actual as a fraction of ALL time, burned_percent is actual / allowed × 100.
// "Budget left" is the unburned share of the BUDGET, so only burned_percent answers it.
function budget(objective: number, actual: number) {
  const allowed = 1 - objective / 100;
  return {
    objective,
    allowed_downtime_ratio: allowed,
    actual_downtime_ratio: actual,
    remaining_ratio: allowed - actual,
    burned_percent: (actual / allowed) * 100,
    met: 1 - actual >= objective / 100,
  };
}

describe("budgetRemaining", () => {
  it("reports an untouched budget as 100 % left, whatever the objective", () => {
    expect(budgetRemaining(budget(99.9, 0))).toBeCloseTo(100, 6);
    expect(budgetRemaining(budget(99, 0))).toBeCloseTo(100, 6);
  });

  it("reports the unburned share of the budget, not of all time", () => {
    // 99.9 % objective, 0.025 % downtime: a quarter of the budget burned.
    expect(budgetRemaining(budget(99.9, 0.00025))).toBeCloseTo(75, 6);
  });

  it("clamps an overspent budget at 0", () => {
    expect(budgetRemaining(budget(99.9, 0.002))).toBe(0);
  });
});

describe("sloStatus", () => {
  it("calls a healthy SLO with zero errors Meeting, not At risk", () => {
    expect(sloStatus(budget(99.9, 0))).toBe("met");
  });

  it("calls a met SLO with at most a quarter of its budget left At risk", () => {
    expect(sloStatus(budget(99.9, 0.0008))).toBe("risk");
  });
});
