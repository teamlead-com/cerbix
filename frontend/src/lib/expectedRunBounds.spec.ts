import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

import {
  EXPECTED_RUN_RETENTION_DEFAULT_DAYS,
  EXPECTED_RUN_RETENTION_MAX_DAYS,
  EXPECTED_RUN_RETENTION_MIN_DAYS,
} from "./expectedRunBounds";

// G3 — the client mirrors the SERVER's ledger bounds rather than re-typing them.
//
// `MonitorDetailView.vue` carried `expectedRunRetentionDays = 14` and `expectedRunMinRetentionDays
// = 2` as its own constants, while the canary and monitor bounds beside it ship a fixture for
// exactly this. A change to either on the server left the view asking for a range the server
// refuses, with nothing failing until an operator met a 400.
//
// The mutation that must kill this: change a value on either side alone.
const FIXTURE = resolve(__dirname, "../../../internal/domain/testdata/expected_run_bounds.json");

describe("the ledger bounds the client mirrors", () => {
  it("matches the server's published values", () => {
    const published = JSON.parse(readFileSync(FIXTURE, "utf8")).bounds as Record<string, number>;
    const mirrored: Record<string, number> = {
      EXPECTED_RUN_RETENTION_DEFAULT_DAYS,
      EXPECTED_RUN_RETENTION_MIN_DAYS,
      EXPECTED_RUN_RETENTION_MAX_DAYS,
    };
    // Compared as a SET in both directions: a bound the server publishes and the client does not
    // mirror is the same defect as a mismatched value, and a count alone would miss a swap.
    expect(Object.keys(mirrored).sort()).toEqual(Object.keys(published).sort());
    expect(mirrored).toEqual(published);
  });
});
