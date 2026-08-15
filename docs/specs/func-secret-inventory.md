# func-secret-inventory — Project secret inventory & `*_ref` for the file provider (FR-020)

Status: **APPROVED (design r7 amendment, 2026-08-15).** UI mock approved by the owner; six
independent design passes (r1–r6) approved the original design, whose r6 fixation — the
authoritative read is the dispatch-authorization **linearization point** — stands unchanged.
**r7 is a post-implementation amendment** (§2, §4.4.5, §4.7, §9, §10) closing **three
confirmed audit blockers** — pull-claim blindness, missing execution binding, envelope
field-set/whole-envelope stripping — **plus one P1** (publish-failure backoff, §4.4.5). Two of
the blockers are holes in THIS DOCUMENT, faithfully implemented; one is a defect the document
never required a test for. Both review rounds returned CHANGES REQUIRED; every technical
finding from them is applied in this text, and by the two-round contract there was no third
round. The one item that was not a review finding — the §2 A/B fork on whether carrier routing
metadata stays outside the threat model — was **decided by the owner on 2026-08-15 as option
A**, which closes the amendment. Approval covers the SPEC, never its implementation — code is
accepted only against the §9 matrix plus
D-0155/**D-0160**/traceability/runbook/iteration evidence.

Implementation acceptance: **RESTORED in iter-0120.** `iter-0116` accepted the
implementation against the r6 matrix and that report stands as written — it is superseded,
not corrected: the r6 matrix could not have caught these defects because it never asked for
the tests §9 now requires. iter-0119 fixed all four audit findings with regressions verified to
fail when reverted and closed §10 slice 4, but deliberately withheld acceptance while three
§9 items were unevidenced. iter-0120 closes them — a live auth-less-target fail-open pin
(mutation-proven), live generation-3 emission with a mixed-capability barrier, and a browser
gate — and additionally fixes three P0s the gen3 review found inside slice 4 itself. The
amended matrix is fully evidenced and acceptance is restored there, not here.

**Gates — two, with different scopes; both updated by iter-0119:**

- **Availability gate (pull only) — LIFTED in iter-0119.** The claim barrier was
  two-directional and blackholed every non-credentialed monitor in a pull region. It is
  fixed (one atomic capability-bounded lease for jobs and tests), the regression is verified
  to fail when the fix is reverted, and `make secret-smoke` exercises a live pull agent.
- **Security statement (both transports) — CLOSED in iter-0120.**
  Execution binding, the structural gate and the field-set rules have shipped, so retargeting
  a credentialed probe or stripping its credential now fails closed before any connection to
  the target. Acceptance is not re-declared: three items of the §9 matrix remain unevidenced
  (the fail-open case pinned against a live auth-less target, a browser E2E for the Secrets
  panel lifecycle, and generation 3 exercised in a live mixed-capability fleet). See
  [`iter-0119`](../iterations/iter-0119.md) §8.

## 1. Context & motivation

The Monitoring-as-Code file provider (FR-017) forbids inline secrets (D-0152) and rejects
credentialed monitor types with `unsupported_type` until "the referenced secret-inventory
contract exists" (func-monitoring-as-code §3.1). This spec is that contract: a
**project-scoped secret inventory** managed only via UI/API, plus strict per-type `settings`
schemas whose credential fields are **references** (`password_ref: <name>`) — never values.

## 2. Requirement

- **FR-020**: A project member with write access manages named secrets in a per-project
  inventory (create; edit = rename and/or rotate; delete; values write-only). Bundles may
  declare `postgres`/`mysql`/`redis`/`rabbitmq` monitors via typed `settings` with `*_ref`
  fields resolved against the bundle's project inventory. UI/API monitors may use the same
  references.
- **NFR-015 (fail-closed secrecy, honest boundary)**: A secret value exists in plaintext in
  exactly three places: (1) **transiently in the core materializer's memory** while it
  decrypts the at-rest ciphertext and immediately re-wraps it for dispatch (buffers zeroed
  best-effort, plaintext never returned to callers, never placed in any snapshot or cache);
  (2) inside the executing prober process at probe time; (3) on the wire **to the monitored
  target** (delivering the credential is the probe's purpose; §4.8 makes unencrypted target
  transport an explicit opt-in, never a default). Everywhere else — inventory rows, the
  scheduler snapshot, dispatch payloads in the broker, dead-letter queues, `pull_jobs` rows —
  the value exists only as AES-256-GCM ciphertext under **two separate keyrings**: the
  at-rest master (never leaves core) and per-region dispatch keyrings (§4.7). Values are
  never returned by any API, never in logs/audit/metrics/diagnostics/hashes, never
  resolvable across a project boundary. The feature is unavailable without its keys — no
  plaintext fallback, enforced at config load (§4.1). **And no credential-LESS fallback:** a
  monitor whose definition carries a credential never executes without it. A credential that
  is missing, empty, or not openable is a typed failure (`probe_error`, §4.7) raised *before*
  any connection to the target — never a silently downgraded unauthenticated probe, and never
  a result recorded as liveness. **"Missing or empty" includes a credential that decrypts to a
  zero-length value:** an envelope carrying the exact expected field name with empty plaintext
  must fail identically, otherwise the field-set rule is bypassable by content instead of by
  shape.
- **Threat scope of that guarantee, stated precisely (r7) — it is narrower than "nobody can
  strip a credential".** It holds against a party who can modify the execution **body** inside
  an authenticated v2 carrier. It does **not** hold against a party who can author the
  carrier's own routing metadata: writing an arbitrary `pull_jobs` row or publishing to a v1
  queue, where they may set `protocol_version`, omit the envelope entirely, or change the
  monitor `type` to a non-credentialed one. A DB-less executor cannot distinguish a
  legitimately credential-free legacy job from a downgraded one, because nothing it holds is
  authenticated — no per-field AAD can fix that, since the attack is to have no fields at all.
  The structural gate (§4.7) closes every shape reachable inside a trusted-generation carrier,
  and that is the whole guarantee this spec makes — **option A below, chosen deliberately.**
  It would be false to claim the contract specified here *grows into* "every job is
  authenticated": a credential-free job carries zero ciphertexts, therefore no GCM tag and no
  MAC of any kind, so per-field AAD has nothing to authenticate with. Promising that end state
  while also forbidding an envelope on credential-free schemas would be two contracts at once,
  and r7 round 1 did exactly that.
- **DECIDED by the owner, 2026-08-15: option A. Recorded in D-0160**, together with B's
  rejection and its cost, so the trade-off is not rediscovered as a surprise later. This was a
  product-risk call, not a technical preference:
  - **A — chosen; specified here.** The carrier generation is trusted metadata, and a party
    who can author carrier routing (an arbitrary `pull_jobs` row, or a publish into a legacy
    queue) is **permanently** outside the threat model. Cost, stated without softening:
    someone holding broker write — a credential typically distributed more widely than the
    core database's — can emit a legacy-generation job for a credentialed monitor and obtain
    an anonymous probe that reports Up. Monitoring can be made to lie by anyone who can write
    to the queue.
  - **B — declined for now; its own spec section and D-record if ever revisited.** A root
    `job_auth` tag (MAC over the canonical execution DTO + carrier generation + monitor id +
    execution revision + job id) on **every** job in `enforced` mode, credential-free ones
    included, verified before any prober runs. This is what actually closes the downgrade
    path — and note the tri-state rule forbids a *credential envelope* on a credential-free
    schema, which does not forbid a `job_auth` tag there. Cost: it changes the
    non-credentialed dispatch path, requires a fully capable fleet, and is full-job
    integrity — materially larger than FR-020, which is precisely why it must not be smuggled
    in under this clause.
  Under A, the residual for legacy-generation traffic is exactly today's pre-FR-020
  property — not a new exposure introduced by this feature, but stated here rather than left
  to be inferred.

## 3. Scope & non-goals

In scope (v1): inventory + normalized `monitor_secret_refs`; domain-owned typed settings
schemas; authoritative-read `MaterializeExecutionConfig` on all six dispatch paths;
versioned credential envelope with a **verifiable wire barrier** (v2 queue / agent
capabilities); typed executor `probe_error` outcome; ciphertext-only scheduler snapshot;
rotation fencing; per-region dispatch keyrings with AAD binding; Secrets UI panel per the
approved mock.

Non-goals: org/shared secrets; external secret managers; user-facing value history;
hard recall of already-enqueued jobs (post-read in-flight exposure is bounded by ACK/TTL/
DLQ purge and fenced at ingest — §4.4.3);
job-scoped one-time secret fetch for pull agents (v2 refinement); asymmetric per-executor
keys; `promql`/`synthetic`/`composite` file support; retroactive normalization of existing
UI-monitor configs (their **snapshot handling** does change — §4.4.2 — but stored rows are
untouched).

## 4. Model & semantics

### 4.1 Inventory entity & fail-closed configuration

`project_secrets`: `id uuid`, `project_id` FK→projects `ON DELETE CASCADE`, `name` slug
(`^[a-z][a-z0-9-]{0,62}$`), `value_encrypted text NOT NULL`, `created_at`, `rotated_at`;
unique `(project_id, name)`.

Strict config (no warn-and-continue): feature switch `secrets.enabled: true` validated by
the central loader at startup:

- requires `security.encryption_key` (at-rest master) AND a valid `security.dispatch`
  keyring (§4.7) on materializing roles (api/all, scheduler) — else the process fails
  startup;
- an executor configured with `secrets.enabled: true` but no dispatch keyring fails
  startup; one that persistently cannot decrypt received envelopes degrades readiness and
  reports it in its heartbeat/capabilities (§4.7);
- `secrets.enabled: false` (default): Secrets API → `404 feature_disabled`; `*_ref` bundles
  reject as bindable errors (per-project freeze); nothing else changes;
- **role-ownership gate (closed in iteration 1 review):** `worker` owns only operational
  HTTP plus its regional jobs/tests consumers. It does not instantiate instance settings,
  mailer, generic outbox delivery, the user/API router, or the authoritative materializer;
  those stay in the master-key trust domain (`api`/`scheduler`/`all`). Thus a DB-backed
  executor cannot claim master-encrypted core work, and executor profiles carrying the
  master are rejected even while the feature/envelope mode is disabled;
- `CreateProjectSecret`/`UpdateProjectSecret` hard-fail on a nil cipher; empty values →
  `400`.

### 4.2 Bundle contract & credential-field policy

Typed `settings` are owned by `internal/domain` — ONE validator for API/UI/MaC; the file
provider owns YAML shape/strictness only. Policy by surface: **MaC** — literal credential
keys forbidden (D-0152), `_ref` required; **API/UI** — exactly-one-of `password` |
`password_ref`; **test-before-save** — a raw value is materialized straight into the
envelope (no ref row).

| type | required | optional (default) | notes |
|------|----------|--------------------|-------|
| `postgres` | `username`, `database`, `password_ref`\* | `sslmode` (**`require`** for ref-based; allowlist `disable\|require\|verify-ca\|verify-full`), `query` (`SELECT 1`, ≤ 1 KiB) | `require` = **encrypted, server identity NOT verified** — stated plainly; `verify-ca`/`verify-full` available for verified TLS. Runtime's historical default `prefer` is intentionally not carried over for ref-based monitors; existing UI monitors unmigrated. `disable` = explicit opt-in (§4.8). |
| `mysql` | `username`, `database`, `password_ref`\* | `tls` (**`true`** for ref-based; verified against system roots + hostname per Go TLS defaults), `tls_skip_verify` (explicit opt-in, never silent), `query` (`SELECT 1`, ≤ 1 KiB) | `tls: false` = explicit opt-in. |
| `redis` | `password_ref`\* | `username` (ACL), `tls` (**`true`**, verified), `tls_skip_verify` (explicit) | today's prober AUTHs over plain TCP; v1 adds TLS. |
| `rabbitmq` | `mode` | management: `username`, `password_ref`\*, `path` (≤ 512 B), `tls` (**`true`**, verified), `tls_skip_verify` (explicit) | **Conditional:** `mode: amqp` = protocol-header handshake only (today's prober), credential fields **forbidden**; `mode: management` requires them, defaults https. |

\* in MaC; in API/UI the slot is exactly-one-of value/ref. Bounds: `username`/`database`
≤ 256 B. `*_ref` must match the secret-name slug (`invalid_ref`, bindable). Unknown keys
reject — no `config: map[string]string` escape hatch. **Canonical hash covers the ref NAME,
not the value.**

### 4.3 Referential integrity — `monitor_secret_refs`

`monitor_secret_refs (monitor_id FK→monitors ON DELETE CASCADE, project_id, setting_key,
secret_id FK→project_secrets(id) ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED)`, with
a composite tenant-safe FK. Maintained atomically by every monitor write.

- **Delete guard DB-enforced**: commit-time FK failure → `409 secret_in_use` (exact counts
  from ref rows); deferred NO ACTION keeps the project-delete cascade order-independent
  (verified on real PG, both storage modes).
- **Rename/apply serialization**: ref-resolving writes (`Apply`, API save) take `SELECT …
  FOR KEY SHARE` on referenced secret rows; rename takes `FOR UPDATE` and re-checks
  file-managed refs after the lock; fixed lock order (secret rows by id asc, then monitor
  rows) — the persisted `*_ref` name and the ref row's `secret_id` cannot diverge.
- Quota (≤ 100/project) under a per-project transactional advisory lock shared with Create.

### 4.4 Resolution — validate at save/apply, materialize from the authoritative read

1. **Save/apply time:** every `*_ref` must resolve (under §4.3 locks) to a same-project
   secret before any writes; failure = typed bindable `BundleError(secret_ref_not_found)`
   → per-project freeze with LKG; UI/API saves fail `400`. Config stores the NAME; the ref
   row stores `secret_id`.
2. **Decrypt-free read split (not post-decrypt redaction):** credential values are
   write-only, so NO display/list/detail path ever needs them. The store read surface is
   split: API, domain, provenance, diagnostics and the scheduler snapshot all consume a
   **no-decrypt DTO** in which secret config keys are omitted by schema — neither plaintext
   nor ciphertext is ever serialized outward; post-decrypt redaction is no longer a
   security boundary. The ONLY decrypt callers are the authoritative materializer (§4.4.3)
   and the reencrypt/rotation internals. Tests: monitor Get/List/API serialization never
   invokes Decrypt and contains neither plaintext nor ciphertext, for inline AND ref
   monitors; the snapshot is plaintext-free for both kinds.
3. **Authoritative-read materialization (config, eligibility, routing, cadence — all from
   one read):** `MaterializeExecutionConfig` runs immediately before every enqueue/publish —
   scheduled AMQP, scheduled pull, AMQP test-RPC, pull test, local/inproc test,
   unsaved-config test — and reads **the entire execution row (config, `execution_revision`,
   `enabled`, type, `region`, interval/confirm inputs) + secret ciphertext in ONE database
   read**, then builds the job wholly from that read (never from the cached snapshot
   Monitor). The snapshot only *nominates* due candidates; the read **re-authorizes**:
   - **Eligibility:** rows now missing, `enabled=false`, or of a non-active/testable type
     are **skipped** (`skipped_current_state`): no job, no cadence advance as "sent", and
     the snapshot entry is refreshed/pruned. A disable landing between the cadence decision
     and the read can never dispatch (and the ingest gate additionally refuses results for
     disabled monitors — belt and braces).
   - **Routing:** after the read, due entries are **regrouped by the authoritative
     `region`**, and only then is the region keyring and the v2 transport/queue selected —
     the AAD `region` and the routing region come from the same row. A region move landing
     before the read can never encrypt a credential under the old region's key or publish
     to the old region's queue.
   - **Cadence:** `nextRun` advances using the authoritative interval/confirm inputs from
     the same read, never the snapshot's stale values.
   **Linearization point (precise boundary, not an absolute):** the authoritative read is
   the point at which dispatch is authorized. Any change COMMITTED BEFORE the read —
   disable, region move, target/secret change — is honored: no old-region encryption, no
   old-target job, for rotate AND ordinary concurrent `UpdateMonitor` (all §9 regressions,
   tested on both sides of the read). A change committed AFTER the read cannot recall the
   already-materialized old-revision job: it may still reach the previous executor/target,
   its RESULT is rejected by the D-0142 fence at ingest, and its payload persists until
   ACK/TTL/DLQ purge (in-flight exposure — recorded in the D-0155 exposure statement).
   Immediate credential revocation is achieved by rotating the credential at the TARGET;
   hard recall of enqueued jobs is an explicit non-goal. Non-credentialed monitors keep
   today's snapshot-built dispatch unchanged.
4. **Bounded batch materialization (no N+1 storm):** for a scheduler tick, the due
   credentialed set is materialized as a **bounded batch per region** (single joined query
   over the due monitor ids; concurrency cap + statement timeout consistent with the
   existing apply-tx bounds). Failures are per-monitor (one bad ref never blocks the
   batch); envelope/payload size is bounded post-base64 within the existing payload caps.
5. **Materialization failure** (missing row, decrypt error, missing key) = operational
   dispatch rejection: no job, `cerbix_secret_resolution_failed_total{reason}` + rate-
   limited names-only log, cadence does not mark a probe as sent; never DOWN. A
   **persistently failing** monitor (e.g. permanently corrupt at-rest ciphertext) is
   retried with a **backoff floor** (never faster than its own interval, with exponential
   backoff up to the resync period) so "no cadence advance" cannot degenerate into a
   per-tick DB/decrypt hammer.
   **The floor covers every path that ends without a dispatched job, not only materialization
   (r7).** Materialization failure and **publish/enqueue failure** (broker unreachable, queue
   refused, pull-row insert failed) are distinguishable in cause but identical in this
   respect: both must leave the monitor on the backoff schedule rather than eligible again on
   the next tick. Otherwise the one case that is *guaranteed* to hit every credentialed
   monitor at once — a broker outage — turns each 1s tick into a full authoritative-read +
   decrypt + seal storm across the whole due set, i.e. the failure mode this floor exists to
   prevent, reached through the door it did not cover. The two causes keep separate metric
   reasons (a publish failure is an operational transport fault, not a secret-resolution
   fault) and neither marks a probe as sent.
   **Retry eligibility is defined as state, not as a rate** — so it is verifiable without
   depending on a clock. **On failure:** the consecutive-failure counter increments, the
   next-eligible time becomes `now + backoff`, and the cadence/"sent" marker is **not** set.
   **On success:** the counter resets, the "sent" marker **is** set, and next-eligible returns
   to ordinary cadence (`now + authoritative interval`) — an r7-round-1 wording said the marker
   was set in neither case, which read literally would re-dispatch a successfully published
   monitor on the very next tick. "Not marked as sent" and "eligible again on the next tick"
   are different statements, and treating them as one is precisely how the storm arose. §9
   therefore asserts **at most one attempt per backoff window** with the window fixed by the
   test, rather than a count of reads over a count of ticks. **Scope:** this governs the
   credentialed materialize→publish path that this spec owns; the general non-credentialed
   publish path keeps today's behaviour and is not silently changed here.

### 4.5 Rotation fencing

The rotate tx: locks referencing monitor rows in the fixed §4.3 order, bumps their
`execution_revision` via the exact **D-0142-safe watermark path** `UpdateMonitor` uses
(disabled monitors included), writes audit, and sends `monitor_config_changed` NOTIFY —
audit/NOTIFY only when rows actually changed — all in ONE transaction. The scheduler's
`ConfigNotifier` reloads push-based (15s refresh as fallback). With §4.4.3, in-flight
old-value jobs carry the old revision and are rejected at ingest; the next dispatch reads
the new value + new revision atomically. Hash/generation untouched — reconcile stays no-op.

### 4.6 Edit, deletion & lifecycle guards

One `PATCH` = rename and/or rotate. **Rotate**: any time; refs intact; fencing §4.5.
**Rename**: under `FOR UPDATE`; file-managed refs → `409 secret_renamed_in_use`; UI-only →
atomic re-point via a dedicated metadata write path (not `UpdateMonitor`; identity/value
unchanged → no revision bump), audit records the sweep; unique slug (`409` duplicate).
**Delete**: `409 secret_in_use` while referenced; no force-delete. Project delete cascades
inventory + refs.

### 4.7 Dispatch keyrings, envelope, wire barrier

**Keyrings.** Two, never shared:

- `security.encryption_key` — at-rest master; executors never receive it.
- `security.dispatch` — per-region **keyrings**: `region → {primary: {id, key},
  previous: [{id, key}, …]}`. Core encrypts with the region's primary; executors decrypt by
  `key_id` (primary or previous), enabling dispatch-key rotation while queued/DLQ payloads
  drain. Config validation: `key_id`s unique AND key bytes distinct within a region;
  startup rejects a region mismatch between an executor's configured region and its
  keyring. A `default` entry is valid **only** in single-region deployments or under an
  explicit `shared_trust_acknowledged: true` — which honestly widens the recorded exposure
  statement (one key opens all regions' retained payloads) and is surfaced in the config
  status/metric. Dispatch-key rotation has its own runbook: retire a previous key only
  after max job TTL **and** an explicit DLQ purge (or a recorded acknowledgement that
  old-key DLQ ciphertext is unrecoverable-by-design); at-rest `reencrypt` does not apply
  to payloads.

**Envelope.** A **typed transport DTO** (never written into persisted `Monitor.Config`,
never passing domain/API redaction surfaces): `credential_envelope: {v, region, key_id,
job_id, fields: {password: <AES-256-GCM ciphertext>}}`. `job_id` is **always
core-generated before encryption** (no transport-dependent context).

**Envelope version and rollout barrier (r7 — the binding change is a WIRE change).** The r7
AAD is not compatible with the r6 AAD, so it ships as `v: 2`, never as a redefinition of
`v: 1`. Silently changing what `v: 1` binds would break a rolling upgrade in both directions:
a new sealer and an old executor derive different AAD for the same declared version, so an
old executor fails every new job with `decrypt_auth_failed`, and a new executor cannot open
old envelopes still queued. Normative:

- Executors advertise `capabilities: {credential_envelope: 2}`; the existing `1` keeps its
  meaning, so capability is generational, not boolean, and the §4.4.4 existential readiness
  check counts executors capable of the **binding generation core is about to emit** — a
  `credential_envelope: 1` executor is not evidence of readiness for a `v: 2` region.
- Core emits `v: 2` into a region only once that region's existential check passes for
  generation 2; until then it keeps emitting `v: 1` (or refuses, per §4.4.4) — mixed
  generations are resolved by capability, never by hope.
- **The carrier generation is named, not delegated (r7 round 2).** "Reuse the existing v1/v2
  split" would be wrong: today's `protocol_version = 2` / `checks.jobs.v2.<region>` /
  `/v2/jobs` carrier already *means* envelope `v: 1`, and every capability-1 executor consumes
  it — so putting `v: 2` envelopes on that carrier hands them straight to executors that
  cannot open them, and a capability check does not stop a consumer from taking a message.
  The mapping is therefore explicit and physical: **carrier generation 2** (`protocol_version
  = 2`, `checks.{jobs,tests}.v2.<region>`, `/v2/{jobs,tests}`) keeps envelope `v: 1`;
  **carrier generation 3** (`protocol_version = 3`, `checks.{jobs,tests}.v3.<region>`,
  `/v3/{jobs,tests}`) carries envelope `v: 2`. A capability-1 executor neither consumes nor
  claims generation 3; a capability-2 executor handles legacy v1, generation 2 and generation
  3. Jobs **and** tests, AMQP **and** pull — a generation introduced on one of the four paths
  and not the others reproduces exactly the asymmetry that caused the pull blackhole.
- **The carrier generation is TRUSTED metadata and must arrive out of band.** The mandatory
  entrypoint receives it as a separate argument stamped by the transport adapter — for AMQP,
  the queue the message was consumed from; for pull, the generation the server selected when
  it chose the row — and **never** reads it from `job.ProtocolVersion`, which is body content
  an attacker edits. Treating the carrier as normative while sourcing it from the untrusted
  payload would make the whole structural gate self-referential.
- A generation-2 executor opens a queued `v: 1` envelope with the r6 binding for as long as
  `v: 1` emission is permitted, so drain is not a flag day. Retiring a generation follows the
  dispatch-key rotation runbook's shape: stop emission, wait out max job TTL, purge or write
  off the DLQ, then drop support.

The AEAD **AAD** (generation 2) is a
**canonical length-prefixed binary encoding** of `(v, region, key_id, monitor_id,
execution_revision, field_name, job_id, field_set_digest, body_digest)` — unambiguous by
construction. This prevents **cross-context transplant** (between monitors, fields,
revisions, jobs) and, via the last two parts (r7), **within-context tampering** — swapping
the execution the credential is used for, or removing the credential from the job. Exact
replay of the same job payload to its own execution is expected transport behavior and is
fenced by revision/ingest, not by the AAD. Envelope size counts toward existing payload/body
bounds post-base64.

**Execution binding — `body_digest` (r7, closes an r1–r6 hole).** The identity parts above
bind a credential to *whose* it is and *when* it was issued, but not to *what the job asks it
to do*. Without this part, anyone able to WRITE to a v2 queue or a `pull_jobs` row keeps a
valid envelope, edits the job's target, and receives the plaintext credential at an endpoint
of their choosing — the executor's GCM check passes because nothing it verifies has changed.
A transport that is not trusted to READ payloads cannot simultaneously be trusted to write
them; r1–r6 treated the queue as untrusted for confidentiality only, and that asymmetry is
the defect.

Normative: `body_digest` is a SHA-256 over a **versioned canonical encoding of a dedicated
credential-execution DTO** — not over `domain.Monitor`, and not over raw payload bytes
(JSON round-trips are not byte-stable; a whole-struct hash also breaks on fields that carry
no execution meaning). Both sides compute it from the body they hold: core at seal time, the
executor from the body it received. **The computation is not caller-optional:** it happens
inside the single mandatory entrypoint described below, never as a step a call site is
expected to remember — a security check that each new caller must opt into is a check that
will eventually be skipped.

- **In the DTO** — the fields that decide where the credential goes, over what transport, and
  how many times it is transmitted: `type` (selects the prober); `target` (the remote
  endpoint); `timeout`; `retries` (each retry re-transmits the credential); the `conditions`
  **values** — a failed condition re-runs the whole probe when `retries > 0`, so editing them
  changes how many times the credential is sent (their ORDER is fixed in the encoding for
  determinism, not because reordering an all-must-pass set changes the retry count — it
  changes which failure is reported first, and overstating that was an r7-round-1 error);
  plus **every normalized non-secret execution setting of that
  type** as owned by the domain schema (§4.2) — e.g. `username`/`database`/`sslmode`/`query`
  for postgres, `mode`/`username`/`path`/`tls`/`tls_skip_verify` for rabbitmq management,
  where `mode` alone decides credentialed HTTP versus unauthenticated AMQP.
- **Not in the DTO, because already bound elsewhere:** `monitor_id`, `execution_revision`,
  `region` (direct AAD parts — no duplication); `protocol_version` (`Open` accepts one exact
  value); the ciphertexts (their bytes are what GCM covers).
- **Not in the DTO, because safe to change:** everything the executor never reads —
  `project_id`, `name`, `tags`, cadence/threshold inputs, `enabled`/`status`/state counters,
  renotify/grace/escalation/dependency/push fields, timestamps — and the `*_ref` NAME, which
  is materialization metadata: the executor reads only the injected value, and renaming a ref
  in an already-sealed job selects no different ciphertext and changes no remote behavior.

**Guarantee boundary, stated so it is not quietly widened later:** this binds *credential use
and the remote side effects of a credentialed probe*. It is deliberately **not** a general
signature of the job — the excluded state/display fields stay mutable in transit, and a
full-job integrity contract (protecting liveness/result semantics of the whole snapshot) is a
separate decision if it is ever wanted, never an unannounced extension of this clause.

**Canonical encoding — specified to the byte, because "canonical" is otherwise an intention.**
Two independent implementations (core sealer, executor verifier) must agree exactly, and
"canonical" without concrete values is an intention, not a format. It **reuses the existing
`secret.CanonicalAAD` framing** rather than inventing a second one: a uvarint part count
followed by uvarint-length-prefixed parts, which already makes `["ab","c"]` and `["a","bc"]`
distinct by construction. On top of that:

- **DTO version = 1**, emitted as the first part, as the decimal ASCII string `"1"` — numbered
  independently of the envelope `v` and of the carrier generation, so the three can move apart
  without aliasing.
- **Fixed field order**, listed in this spec's DTO bullets, never map iteration order.
- **Integers** (`timeout`, `retries`) as their decimal ASCII representation, no padding, no
  sign for non-negative values — the same textual discipline the rest of the AAD uses, which
  removes width and endianness as questions rather than answering them.
- **Strings** as raw UTF-8 bytes under the uvarint length prefix; never null-terminated,
  never padded.
- **`conditions`** as a count part followed by each condition's parts in array order.
- **Config keys** emitted in byte-wise **sorted** key order, each as a key part then a value
  part.
- **Absent and explicitly-set-to-default encode identically** — normalization runs before
  sealing, and a job may legitimately carry either form; this is the rule most likely to be
  broken by a well-meaning encoder.
- `body_digest` and `field_set_digest` enter the outer AAD as **raw 32-byte SHA-256 output**,
  not hex and not base64 — one encoding, chosen here, so the two sides cannot each be
  self-consistent and mutually incompatible.

§9 pins all of it with **golden vectors** — including shuffled map insertion order, nil versus
empty collections, implicit versus explicit defaults, and a condition reorder — because only a
fixed expected byte string catches an encoder that drifts. D-0160 carries the same values, so
the normative form survives independent of this document's prose.

**Growth guard — via a declarative registry, not a test that reads Go code.** The r7 round-1
wording ("derived from the domain schema, not a hand-kept allowlist") was not implementable
as written: `internal/domain` validates through imperative `allowKeys` calls, and no test can
discover which `m.Config` keys an arbitrary prober happens to read. What is required instead
is **one declarative per-type schema registry** that is the single source for all four
consumers — validation, normalization, the expected secret field set, and the non-secret
binding keys — so that adding a setting to the registry adds it to the digest by
construction. Top-level `domain.Monitor` fields, which are not registry entries, get a
**reflection-based guard**: a new field must be explicitly classified execution-affecting or
not, or the test fails (§9). A digest that silently lags one new setting reopens the hole it
exists to close, and would do so invisibly.

**Structural gate — the check must not live behind the branch it protects (r7).** The
fail-open defect is not primarily about which fields an envelope carries; it is about *who
decides whether an envelope is required at all*. Today the executor opens an envelope only
when one is present, and a job whose `credential_envelope` was **removed entirely** therefore
never reaches any credential code path: it is executed as an ordinary job, so a `redis`
monitor skips `AUTH`, PINGs, and a target that does not demand auth answers Up. Deleting one
JSON member turns an authenticated check into an anonymous one. No key and no forgery needed.
A rule placed inside `Open` cannot catch this, because `Open` is what gets skipped.

Normative:

- **One mandatory entrypoint.** Every executor path — AMQP jobs, AMQP test-RPC, pull jobs,
  pull tests — reaches the prober only through a single `Validate/Materialize` call that
  performs the structural decision itself. It is never the caller's job to notice that a
  credential was expected; a new call site that forgets the check must be impossible to write,
  not merely discouraged. The digest and field-set verification below happen INSIDE that
  entrypoint (see also "computation is not caller-optional" under the canonical encoding).
- **Carrier consistency.** A job delivered by a v2 carrier (v2 queue or v2 claim) must declare
  the exact expected `protocol_version` and carry an envelope; a job that arrives on a v2
  carrier without one is a typed failure, not a legacy job. The carrier is the executor's only
  authenticated signal about which contract applies, so it is treated as normative rather than
  advisory.
- **Tri-state credential requirement, resolved from the effective schema** (§4.2), never from
  what the payload happens to contain:
  - **required** — the type/mode takes a credential (`postgres`, `mysql`, `redis`, `rabbitmq`
    `mode: management`): the envelope must be present and carry **exactly** the expected
    non-empty field set;
  - **forbidden** — the type/mode takes none (`rabbitmq` `mode: amqp`, and every
    non-credentialed monitor type): an envelope present at all is a typed failure. Note this
    corrects an r7-round-1 wording that had `mode: amqp` "expect none" and succeed on an empty
    set — that path must reject the envelope, not open it vacuously;
  - **invalid** — any other combination (unknown type, missing mode, credential-required
    schema with an absent envelope, envelope on a forbidden schema) is a typed failure.
- Every failure above is `probe_error` (`decrypt_auth_failed` — the taxonomy stays
  non-oracular) raised **before any connection to the target**, never a heartbeat, never a
  status flip.

**Field-set binding — `field_set_digest` (r7, scope stated honestly).** With the tri-state
policy above, an exact expected set is known before authentication, so for **today's
schemas — all single-field — the policy is the operative protection** and the digest adds no
detection the policy does not already provide. It is specified now for one reason, stated
plainly rather than dressed up as defence-in-depth: the canonical sorted field-NAME set is
bound into every field's AAD so that a future multi-field envelope (a second credential slot,
a client certificate) cannot be *partially* truncated into a still-valid job without a second
security review. Verification order is fixed — structural gate, then field-set policy, then
AEAD — so behaviour is deterministic and testable rather than dependent on which check happens
to fire first. §9 covers it with a **synthetic multi-field vector**, since §4.2 defines no
real multi-field schema yet; that test is a contract for the primitive, not a claim about
current schemas.

**Exposure statement (honest).** A leaked region key opens **all retained
payloads of that region until TTL/DLQ purge**, not merely instantaneous in-flight traffic;
and a config/secret change committed after a job's authoritative read does not recall that
job — it remains in-flight exposure until ACK/TTL/DLQ purge, its result fenced at ingest
(§4.4.3). Both statements go into D-0155.

**At-rest AAD (same context-integrity principle — honest scope).**
`project_secrets.value_encrypted` is AEAD-bound with AAD = canonical `(project_id,
secret_id)` — stable identifiers, so rename stays metadata-only. A ciphertext row
transplanted between tenant rows fails authentication at decrypt: project A can never be
made to dispatch project B's **inventory** credential (cross-project swap test in §9).
Scope stated honestly: this guarantee covers the inventory; legacy **inline**
`monitors.config` ciphertext remains AAD-less as today (its *dispatch* is protected by the
envelope; binding it at rest is a recorded follow-up hardening decision/migration, not
silently claimed). `internal/secret` gains **byte-oriented AAD variants**
(`EncryptBytes`/`DecryptBytes(aad)`) used by the materializer so plaintext lives in
zeroable `[]byte` buffers, wiped best-effort after re-wrap — stated as best-effort:
Go gives no guaranteed zeroing, and the legacy string API is not used on this path.

**Wire barrier (verifiable, not presumed).** An old executor cannot emit `probe_error` and
must never receive an envelope it would misread as a credential-less job:

- **AMQP (jobs AND tests):** envelope-bearing payloads are published only to **versioned
  queues** — `checks.jobs.v2.<region>` and `checks.tests.v2.<region>` (the test-RPC gets a
  v2 reply contract alongside) — consumed only by envelope-capable workers (which consume
  v1 and v2 during the transition). Credentialed payloads are NEVER published to a v1
  queue. Region health/alerting for credentialed monitors counts **v2-queue consumers**
  specifically (separate consumer metrics + TTL-expiry alert tests per v2 queue) — a
  v1-only consumer is not evidence of readiness. No v2 consumer → v2
  payloads TTL-expire → the region credential-capability alert fires; no unauthenticated
  probe, no false DOWN.
- **Pull (claim-level barrier, not heartbeat-only):** `pull_jobs`/`pull_tests` rows carry a
  `protocol_version`, and the claim endpoints are **versioned**: the existing endpoint
  serves only v1 rows; a new v2 claim endpoint serves v2 rows to clients that declare the
  capability on every claim. An old agent — even one whose heartbeat is stale but which
  keeps polling with the shared region token — can never receive a v2 payload from the v1
  endpoint. Agents additionally advertise `capabilities: {credential_envelope: 1}` +
  `credential_ready` in their heartbeat; core dispatches credentialed monitors into a
  region only when **at least one live `credential_ready` v2 agent** exists (an existential
  check — never vacuously true on an empty set). Same contract for pull tests. An
  envelope-capable agent that receives an unsupported FUTURE version returns typed
  `probe_error` — the only direction that test is possible in.
- **The barrier is ONE-directional (r7, closes an r1–r6 hole).** It exists to stop an
  INCAPABLE executor from receiving v2; it must never stop a CAPABLE one from receiving v1.
  A capable agent therefore claims **both classes** for the whole transition — the AMQP half
  of this rule was already stated above ("consumed only by envelope-capable workers, which
  consume v1 and v2 during the transition"), and its pull mirror was missing, which is
  precisely how the defect entered.
  **Mechanism, specified — "polls both" is not enough (r7 round 1).** Two sequential
  long-polls reproduce the same blackhole with a delay: the endpoint holds an empty request
  for the long-poll window, so an agent that polls the empty class first sleeps through
  already-waiting rows of the other class whenever no fresh `NOTIFY` arrives — and two
  independent polls each claiming up to `max` can lease more work than the agent finishes
  before the lease expires. Normative: a capable agent claims through **one capability-scoped
  claim that leases, in a single atomic operation, every generation at or below its declared
  capability**, under one shared `max` and one lease, with the **generation stamped by the
  server** on each returned row so the agent never infers it from the row's own content.
  Fairness must not starve any generation. The count is not two: during the generation-3
  transition there are **three** classes — legacy v1, generation 2 (envelope `v: 1`) and
  generation 3 (envelope `v: 2`) — and a capability-1 agent must never be handed a
  generation-3 row, which is why the lease is bounded by declared capability rather than by
  "whatever is available". "The v2/v3 endpoints serve their rows only to capable clients" and
  "a capable client's claim also returns older-generation rows" are therefore distinct
  statements, and only the first is a restriction. The same
  applies to **`pull_tests`**, where the agent likewise selects a single endpoint today — a
  jobs-only fix leaves test-connection broken the same way for ordinary monitors.
  The asymmetry matters because **non-credentialed
  monitors are enqueued as v1 rows**: an agent that polls only the v2 endpoint claims none of
  them, and since a region under `enforced` requires its agent to hold a dispatch keyring,
  "capable" is the only kind of agent such a region has. The whole region then goes dark in
  the worst possible way — rows expire by TTL, no probe runs, so there is no heartbeat, no
  DOWN, and no alert: monitoring stops without saying so. Enabling a security feature must
  never silently disable monitoring; a region that cannot execute credentialed jobs degrades
  to the §4.4.4 operational rejection for THOSE monitors only, and never affects the rest.
  Dual-claim must not weaken the other direction: capability is still re-asserted per claim,
  and the v2 endpoint still serves v2 rows exclusively to declared-capable clients.
- `secrets.enabled` requires the barrier to be effective (config `secrets.dispatch_envelope:
  enforced` + runtime refusal to dispatch credentialed monitors into a region without
  capable executors — surfacing as the §4.4.4 operational rejection, not DOWN).
- Legacy plaintext emission for credentialed/ref monitors never happens in any phase.
  Inline-value monitors move onto the envelope in `enforced` mode (fixing their existing
  plaintext-in-broker exposure); until then they keep today's legacy path.

**Typed executor outcome.** New result-protocol member with a defined wire schema:
`probe_error {monitor_id, execution_revision, reason ∈ no_dispatch_key | unknown_key_id |
decrypt_auth_failed | unsupported_version, job/claim id where the transport has one}`
(a found key_id with failed GCM auth is indistinguishable from a corrupt envelope, so the
reasons never pretend to distinguish them — no diagnostic oracle; persistent
`decrypt_auth_failed` degrades readiness like a key mismatch),
authenticated per transport exactly like heartbeat results (region-scoped), stale-revision-
checked at ingest like any result. Semantics: scheduled — no heartbeat inserted, no status
flip; recorded on the monitor's **probe diagnostics** (new nullable `last_probe_error
{reason, at}` on the monitor scheduling state, readable through the tenant-scoped monitor
detail API, **never** on public status pages). **Clear semantics:** the diagnostic clears
on any revision-valid, LIVE-applied normal heartbeat — Up OR Down — whose server
`received_at >= error.at`; an out-of-order/SLA-only/stale-revision result never clears a
newer executor error; write and clear are atomic with the ingest gate. Plus low-cardinality
`cerbix_executor_probe_error_total{reason}`. The job is acknowledged — this is a diagnostic
outcome, not a retry mechanism (AMQP acks pre-execution today). Test paths — bounded `502`
with the reason. Executors export the same metric and degrade readiness on persistent key
mismatch; via `credential_ready` core distinguishes "live but credential-degraded" agents
from healthy ones (feeds the region credential-capability alert).

### 4.8 Target-transport policy (honest egress)

The final hop to the monitored target follows: **encrypted by default for ref-based
monitors, with server-identity verification stated per option** — postgres `require` =
encrypted-not-verified (verified = `verify-ca`/`verify-full`); mysql/redis `tls: true` =
verified per Go TLS defaults (system roots + hostname), `tls_skip_verify` is an explicit
visible opt-in (never silent); rabbitmq management https. Unencrypted transport is an
explicit opt-in in the monitor definition (reviewable, audited). Existing non-ref monitors
keep today's behavior.

## 5. API

Standard API auth (session cookie, Cerbix API token, OIDC client-credentials), project
authz. RBAC (explicit): `ProjectWrite` includes Editor — matching their power over the
monitors; secret **names** are metadata under `ProjectRead`; values readable by no role.

- `GET /projects/{id}/secrets` → `[{id, name, created_at, rotated_at, used_by: {total,
  file_managed}}]` — `ProjectRead`; `Cache-Control: no-store`.
- `POST /projects/{id}/secrets` `{name, value}` → 201 (never echoed) — `ProjectWrite`;
  `409` duplicate; `400` empty; `409` quota.
- `PATCH /projects/{id}/secrets/{name}` `{name?, value?}` (≥1) → 204 — `ProjectWrite`;
  `409 secret_renamed_in_use`; `409` duplicate.
- `DELETE /projects/{id}/secrets/{name}` → 204 | `409 secret_in_use` — `ProjectWrite`.
- Feature off → `404 feature_disabled`.

Hygiene: value ≤ 4 KiB UTF-8 bytes; body caps; `no-store`; errors/audit/traces/metrics
never contain values or ciphertext. Audit: `secret.create`, `secret.update`
(renamed/rotated + re-point count), `secret.delete`.

## 6. Store & reencrypt

`internal/store/secrets.go` — **every method takes and predicates `project_id`** (a
missing project filter is a security defect per the repo's tenant invariant; there is NO
global resolve-by-id path): `CreateProjectSecret(ctx, projectID, …)`,
`UpdateProjectSecret(ctx, projectID, …)` (§4.3 locks, rename guard, re-point, §4.5 fence —
one tx), `DeleteProjectSecret(ctx, projectID, name)`, `ListProjectSecrets(ctx, projectID)`.
The materializer resolves secrets through `monitors JOIN monitor_secret_refs JOIN
project_secrets` with **equality on all `project_id` columns** — the tenant scope is
carried by the join, never by a bare secret id. A no-decrypt snapshot/DTO read path
(§4.4.2) and the authoritative batch materializer read (§4.4.3/4).
**Delete guard mapping (exact count under lock):** `DeleteProjectSecret` locks the secret
row `FOR UPDATE`, counts referencing rows **under that lock** (exact by construction), and
returns the typed `409 secret_in_use` with that count without attempting the delete; the
deferred FK remains the final commit-time guard for any path that bypasses the guard, and
a commit-time violation still maps to the typed 409 (count then reported as current, not
exact).
Migration `000NN_project_secrets.sql` + `monitor_secret_refs`.

**Reencrypt:** covers `project_secrets`; CAS on the exact old ciphertext; **bounded
convergence** (attempt/deadline budget): rescan-and-retry to a fixed point, and success is
claimed only when a final scan proves **zero rows under the old key** — otherwise exit
non-zero with the skipped count. Same CAS applied to the existing by-id sweeps
(rotate-vs-reencrypt race test). Runbook: previous at-rest key retained until reencrypt
proves convergence; dispatch-key rotation is a separate runbook (§4.7).

## 7. Component changes

- `internal/domain`: typed settings validator (conditional rabbitmq, exactly-one-of,
  bounds, TLS policy fields); consumed by `Monitor.Validate`.
- `internal/fileprovider`: YAML strictness; semantic validation delegated; apply-tx
  existence under §4.3 locks; bindable `secret_ref_not_found`.
- Scheduler/api: ciphertext-only snapshot; authoritative-read materializer on all six
  paths; v2-queue/capability gating.
- Dispatch/worker/agent: v2 queue consumption; envelope DTO decode + JIT decrypt by
  `key_id`; AAD verification; typed `probe_error`; heartbeat capabilities +
  `credential_ready`; readiness wiring.
- Probers: redis/mysql TLS (+ explicit skip-verify), rabbitmq management https default.
- Ingest/result protocol: `probe_error` member (auth, stale-revision check, diagnostics
  write, no heartbeat).

## 8. Frontend

Per the approved mock: Secrets panel (list with used-by, Add, Edit with rename-guard,
Delete with guard error; write-only values); monitor form exactly-one-of "Value | Secret
reference" with inventory dropdown; file-managed detail shows the `password_ref` chip;
missing-ref shows the frozen-bundle diagnostic; monitor detail surfaces `last_probe_error`.

## 9. Verification

- Store (both storage modes): CRUD; feature-off/no-cipher matrix; empty value; delete FK →
  409 under concurrent apply; rename-vs-apply interleaving (locks); rotate bumps exactly
  the referencing monitors via the D-0142 watermark path + audit/NOTIFY-only-on-change;
  quota race; project-delete cascade on real PG (deferred FK); reencrypt bounded
  convergence + zero-old-key proof + rotate-vs-reencrypt race; tenant isolation.
- Domain: per-type matrix (conditional rabbitmq, exactly-one-of, bounds, TLS defaults +
  explicit skip-verify only); bundle inline `password` forbidden; hash covers ref name.
- Scheduler/materializer: **snapshot plaintext-absence for ref AND inline monitors**;
  authoritative-read regressions for **rotate between cadence decision and materialization**
  AND **generic UpdateMonitor(target) between decision and materialization** — the job must
  carry the new config with its own revision, never old-config/new-revision;
  **disable-between-decision-and-read** → skipped, no job, no heartbeat/state mutation
  (ingest additionally refuses results for disabled monitors); **change-committed-AFTER-
  read** → the old-revision job still dispatches but its result is rejected at ingest and
  no state mutates (the linearization-point boundary, tested from both sides);
  **region-move-between-decision-and-read** → the credential is never encrypted under the
  old region's key nor published to the old region's queue (routing regrouped by the
  authoritative row); cadence/nextRun advances from authoritative interval inputs;
  materialization failure → rejection + metric, no cadence advance as "sent", persistent
  failure honors the backoff floor (no per-tick hammer); **publish/enqueue failure (r7)** —
  with the broker refusing every publish, the due credentialed set is attempted **at most once
  per backoff window** (window fixed by the test), not once per tick — asserted through the
  monitor's next-eligible state and failure counter rather than a wall-clock rate: failure
  increments the counter, sets `now + backoff` and does NOT mark sent; **success resets the
  counter, marks sent and restores ordinary cadence** (asserted explicitly — a monitor that
  publishes successfully must not be re-dispatched on the next tick); separate metric reason
  from secret-resolution failure; the non-credentialed publish path asserted unchanged.
- Dispatch/executor: payload plaintext-absence (pull_jobs, AMQP body, DLQ body); envelope
  round-trip; **AAD transplant tests** (cross-monitor, cross-field, cross-revision,
  cross-job → decrypt fails → probe_error; exact same-job replay decrypts and is fenced by
  revision at ingest); **body-tamper matrix (r7)** — a job whose envelope and identity parts
  are untouched but whose execution body is edited fails to open, one case per DTO member:
  `target`, `type`, `timeout`, `retries`, `conditions` (including reorder-only), and at least
  one normalized non-secret setting per credentialed type (`sslmode`, `mode`,
  `tls_skip_verify`, `query`); mirrored by a **negative control** — editing an EXCLUDED field
  (`name`, `tags`, cadence/threshold inputs, the `*_ref` name) still opens, proving the
  boundary is the stated one and not an accidental whole-job signature; **DTO coverage guard
  (r7)** — a test that fails when `domain.Monitor` grows a field, or a per-type schema grows a
  setting, that is not explicitly classified execution-affecting or not, so the digest cannot
  silently lag a new prober option; **canonical-encoding golden vectors (r7)** — fixed
  expected byte strings covering shuffled map insertion order, nil vs empty collections,
  implicit vs explicit normalized defaults, and a condition reorder, so an encoder that drifts
  is caught rather than merely disagreeing with itself; **structural gate (r7)** — the
  regression the field-set rule alone does NOT cover: from a valid credentialed job on a v2
  carrier, remove the `credential_envelope` **entirely** → `probe_error` **before any target
  dial**, asserted separately for AMQP jobs, AMQP test-RPC, pull jobs and pull tests (the
  fail-open case, pinned against a live auth-less redis target that would otherwise answer
  PING and report Up); envelope present on a **forbidden** schema (`rabbitmq` `mode: amqp`,
  any non-credentialed type) → typed failure, never a vacuous empty-set open; credential-
  required schema with no envelope → typed failure; **field-set (r7)** — exact expected set
  enforced from type/mode and never from the payload: missing field, unknown extra field, and
  a credential whose ciphertext decrypts to a **zero-length value** each → `probe_error` before
  dial; **synthetic multi-field vector (r7)** — with a test-only two-field expected set,
  removing one field fails via `field_set_digest` authentication, fixing the primitive's
  contract ahead of any real multi-field schema; verification ORDER asserted (structural gate →
  field-set policy → AEAD) so failures are deterministic;
  **carrier/envelope generation rollout (r7)** — asserted as **physical isolation, not only as
  a capability predicate** (the round-1 matrix checked the predicate alone, which is what a
  capability-1 consumer sitting on a shared queue defeats): a capability-1 executor never
  consumes or claims a generation-3 carrier on any of the four paths (AMQP jobs, AMQP tests,
  pull jobs, pull tests); a capability-2 executor opens a queued generation-2 (`v: 1`) envelope
  under the r6 binding; core emits generation 3 into a region only when the existential
  readiness check passes **for capability 2**, and a `credential_envelope: 1` executor does not
  satisfy it; the entrypoint takes the carrier generation from the transport adapter and a job
  whose body claims a different `protocol_version` than its carrier is rejected (proving the
  generation is not read from attacker-controlled content);
  **cross-project at-rest ciphertext swap** between `project_secrets`
  rows → decrypt/auth failure, no dispatch; **wire barrier**: v1-only worker never receives
  envelope jobs OR tests (v2 jobs+tests queue isolation; TTL-expiry feeds the
  credential-capability alert; v1 consumer present + v2 absent → alert fires); **stale
  old pull agent that keeps polling** the v1 claim endpoint never receives a v2 payload;
  **one-directional barrier (r7)** — a CAPABLE agent, sole agent of an `enforced` region,
  facing a mixed due set claims **both** the v1 rows (ordinary monitors) and the v2 rows
  (credentialed ones); the regression asserted the way the defect showed up in production
  terms, i.e. no v1 row is left to expire by TTL and every ordinary monitor still produces a
  heartbeat once the feature is enabled — a queue-isolation assertion alone does NOT cover
  this and is what let it through; capability still re-asserted per claim in the dual-claim
  path, and v2 rows still never reachable from the v1 endpoint; **dual-claim mechanics (r7)** —
  rows of EVERY claimable generation already present **before** the agent's wait begins and
  **no new `NOTIFY`** arriving (the sequential-long-poll trap: the agent must not sleep out the
  window on an empty first class); no starvation of any generation across repeated claims; one
  shared `max` and one lease across all of them (independent per-class claims must not
  over-lease beyond what the agent completes before expiry); server-stamped generation on every
  returned row; **the lease bounded by declared capability** — with legacy v1, generation-2 and
  generation-3 rows all waiting, a capability-1 agent receives the first two and never the
  third, while a capability-2 agent receives all three; and the whole set mirrored for
  **`pull_tests`**, not jobs only;
  v2 claim endpoint re-asserts capability per claim; pull gating requires ≥1 live
  `credential_ready` v2 agent (never vacuous); capable-agent + future-version envelope →
  probe_error; dispatch-keyring rotation (primary/previous) with a queued old-key job;
  multi-region `default` key rejected by config (allowed only with
  `shared_trust_acknowledged`, which flips the exposure status); wrong-key agent stays
  live but `credential_ready=false` visible to core; probe_error clear matrix
  (revision-valid live Up/Down clears; stale/SLA-only does not).
- API: authz matrix, never-echo, no-store, caps, audit names-only, feature-off 404.
- E2E (live stack): create secret → postgres bundle → green → rotate (in-flight stale
  rejected; no reconcile change) → rename blocked (file) / re-pointed (UI) → delete → 409;
  agent region-key smoke incl. degraded `credential_ready`.

## 10. Deliverables (process)

Spec re-pass → `feat/secret-inventory`: (1) config (feature switch + dispatch keyrings) +
migration + store + domain validator + API + Secrets panel; (2) ref-table wiring +
ciphertext snapshot + authoritative materializer + envelope/AAD + v2 queue + agent
capabilities + probe_error protocol; (3) rotation fencing + reencrypt hardening + prober
TLS + UI monitor-form refs + E2E. `-race` both storage modes throughout; **D-0155** records
the two-keyring model, AAD contract, wire barrier, trust + exposure statements, rollout;
iteration reports; status (`FR-020`, `NFR-015`); traceability; func-monitoring-as-code §3.1
cross-reference update. Slice 1 closed in iter-0115; slices 2–3 and acceptance closed
together in iter-0116, with the per-contract evidence table in that report.

**Slice 4 — r7 amendment (open).** In this order, because two of the blockers are design
defects and §4.7 is the contract the code is written against: (1) this spec + **D-0160**
recording the execution-binding contract, the structural gate and tri-state credential
requirement, the carrier/envelope generation mapping, the one-directional barrier, the
narrowed threat scope of NFR-015 **and the owner's A/B decision from §2**, and the
withdrawn-then-restored acceptance; (2) the **one-directional pull claim** with the single
atomic capability-bounded lease (jobs **and** tests) — the only defect that breaks monitoring
outright, the one gating any pull-region rollout, and dependent on nothing else here;
(3) the **declarative per-type schema registry**, which both of the next two steps build on —
r7 round 2 correctly caught an earlier ordering that put the structural gate first and the
registry after it, which would have meant duplicating type/mode policy and breaking the
single-owner rule the registry exists to establish; (4) the **structural gate** — single
mandatory `Validate/Materialize` entrypoint, out-of-band carrier generation, tri-state
requirement; (5) `body_digest` + `field_set_digest` on **carrier generation 3 / envelope
`v: 2`** with capability `credential_envelope: 2`, golden vectors and the §9 matrices;
(6) the §4.4.5 publish-failure floor. Steps (4)–(5) are settled by the owner's choice of
option A (D-0160); had B been chosen they would have been re-planned around a root `job_auth`
tag instead.

Each lands with its regression **verified to
fail when the fix is reverted** — the three defects here all passed a green suite, so "the
tests are green" is not evidence for this slice. `-race` in both storage modes throughout;
a new iteration report re-establishes acceptance, and `status.md`/`traceability.md` move only
with it. §9 gets a repeatable E2E gate rather than the standalone smoke script the audit found — an
unrunnable check is not a check. It ships as its own Make target (`make secret-smoke`) and
NOT inside `make dev-test`: D-0158 fixes that an E2E goal never starts, stops or redirects a
stack, and this smoke owns its whole world (it builds the binary and provisions its own
throwaway database). The requirement is that it be runnable on a clean checkout, which is
what it now is.
