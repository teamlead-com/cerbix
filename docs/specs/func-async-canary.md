# func-async-canary — a typed external canary for async API journeys (FR-029 / NFR-024)

> **Lifecycle: DESIGN APPROVED — revision 8 (2026-09-03).** Approved by the independent reviewer at
> party [66] after six review rounds and seventeen P0s, with the owner's sign-off on §11.4. Phase A is
> closed; the phases in §12 are separate iterations and each carries its own gate. Nothing outside
> phase B may begin before phase B lands. FR-029 and NFR-024 get their rows in
> `docs/status.md` at that point, and the phases in §12 are separate iterations, not one.
>
> **Revision 2 — the owner's three answers (§11).** The transaction ceiling went DOWN far enough to
> delete the mechanism: the type inherits `maxTimeoutSeconds = 300` and there is no per-type bound.
> Reaping the artifacts is the operator's problem and not a schema field. Private-address targets stay
> refused in v1 with no override.
>
> **Revision 3 — three P0s from the reviewer's first design pass, each a place where this document
> claimed more than it could decide or contradicted itself.** (1) `body: typed object` could not
> coexist with "an inline credential anywhere is refused" — the body is now a closed algebra with a
> declared `secret_ref` node, and the claim is narrowed to what a schema can decide, with the residual
> named in FR-028's own words (D3a). (2) The URL policy refuses plaintext HTTP and private addresses
> while the E2E requires a local fixture — resolved by an injected dialer in the test build and a
> policy proven by unit tests plus one real-path E2E case, never by a product flag (§6.12, §7). (3)
> `{{ correlation_id }}` carried a TARGET-controlled value into a URL unbounded and unescaped — now
> one whole path segment, 256 bytes, UTF-8, no control characters, percent-encoded before parse, and
> the substituted URL re-validated (D4).
>
> **Revision 4 — five more P0s from the reviewer's second pass, four of them things the document simply
> did not say.** `submit` had no accepted-status contract, so `202` — the first use case's own answer —
> was undefined (D3d). Every bound was an adjective, and "the edge and one past it" cannot be written
> against an adjective, so §4b now carries the numbers. A nested secret reference promised "an envelope
> field" without saying where the reference lives for rename, what `monitor_secret_refs.setting_key`
> holds, or what stops a carrier attacker relocating a credential — answered by reusing FR-028's
> named-binding shape rather than inventing a second one (D3b). `completion` had no header semantics at
> all, which is a credential-forwarding bug waiting to be written (D3c). And
> `correlate.source: response_header` pointed at the JSON-path grammar, which an ordinary header name
> like `task-id` does not match, making the field unusable for its own purpose (D5).
>
> **Revision 5 — two P0s from the reviewer's third pass, the first of which was a claim about code I
> had not checked.** D3b said the flat ref key lets `repointSecretRefs` work unchanged; the reviewer
> read `internal/store/secrets.go` and found it decodes `monitors.config` into a `map[string]string`,
> so a nested object there would have failed the decode — and therefore the whole rename — for EVERY
> monitor in the project. The document now states what it always should have: the typed structure is
> the contract, and its canonical JSON is persisted as ONE STRING in a flat key beside the flat ref
> keys (D3e). And "≤ 4 concurrent executions per region" named no enforcement point, which a
> worker-local prefetch cannot provide across several agents — it is now counted in the core at the
> leader's dispatch decision, with saturation as a bounded per-monitor outcome (D9a).
>
> **Revision 6 — two P0s, and both were contradictions my own revision-5 fixes introduced.** The
> persisted document carried `workflow.secrets` — project-secret NAMES — beside the flat ref keys a
> rename updates, so after a rename one monitor would hold the old name in the canonical string and the
> new one in the flat key, while §7 demanded the string stay byte-identical. The map is now INPUT ONLY,
> projected into the flat keys, which are the sole persisted identity (D3f). And "three consecutive
> saturated runs go DOWN" was a state machine smuggled in as a sentence, with no counter, no storage
> and no freshness semantics: deleted in favour of one ordinary DOWN heartbeat through the existing
> `failure_threshold`, plus the region-alert edge the product already fires for a worker-less region —
> with the interval marked UNGOVERNED rather than unavailable, because a sample we could not take is
> not evidence about the target (§11.5 is the owner's to overrule).
>
> **Revision 7 — two P0s, and both corrected something I asserted without checking.** The hash rule
> said renaming a secret "moves neither" while the hash covers the flat ref BY NAME and
> `repointSecretRefs` changes exactly that name. Reading the code settled it: `canonicalHash` already
> hashes `password_ref` the same way, and it is safe because a rename of a secret referenced by a
> FILE-MANAGED monitor is refused outright — so the canary inherits a solved problem, and the fix the
> reviewer proposed (hash over a stable `secret_id`) is what becomes necessary the day that refusal is
> lifted (D3g). And the SLI: I had reopened a decision the owner already made, on an analogy that was
> also factually wrong — maintenance is suppression, while FR-021's "ungoverned" is pre-declaration
> time that reads UNKNOWN. Those samples count as UNAVAILABLE like any other DOWN, and attribution
> lives in the reason, the region alert and the metrics rather than in a silent exclusion.
>
> **Revision 8 — three P0s, and two of them are the SAME mistake of mine twice: I changed a decision
> and left the test matrix demanding the old behaviour.** §7 still asked for a canary rename to
> re-point a ref while D3g says that rename is refused, and still expected a saturated interval to be
> "ungoverned" after the decision producing it was deleted. Both are rewritten from the decisions
> rather than patched. The third was a real hole: the cross-host credential rule sat inside the
> COMPLETION decision, leaving `submit` — the request that actually carries the credential — covered
> only by `net/http`'s default, which strips `Authorization` and has never heard of `x-api-key`
> (D3c1). Plus two P1s the review was right to insist on: reserved header names the runner owns, and
> `result.max_latency`, which existed in the example and nowhere else (§5.5).
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

**D2a — the ceiling is the one the product already has: 300 seconds, and no per-type exception**
(owner, revision 2). Revision 1 proposed 900 s for this type alone. The owner asked whether it could be
lower, and it can — to the point where the mechanism disappears: `async_canary` inherits
`maxTimeoutSeconds = 300` like every other type, so there is no second bound in the product and no
per-type ceiling to keep honest.

What the smaller number buys, and it is not tidiness:

- **Recovery after an executor crash is bounded by the in-flight deadline** (D9), which is derived from
  the timeout. At 900 s a crashed runner parks its monitor for a quarter of an hour — in a MONITORING
  product, which is the wrong trade to make for a use case that finishes in seconds.
- **SLI density.** `interval >= timeout` (D9), so a 900 s canary samples four times an hour and one
  failure is 25 % of that hour. At 300 s it samples twelve times, and the number a Service reports
  means something closer to what a reader assumes it means.
- **Time to page.** With the ordinary two confirmations, 300 s pages in ten minutes and 900 s in half
  an hour. A canary that notices in half an hour is not an early warning.

**What it costs, stated rather than discovered:** a journey whose declared promise exceeds five minutes
cannot be expressed in v1. That is a real limit — if the first use case's upload can legitimately take
longer than five minutes under load, this number is wrong and must be decided before phase C rather
than found by a false DOWN in production (§11.1).

**And the boundary where D2 itself stops being right, unchanged:** this shape holds while the bound is
MINUTES. A journey measured in hours cannot hold a delivery, and the answer then is a different feature
— asynchronous result ingest — not a larger number. Raising 300 s is a spec revision with a named use
case, which re-checks the lease, the queue and the alerting consequences together; it is deliberately
not a constant someone edits.

**D3 — closed unions, no opaque settings, no embedded script.** `submit.kind` is `http_json` or
`multipart_fixture`; `completion.kind` is `sse` or `poll_json`; `correlate.source` is `response_json`
or `response_header`; `cleanup.kind` is `lifecycle_prefix` or `none`. Every other field is a scalar
with a type and a bound, or a list of those. There is no `config: map[string]string`, no arbitrary
JSON, no expression language and no user-supplied code. Unknown fields are REFUSED, not ignored —
`unsupported_field` with the key named, as the file provider already does for flat settings.

**D3d — submit declares which statuses mean "accepted" (P0, review round 2).** Charla answers `202`;
the schema had no way to say so, which left `200`, `204` and `500`-with-a-parseable-body undefined.
`submit.accepted_status` is a non-empty list of **2xx** statuses (200..299), deduplicated and sorted
canonically, evaluated against the FINAL response after redirects — the reviewer's narrowing (P1,
round 3), and it is the right one: a 3xx is not an outcome when redirects are followed, and a 4xx or
5xx that a workflow wanted to call "accepted" is a target contract nobody should encode here. Any
other status fails the `submit` stage with the bounded code class and no body.
Correlation is attempted ONLY on an accepted status — a `task_id` extracted from an error body is not
a transaction that exists.

**D4 — one template substitution, in one place, and the value is TARGET-CONTROLLED (P0, review round
1).** `{{ correlation_id }}` is the only substitution the contract has, and it is legal ONLY in
`completion.url`. Not in the submit URL, not in headers, not in a body, not in the cleanup prefix. A
second substitution site is how a contract becomes a template language.

Revision 1 stopped there, which left the substituted value unbounded and unescaped — and that value
comes from the TARGET's response, so `../`, `?`, `#`, `@` or a newline in it rewrites the request
cerbix is about to make. Revision 3 constrains it:

- the placeholder must occupy **exactly one whole path segment** of `completion.url` — never a
  fragment of a segment, never a query value, never the host;
- the extracted value is bounded at **256 bytes**, must be valid UTF-8, and must contain no control
  characters; over-length or malformed fails the `correlate` stage with a typed reason;
- the value is **percent-encoded as a single path segment** before the URL is parsed, so a `/`, `?`,
  `#` or `@` inside it stays data and cannot change the request's shape or target;
- the completion URL is re-validated against the whole URL policy (§6.12) AFTER substitution, not
  before, because the pre-substitution template is not the request that gets made.

**D5 — TWO restricted grammars, one per source, because a header name is not a JSON path (P0, review
round 2).** Revision 3 pointed `correlate.path` at the JSON grammar for both sources, and an ordinary
header name — `task-id` — does not match it: the field was unusable for the case it exists for.

*JSON path* (`response_json`, `required_json_fields`, `lifecycle_path`, `success.path`,
`failure.path`): a dot-separated sequence of object keys and non-negative array indices,
`^[A-Za-z_][A-Za-z0-9_]*(\.([A-Za-z_][A-Za-z0-9_]*|[0-9]+))*$`, depth ≤ 8, length ≤ 200. It addresses
one value; it cannot filter, iterate or compute.

*Header name* (`response_header`): an RFC 7230 token, `^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$`,
canonicalized to lower case for lookup. A response carrying the same header more than once fails
`correlate` rather than picking one — two values are not a correlation id.

No JQ, no JSONata, no expressions, in either grammar. Anything beyond them re-opens this decision
rather than justifying an escape hatch.

**D6 — three assertion kinds, and no more.** Existence (`required_json_fields`), string equality
(`success.value`), numeric equality. No regular expressions, no ranges, no comparisons, no negation in
v1. Each is decidable, each has one failure message, and none can be written in a way that passes for
the wrong reason.

**D7 — a credential-bearing header takes a BINDING and nothing else, by SCHEMA.** Header entries
are typed: `{name, secret_ref}` or `{name, value}`.

**Reserved header names, refused at the write boundary** (P1, review round 5): `idempotency-key`,
`host`, `content-length` and `transfer-encoding` on any stage, plus `content-type` on a
`multipart_fixture` submit. Case-insensitive. The runner OWNS these: it derives the idempotency key
from the scheduled run (D8) and the multipart boundary from its own encoder, so an author-supplied
value would make the stable-key contract ambiguous and could produce a body that does not parse. A
schema that silently overrode them would be worse than one that refuses.

**The credential-bearing set is frozen HERE, not referenced** (P1, review round 3): `authorization`,
`proxy-authorization`, `cookie`, `x-api-key`, `api-key`, `x-auth-token`, `auth-token`,
`x-access-token`, `access-token`, `private-token`. Ten names, compared case-insensitively. It happens
to be FR-028's set today, and a spec that says "the set FR-028 uses" would silently change meaning if
that one changed — so this document owns its own copy, and widening it is an amendment here. A name in the credential-bearing set accepts only
the first form — not because a heuristic inspected the value, but because the schema of that field
does not have a `value`. This is the same rule FR-028's D7 reaches for, arrived at from the other
side, and it is why an extracted-token flow is not needed here: a canary's credential comes from the
inventory, not from a login step. (v1 has no login step at all; see §9.)

**D3a — the request body is a CLOSED ALGEBRA with declared secret positions, and the guard claims only
what it can decide (P0, review round 1).** Revision 1 wrote `body: typed object` and, three sections
later, promised to refuse "an inline credential anywhere, at any depth". Those cannot both be true: a
free-form object's string leaf is exactly the case FR-028's D7 proved undecidable. Revision 3 resolves
it by narrowing BOTH ends rather than picking one.

*The algebra.* A body is an object whose values are: a string, a number, a boolean, a nested object, a
list of those, or the typed node `{secret_ref: <binding>}` — which is how a credential
legitimately enters a body. Keys match `^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`. Depth ≤ 8, ≤ 64 keys per
object, ≤ 32 elements per list, ≤ 8 KiB canonical JSON for the whole body, ≤ 1 KiB per string leaf.
`multipart.fields` is the same algebra restricted to a flat map of scalar leaves and `secret_ref`
nodes. Anything else — a null, a `secret_ref` node inside a string, a key outside the grammar, an
over-bound document — is `unsupported_field` or `domain_invalid` with the position named.

*The claim, at the size it actually has.* cerbix refuses a `value` where the schema says a binding
(headers, D7), refuses any node shape it does not know, refuses a binding no `workflow.secrets` entry
declares, and resolves a declared binding at dispatch so no secret value ever appears in the document. It **cannot** detect a credential pasted as an
ordinary string leaf in a body, and no rule in this specification pretends otherwise — the same
residual as FR-028 D7, in the same words, because it is the same undecidable thing. The rejected
alternative was an operation-specific closed schema per target API, which would require cerbix to know
Charla's request shape; that is not a general product, it is a client library.

**D3b — a secret is a NAMED BINDING declared once, and its POSITION is what the digest covers (P0,
review round 2).** Revision 3 wrote `secret_ref: <project-secret-name>` at each position and said the
value "resolves to an envelope field" — which left three things undefined that FR-028 had to answer the
hard way: where the reference lives for `repointSecretRefs`, what `monitor_secret_refs.setting_key`
holds, and what stops a carrier attacker moving a credential from one position to another. This
revision reuses FR-028's answer instead of inventing a second one.

- The workflow declares `secrets:` once — a map of BINDING NAME → project secret name, grammar
  `^[a-z][a-z0-9_-]{0,39}$`, at most 8 per monitor.
- A position references a binding: `{secret_ref: <binding>}` in a header entry, a body leaf or a multipart
  field. The project secret's name appears exactly once in the document, in `secrets:`.
- The stored canonical config carries one FLAT key per binding, `canary_secret_<binding>_ref` =
  the project secret's name. That is the same shape as `scenario_secret_<binding>_ref`, so
  `repointSecretRefs`, `monitor_secret_refs`, delete-counting and rotation all work with NO
  canary-aware code — the property FR-028 D6 was chosen for, and the reason D6a was never built.
  **This is only true because of D3e, and revision 4 asserted it without saying why** (P0, review
  round 3).
- The envelope carries one field per binding, named by the same derivation FR-028 uses.
- **Relocation is defeated by the execution digest, not by a rule:** the canonical workflow document
  and the ref keys are EXECUTION BINDING KEYS, so `EnvelopeV2`'s body digest covers the exact positions
  a binding is used in. Moving `{secret_ref: upload}` from a header to a body — or to a different step —
  produces a different document, and the AEAD fails before any request. This is the mechanism that
  FR-028 stage 2 had to add after a test proved the digest did NOT cover the scenario; here it is a
  requirement from the first line rather than a correction.

**D3e — the typed document is PERSISTED as one canonical JSON STRING, because `monitors.config` is
a flat `map[string]string` and always was (P0, review round 3).** Revision 4 said the storage form
"may be canonical JSON" and left the shape unstated, which let it claim a rename path it would have
broken. The reviewer checked the code instead of the claim: `repointSecretRefs`
(`internal/store/secrets.go`) locks the monitor row and decodes `monitors.config` into a
`map[string]string`, and `domain.Monitor.Config` is that type everywhere. A nested object under
`config` does not merely fail for the canary — **it fails the decode, and therefore fails the whole
rename, for every monitor in that project.**

So the contract is stated exactly: the workflow document is validated as a TYPED, nested structure at
every write surface, and persisted as its canonical JSON serialization in ONE flat key,
`config["workflow"]`, beside the flat `canary_secret_<binding>_ref` keys. `config` stays a flat map of
strings, every existing store path keeps working unchanged, and D3b's claim becomes true rather than
hopeful.

**The obvious objection, answered rather than avoided:** this is the same STORED shape as
`synthetic`'s scenario blob, which §0 condemns. The difference is which end is the contract. For
`synthetic` the blob IS the contract — a user writes it, a bundle would have to carry it verbatim, and
nothing can validate what it means, which is why every rule about it has to guess. Here the blob is a
canonical PROJECTION of a document that was fully typed and fully validated before it was written; no
surface accepts it as input, no bundle carries it, and nothing reads it back as a user contract. The
condemnation was of an unvalidated document as an interface, not of JSON as a storage encoding.

**What this obliges, and it is in §7 as tests rather than as prose:** the rename, rotation and
delete-guard paths are exercised for a canary monitor exactly as `TestScenarioBindingRidesTheOrdinaryRefPath`
exercises them for a scenario — the ref key re-pointed, the workflow document left byte-identical, the
delete guard counting the binding, rotation needing no monitor edit — and the envelope's body digest
covers the canonical string, so a binding moved between positions is a different document.

**D3f — `workflow.secrets` is INPUT ONLY; the persisted document carries binding MARKERS and no
project-secret name (P0, review round 4).** Revision 5 created a second source of truth and did not
notice: the canonical document contained `workflow.secrets: {upload: <project-secret-name>}` while the
sibling flat `canary_secret_upload_ref` held the same name — and a UI rename updates only the flat key,
because that is all `repointSecretRefs` knows about. One stored monitor would then carry the OLD name
inside the canonical string and the NEW name in the flat key, while §7 simultaneously demanded the
string stay byte-identical across a rename. Both cannot be true, and the document said both.

The projection, stated once:

- `workflow.secrets` exists in the YAML and in the API request. It is validated there — every binding
  name unique and in grammar, every project secret resolvable in this project, every declared binding
  used, every used binding declared — and then it is **not persisted**.
- What is persisted in `config["workflow"]` is the document with `secret_ref: <binding>` markers at
  their positions and NO `secrets` map. A binding name is not a secret and not an identity; it is a
  slot.
- The flat `canary_secret_<binding>_ref` keys are the **sole persisted identity** of which project
  secret fills which slot. `repointSecretRefs` updates them and nothing else, so the canonical string
  is genuinely byte-identical across a rename — the claim §7 makes is now a consequence rather than a
  hope.
- Reads reconstruct the pairing from the flat keys, so an API or UI response after a rename shows the
  NEW name with no stale copy anywhere to disagree with it.
- The semantic hash covers the canonical string AND the flat refs (§5.3). **Revision 6 then claimed
  that renaming the secret "is not [semantic], and moves neither", which is wrong as written and the
  reviewer caught it: the flat ref holds a NAME, `repointSecretRefs` changes exactly that name, so by
  the stated rule a rename WOULD move the hash.** What is actually true is narrower and is inherited
  rather than invented — see D3g.

**D3g — the hash covers the ref by NAME, and a rename cannot make it stale because a rename is
REFUSED where the hash exists (P0, review round 5).** Checked in the code rather than reasoned about:
`canonicalHash` (`internal/fileprovider/canonical.go`) hashes the whole flat `Config` map, which
already includes `password_ref` — so FR-020 has had this exact property since it shipped, and the
canary inherits a solved problem rather than creating one. It is safe for the same reason it is safe
there: `UpdateProjectSecret` refuses to rename a secret referenced by a FILE-MANAGED monitor
(`SecretRenamedInUseError`, joined through `managed_monitors`), and the semantic hash is a
file-provider concept that exists only for file-managed monitors. A UI-managed monitor has no
semantic hash to go stale; its rename simply re-points the flat key.

So the honest statement, replacing revision 6's overclaim: **changing which project secret fills a
binding moves the hash; renaming a secret that a file-managed canary references is refused outright,
so the stale-hash case cannot occur.** The tests are the reviewer's: a no-op re-apply after a
UI-managed rename moves no revision, and re-pointing a binding at a DIFFERENT secret bumps it exactly
once.

**What this costs, named so it is a decision rather than an omission.** The day file-managed renames
are unblocked, this becomes wrong and the fix is the reviewer's: resolve each binding to its
tenant-scoped `secret_id` during apply and hash `(binding, secret_id)`, leaving the name in the flat
config for lookup and display only. That is not free — `canonicalHash` is a PURE function today,
computed at plan time with no database, and hashing ids moves it inside the apply transaction where
refs are resolved. It is the right change when the constraint changes; it is not worth restructuring
the file provider for a case the product currently refuses.

**D3c1 — a binding-backed header is dropped on ANY host or port change, at EVERY hop, in EVERY stage
(P0, review round 5).** Revision 4 wrote this rule inside the completion decision and left `submit`
uncovered, which is the more dangerous half: submit is the request that carries the credential, it
follows redirects, and **`net/http` strips only `Authorization` across hosts — it has never heard of
`x-api-key`.** Relying on that default would leak every other credential-bearing header to whatever a
redirect points at.

The rule, owned by this product and not by the transport: before each request in each stage — submit,
completion, and any redirect hop of either — the executor compares the NORMALIZED host and port
against the previous hop's. On any change it removes EVERY header whose value came from a binding, and
sends the rest. Not just `authorization`; the whole binding-backed set, because the schema knows which
headers those are and the HTTP client does not. Proven by a submit that redirects to a different
public host, asserting the target receives no binding header at all — `x-api-key` named explicitly in
the test, because it is the one a default would have let through.

**D3c — completion NEVER inherits submit's headers, and a credential cannot cross hosts (P0, review
round 2).** Revision 3 said nothing about completion headers, and silence there is a credential
forwarding bug waiting to be written: the completion URL is a different URL, and in general a
different host. This revision states it:

- The completion request sends ONLY the headers declared under `completion.headers`, typed exactly as
  submit's are. Nothing is inherited.
- If any completion header carries a binding, the completion URL's host MUST equal the submit URL's
  host, checked at validation. Sending a credential to a host the operator did not authenticate
  against is refused at the write boundary, not at run time.
- Redirects are governed by **D3c1 and only by D3c1**: normalized host OR PORT, every hop, every
  stage. This bullet said "a redirect that changes host" while D3c1 says host or port, and a weaker
  sentence left standing beside a stronger one is how an implementer picks the weaker (P1, review
  round 6). There is one rule and it is D3c1's.

**D8 — a stable idempotency key derived from the SCHEDULED RUN, not from the job.** The executor sends
`Idempotency-Key: <derived>` on submit. The derivation is over the monitor id and the scheduled tick,
NOT the job id: a redelivered AMQP message, a re-claimed pull job after a lease expiry, and a retried
transport attempt are all the same execution and must carry the same key. A different tick is a
different transaction.

**Half of this guarantee is the target's, and the specification says so rather than implying
otherwise:** cerbix guarantees the same key for the same execution. Whether a second submit with that
key creates a second task is Charla's contract, not ours. If the target ignores the header, retries
create duplicates and no design here prevents it — the runbook says that in those words.

**D9a — per-region concurrency is enforced where dispatch happens, and saturation is a bounded
per-monitor outcome (P0, review round 3).** Revision 4 wrote "≤ 4 concurrent canary executions per
region" and a reason code, and said nothing about who counts. A worker-local prefetch does not bound a
region with several agents and several queues, so the number would have been decoration.

The count lives in the CORE, in the same in-flight table D9 introduces, and the enforcement point is
the scheduler's dispatch decision — which is already serialized, because the scheduler is
leader-elected through a Postgres advisory lock and is the only writer of that table. That makes the
check a plain `count(*) WHERE region = $1 AND deadline > now()` inside the transaction that inserts the
new row; no semaphore, no second lock, no cross-process protocol. The DB rows exist for CRASH RECOVERY
and for the executor's ack, not for mutual exclusion between schedulers.

**Saturation invents no counter (P0, review round 4).** Revision 5 said "after three consecutive
saturated runs it goes DOWN" and defined neither where that streak lives, nor when the heartbeat is
written, nor what it means for freshness and the SLI — a new state machine smuggled in as a sentence.
It is deleted. A scheduled run that cannot be dispatched writes ONE ordinary DOWN heartbeat with the
typed reason (`region_saturated`, or `no_capable_runner`), and the monitor's EXISTING
`failure_threshold` decides whether that becomes a status flip, exactly as for every other failure.
No new counter, no new transition, no new persistence, and it composes with confirmations, renotify
and escalation because it IS the ordinary path.

**And the product already had an answer for a region-wide shortage, which this reuses rather than
duplicates:** `EvaluateRegionWorkerAlerts` fires once per TRANSITION, per affected project, when a
region has had no live worker for a grace period — so an operator learns the cause once instead of
through N monitor alerts. `no_capable_runner` and `region_saturated` join that evaluation as the same
kind of edge.

**The SLI consequence: these samples count as UNAVAILABLE, like every other DOWN (P0, review round
5).** Revision 6 proposed excluding them as "ungoverned, the way maintenance already does". Two things
were wrong with that, and the second is a fact I asserted without checking. First, the brief settled
it: a runner failure or absence becomes an ordinary monitor DOWN feeding the ordinary `monitors[]` and
`sli[]`, with no separate reliability model — and I reopened a decision the owner had already made.
Second, **"the way maintenance already does" is false**: maintenance is a suppression, while
"ungoverned" in FR-021 is PRE-DECLARATION time and it reads **UNKNOWN — not dropped, not invented**
(`serviceround3_internal_test.go`). They are different concepts, and neither is a per-sample
exclusion. Inventing one would be a new reliability model — facts, schema, aggregation, coverage —
smuggled in as a sentence, which is the same mistake as revision 5's saturation streak.

The trade-off the owner keeps, stated rather than hidden: cerbix's own shortage counts against the
target's number. That is mitigated where attribution belongs — the bounded reason on the heartbeat,
the region alert firing once per transition, and the metrics — and NOT by a silent exclusion from
reliability. If operations later needs target-versus-platform attribution in the number itself, that
is a change to the reliability model with its own design, not a special case here.

**Race and recovery, tested rather than asserted:** a leader that dies mid-dispatch leaves rows whose
deadlines expire, so the region's capacity returns without operator action; a leader change does not
double-count, because the new leader reads the same table; and the per-region limit and the per-monitor
in-flight lease are independent — one monitor cannot hold two slots, and four monitors cannot exceed
the region's four.

**D9 — no two executions of one monitor at a time, enforced in the CORE.** A DB-less executor cannot
take a lease, so the scheduler does not dispatch a second job for a monitor whose previous job is
in flight, and an in-flight record expires on its own deadline so a crashed executor does not park the
monitor forever. Consequences: the monitor's `interval_seconds` must be ≥ its `timeout_seconds`,
validated at the write boundary; and recovery after an executor crash is bounded by the in-flight
deadline, which the runbook states as a number rather than leaving to be discovered.

**D10 — cleanup is a VALIDATION, never a deletion.** `lifecycle_prefix` means the executor checks that
`result.lifecycle_path` begins with the declared prefix and fails the stage if it does not. The
executor is never given delete rights, and cerbix never removes an object it did not create. Reaping
is the target's lifecycle policy, and the schema does NOT require it to be declared (owner, revision 2:
"пусть оператор решает эту проблему; жёстко требовать от cerbix то, что не в его спеке, нелогично").
That is also the technically honest call: cerbix has no rights on the object store, so a mandatory
"name your reaping policy" field would be an assertion nobody can verify — the exact shape of promise
this project has spent two arcs learning not to make. The runbook says the consequence in plain words
instead: a canary creates a real object every run, and if nothing sweeps them, that is a bill somebody
pays quietly. `kind: none` is legal only with `acknowledged: true`, and that acknowledgement
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
`value` in a credential-bearing header entry and any position whose schema says a binding. A guard
that stops at the first level would pass exactly the documents this type introduces.

## 4. What one long probe costs, and where it is paid

D2 holds a delivery open for up to the completion timeout. That is the whole price, and it lands in
four places — each a decision, not a discovery:

1. **No new ceiling at all.** Revision 1 gave the type its own 900 s maximum; revision 2 drops that —
   `async_canary` lives under the same `maxTimeoutSeconds = 300` as everything else (D2a). This is the
   one cost of D2 that turned out to be avoidable, and avoiding it removes a mechanism rather than
   adding one.
2. **The pull CLAIM LEASE becomes per-job — and revision 1 named the wrong number, which is worth
   correcting in place rather than quietly.** There are two clocks on a pull job and they do different
   things: `expires_at`, set at enqueue to the monitor's INTERVAL (a job a full cycle late is stale and
   is dropped), and `lease_expires_at`, set at CLAIM to `pullJobLeaseSeconds` — **hardcoded 30 seconds
   in `handlers_agent.go`, for the whole batch, regardless of which monitors are in it**. It is the
   second one that matters here: a five-minute canary under a thirty-second lease is re-claimable while
   it is still running, and a second executor then submits again. That is precisely the duplicate D8
   exists to prevent, and no idempotency key saves us if the target does not honour it.

   The fix is a per-job lease stored on the row at enqueue (the scheduler knows the monitor's timeout
   there) and applied at claim, with the existing 30 s as the default for every job that does not ask
   for more. **Existing monitors are byte-identical under this change**; §4a says what it exposes.
3. **A separate AMQP queue per workflow kind.** Consumer prefetch is **16**. Sixteen canaries holding
   ten-minute deliveries would starve every ordinary check in that region. Capability-aware dispatch
   implies separate queues anyway; this makes it mandatory rather than tidy. The broker's own
   consumer acknowledgement timeout is an upper bound on this design and MUST be verified against the
   pinned RabbitMQ image before implementation — it is not asserted here from memory.
4. **Stage-level bounds inside the one probe.** `submit` gets a short timeout of its own (seconds), so
   a hung submit fails as `submit` rather than consuming the whole window and reporting `await_result`.

## 4a. What this touches that already exists — asked by the owner, checked rather than assumed

Three changes in §4 are not local to the new type, so each was traced through the shared code:

- **The type-scoped validation rules touch nothing.** `interval >= timeout` (D9) and the stage bounds
  live under `if m.Type == MonitorAsyncCanary` in `Monitor.Validate`. They MUST NOT be written as
  general rules: an ordinary HTTP monitor with a 30 s interval and a 60 s timeout is legal today and
  common, and a general rule would refuse it at its next write — the FR-028 D6b defect exactly, whose
  lesson was that a rule keyed on shape rather than on TYPE reaches configurations nobody meant it to.
- **The per-workflow-kind AMQP queue is additive.** New queue, new binding, existing queues and their
  consumers untouched; an executor that does not announce the capability simply never sees a canary
  job. It is an OPERATIONAL step on upgrade, under the same staged-broker discipline as D-0157, and
  the runbook carries it.
- **The pull claim lease is the one shared thing that changes**, and it is additive by construction:
  the per-job lease defaults to today's 30 seconds, so every existing monitor keeps the behaviour it
  has, byte for byte.

**And it exposes a defect that predates this feature, which is stated here rather than fixed by
accident.** A pull-region monitor whose `timeout_seconds` exceeds 30 (legal today — the bound is 300)
is already re-claimable while its probe is still running: the claim lease is 30 s and nothing derives
it from the probe's own length. For ordinary types the cost is an extra request and a duplicate
heartbeat, which is why nobody has noticed. For a canary the same race is a duplicate external
side effect, which is why this arc cannot leave it. Whoever implements phase D fixes it for every type
at once, and the release notes say so.

## 4b. The bounds, as numbers (P0, review round 2)

Revision 3 listed these as adjectives — "bounded response size", "bounded attempts" — and a test that
says "the edge and one past it" cannot be written against an adjective. Every limit v1 enforces, with
its value and its refusal:

| What | Bound | Refused at | Reason if exceeded |
| --- | --- | --- | --- |
| monitor `timeout` | ≤ 300 s (the product's existing bound, D2a) | write | `domain_invalid` |
| `interval` | ≥ `timeout`, ≤ 86400 s | write | `domain_invalid` |
| `submit_timeout` | 1–60 s, ≤ monitor timeout | write | `domain_invalid` |
| `completion.timeout` | 1 s – monitor timeout | write | `domain_invalid` |
| `poll.interval` | 1–60 s | write | `domain_invalid` |
| `poll.max_attempts` | 1–600, and `interval × max_attempts ≤ completion.timeout` | write | `domain_invalid` |
| headers per request | ≤ 16 (submit and completion counted separately) | write | `domain_invalid` |
| header name / value | ≤ 64 B / ≤ 1024 B | write | `domain_invalid` |
| bindings per monitor | ≤ 8 | write | `domain_invalid` |
| body: depth / keys per object / list elements / total / string leaf | 8 / 64 / 32 / 8 KiB / 1 KiB | write | `domain_invalid` |
| `multipart.fields` | ≤ 16, value ≤ 1 KiB | write | `domain_invalid` |
| `required_json_fields` | ≤ 16 paths | write | `domain_invalid` |
| `failure_events` / `failure.values` | ≤ 8 each | write | `domain_invalid` |
| fixture: registry maximum / `small_wav_v1` | ≤ 1 MiB / pinned at its exact size and SHA-256 | build + write | `domain_invalid` |
| `result.max_latency` | 1 s ≤ value ≤ the monitor's `timeout` (§5.5) | write | `domain_invalid` |
| reserved header names | `idempotency-key`, `host`, `content-length`, `transfer-encoding`; plus `content-type` on a multipart submit | write | `unsupported_field` |
| correlation id | ≤ 256 B, valid UTF-8, no control characters | run (`correlate`) | `correlation_invalid` |
| submit / poll response body read | ≤ 256 KiB | run | `response_too_large` |
| SSE single event / single line | ≤ 64 KiB / ≤ 16 KiB | run | `event_too_large` |
| SSE bytes over one connection | ≤ 8 MiB | run | `response_too_large` |
| redirects per request | ≤ 3 | run | `too_many_redirects` |
| concurrent canary executions per region | ≤ 4 | dispatch | `region_saturated` |
| DNS / connect / TLS handshake | 5 s / 5 s / 5 s each | run | the stage's transport class |

Write-time bounds are checked by the schema on every surface; run-time bounds are the executor's, and
each has a normalized reason that never carries the value that broke it.

**The transport retry policy, fixed here rather than left to an implementer (P1, review round 2).**
Submit retries at most **twice** on a connection-level failure BEFORE any response byte is read, with
the same idempotency key and a 1 s then 3 s pause; it never retries after a response status is seen,
because a status is an answer. Poll does not retry outside its declared attempts. SSE does **not**
reconnect in v1 — a dropped connection fails `await_result` with a transport class, and reconnection
is deliberately deferred because resuming a stream correctly needs a resume token the contract does
not have.

**The lease and in-flight formulas (P1, review round 2).** Pull claim lease = `timeout + 30 s`.
In-flight deadline = `timeout + 60 s`, after which the scheduler may dispatch the monitor again — so a
crashed executor parks its monitor for at most that, and the runbook states the number rather than
leaving it to arithmetic.

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
    interval: 5m             # >= timeout (D9)
    timeout: 5m              # <= 300s — the product's existing bound, no per-type ceiling (D2a)

    workflow:
      kind: async_transaction_v1

      # Every project secret this workflow uses, named ONCE (D3b). A position writes `secret_ref`
      # — the brief's own spelling — but its value is a BINDING declared here, not a project-secret
      # name, which is what keeps rename/rotation/delete on the existing flat path (§11.4).
      #
      # This map is INPUT ONLY (D3f): it is validated here and then projected into the flat
      # `canary_secret_<binding>_ref` keys, which are the sole persisted identity. The stored
      # document keeps the `secret_ref: upload` markers and no project-secret name at all, so a
      # rename touches one place and the stored document does not go stale behind it.
      secrets:
        upload: charla-upload-token      # binding name → project secret name

      submit:
        kind: multipart_fixture          # | http_json
        method: POST                     # POST only in v1
        url: https://files.example.com/files/upload
        submit_timeout: 30s              # own bound, so a hung submit fails as `submit` (§4.4)
        accepted_status: [202]           # what "accepted" means here; anything else fails submit (D3d)
        headers:
          - name: authorization          # credential-bearing → a binding ONLY, never a value (D7)
            secret_ref: upload
          - name: x-tenant               # ordinary → typed non-secret value
            value: canary
        fixture_ref: small_wav_v1        # multipart_fixture only; a registry key (D11)
        multipart:
          file_field: file
          fields:
            only_audio: false            # typed scalars: string | number | boolean
        # http_json only — the closed algebra of D3a, never a JSON string. A credential enters a body
        # ONLY as a `{secret_ref: <binding>}` node; a literal string leaf is NOT detectable (D3a residual).
        # body:
        #   tenant: canary
        #   attempts: 1
        #   dry_run: false
        #   token: { secret_ref: upload }   # the binding, resolved at dispatch (D3b)

      correlate:
        source: response_json            # | response_header
        path: task_id                    # JSON-path grammar (D5)
        # header_name: task-id           # response_header only — RFC 7230 token grammar (D5)

      completion:
        kind: sse                        # | poll_json
        url: https://files.example.com/tasks/{{ correlation_id }}/events
        timeout: 5m                      # <= the monitor's timeout
        # Completion NEVER inherits submit's headers (D3c). Declare what it needs; a binding here
        # requires the completion host to equal the submit host, and is dropped on a cross-host
        # redirect.
        headers:
          - name: authorization
            secret_ref: upload
        sse:
          success_event: task.completed
          failure_events: [task.failed]
          required_json_fields: [s3_path, byte_size, media_type]
        # poll_json:
        #   interval: 5s
        #   max_attempts: 60               # interval * max_attempts <= completion timeout
        #   success: { path: status, value: completed }
        #   failure: { path: status, values: [failed, cancelled] }

      result:
        max_latency: 4m                  # the PROMISE (§5.5); the monitor timeout is the LIMIT
        # Asserted against the RESULT DOCUMENT, defined in §5.4 — for `sse` it is the JSON payload of
        # the success event; for `poll_json` it is the body of the response that satisfied `success`.
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
- Durations canonicalize to whole seconds; the YAML accepts `5m`, the canonical form is `300`.

### 5.5 `result.max_latency`: the promise, measured once

Revision 7 had it in the example and nowhere else — no bound, no start, no end, no failure. It is the
journey's declared PROMISE, distinct from the monitor's `timeout`, which is the LIMIT that stops the
probe:

- **Start:** immediately before the FIRST submit attempt leaves the executor.
- **End:** after `cleanup_validation` completes — the last thing the stage list contains, so the
  measurement covers what the operator declared and not a prefix of it.
- **It includes** the transport retry waits (D8's 1 s and 3 s), because an operator's promise is about
  the journey and not about our internal luck. It is the same number the heartbeat reports as latency.
- **Bound:** 1 s ≤ `max_latency` ≤ the monitor's `timeout`, checked at the write boundary. Equal is
  legal and means "the promise is the limit".
- **Failure:** a journey that COMPLETED but took longer than the promise fails `assert_result` with
  `latency_exceeded` — a real outcome, distinct from a timeout, which is the probe being stopped
  before it finished.

### 5.4 The result document

Assertions need one named thing to assert against, and revision 1 left it implied. **The result
document is:** for `completion.kind: sse`, the JSON payload of the event whose type equals
`success_event`; for `poll_json`, the body of the response that satisfied `success`. Nothing else is
the result — not an earlier event, not the submit response, not a later event that happens to arrive
before the connection closes. `required_json_fields` and `lifecycle_path` address that document
through the D5 grammar. A result document that is not valid JSON, exceeds the response bound, or lacks
the path named by `lifecycle_path` fails `assert_result` with the field named and the value never
echoed.

### 5.3 The semantic hash

**It changes when, and only when, execution semantics change:** the workflow kind or
version, any URL, method, submit kind or body, the fixture id, the accepted-status set, every BINDING
and the POSITIONS it is used in, and the flat `canary_secret_<binding>_ref` mapping — which project
secret fills which slot, by NAME and not by value, so rotating a secret's value does not move the hash
and must not reschedule the monitor, while pointing a binding at a DIFFERENT secret does. The hash is
taken over the canonical document string (markers, no `secrets` map — D3f) together with those flat
refs, so the two halves cannot disagree about identity — the
correlation source or path, the completion kind and every field under it, timeouts and attempt bounds,
every assertion, and the cleanup kind, prefix and acknowledgement. Reformatting the YAML, reordering
maps, reordering a set-list or changing a comment does not move it.

## 6. Acceptance invariants (FR-029 / NFR-024)

1. A bundle declaring `async_canary` is accepted only if EVERY field validates against the typed
   schema; one unknown field, one wrong union branch, one out-of-range bound refuses the whole bundle
   with the key named, and the previous last-known-good is retained.
2. A `value` in a credential-bearing header is refused by SCHEMA at any depth, inside any list, with
   the position named and the value never echoed; a `{secret_ref: <binding>}` node resolves from the
   project inventory at dispatch and its value never enters the document; a binding no
   `workflow.secrets` entry declares is refused, and a declared binding nobody uses is refused. What this invariant does NOT claim, per D3a and
   FR-028's D7: a credential pasted as an ordinary string leaf in a body or a non-credential header is
   not detectable and is not refused.
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
    rebinding answer cannot be used. **There is no configuration that relaxes it** — no `allow_private`
    flag, in v1 or as a hidden setting, because a flag reachable in production is the policy's own
    bypass. Tests reach a local fixture through an INJECTED dialer at the seam, never through a
    product option (§7).
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
bundle; a `value` in a credential-bearing header refused, at top level and nested in a list; a
`{secret_ref: <binding>}` node in a body resolving to an envelope field with nothing in the stored
document; a binding used but not declared, and declared but not used, each refused; a body
that breaks the algebra (null, unknown node shape, key outside the grammar, over-depth, over-size)
refused with the position; an arbitrary fixture path or URL refused; an unknown field refused; a
semantic no-op retaining the revision; a semantic change bumping it exactly once; an invalid
replacement preserving last-known-good. **And one test that asserts a NON-guarantee**, so a later
reader does not mistake silence for coverage: a credential pasted as an ordinary string leaf in a body
is ACCEPTED, because it is undetectable (D3a) — the case is named in the matrix rather than missing
from it.

**The secret projection and the three rename scenarios (P0, review rounds 3–5).** Revision 7 changed
what a rename DOES and left this matrix demanding the old behaviour — the same slip twice in one
document, so the scenarios are now split the way the code splits them:

- *file-managed* — renaming a project secret referenced by a file-managed canary is REFUSED with
  `SecretRenamedInUseError`; the stored config and the monitor's revision are unchanged, and the
  canonical workflow string is untouched;
- *UI-managed* — the rename succeeds, `repointSecretRefs` re-points the flat
  `canary_secret_<binding>_ref`, the canonical `config["workflow"]` string is BYTE-IDENTICAL, and an
  API read afterwards shows the new name. No semantic revision is involved at all: a UI-managed
  monitor has no semantic hash (D3g);
- *bundle re-points a binding at a DIFFERENT secret* — the semantic hash moves and the revision bumps
  exactly once; a re-apply of the unchanged bundle after that moves neither.

Plus, unchanged by the split: rotation of a secret's VALUE needs no monitor edit and moves no hash ·
the delete guard counts both bindings · and a binding moved between two positions produces a different
canonical string, so the envelope's body digest fails before any request.

**Region capacity and shortage (P0, review rounds 4–5).** The fifth dispatch into a full region is
refused; that scheduled run writes ONE ordinary DOWN heartbeat with `region_saturated`, and the sample
counts as UNAVAILABLE in the service's SLI like any other DOWN — the earlier "ungoverned" expectation
is gone from this matrix with the decision that produced it · a region with no capable executor
produces the same shape with `no_capable_runner` · capacity returns when an in-flight deadline expires
and the next scheduled run dispatches normally · a leader change mid-dispatch does not double-count ·
the per-region limit and the per-monitor in-flight lease are independent, proven by four monitors
saturating a region while one monitor still cannot hold two slots · and the region-alert edge fires
once per transition rather than once per monitor.

**Credentials and redirects (P0, review round 5).** A submit that redirects to a different public
host: the target receives NO binding-backed header, `x-api-key` asserted by name because `net/http`'s
own default would have let exactly that one through while stripping `authorization` · the same across
a port change on the same host · a same-host, same-port redirect KEEPS them, so the rule is not a
blanket strip that breaks ordinary flows · completion inherits nothing from submit in any case · and a
completion binding whose host differs from submit's is refused at the WRITE boundary, never sent.

**Reserved headers and the promise (P1, review round 5).** `idempotency-key`, `host`,
`content-length`, `transfer-encoding` and — on a multipart submit — `content-type` are each refused at
the write boundary, case-insensitively, on both stages · a journey that completes inside the monitor
timeout but outside `max_latency` fails `assert_result` with `latency_exceeded`, distinct from a
timeout · the measurement starts before the first submit attempt and ends after cleanup validation,
proven by a run whose retry waits are what push it past the promise.

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

**The fixture and the URL policy contradict each other, and the resolution is explicit (P0, review
round 1).** §6.12 refuses plaintext HTTP and private addresses, and a local fixture is both. The
happy-path E2E therefore reaches the fixture through a dialer injected AT THE SEAM in the test build —
not through a product flag, which would put the bypass in production. That leaves the policy itself
unproven by the E2E, so it is proven twice elsewhere: by unit tests over a table of addresses and
redirect chains, and by ONE E2E case that points a real monitor at a private address on the REAL path
and asserts the policy reason. Happy path with injection, policy without it.

**Correlation-id handling** (P0, review round 1): an id containing `/`, `?`, `#`, `@`, a newline or a
percent sequence is percent-encoded into one path segment and the request still addresses the intended
resource; an id over 256 bytes, one with a control character and one that is not valid UTF-8 each fail
`correlate` with a typed reason; the substituted URL is re-validated by the policy, proven by an id
crafted to move the request to another host.

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

## 11. Owner questions — ANSWERED (2026-09-03, revision 2)

1. **The transaction ceiling — lower.** The owner asked whether it could be smaller than the proposed
   900 s. It can, to the point of deleting the mechanism: the type inherits the product's existing
   `maxTimeoutSeconds = 300` (D2a). **One thing this makes urgent rather than optional:** the first use
   case's real duration. If a Charla upload can legitimately exceed five minutes under load, 300 s is
   the wrong number and the wrong failure — a healthy-but-slow service reported DOWN. Phase C's first
   measurement is the p99 of that journey, and the number is revisited then with evidence rather than
   argued now.
2. **The lifecycle policy — the operator's problem, not a schema field.** Accepted, and the reasoning
   is stronger than convenience: cerbix has no rights on the object store, so a mandatory declaration
   would be an unverifiable claim. The runbook states the consequence (D10).
3. **Private-address targets — refused in v1, no override.** Confirmed.
4. **SIGNED OFF by the owner, 2026-09-03: the binding map stands.** The one deviation from the brief,
   decided rather than assumed. The brief wrote `secret_ref: <project-secret-name>` at each position. This design keeps the FIELD NAME
   and changes what the value means: it names a BINDING declared once under `workflow.secrets`, which
   maps to the project secret. The reason is mechanical rather than aesthetic — the flat key
   `canary_secret_<binding>_ref` is what keeps rename, rotation and delete-counting on the path
   `password_ref` already runs on, with no canary-aware code (D3b, D3e). Writing the project-secret
   name at each position would need either a nested-aware repoint or a synthesized binding name, and
   FR-028 already paid to learn which of those is cheaper. The independent reviewer recommended the
   same on technical grounds — one reference per secret, the existing rename/delete/rotation paths, and
   the positions covered by the execution digest without nested storage — and the owner chose it. The
   deviation is now a decision with a name on it, not an implementer's preference.
5. **CLOSED without needing you — I had reopened a decision you already made.** Revision 6 proposed
   excluding `region_saturated` and `no_capable_runner` intervals from the SLI as "ungoverned". The
   brief already said these become an ordinary monitor DOWN feeding the ordinary `monitors[]` and
   `sli[]` with no separate reliability model, and the analogy I reached for was wrong on the facts
   besides (maintenance is suppression; FR-021's "ungoverned" is pre-declaration time that reads
   UNKNOWN). They count as unavailable. Attribution lives in the reason, the region alert and the
   metrics. An operator canarying a LAN
   service is blocked in v1, and the threat model says so rather than leaving it to be discovered.

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
