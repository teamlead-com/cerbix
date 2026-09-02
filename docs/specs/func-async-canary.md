# func-async-canary — a typed external canary for async API journeys (FR-029 / NFR-024)

> **Lifecycle: DESIGN — revision 1, NOT approved.** Opened 2026-09-03 from the owner's brief. Nothing
> here is implementable until this document has been reviewed and the owner has answered §11. FR-029
> and NFR-024 get their rows in `docs/status.md` when this design is approved, and the phases in §10
> are separate iterations, not one.
>
> **What this is NOT, said first because the name invites it:** not a browser runner, not Playwright or
> k6, not a queue consumer, not a webhook receiver, not shell execution, not a plugin host, and not a
> workflow DSL. It is ONE closed workflow shape — submit, correlate, await a terminal result, assert —
> expressed in a typed schema with no free-form escape hatch.

## 0. Why a new type instead of extending `synthetic`

`synthetic` is an untyped document: a JSON blob in one config key. Everything the product has had to
say about that blob since has been said by GUESSING at it. FR-028 is the proof — D7 cannot detect a
credential by value, so it falls back to a rule about header NAMES, and the cost landed two weeks
later: the ordinary canary journey (`POST /login` → extract a token → `Authorization: Bearer
{{token}}`) is now refused, because the rule cannot tell an extracted runtime value from a pasted
secret. A schema can: a field either takes a `secret_ref` or it does not, and nothing has to be
guessed.

Three more consequences follow from the same root, and each is a thing this design gets for free:

- **Monitoring-as-Code.** `synthetic` cannot live in a bundle (FR-028 D9) because a flat
  `settings map[string]string` cannot carry a nested scenario, and admitting it as a JSON STRING would
  put an unvalidated document inside a validated bundle — the file provider would be guessing again.
  A nested typed schema is what makes MaC possible at all, and it is MaC-first here (§7).
- **A semantic hash that means something.** Today the scenario's hash is over bytes. With a typed
  document, "the semantics changed" is a statement about fields, and canonicalization can be defined
  rather than approximated (§5).
- **Bounded blast radius.** Every limit in §6 is a field with a type and a maximum, checked at the
  write boundary. In an untyped document the same limits have to be re-derived at execution time by
  whoever remembers.

Extending `synthetic` toward this shape would mean re-deriving all four properties inside a blob. This
document takes the other road, and the price is honest: a second monitor type with its own schema,
its own executor and its own tests.

## 1. The product goal, in one path

    submit → correlation id → await terminal result → assertions → normalized heartbeat

First use case, and the one the contract is proven against: **Charla Files** — upload a fixture as
multipart, receive `202` with a `task_id`, await an SSE `task.completed`, and assert that the result
carries `s3_path`, `byte_size` and `media_type`.

What an operator gets is an ordinary cerbix monitor: it belongs to a project and a region, it produces
heartbeats, it can be a member of a Service and feed its SLI, it alerts and escalates like everything
else. **No second reliability model.**

## 2. Requirements

- **FR-029 — a typed external canary runs one async transaction and reports it as an ordinary
  monitor.** A new monitor type `async_canary` carries exactly one workflow kind,
  `async_transaction_v1`, declared in a nested typed schema with closed unions and no free-form
  fields. It is executed by a capability-announcing executor in the monitor's region, submits with a
  stable idempotency key, correlates by a declared path, awaits a terminal outcome by SSE or by
  polling, asserts declared fields, validates the cleanup boundary, and returns ONE heartbeat carrying
  up/down, total latency, the failed stage and a bounded code class. It is declarable in a
  Monitoring-as-Code bundle, and MaC is the first surface it has.
- **NFR-024 — nothing the canary touches leaks, and nothing it does can take the product down.**
  Secrets reach the executor only through the existing envelope, live in memory for one execution and
  are wiped after it; no request or response body, header, secret value, correlation id, object path,
  presigned URL or raw target URL is written to a heartbeat, a log line, an error, an audit row or a
  metric label. A region with no capable executor, a capability mismatch, an unreachable target and a
  crashed executor are each a NORMALIZED, bounded, per-monitor outcome — never an indefinite pending
  state, never a readiness flip, never an effect on another monitor.

## 3. The decisions

**D1 — a capability, not a role.** The brief asked for `cerbix serve --role canary`. This design
refuses that and puts the canary executor in the roles that already exist: `worker` (AMQP) and `agent`
(pull), both already DB-less. A fifth role would duplicate lifecycle, readiness, region registration
and token auth to gain nothing the capability does not already give — and the negotiation this feature
needs is not new: the scheduler already asks a region what it can open (`LiveCredentialReadyAgentRegions`
with an envelope generation) and lowers the carrier when the answer is no. That mechanism generalizes
from one number to a SET of `workflow_kind@version`, and `no_capable_runner` falls out of it rather
than being invented. An operator who wants the canary on its own host runs another `agent` with
`--workflows async_transaction_v1`; isolation stays a deployment decision, which is where it belongs.

**D2 — one probe, one heartbeat: the transaction does NOT outlive its job.** The alternative — submit
in one job, await in another — was considered and rejected. It breaks the product's spine, which is
that the executor answers the job it was given and the heartbeat it returns is the whole measurement.
Concretely: the runner has no database, so an in-flight transaction would have to be stored in the
CORE, which forces exactly the values NFR-024 forbids storing (correlation id, result path) into a
table; latency would have to be glued from two jobs with two clocks; and one failure mode ("no result")
becomes four ("submit landed but the await job was lost", "the await ran twice", "the transaction is
orphaned", "the second job went to a different executor"). The cost of the chosen shape is a delivery
held open for the duration, and §4 pays it explicitly.

**D2a — the boundary where D2 stops being right, named now.** This shape is valid while the completion
bound is MINUTES. A use case measured in hours cannot hold a delivery, and the answer then is a
different feature — an asynchronous result ingest — and not a larger timeout. The type's hard ceiling
is **900 seconds**, in the schema, so that crossing it is a decision someone has to make rather than a
number someone edits.

**D3 — closed unions, no opaque settings, no embedded script.** `submit.kind` is `http_json` or
`multipart_fixture`; `completion.kind` is `sse` or `poll_json`; `correlate.source` is `response_json`
or `response_header`; `cleanup.kind` is `lifecycle_prefix` or `none`. Every other field is a scalar
with a type and a bound, or a list of those. There is no `config: map[string]string`, no arbitrary
JSON, no expression language and no user-supplied code. Unknown fields are REFUSED, not ignored —
`unsupported_field` with the key named, as the file provider already does for flat settings.

**D4 — one template substitution, in one place.** `{{ correlation_id }}` is the only substitution the
contract has, and it is legal ONLY in `completion.url`. Not in the submit URL, not in headers, not in
a body, not in the cleanup prefix. A second substitution site is how a contract becomes a template
language.

**D5 — a restricted JSON path grammar, written down rather than borrowed.** No JQ, no JSONata, no
expressions. The grammar is: a dot-separated sequence of object keys and non-negative array indices,
`^[A-Za-z_][A-Za-z0-9_]*(\.([A-Za-z_][A-Za-z0-9_]*|[0-9]+))*$`, maximum depth 8, maximum length 200.
It addresses one value. It cannot filter, cannot iterate, cannot compute. Anything a user might want
beyond that is a request to re-open this decision, not a reason to allow an expression.

**D6 — three assertion kinds, and no more.** Existence (`required_json_fields`), string equality
(`success.value`), numeric equality. No regular expressions, no ranges, no comparisons, no negation in
v1. Each is decidable, each has one failure message, and none can be written in a way that passes for
the wrong reason.

**D7 — a credential-bearing header takes a `secret_ref` and nothing else, by SCHEMA.** Header entries
are typed: `{name, secret_ref}` or `{name, value}`. A name in the credential-bearing set accepts only
the first form — not because a heuristic inspected the value, but because the schema of that field
does not have a `value`. This is the same rule FR-028's D7 reaches for, arrived at from the other
side, and it is why an extracted-token flow is not needed here: a canary's credential comes from the
inventory, not from a login step. (v1 has no login step at all; see §9.)

**D8 — a stable idempotency key derived from the SCHEDULED RUN, not from the job.** The executor sends
`Idempotency-Key: <derived>` on submit. The derivation is over the monitor id and the scheduled tick,
NOT the job id: a redelivered AMQP message, a re-claimed pull job after a lease expiry, and a retried
transport attempt are all the same execution and must carry the same key. A different tick is a
different transaction.

**Half of this guarantee is the target's, and the specification says so rather than implying
otherwise:** cerbix guarantees the same key for the same execution. Whether a second submit with that
key creates a second task is Charla's contract, not ours. If the target ignores the header, retries
create duplicates and no design here prevents it — the runbook says that in those words.

**D9 — no two executions of one monitor at a time, enforced in the CORE.** A DB-less executor cannot
take a lease, so the scheduler does not dispatch a second job for a monitor whose previous job is
in flight, and an in-flight record expires on its own deadline so a crashed executor does not park the
monitor forever. Consequences: the monitor's `interval_seconds` must be ≥ its `timeout_seconds`,
validated at the write boundary; and recovery after an executor crash is bounded by the in-flight
deadline, which the runbook states as a number rather than leaving to be discovered.

**D10 — cleanup is a VALIDATION, never a deletion.** `lifecycle_prefix` means the executor checks that
`result.lifecycle_path` begins with the declared prefix and fails the stage if it does not. The
executor is never given delete rights, and cerbix never removes an object it did not create. Reaping
is the target's lifecycle policy, and the spec requires that policy to be NAMED in the bundle comment
or the runbook, because a canary that creates real objects forever and no one sweeps them is a bill
somebody pays quietly. `kind: none` is legal only with `acknowledged: true`, and that acknowledgement
is visible in the API, the UI and the audit trail — an operator can accept the debt, but not silently.

**D11 — the fixture is a registry key, pinned by digest, embedded in the binary.** `fixture_ref` names
an entry in a closed registry compiled into cerbix: `small_wav_v1`, with its SHA-256 pinned and a hard
maximum size. Not a path, not a URL, not an inline blob, not an operator-supplied file — those are
supply-chain surface, and a canary that uploads whatever it is pointed at is an exfiltration primitive.
Rotating a fixture is a RELEASE, which is the cost of this decision and is stated in the runbook.

**D12 — nested typed bundles, built generally and admitted narrowly.** The file provider gains a
nested typed schema path (a bundle `format` bump), because a JSON string inside a bundle is an
unvalidated document inside a validated one. The machinery is written to be general — it is what would
eventually let `synthetic` into a bundle (FR-028 D9) — but v1 admits ONE type through it,
`async_canary`, and `fileSupportedTypes` gains that type only after the schema, the secret-ref
contract, the semantic hash and their tests all exist.

**D13 — the MaC secret guard recurses.** Today it refuses inline secrets by KEY NAME over a flat map.
Over a nested document it walks every field, at every depth, including inside lists, and refuses any
`value` in a credential-bearing header entry and any field whose schema says `secret_ref`. A guard
that stops at the first level would pass exactly the documents this type introduces.

## 4. What one long probe costs, and where it is paid

D2 holds a delivery open for up to the completion timeout. That is the whole price, and it lands in
four places — each a decision, not a discovery:

1. **The type's timeout ceiling is its own.** `maxTimeoutSeconds` stays 300 for every existing type:
   raising it globally would hand every HTTP monitor a 15-minute timeout. `async_canary` carries its
   own maximum of 900 s (D2a).
2. **The pull lease becomes per-job.** `EnqueuePullJob` defaults to a **60-second** TTL today. A
   10-minute await under a 60-second lease means a second agent re-claims a job the first is still
   running — which is precisely the duplicate submit D8 exists to prevent. The TTL is derived from the
   monitor's timeout plus slack, and the recovery-after-crash latency it implies is documented.
3. **A separate AMQP queue per workflow kind.** Consumer prefetch is **16**. Sixteen canaries holding
   ten-minute deliveries would starve every ordinary check in that region. Capability-aware dispatch
   implies separate queues anyway; this makes it mandatory rather than tidy. The broker's own
   consumer acknowledgement timeout is an upper bound on this design and MUST be verified against the
   pinned RabbitMQ image before implementation — it is not asserted here from memory.
4. **Stage-level bounds inside the one probe.** `submit` gets a short timeout of its own (seconds), so
   a hung submit fails as `submit` rather than consuming the whole window and reporting `await_result`.

## 5. Schema, canonicalization and the semantic hash

### 5.1 The v1 contract, in full

```yaml
# One monitor of type `async_canary`, in a Monitoring-as-Code bundle. Nested and typed:
# no `settings` map, no JSON string, no free-form field anywhere below.
monitors:
  - uid: charla-files-upload
    type: async_canary
    name: Charla Files · upload journey
    region: eu-probe
    interval: 15m            # >= timeout (D9)
    timeout: 10m             # <= 900s, the type's ceiling (D2a)

    workflow:
      kind: async_transaction_v1

      submit:
        kind: multipart_fixture          # | http_json
        method: POST                     # POST only in v1
        url: https://files.example.com/files/upload
        submit_timeout: 30s              # own bound, so a hung submit fails as `submit` (§4.4)
        headers:
          - name: authorization          # credential-bearing → secret_ref ONLY (D7)
            secret_ref: charla-upload-token
          - name: x-tenant               # ordinary → typed non-secret value
            value: canary
        fixture_ref: small_wav_v1        # multipart_fixture only; a registry key (D11)
        multipart:
          file_field: file
          fields:
            only_audio: false            # typed scalars: string | number | boolean
        # body:                          # http_json only — a typed object, never a JSON string

      correlate:
        source: response_json            # | response_header
        path: task_id                    # the restricted grammar of D5

      completion:
        kind: sse                        # | poll_json
        url: https://files.example.com/tasks/{{ correlation_id }}/events
        timeout: 10m                     # <= the monitor's timeout
        sse:
          success_event: task.completed
          failure_events: [task.failed]
          required_json_fields: [s3_path, byte_size, media_type]
        # poll_json:
        #   interval: 5s
        #   max_attempts: 120
        #   success: { path: status, value: completed }
        #   failure: { path: status, values: [failed, cancelled] }

      result:
        max_latency: 10m
        required_json_fields: [s3_path, byte_size, media_type]
        lifecycle_path: s3_path

      cleanup:
        kind: lifecycle_prefix           # | none (then `acknowledged: true` is required, D10)
        prefix: canary/
        acknowledged: true
```

Everything not in this document is not in the contract. A field the schema does not name is
`unsupported_field` with the key, and the bundle is refused whole.

### 5.2 Canonicalization

The runtime config MAY be stored as canonical JSON; the YAML parser fully validates the structure and
refuses unknown fields BEFORE reconcile. Storage form is not the contract; the schema is.

**Canonicalization.**

- **Map order is insignificant** — keys are sorted before hashing, always.
- **List order is significant only where the contract says so**, and the contract says so nowhere in
  v1: `headers`, `multipart.fields`, `required_json_fields` and `failure.values` are all SETS, sorted
  by their canonical key before hashing, and duplicates are refused rather than deduplicated.
- **Header names canonicalize to lower case**; two entries whose names differ only in case are ONE
  header and are refused as a duplicate — the rule FR-028 learned the hard way, applied here at the
  schema level.
- Durations canonicalize to whole seconds; the YAML accepts `10m`, the canonical form is `600`.

### 5.3 The semantic hash

**It changes when, and only when, execution semantics change:** the workflow kind or
version, any URL, method, submit kind or body, the fixture id, any `secret_ref` IDENTITY (the name, not
the value — rotating a secret does not move the hash and must not reschedule the monitor), the
correlation source or path, the completion kind and every field under it, timeouts and attempt bounds,
every assertion, and the cleanup kind, prefix and acknowledgement. Reformatting the YAML, reordering
maps, reordering a set-list or changing a comment does not move it.

## 6. Acceptance invariants (FR-029 / NFR-024)

1. A bundle declaring `async_canary` is accepted only if EVERY field validates against the typed
   schema; one unknown field, one wrong union branch, one out-of-range bound refuses the whole bundle
   with the key named, and the previous last-known-good is retained.
2. An inline credential anywhere in the document — at any depth, inside any list — is refused, and the
   refusal names the field without echoing the value.
3. `{{ correlation_id }}` outside `completion.url` is refused; any other `{{ … }}` is refused
   anywhere.
4. Two headers whose names differ only in case are refused as a duplicate, and the canonical form is
   what the digest covers.
5. A semantic no-op re-apply changes no monitor row and moves no revision; a semantic change moves the
   revision exactly once.
6. The executor receives a job ONLY if it announced the workflow kind and version; a region with no
   such executor produces `no_capable_runner`, a version skew produces `capability_mismatch`, and both
   are bounded, per-monitor and eventually a normal monitor DOWN — never an indefinite pending.
7. Submit carries an idempotency key that is identical across a redelivery, a re-claim and a transport
   retry of the same scheduled run, and different for the next run.
8. The scheduler never has two in-flight executions of one monitor; a crashed executor's monitor
   resumes after the in-flight deadline and not before.
9. No heartbeat, log line, error, audit row or metric label contains a URL, a request or response
   body, a header value, a secret, the correlation id or the result object path — asserted by planting
   distinctive values in every one of those positions and searching all five outputs.
10. A result missing a required field, a `lifecycle_path` outside the declared prefix, a failure event,
    a poll attempt-limit exhaustion and a completion timeout each produce DOWN with the correct
    `failed stage` and no other difference.
11. Every bound holds under adversarial input: response size, SSE event and line size, poll attempts,
    redirect count, fixture size, total duration, concurrent executions per region.
12. The URL policy refuses plaintext HTTP, loopback, link-local, private and cloud-metadata addresses
    AFTER resolution, re-validates every redirect hop, and connects to the address it validated so a
    rebinding answer cannot be used.
13. `cleanup.kind: none` without `acknowledged: true` is refused; with it, the acknowledgement is
    visible in the API payload, the UI and the audit trail.
14. A canary monitor is an ordinary member of a Service: it appears in `monitors[]`, contributes to
    `sli[]`, alerts and escalates exactly as any other type, and nothing in this feature adds a second
    reliability path.

## 7. Required test matrix (written before the code)

**Domain / unit.** Every union branch accepted; every forbidden branch combination refused
(`fixture_ref` with `http_json`, `body` with `multipart_fixture`, `sse` fields under `poll_json`);
unknown field at depth; header canonicalization and duplicate refusal; credential-bearing header with
`value` refused; `{{ correlation_id }}` in a submit URL, a header and a body each refused; the JSON
path grammar accepting its language and refusing JQ-shaped input, over-deep and over-long paths;
duration canonicalization; interval ≥ timeout; every bound at its edge and one past it; idempotency key
stability across redelivery and difference across ticks; cleanup acknowledgement rules.

**URL policy / SSRF.** Loopback, link-local, RFC1918, unique-local, and `169.254.169.254` refused after
resolution; a name that resolves to a public address on the first lookup and a private one on the
second (rebinding) refused because the connection uses the validated address; a redirect chain whose
second hop is private refused; plaintext HTTP refused; the redirect limit; DNS, connect, TLS and read
deadlines each fired independently.

**File provider.** A valid `multipart_fixture` + `sse` bundle; a valid `http_json` + `poll_json`
bundle; an inline token in a header refused; an inline token nested in a body refused; an arbitrary
fixture path or URL refused; an unknown field refused; a semantic no-op retaining the revision; a
semantic change bumping it exactly once; an invalid replacement preserving last-known-good.

**Runner.** Submit extracts the correlation id from JSON and from a header; SSE success, declared
failure event, timeout, malformed event, oversized event and oversized line; poll success, declared
failure value, attempt-limit exhaustion; a missing required result field failing `assert_result`; a
`lifecycle_path` outside the prefix failing `cleanup_validation`; the idempotency key surviving a
retry; a second concurrent execution of one monitor refused; and the leak test of invariant 9 across
result, log and error.

**E2E, against a controlled local HTTP/SSE fixture only — never a real external provider.** A
file-managed canary monitor reaching the scheduler, the executor and a heartbeat, and feeding a
service SLI; a region with no capable executor producing the normalized down reason; a race/security
pass over the secret envelope and the in-flight lease.

## 8. Threat model

- **Secrets.** Values reach the executor only inside the existing envelope, bound to the monitor and
  the execution body; they exist in memory for one execution and are wiped after it. They are never in
  the YAML, the stored config, the job payload's plaintext, an audit detail, a heartbeat, a log or a
  metric. The bundle carries a NAME.
- **SSRF and DNS rebinding.** The canary points at operator-supplied URLs by design, which makes it a
  request forger if unbounded. Mitigation is resolve → validate → connect to the validated address,
  repeated per redirect hop, with a public-address allowlist rather than a blocklist of known-bad
  ranges.
- **Fixture supply chain.** A registry key with a pinned digest, embedded at build time. An operator
  cannot make the canary upload a file of their choosing, and a fixture change is a release with a
  changed digest.
- **Side effects.** Every run creates a real external artifact. The idempotency key bounds duplicates
  to what the target's contract allows; the lifecycle prefix bounds where they land; the acknowledged
  `none` makes an unmanaged accumulation an explicit, visible choice.
- **Retention.** cerbix stores no artifact, no body and no path. What accumulates lives at the target,
  and the runbook names whose policy reaps it.
- **Availability of the product itself.** §10.

## 9. Non-goals for v1, stated so nobody infers them

Browser workflows and anything requiring JavaScript execution. Webhook callbacks (cerbix does not
receive). Queue assertions. Arbitrary scripts, plugins or user-supplied code. Arbitrary retries — only
the declared poll interval and attempt bound, plus a transport retry policy fixed by this
specification. A login step: a canary's credential comes from the inventory, and chaining an extracted
token into a later credential-bearing header is FR-028's open question (roadmap N6), not this type's.
Multiple workflow kinds: one, versioned. Object deletion. Any UI before the MaC contract and the
executor both work.

## 10. Blast radius: what a misconfigured canary may take down (the owner's standing rule)

Nothing outside itself. Each of these is an invariant, not an aspiration:

- **A canary never blocks another monitor.** Its long-held delivery lives in its own AMQP queue, and
  its pull jobs carry their own lease; prefetch pressure cannot reach the region's ordinary checks
  (§4.3).
- **A region with no capable executor is a per-monitor DOWN with a normalized reason,** bounded in
  time. Not a readiness flip, not an indefinite pending, not an effect on the region's other monitors.
- **A canary that cannot resolve its secret fails its own probe** with a typed reason, exactly as a
  credentialed monitor does today. Readiness is never coupled to a secret, a key or a fixture.
- **A malformed bundle refuses itself and keeps the last-known-good,** as every bundle already does.
- **An executor crash parks one monitor for one in-flight deadline,** and nothing else.
- **A hostile or hanging target consumes bounded time, bounded memory and one queue slot.** Every
  limit in §6.11 exists for that sentence.

## 11. Owner questions, to answer before implementation

1. **The transaction ceiling.** 900 s as the type's hard maximum, with anything longer being a
   different feature (D2a) — accepted, or is a lower number wanted for v1?
2. **The lifecycle policy.** Should the schema REQUIRE a named external reaping policy for
   `lifecycle_prefix` (a free-text field naming it, validated as non-empty), or is the runbook's word
   enough?
3. **Private-address targets.** v1 refuses them with no override. An operator canarying a LAN service
   is blocked. Accepted for v1?

## 12. Phases — separate iterations, in this order

| Phase | Contents | Gate |
| --- | --- | --- |
| A | This document + the decision record. No code. | design review |
| B | Domain types, the typed parser, canonicalization and the semantic hash, unit tests. The type is NOT admitted to bundles yet. | `-race`, docs-check |
| C | The executor: `http_json` + `poll_json` end to end, capability announcement, dispatch filtering, `no_capable_runner`. Local fixture server in tests. | `-race`, live single stack |
| D | The in-flight lease, the idempotency key, the per-job pull lease, the per-kind queue, interval ≥ timeout. | `-race`, live stack, race pass |
| E | `sse`, the fixture registry, admission to `fileSupportedTypes`, metrics, runbook, E2E. | full gates |
| F | UI as a typed form only. No JSON editor. | owner-approved mock first |

`poll_json` precedes `sse` deliberately: it exercises the whole skeleton — submit, correlate, await,
assert, cleanup validation — with short requests and no long-lived connection, so the first vertical
slice carries the least risk. SSE is then one stage swapped into a proven path.
