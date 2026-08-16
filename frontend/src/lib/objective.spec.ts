import { describe, expect, it } from "vitest";

import { canonicalObjective } from "@/lib/objective";

// The client mirror of Go's domain.CanonicalObjective (D-0165, iter-0143): the case table
// matches the Go unit test case-for-case, proving CURRENT parity between the two rules.
// (The tables are independent copies — they do not compare each other automatically, so a
// coordinated rule+test change on one side is not detected here.)
describe("canonicalObjective", () => {
  it("admits only the open interval (0,100) at four canonical decimals", () => {
    const cases: Array<[number, number | null]> = [
      [100, null], // zero error budget is not a supported configuration
      [99.99995, null], // rounds to 100 — rejected by the post-round bound
      [99.9999, 99.9999], // the maximum admissible objective
      [99.99994, 99.9999],
      [0.0001, 0.0001],
      [0.00001, null], // rounds to zero
      [100.00004, null], // raw >= 100 is rejected as typed, never rounded into range
      [100.0001, null],
      [0, null],
      [-1, null],
      [101, null],
      [Number.NaN, null],
      [Number.POSITIVE_INFINITY, null],
      [Number.NEGATIVE_INFINITY, null],
    ];
    for (const [input, want] of cases) {
      expect(canonicalObjective(input), `canonicalObjective(${input})`).toBe(want);
    }
  });
});
