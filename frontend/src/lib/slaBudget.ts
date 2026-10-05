import type { components } from "@/api/schema";

type ErrorBudget = components["schemas"]["ErrorBudget"];

export type SloStatus = "met" | "risk" | "breach" | "none";

/**
 * Percent (0..100) of the error budget still unspent.
 *
 * Only `burned_percent` answers this. `remaining_ratio` is `allowed − actual` as a fraction of ALL
 * time, so an untouched 99.9 % budget is 0.001 of it — rendering that ×100 read as "0.1 % left" and
 * flagged a perfect SLO At risk (iter-0198).
 */
export function budgetRemaining(eb?: ErrorBudget): number {
  if (!eb) return 0;
  return Math.max(0, Math.min(100, 100 - (eb.burned_percent ?? 0)));
}

export function sloStatus(eb?: ErrorBudget): SloStatus {
  if (!eb) return "none";
  if (!eb.met) return "breach";
  return budgetRemaining(eb) <= 25 ? "risk" : "met";
}
