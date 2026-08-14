# func-secret-inventory — Project secret inventory & `*_ref` for the file provider (FR-020)

Status: **APPROVED (design r6 + linearization-point wording, 2026-08-14).** UI mock approved
by the owner; six independent design passes (r1–r6) by the `cerbix-reviewer` agent; the r6
verdict is APPROVED with one mandatory wording fixation (applied below, §4.4.3/§4.7/§9):
the authoritative read is the dispatch-authorization **linearization point**. This approves
the SPEC, not the future implementation — code is accepted only against the §9 test matrix
plus D-0155/traceability/runbook/iteration evidence.

Implementation acceptance: **DONE in iter-0116**. This does not retroactively conflate the
design approval with implementation approval; the latter is evidenced in
[`iter-0116`](../iterations/iter-0116.md), `status.md` and `traceability.md` against §9.

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
  plaintext fallback, enforced at config load (§4.1).

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
never passing domain/API redaction surfaces): `credential_envelope: {v: 1, region, key_id,
job_id, fields: {password: <AES-256-GCM ciphertext>}}`. `job_id` is **always
core-generated before encryption** (no transport-dependent context). The AEAD **AAD** is a
**canonical length-prefixed binary encoding** of `(v, region, key_id, monitor_id,
execution_revision, field_name, job_id)` — unambiguous by construction. This prevents
**cross-context transplant** (between monitors, fields, revisions, jobs); exact replay of
the same job payload to its own execution is expected transport behavior and is fenced by
revision/ingest, not by the AAD. Envelope size counts toward existing payload/body bounds
post-base64. Exposure statement (honest): a leaked region key opens **all retained
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
  failure honors the backoff floor (no per-tick hammer).
- Dispatch/executor: payload plaintext-absence (pull_jobs, AMQP body, DLQ body); envelope
  round-trip; **AAD transplant tests** (cross-monitor, cross-field, cross-revision,
  cross-job → decrypt fails → probe_error; exact same-job replay decrypts and is fenced by
  revision at ingest); **cross-project at-rest ciphertext swap** between `project_secrets`
  rows → decrypt/auth failure, no dispatch; **wire barrier**: v1-only worker never receives
  envelope jobs OR tests (v2 jobs+tests queue isolation; TTL-expiry feeds the
  credential-capability alert; v1 consumer present + v2 absent → alert fires); **stale
  old pull agent that keeps polling** the v1 claim endpoint never receives a v2 payload;
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
