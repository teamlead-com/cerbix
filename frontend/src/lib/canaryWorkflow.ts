// FR-029 phase F in the editor. A canary's workflow is a NESTED TYPED document with closed unions
// and no free-form field anywhere: no `settings` map, no JSON string, and a key the schema does not
// name refuses the whole thing by name.
//
// Every rule here is the SERVER's rule (`internal/domain/canary.go`, enforced from `Monitor.Validate`
// on every write surface). This file exists so the operator meets each one AT THE FIELD instead of as
// a 400 after saving — the approved mock's contract — and it deliberately invents no rule of its own:
// a client-only refusal would be a rule nobody could discover from the API, and a client-only
// permission would be a lie.
//
// What the client does NOT do is canonicalize. The stored document is one canonical string (D3e) and
// canonicalization belongs to the server on every write surface; duplicating it here would be a second
// implementation of a hash-relevant function, which is how two surfaces start disagreeing about
// identity. The form sends an ordinary JSON document and the server normalizes it.

export const CANARY_WORKFLOW_KIND = "async_transaction_v1";
export const CANARY_WORKFLOW_KEY = "workflow";

/** Bounds, character for character from `internal/domain/canary.go`. */
export const CANARY_MAX_BINDINGS = 8;
export const CANARY_MAX_HEADERS_PER_REQUEST = 16;
export const CANARY_MAX_HEADER_NAME_BYTES = 64;
export const CANARY_MAX_HEADER_VALUE_BYTES = 1024;
export const CANARY_MAX_MULTIPART_FIELDS = 16;
export const CANARY_MAX_REQUIRED_FIELDS = 16;
export const CANARY_MAX_FAILURE_VALUES = 8;
export const CANARY_MIN_SUBMIT_TIMEOUT = 1;
export const CANARY_MAX_SUBMIT_TIMEOUT = 60;
export const CANARY_MIN_POLL_INTERVAL = 1;
export const CANARY_MAX_POLL_INTERVAL = 60;
export const CANARY_MAX_POLL_ATTEMPTS = 600;
export const CANARY_MAX_JSON_PATH_BYTES = 200;
export const CANARY_MAX_JSON_PATH_DEPTH = 8;
export const CANARY_MAX_STRING_LEAF_BYTES = 1024;
export const CANARY_MAX_BODY_BYTES = 8 * 1024;
export const CANARY_MIN_POLL_ATTEMPTS = 1;

/**
 * Byte length, because the server counts BYTES.
 *
 * Go's `len(s)` is bytes; JavaScript's `.length` is UTF-16 code units, so `"é"` is one there and two
 * to Go. A client measuring `.length` accepts values the server refuses — and only for non-ASCII
 * input, which no happy-path test ever produces.
 */
export function canaryByteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

/**
 * A JSON encoder that agrees with Go's `encoding/json` on the two things that matter here.
 *
 * `JSON.stringify` disagrees with Go twice, and both showed up as client-valid 400s:
 *
 *  1. **Escaping.** Go HTML-escapes `<`, `>` and `&` into six-byte `\u003c` sequences (and escapes
 *     U+2028/U+2029), while `JSON.stringify` leaves them literal. So a body of `"<".repeat(1000)`
 *     rows measured ~8 KB here and ~48 KB to Go — a six-fold difference, not the "few bytes" an
 *     earlier comment claimed. Measuring the body with `JSON.stringify` could not enforce
 *     `CanaryMaxBodyBytes` at all.
 *  2. **Numbers.** A number must reach the wire as the operator's exact TOKEN. Routing it through a
 *     JS `Number` silently rewrote `9007199254740993` to `…92`, and turned a 400-digit value into
 *     `null` — a document the closed algebra refuses. The server had the same defect from the other
 *     direction until `UseNumber` was added.
 *
 * The shapes are the closed set this form builds: string, raw number token, boolean, array, object.
 */
export type CanaryJSON =
  | string
  | boolean
  | { __rawNumber: string }
  | CanaryJSON[]
  | { [k: string]: CanaryJSON };

const GO_ESCAPES: Record<string, string> = {
  '"': '\\"',
  "\\": "\\\\",
  "\n": "\\n",
  "\r": "\\r",
  "\t": "\\t",
  "<": "\\u003c",
  ">": "\\u003e",
  "&": "\\u0026",
  "\u2028": "\\u2028",
  "\u2029": "\\u2029",
};

function goQuote(s: string): string {
  let out = '"';
  for (const ch of s) {
    const mapped = GO_ESCAPES[ch];
    if (mapped !== undefined) {
      out += mapped;
      continue;
    }
    const code = ch.codePointAt(0)!;
    if (code < 0x20) {
      out += "\\u" + code.toString(16).padStart(4, "0");
      continue;
    }
    out += ch;
  }
  return out + '"';
}

/** A raw number token, preserved verbatim so no JS Number ever touches the operator's digits. */
export function canaryRawNumber(token: string): { __rawNumber: string } {
  return { __rawNumber: token };
}

function isRawNumber(v: CanaryJSON): v is { __rawNumber: string } {
  return typeof v === "object" && v !== null && !Array.isArray(v) && "__rawNumber" in v;
}

export function canaryEncode(v: CanaryJSON): string {
  if (typeof v === "string") return goQuote(v);
  if (typeof v === "boolean") return v ? "true" : "false";
  if (isRawNumber(v)) return v.__rawNumber;
  if (Array.isArray(v)) return "[" + v.map(canaryEncode).join(",") + "]";
  // Keys sorted, as Go marshals a map — so a measurement and a document do not depend on the order
  // rows happen to sit in.
  const keys = Object.keys(v).sort();
  return "{" + keys.map((k) => goQuote(k) + ":" + canaryEncode((v as Record<string, CanaryJSON>)[k])).join(",") + "}";
}

/** The two closed unions the form switches on. A `default` that accepts is an undocumented escape. */
export const CANARY_SUBMIT_KINDS = ["http_json", "multipart_fixture"] as const;
export const CANARY_COMPLETION_KINDS = ["sse", "poll_json"] as const;
export const CANARY_CORRELATE_SOURCES = ["response_json", "response_header"] as const;
export const CANARY_CLEANUP_KINDS = ["lifecycle_prefix", "none"] as const;

export type CanarySubmitKind = (typeof CANARY_SUBMIT_KINDS)[number];
export type CanaryCompletionKind = (typeof CANARY_COMPLETION_KINDS)[number];
export type CanaryCorrelateSource = (typeof CANARY_CORRELATE_SOURCES)[number];
export type CanaryCleanupKind = (typeof CANARY_CLEANUP_KINDS)[number];

/** `^[a-z][a-z0-9_-]{0,39}$` and `^[A-Za-z_][A-Za-z0-9_]*(\.(…|[0-9]+))*$` — the server's grammars. */
export const CANARY_BINDING_NAME_RE = /^[a-z][a-z0-9_-]{0,39}$/;
export const CANARY_JSON_PATH_RE = /^[A-Za-z_][A-Za-z0-9_]*(\.([A-Za-z_][A-Za-z0-9_]*|[0-9]+))*$/;
export const CANARY_HEADER_NAME_RE = /^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$/;

/**
 * The finite set whose value must be a BINDING rather than a literal (D7). It is the only enforceable
 * half of the rule: a credential is not detectable by shape, so one pasted into a header nobody would
 * call a credential header — or into a body — is legal and is NOT caught. The form says so where it
 * matters instead of implying a guarantee this set cannot give.
 */
export const CANARY_CREDENTIAL_HEADERS: readonly string[] = [
  "authorization",
  "proxy-authorization",
  "cookie",
  "x-api-key",
  "api-key",
  "x-auth-token",
  "auth-token",
  "x-access-token",
  "access-token",
  "private-token",
];

/** Names the RUNNER owns: it derives the idempotency key and the multipart boundary itself. */
export const CANARY_RESERVED_HEADERS: readonly string[] = [
  "idempotency-key",
  "host",
  "content-length",
  "transfer-encoding",
];

/** Legal in exactly ONE field — the completion URL — because nothing has produced an id at submit. */
export const CANARY_CORRELATION_PLACEHOLDER = "{{ correlation_id }}";

/**
 * The fixture REGISTRY, mirrored from `internal/domain/canarycanonical.go`. A fixture is a registry
 * key with a pinned SHA-256 and never an upload or a path, so the control is a select over this list
 * — a free-text box let a reviewer put `https://evil.example/file.wav` past the form and straight
 * into a 400 (P1, party [84]).
 */
export const CANARY_FIXTURES: readonly string[] = ["small_wav_v1"];

/** `^[A-Za-z_][A-Za-z0-9_.-]{0,63}$` — the server's body-key grammar. */
export const CANARY_BODY_KEY_RE = /^[A-Za-z_][A-Za-z0-9_.\-]{0,63}$/;
export const CANARY_MAX_BODY_KEYS = 64;

export const CANARY_SECRET_REF_PREFIX = "canary_secret_";
export const CANARY_SECRET_REF_SUFFIX = "_ref";

export function canarySecretRefKey(binding: string): string {
  return `${CANARY_SECRET_REF_PREFIX}${binding}${CANARY_SECRET_REF_SUFFIX}`;
}

// ── The form's own model ────────────────────────────────────────────────────────────────────────
// Flat and string-typed where a form field is a string, because that is what an input holds. The
// conversion to the document's types happens once, in `buildCanaryConfig`.

export type CanaryBinding = { name: string; secret: string };
export type CanaryHeaderRow = { name: string; value: string; secretRef: string };
export type CanaryFieldRow = { key: string; value: string; secretRef: string };

export type CanaryForm = {
  bindings: CanaryBinding[];
  submitKind: CanarySubmitKind;
  submitURL: string;
  submitTimeout: string;
  acceptedStatus: string;
  submitHeaders: CanaryHeaderRow[];
  fixtureRef: string;
  fileField: string;
  multipartFields: CanaryFieldRow[];
  bodyFields: CanaryFieldRow[];
  correlateSource: CanaryCorrelateSource;
  correlatePath: string;
  correlateHeaderName: string;
  completionKind: CanaryCompletionKind;
  completionURL: string;
  completionTimeout: string;
  completionHeaders: CanaryHeaderRow[];
  sseSuccessEvent: string;
  sseFailureEvents: string;
  sseRequiredFields: string;
  pollInterval: string;
  pollMaxAttempts: string;
  pollSuccessPath: string;
  pollSuccessValue: string;
  pollFailurePath: string;
  pollFailureValues: string;
  maxLatency: string;
  resultRequiredFields: string;
  lifecyclePath: string;
  cleanupKind: CanaryCleanupKind;
  cleanupPrefix: string;
  cleanupAcknowledged: boolean;
};

/** A refusal, and WHERE in the form it belongs — the one thing only a client can know. */
export type CanaryRefusal = { field: string; message: string };

export function emptyCanaryForm(): CanaryForm {
  return {
    bindings: [],
    submitKind: "http_json",
    submitURL: "",
    submitTimeout: "30",
    acceptedStatus: "202",
    submitHeaders: [],
    fixtureRef: "",
    fileField: "file",
    multipartFields: [],
    bodyFields: [],
    correlateSource: "response_json",
    correlatePath: "",
    correlateHeaderName: "",
    completionKind: "poll_json",
    completionURL: "",
    completionTimeout: "",
    completionHeaders: [],
    sseSuccessEvent: "",
    sseFailureEvents: "",
    sseRequiredFields: "",
    pollInterval: "5",
    pollMaxAttempts: "60",
    pollSuccessPath: "status",
    pollSuccessValue: "completed",
    pollFailurePath: "",
    pollFailureValues: "",
    maxLatency: "",
    resultRequiredFields: "",
    lifecyclePath: "",
    cleanupKind: "lifecycle_prefix",
    cleanupPrefix: "",
    cleanupAcknowledged: false,
  };
}

export function isCredentialHeader(name: string): boolean {
  return CANARY_CREDENTIAL_HEADERS.includes(name.trim().toLowerCase());
}

export function isReservedHeader(name: string): boolean {
  return CANARY_RESERVED_HEADERS.includes(name.trim().toLowerCase());
}

/** Comma/space separated list → trimmed non-empty entries, in the order typed. */
export function splitList(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter((s) => s !== "");
}

/**
 * `{{ correlation_id }}` must occupy ONE WHOLE PATH SEGMENT — never a fragment of one, never inside
 * the query, never in the host. Mirrors `canaryPlaceholderIsWholeSegment`.
 *
 * The client used to substitute every marker and then parse, which accepted `.../x{{ correlation_id }}`
 * and two markers in one URL: both are refused by the server (P1, party [84]).
 */
function placeholderIsWholeSegment(raw: string): boolean {
  const idx = raw.indexOf(CANARY_CORRELATION_PLACEHOLDER);
  if (idx < 0) return false;
  const q = raw.search(/[?#]/);
  if (q >= 0 && idx > q) return false;
  const before = raw.slice(0, idx);
  const after = raw.slice(idx + CANARY_CORRELATION_PLACEHOLDER.length);
  if (!before.endsWith("/")) return false;
  if (before.endsWith("://")) return false; // that would put it in the host
  return after === "" || after.startsWith("/") || after.startsWith("?") || after.startsWith("#");
}

function hostOf(url: string): string | null {
  try {
    const u = new URL(url);
    return `${u.hostname.toLowerCase()}:${u.port || (u.protocol === "https:" ? "443" : "80")}`;
  } catch {
    return null;
  }
}

// ── Refusals, positioned ───────────────────────────────────────────────────────────────────────

function headerRefusals(
  rows: CanaryHeaderRow[],
  where: string,
  bindings: CanaryBinding[],
  multipart = false,
): CanaryRefusal[] {
  const out: CanaryRefusal[] = [];
  const seen = new Set<string>();
  if (rows.length > CANARY_MAX_HEADERS_PER_REQUEST) {
    out.push({ field: where, message: `at most ${CANARY_MAX_HEADERS_PER_REQUEST} headers` });
  }
  rows.forEach((h, i) => {
    const name = h.name.trim().toLowerCase();
    const at = `${where}.${i}`;
    if (name === "") {
      out.push({ field: at, message: "a header needs a name" });
      return;
    }
    if (!CANARY_HEADER_NAME_RE.test(name)) {
      out.push({ field: at, message: `${name} is not a valid header name` });
    }
    if (canaryByteLength(name) > CANARY_MAX_HEADER_NAME_BYTES) {
      out.push({ field: at, message: `a header name is at most ${CANARY_MAX_HEADER_NAME_BYTES} bytes` });
    }
    // Case-insensitive duplicates: the canonical form is what the digest covers, so two headers
    // differing only in case are one header with two values and the schema refuses them.
    if (seen.has(name)) {
      out.push({ field: at, message: `${name} is declared twice (header names are case-insensitive)` });
    }
    seen.add(name);
    // `content-type` is the runner's on a multipart submit: its own encoder owns the boundary, so an
    // author-supplied value produces a body that does not parse.
    if (isReservedHeader(name) || (multipart && name === "content-type")) {
      out.push({ field: at, message: `${name} is set by the runner and may not be declared` });
    }
    if (isCredentialHeader(name)) {
      // D7, taught by the control: this header carries a BINDING and nothing else. The refusal
      // never echoes the value — a validation message is a place a credential leaks exactly as a
      // log line is.
      if (h.value.trim() !== "") {
        out.push({ field: at, message: `${name} carries a credential: reference a binding, never a value` });
      }
      if (h.secretRef.trim() === "") {
        out.push({ field: at, message: `${name} needs a binding` });
      }
    } else if (h.secretRef.trim() === "") {
      // An ordinary header with no value is refused by the server: a header that says nothing is not
      // a header, and the schema has no place for one.
      if (h.value === "") {
        out.push({ field: at, message: `${name} has no value` });
      }
      if (canaryByteLength(h.value) > CANARY_MAX_HEADER_VALUE_BYTES) {
        out.push({ field: at, message: `a header value is at most ${CANARY_MAX_HEADER_VALUE_BYTES} bytes` });
      }
    }
    if (h.secretRef.trim() !== "" && h.value !== "") {
      out.push({ field: at, message: `${name} takes a value or a binding, not both` });
    }
    const ref = h.secretRef.trim();
    if (ref !== "" && !bindings.some((b) => b.name === ref)) {
      out.push({ field: at, message: `no binding named ${ref} is declared` });
    }
  });
  return out;
}

/** Mirrors `validateCanaryFieldList`: a bound, the path grammar per entry, and no duplicates. */
function fieldListRefusals(raw: string, field: string, what: string, required: boolean): CanaryRefusal[] {
  const out: CanaryRefusal[] = [];
  const list = splitList(raw);
  if (required && list.length === 0) {
    out.push({ field, message: `${what} must name at least one field` });
  }
  if (list.length > CANARY_MAX_REQUIRED_FIELDS) {
    out.push({ field, message: `at most ${CANARY_MAX_REQUIRED_FIELDS} paths` });
  }
  const seen = new Set<string>();
  for (const p of list) {
    // The SAME checker every other path uses, so a rule added there cannot be missing here — which
    // is how the depth bound came to be enforced in one place and not the other.
    const bad = pathRefusal(p, field, `path ${p}`);
    if (bad) out.push(bad);
    if (seen.has(p)) out.push({ field, message: `${p} is listed twice` });
    seen.add(p);
  }
  return out;
}

/** Mirrors `validateCanaryBody`: the key grammar, the key bound, and no blank or duplicate key. */
function fieldRowRefusals(rows: CanaryFieldRow[], field: string, bindings: CanaryBinding[]): CanaryRefusal[] {
  const out: CanaryRefusal[] = [];
  if (rows.length > CANARY_MAX_BODY_KEYS) {
    out.push({ field, message: `at most ${CANARY_MAX_BODY_KEYS} keys` });
  }
  const seen = new Set<string>();
  rows.forEach((row, i) => {
    const k = row.key.trim();
    const at = `${field}.${i}`;
    // A blank or duplicate key used to be dropped or overwritten in silence by `fieldMap`, so the
    // typed controls could change what the operator meant without saying anything (P1, party [84]).
    if (k === "") {
      out.push({ field: at, message: "a field needs a key" });
    } else if (!CANARY_BODY_KEY_RE.test(k)) {
      out.push({ field: at, message: `${k} is not a valid key` });
    }
    if (k !== "" && seen.has(k)) out.push({ field: at, message: `${k} is declared twice` });
    seen.add(k);
    const ref = row.secretRef.trim();
    if (ref !== "" && !bindings.some((b) => b.name === ref)) {
      out.push({ field: at, message: `no binding named ${ref} is declared` });
    }
    if (ref !== "" && row.value.trim() !== "") {
      out.push({ field: at, message: `${k || "a field"} takes a value or a binding, not both` });
    }
    // A string LEAF is bounded in bytes by `validateCanaryValue`. Only a string leaf: a row that
    // parses as a number or a boolean is not a string, and the form types it as such.
    if (ref === "" && !isTypedScalar(row.value) && canaryByteLength(row.value) > CANARY_MAX_STRING_LEAF_BYTES) {
      out.push({ field: at, message: `a value is at most ${CANARY_MAX_STRING_LEAF_BYTES} bytes` });
    }
    // A number token must be REPRESENTABLE: the server calls `json.Number.Float64()` and refuses a
    // token that overflows, so a 400-digit value is not a number to it either. The token is still
    // preserved verbatim for everything inside the range, including values a float64 cannot hold
    // EXACTLY — `Float64()` succeeds there and the digits reach the target unchanged.
    if (ref === "" && isNumberToken(row.value) && !Number.isFinite(Number(row.value.trim()))) {
      out.push({ field: at, message: `${k || "a value"} is out of range for a number` });
    }
  });
  // The encoded body as a whole, measured with the SAME escaping Go uses. `JSON.stringify` leaves
  // `<`, `>` and `&` literal where Go writes six-byte `\u003c` sequences, so a body of angle
  // brackets measured here at 8 KB and to Go at 48 KB — the earlier "within a few bytes" comment was
  // wrong by six times and the bound was unenforceable.
  const encoded = canaryByteLength(canaryEncode(fieldMap(rows)));
  if (encoded > CANARY_MAX_BODY_BYTES) {
    out.push({ field, message: `the body is at most ${CANARY_MAX_BODY_BYTES} bytes encoded` });
  }
  return out;
}

/** True when a value string will be emitted as a NUMBER token rather than a string leaf. */
function isNumberToken(raw: string): boolean {
  const v = raw.trim();
  return v !== "" && /^-?(0|[1-9][0-9]*)(\.[0-9]+)?$/.test(v);
}

/** True when a value string will be emitted as a number or a boolean rather than a string leaf. */
function isTypedScalar(raw: string): boolean {
  const v = raw.trim();
  return v === "true" || v === "false" || isNumberToken(v);
}

/** Mirrors `validateCanaryJSONPath` in full: presence, BYTE length, grammar, and DEPTH. */
function pathRefusal(raw: string, field: string, what: string): CanaryRefusal | null {
  const p = raw.trim();
  if (p === "") return { field, message: `${what} is required` };
  if (canaryByteLength(p) > CANARY_MAX_JSON_PATH_BYTES) {
    return { field, message: `${what} is at most ${CANARY_MAX_JSON_PATH_BYTES} bytes` };
  }
  if (!CANARY_JSON_PATH_RE.test(p)) {
    return { field, message: `${what} uses dotted keys and numeric indices only — no expressions` };
  }
  // The depth bound was omitted for two review rounds while the comment claimed the mirror was
  // complete: a nine-segment path passed the form and met a 400.
  if (p.split(".").length > CANARY_MAX_JSON_PATH_DEPTH) {
    return { field, message: `${what} is at most ${CANARY_MAX_JSON_PATH_DEPTH} segments deep` };
  }
  return null;
}

function urlRefusals(raw: string, field: string, allowPlaceholder: boolean): CanaryRefusal[] {
  const out: CanaryRefusal[] = [];
  const url = raw.trim();
  if (url === "") {
    out.push({ field, message: "a URL is required" });
    return out;
  }
  // The placeholder check comes FIRST: a URL carrying it does not parse as one, so parsing it would
  // report "not a valid URL" for a document whose real problem is the position of the placeholder.
  const withoutPlaceholder = url.split(CANARY_CORRELATION_PLACEHOLDER).join("x");
  const markers = url.split(CANARY_CORRELATION_PLACEHOLDER).length - 1;
  if (markers > 0 && !allowPlaceholder) {
    out.push({
      field,
      message: `${CANARY_CORRELATION_PLACEHOLDER} is only legal in the completion URL — nothing has produced an id yet here`,
    });
  }
  if (/\{\{/.test(withoutPlaceholder)) {
    out.push({ field, message: "no other placeholder is legal anywhere in a canary" });
  }
  if (allowPlaceholder && markers > 1) {
    out.push({ field, message: `at most one ${CANARY_CORRELATION_PLACEHOLDER} is allowed` });
  }
  if (allowPlaceholder && markers === 1 && !placeholderIsWholeSegment(url)) {
    out.push({
      field,
      message: `${CANARY_CORRELATION_PLACEHOLDER} must occupy one whole path segment`,
    });
  }
  let parsed: URL | null = null;
  try {
    parsed = new URL(withoutPlaceholder);
  } catch {
    out.push({ field, message: "not a valid absolute URL" });
    return out;
  }
  if (parsed.protocol !== "https:") {
    out.push({ field, message: "HTTPS only" });
  }
  if (parsed.username !== "" || parsed.password !== "") {
    out.push({ field, message: "a URL may not carry credentials in its userinfo" });
  }
  return out;
}

/**
 * Every refusal the form can raise before saving, each positioned at the field it belongs to.
 *
 * `monitorTimeout` is the monitor's own `timeout_seconds`, because several bounds are expressed
 * against it rather than against a constant: the promise and the stage budgets have to fit inside the
 * probe that carries them.
 */
export function canaryRefusals(f: CanaryForm, monitorTimeout: number, projectSecrets: string[]): CanaryRefusal[] {
  const out: CanaryRefusal[] = [];

  // ── bindings, declared once ──
  if (f.bindings.length > CANARY_MAX_BINDINGS) {
    out.push({ field: "bindings", message: `at most ${CANARY_MAX_BINDINGS} bindings` });
  }
  const names = new Set<string>();
  f.bindings.forEach((b, i) => {
    const at = `bindings.${i}`;
    if (!CANARY_BINDING_NAME_RE.test(b.name)) {
      out.push({ field: at, message: "a binding name is lower-case, starts with a letter, up to 40 chars" });
    }
    if (names.has(b.name)) out.push({ field: at, message: `${b.name} is declared twice` });
    names.add(b.name);
    if (b.secret.trim() === "") {
      out.push({ field: at, message: `binding ${b.name} names no project secret` });
    } else if (projectSecrets.length > 0 && !projectSecrets.includes(b.secret)) {
      // One of the two things only a client can know: which secrets exist right now.
      out.push({ field: at, message: `no project secret named ${b.secret}` });
    }
  });

  // ── submit ──
  out.push(...urlRefusals(f.submitURL, "submitURL", false));
  const st = Number(f.submitTimeout);
  if (!Number.isInteger(st) || st < CANARY_MIN_SUBMIT_TIMEOUT || st > CANARY_MAX_SUBMIT_TIMEOUT) {
    out.push({
      field: "submitTimeout",
      message: `between ${CANARY_MIN_SUBMIT_TIMEOUT} and ${CANARY_MAX_SUBMIT_TIMEOUT} seconds`,
    });
  } else if (monitorTimeout > 0 && st > monitorTimeout) {
    out.push({ field: "submitTimeout", message: "must not exceed the monitor's timeout" });
  }
  const statuses = splitList(f.acceptedStatus).map(Number);
  if (statuses.length === 0) {
    out.push({ field: "acceptedStatus", message: "name at least one accepted status" });
  }
  statuses.forEach((code) => {
    if (!Number.isInteger(code) || code < 200 || code > 299) {
      out.push({ field: "acceptedStatus", message: "accepted statuses are 2xx" });
    }
  });
  if (new Set(statuses).size !== statuses.length) {
    out.push({ field: "acceptedStatus", message: "a status is listed twice" });
  }
  out.push(...headerRefusals(f.submitHeaders, "submitHeaders", f.bindings, f.submitKind === "multipart_fixture"));

  if (f.submitKind === "multipart_fixture") {
    const fx = f.fixtureRef.trim();
    if (fx === "") {
      out.push({ field: "fixtureRef", message: "a fixture is required — it is a registry key, never an upload" });
    } else if (!CANARY_FIXTURES.includes(fx)) {
      // The registry is the whole contract: the runner carries the bytes and verifies a pinned
      // SHA-256 before uploading them, so a key it does not have is not a fixture.
      out.push({ field: "fixtureRef", message: `${fx} is not a fixture the runner carries` });
    }
    if (f.fileField.trim() === "") {
      out.push({ field: "fileField", message: "multipart needs a file field name" });
    }
    if (f.multipartFields.length > CANARY_MAX_MULTIPART_FIELDS) {
      out.push({ field: "multipartFields", message: `at most ${CANARY_MAX_MULTIPART_FIELDS} fields` });
    }
    out.push(...fieldRowRefusals(f.multipartFields, "multipartFields", f.bindings));
  } else {
    if (f.bodyFields.length === 0) {
      out.push({ field: "bodyFields", message: "http_json needs a body" });
    }
    out.push(...fieldRowRefusals(f.bodyFields, "bodyFields", f.bindings));
  }

  // ── correlate: the two grammars are source-specific ──
  if (f.correlateSource === "response_json") {
    const bad = pathRefusal(f.correlatePath, "correlatePath", "the correlation path");
    if (bad) out.push(bad);
  } else if (!CANARY_HEADER_NAME_RE.test(f.correlateHeaderName.trim().toLowerCase())) {
    out.push({ field: "correlateHeaderName", message: "not a valid header name" });
  }

  // ── completion ──
  out.push(...urlRefusals(f.completionURL, "completionURL", true));
  const ct = Number(f.completionTimeout);
  if (!Number.isInteger(ct) || ct <= 0) {
    out.push({ field: "completionTimeout", message: "must be positive" });
  } else if (monitorTimeout > 0 && ct > monitorTimeout) {
    out.push({ field: "completionTimeout", message: "must not exceed the monitor's timeout" });
  }
  out.push(...headerRefusals(f.completionHeaders, "completionHeaders", f.bindings));
  // D3c1: a completion header may carry a binding only when the completion host equals the submit
  // host. Sending the submit credential to a different origin is the mistake the rule forbids.
  const boundCompletion = f.completionHeaders.some((h) => h.secretRef.trim() !== "");
  if (boundCompletion) {
    const a = hostOf(f.submitURL.trim());
    const b = hostOf(f.completionURL.split(CANARY_CORRELATION_PLACEHOLDER).join("x").trim());
    if (a && b && a !== b) {
      out.push({
        field: "completionHeaders",
        message: "a binding here requires the completion host to equal the submit host",
      });
    }
  }

  if (f.completionKind === "sse") {
    if (f.sseSuccessEvent.trim() === "") {
      out.push({ field: "sseSuccessEvent", message: "an sse completion needs its success event" });
    }
    out.push(...fieldListRefusals(f.sseRequiredFields, "sseRequiredFields", "sse required_json_fields", false));
    if (splitList(f.sseFailureEvents).length > CANARY_MAX_FAILURE_VALUES) {
      out.push({ field: "sseFailureEvents", message: `at most ${CANARY_MAX_FAILURE_VALUES} failure events` });
    }
  } else {
    const pi = Number(f.pollInterval);
    const pa = Number(f.pollMaxAttempts);
    if (!Number.isInteger(pi) || pi < CANARY_MIN_POLL_INTERVAL || pi > CANARY_MAX_POLL_INTERVAL) {
      out.push({
        field: "pollInterval",
        message: `between ${CANARY_MIN_POLL_INTERVAL} and ${CANARY_MAX_POLL_INTERVAL} seconds`,
      });
    }
    if (!Number.isInteger(pa) || pa < CANARY_MIN_POLL_ATTEMPTS || pa > CANARY_MAX_POLL_ATTEMPTS) {
      out.push({
        field: "pollMaxAttempts",
        message: `between ${CANARY_MIN_POLL_ATTEMPTS} and ${CANARY_MAX_POLL_ATTEMPTS}`,
      });
    }
    if (Number.isInteger(pi) && Number.isInteger(pa) && Number.isInteger(ct) && pi * pa > ct) {
      out.push({
        field: "pollMaxAttempts",
        message: "interval × attempts must fit inside the completion timeout",
      });
    }
    const badSuccess = pathRefusal(f.pollSuccessPath, "pollSuccessPath", "the success path");
    if (badSuccess) out.push(badSuccess);
    if (f.pollSuccessValue.trim() === "") {
      out.push({ field: "pollSuccessValue", message: "a success value is required" });
    }
    // Poll failure is a PAIR: a path with no values, or values with no path, is refused by the
    // server — half a failure condition can never match anything.
    const fVals = splitList(f.pollFailureValues);
    const fPath = f.pollFailurePath.trim();
    if (fPath !== "" || fVals.length > 0) {
      const bad = pathRefusal(f.pollFailurePath, "pollFailurePath", "the failure path");
      if (bad) out.push(bad);
      if (fVals.length === 0) {
        out.push({ field: "pollFailureValues", message: "a failure path needs at least one value" });
      }
    }
    if (fVals.length > CANARY_MAX_FAILURE_VALUES) {
      out.push({ field: "pollFailureValues", message: `at most ${CANARY_MAX_FAILURE_VALUES} values` });
    }
  }

  // ── result: the promise is inside the limit ──
  const ml = Number(f.maxLatency);
  if (!Number.isInteger(ml) || ml <= 0) {
    out.push({ field: "maxLatency", message: "must be positive" });
  } else if (monitorTimeout > 0 && ml > monitorTimeout) {
    out.push({ field: "maxLatency", message: "the promise must fit inside the monitor's timeout" });
  }
  out.push(...fieldListRefusals(f.resultRequiredFields, "resultRequiredFields", "required_json_fields", true));
  // ALWAYS required, not only for a lifecycle_prefix cleanup: `validateCanaryResult` runs the path
  // grammar over it unconditionally, and an empty path fails that. The client used to treat it as
  // optional, which made `cleanup.kind: none` with no lifecycle path a client-valid 400.
  {
    const bad = pathRefusal(f.lifecyclePath, "lifecyclePath", "the lifecycle path");
    if (bad) out.push(bad);
  }

  // ── cleanup: D10 ──
  if (f.cleanupKind === "lifecycle_prefix") {
    if (f.cleanupPrefix.trim() === "") {
      out.push({ field: "cleanupPrefix", message: "a lifecycle prefix is required" });
    } else if (canaryByteLength(f.cleanupPrefix) > CANARY_MAX_STRING_LEAF_BYTES) {
      out.push({ field: "cleanupPrefix", message: `a prefix is at most ${CANARY_MAX_STRING_LEAF_BYTES} bytes` });
    }

  } else if (!f.cleanupAcknowledged) {
    out.push({
      field: "cleanupAcknowledged",
      message: "cleanup: none must be acknowledged — nothing sweeps what this canary creates",
    });
  }

  // ── every declared binding is used somewhere ──
  const used = new Set<string>();
  for (const h of [...f.submitHeaders, ...f.completionHeaders]) {
    if (h.secretRef.trim() !== "") used.add(h.secretRef.trim());
  }
  for (const row of [...f.bodyFields, ...f.multipartFields]) {
    if (row.secretRef.trim() !== "") used.add(row.secretRef.trim());
  }
  f.bindings.forEach((b, i) => {
    if (!used.has(b.name)) {
      out.push({ field: `bindings.${i}`, message: `binding ${b.name} is declared and never used` });
    }
  });

  return out;
}

// ── What goes on the wire ──────────────────────────────────────────────────────────────────────

/**
 * A typed scalar from a form field. Numbers and booleans are recognised so the document carries the
 * type the schema expects; everything else is a string. A `secretRef` row becomes a
 * `{secret_ref: <binding>}` node, which is the ONLY way a credential enters a body (D3a) — and the
 * residual is stated where the operator can see it: a credential pasted as an ordinary string leaf is
 * not detectable and is not refused.
 */
function typedValue(row: CanaryFieldRow): CanaryJSON {
  const ref = row.secretRef.trim();
  if (ref !== "") return { secret_ref: ref };
  const raw = row.value.trim();
  if (raw === "true") return true;
  if (raw === "false") return false;
  // The operator's exact DIGITS, never `Number(raw)`: coercion rewrote `9007199254740993` to `…92`
  // and turned a 400-digit value into `null`, which the closed algebra refuses. A token that a
  // JS number cannot hold is still a legal JSON number, and the server keeps it verbatim.
  if (isNumberToken(raw)) return canaryRawNumber(raw);
  return row.value;
}

function headerDocs(rows: CanaryHeaderRow[]): CanaryJSON[] {
  return rows.map((h): CanaryJSON => {
    const name = h.name.trim().toLowerCase();
    const ref = h.secretRef.trim();
    // A header is a binding OR a value, never both: the union is closed here exactly as it is in the
    // schema, so a row cannot reach the wire carrying an empty `value` beside a `secret_ref`.
    const doc: { [k: string]: CanaryJSON } = { name };
    if (ref !== "") doc.secret_ref = ref;
    else doc.value = h.value;
    return doc;
  });
}

function fieldMap(rows: CanaryFieldRow[]): { [k: string]: CanaryJSON } {
  const out: { [k: string]: CanaryJSON } = {};
  for (const row of rows) {
    const k = row.key.trim();
    if (k !== "") out[k] = typedValue(row);
  }
  return out;
}

/**
 * The monitor `config` a canary is written with: the workflow document under one key, and one flat
 * `canary_secret_<binding>_ref` per binding.
 *
 * `workflow.secrets` is INPUT-ONLY (D3f) and is deliberately NOT in the document: the stored document
 * keeps the `secret_ref` markers and no project-secret name at all, so a rename touches one flat key
 * and the document cannot go stale behind it. A client that sent only the document would create a
 * monitor whose bindings resolve to nothing.
 *
 * The document is ordinary JSON, not canonical JSON. Canonicalization is the server's on every write
 * surface; a second implementation here is how two surfaces start disagreeing about identity.
 */
export function buildCanaryConfig(f: CanaryForm): Record<string, string> {
  const submit: { [k: string]: CanaryJSON } = {
    kind: f.submitKind,
    method: "POST",
    url: f.submitURL.trim(),
    submit_timeout: canaryRawNumber(String(Number(f.submitTimeout))),
    accepted_status: splitList(f.acceptedStatus).map((c) => canaryRawNumber(String(Number(c)))),
    headers: headerDocs(f.submitHeaders),
  };
  if (f.submitKind === "multipart_fixture") {
    // FLAT on `submit`, not nested under `multipart`: that is the shape of the persisted canonical
    // document (`canonicalSubmit.FileField` / `.Fields`), and the parser refuses unknown fields. The
    // first version of this nested them, so the ENTIRE multipart arm of the form built a document
    // the server rejected with `unknown field "multipart"` — and 44 client tests never saw it,
    // because none of them ran the document through the server. Found by the cross-surface seam the
    // reviewer's P1 required (party [84]); it is the sixth defect of that P1 and was not on the list.
    submit.fixture_ref = f.fixtureRef.trim();
    submit.file_field = f.fileField.trim();
    submit.fields = fieldMap(f.multipartFields);
  } else {
    submit.body = fieldMap(f.bodyFields);
  }

  const correlate: { [k: string]: CanaryJSON } =
    f.correlateSource === "response_json"
      ? { source: "response_json", path: f.correlatePath.trim() }
      : { source: "response_header", header_name: f.correlateHeaderName.trim().toLowerCase() };

  const completion: { [k: string]: CanaryJSON } = {
    kind: f.completionKind,
    url: f.completionURL.trim(),
    timeout: canaryRawNumber(String(Number(f.completionTimeout))),
    headers: headerDocs(f.completionHeaders),
  };
  if (f.completionKind === "sse") {
    completion.sse = {
      success_event: f.sseSuccessEvent.trim(),
      failure_events: splitList(f.sseFailureEvents),
      required_json_fields: splitList(f.sseRequiredFields),
    };
  } else {
    completion.poll = {
      interval: canaryRawNumber(String(Number(f.pollInterval))),
      max_attempts: canaryRawNumber(String(Number(f.pollMaxAttempts))),
      success_path: f.pollSuccessPath.trim(),
      success_value: f.pollSuccessValue.trim(),
      failure_path: f.pollFailurePath.trim(),
      failure_values: splitList(f.pollFailureValues),
    };
  }

  const doc: { [k: string]: CanaryJSON } = {
    kind: CANARY_WORKFLOW_KIND,
    submit,
    correlate,
    completion,
    result: {
      max_latency: canaryRawNumber(String(Number(f.maxLatency))),
      required_json_fields: splitList(f.resultRequiredFields),
      lifecycle_path: f.lifecyclePath.trim(),
    },
    cleanup: {
      kind: f.cleanupKind,
      prefix: f.cleanupKind === "lifecycle_prefix" ? f.cleanupPrefix.trim() : "",
      acknowledged: f.cleanupAcknowledged,
    },
  };

  const config: Record<string, string> = { [CANARY_WORKFLOW_KEY]: canaryEncode(doc) };
  for (const b of f.bindings) {
    if (b.name !== "" && b.secret.trim() !== "") config[canarySecretRefKey(b.name)] = b.secret.trim();
  }
  return config;
}

/**
 * Reading a saved canary back into the form. The document holds `secret_ref` markers and the flat keys
 * hold the project-secret names, so the two halves are recombined here — the same shape the server's
 * `ParseCanaryConfig` reconstructs, and the reason the read view needs no JSON editor to be complete.
 */
export function parseCanaryConfig(config: Record<string, string>): CanaryForm | null {
  const raw = config[CANARY_WORKFLOW_KEY];
  if (!raw) return null;
  let doc: Record<string, any>;
  try {
    doc = JSON.parse(raw);
  } catch {
    return null;
  }
  const f = emptyCanaryForm();
  const headerRows = (arr: any): CanaryHeaderRow[] =>
    Array.isArray(arr)
      ? arr.map((h: any) => ({
          name: String(h?.name ?? ""),
          value: typeof h?.value === "string" ? h.value : "",
          secretRef: typeof h?.secret_ref === "string" ? h.secret_ref : "",
        }))
      : [];
  const fieldRows = (obj: any): CanaryFieldRow[] =>
    obj && typeof obj === "object"
      ? Object.keys(obj).map((k) => {
          const v = obj[k];
          if (v && typeof v === "object" && typeof v.secret_ref === "string") {
            return { key: k, value: "", secretRef: v.secret_ref };
          }
          return { key: k, value: v === null || v === undefined ? "" : String(v), secretRef: "" };
        })
      : [];

  for (const key of Object.keys(config)) {
    if (key.startsWith(CANARY_SECRET_REF_PREFIX) && key.endsWith(CANARY_SECRET_REF_SUFFIX)) {
      const name = key.slice(CANARY_SECRET_REF_PREFIX.length, key.length - CANARY_SECRET_REF_SUFFIX.length);
      if (name !== "") f.bindings.push({ name, secret: config[key] });
    }
  }

  const s = doc.submit ?? {};
  f.submitKind = CANARY_SUBMIT_KINDS.includes(s.kind) ? s.kind : "http_json";
  f.submitURL = String(s.url ?? "");
  f.submitTimeout = String(s.submit_timeout ?? "");
  f.acceptedStatus = Array.isArray(s.accepted_status) ? s.accepted_status.join(", ") : "";
  f.submitHeaders = headerRows(s.headers);
  f.fixtureRef = String(s.fixture_ref ?? "");
  f.fileField = String(s.file_field ?? "file");
  f.multipartFields = fieldRows(s.fields);
  f.bodyFields = fieldRows(s.body);

  const c = doc.correlate ?? {};
  f.correlateSource = CANARY_CORRELATE_SOURCES.includes(c.source) ? c.source : "response_json";
  f.correlatePath = String(c.path ?? "");
  f.correlateHeaderName = String(c.header_name ?? "");

  const cp = doc.completion ?? {};
  f.completionKind = CANARY_COMPLETION_KINDS.includes(cp.kind) ? cp.kind : "poll_json";
  f.completionURL = String(cp.url ?? "");
  f.completionTimeout = String(cp.timeout ?? "");
  f.completionHeaders = headerRows(cp.headers);
  f.sseSuccessEvent = String(cp.sse?.success_event ?? "");
  f.sseFailureEvents = Array.isArray(cp.sse?.failure_events) ? cp.sse.failure_events.join(", ") : "";
  f.sseRequiredFields = Array.isArray(cp.sse?.required_json_fields) ? cp.sse.required_json_fields.join(", ") : "";
  f.pollInterval = String(cp.poll?.interval ?? "");
  f.pollMaxAttempts = String(cp.poll?.max_attempts ?? "");
  f.pollSuccessPath = String(cp.poll?.success_path ?? "");
  f.pollSuccessValue = String(cp.poll?.success_value ?? "");
  f.pollFailurePath = String(cp.poll?.failure_path ?? "");
  f.pollFailureValues = Array.isArray(cp.poll?.failure_values) ? cp.poll.failure_values.join(", ") : "";

  const r = doc.result ?? {};
  f.maxLatency = String(r.max_latency ?? "");
  f.resultRequiredFields = Array.isArray(r.required_json_fields) ? r.required_json_fields.join(", ") : "";
  f.lifecyclePath = String(r.lifecycle_path ?? "");

  const cl = doc.cleanup ?? {};
  f.cleanupKind = CANARY_CLEANUP_KINDS.includes(cl.kind) ? cl.kind : "lifecycle_prefix";
  f.cleanupPrefix = String(cl.prefix ?? "");
  f.cleanupAcknowledged = cl.acknowledged === true;

  return f;
}
