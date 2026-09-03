import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { canaryEncode } from "./canaryWorkflow";

// ESCAPING PARITY, client half. `internal/domain/canaryencoding_test.go` publishes what Go's
// `encoding/json` ACTUALLY produces; this asserts the client's hand-rolled encoder reproduces every
// entry byte for byte.
//
// It exists because guessing failed twice. `JSON.stringify` UNDER-counted a body, because Go
// HTML-escapes `<`, `>` and `&` into six bytes each. The replacement OVER-counted, because Go writes
// short escapes for backspace and form feed where the client wrote six-byte ones. Under-counting lets
// the server refuse what the form accepted; over-counting makes the form refuse what the server would
// take. Both break the same promise, and both came from recalling the Go source instead of reading it.
const FIXTURE = resolve(__dirname, "../../../internal/domain/testdata/canary_encoding.json");

describe("the hand-rolled encoder reproduces Go byte for byte", () => {
  const fixture = JSON.parse(readFileSync(FIXTURE, "utf8")) as {
    strings: Record<string, string>;
    body_input: Record<string, unknown>;
    body_encoded: string;
  };

  it("covers a meaningful table rather than a handful of cases", () => {
    // A fixture that shrank would make every assertion below vacuous.
    expect(Object.keys(fixture.strings).length).toBeGreaterThanOrEqual(25);
  });

  for (const [name, expected] of Object.entries(
    JSON.parse(readFileSync(FIXTURE, "utf8")).strings as Record<string, string>,
  )) {
    it(`encodes ${name} exactly as Go does`, () => {
      // The fixture holds Go's OUTPUT; the input is that output parsed back, which is exact because
      // JSON round-trips a string.
      const input = JSON.parse(expected) as string;
      expect(canaryEncode(input)).toBe(expected);
    });
  }

  it("encodes an object the way Go marshals a map", () => {
    expect(canaryEncode(fixture.body_input as never)).toBe(fixture.body_encoded);
  });
});
