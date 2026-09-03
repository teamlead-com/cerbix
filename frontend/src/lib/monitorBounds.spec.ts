import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { MAX_MONITOR_DESCRIPTION, descriptionLength } from "./monitorBounds";

// FR-030 D1: one bound, counted the same way twice. The server publishes its value from
// internal/domain/monitordescription_test.go; the form mirrors it here, and this spec is what fails if
// either side moves alone — the lesson the canary form paid three review rounds for.
const FIXTURE = resolve(__dirname, "../../../internal/domain/testdata/monitor_bounds.json");

describe("monitor description bound", () => {
  it("mirrors the server's published limit exactly", () => {
    const published = JSON.parse(readFileSync(FIXTURE, "utf8")).bounds as Record<string, number>;
    expect(published.MAX_MONITOR_DESCRIPTION).toBe(MAX_MONITOR_DESCRIPTION);
  });

  it("counts code points, not UTF-16 units or bytes, as the server does", () => {
    expect(descriptionLength("a".repeat(200))).toBe(200);
    expect(descriptionLength("я".repeat(200))).toBe(200); // 400 bytes
    expect(descriptionLength("🙂".repeat(200))).toBe(200); // 400 UTF-16 units, 800 bytes
    expect("🙂".repeat(200).length, "the naive .length would refuse a legal value").toBe(400);
  });
});
