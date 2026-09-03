import { describe, it, expect } from "vitest";
import {
  buildCanaryConfig,
  canaryRefusals,
  canarySecretRefKey,
  emptyCanaryForm,
  isCredentialHeader,
  parseCanaryConfig,
  CANARY_CORRELATION_PLACEHOLDER,
  CANARY_WORKFLOW_KEY,
  CANARY_WORKFLOW_KIND,
  type CanaryForm,
} from "./canaryWorkflow";

// A valid poll-completion canary, matching the shape the server's own fixture uses. Every test below
// starts from something that PASSES, so a refusal it asserts is caused by the one thing it changed.
function validForm(): CanaryForm {
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
  f.resultRequiredFields = "s3_path, byte_size, media_type";
  f.lifecyclePath = "s3_path";
  f.cleanupKind = "lifecycle_prefix";
  f.cleanupPrefix = "canary/";
  return f;
}

const at = (f: CanaryForm, field: string, secrets: string[] = ["upload-token"]) =>
  canaryRefusals(f, 300, secrets).filter((r) => r.field === field);

describe("canary workflow rules", () => {
  it("accepts the valid shape and refuses nothing", () => {
    expect(canaryRefusals(validForm(), 300, ["upload-token"])).toEqual([]);
  });

  it("refuses a literal in a credential-bearing header and demands a binding instead (D7)", () => {
    const f = validForm();
    f.submitHeaders[0] = { name: "authorization", value: "Bearer sk-EXAMPLE", secretRef: "" };
    const found = at(f, "submitHeaders.0");
    expect(found.map((r) => r.message).join(" | ")).toMatch(/reference a binding, never a value/);
    expect(found.map((r) => r.message).join(" | ")).toMatch(/needs a binding/);
    // The refusal never echoes the value: a message is a place a credential leaks like a log line.
    for (const r of found) expect(r.message).not.toContain("sk-EXAMPLE");
  });

  it("knows the credential-bearing set and does not guess by shape", () => {
    expect(isCredentialHeader("Authorization")).toBe(true);
    expect(isCredentialHeader("X-API-Key")).toBe(true);
    // The residual, asserted as a NON-guarantee: an ordinary header is not policed.
    expect(isCredentialHeader("x-tenant")).toBe(false);
    const f = validForm();
    f.submitHeaders.push({ name: "x-my-token", value: "sk-EXAMPLE-not-detectable", secretRef: "" });
    expect(at(f, "submitHeaders.2")).toEqual([]);
  });

  it("refuses the correlation placeholder outside the completion URL", () => {
    const f = validForm();
    f.submitURL = `https://files.example.com/files/${CANARY_CORRELATION_PLACEHOLDER}/upload`;
    expect(at(f, "submitURL").map((r) => r.message).join()).toMatch(/only legal in the completion URL/);
    // And it is accepted in the completion URL, which is the other half of the same rule.
    expect(at(validForm(), "completionURL")).toEqual([]);
  });

  it("refuses any other placeholder anywhere", () => {
    const f = validForm();
    f.completionURL = "https://files.example.com/tasks/{{ task }}";
    expect(at(f, "completionURL").map((r) => r.message).join()).toMatch(/no other placeholder/);
  });

  it("refuses plaintext HTTP and userinfo credentials", () => {
    const f = validForm();
    f.submitURL = "http://files.example.com/files/upload";
    expect(at(f, "submitURL").map((r) => r.message).join()).toMatch(/HTTPS only/);
    const g = validForm();
    g.submitURL = "https://user:pass@files.example.com/files/upload";
    expect(at(g, "submitURL").map((r) => r.message).join()).toMatch(/userinfo/);
  });

  it("refuses duplicate headers case-insensitively", () => {
    const f = validForm();
    f.submitHeaders.push({ name: "X-Tenant", value: "other", secretRef: "" });
    expect(at(f, "submitHeaders.2").map((r) => r.message).join()).toMatch(/declared twice/);
  });

  it("refuses a header the runner owns", () => {
    const f = validForm();
    f.submitHeaders.push({ name: "idempotency-key", value: "mine", secretRef: "" });
    expect(at(f, "submitHeaders.2").map((r) => r.message).join()).toMatch(/set by the runner/);
  });

  it("refuses an undeclared binding, and a declared one nobody uses", () => {
    const f = validForm();
    f.submitHeaders[0].secretRef = "nope";
    expect(at(f, "submitHeaders.0").map((r) => r.message).join()).toMatch(/no binding named nope/);

    const g = validForm();
    g.bindings.push({ name: "spare", secret: "upload-token" });
    expect(at(g, "bindings.1").map((r) => r.message).join()).toMatch(/declared and never used/);
  });

  it("knows which project secrets exist — one of the two things only a client can", () => {
    const f = validForm();
    expect(at(f, "bindings.0", ["something-else"]).map((r) => r.message).join()).toMatch(/no project secret/);
  });

  it("keeps every bound inside the monitor's timeout", () => {
    const f = validForm();
    expect(canaryRefusals(f, 60, ["upload-token"]).map((r) => r.field)).toContain("completionTimeout");
    const g = validForm();
    g.maxLatency = "400";
    expect(at(g, "maxLatency").map((r) => r.message).join()).toMatch(/fit inside the monitor's timeout/);
    const h = validForm();
    h.submitTimeout = "90";
    expect(at(h, "submitTimeout").map((r) => r.message).join()).toMatch(/between 1 and 60/);
  });

  it("refuses a poll budget that cannot fit its own window", () => {
    const f = validForm();
    f.pollInterval = "10";
    f.pollMaxAttempts = "60"; // 600s > 240s completion timeout
    expect(at(f, "pollMaxAttempts").map((r) => r.message).join()).toMatch(/fit inside the completion timeout/);
  });

  it("refuses a path outside the restricted grammar", () => {
    const f = validForm();
    f.correlatePath = "task[0].id";
    expect(at(f, "correlatePath").map((r) => r.message).join()).toMatch(/no expressions/);
  });

  it("refuses accepted statuses that are not a 2xx set", () => {
    const f = validForm();
    f.acceptedStatus = "302";
    expect(at(f, "acceptedStatus").map((r) => r.message).join()).toMatch(/2xx/);
    const g = validForm();
    g.acceptedStatus = "202, 202";
    expect(at(g, "acceptedStatus").map((r) => r.message).join()).toMatch(/listed twice/);
  });

  it("refuses a completion binding across hosts (D3c1)", () => {
    const f = validForm();
    f.completionURL = `https://events.other.example.com/tasks/${CANARY_CORRELATION_PLACEHOLDER}`;
    f.completionHeaders = [{ name: "authorization", value: "", secretRef: "upload" }];
    expect(at(f, "completionHeaders").map((r) => r.message).join()).toMatch(/completion host to equal the submit host/);
    // Same host is fine, which is what makes the rule about the HOST and not about bindings.
    const g = validForm();
    g.completionHeaders = [{ name: "authorization", value: "", secretRef: "upload" }];
    expect(at(g, "completionHeaders")).toEqual([]);
  });

  it("refuses cleanup: none without the acknowledgement (D10)", () => {
    const f = validForm();
    f.cleanupKind = "none";
    f.cleanupPrefix = "";
    expect(at(f, "cleanupAcknowledged").map((r) => r.message).join()).toMatch(/must be acknowledged/);
    f.cleanupAcknowledged = true;
    expect(at(f, "cleanupAcknowledged")).toEqual([]);
  });

  it("demands a fixture registry key for multipart, never an upload", () => {
    const f = validForm();
    f.submitKind = "multipart_fixture";
    f.bodyFields = [];
    f.fixtureRef = "";
    expect(at(f, "fixtureRef").map((r) => r.message).join()).toMatch(/registry key, never an upload/);
  });
});

describe("what goes on the wire", () => {
  it("sends the document AND the flat ref keys, with no project-secret name inside the document", () => {
    const cfg = buildCanaryConfig(validForm());
    expect(cfg[canarySecretRefKey("upload")]).toBe("upload-token");
    const doc = cfg[CANARY_WORKFLOW_KEY];
    expect(doc).toBeTruthy();
    // D3f: `workflow.secrets` is INPUT-ONLY. The stored document keeps the marker and never the name.
    expect(doc).not.toContain("upload-token");
    expect(doc).toContain("secret_ref");
    expect(JSON.parse(doc).kind).toBe(CANARY_WORKFLOW_KIND);
  });

  it("never emits a settings map, a nested JSON string, or a secrets block", () => {
    const doc = JSON.parse(buildCanaryConfig(validForm())[CANARY_WORKFLOW_KEY]);
    expect(doc.settings).toBeUndefined();
    expect(doc.secrets).toBeUndefined();
    // No value anywhere in the document is itself a JSON document.
    const walk = (v: unknown): void => {
      if (typeof v === "string") expect(v.trim().startsWith("{")).toBe(false);
      else if (Array.isArray(v)) v.forEach(walk);
      else if (v && typeof v === "object") Object.values(v).forEach(walk);
    };
    walk(doc);
  });

  it("types scalars so the schema gets what it expects, and a binding becomes a secret_ref node", () => {
    const f = validForm();
    f.bodyFields = [
      { key: "tenant", value: "canary", secretRef: "" },
      { key: "attempts", value: "1", secretRef: "" },
      { key: "dry_run", value: "false", secretRef: "" },
      { key: "token", value: "", secretRef: "upload" },
    ];
    const body = JSON.parse(buildCanaryConfig(f)[CANARY_WORKFLOW_KEY]).submit.body;
    expect(body.tenant).toBe("canary");
    expect(body.attempts).toBe(1);
    expect(body.dry_run).toBe(false);
    expect(body.token).toEqual({ secret_ref: "upload" });
  });

  it("emits a header as a binding OR a value, never both", () => {
    const headers = JSON.parse(buildCanaryConfig(validForm())[CANARY_WORKFLOW_KEY]).submit.headers;
    const bound = headers.find((h: any) => h.name === "authorization");
    expect(bound).toEqual({ name: "authorization", secret_ref: "upload" });
    expect("value" in bound).toBe(false);
  });

  it("round-trips: a saved canary reads back into the same form", () => {
    const original = validForm();
    const back = parseCanaryConfig(buildCanaryConfig(original));
    expect(back).not.toBeNull();
    // The binding halves are recombined from the document's marker and the flat key's name, which is
    // why the read view needs no JSON editor to be complete.
    expect(back!.bindings).toEqual([{ name: "upload", secret: "upload-token" }]);
    expect(back!.submitURL).toBe(original.submitURL);
    expect(back!.completionURL).toBe(original.completionURL);
    expect(back!.pollSuccessValue).toBe("completed");
    expect(back!.cleanupPrefix).toBe("canary/");
    expect(canaryRefusals(back!, 300, ["upload-token"])).toEqual([]);
  });

  it("returns null for a document it cannot read rather than a half-built form", () => {
    expect(parseCanaryConfig({})).toBeNull();
    expect(parseCanaryConfig({ [CANARY_WORKFLOW_KEY]: "{not json" })).toBeNull();
  });
});

// The five categories the independent reviewer found (party [84]): shapes `canaryRefusals` returned
// `[]` for while the Go validator rejects them. My parity claim rested on ONE happy vector, which
// cannot prove that every client-accepted shape is server-valid — the reviewer said so and was right.
//
// One case per named branch. Each starts from a form that PASSES, so the refusal it asserts is caused
// by the single thing it changed.
describe("client/server parity — the branches the reviewer found", () => {
  it("1. a fixture is a registry key, not free text", () => {
    const f = validForm();
    f.submitKind = "multipart_fixture";
    f.bodyFields = [];
    f.fixtureRef = "https://evil.example/file.wav";
    f.fileField = "file";
    expect(at(f, "fixtureRef").map((r) => r.message).join()).toMatch(/not a fixture the runner carries/);
    f.fixtureRef = "small_wav_v1";
    expect(at(f, "fixtureRef")).toEqual([]);
  });

  it("2. the correlation marker occupies ONE WHOLE path segment, and appears at most once", () => {
    const f = validForm();
    // A fragment of a segment: the client used to substitute and parse, so this passed.
    f.completionURL = `https://files.example.com/tasks/x${CANARY_CORRELATION_PLACEHOLDER}`;
    expect(at(f, "completionURL").map((r) => r.message).join()).toMatch(/whole path segment/);
    // Two markers.
    f.completionURL = `https://files.example.com/${CANARY_CORRELATION_PLACEHOLDER}/${CANARY_CORRELATION_PLACEHOLDER}`;
    expect(at(f, "completionURL").map((r) => r.message).join()).toMatch(/at most one/);
    // In the query rather than the path.
    f.completionURL = `https://files.example.com/tasks?id=${CANARY_CORRELATION_PLACEHOLDER}`;
    expect(at(f, "completionURL").map((r) => r.message).join()).toMatch(/whole path segment/);
    // A completion URL with NO marker is legal: not every API addresses the transaction by path.
    f.completionURL = "https://files.example.com/tasks/latest";
    expect(at(f, "completionURL")).toEqual([]);
  });

  it("3. body keys obey the grammar and the bound, and a blank or duplicate key is refused", () => {
    const f = validForm();
    f.bodyFields = [{ key: "not valid", value: "x", secretRef: "" }];
    expect(at(f, "bodyFields.0").map((r) => r.message).join()).toMatch(/is not a valid key/);

    // Blank and duplicate used to be dropped or overwritten in SILENCE by `fieldMap`, so the typed
    // controls could change what the operator meant without saying anything.
    const g = validForm();
    g.bodyFields = [{ key: "", value: "x", secretRef: "" }];
    expect(at(g, "bodyFields.0").map((r) => r.message).join()).toMatch(/needs a key/);

    const h = validForm();
    h.bodyFields = [
      { key: "tenant", value: "a", secretRef: "" },
      { key: "tenant", value: "b", secretRef: "" },
    ];
    expect(at(h, "bodyFields.1").map((r) => r.message).join()).toMatch(/declared twice/);

    const many = validForm();
    many.bodyFields = Array.from({ length: 65 }, (_, i) => ({ key: `k${i}`, value: "v", secretRef: "" }));
    expect(at(many, "bodyFields").map((r) => r.message).join()).toMatch(/at most 64 keys/);
  });

  it("4a. sse required fields obey the path grammar", () => {
    const f = validForm();
    f.completionKind = "sse";
    f.sseSuccessEvent = "task.completed";
    f.sseRequiredFields = "a[0]";
    expect(at(f, "sseRequiredFields").map((r) => r.message).join()).toMatch(/no expressions/);
  });

  it("4b. poll failure is a PAIR — values without a path, or a path without values", () => {
    const f = validForm();
    f.pollFailureValues = "failed";
    f.pollFailurePath = "";
    expect(at(f, "pollFailurePath").map((r) => r.message).join()).toMatch(/required/);

    const g = validForm();
    g.pollFailurePath = "status";
    g.pollFailureValues = "";
    expect(at(g, "pollFailureValues").map((r) => r.message).join()).toMatch(/at least one value/);

    // The complete pair passes, which is what makes this a rule about the PAIR.
    const h = validForm();
    h.pollFailurePath = "status";
    h.pollFailureValues = "failed, cancelled";
    expect(at(h, "pollFailurePath")).toEqual([]);
    expect(at(h, "pollFailureValues")).toEqual([]);
  });

  it("4c. lifecycle_path is required ALWAYS, not only for a lifecycle_prefix cleanup", () => {
    const f = validForm();
    f.cleanupKind = "none";
    f.cleanupPrefix = "";
    f.cleanupAcknowledged = true;
    f.lifecyclePath = "";
    // `validateCanaryResult` runs the path grammar over it unconditionally, and "" fails that.
    expect(at(f, "lifecyclePath").map((r) => r.message).join()).toMatch(/required/);
  });

  it("4d. a field list refuses a duplicate and an over-long path", () => {
    const f = validForm();
    f.resultRequiredFields = "s3_path, s3_path";
    expect(at(f, "resultRequiredFields").map((r) => r.message).join()).toMatch(/listed twice/);

    const g = validForm();
    g.resultRequiredFields = "a".repeat(201);
    expect(at(g, "resultRequiredFields").map((r) => r.message).join()).toMatch(/at most 200 bytes/);
  });

  it("5. multipart forbids content-type, and an ordinary header needs a value", () => {
    const f = validForm();
    f.submitKind = "multipart_fixture";
    f.bodyFields = [];
    f.fixtureRef = "small_wav_v1";
    f.submitHeaders = [{ name: "content-type", value: "multipart/form-data", secretRef: "" }];
    expect(at(f, "submitHeaders.0").map((r) => r.message).join()).toMatch(/set by the runner/);

    const g = validForm();
    g.submitHeaders.push({ name: "x-empty", value: "", secretRef: "" });
    expect(at(g, "submitHeaders.2").map((r) => r.message).join()).toMatch(/has no value/);

    // A value AND a binding together is refused: the position is a union of one or the other.
    const h = validForm();
    h.submitHeaders[0] = { name: "authorization", value: "literal", secretRef: "upload" };
    expect(at(h, "submitHeaders.0").map((r) => r.message).join()).toMatch(/not both/);
  });

  it("on a NON-multipart submit, content-type is an ordinary header", () => {
    // The rule is about the multipart encoder owning the boundary, not about the name being magic.
    const f = validForm();
    f.submitHeaders.push({ name: "content-type", value: "application/json", secretRef: "" });
    expect(at(f, "submitHeaders.2")).toEqual([]);
  });
});
