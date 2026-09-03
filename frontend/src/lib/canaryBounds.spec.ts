import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import * as lib from "./canaryWorkflow";

// The BOUNDS PARITY gate, client half. `internal/domain/canarybounds_test.go` publishes every bound
// the Go validator enforces; this asserts each one is either MIRRORED here with the same value, or
// listed below as unreachable from the typed form WITH A REASON.
//
// It exists because three rounds of independent review found the same shape of defect three times:
// the form mirrors most rules and silently omits one, so it promises validity and the operator meets
// a 400. Fixing each named omission is not a mechanism — it is waiting for the next reviewer. A bound
// nobody mirrors and nobody justifies now fails here.
const FIXTURE = resolve(__dirname, "../../../internal/domain/testdata/canary_bounds.json");

/**
 * Bounds the FORM cannot reach, each with the reason. A reason is required: "not used" is how an
 * omission disguises itself as a decision.
 */
const UNREACHABLE: Record<string, string> = {
  // The form builds a FLAT body — one row is one top-level key holding a scalar or a binding — so
  // there is no nesting and no list for these to bound. A body editor that gained either would have
  // to mirror them, and this entry is what would make that visible.
  CANARY_MAX_BODY_DEPTH: "the form's body is flat: one row is one top-level key, never a nested object",
  CANARY_MAX_BODY_LIST_ELEMENTS: "the form's body has no list values; a row is a scalar or a binding",
  // The correlation id is produced by the TARGET at run time and never typed into this form.
  CANARY_MAX_CORRELATION_BYTES: "the correlation id comes from the target at run time; the form never carries one",
};

function utf8Bytes(s: string): number {
  return new TextEncoder().encode(s).length;
}

describe("bounds parity with the Go validator", () => {
  it("mirrors every published bound, or says in writing why it cannot be reached", () => {
    const published = JSON.parse(readFileSync(FIXTURE, "utf8")).bounds as Record<string, number>;
    const missing: string[] = [];
    const wrong: string[] = [];

    for (const [name, value] of Object.entries(published)) {
      const mirrored = (lib as unknown as Record<string, unknown>)[name];
      if (mirrored === undefined) {
        if (!UNREACHABLE[name]) missing.push(name);
        continue;
      }
      if (mirrored !== value) wrong.push(`${name}: client ${String(mirrored)} vs server ${value}`);
    }

    expect(
      missing,
      "these bounds exist in the Go validator and the form neither mirrors nor justifies them — " +
        "mirror them, or add an entry to UNREACHABLE with the reason",
    ).toEqual([]);
    expect(wrong, "a bound is mirrored with the WRONG value").toEqual([]);

    // A stale justification is its own defect: it makes a bound look considered when it is gone.
    for (const name of Object.keys(UNREACHABLE)) {
      expect(published[name], `${name} is justified as unreachable but no longer exists in Go`).toBeDefined();
      expect(
        (lib as unknown as Record<string, unknown>)[name],
        `${name} is justified as unreachable but IS mirrored — remove the justification`,
      ).toBeUndefined();
    }
  });

  it("measures BYTES, not UTF-16 code units, wherever the server counts bytes", () => {
    // Go's `len(s)` is bytes. JS `.length` is UTF-16 code units, so a two-byte character counts once
    // there and twice here — a client that used `.length` would accept a value the server refuses,
    // and only for non-ASCII input, which is exactly the kind of gap a happy-path test never sees.
    const twoByte = "é".repeat(600); // 600 code units, 1200 bytes
    expect(twoByte.length).toBe(600);
    expect(utf8Bytes(twoByte)).toBe(1200);
    expect(lib.canaryByteLength(twoByte)).toBe(1200);
  });
});
