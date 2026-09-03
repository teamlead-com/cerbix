import { describe, expect, it } from "vitest";
import { writeFileSync, mkdirSync, readFileSync, existsSync } from "node:fs";
import { dirname, resolve } from "node:path";

import {
  buildCanaryConfig,
  canaryRefusals,
  emptyCanaryForm,
  CANARY_CORRELATION_PLACEHOLDER,
  type CanaryForm,
} from "./canaryWorkflow";

// The CROSS-SURFACE SEAM, and the reason it exists: my parity claim rested on one happy vector, and
// the independent reviewer found five categories of shapes the client accepted and the server
// refuses (party [84]). One example proves one example.
//
// This is the other half of the remedy. It enumerates every UNION VARIANT the form can produce,
// keeps only the ones `canaryRefusals` calls valid, and writes them to a fixture that a GO test
// reads and runs through the real `Monitor.Validate`. So:
//
//   - if the client starts accepting something the server refuses, the Go test fails;
//   - if the client changes what it produces, this test rewrites the fixture and the diff is visible
//     in review rather than silent.
//
// Neither half can pass alone, which is the point: a TypeScript-only test cannot know the Go rules,
// and a Go-only test cannot know what the form builds.
const FIXTURE = resolve(__dirname, "../../../internal/domain/testdata/canary_form_variants.json");

function base(): CanaryForm {
  const f = emptyCanaryForm();
  f.bindings = [{ name: "upload", secret: "upload-token" }];
  f.submitURL = "https://files.example.com/files/upload";
  f.submitTimeout = "30";
  f.acceptedStatus = "202";
  f.submitHeaders = [
    { name: "authorization", value: "", secretRef: "upload" },
    { name: "x-tenant", value: "canary", secretRef: "" },
  ];
  f.bodyFields = [{ key: "tenant", value: "canary", secretRef: "" }];
  f.correlateSource = "response_json";
  f.correlatePath = "task_id";
  f.completionKind = "poll_json";
  f.completionURL = `https://files.example.com/tasks/${CANARY_CORRELATION_PLACEHOLDER}`;
  f.completionTimeout = "240";
  f.pollInterval = "5";
  f.pollMaxAttempts = "48";
  f.pollSuccessPath = "status";
  f.pollSuccessValue = "completed";
  f.maxLatency = "240";
  f.resultRequiredFields = "s3_path, byte_size";
  f.lifecyclePath = "s3_path";
  f.cleanupKind = "lifecycle_prefix";
  f.cleanupPrefix = "canary/";
  return f;
}

/** Every union variant, plus the edges a happy path never reaches. */
function variants(): { name: string; form: CanaryForm }[] {
  const out: { name: string; form: CanaryForm }[] = [];
  const add = (name: string, mut: (f: CanaryForm) => void) => {
    const f = base();
    mut(f);
    out.push({ name, form: f });
  };

  add("http_json + poll_json + lifecycle_prefix", () => {});
  add("http_json + poll_json + full failure pair", (f) => {
    f.pollFailurePath = "status";
    f.pollFailureValues = "failed, cancelled";
  });
  add("http_json + sse", (f) => {
    f.completionKind = "sse";
    f.sseSuccessEvent = "task.completed";
    f.sseFailureEvents = "task.failed";
    f.sseRequiredFields = "s3_path, byte_size";
  });
  add("multipart_fixture + poll_json", (f) => {
    f.submitKind = "multipart_fixture";
    f.bodyFields = [];
    f.fixtureRef = "small_wav_v1";
    f.fileField = "file";
    f.multipartFields = [{ key: "only_audio", value: "false", secretRef: "" }];
  });
  add("multipart_fixture + sse + binding in a multipart field", (f) => {
    f.submitKind = "multipart_fixture";
    f.bodyFields = [];
    f.fixtureRef = "small_wav_v1";
    f.fileField = "file";
    f.multipartFields = [{ key: "token", value: "", secretRef: "upload" }];
    f.submitHeaders = [{ name: "x-tenant", value: "canary", secretRef: "" }];
    f.completionKind = "sse";
    f.sseSuccessEvent = "task.completed";
  });
  add("response_header correlation", (f) => {
    f.correlateSource = "response_header";
    f.correlateHeaderName = "task-id";
    f.correlatePath = "";
  });
  add("cleanup: none, acknowledged", (f) => {
    f.cleanupKind = "none";
    f.cleanupPrefix = "";
    f.cleanupAcknowledged = true;
  });
  add("completion URL with no correlation marker", (f) => {
    f.completionURL = "https://files.example.com/tasks/latest";
  });
  add("no bindings at all", (f) => {
    f.bindings = [];
    f.submitHeaders = [{ name: "x-tenant", value: "canary", secretRef: "" }];
  });
  add("binding in the body, completion header on the same host", (f) => {
    f.bodyFields = [{ key: "token", value: "", secretRef: "upload" }];
    f.submitHeaders = [{ name: "x-tenant", value: "canary", secretRef: "" }];
    f.completionHeaders = [{ name: "authorization", value: "", secretRef: "upload" }];
  });
  // Edges: the exact bounds, where an off-by-one on either side would show.
  add("edge: submit_timeout at its ceiling", (f) => {
    f.submitTimeout = "60";
  });
  add("edge: max_latency equals the monitor timeout", (f) => {
    f.maxLatency = "300";
    f.completionTimeout = "300";
  });
  add("edge: poll interval × attempts exactly fills the window", (f) => {
    f.completionTimeout = "240";
    f.pollInterval = "5";
    f.pollMaxAttempts = "48";
  });
  add("edge: sixteen required fields", (f) => {
    f.resultRequiredFields = Array.from({ length: 16 }, (_, i) => `f${i}`).join(", ");
  });
  // The encoding variants (P1-A / P1-B, party [93]). These are the ones a positive fixture CAN carry:
  // documents the form calls valid whose bytes previously disagreed with Go. The server half proves
  // the disagreement is gone rather than merely re-measured on this side.
  add("encoding: a body of HTML characters, just under the byte bound", (f) => {
    // Go writes SIX bytes per `<`. Six rows of 200 keeps every leaf inside the 1024-byte string
    // bound while the whole body lands near 7.3 KB canonical — legal, and impossible to measure
    // correctly with JSON.stringify, which would have counted about 1.3 KB.
    f.bodyFields = Array.from({ length: 6 }, (_, i) => ({
      key: `angle${i}`,
      value: "<".repeat(200),
      secretRef: "",
    }));
  });
  add("encoding: a number past float64's exact range", (f) => {
    f.bodyFields = [{ key: "big", value: "9007199254740993", secretRef: "" }];
  });
  add("encoding: a trailing zero, and exponents in all three spellings", (f) => {
    // The earlier version of this variant was named "trailing zero and an exponent" and contained no
    // exponent — the name promised coverage the fixture did not have, which is how `1e3` stayed
    // broken through a review round.
    f.bodyFields = [
      { key: "trailing", value: "1.10", secretRef: "" },
      { key: "small", value: "0.1", secretRef: "" },
      { key: "exp", value: "1e3", secretRef: "" },
      { key: "expPlus", value: "1.5E+10", secretRef: "" },
      { key: "expMinus", value: "2e-7", secretRef: "" },
    ];
  });
  add("encoding: short escapes Go writes short", (f) => {
    // Backspace and form feed cost two bytes to Go and six to a naive encoder; a vertical tab costs
    // six to both. Carrying all three keeps the difference measurable rather than remembered.
    f.bodyFields = [{ key: "escapes", value: "a\bb\fc\vd", secretRef: "" }];
  });
  add("encoding: a large token a float64 cannot hold exactly but CAN represent", (f) => {
    // The server's rule is representability, not exactness: `Float64()` succeeds here, so the token
    // is legal and reaches the target with its digits intact. A 400-digit token overflows and is
    // refused by BOTH sides — that case is a negative one and lives in canaryWorkflow.spec.ts.
    f.bodyFields = [{ key: "huge", value: "1" + "0".repeat(300), secretRef: "" }];
  });
  add("encoding: multibyte and control characters in a value", (f) => {
    f.bodyFields = [{ key: "text", value: "héllo мир 日本\tand a tab", secretRef: "" }];
  });
  add("edge: eight bindings, every one used", (f) => {
    f.bindings = Array.from({ length: 8 }, (_, i) => ({ name: `b${i}`, secret: "upload-token" }));
    f.bodyFields = f.bindings.map((b) => ({ key: b.name, value: "", secretRef: b.name }));
    f.submitHeaders = [{ name: "x-tenant", value: "canary", secretRef: "" }];
  });
  return out;
}

describe("the form's valid documents are valid to the server", () => {
  // COMPARES by default and only writes when asked. A silent rewrite would put the drift in
  // `git status` where it can be committed without a thought; failing here says what changed and
  // makes regenerating a deliberate act. Set `CERBIX_UPDATE_SEAM=1` to regenerate.
  it("keeps the Go seam fixture identical to what the form produces", () => {
    const MONITOR_TIMEOUT = 300;
    const rows = variants().map(({ name, form }) => {
      const refusals = canaryRefusals(form, MONITOR_TIMEOUT, ["upload-token"]);
      return { name, refusals, config: buildCanaryConfig(form) };
    });

    // Every variant listed here must be one the FORM considers valid. A variant that is refused is a
    // mistake in this file, and saying so beats writing a fixture the Go side would then reject.
    for (const r of rows) {
      expect(r.refusals, `variant "${r.name}" is refused by the form itself: ${JSON.stringify(r.refusals)}`).toEqual(
        [],
      );
    }
    expect(rows.length).toBeGreaterThanOrEqual(21);

    const want =
      JSON.stringify(
        {
          note:
            "GENERATED by frontend/src/lib/canaryWorkflow.seam.spec.ts. Every entry is a config the " +
            "TYPED FORM produces and considers valid; internal/domain/canaryform_seam_test.go runs each " +
            "through the real Monitor.Validate. Do not hand-edit: regenerate with " +
            "CERBIX_UPDATE_SEAM=1 npm test.",
          monitor_timeout_seconds: MONITOR_TIMEOUT,
          variants: rows.map((r) => ({ name: r.name, config: r.config })),
        },
        null,
        2,
      ) + "\n";

    if (process.env.CERBIX_UPDATE_SEAM === "1") {
      mkdirSync(dirname(FIXTURE), { recursive: true });
      writeFileSync(FIXTURE, want);
      return;
    }
    expect(existsSync(FIXTURE), `the seam fixture is missing — regenerate with CERBIX_UPDATE_SEAM=1`).toBe(true);
    const have = readFileSync(FIXTURE, "utf8");
    // The message matters more than the diff: whoever sees this changed the form and has not told
    // the Go side, which is precisely the drift this seam exists to prevent.
    expect(
      have,
      "the form now produces something different from the committed Go seam fixture — " +
        "regenerate with CERBIX_UPDATE_SEAM=1 npm test and commit the diff",
    ).toBe(want);
  });
});
