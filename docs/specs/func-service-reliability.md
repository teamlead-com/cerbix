# func-service-reliability — Service as a reliability-domain resource (FR-021 / NFR-016)

Status: **APPROVED (design r8, 2026-08-15)** — independent adversarial design review signed off;
this approves the DESIGN, not an implementation, and phase-1 work starts on its own branch with
D-0159. Seven adversarial passes: r1 returned nine P0s,
r2 seven more, r3 seven again — all seven of r3's being defects r3 itself introduced while
closing r2's — and r4 three, all local contract defects, with the central design and every r3
fix accepted. r5 closes those three plus one lock-analysis correction: the cadence bounds now
derive from **one** mechanism instead of three mutually exclusive numbers, a retroactive
maintenance mutation carries a **persisted preview token with a confirm-time CAS**, the leftover
bucket-boundary phrasing on maintenance cancellation is gone, and the claim that routing rows
form no lock class is replaced by the implicit-RI analysis it should have been. A targeted check
of those fixes then found three defects inside them, closed here: a slice deadline is enforced
**caller-side over the whole slice** because `statement_timeout` bounds each statement and not a
transaction; the preview CAS covers a **complete relation** of affected services rather than a
truncatable array, and an approximate preview cannot be confirmed; and the global lock-order
assertion is scoped to FR-021's own paths, with the pre-existing `UpdateMonitor` disagreement
named as FR-012 backlog instead of asserted away. A further check of the new CAS found that it
compared the rows in the affected set but not the SET, so a service created — or newly
referencing the monitor — between preview and confirm could be mutated unpreviewed; r7 re-resolved
the set inside the mutating transaction and required exact equality, and r8 closes the phantom
that remained — row locks cannot protect a row that does not exist yet, so the affected set is
serialized by a named project `service_membership` advisory lock that confirm and every
set-changing path both take. Every round
confirmed the central model — `monitors[]` as operational context, `sli[]` as a separately
declared meaning of availability, the revision as part of the FACT, a purpose-built aligned
store — and every round found the machinery around it not implementable as written. r4 closes
the round-3 findings: the epoch now resolves the declaration **at its own effective instant**,
the ignore policy has a variable in the algebra, the fact carries **both** result axes as
durations, the ingest handshake resolves membership **as of the heartbeat's bucket** and fires
only on a real insertion, work partitions are bucket-aligned, and an archived maintenance window
keeps its effective span. The owner decisions stand unchanged: duration-weighted facts in
microseconds, archive separated from annul, bundle format 2 with a project-unique monitor slug,
and native RANGE partitions in both storage modes.

Phases 1–2 are specified to implementable depth; phases 3–5 are intent only. No code exists
and none will until this text passes.

> **Terminology.** A Cerbix **Service** is a reliability-domain resource representing an
> operational unit. It is not a monitor, not a status-page component and not a catalog entry.
> A **definition revision** is what a human declared availability to MEAN. An **evaluation
> epoch** is what the system was MEASURING. A **canonical bucket** is the fixed-width UTC
> interval every fact is keyed by.

## 1. Domain intent

Cerbix already measures (checks), scores (SLO/SLI, error budgets), records failure
(incidents), routes response (escalation/on-call) and publishes state (status pages). What it
lacks is the object those five things are *about*: the operational unit whose reliability an
on-call engineer actually reasons about.

Today that concept is **already present, expressed four incompatible ways**:

| Primitive | What it expresses | Why it is not the missing object |
|---|---|---|
| `composite` monitor | health rollup over children (`all`/`any`) | it IS a monitor: has its own SLO, its own incident, and must run in `core` |
| monitor `depends_on` + `DownAncestors` | dependency graph | monitor-level, and used only for delivery-time alert suppression (fail-open) |
| status-page `components` | user-facing grouping | scoped to ONE status page (`status_page_id`), one component = one monitor; duplicated per page |
| monitor `tags` | ad-hoc grouping | free labels, no semantics, no ownership, no budget |

None of them owns *ownership*, *criticality*, or an *aggregate error budget*. The purpose of
this specification is therefore **normalization of an existing domain concept**, not the
addition of a product feature: give the four primitives non-overlapping responsibilities and
introduce the single object they have been approximating.

The strongest structural evidence that this fits the current model: `sla_targets` already
carries a scope axis —

```sql
monitor_id uuid NULL,
project_id uuid NULL,
CHECK ((monitor_id IS NULL) <> (project_id IS NULL))
```

— and the project level was always groundwork. Monitor-level SLO is too local (a service has
8 checks and no single availability number); project-level is too broad (one error budget for
all of Payments is meaningless). `service_id` is a third value in an existing pattern, at the
granularity operators actually use.

**Central thesis:** a Service is the place where it is *explicitly declared what reliability
means for this operational unit*. Everything else in this document follows from that.

## 2. Requirement

- **FR-021**: A project member can define Services inside a project: an operational unit with
  an owner (routing), a monitor membership (operational context), an **explicit SLI
  selection** (reliability inputs), an aggregation policy, and SLO targets. Cerbix
  materializes a per-service reliability timeline from monitor observations and reports
  service availability, error budget and burn rate over the supported windows.
- **NFR-016**: Reported service reliability is **explainable and reproducible over its
  declared inputs**. Every stored reliability fact references the evaluation epoch that
  produced it, which resolves to exactly one definition revision; identical observations,
  epoch snapshot and declared exclusions reproduce identical output; a change to a declared
  input is a **recorded** change carrying an audited repair of the affected range, never a
  silent rewrite and never a silent no-op; a window is never reported unless it passes both
  coverage axes of §11.2; and the absence of reliability inputs is never rendered as perfect
  reliability.

r2 stated reproducibility as an absolute. It cannot be: maintenance windows are declarations
about past time, and the product lets an operator create and remove them. The honest invariant
is reproducibility over *declared* inputs, with every change to a declared input carrying its
own repair, invalidation and audit trail (§10.9).

## 3. Scope, non-goals, and the time-series tripwire

**In scope (phases 1–2):** the Service resource; monitor membership; SLI definition with
immutable revisions; the evaluation-semantics projection and epochs; the effective-state model
and the piecewise bucket reducer; aggregation policies (region-aware and quorum); the
duration-weighted reliability fact store with sealing; the seal/ingest handshake;
leader-owned materialization, recompute and backfill over durable ranges; service SLO / error
budget / burn-rate reporting; two coverage axes; two-layer health presentation; bundle
format 2 and the monitor slug it needs; optional adoption.

**Explicit non-goals.** Cerbix materializes **exactly one derived reliability timeline per
Service — one fact per canonical bucket, tagged with the evaluation epoch in force** (§6),
never a series per revision — retains only what the supported SLO windows require, and computes
a fixed set of reliability aggregates. Cerbix does **not**: serve arbitrary time-series
queries; store generic telemetry; support user-defined downsampling; expose a query language;
act as a metrics backend; provide a service catalog (repository links, documentation, tech
stack, deployment metadata, generic scorecards belong to an external catalog).

**Tripwire.** The moment this feature requires arbitrary series, configurable downsampling or
a query language, it has left its scope — that is a signal to stop, not to add a sprint.

**Storage-engine decision (owner).** `service_reliability_buckets` uses **native UTC RANGE
partitions in BOTH storage modes**, with identical key, uniqueness, seal, recompute and
retention semantics, and **no** TimescaleDB hypertable or compression in phase 1. The reason
is specific to this table rather than a general preference: by design it rewrites SEALED rows
weeks old under an audited recompute, which is exactly the access pattern compressed chunks
serve badly. One code path and retention by partition drop are worth more here than reusing
the heartbeat machinery. Revisit after phase 2 with measured workload. Recorded in D-0159.

**Field admission rule.** A field may live on Service only if it affects (a) the reliability
computation, (b) the routing of operational response, or (c) the interpretation of health.
Anything else is catalog metadata and is rejected.

## 4. Identity, scope, and boundaries

```
Organization   → tenant / isolation boundary            (unchanged)
Project        → authorization / configuration boundary (unchanged)
Service        → reliability / ownership boundary       (new)
Monitors       → measurement                            (unchanged)
```

A Service belongs to exactly one project. Every table introduced by this feature carries
`project_id` and participates in the tenant-safe composite-FK scheme of §16 — including the
ones r2 forgot: epochs, durable ranges, cursors, late-arrival records and owner references.

**Service is NOT a security boundary.** There is no `ActionServiceRead`/`ActionServiceWrite`;
authorization stays at the project level. Introducing a fourth authz level would duplicate the
tenant-isolation surface across filtering, API authorization, background jobs, MaC
reconciliation, incident visibility and status pages for no isolation gain.

`owner` is **not a free-text team label** (that would fail the field-admission rule); it is a
reference to existing operational routing primitives — an escalation policy and/or an on-call
schedule — so that "who is responsible" is actionable rather than decorative.

`tier` (criticality) is admitted **only if it carries behavior** — e.g. it selects default
burn-rate rules, escalation urgency, or status-page prominence. If phase 1 gives it no
behavior, phase 1 does not have the field.

## 5. Membership vs SLI — the load-bearing distinction

```
monitors[]  → operational context: what is shown on the service, what diagnostics exist
sli[]       → reliability inputs: what actually counts toward availability and budget
INVARIANT:  sli ⊆ monitors, and the two are declared INDEPENDENTLY
```

The invariant is not `monitors[] ≠ sli[]`: a service whose every diagnostic genuinely counts
toward availability is legitimate, and forbidding equality outlaws it for no benefit. What
must hold is that the two are **separately declared** — so editing `monitors[]` never
implicitly edits `sli[]` — and that every reliability input is visible in the operational
context it is reported beside. An SLI member outside `monitors[]` would be a number nobody
can see the source of.

Example:

```yaml
monitors: [checkout-http, checkout-db, checkout-redis, checkout-grpc, checkout-synthetic, checkout-queue]
sli:      [checkout-http, checkout-synthetic]
```

Rationale: without this split, the ordinary engineering action "let me add a Redis check for
diagnostics" silently redefines what availability of checkout means. Silent semantic drift of
a reported number is exactly what the project's existing invariants forbid.

An SLI member may be any monitor in the same project, including a `composite` (nested
aggregation is well defined; §9). A monitor may participate in the SLI of **several**
services — that is one measurement source feeding two independent reliability definitions,
not double counting.

A Service with an **empty `sli[]` is valid**: it has operational context and no SLO. It
reports SLO/budget as *unavailable* — **never as 100%**.

## 6. Two axes: definition revision and evaluation epoch

One revision stream cannot work. It would have to carry two different kinds of change — what
a human declares availability to MEAN, and what the system happened to be MEASURING — and
those have different owners, different authority rules and different reporting semantics.

### 6.1 `definition_revision` — the declaration

```
DefinitionRevision
├── revision            monotonic per service, assigned under the service lock
├── created_at          statement_timestamp() of the writing transaction
├── effective_at        §6.4
├── state               effective | superseded_before_effect        (§6.5)
├── monitors[]          operational context
├── members[]           monitor refs that form the SLI
├── aggregation_policy
├── region_policy
├── missing_data_policy
├── maintenance_policy
├── freshness_policy    how long a member's last state is held
└── created_by / audit
```

This is the Service's declaration. Only a subject holding authority over the Service changes
it — the UI for a UI-owned service, the bundle for a file-owned one — and it is what the
ownership matrix in §15.1 protects.

### 6.2 `evaluation_epoch` — the observed execution semantics

```
EvaluationEpoch
├── epoch_id
├── epoch_seq             monotonic per service, assigned under the service lock
├── definition_revision   the declaration in force
├── created_at            statement_timestamp() of the writing transaction
├── effective_at          §6.4
├── state                 effective | superseded_before_effect      (§6.5)
├── members_snapshot[]    per member: the evaluation-semantics projection of §6.3,
│                         plus its resolved stale_deadline under the freshness policy
└── snapshot_hash         canonical hash of members_snapshot
```

An epoch is a **system-authored immutable projection of the input properties the evaluator
reads**. It is not a declaration and it never changes one.

- **A fact references the epoch only.** The epoch resolves to exactly one definition
  revision, so no compound key is needed on the fact.
- **Every definition-revision transaction creates a matching epoch**, unconditionally. r2
  triggered epochs only from monitor execution writes, which left a new revision with no epoch
  a fact could reference — an unsatisfiable foreign key, not a latency problem. The
  snapshot-hash no-op rule below applies **only** to epochs driven by a monitor execution
  write.
- **Execution-driven epochs are created eagerly, in the same database transaction as the
  monitor write**, for every service referencing that monitor, under the lock order of §15.4,
  all stamped with one `created_at`. A lazy "snapshot on next evaluation" was considered and
  rejected: it moves the linearization point into the first evaluator run, leaving an interval
  in which concurrent ingest or backfill lands in the wrong epoch, and makes the boundary
  depend on when a crashed process recovers.
- **An execution-driven epoch resolves the declaration in force at its OWN `effective_at`,
  not the one active when it was written.** Under the service lock it selects the winning
  effective definition revision *at that boundary*, **including a same-boundary revision that
  is pending and not yet in effect**. Without this the two axes silently diverge: a service
  edit at `12:00:10` makes `rev2` effective at `12:01`, a monitor edit at `12:00:40` creates an
  epoch on the same boundary and supersedes `rev2`'s epoch, and if that epoch resolved "the
  revision active right now" it would point at `rev1` — leaving `12:01` governed by `rev2` with
  its only effective epoch resolving to `rev1`. Invariant 7 would hold as a foreign key and
  fail as a meaning. Both interleavings are tested: declaration-then-monitor and
  monitor-then-declaration must both end with the winning epoch at the boundary resolving to
  the winning declaration at that boundary.
- **The trigger is the existing `execution_revision` bump; the no-op decision is the
  evaluation-semantics hash of §6.3.** A rename bumps `execution_revision` under the existing
  fail-safe contract but changes no evaluation input, so it produces no epoch.
- **Fan-out is bounded** by `max_services_per_monitor` (§10.10), and the count is taken under
  the monitor row lock, so two concurrent service writes cannot both pass a stale count.
- Writes that do not move `execution_revision` — status and counter updates, ciphertext
  re-encryption that does not change the referenced secret's generation, dependency-only
  edits — create no epoch.

**Ownership consequence, which is the point of splitting the axes.** Creating a system epoch
does not touch a file-owned service's declaration: its canonical hash, generation, current
definition revision and provenance are unchanged, and a reapply is still a no-op. An operator
may therefore edit a monitor's interval even when a file-owned service uses it. The blocking
and freezing rules of §15.1 apply to **declaration** mutations only.

### 6.3 The evaluation-semantics projection

r2 triggered on a broad `execution_revision` bump but decided no-op on a narrow snapshot of
`(type, region, interval, stale_deadline, enabled)`. That does not remove the allowlist; it
hides it inside the hash, and it hides it in the dangerous direction: a **target change** bumps
`execution_revision`, leaves that snapshot byte-identical and therefore produces no epoch —
while §12.1 correctly says a target change can make two numbers incomparable.

Phase 1 defines ONE canonical projection, `EvaluationSemantics(monitor)`, covering everything
that changes *what endpoint or operation produced `heartbeat.up`*:

| In the projection | Why |
|---|---|
| `type` | a `push` member and an HTTP member mean different things by a missing observation |
| `target` | the endpoint the verdict is about |
| `method`, request shape, type-specific execution config | the operation performed |
| conditions / assertions / scenario steps, in canonical order | what counts as `up` |
| `region` | the vantage point |
| `interval`, `timeout`, `retries` | cadence, and therefore held state and staleness |
| `enabled` | whether the member produces observations at all |
| push dead-man TTL | the failure definition for `push` |
| credential **identity and generation** — secret reference name plus rotation fence | a rotated credential can change what the probe is authorized to see |

| Excluded | Why |
|---|---|
| display name, description, tags | presentation |
| notification, escalation and dependency wiring | delivery, not measurement |
| confirmation and failure thresholds | §6.7 — cadence and alerting, not measured state |
| status, counters, result watermarks | server-owned runtime |
| secret **material** and ciphertext | never enters a row or a hash; re-encryption alone is not a semantic change |

Two rules make this a contract rather than a list:

1. **The projection is defined beside the field set that decides an `execution_revision`
   bump, in one place.** They cannot drift into two lists maintained by two people.
2. **The classification is exhaustive, not an allowlist.** Every field that can bump
   `execution_revision` is explicitly classified IN or OUT, with no default, and a test fails
   when a new field is added without a classification. This is the FR-020 lesson applied: a
   rule with a silent default is a rule that mishandles the next field added.

Required tests: target-only change, condition-only change and secret-generation rotation each
produce an epoch; rename-only, status update and ciphertext re-encryption each produce none.

### 6.4 Temporal validity: when a write happened, and when it starts to govern

r2 conflated three instants and contradicted itself between them. r3 separates them:

```
created_at    = statement_timestamp()          -- when the write happened; audit and ordering
effective_at  = ceil_to_bucket(created_at)     -- when the row starts to govern buckets
```

- `created_at` comes from the **database clock**, never an application clock.
- **Ordinary writes are prospective and CEIL, not floor.** A write at `12:00:30` governs from
  `12:01:00`. r2 called this "floor" and then gave `12:01:00` as the answer; the two disagree
  at every non-boundary instant, and the disagreement was load-bearing.
- **A write landing exactly on a boundary uses that boundary**: `ceil_to_bucket(12:01:00) =
  12:01:00`. Stated because the equality case is where two implementers diverge.
- Validity is half-open over **effective** rows only:
  `[r.effective_at, next_effective(r).effective_at)`.
- **First adoption is the one retroactive case**, covered by §6.6.
- **A retroactive rewrite is a separate, audited administrative operation** with an explicit
  requested range (§10.6, §10.9). "Recompute" is never a synonym for it.
- **"Recompute a revision or epoch" means exactly its validity range**, intersected with raw
  availability — not "everything raw that still exists".

Because both axes are ceiled to a bucket boundary, a bucket is never split by a boundary, and
"exactly one active epoch per bucket" holds by construction rather than by rounding convention.

### 6.5 Same-boundary resolution — exactly one winner

Two writes at `12:00:10` and `12:00:40` both target `12:01:00`. Immutable rows plus a half-open
interval keyed on `effective_at` gives no order between them and a zero-length interval for the
loser. The resolution:

- `revision` and `epoch_seq` are **monotonic sequences assigned under the per-service lock**,
  so the order of two writes is total even when their `effective_at` is equal and their
  `created_at` falls within clock resolution.
- At most one row per `(service_id, effective_at)` may be in state `effective`. The later
  write, holding the same per-service lock, marks the earlier same-boundary row
  **`superseded_before_effect`** in the same transaction. That row is retained for audit, is
  never referenced by a fact, and contributes no validity interval.
- **Durable ranges belonging to a superseded row are cancelled or re-targeted in that same
  transaction** (§10.8), so no job survives pointing at a row that never took effect.
- Storage enforces it: a partial unique index on `(service_id, effective_at) WHERE state =
  'effective'`, on both the revision and the epoch table.

Consequence: validity intervals over effective rows are non-empty and totally ordered, and a
fact resolves to exactly one epoch and one revision under any interleaving.

### 6.6 First adoption is a declared reconstruction, not a measurement claim

Creating a Service may request a backfill. Revision 1 and epoch 1 then carry an explicitly
retroactive `effective_at = floor(backfill_start)`, so the facts produced have a producing
definition rather than predating one.

What must be said plainly, because r2 implied more than it can deliver: the backfilled range
is evaluated under the **current** execution snapshot of its members. Cerbix holds no history
of what each monitor's interval, region or target used to be before the service existed. A
backfilled segment is therefore a **declared reconstruction** — "this is what the window looks
like when evaluated with today's members as declared today" — and it is labelled as such in the
API payload and on the timeline, and excluded from any claim of historical fidelity. It is not
evidence that the members were configured that way at the time. Presenting a reconstruction as
a measurement is the same class of lie this document exists to prevent.

### 6.7 Confirmation thresholds are not an input

The evaluator reads raw `heartbeat.up`, exactly as the existing monitor SLI does. Confirmation
and failure thresholds change **cadence and alerting**, not the measured state, and no
historical series of confirmed status transitions exists to read instead. This is stated
normatively so it is not left to an implementer: reproducing `monitor.status` would require a
transition history the product does not have, and inventing one is out of scope for phase 1.

## 7. Effective monitor state and the canonical bucket

### 7.1 Sample-and-hold

Service aggregation integrates **member state over time**, not per-bucket counters. Counters
cannot express cross-monitor simultaneity, and members have heterogeneous intervals (30s and
5m in one service is normal).

```
monitor observation
    ↓  normalization owned by the monitor TYPE
normalized state: GOOD | BAD | UNKNOWN
    ↓  held effective until
min(next_observation, stale_deadline)
    ↓
service aggregation integrates effective member states over the bucket
```

**Normalization is the monitor type's responsibility**, not the service's: an active probe and
a dead-man's-switch (`push`) disagree on what a missing observation means. The service layer
consumes `GOOD|BAD|UNKNOWN` and never re-guesses.

**Freshness.** Each member has a `stale_deadline` derived from the epoch's freshness policy
applied to its snapshotted type and interval (active probes: a small multiple of the interval;
`push`: its own dead-man TTL). What happens past the deadline is the type's answer, not a
global rule: for an active probe the absence of a result is uncertainty, so the state becomes
`UNKNOWN`; for `push` the absence IS the failure, so it becomes `BAD`.

The precise claim: an **observed** `GOOD`/`BAD` is never held indefinitely — it decays at the
deadline. A type-derived state may then persist: a stale enabled `push` member stays `BAD`
until a real ping arrives, which is what a dead-man's switch means and matches the existing
implementation.

**Member state precedence**, evaluated in this order at every instant:

1. `enabled = false` in the epoch snapshot → **excluded** (not `BAD`). Scheduled ingest already
   drops disabled monitors and the push dead-man requires `enabled`; a deliberately disabled
   member must not tank a service forever. Disabling is in the evaluation-semantics projection,
   so it produces an epoch and the exclusion is visible on the timeline rather than inferred.
2. otherwise normalize by type, including the stale rules above;
3. then apply `maintenance_policy`: a member inside a maintenance window is **excluded**, and
   exclusion wins over the normalized state — a stale `push` member inside maintenance is
   excluded, not `BAD`. Maintenance ending does not reset liveness: the next sample is
   immediately `BAD` again until a real ping, which is correct and must not be smoothed.
4. then `missing_data_policy` decides what an `UNKNOWN` member contributes (§8.1).

### 7.2 The canonical bucket and the piecewise reducer

r2 said "integrate over the interval, BAD dominates GOOD, UNKNOWN fills the parts covered by
neither" and then stored one enum. That is not a reducer: a member GOOD for 20s and UNKNOWN for
40s with no BAD has no verdict under those words, and a member excluded by maintenance for 40s
of a bucket cannot be expressed by an enum at all. Both are ordinary cases, not edge cases.

**Buckets** are UTC half-open intervals `[start, end)` of `bucket_size` (60s in phase 1),
aligned to Unix epoch, never to a service's creation time.

**The reducer is piecewise over breakpoints.** Within a bucket, collect the breakpoint set:

```
bucket start and end
each observation instant of each declared member
each member's stale_deadline
each maintenance window edge intersecting the bucket
```

Between two consecutive breakpoints every member's effective state is constant by
construction, so the aggregation of §9 is evaluated **once per sub-interval** and its outcome
accumulates the sub-interval's exact length into one of four accumulators:

```
good_duration      sub-intervals whose service outcome is GOOD
bad_duration       ...                                  BAD
unknown_duration   ...                                  UNKNOWN
excluded_duration  ...                                  EXCLUDED   (§8.1 — declared only)
```

**Conservation is a stored invariant**, checked on write:
`good + bad + unknown + excluded = bucket_size`, exactly, in integer microseconds.

**Microseconds, not milliseconds.** Breakpoints derive from `timestamptz`, which is microsecond
precision. Storing milliseconds would require a rounding rule with a conservation correction,
and an undocumented rounding rule inside an error budget is precisely the quiet error this
document is written against. Durations are `bigint` microseconds.

**Equality is resolved by half-openness everywhere.** An observation at exactly `end` belongs
to the next bucket. A `stale_deadline` falling exactly on a sample instant has already expired.
A maintenance window `[from, to)` excludes `from` and not `to`.

**Ordering of observations.** `heartbeats` already carries a unique `(monitor_id, ts)`
(migration `00039`) and replay is `ON CONFLICT DO NOTHING`, so two observations cannot share an
instant and no tie-break is required. r2 specified `(ts, observed_at, ctid)`; **`ctid` is
removed from the semantic contract** — it is a physical heap location that changes under
`VACUUM FULL` and table rewrites, and an output depending on it is not reproducible. If a future
input genuinely needs multiple events at one instant, the answer is a durable ingest sequence
in the schema, never heap location.

**The evaluator is a pure function** of `(observations, epoch snapshot, maintenance spans,
range)`. It takes no clock: `now` enters only as the caller's range bound. This is what makes
the determinism property test of §10.8 testable rather than aspirational.

## 8. Missing data, exclusion, and what may not be laundered

### 8.1 The eligibility algebra

Four dispositions, and the distinction between the last two is load-bearing:

| Disposition | Availability | Coverage denominator | Meaning |
|---|---|---|---|
| GOOD | numerator and denominator | yes | measured, serving |
| BAD | denominator | yes | measured, failing |
| UNKNOWN | neither | **yes** | we should have measured and did not |
| EXCLUDED | neither | **no** | we declared this time out of scope |

`unknown_duration` hurts coverage; `excluded_duration` does not, because an exclusion is
something the operator declared in advance. That asymmetry is an attack surface and must be
closed explicitly:

> **`missing_data_policy: ignore` may never move time from `unknown_duration` into
> `excluded_duration`.** `ignore` removes an UNKNOWN member from the aggregation, and may do
> so only while other known eligible members still make the interval decidable. If `ignore`
> removed the last source of information, the interval is **UNKNOWN**, not EXCLUDED. The mixed
> case — some members excluded by maintenance, the rest ignored as unknown, nothing known
> remaining — is likewise UNKNOWN.

Without this rule, `missing_data_policy: ignore` is a one-line configuration change that buys
100% coverage on a service that measured nothing, and the coverage axis of §11.2 — the reason
this document has a second axis at all — is defeated from the settings page.

`excluded_duration` is admissible **only** for declared eligibility exclusions: a maintenance
window, or a member disabled in the epoch snapshot. An ignored member is **not** an exclusion;
it has its own variable in the algebra (§9.3) and its own provenance cause, so a window
weakened by `ignore` is never mistaken for one weakened by a declared exclusion.

### 8.2 Policies

- `missing_data_policy` (part of the revision): `unknown` (default), `bad`, or `ignore`,
  subject to the laundering rule above.
- `maintenance_policy` (part of the revision): `exclude` (default), mirroring today's rule
  that maintenance heartbeats leave both numerator and denominator.

Maintenance windows are themselves declarations about time. Which of their mutations are
prospective, which are retroactive repairs, and what happens beyond raw retention is §10.9.

## 9. Aggregation — a total function

`aggregation_policy` and `region_policy` are part of the definition revision, because changing
them changes the meaning of history exactly as changing members does.

Policies must cover Cerbix's multi-region reality: the same logical check frequently runs from
`core`, `geo1`, `geo2`, and neither `all` nor `any` is correct there (`all` marks a service
down when one vantage point breaks; `any` calls it healthy when two of three regions are
dark). Phase 1 therefore specifies policy objects rather than boolean modes:

```yaml
aggregation:
  mode: quorum            # all | any | quorum
  degraded_min: 1         # the availability threshold
  healthy_min: 3          # splits GOOD into HEALTHY and DEGRADED
region:
  mode: per_region        # per_region | any_region | all_regions
  degraded_min_regions: 1
  healthy_min_regions: 3  # default: every region in the expected set
```

### 9.1 The fact carries durations, and two derived results

The reducer emits **two** results per sub-interval, and both are integrated into durations.
r3 promised health per sub-interval, showed `SLI status: HEALTHY` in §12.2, and then stored only
the availability axis — which makes a bucket that was HEALTHY for 60s byte-identical to one that
was HEALTHY for 30s and DEGRADED for 30s. Once the raw heartbeats are gone, the difference is
unrecoverable, and a two-layer health card whose second layer has no history is not the product
§12.2 describes. Storing it costs four more integers and no new machinery: the reducer already
computes health per sub-interval, and discarding it was throwing away work already done.

```
availability axis   good_duration + bad_duration + unknown_duration + excluded_duration
health axis         healthy_duration + degraded_duration + down_duration
                    + health_unknown_duration + excluded_duration
```

Both sum to `bucket_size` exactly, in microseconds, and `excluded_duration` is shared: declared
exclusion is the same time under either reading. Both roll up as exact sums (§10.2). Only the
**availability** axis enters SLO, budget, burn and coverage (§11.1); the health axis is history
for presentation and is never substituted for availability.

```
availability  GOOD | BAD | UNKNOWN                 ← the only input to SLO and error budget
health        HEALTHY | DEGRADED | DOWN | UNKNOWN  ← presentation, with its own history
```

`DEGRADED` maps to `availability = GOOD`: the service was serving. The mapping is stated here
rather than left implicit, because a budget that quietly counted degradation as failure would
be a different product.

The **display enums** for a whole bucket are derived from the durations, never stored as the
truth, and never used in budget math. Both axes use the same pessimistic shape:

```
availability tick   bad > 0 → BAD; else unknown > 0 → UNKNOWN; else good > 0 → GOOD;
                    else EXCLUDED (rendered as an excluded tick, never as good)
health tick         down > 0 → DOWN; else degraded > 0 → DEGRADED;
                    else health_unknown > 0 → UNKNOWN; else healthy > 0 → HEALTHY;
                    else EXCLUDED
```

That display rule is deliberately pessimistic — a one-second outage colours the whole tick — so
the picture never hides an outage, while the number underneath stays exact. It is the same
split already accepted between `health` and `availability`.

### 9.2 The evaluation order, fixed

Per sub-interval (§7.2), for one service:

1. **Normalize** each declared member by type (§7.1).
2. **Exclude** by `enabled` and by `maintenance_policy` → this yields the `excluded`
   cardinality, which §9.3 needs.
3. **Apply `missing_data_policy`** to remaining `UNKNOWN` members, subject to §8.1.
4. **Aggregate within each region** using `aggregation_policy` over that region's eligible
   members.
5. **Aggregate regions** using `region_policy` over the expected region set.
6. **Emit** the sub-interval's disposition, which the reducer accumulates.

### 9.3 Member truth table, with eligibility

Within one region, over that region's **declared** members:

```
d  declared in the revision for this region
x  excluded — maintenance or disabled in the epoch snapshot (declared exclusions only)
i  ignored  — UNKNOWN members removed by missing_data_policy: ignore, subject to §8.1
n  = d − x − i     eligible, partitioned after the policy into g good, b bad, u unknown
```

r3 defined `n = d − x` and required `n` to partition into `g + b + u`, which left an ignored
member with nowhere to be: either it stayed in `u` and the policy did nothing, or it vanished
and the equality was false. `i` is that missing variable. Concretely, `mode: all` over a GOOD
and an UNKNOWN member under `ignore` now yields GOOD — r3's table returned UNKNOWN and silently
contradicted §8.1.

**Thresholds are declared against `d` and clamped to the post-policy `n` at evaluation time:**

```
effective_degraded_min = min(degraded_min, n)
effective_healthy_min  = min(healthy_min,  n)
```

Clamping is what keeps a *temporary* exclusion from invalidating a *stored* declaration. r2's
table returned BAD when `degraded_min = 3` and one of three members entered maintenance — a
failure verdict caused by a planned exclusion. It would also mean an execution change
(disabling a monitor) can effectively invalidate a file-owned declaration, which §6.2 forbids.

| mode | availability | health |
|---|---|---|
| `all` | `n = 0` → §9.4; `b > 0` → BAD; `u > 0` → UNKNOWN; else GOOD | DOWN / UNKNOWN / HEALTHY correspondingly |
| `any` | `n = 0` → §9.4; `g > 0` → GOOD; `b = n` → BAD; else UNKNOWN | HEALTHY / DOWN / UNKNOWN |
| `quorum` | `n = 0` → §9.4; `g ≥ effective_degraded_min` → GOOD; else if `g + u ≥ effective_degraded_min` → UNKNOWN; else BAD | `g ≥ effective_healthy_min` → HEALTHY; `g ≥ effective_degraded_min` → DEGRADED; `g + u ≥ effective_degraded_min` → UNKNOWN; else DOWN |

`degraded_min` is the availability threshold and `healthy_min` only splits GOOD into HEALTHY
and DEGRADED: since `degraded_min ≤ healthy_min`, availability has ONE good branch, and writing
it as two invites an implementer to make them differ.

**Weakened declarations are never silent.** Whenever `effective_min < declared_min` for a
sub-interval, the bucket's provenance records `declaration_weakened` with the cardinalities and
their causes, **distinguishing `x` from `i`**, the API payload carries it, and the UI marks the
affected span. The distinction matters to the reader: a quorum weakened by a planned maintenance
window is an operator's own decision, while a quorum weakened because data went missing and the
policy discarded it is a measurement problem wearing the same clamp. A window computed under a
clamped quorum says so, and says which kind; it does not quietly report a stricter promise than
it kept.

### 9.4 The empty-eligible case, and the region table

**`n = 0` is resolved by CAUSE, not by a single constant** — the three cases are distinguished
by which variable emptied the set:

| cause | region result |
|---|---|
| `x = d` — every declared member excluded by maintenance or `enabled = false` | **EXCLUDED**, declared out of scope |
| `i > 0` — `ignore` removed the last source of information, alone or mixed with exclusions | **UNKNOWN**; §8.1 forbids calling this excluded |
| `d = 0` | cannot occur inside evaluation: a service with no declared members never reaches the evaluator at all (§9.5) |

**The expected region set** is the set of distinct regions of the declared members in the epoch
snapshot — not a free-text list. A region present in the snapshot with no observations is
**UNKNOWN**, not absent: silently dropping it would let two dark regions look like unanimous
health.

The region stage carries the same three variables one level up: a region is EXCLUDED only when
its members were excluded by declaration, never when `ignore` emptied it.

Across regions, let `R` be the expected set minus regions that are EXCLUDED, with `gr` GOOD,
`br` BAD, `ur` UNKNOWN, and `hr` the count of regions whose health is HEALTHY:

```
effective_degraded_min_regions = min(degraded_min_regions, |R|)
effective_healthy_min_regions  = min(healthy_min_regions,  |R|)
```

| condition | availability | health |
|---|---|---|
| `\|R\| = 0` | EXCLUDED if every region was excluded, otherwise UNKNOWN | correspondingly |
| `gr ≥ effective_degraded_min_regions` | GOOD | `hr ≥ effective_healthy_min_regions` → HEALTHY, else DEGRADED |
| `gr + ur ≥ effective_degraded_min_regions` | UNKNOWN | UNKNOWN |
| otherwise | BAD | DOWN |

`mode: any_region` is sugar for `degraded_min_regions = 1, healthy_min_regions = 1`;
`all_regions` sets both to `|expected|`. Defaults are `healthy_min_regions = |expected|` and
`degraded_min_regions = 1`, which for the common single-region service collapses to the obvious
behaviour and for a multi-region service means *one dark vantage point makes the service
degraded, not down* — the outcome §9's opening paragraph demands.

**Policy defaults**, so that "every field has a stated default" (§21) is true of these too:
`aggregation.mode: all` — every declared reliability input must be good, the conservative
reading of "these count"; `region.mode: per_region` with the thresholds of §9.4;
`missing_data_policy: unknown`; `maintenance_policy: exclude`; `freshness` for active probes
`3 × interval` with a `90s` floor, and for `push` the monitor's own dead-man TTL.

### 9.5 Validation, against the declaration and not against the moment

**A Service with an empty `sli[]` never reaches the aggregation at all.** §5 makes it a valid
state and invariant 41 forbids reporting it as 100%, but r3 also wrote "`d = 0` cannot occur"
and "at least one declared member per region", so the same document both permitted and outlawed
it. The resolution is a short circuit ahead of everything else:

> An empty-SLI declaration creates its definition revision and a **matching empty epoch**, so
> the declaration's history stays continuous and every later revision has a predecessor. It
> creates **no facts, no ingest handshake rows, no watermark and no SLO**; the service reports
> availability, budget and coverage as `unavailable — no reliability inputs declared`; and the
> threshold validation below does not apply to it, because there is nothing to validate against.

Once `sli[]` is non-empty, and only then, this is enforced at write time on the **declared**
cardinality, and re-checked whenever the declared set changes:

```
1 ≤ declared members per region
0 < degraded_min ≤ healthy_min ≤ declared_count
0 < degraded_min_regions ≤ healthy_min_regions ≤ |expected regions|
```

A policy that has become unsatisfiable **because the declaration changed** is a definition
error surfaced on the service. A policy momentarily unsatisfiable **because members are
excluded** is not an error at all — it is the clamp of §9.3. Conflating the two is what made
r2's table return a failure verdict for a planned maintenance window.

Nested members (a `composite` inside the SLI) are permitted: the composite's own `all|any`
produces its normalized state, which enters step 1 as one member. The UI must show that
nesting, otherwise a budget derived from `quorum(any(A,B,C), HTTP, synthetic)` is
unexplainable to the person reading it.

## 10. Storage and computability (the phase-1 core)

### 10.1 Why a new fact store is required

`heartbeats_daily(monitor_id, day, up, total)` cannot serve service aggregation: knowing "A was
up 1400/1440" and "B was up 1430/1440" does not say whether they were down *at the same time*.
Boolean cross-monitor logic needs bucket alignment, not daily counters. Raw heartbeats are
retention-bounded (`heartbeats.retention_days`, default 30) while `StandardWindows` includes
90d. Therefore service reliability needs its **own materialized facts**.

### 10.2 Fact shape

```
service_reliability_buckets
├── service_id, project_id     tenant-safe composite FK
├── evaluation_epoch_id        the epoch that produced it; resolves to one definition revision
├── bucket_start, bucket_size  UTC, half-open, canonical 60s in phase 1
├── good_duration              bigint µs   ┐ availability axis
├── bad_duration               bigint µs   │ CHECK: these four sum to bucket_size exactly
├── unknown_duration           bigint µs   │
├── excluded_duration          bigint µs   ┘ (shared with the health axis)
├── healthy_duration           bigint µs   ┐ health axis (§9.1)
├── degraded_duration          bigint µs   │ CHECK: these three plus health_unknown
├── down_duration              bigint µs   │ plus excluded also sum to bucket_size
├── health_unknown_duration    bigint µs   ┘
├── state                      PROVISIONAL | SEALED
├── sealed_at                  when it became immutable to ordinary ingest
├── sealed_ingest_generation   the value CAS'd at seal time (§10.4)
├── maintenance_generation     the declared-exclusion generation it was computed under (§10.9)
└── provenance                 bounded counts, causes and flags (§10.3)
```

Primary key `(service_id, bucket_start)`; partitioned by `bucket_start` (§3).

Hour and day rollups are **sums of the durations on both axes** over sealed canonical buckets,
**keyed by epoch and never merged across an epoch boundary**. Summation is exact and associative, so a
rollup loses nothing — which is the second reason durations beat a per-bucket enum: an enum
rollup would be a second rounding applied on top of the first, and neither would be measurable
afterwards.

The canonical granularity must stay fine enough for the shortest supported burn window,
otherwise future burn semantics are foreclosed by the storage choice.

### 10.3 Provenance that survives raw retention

r2 stored a verdict and a revision and then promised the number would be explainable at 90 days
— when the raw heartbeats behind it are 60 days gone. A revision explains the DEFINITION; it
does not say which member, which region or which exclusion produced a given bucket.

Each fact therefore carries a bounded provenance record:

- counts per aggregation stage: `declared`, `eligible`, `good`, `bad`, `unknown`,
  `excluded_disabled`, `excluded_maintenance`;
- for a bucket with `bad_duration > 0`, a bounded set of member references that caused it;
- **for a bucket with `unknown_duration > 0`, a bounded set of member references and the reason
  each was undecided** — no observation, past `stale_deadline`, or region never reported — with
  an overflow count when the bound is exceeded. r3 kept causes only for BAD, which left the most
  common reason a 90-day window is `partial` unexplainable exactly when the raw heartbeats
  behind it are gone. A promise the bound cannot carry must be removed from the promise; this
  one fits inside the bound, so it is kept rather than removed;
- `declaration_weakened` with the clamped thresholds and cause, when §9.3 clamped;
- stable maintenance identity and `maintenance_generation` for every exclusion applied, so a
  window archived later still resolves to a name and a reason (§10.9);
- `reconstructed` when the bucket came from a first-adoption backfill (§6.6).

Bounded is the operative word: a fixed small structure per bucket, not an event log. Anything
the bound cannot carry is removed from the explainability promise rather than implied by it.

### 10.4 Sealing, the late-arrival boundary, and the ingest handshake

```
OPEN → PROVISIONAL → (bucket_end + late_arrival_grace) → SEALED
```

Sealing is part of the correctness model, not an optimization: without it the budget silently
drifts between two viewings and the number becomes untrustworthy.

**`late_arrival_grace` is its own bounded setting, not `result.allowed_skew`.** r2 reused the
skew and that was wrong: `allowed_skew` bounds a worker clock running FAST — ingest rejects
`ts > now + skew` — and says nothing about how late a result may arrive. Old results are
accepted up to raw retention, the durable results queue has no TTL, and an agent's historical
backfill can arrive much later still. The grace is an explicit accounting-finality policy with
a default and a maximum (§10.10).

**The handshake, because "visible to the sealing transaction" is not a mechanism.** r2 declared
that a heartbeat counts if it is visible to the sealing transaction. PostgreSQL at READ
COMMITTED takes a snapshot per *statement*, no isolation level or lock was specified, and
nothing let the ingest side discover it had lost the race:

```
1. seal reads observations in snapshot S; heartbeat H is not yet committed
2. H commits; its ingest sees no sealed row and records nothing
3. seal writes SEALED and advances the watermark, excluding H
   → H is excluded from the number and there is no record that it was excluded
```

The promised late-arrival metric and timeline marker are unreachable in that design, and
`heartbeats` carries no `received_at` from which the exclusion could be reconstructed later.

Phase 1 specifies a real mutual-exclusion point:

```
service_bucket_ingest
├── service_id, project_id, bucket_start     PRIMARY KEY (service_id, bucket_start)
└── ingest_generation  bigint
```

- **Every heartbeat ingress that actually INSERTS a row** — scheduled result, push ping,
  synthesized dead-man DOWN, agent historical backfill — upserts this row, incrementing
  `ingest_generation`, **in the same transaction as the heartbeat insert**. All four origins go
  through ONE ingress helper; a second path is a second set of rules.
- **The handshake is gated on real insertion, not on delivery.** Every ingest path today is
  `INSERT … ON CONFLICT (monitor_id, ts) DO NOTHING`: the scheduled path returns
  `ReasonDuplicate` when `RowsAffected() == 0` (`internal/store/monitors.go`), and the historical
  path counts only rows actually inserted (`internal/store/heartbeats.go`). r3 said "every
  heartbeat ingress" without that link, which turns an ordinary redelivery of an already-counted
  heartbeat into a **false late-arrival record**: the row was counted before the seal, its
  duplicate arrives after, and the helper would file it as data the seal excluded. A duplicate
  is a **full no-op** — no generation bump, no late record, no metric. The dead-man path checks
  `RowsAffected()` for the same reason, even though a collision there is rare.
- **Membership is resolved AS OF the heartbeat's bucket, never as of now.** The affected services
  are those whose **SLI** declared the monitor in the definition revision effective at
  `bucket_start(heartbeat.ts)` — read from the epoch snapshot for that instant, not from today's
  reference rows, and from `sli[]` only, never from the operational `monitors[]`. Historical
  ingest is deliberately revision-exempt and can arrive days late, so today's membership is the
  wrong question in both directions: a member removed from the SLI at 12:00 would leave an 11:59
  heartbeat with no generation bump and no late record even though the 11:59 fact was produced by
  an epoch containing it, and a member added at 12:00 would dirty buckets in which it was not yet
  a member. Reference rows are therefore retained for at least the raw-data and late-arrival
  horizon, and the fan-out cap is evaluated against that historical instant rather than against
  today's count.
- **A monitor in no service's SLI at that instant writes nothing**, so an installation with zero
  services pays nothing — which is what makes the "costs nothing until a service exists" claim of
  §17 true rather than rhetorical.
- **The sealing transaction MATERIALIZES then locks the ingest row for every bucket in its
  range** — `INSERT … ON CONFLICT (service_id, bucket_start) DO UPDATE` returning the row, which
  both creates a missing row and takes the lock on an existing one. Locking only rows that
  happen to exist would leave a phantom: a bucket that received no heartbeat before the seal has
  no row to lock, and a concurrent ingest could insert one and commit inside the seal's window.
  The upsert closes it, because a concurrent ingest then blocks on the same primary key.
- The seal records the observed `ingest_generation` as `sealed_ingest_generation`, computes,
  writes the facts as SEALED and advances the watermark, then commits.
- **After the seal commits, an ingest for that bucket discovers it under the same row lock** and
  reads the FACT's state — the ingest row is the mutual-exclusion point, the fact is the
  authority. Seeing `SEALED`, it records the arrival instead of dirtying the bucket, and
  increments a low-cardinality metric. The heartbeat itself is still recorded as raw data.
- **Late arrivals are AGGREGATED, not one retained row per event.** A single historical agent
  batch landing after a seal, multiplied by `max_services_per_monitor`, would otherwise create
  millions of retained rows. `service_late_arrival` is keyed
  `(service_id, project_id, bucket_start, monitor_id)` and carries `count`, `first_received_at`,
  `last_received_at`, a bounded set of example timestamps and an overflow counter; the key is
  unique and the write is an upsert, so redelivery of a genuinely late row cannot multiply the
  evidence. Retention follows the facts (§10.6); per-bucket size is bounded by §10.10.
- Because the decision reads the fact and not the ingest row, ingest rows carry no history and
  are pruned with their partition once their buckets are sealed; a late arrival re-creates the
  row it needs, finds the sealed fact, and is classified correctly.
- **Provisional buckets** are simply marked dirty by the generation bump; the materializer
  recomputes buckets whose generation moved since their last computation.

A sealed fact disagreeing with raw history therefore always has a durable, renderable
explanation rather than looking like corruption — including months later, when the raw rows are
gone but the late-arrival records remain (they are retained with the facts, §10.6).

A sealed bucket is rewritten in exactly two cases, both audit-visible:

1. an explicit definition-revision or epoch recompute over that row's validity range;
2. an administrative repair, annul or backfill with an explicit requested range (§10.9).

### 10.5 The `sealed_through` watermark

**`sealed_through` is defined by CONTIGUITY, not by the newest sealed row**: it is the greatest
bucket boundary `T` such that every canonical bucket in `[materialization_start, T)` exists and
is `SEALED`. A hole — a bucket a failed batch never wrote — holds the watermark at the hole
instead of being jumped over, which is why §11.2 can treat storage continuity as answered by
one scalar.

The watermark is durable, advanced only by a sealing transaction, and never moves backwards
except under an audited operation that explicitly unseals or invalidates a range (§10.9); that
operation records the retraction, because a window that silently shortened is
indistinguishable from a bug. Materialization gaps are therefore visible as a stalled watermark
rather than as a plausible number.

### 10.6 Recompute, retention and backfill

- **Recompute range is the target's validity interval intersected with raw availability.**
  Beyond raw retention, prior facts keep their original epoch and revision (§6.4).
- **Derived facts are NOT pruned by heartbeat retention.** They must outlive raw data — that is
  the only way a 90d window exists at all. Their retention is independent and at least as long
  as the longest supported window, by partition drop (§3). Late-arrival records and provenance
  share the facts' retention, not the heartbeats'.
- **Backfill on service creation** is a bounded leader-run job with visible progress, under
  revision 1's explicitly retroactive `effective_at`, and its output is labelled a declared
  reconstruction (§6.6).
- **An audited recompute that materially moves a sealed window is recorded with the affected
  range and before/after availability**, so "why did last month change?" always has an answer.
  It is not blocked: a large correction usually means the old number was wrong.

### 10.7 Ownership and real fencing

Only the elected scheduler leader materializes, seals, recomputes, repairs and backfills. API
and MaC only commit declarations (and, transactionally, epochs, invalidations and durable
ranges) and publish a change event; they never compute.

**r2 claimed this reuses `LeaderSession`, and that was false about this codebase.**
`LeaderSession` is the file-provider path; its own documentation says it is "distinct from
`TryBecomeLeader` (used by the scheduler), which hands back bare callbacks and lets writes use
the pool". The scheduler holds its advisory lock on a pinned connection but writes through the
pool, and its watchdog runs every 5s — so a deposed leader can still commit a pooled
transaction after a successor exists. Phase 1 therefore **requires an ownership migration, not
a reuse**:

- the election mechanism stays the existing PostgreSQL advisory lock — no second ownership
  mechanism is introduced — but the scheduler's election becomes a session object of the same
  shape as `LeaderSession`, so that lock ownership and the writing connection are the same
  thing;
- **every service-fact batch transaction runs on the lock-owning connection**, so losing the
  lock aborts the in-flight batch, exactly as a file apply does today;
- because one connection cannot serve a long recompute and the watchdog at once, batches are
  short and serialized — the fencing fix must not become a cadence stall, which is the trap
  §10.10 exists to prevent.

Passing the existing `check()` callback into the computation is **not** fencing and does not
satisfy this requirement.

### 10.8 Durable work is a set of RANGES, not "the current job"

```
API / MaC → commit declaration (+ epoch, + invalidation, + durable ranges) → NOTIFY
          → scheduler leader → recompute each range under ITS OWN epoch → write facts
```

`NOTIFY` is lost if no leader is listening between commit and subscribe, and "the successor
resumes from the first unrecomputed range" is impossible without state to resume from. Worse,
r2 superseded a running job whenever a newer revision or epoch arrived, which abandons work
that is still valid:

```
E1 backfill of [t0, 12:01) has reached the halfway point
monitor changes at 12:00:30 → E2 becomes effective at 12:01
r2 supersedes the E1 job → the unfinished E1 buckets are never written
→ sealed_through stalls at the hole forever (§10.5), and no E2 job is scoped to fill it
```

Phase 1 therefore persists **dirty intervals**, not a current job:

```
service_repair_ranges
├── service_id, project_id
├── range_start, range_end        half-open, bucket-aligned
├── reason                        declaration | epoch | late_data | maintenance | admin | backfill
├── state                         pending | running | complete | error | superseded
├── cursor                        progress inside the range, so a resume continues
├── maintenance_generation        CAS target (§10.9)
└── attempts, next_attempt_at     retry with the floor of §10.10
```

- **The epoch is resolved per bucket, not per range.** A range spanning an epoch boundary
  evaluates each subrange under the epoch effective there. This is what makes the next rule
  safe.
- **A newer epoch or revision enqueues its own range; it never cancels unfinished historical
  work.** Supersession is legal only when a successor range atomically assumes the **union** of
  the ranges it supersedes, and the union is stored, not implied.
- Overlapping and adjacent ranges coalesce; coalescing preserves the union exactly.
- Ranges belonging to a `superseded_before_effect` row (§6.5) are cancelled in that same
  transaction, because that row never governs any bucket.
- `NOTIFY` is only a wake hint, and a **periodic resync** is mandatory so a missed notification
  costs latency, never correctness.
- Service or project deletion cancels its ranges inside the deleting transaction.

**One computation path.** The incremental pass MUST be literally "recompute this bucket range"
— the same implementation the batch recompute uses, invoked with different bounds. Two code
paths diverge at the edges and the divergence is undetectable in normal operation.

**Work partitions are bucket-aligned, and that is a constraint, not an omission.** A range is a
whole number of buckets, the fact's primary key is one whole bucket, and its CHECK requires the
durations to sum to `bucket_size`. r3's determinism invariant demanded partitions whose
boundaries fall *strictly inside* a bucket, which cannot coexist with either: splitting
`[00:00, 01:00)` into two thirty-second halves leaves the first run to write 30µs-worth of
durations and violate the CHECK, or to read and write the whole bucket — in which case it was
never a partition of the input. r4 does not design an idempotent partial accumulator to rescue
that, because none is needed: the sub-bucket dimension belongs to the reducer, not to the work
queue.

**The determinism invariant, restated so it is both testable and satisfiable:** for the same
observations, epoch snapshot and maintenance spans, an incremental run over ANY **bucket-aligned**
partition of a range produces the same fact CONTENT as one batch run over that range — both
duration axes exactly, the provenance, and the epoch reference. Lifecycle metadata (`state`,
`sealed_at`, `sealed_ingest_generation`, `recomputed_at`) is excluded, since literal row equality
is impossible for it.

The property test therefore works on two levels, and the second is where r3's intent actually
belongs:

1. it partitions the WORK only on bucket boundaries, writes incrementally, rebuilds the same
   range in a scratch table, and compares sorted canonical content plus both conservation checks
   on every row;
2. **inside each bucket** it generates observation instants, stale deadlines and maintenance
   edges at arbitrary sub-second offsets — including offsets that coincide exactly with the
   bucket edges and with each other — and compares the piecewise reducer against an independent
   oracle implementation. This is where §7.2 either holds or does not, and it is reachable
   without pretending the work queue can be split mid-bucket.

### 10.9 Maintenance is a retroactive declaration, and its mutations are two different acts

The evaluator depends on maintenance spans while `maintenance_windows` rows are hard-deleted
today (`DELETE FROM maintenance_windows`, `internal/store/sla.go`). Storing exclusion counts in
a fact explains an old output but cannot reproduce it, and nothing today repairs the affected
range — so a deletion silently desynchronizes the number from the declaration until some
unrelated recompute rewrites it, at which point it looks like corruption.

The model is small because the existing one is small: a window is
`(project_id, monitor_id NULL = whole project, [starts_at, ends_at), reason)`, with **no
recurrence** and no update path. Phase 1 therefore does not introduce bitemporal evaluation. It
introduces a generation, an invalidation, and a separation of two acts that r2 treated as one.

**Two distinct intents on removal, separated in the API and the UI:**

| Action | What it claims | Effect on sealed past |
|---|---|---|
| `archive` (the ordinary delete) | "this window is no longer part of active inventory" — for an active window, "this maintenance ends at this instant" | **None.** Elapsed and sealed time keeps its applied exclusion, and keeps it under any FUTURE recompute too (the effective-span rule below). Raw data is not required, so an old window can always be archived |
| `annul` (a separate, privileged action) | "this exclusion was a mistake and the past must be recomputed without it" | The symmetric retroactive rule below applies |

The ordinary `DELETE` maps to `archive`. This is what makes "old windows can never be cleaned
up" a non-problem without inventing a `superseded declaration` flag on arbitrary facts.

**Creation has only one intent, so it is decided purely by range** — and the same rule then
governs `annul`:

- a mutation whose affected range lies entirely in provisional or future time is **ordinary and
  prospective**: it bumps the generation, marks the provisional range dirty, and needs no
  repair of sealed data;
- a mutation whose affected range **intersects sealed time is a retroactive repair by
  definition**, whether it is a create or an annul: it requires a preview of the affected
  range, an audit entry with before/after availability, and it is bounded by raw availability;
- if the sealed part of the range cannot be recomputed because the raw heartbeats are gone, the
  mutation **fails closed** with `409 unrecomputable_range` and the earliest repairable
  instant. It is never partially applied, and it is never silently accepted with no effect —
  successfully creating a window over last month and changing no number is the worst of the
  three outcomes, because it looks like a completed command;
- a create whose range spans both sealed and future time is ONE operation: the raw-availability
  fence failing rolls the whole thing back.

**Preview and confirm are two transactions, so they need a token.** r4 required a preview with
before/after for every retroactive mutation and never said what binds the preview to the state it
was computed against. Computing the whole project-wide range synchronously inside the mutating
transaction would violate the fan-out and bounded-work contract of §10.10; computing it
separately and confirming later can audit a before/after that was never true.

```
maintenance_preview
├── preview_id, project_id
├── requested_range              the operator's explicit range
├── maintenance_generation       per project
├── raw_floor                    the earliest instant still recomputable when the preview ran
├── coverage                     complete | approximate
├── computed_at, expires_at
└── created_by

maintenance_preview_services     server-side, one row per affected service — COMPLETE, never a sample
├── preview_id, project_id, service_id
├── definition_generation
└── before_availability          over the requested range
```

- **The CAS set is a relation, not a bounded array.** r5 kept `affected_services[]` "bounded, with
  overflow reported" and in the same breath promised confirm would re-read *every* generation —
  which cannot both be true: if the array truncates, the generations of the services that did not
  fit are never checked and a stale preview can be confirmed. The affected rows therefore live in
  a normalized relation that is always complete; the **API response** may show a bounded sample
  with a total count, because the bound was ever only about response size.
- The preview is a **read-only computation** subject to the same slice deadline as any other
  service work. If it cannot cover the whole requested range within its budget, it is returned
  `approximate` — and **an `approximate` preview cannot be confirmed.** It is a decision aid; the
  operator either narrows the range or waits for a complete one. Applying the part nobody
  previewed would make "preview" a word rather than a control.
- **Confirm is a CAS over the SET, not only over the rows in it.** Re-reading the generations of
  the services already in the relation proves those rows did not move; it does not prove the
  affected set is still the same one. A Service created between preview and confirm, or an
  existing Service that adds the monitor to its `sli[]`, is affected by the mutation while
  neither the project's `maintenance_generation` nor any recorded `definition_generation` needs
  to change — so the token would confirm and mutate a service nobody previewed. Deletion is the
  mirror case and must be equally stale rather than silently shrinking the set.

  Therefore: **inside the mutating transaction, after taking the §15.4 locks, confirm re-resolves
  the affected set with the SAME resolver the preview used, tenant-scoped, and requires exact
  equality** — the same set of `service_id`s and the same `definition_generation` for each — plus
  an unchanged `maintenance_generation`, `coverage = complete`, an unexpired token, and the
  `raw_floor` predicate below. Any difference in either direction fails closed with
  `409 preview_stale`, naming the services added, removed or changed, and **nothing is partially
  applied**.

  **Row locks are not enough, because the affected set is a PREDICATE.** Locking the rows the
  resolution found protects those rows; it protects nothing against a Service that does not exist
  yet. Between confirm's exact-set `SELECT` and its commit, another transaction can create
  Service C, or change a previously unaffected C so that it enters a monitor-scoped set — C was
  in no relation and no row of C was locked, so the comparison already passed and the mutation
  commits over it. Re-resolving under the mutation's own row locks narrows that window; it does
  not close it, and a narrower check-then-act is still check-then-act.

  The predicate is therefore closed by a **named serialization primitive**, the
  `service_membership` transaction-scoped advisory lock, keyed per project:

  - **confirm takes it BEFORE the re-resolution** and holds it through the mutation's commit;
  - **every path that can change the affected-set predicate takes the same lock first**, ahead of
    any service or monitor row lock, per the single order of §15.4. Those paths are enumerated
    rather than described, because an unenumerated one is precisely this hole again: Service
    create; Service delete; any definition write that changes `monitors[]` or `sli[]`, whether it
    comes from the API, from a format-2 apply, or from a system-authored revision; and monitor
    delete or project-move, which §15.1 makes author a system definition revision.

  Everything else is untouched: ordinary monitor execution writes create epochs rather than
  declarations, and ingest, seal and range processing never take it. The lock is held briefly and
  is project-scoped, and it does **not** make an unrelated change stale — it only serializes the
  competing writer, after which the resolver still decides whether that writer was affected at
  all. A `SERIALIZABLE` predicate-protection design would give the same guarantee, and was
  rejected as heavier: it obliges every one of those paths to carry a retry loop.

  A project-level "service set generation" bumped on create, delete and SLI-membership change was
  considered and rejected. It is a rule that would have to live in three write paths, where a
  missed bump is silent and reproduces exactly this hole; and being project-wide it cannot tell
  an unrelated new service from an affected one, so it would invalidate a monitor-scoped preview
  whenever anything at all was created in the project. Re-resolution costs one bounded query
  (`max_services_per_project`) and answers the question directly.
- **`raw_floor` is checked as a monotonic predicate, not for equality**: the requested range's
  sealed start must still be `≥` the current repairable floor. The floor advances continuously
  with heartbeat retention, so byte equality would make every token stale by construction — the
  question is whether the range is still repairable, not whether the clock stood still.
- **The audit records the before/after computed by the repair, not the preview's estimate.** The
  preview tells the operator what they are about to do; the audit says what happened. Conflating
  them is how an "audited" number ends up being a projection nobody verified.
- A confirm without a valid token is rejected. There is no path that performs a retroactive
  mutation on sealed data without one.

**Linearization, because enqueueing a repair does not by itself stop the lie.** A pending job
leaves minutes or hours in which the API confidently serves a number computed under a
declaration that no longer exists. In the same transaction as the mutation, Cerbix therefore:

1. increments a per-project `maintenance_generation`;
2. enqueues or coalesces the affected `service_repair_ranges` for every service whose members
   the window covers;
3. **atomically invalidates the affected facts** — the reads in that range return
   `repairing` with the affected interval instead of the stale number, and the `sealed_through`
   watermark is rewound to the earliest affected bucket where continuity requires it, with the
   retraction recorded per §10.5.

**Batch CAS.** Each repair batch records the `maintenance_generation` it read and commits only
if that generation is still current; otherwise it re-enqueues its remaining range. Without this,
"incremental and batch both read the current declaration" is not a guarantee at all, because
two batches of one range can read two different "currents".

**The effective span survives archiving, and this is what makes `archive` mean what the table
says.** r3 wrote that the evaluator reads "only the current, non-archived declaration", which
quietly turns every archive into an annul on the next unrelated recompute: an expired window
that excluded a real outage would be ignored, the exclusion would become BAD, and a sealed
bucket would change with no preview, no raw fence and no audited intent — the exact operation
`archive` promises not to perform.

> **The evaluator reads a retained maintenance row over
> `[starts_at, min(ends_at, cancel_effective_at))`, regardless of `archived_at`.**
> `archived_at` hides the window from active inventory and from future applicability;
> `cancel_effective_at` truncates it prospectively; **only an explicit `annul` removes the span
> from the evaluator's input** — which is exactly why `annul`, and not `archive`, carries the
> preview, the audit and the raw fence.

This is one effective-span predicate over a row that is already retained, not a bitemporal join
in the materialization path.

**`cancel_effective_at` is the exact database statement time of the cancelling transaction**,
not the next bucket boundary. r3 said "ends now" in the table and "from the mutation's bucket
boundary" in its worked case — different instants. The piecewise reducer of §7.2 handles
maintenance edges at arbitrary offsets, so there is no reason to round, and rounding would
silently extend or shorten a real exclusion by up to a bucket.

**Retention and provenance.** Archived rows keep `archived_at`, `cancel_effective_at`, their
stable id and their reason for at least the facts' retention, so a provenance lookup at 90 days
still resolves to a name and a reason.

**Fan-out.** A project-wide window touches every service in the project. It must not take
unbounded synchronous locks: the generation bump plus durable ranged repairs is the mechanism,
not a mass synchronous rewrite inside the mutating transaction.

### 10.10 Bounds, with actual numbers

r2 titled this section "with numbers" and then listed only names. Every bound below is
configuration with a stated default and a hard maximum; the hard maximum is not operator-
raisable without a code change, because these are what keep the leader's dispatch tick alive.

| Bound | Default | Hard max | Purpose |
|---|---|---|---|
| `bucket_size` | 60s | 60s (fixed in phase 1) | canonical granularity |
| `late_arrival_grace` | 120s | 15m | accounting finality (§10.4) |
| `max_services_per_project` | 50 | 200 | caps declaration growth |
| `max_members_per_revision` | 50 | 200 | caps aggregation width |
| `max_services_per_monitor` | 10 | 25 | caps epoch and ingest fan-out from one monitor write |
| `recompute_batch_buckets` | 1440 (1 day) | 10080 (7 days) | work per transaction |
| `recompute_budget_per_cycle` | 2s | 10s | TOTAL service-work wall clock per leader cycle, spent in slices |
| batch `statement_timeout` | remaining slice budget | — | re-derived before every statement; there is no separate static ceiling, because any value above `max_dispatch_delay` is unreachable and would only imply a second contract |
| batch `lock_timeout` | min(3s, remaining slice budget) | — | likewise capped by the remainder, so a lock wait cannot outlive the slice |
| `max_concurrent_batches` | 1 | 1 | §10.7 fences on the lock-owning connection, so batches are serialized by construction |
| retry backoff | 5s floor, ×2, cap 5m | cap 15m | on range error |
| `min_decidable_coverage` | **0.95, fixed in phase 1** | not operator-settable | below this a window is `partial` (§11.2) |
| `max_provenance_causes` | 8 per cause class | 32 | bounded member sets in §10.3, with an overflow count |
| `max_late_examples` | 8 per aggregated record | 32 | bounded example timestamps in §10.4, with an overflow count |
| `max_dispatch_delay` | 250ms | 1s | the cadence guarantee below |
| `max_scheduling_tolerance` | 25ms | 100ms | the slack the cadence assertion allows for Go scheduling and commit round-trip |

**`min_decidable_coverage` is deliberately not a knob.** r3 listed it as configuration with no
floor, which lets an operator set `0.01` and recreate the exact failure the coverage axis was
introduced to prevent — a confident `100%` derived from almost no measurement. In phase 1 the
value is fixed at `0.95`; if a later phase makes it settable it takes a hard minimum and
validation, not a blank maximum column.

**Serialization of the fan-out cap.** `max_services_per_monitor` is checked under the monitor
row lock, and `max_services_per_project` under the project's service-creation lock, so two
concurrent writers cannot both pass a stale count and exceed the cap.

**The cadence guarantee is ONE mechanism, and the three numbers derive from it.** r4 listed a 2s
service budget, a 15s statement timeout and a 250ms dispatch delay as if they were independent;
they are not satisfiable together, because a transaction allowed 15s blows both of the others,
and "cancelled at the statement timeout" bounds nothing at 250ms. The mechanism is a **per-slice
deadline**:

```
slice_deadline = min(remaining recompute budget this cycle, max_dispatch_delay)
```

- **The slice deadline is enforced caller-side over the WHOLE slice** — `BEGIN` through
  `commit`/`rollback` — as a context deadline. r5 said it was "applied as the transaction's
  `statement_timeout`", and that does not bound a transaction: PostgreSQL applies
  `statement_timeout` to **each statement**, so a batch of four statements at 240ms each passes a
  250ms limit four times over and delays dispatch by a second. A single-slow-query test does not
  catch it either, which is why the acceptance below requires consecutive statements.
- **Server-side timeouts are re-derived before every statement from the REMAINING slice budget**,
  never set once at `BEGIN`. Both `statement_timeout` and `lock_timeout` are capped by what is
  left, so no server-side wait — including a lock wait — can outlive the deadline. When the
  remainder reaches zero the slice rolls back and its range resumes from the cursor.
- Because a slice never exceeds `max_dispatch_delay`, dispatch is serviced between slices and the
  observable the test asserts follows by construction. `recompute_budget_per_cycle` is the
  **total** across slices in one cycle, not the length of any one of them.
- **Batch size adapts** within `[1, recompute_batch_buckets]`, targeting roughly 60% of the slice
  deadline and shrinking on a timeout. A fixed large batch under a tight deadline would time out
  every slice and commit nothing — a livelock that makes progress zero while every individual
  bound is respected.
- **A single bucket that cannot be computed within the deadline is a fault, not a retry loop.**
  It raises its own metric and moves the range to `error` with backoff, because a 60-second
  bucket that takes longer than `max_dispatch_delay` to reduce means something pathological, and
  hiding it inside an infinite retry is how a stalled watermark becomes invisible.

**Acceptance tests accumulation, not a single slow query.** With `max_services_per_project`
services and `max_members_per_revision` members each, the assertion covers **at least two
consecutive statements that each finish just under the sub-deadline** — the case a single-query
test cannot see — plus a lock-wait path and a rollback path, and asserts that the total elapsed
time of the slice, `BEGIN` through `commit` or `rollback`, is within `max_dispatch_delay` plus an
explicitly stated scheduling tolerance (`max_scheduling_tolerance`, 25ms). It also asserts that
the dispatch tick is never delayed beyond that sum and that no unbounded goroutine or in-memory
queue is created.

### 10.11 DEFAULT-month adoption and its declared bound (amendment, D-0161 / iter-0137)

*Added during phase-1 implementation: the mechanism below emerged from review (iter-0134…0137)
and is normative because its bound and its recovery mode are externally observable.*

The leader pre-creates month partitions ahead of time; when it cannot (leader down across a
month boundary, fresh adoption of old data), inserts land in the DEFAULT partition and are
**never lost — only stranded**. Adoption moves a stranded month into a proper attached
partition with the **parent copy authoritative throughout**: the long phase only COPIES into a
standalone staging table (every parent read keeps seeing every fact), and one short fenced
transaction — `DELETE…RETURNING` of everything still in DEFAULT, imposed on the staging, then
`ATTACH` — runs under the parent's `ACCESS EXCLUSIVE` lock inside ONE absolute
Begin-through-commit budget (5s, capped by the caller's remaining deadline).

The fenced workload is inherently O(rows still in DEFAULT for the month): under native
partitioning, any row removed from DEFAULT before ATTACH is invisible through the parent, so
hole-free incremental shrinking of the fence does not exist. Therefore:

- **The automatic path declares a supported bound**: `adoption_fence_max_rows` = 100 000
  remaining DEFAULT rows per month. The bound is enforced **twice** — a cheap preflight
  before any parent lock, and **again under the `ACCESS EXCLUSIVE` lock** before the sweep,
  because the unlocked count is advisory by construction (a writer can commit between the
  count and the lock; a bound checked only outside the lock is a TOCTOU, not a bound).
- **The bound is a row count, not a wall-clock proof.** A month under the bound on
  pathologically slow storage can still exhaust the 5s fence every cadence.
- **The recovery mode for BOTH cases is one shipped operator artifact**:
  `cerbix adopt-fact-month --config <path> --month YYYY-MM [--timeout 10m]`, run in a
  maintenance window. It executes the SAME adoption code path with an operator-chosen fence
  budget and the row gate off, validates its month input, is idempotent (an attached month is
  a no-op success), and is itself covered by an end-to-end test. There is no documented-but-
  untested psql procedure.
- A refused or persistently failing automatic adoption keeps every fact visible through the
  parent and surfaces through the fact-maintenance metric pair (§21); the refusal names the
  operator command.

## 11. SLO, error budget, burn rate — and two independent coverage axes

### 11.1 The formulas

```
availability = good_duration / (good_duration + bad_duration)
coverage     = (good_duration + bad_duration) / (good_duration + bad_duration + unknown_duration)
error budget = (1 − objective) semantics, reusing internal/sla
burn rate    = multi-window, reusing the existing rule shapes
```

Both are computed from summed durations over the window. `excluded_duration` appears in
**neither denominator**: declared-out-of-scope time is not a measurement failure and not a
measurement success. A zero denominator in either formula yields `unavailable` — **never**
`100%` and never `0×`.

r2 stopped at availability and offered a window whenever its bucket rows existed. That
combination lets the product state a confident lie: 129,600 buckets materialized over 90 days,
129,599 of them entirely `unknown_duration` and one `good`, yields `100% / 90d` from sixty
seconds of measurement. It violates NFR-016 in this document's own words, and the
empty-`sli[]` guard does not catch it, because the SLI is not empty.

### 11.2 Storage continuity and decidable coverage are different questions

- **Storage continuity**: the expected buckets exist, contiguously, through the service's
  `sealed_through` watermark (§10.5). This answers "did we materialize the window".
- **Decidable coverage**: the fraction above, after exclusion. This answers "did we MEASURE the
  window".

Both must pass. Every reliability response carries `as_of`, `sealed_through`, the summed
durations of **both** axes (§9.1), the coverage fraction, and any `repairing` interval. Below `min_decidable_coverage`
(§10.10) the objective, budget and burn rate are returned as `partial` **with the fraction and
the reason**; at zero decidable time they are `unavailable`. Neither is ever rendered as `0×`
or `100%`.

### 11.3 A window ends at `sealed_through`, not at `now`

This follows from sealing and r1 missed it: a rolling window ending at `now` always includes a
provisional tail, so under "fully covered" every window would be permanently partial. The
window is therefore `[sealed_through − window, sealed_through)` and the response says so. The
provisional tail is visible on the timeline and excluded from the number.

Operators still need a live signal, and it is a **different named thing** — a categorical
`current_health` derived from provisional data, explicitly unstable — not a second variant of
the same percentage. A short burn window lying entirely to the right of `sealed_through` returns
`insufficient_sealed_coverage`, not zero burn and not a number derived from one bucket.

`sla_targets` gains `service_id` as a third exclusive scope alongside `monitor_id` /
`project_id` (the existing CHECK becomes a three-way exclusivity). Until phase 5, a
service-scoped `burn_alert_enabled` is rejected at both the schema and the application layer
even though the shared table carries the column.

**Objective changes.** `sla_targets.objective` is mutable and is not part of the definition
revision, so changing 99.9 → 99.99 changes the budget over identical facts instantly. Phase 2
treats the objective as a **current-view parameter**: the returned budget always states which
objective produced it, and a change is annotated on the timeline. Giving the objective its own
effective history is a legitimate future refinement and is deliberately not phase 2 — but
silently reporting a recomputed budget without saying which objective it used is not.

**Reporting only in phase 2.** Service SLOs are computed and displayed but do **not** alert
(§13).

*Amendment (D-0163 / iter-0138):* until phase 5 owns alerting, the displayed burn pair is
**fixed at 1h/6h** (the approved card), each window `[sealed_through − w, sealed_through)`;
a pair whose equivalent real-time window holds no sealed time returns
`insufficient_sealed_coverage`. Hour/day rollups are computed on read as §10.2's exact
epoch-keyed sums; no rollup table is stored in phase 2.

*Amendment (D-0164 / iter-0140):* `current_health` is a **right-continuous point
evaluation** at the DB clock's `as_of`: the declared SLI semantics through the same member
and aggregation rules as the facts, with inputs effective exactly at as_of included and
observations after as_of excluded. No fixed-width window approximates it — freshness
durations are nanosecond-granular, so a derived stale deadline can split any width.

*Amendment (D-0165 / iter-0142):* SLA objectives live in the **open interval (0,100)** at
four canonical decimals (maximum 99.9999), for the service scope AND the pre-existing
monitor scope alike — invariant 46's "existing monitor-level SLO behaviour is unchanged" is
bounded by this correction, because objective=100 was only ever storable by an accident of
numeric precision and the shared burn math answers a zero error budget with 0×. A true
zero-error-budget objective is a separately specified cross-scope non-goal here.

## 12. Presentation

### 12.1 Reporting across revisions and epochs

§6 forbids a single number silently composed of two definitions; §11 defines one number over a
window. The resolution, stated so phase 2 cannot pick something else:

- A window that spans **definition revisions** is returned as **segments**, each with its
  revision, its own availability and its own coverage. No aggregate across a definition
  boundary is offered — not even labelled, because a number that exists gets quoted.
- A window that spans only **evaluation epochs** within one definition revision is likewise
  segmented in the response, but the UI may visually group adjacent epochs with identical
  evaluation semantics. Grouping is presentation; the provenance stays in the payload.
- Hour and day rollups are keyed by epoch and never merged across a boundary of either axis.
- Definition boundaries render prominently on the timeline; evaluation epochs render as thin,
  groupable markers that expand into the audit record. Hiding them entirely was r1's proposal
  and is wrong: an interval, region or target change can make two numbers incomparable, and §6
  forbids splicing across exactly that.
- Segments produced by a first-adoption backfill are marked as declared reconstructions (§6.6),
  and a `repairing` interval is rendered as such rather than as data.

### 12.2 Health vs reliability

Two distinct states, shown as such rather than merged:

```
checkout
  SLI status:  HEALTHY      ← derived from sli[] only
  SLO 99.95%   Budget 72%   Burn 1h 0.4x / 6h 0.6x
  Diagnostics: DEGRADED     ← derived from monitors[]
    redis-latency failing
```

Merging them produces the confusing `DEGRADED / budget 100%` card; separating them tells the
operator precisely that customer-facing availability is intact while something inside the
service is wrong. Labels must make the SLI layer read as the authoritative customer-facing one.

**Public projection invariant:** a public Service status is derived **only** from declared SLI
semantics. A diagnostic monitor can never degrade a public component unless it participates in
the SLI.

**The UI mock** for the phase-1/2 surface, produced before any frontend code and **approved**
(2026-08-16), — services list, service detail, the
coverage and segment states, the declaration editor, and the revision/epoch/provenance history
— is the design-track deliverable recorded in `docs/design/notes.md`. The timelines reuse the
product's existing uptime-signal motif; `UNKNOWN` is a short tick and `PROVISIONAL` is reduced
opacity on the same grid, so neither can be read as a status colour.

## 13. Alerting boundary

Phase 1–2: service SLOs **calculate = yes, display = yes, alert = no**.

Monitor-level burn alerting already exists and pages through the same channels. Turning on
service burn alerts without an ownership rule would page twice for one failure — the exact
noise this specification claims to reduce. Alert ownership (which level is the single source of
paging semantics, and how a monitor in an SLI delegates or suppresses its own burn alert) is
phase 5 and is deliberately unspecified here.

## 14. Dependencies and incident correlation (phase 3)

*Specified to implementable depth in the phase-3 spec cycle (2026-08-17); until then this
section was intent only. Decision record reserved: D-0166.*

Three behaviors must never be conflated:

| Behavior | Source | Effect |
|---|---|---|
| alert suppression | monitor `depends_on` | mutes **delivery** only (exists today, fail-open) |
| incident correlation | service dependencies | **links** facts |
| impact presentation | service dependencies | UI / status representation |

**Invariant:** the service dependency graph never suppresses the recording of facts or the
creation of incidents. It annotates (`root cause probable: payments`, `affected: checkout,
subscriptions`). This preserves the project's existing philosophy — facts always keep
recording, only delivery is muted — and prevents a real second incident from being hidden under
a "root" one. The service graph is the canonical *impact* graph; monitor `depends_on` remains
the low-level *suppression* override; the former may recommend the latter but they are not
required to be identical. (Recommendation tooling is explicitly out of phase 3 — §14.8.)

### 14.1 The graph

`service_dependencies` — one row per directed edge *child depends on parent*:

```
service_dependencies
├── service_id      the dependent (downstream) service
├── depends_on_id   the dependency (upstream) service
├── project_id
├── created_at / created_by (audit)
PK (service_id, depends_on_id) · CHECK (service_id <> depends_on_id) · index on depends_on_id
FK (service_id,    project_id) → services (id, project_id) ON DELETE CASCADE
FK (depends_on_id, project_id) → services (id, project_id) ON DELETE CASCADE
```

Both composite FKs share ONE `project_id` column, so **same-project is enforced by the
schema**, not by a query that a later writer can forget (the 00060/00061 tenant pattern).
Cross-project impact is out of scope exactly as it is for monitor dependencies.

The graph is a DAG by validation at write time: a self, direct or transitive cycle is a 400;
traversal depth is capped at 10, and hitting the cap **rejects the write and names the cap** —
a silently truncated cycle check would be a cycle check that lies. Direct upstream edges per
service are bounded by `max_service_dependencies` = **20, fixed in phase 3** (the
`min_decidable_coverage` pattern: a bound the operator cannot lower away); the write is a
replace-set, and an oversized set is a 400 naming the bound.

**Concurrency.** Two concurrent edge writes, each acyclic against the state it read, can
compose a cycle. Edge writes therefore serialize on a **per-project `service_graph` advisory
xact lock**, taken by every edge-mutating path — enumerated, not implied: **service
create-with-edges, the UI replace-set (§14.2), format-2 apply's edge track, and service
delete** (whose cascade mutates the graph) — one store path, as with the shared validator of
§15.2. In the §15.4 order it sits immediately after the `service_membership` advisory lock,
always `membership → graph → row classes`; no path takes them in the other direction. The
cycle check (recursive CTE, cap 10) runs inside the same transaction and is sound because of
the lock, which a concurrent-writers test asserts (two edges individually acyclic, jointly a
cycle — exactly one commits). The cap is pinned at the boundary: depth 9 and depth 10 chains
are valid, and the first write that would require depth 11 is the rejected one, so "cap hit"
is a tested value and not a phrase.

### 14.2 Edges are OUTSIDE the declaration

§6.2 already rules: *dependency-only edits create no epoch*; §6.3 classifies *dependency
wiring* out of the evaluation-semantics projection — delivery, not measurement. Phase 3 takes
the same position for the declaration axis:

- Edges are **not part of `DefinitionRevision`** and do not enter the canonical service hash.
  An edge write creates no definition revision, no epoch, moves no `sealed_through`, and
  changes the meaning of no stored number. Required regression: an edge-only mutation leaves
  revision count, epoch count and every canonical hash byte-identical.
- **Mutation authority is the service's owner** — the UI for a UI-owned service, the bundle
  for a file-owned one. This is the plain ownership rule, not the §15.1 matrix: the matrix
  protects *declarations*, and an edge is not one.
- **The transport is a dedicated pair of routes** — no generic service-update endpoint exists
  and none is invented for this:
  `GET /api/v1/projects/{projectID}/services/{serviceID}/dependencies` returns the edge set
  plus **`graph_generation`**, a per-service edge-set counter; and
  `PUT .../services/{serviceID}/dependencies` takes `{depends_on: [service_id],
  graph_generation}` as a replace-set. Edges are outside the declaration, so the existing
  `expected_revision` CAS cannot protect them; `graph_generation` is their own concurrency
  token: a mismatch is a **409** and the two-operators lost-update (`[A]` read twice,
  `[A,B]` and `[A,C]` submitted) resolves as first-committer-wins, second told so. A no-op
  replace-set (identical set) bumps nothing and audits nothing.
- **Every non-no-op delta writes a tenant-scoped audit row in the SAME transaction** — TYPED
  actor attribution in the canonical columns (`actor_user_id` for humans and token
  principals, `via_token`, and a human-readable label — an email, a token name, or the
  provider id — in the target), plus added and removed edges (bounded lists) — because
  `created_by` on surviving rows cannot testify about a removal: deleting an edge deletes
  the only row that knew who made it. The audit row is what remains. *(Attribution typed per
  review [276] P1-4: an opaque string in the target would leave a human edit actorless in
  the audit API.)*
- **Bundle format 2** gains an optional per-service key `depends_on: [service-slug]` —
  references are service slugs, tenant-scoped, and MAY point at a UI-owned service (the
  cross-owner reference rule of §15.2 for monitors, applied unchanged). Edges reconcile as a
  **separate track** from the declaration on apply, through the SAME validator and mutator as
  the UI route under the provider's owner authority (replace-set under the `service_graph`
  lock, same audit rule, actor = the provider): an edge-only bundle change mutates edges and
  creates no revision — the exact analogue of "moving a file may update provenance
  separately". `depends_on` does **not** enter the canonical hash; the no-op rule of §15.2 is
  therefore undisturbed.
- **A desired edge pins its target — one deletion contract, the §15.1 shape.** An incoming
  bundle whose `depends_on` slug resolves to nothing is **rejected at validation and the
  provider keeps last-known-good** — fail-fast on author error, and LKG stays literally true
  because a target is never allowed to vanish underneath an applied desired state: a UI (or
  API) delete of a service named in any file-owned service's applied `depends_on` returns
  **409 naming the provider and the referencing service**, until the bundle drops the slug.
  One provider's desired state may remove its own target and the edge atomically; a removal
  that would dangle ANOTHER provider's desired edge freezes exactly as §15.1 row 4 freezes
  cross-provider dangling references. (An earlier draft allowed the delete and let the
  provider "freeze with last-known-good" — incoherent, since the cascaded edge's target no
  longer exists and no good state remains to keep; the guard is the version of this rule that
  can be true.) **An ORPHANED managed service still pins** ([276] P1-2): MaC deliberately
  preserves an absent-from-bundle service as file-owned last-known-good, so its desired edges
  are exactly the state the pin keeps restorable — excluding orphans would let a target
  delete break LKG literally. Deletion tests observe graph state immediately after the delete
  attempt and after the next reconcile, not merely the apply verdict.
- Deleting a service (when permitted by the rule above) cascades its edges (both directions)
  and its impact links (§14.3); already-written 🕸 timeline notes are immutable text and
  remain. UI-owned edges never block anything: only an applied file desired-state pins.

### 14.3 Correlation — symmetric at open, event-driven, best-effort

**Anchor.** An incident participates in correlation iff it has a `monitor_id` (every auto
incident; a manual one only if it is monitor-anchored). Its **own services** are the services
whose current effective `monitors[]` contains that monitor — operational membership (§5), not
`sli[]`: an incident on a diagnostics-only member is operationally that service's incident,
and correlation is annotation, not measurement. Membership resolves **at delivery time**
against the current effective definition revision; correlation runs only at open-time events,
so the drift window is the delivery lag and is accepted and stated (no retrospection exists
that it could corrupt).

**The relation.** `incident_service_impacts` — structured links, the §14 table's "links facts"
made a table. Tenancy is enforced by the SCHEMA on **both** ends, the §16 composite rule with
no exception for a link table:

```
incident_service_impacts
├── incident_id
├── service_id
├── project_id    ONE shared column under BOTH composite FKs
├── role          probable_root | affected
├── path          canonical ROOT-FIRST slug path (§ below), endpoint-inclusive,
│                 bounded text[], max length depth cap + 1 = 11
├── computed_at
PK (incident_id, service_id, role)
FK (incident_id, project_id) → incidents (id, project_id) ON DELETE CASCADE
FK (service_id,  project_id) → services  (id, project_id) ON DELETE CASCADE
```

`incidents` gains `UNIQUE (id, project_id)` in the same migration (the 00060/00061 target
pattern — it does not have one today), and `incidents.monitor_id` — which this section
promotes from a UI convenience into a background-query anchor — is **hardened to the composite
`(monitor_id, project_id) → monitors (id, project_id)`** so a cross-project anchor is
unrepresentable, not merely unqueried: today only the API checks that membership, and a
background writer bypasses the API. Required negative tests at BOTH layers: a direct-SQL/store
attempt to link an incident in project A to a service or monitor in project B fails on the
constraint, and the API never exposes another project's slug, name or path inside an
authorized incident payload. These tables and the correlation query join §16's enumeration.

**The attempt snapshot.** Membership, the graph and the witness set are read under the
project's `service_membership` → `service_graph` advisory locks (the §15.4 order; the
incident row locks below come later, the same direction on every path), so every fact an
attempt encodes existed together in ONE committed state — separate READ COMMITTED reads
could mix an already-deleted edge with a witness that only committed after the deletion.
*(Added during phase-3 implementation, review [276] P1-1; proven by two barrier
regressions: a graph replace and a membership change each committing while the attempt
demonstrably waits on the lock, with the attempt then seeing the post-commit state.)*

**The witness bound.** Witnesses are bounded per REACHABLE ENDPOINT SERVICE — the anchor's
ancestors and descendants in the locked snapshot, and nothing else: the oldest
`max_correlation_witnesses_per_service` = **5, fixed** open monitor-anchored incidents by
ascending `(started_at, id)` — a deterministic function of the committed state at attempt
time, so a redelivery against unchanged state selects the identical set. Selection is per
endpoint (one incident may be within the bound for one of its services and beyond it for
another); the ANCHOR is the attempt's subject, never a witness, never counted. The witness
READ is scoped to those endpoints and capped IN SQL (per-endpoint `LIMIT`, totals from an
aggregate), so an attempt's row transfer is bounded by `endpoints × cap` and never by the
project's open-incident count; an attempt with no reachable endpoint reads no witnesses at
all. SERVICE-level completeness is unchanged — a reachable service with any open incident
still gets its probable_root row; the bound truncates witness LISTS, capping the attempt's
lock and write set by construction (≤ 1 + 5 × reachable services). Overflow counts ONLY
omitted witnesses of reachable endpoints — an unrelated service's incident pile is neither
an omission nor telemetry — and is returned by the attempt, logged by the worker and counted
(`§14.6`), never silent. *(Added during phase-3 implementation, review [276] P1-3 under the
[278] conditions accepted [280]; reachable-endpoint scoping per the [283] final-round
disposition — project-wide witness reads were both an unbounded read and false omission
telemetry.)*

**When — a dedicated durable topic, not a rider.** Correlation gets its own outbox topic,
`incident_correlation`, enqueued in the SAME transaction that creates a monitor-anchored
incident's `opened` state (the existing spine; non-anchored incidents enqueue nothing). It
does NOT ride the `incident_event` webhook delivery: that branch returns on webhook failure
before any rider runs, so a dead webhook would dead-letter correlation with it — and making
delivery wait on correlation would invert the harm. Two topics, two failure envelopes:
**webhook death never blocks correlation; a correlation failure never blocks incident
delivery.** Both inherit the outbox worker's retry/backoff/dead-letter unchanged. (Change
pattern: topic whitelist migration + a worker case + the interface-growth fakes, per the
standing gotcha.)

**Mixed-version activation — the claim fence, over the WHOLE lifecycle.** A new topic is not
deployable by whitelist alone: `ClaimDueOutbox` claims every due row `WHERE status = 'pending'`
with NO topic predicate, and core delivery is owned by `all`, `api` AND `scheduler` — so during
a rolling deployment an old owner claims the new topic's row, falls through to `unknown outbox
topic`, burns an attempt per claim and can park a durable correlation fact as dead before any
capable owner reaches it. And enqueue-time fencing alone is a fence with three gaps, because
the row's status is REWRITTEN downstream: a failed attempt below max writes `status='pending'`
(`FailOutbox`), and both dead replays — single and replay-all, reachable through an OLD API
replica's admin endpoints during the mixed fleet — write `status='pending'` too. A fence that
only holds until the first transient failure is not a barrier.

The barrier is therefore TWO schema pieces, covering every transition:

- **An immutable class column.** `fenced boolean NOT NULL DEFAULT false`, set once at enqueue
  (`true` for `incident_correlation` and every post-phase-2 topic) and never modified by any
  transition — it survives claim, failure, dead and replay.
- **A demotion CHECK.** `CHECK (NOT fenced OR status <> 'pending')`: a fenced row may be
  `pending_fenced`, `delivered` or `dead`, but the legacy-claimable state is UNREPRESENTABLE
  for it. The old replay shape (`SET status='pending' WHERE ... status='dead'`) executed
  against a fenced row **fails closed on the constraint** — an error, never a silent
  demotion. (Old `FailOutbox` needs no guard beyond this: an old owner can only fail a row it
  claimed, and it can never claim a fenced one.)

The phase-3 transitions read the column as the ONE source of truth for the claimable class:
`ClaimDueOutbox` claims `pending` rows unconditionally (legacy topics, known to every owner)
plus `pending_fenced` rows **whose topic is in its own dispatch set**; a failed attempt below
max restores `CASE WHEN fenced THEN 'pending_fenced' ELSE 'pending' END`; both replays restore
the same expression; delivery marks `delivered` regardless of class. The topic→class mapping
at ENQUEUE lives in one store-side map and is pinned by test — that mapping is code, not
schema, and the spec claims schema force only for what the schema enforces: **once a row is
fenced, no transition, no old binary and no operator endpoint can ever make it
legacy-claimable.** A fenced row enqueued while old owners still run simply WAITS —
at-least-once latency, never loss — and a capable owner always eventually exists because
every producer of the topic is itself a core-delivery owner. Migration: the status CHECK
widens with `pending_fenced`, the `fenced` column and demotion CHECK are added, and the
partial claim index covers the fenced class.

Required mixed-version regressions: the legacy claim shape against a fenced row claims zero
rows and changes zero attempts, then a capable owner delivers it; a capable claim followed by
ONE forced failure leaves the row `pending_fenced` (old claim still zero) and the capable
retry delivers; a dead fenced row through single replay AND replay-all stays fenced (old claim
still zero); the legacy replay SQL against a dead fenced row is REJECTED by the schema; an
ordinary legacy-topic retry/replay still round-trips through `pending` unchanged; and the
`incident_correlation` enqueue is pinned to `fenced=true` so a refactor cannot demote the
producer.

**What.** On the correlation attempt for incident `I` with own services `S`:

- *upward*: every service `A` reachable upstream from `S` (cap 10) that has an OPEN
  monitor-anchored incident `J` right now → insert `(I, A, probable_root, path)` **and the
  mirror back-fill `(J, S', affected, path)`** for the `S' ∈ S` the path ascends from.
  *(Added during phase-3 implementation: without the mirror, the child-first interleaving
  leaves the parent incident without its affected row, making relation CONTENT depend on
  opening order — the symmetry of this section is about content, not merely observation.)*
- *downward*: every open incident `J` on a service `D` reachable downstream from some
  `S' ∈ S` → insert `(I, D, affected, path)`, **and** back-fill `(J, S', probable_root,
  path)` for that same `S'` — the service the path to `D` descends from, carrying the SAME
  root-first array (there is no "reverse path": the stored direction is fixed and the role
  says which endpoint is the row's own service). The late-root race is closed by symmetry,
  not by a retrospective sweep.

`probable_root` marks **every** upstream service on a path with an open incident; the relation
records candidates and their paths, it does not elect a single culprit — ranking (by path
depth) is presentation.

**The canonical path.** A diamond admits two paths between the same pair, and the PK holds one
row, so the stored path must not depend on CTE row order or query plan. The path is computed
deterministically BEFORE insert: **shortest wins; equal lengths tie-break lexicographically on
the immutable-slug sequence**. Shape, stated exactly so no convention is implied: the array is
**endpoint-inclusive and root-first** — `[upstream endpoint, …, downstream endpoint]` — so a
direct edge stores 2 slugs and a maximal depth-10 chain stores **11**, which is the schema
bound (`depth cap + 1`); a 10-slug bound would silently drop an endpoint and make "reversal"
ambiguous. Stored and wire type: `text[]` of slugs. Required regressions: the diamond, the
equal-length tie, the direct-edge 2-slug and depth-10 11-slug boundaries, the back-fill row
carrying the identical array as its `affected` counterpart, and byte-identical relation
content across a redelivery.

**One transaction, or nothing.** The correlation attempt is a single store transaction on the
delivery side:

1. lock the anchor incident row and every counterpart incident row found, `FOR UPDATE` in
   ascending id order;
2. recheck every locked incident is STILL open — "open" read before the lock is a
   check-then-act, and a link or note landing in a just-resolved incident would rewrite closed
   history (invariant 56);
3. insert the link rows, `ON CONFLICT DO NOTHING`;
4. for each incident that gained at least one NEW row in step 3, insert its 🕸 note — in this
   same transaction, so "links committed but the promised note can never appear" (every retry
   ON CONFLICTs away) and "note without links" are both unrepresentable;
5. commit, or none of it happened and the outbox retry re-attempts.

**Symmetry — qualified to what the mechanism guarantees.** A correlation attempt starts only
after its incident's row committed (same-transaction enqueue). Take incidents I and J on
graph-related services, both still open at the moment either's attempt commits: if I's attempt
read before J's commit, then J's attempt — which starts after J's commit, which is after I's
commit — reads after I's commit and sees I. Both attempts missing each other is impossible;
at-least-once redelivery only widens the union. An incident **resolved before any authoritative
attempt reaches it is skipped by design** — that is the no-retrospection rule (invariant 56),
not a missed link. Both interleavings (child-first, parent-first) are required tests, per the
§6.2 discipline, plus: webhook-dead-but-correlation-delivered, resolve-racing-correlate (the
step-2 barrier holds), note-insert-failure rolls back the batch, and redelivery inserts zero
rows and zero notes.

**Idempotency.** Step 3's `ON CONFLICT` plus step 4's newly-inserted condition make the whole
attempt idempotent: a redelivery inserts nothing and writes nothing; a genuinely new late root
inserts new rows and earns a new note, which is a new fact at a new time, not a duplicate. Note
rendering is bounded: at most 8 service names per role, then `+N more` — the relation stays
complete, only the prose truncates, and the truncation names its remainder.

### 14.4 API

- **Impacts are an authenticated-detail enrichment, never a field of the shared incident
  model.** The incident DETAIL payload (authenticated, project-scoped) gains
  `impacts: [{service_id, slug, name, role, path, computed_at}]`; the incident LIST carries
  **no impacts field in phase 3** — the list endpoint returns unbounded project history today,
  and multiplying it by the per-incident impact set is the read amplification §14.7 forbids.
  The field is populated by the authenticated handlers only, NOT added to `domain.Incident`
  where the status-page serializer would inherit it: the public path embeds the incident model
  and redacts by allowlisting-in-reverse (`PublicRedacted`), so a shared field would ride into
  unauthenticated JSON the moment one future redactor forgets it. **Public and internal
  status-page projections carry no impacts until phase 4 explicitly opts in** (§17: existing
  status pages unchanged; internal topology is not public data). Required regression: the raw
  UNAUTHENTICATED status-page JSON for a page whose project has correlated incidents contains
  no impact service ids, slugs, names or paths.
- Edge reads and writes use the dedicated `/dependencies` routes of §14.2 (with
  `graph_generation`); service create additionally accepts `depends_on` for create-with-edges
  under the same validator and lock. The edge READ returns the token and both directions from
  ONE SQL snapshot, so the token always names the returned set — split reads could straddle a
  concurrent replace and earn the next PUT a spurious 409 ([276] P2-1).
- **Every impact read is tenant-scoped at the owning data boundary** ([276] P0-1): the store's
  impact listing takes the project id and predicates on the LINK rows' own `project_id`, so a
  foreign incident id yields nothing even under a buggy handler-level access check; the
  handler's `incidentAccess` check remains on top. The §16 negative tests cover this store
  boundary directly.
- The service detail payload gains `dependencies: {upstream: [{service, health}],
  downstream: [...]}` with health from the phase-2 two-layer signal — read in **one snapshot
  with a CONSTANT number of set-wise statements** (scope+epoch, observations, maintenance
  spans, diagnostics), never a per-service `ServiceHealthNow` loop (§14.7). Semantics keep
  ONE owner: the batched read feeds the same four relations to the same pure evaluator; only
  the loading is set-wise. *(The neighbour set is bounded by the direct-edge cap upstream but
  by the project service cap DOWNSTREAM — a popular upstream can be depended on by every
  other service — so "bounded by 2× the edge cap" is false and the statement count, not the
  set size, is what the bound has to be about: [288] P1-2.)*
- **The UI/API edge write obeys ownership** (§14.2): the replace-set path refuses a
  file-owned service with `managed_by_file` — active or orphaned — exactly as
  `PutServiceDeclaration` does, so a UI PUT can never overwrite a provider's applied desired
  edges. The provider track calls the shared mutator directly under its own authority.
- **The edge request contract is strict**: `depends_on` and `graph_generation` are both
  REQUIRED and presence-checked (an omitted array is not an empty replace-set, an omitted
  token is not a passing zero), `[]` is the legitimate way to clear the set, and every id in
  the path or the body is validated at transport — a malformed uuid is a 400, never a store
  cast that surfaces as 500.
- **A failed impact read is disclosed, not disguised**: the authenticated detail returns
  `impacts: null` with `impacts_unavailable: true`, because an empty array is the honest
  statement "this incident has no links" and a degraded read must not borrow it ([288] P1-4).
- `openapi.yaml` bump + regenerated `schema.d.ts`, per the standing contract.

### 14.5 UI (phase-3 scope: lists and badges — no visual graph)

Approved scope, mock-gated before any frontend code (§22): a "Depends on" multiselect in the
service form (project's services minus itself; cycle → the API's 400 rendered verbatim);
Upstream/Downstream blocks with health dots on the service detail; 🕸 `probable root` /
`affected` chips on the incident detail linking to the named services; the 🕸 timeline note
renders through the existing system-update mechanism, unchanged.

### 14.6 Observability

`cerbix_service_impact_links_total{role}` (counter, incremented on INSERTED rows only),
`cerbix_service_impact_correlation_failures_total` (best-effort failures — the WARN path made
countable), and `cerbix_service_impact_witness_overflow_total` (open incidents beyond the
per-service witness bound — a deterministic durable-fact omission that must be visible; plain
counter, NO tenant/project/service labels). No gauge: the relation is queryable and bounded.

### 14.7 Bounds (§21 discipline)

| Bound | Value | Settable | Why |
|---|---|---|---|
| `max_service_dependencies` | 20 direct edges | no (phase 3) | a graph a human can still read |
| traversal depth cap | 10 | no | mirrors monitor graph; cap hit rejects, never truncates; pinned at 9/10/11 |
| stored `path` length | ≤ depth cap + 1 = 11 slugs, endpoint-inclusive, root-first | no | a depth-10 chain has 11 endpoints; a 10-slug bound would drop one silently |
| `max_correlation_witnesses_per_service` | 5 open anchored incidents, oldest by (started_at, id) | no | caps the attempt's lock/write set; overflow returned, logged and counted — never silent |
| 🕸 note names per role | 8 + `+N more` | no | bounded prose over a complete relation |
| impacts on incident LIST | none (detail only) | no | the list is unbounded history; list × impacts is unbounded² |
| dependency health on service detail | ONE batched snapshot query | no | per-neighbour `ServiceHealthNow` is an N+1 at the project service cap |

Read-side work bounds are TESTED, not asserted: the incident-list handler is pinned to emit no
impacts and no per-row impact query; the service-detail dependency block is pinned to a
constant query count at the maximum neighbour set.

### 14.8 Out of phase 3

Recommending monitor `depends_on` from service edges ("may recommend", deliberately unbuilt);
any visual graph rendering; status-page projection of impact (phase 4 DECLINED it — §15.0 keeps impact links non-public; a later phase would need its own owner decision); alerting on impact
(phase 5); any retrospective annotation of resolved incidents; cross-project edges.

## 15. Coexistence and migration

**Status pages.** Service projection becomes the default *managed* component type; **manual
components remain first-class** (the "third-party provider: degraded" case that Cerbix does not
monitor). Existing components are never converted automatically: an explicit "convert to
Service-backed component" action with a preview of the resulting public change — status pages
are customer-visible artifacts.

*Specified to implementable depth in the phase-4 spec cycle (2026-08-17); until then this was
intent only. Decision record reserved: D-0167.*

### 15.0 The status-page projection (phase 4)

**Tenancy first: `components` currently carries NO tenant column at all.** It reaches its org
only through `status_pages`, so a `service_id` alone would bind service→project and leave
component→org unbound: a direct writer could hang an org-B service on an org-A page. Migration
therefore gives the row its own tenant identity, mirroring `status_pages` (org-level page ⇒
`project_id IS NULL`):

```
components
├── org_id          uuid NOT NULL      (backfilled from status_pages)
├── source_project  uuid NULL          the project of the BINDINGS — not of the page
├── source          text NOT NULL      'monitor' | 'service' | 'manual'
├── monitor_id      uuid NULL          the monitor binding, retained while dormant
├── service_id      uuid NULL          the service binding, retained while dormant
├── manual_status   text NOT NULL DEFAULT ''
FK (status_page_id, org_id)     → status_pages (id, org_id)
FK (source_project, org_id)     → projects (id, org_id)
FK (monitor_id, source_project) → monitors (id, project_id)   ON DELETE SET NULL (monitor_id)
FK (service_id, source_project) → services (id, project_id)   ON DELETE RESTRICT
CHECK  the ACTIVE source's binding is present:
       source='monitor' ⇒ monitor_id IS NOT NULL
       source='service' ⇒ service_id IS NOT NULL
       source='manual'  ⇒ (nothing required)
CHECK  source <> 'manual' ⇒ source_project IS NOT NULL
CHECK  manual_status <> 'no_data'
CONSTRAINT TRIGGER (deferred, on components and on status_pages.project_id):
       a PROJECT-SCOPED page admits only components whose source_project is that project;
       an ORG-LEVEL page admits any project of the org
```

Three corrections the reviewer was right to force, each of which I had conflated:

- **Page scope and source project are different things.** An org-level page legitimately holds
  components from several projects, so the row's project column describes its BINDINGS
  (`source_project`), never the page. A manual component has no binding and therefore no
  project — hence nullable, with the CHECK making it non-null exactly when a binding is active.
- **"A project-scoped page's component carries that project" is not expressible as a CHECK** —
  a CHECK cannot read `status_pages`, and the `(status_page_id, org_id)` FK proves org only. It
  is a DEFERRED CONSTRAINT TRIGGER, on both sides (inserting a foreign component, and narrowing
  a page's scope afterwards), and it is named as such so implementation cannot quietly downgrade
  it to an application check.
- **Both dormant bindings live in ONE project, so conversion is same-project by rule.** A
  component may not be converted from a monitor in project A to a service in project B; the
  operator creates a different component instead. Per-binding project identity was the
  alternative and it buys nothing: a component IS one thing on a page, and a cross-project
  "revert" would restore a binding the page's audience never saw. Direct-SQL negatives cover
  all three: cross-org component, foreign component on a project-scoped page, and cross-project
  dormant binding.

**The source is a DISCRIMINATOR, not the presence of a field.** An exclusivity CHECK over
"which column is populated" cannot coexist with the reversibility this section promises: a
revert has to restore the binding it replaced, so the binding must survive being inactive.
`source` names the active one; the other columns keep their bindings **dormant**. That single
mechanism makes every transition reversible without a second staging table:

| From | To | What happens | Revert restores |
|---|---|---|---|
| manual | service | `source='service'`, `manual_status` untouched | the same manual status |
| monitor | service | `source='service'`, `monitor_id` retained dormant | the same monitor binding |
| service | manual | `source='manual'` | — |
| service | monitor | `source='monitor'` (only if a dormant `monitor_id` survives) | — |
| monitor | manual / manual | monitor | unchanged from today | — |

Backfill for shipped rows is total and deterministic: `monitor_id IS NOT NULL ⇒ 'monitor'`,
otherwise `'manual'` — including rows with neither binding nor status, which are manual
components an operator has not set yet (they render exactly as they do today).

**One inherited behaviour is kept, and one is refused.** A monitor-backed component whose
`manual_status` is set currently renders the manual value as an override (D-0021); that stays,
because changing it would alter shipped public output for no reason this phase needs. A
**service-backed component takes no manual override**: a service is the measured unit, and
letting an operator hand-paint its public status would publish the unmeasured as measured —
the one thing this whole feature exists to prevent. Operators express judgement through
incidents, which is what incidents are for. `manual_status = 'no_data'` is likewise refused by
CHECK: `no_data` is a COMPUTED statement that measurement is absent, never an assertion someone
types.

**Only the SLI layer is projected.** The two-layer signal of §12.2 exists because a
diagnostics-only failure must never touch the customer-facing layer; publishing the
diagnostics layer would also publish monitor NAMES, which is internal topology. So the public
projection reads the SLI layer alone, and **the impact links of §14 stay non-public** —
invariant 59 allowed phase 4 to opt in explicitly, and phase 4 explicitly declines: a public
page naming a probable-root service would publish the dependency graph to customers.

**The projection needs an input the live signal does not currently expose.**
`ServiceHealthNow` deliberately collapses "excluded by a maintenance window" and "genuinely
unknown" into one `sli: unknown` — the four declared categories, nothing invented (§12.2). A
projection that maps `excluded → maintenance` cannot read that; inventing a second evaluator
would give the hardest-reviewed rule of phase 2 a second owner. So phase 4 adds ONE authoritative
batched projection read, at ONE database `as_of`, over the whole page's service components:

```
ServiceStatusProjection      (ONE report snapshot per page render; CONSTANT statement count
                              regardless of component count — not necessarily one statement)
├── service_id
├── sli            healthy | degraded | down | unknown
├── excluded       true when a maintenance exclusion is in force AT as_of
├── reason         the payload's own reason for a non-measured sli (never prose invented here)
├── sealed_through the service's watermark (the 90-day window ends HERE, not at as_of)
└── sealed_in_window  whether a sealed fact exists INSIDE the 90-day window
```

`sealed_in_window`, not "any fact exists": a single ancient fact would otherwise claim history
the strip cannot show. And the window ends at `sealed_through`, matching the internal report
(§11.3) — ending it at the page's `as_of` would invent an unsealed tail nobody measured.

**A missing service is an ERROR, not an honest `no_data`.** With the tenant FK and `RESTRICT`,
an active service source CANNOT be absent; if the projection comes back without it, the read is
wrong — a query omission, a tenancy bug, corruption. So that case degrades the render (the
component reports unavailable and the failure is logged and counted), and never converts into
the calm public statement "we have no data about this". Absence of a row and absence of
measurement are different facts.

It reuses the existing evaluator (`reliability.StateAt` over the same four relations,
`serviceNeighbourHealthTx`'s set-wise loading shape) and returns the excluded flag the internal
card folds away — the same numbers, one extra bit, no second semantics owner.

**Precedence is total and stated in order.** The first matching row wins:

| # | Condition | Public component status |
|---|---|---|
| 1 | `excluded` (maintenance window in force at `as_of`) | `maintenance` |
| 2 | `sli = down` | `major_outage` |
| 3 | `sli = degraded` | `degraded` |
| 4 | `sli = healthy` | `operational` |
| 5 | `sli = unknown` · no `sli[]` declared · service absent | **`no_data`** (new) |

Two consequences the reviewer was right to demand in writing:

- **`sealed_in_window` does not affect the STATUS.** A service that is live-healthy before its first
  bucket seals is `operational` — its current state IS measured — while its 90-day strip is
  absent, because its history is not. Conflating the two would have made a healthy new service
  publish `no_data`, which is false in the opposite direction.
- **Maintenance outranks absence.** A service inside a declared window publishes `maintenance`
  even if nothing is sealed and even if the SLI is unknown: the operator declared that window,
  and it is the more specific true statement.

**`no_data` is a first-class public status, and it does NOT join the severity ladder.** The
existing order is `operational(0) < maintenance(1) < degraded(2) < partial_outage(3) <
major_outage(4)`, and squeezing absence into it forces a false comparison — is not-knowing
better or worse than declared maintenance? Neither: it is a different KIND of statement. So the
summary is computed as two values instead of one:

```
summary            ComponentStatus   worst-of over MEASURED components; `operational` when
                                     none are worse; `no_data` when NOTHING is measured
                                     (all-no_data page, or empty page)
unmeasured_count   integer           components whose status is no_data
summary_state      enum              operational | impaired | no_data | empty
                                     (the headline discriminator; `impaired` covers every
                                      measured status worse than operational)
```

`summary` stays a `ComponentStatus` so existing clients keep parsing the field they already
read; `summary_state` and `unmeasured_count` are additive. The summarizer is ONE total function
and it is **fail-closed by construction**: it classifies each component as measured-with-a-known
status or unmeasured, and an unrecognized enum value counts as UNMEASURED — never as severity 0.
That replaces the "`severity()`'s default becomes no_data" phrasing, which was not implementable
(the function returns an int); the ordering function keeps its five measured values and the
unknown case never reaches it.

The headline contract, total over every case:

| Page contents | Headline |
|---|---|
| all measured, worst = operational | *All systems operational* |
| measured worst = X (≠ operational), any `unmeasured` | X's own headline, **plus** "data missing for N components" |
| measured worst = operational, `unmeasured` > 0 | *Operational, with no data for N components* — never plain "all systems operational" |
| every component `no_data` | *No data* — not operational |
| **no components at all** | *No components configured* — not operational |

**Phase 4 changes public output in THREE places, and all three are inherited lies of the same
shape.** §17 enumerates them; none is a side effect of a new feature:

1. a `pending` monitor component rendered `operational` → renders `no_data`;
2. an EMPTY page rendered `operational` (`SummaryStatus(nil)`) → reports "no components
   configured";
3. a MANUAL component whose `manual_status` is empty rendered `operational` (the handler's
   `status := domain.CompOperational` default) → renders `no_data`. An operator who has not
   stated a status has not stated health, and the page must not state it for them.

The third is the one an existing installation is most likely to notice, which is precisely why
it is named rather than shipped quietly: an operator who wants the old appearance sets the
manual status explicitly to `operational`, which is now the only way to say it.

A `no_data` component is always LISTED and named. Hiding it would be the same lie by omission
this feature rejects for composites (§15.5).

**This corrects an existing FR-012 defect, deliberately and visibly.**
`domain.ComponentStatusFromMonitor` maps `pending` — a monitor that has never been confirmed
either way — to `operational`, so today's public pages already present the unknown as healthy.
Phase 4 maps it to `no_data`. That is a CHANGE IN PUBLIC BEHAVIOUR of a shipped feature, so it
is recorded here, in §17, and in D-0167 rather than slipped in as a detail of a new one.

**The 90-day history: both fields, both bounded.** The existing `ComponentView` publishes
`uptime_90d` AND `daily[]`, so specifying only the strip would leave the aggregate to be
improvised:

- **`daily[]`** — UTC days aggregated from SEALED `service_reliability_buckets` (provisional
  excluded), over `[sealed_through − 90d, sealed_through)`, at most 90 points. Each point
  carries `date`, `uptime`, and `decidable_fraction` — DECIDABLE coverage, the §11.2 axis, not
  storage continuity, because that is the axis that says whether the number may be quoted at
  all. A day with no sealed bucket is **absent from the array**, never a zero and never an
  interpolation; a partially decidable day is present with its fraction, so a reader can tell a
  quiet day from a half-measured one. No sealed data in the window → no array and the
  `no data yet` label.
- **`uptime_90d`** — follows the internal report's honesty rules verbatim (§11.2/§11.3): below
  `min_decidable_coverage` it is **withheld**, and a window spanning definition revisions is
  withheld too, because a single number across two definitions of availability is the confident
  lie §12.1 forbids. Withholding is expressed on the wire as `uptime_90d: null` plus
  `uptime_withheld_reason` (`insufficient_decidable_coverage` | `spans_definition_revisions` |
  `no_sealed_data`) — a null with no reason would be indistinguishable from a serialization
  slip. It is never synthesized to fill the field.
- **Bounds are load-bearing here, not hygiene — and "keep rendering + log" is not a bound.**
  The public render is unauthenticated and already N+1 over components; a service component
  multiplies that by 90 days of facts. A create-time cap does nothing about a page that is
  already enormous, so the contract has three layers instead of one:

  1. **Constant statement count** per render under ONE snapshot, regardless of component count
     (the §14-proven shape) — necessary, not sufficient: statements are bounded while rows,
     memory and time are not.
  2. **A per-page ceiling that can only shrink.** `status_pages.component_ceiling` is persisted
     as `max(50, current component count)` at migration. New components are refused above it, so
     an oversized page cannot grow, and every remediation lowers the ceiling permanently.
  3. **An absolute fail-closed safety ceiling** of `500` components for the PUBLIC render. Above
     it the page does not render a truncated subset — a partial page pretending to be the whole
     one is the lie this section exists to prevent — it returns an explicit
     `status_page_over_safe_limit` response naming the count and the limit, while the
     AUTHENTICATED management view still lists everything so the operator can fix it. Plus a
     short-TTL render cache as the rate bound.

  Acceptance requires evidence of bounded ROWS, MEMORY and TIME at the ceiling — not only a
  constant SQL statement count — and nothing is ever silently truncated or hidden.

Same owner of truth as the internal report throughout: a customer and an operator can never be
shown two different availabilities for one service.

**Public incidents and maintenance: the existing scope, stated rather than assumed.** A
monitor-backed component today adds its project to the page's set, and the page then publishes
that project's incidents and maintenance windows. A service-backed component does **exactly the
same** — its project joins the same set, nothing narrower and nothing wider. This is written
down because "impact links stay non-public" does not answer it: an unrelated incident in that
project appears on the page, as it already does for monitor components, and an operator adding
a service component must know that. A raw unauthenticated JSON regression pins it, so the
boundary cannot drift silently in either direction.

**Conversion is explicit, previewed, audited and REVERSIBLE.**

```
Convert  manual  → service:  source='service', service_id = X,  manual_status PRESERVED
Revert   service → manual:   source='manual',  service_id RETAINED (dormant),
                             the same manual_status returns
```

The revert does NOT clear `service_id` — that would contradict the dormant-binding model above
and throw away the affordance to convert back without re-choosing the service.

The preview states, per affected component and for the page summary, what customers see now
and what they would see after — because a status page is a customer-visible artifact and an
operator who cannot see the delta cannot consent to it. Both directions write an audit row
with the actor **in the mutating transaction**. Nothing is ever converted automatically: not on
upgrade, not when a service is created, not by a reconcile.

**Consent needs a linearization point, or it is consent to something else.** Between preview
and confirm, another admin can change a component's source, edit a manual status, add, remove or
reorder components — and the confirm would then apply to a page the operator never saw. So the
preview carries a **structural CAS**, and the confirm recomputes inside its own transaction and
compares:

```
components.revision                 bigint, bumped by ANY mutation of THAT component
status_pages.component_generation   bigint, bumped by ANY component mutation on the page —
                                    add, remove, reorder, source change, manual status, name
```

The page generation deliberately bumps on neighbour edits too: the preview shows the page
SUMMARY, so an admin who changes a different component's manual status changes what admin A
consented to, while leaving A's target row untouched. Bumping only on add/remove/reorder would
let that race through — the reviewer's exact scenario, and the required regression.

A mismatch on either is **409 `page_configuration_stale`** with the same first-committer-wins
semantics as `graph_generation` (§14.2) — the operator re-previews rather than applying a stale
consent. What is deliberately NOT locked is live health: a service can legitimately change state
between preview and confirm, so the preview stamps its `as_of` and discloses that the *structure*
is what was agreed, not the momentary status. Two-admin stale-preview regressions are required
in both directions (source change and component-set change).

**Deletion, stated per case instead of per FK.** Three different deletions reach a component,
and pretending one rule covers them is how the first draft contradicted itself:

| Deleted | Component behaviour |
|---|---|
| a **service** bound as the ACTIVE source | `ON DELETE RESTRICT` → **409 naming the page and the component**. `SET NULL` would BE the automatic conversion invariant 70 forbids; `CASCADE` would silently delete a row customers can see. |
| a **monitor** bound as the ACTIVE source | the SHIPPED behaviour is kept: `SET NULL (monitor_id)`, and — because the CHECK requires an active binding — the same statement sets `source='manual'`. This IS an automatic conversion, it exists today (a deleted monitor's component already renders as manual), and it is recorded here as an **inherited exception** rather than being either denied or silently changed. |
| a **dormant** binding of either kind | cleared, no source change, no public effect: a dormant binding is a revert affordance, not a statement. |

**Project and org deletion keep their single-statement cascade (D-0150/FR-019).** `RESTRICT`
governs the deletion of an INDIVIDUAL service only. A project cascade removes its services and,
with them, any component bound to one — including components on ORG-LEVEL pages, which is
exactly the case the reviewer identified as blocked. Those components are therefore enumerated
in the project-deletion PREVIEW ("N status-page components will disappear from M pages"), so the
cascade stays one statement and the operator still consents to the public consequence. Tests
cover both a project-scoped page and an org-level page holding that project's service.

**Composite.** After Service exists, `composite` narrows to what it actually is: a logical
monitor. Conversion is explicit and non-destructive:

```
Convert composite → Service
  creates: Service (monitors = children, aggregation = all/any)
  keeps:   the composite, its historical SLO, incidents, timeline
  marks:   composite deprecated / superseded
  requires: explicit confirmation of sli[] — never silently "all children"
```

The SLI is a REQUIRED input with no default, and every live child joins the operational CONTEXT
regardless — the two lists exist precisely so that keeping a diagnostic visible does not change the
number. A composite whose declared children are not ALL live is REFUSED, naming the missing ids:
converting on the survivors would move the aggregation's meaning without anyone stating it, since
`all` over 2 is not `all` over 3 and a quorum threshold is defined against a specific N. The quorum
translation is `degraded_min = n − M + 1` with `healthy_min` EQUAL to it — the exact binary mapping,
because a composite has two states and adding a degraded band would report more than the composite
did on a page a customer reads. Widening that vocabulary is a separate owner decision with a preview
that shows the delta.

No historical data is migrated or reinterpreted in v1.

### 15.5 After conversion, a composite stays visible until its owner retires it (phase 4)

A converted composite **keeps running**: it still probes, still holds its SLO, and can still
open an incident and deliver an alert. So it stays in the monitor list, at full strength. A
monitor that pages someone while being absent from the list is the same
system-does-one-thing-shows-another defect §14 and §11 exist to prevent — the on-call would get
the alert and fail to find its source. Saving a list row is not worth that.

- **The link is stored once and rendered from BOTH ends.** One nullable column on the monitor,
  `superseded_by_service_id`, with the composite tenant FK `(superseded_by_service_id,
  project_id) → services (id, project_id)` and `ON DELETE SET NULL` — losing the successor is a
  lost annotation, not a corrupted monitor. It is not unique: two composites may legitimately be
  superseded by one service. The service side derives `converted from →` by reading the same
  column, so there is ONE fact and no pair to fall out of sync. Deleting the composite leaves the
  service untouched; deleting the service clears the annotation and nothing else.
- **Nothing is hidden by default.** A "hide superseded" filter is legitimate as an operator's
  CHOICE, off initially. A default that hides a working monitor is a false statement about what
  the installation is checking.
- **Retiring is a lifecycle MARKER on top of the existing execution switch, and one
  transaction.** `retired_at` alone stops nothing: the scheduler, the dead-man evaluator, result
  ingest, incident and SLO paths all key on `enabled`, revisions and state sequences. So
  `retire` is ONE transaction that sets **both**: `retired_at = now()` (the lifecycle statement,
  which is what removes it from the active list and invites nothing) **and** `enabled = false`
  (the existing, already-proven execution semantics). Reusing `enabled` alone would conflate an
  afternoon's disable with "superseded forever"; using `retired_at` alone would leave a retired
  monitor probing and paging. The same transaction carries the fences the disable path already
  owns — the `execution_revision` bump and its epoch fan-out for every service that references
  the composite (§6.2), the `state_sequence` advance so queued transition deliveries are
  recognized as stale at delivery time (the existing #2 gate), and the audit row. Re-activation
  is the explicit inverse (`retired_at = NULL`, `enabled = true`), audited, so a mistaken retire
  is recoverable; a file-managed composite refuses both through the ownership rule that already
  governs its `enabled`. A retired composite keeps its history, incidents and past numbers, and
  still renders under the "superseded" filter.
- **The lifecycle actions are COMPOSITE-only, and file ownership splits by DECLARATION authority,
  not by row.** `retire`/`reactivate` and the conversion refuse for a file-managed composite,
  because they write `enabled` or create a declaration — state a reapply would restate. The
  `superseded_by_service_id` annotation is PERMITTED there: it is not part of bundle format 2, it
  enters no canonical hash or generation, and no reconcile can contradict it, so refusing it would
  remove the only way to annotate a file-managed composite while protecting nothing. §15.1's "may
  MUTATE a resource" therefore means declaration/desired-state authority, not every column of the
  row.
- **Composite conversion is one serialized transaction, not a read-then-create.** It takes the
  composite row `FOR UPDATE`, then creates the service, writes the link and the audit row
  atomically; two simultaneous confirms therefore cannot create two services with
  last-writer-wins on the non-unique successor column. The service slug is derived from the
  composite's, and a collision is a **409 naming the existing slug** rather than a
  silently-suffixed second service; a re-confirm of an already-converted composite is a no-op
  returning the existing service (idempotent by the link column). A file-managed composite may
  be converted only by its provider's authority, per §15.1.

Retire is available for ANY composite, whether or not it was converted: an operator who built
the service by hand is in the same position as one who used the conversion tool.

The moment a composite stops being a source of alerts is therefore the same moment it leaves
the list, which is the only arrangement in which its absence from the list is not a lie.

### 15.1 Ownership matrix — declarations only

```
Resource ownership determines who may MUTATE a resource, not who may REFERENCE it.
```

**"Mutate" here means DECLARATION and desired-state authority, not every column of the row.** The
system and the runtime already write non-declarative state on a file-managed monitor — status,
counters, watermarks, revisions — and no reconcile contradicts them. A presentation-only annotation
that is absent from the bundle format, enters no canonical hash or generation, and cannot be restated
by an apply is therefore permitted as well; `superseded_by_service_id` (§15.5) is the one such field
phase 4 adds. What ownership forbids is a UI write to a DECLARED field, because the very next apply
would silently overwrite it — which is why `enabled`, and therefore `retire`/`reactivate`, stay
refused. Without this narrowing the matrix would read as a whole-row prohibition and contradict a
permission §15.5 grants.

The rule that decides every cell: **a system-authored revision is permitted only when the
mutating authority may also mutate every affected Service; otherwise the mutation is blocked or
the provider freezes.**

| Monitor owner | Service owner | Removing the monitor from the service's reach |
|---|---|---|
| UI | UI | Delete/move creates a system definition revision **atomically with the write**; both are UI-authority, so nothing is mutated behind an owner's back. |
| file | UI | The bundle removing the monitor is **rejected and the bundle keeps its last-known-good**. A routine bundle edit must not silently redefine a UI-owned service's availability. |
| UI | file | The UI delete/move returns **409** until the declarative reference is removed from the bundle. The system may not rewrite a file-owned declaration, and a UI action must not force it to. |
| file | file | One atomic desired state may remove the reference and the monitor together. A desired state that leaves a dangling reference, or that crosses providers, freezes with last-known-good. |

`DeleteMonitor` cascades heartbeat rows physically, so the revision cannot be an eventual
post-hook: it is created in the same transaction, under the lock order of §15.4, with the
system audit entry in that transaction, and with the matching epoch of §6.2.

**Evaluation epochs are exempt from this matrix.** They do not mutate a declaration (§6.2), so
an execution-property change on a monitor creates epochs for every referencing service
regardless of ownership, and a file provider's reapply remains a no-op. Blocking an interval
edit because some file-owned service references the monitor would be a cost with no
corresponding protection.

### 15.2 Monitoring as Code — bundle format 2

`func-monitoring-as-code` §3 admits new top-level resource maps only in a later bundle format.
r2 listed what such a contract would have to contain and then did not choose it, which is a TODO
wearing the clothes of a specification. Phase 1 chooses:

**Format.** `format: 2` introduces the `services` map. **Format 1 remains valid** and simply
declares no services (§17). A format-1 bundle is never rewritten or upgraded implicitly.

```yaml
format: 2
project: payments
monitors:
  checkout-http:
    slug: checkout-http
    name: Checkout HTTP
    type: http
    target: https://checkout.example.com/healthz
    region: core
    interval: 30s
  checkout-synthetic:
    slug: checkout-synthetic
    name: Checkout synthetic
    type: push
    dead_man: 5m
services:
  checkout:
    name: Checkout
    owner:
      escalation_policy: payments-oncall
    monitors: [checkout-http, checkout-synthetic, checkout-db, checkout-redis]
    sli:      [checkout-http, checkout-synthetic]
    aggregation: { mode: quorum, degraded_min: 1, healthy_min: 2 }
    region:      { mode: per_region, degraded_min_regions: 1, healthy_min_regions: 1 }
    missing_data: unknown
    maintenance:  exclude
    freshness:    { active_multiplier: 3, active_floor: 90s }
```

- The map key is the service **slug**: project-unique, immutable, the MaC reference key and the
  URL segment.
- **Format-2 monitor entries declare `slug` explicitly**, and it defaults to the map key (the
  provider source UID) when omitted. r3's example used map keys as references while the entries
  carried neither the format-1 required `name` nor a `slug`, so the example was invalid under
  its own strict schema and the reference target was undefined. Both are fixed above: entries
  are valid format-2 documents, and the reference target is a field the author can see.
- `monitors[]` and `sli[]` reference **monitor slugs** (§15.3), resolved tenant-scoped in one
  query. Provider UID remains the provider-local ownership identity and is explicitly **not**
  the cross-owner reference key, because §20.1 permits a UI-owned service to reference a
  file-owned monitor and the reverse.
- **The same bundle produces the same slugs on every installation, or fails by name.** Applying a
  format-2 bundle whose declared slug differs from the row's existing immutable slug is rejected
  with `monitor_slug_immutable`, naming both values. There is no server-dependent fallback at
  apply time: a hidden UUID-derived suffix appearing on one install and not another would make a
  Git-tracked bundle non-portable, which defeats the point of declaring services in files.

**Strict closed schemas.** Unknown keys are rejected. Every field has a stated default. The
same validator serves the API and the file provider — one owner, as with credentialed monitor
settings in FR-020.

**Canonical hash**, per `func-monitoring-as-code` §7 without exception: computed after defaults,
duration conversion and domain validation; `monitors` and `sli` are set-like and are sorted and
deduplicated; policy maps are key-sorted; excluded are comments, whitespace, key order, source
path and mtime, server-owned fields (revision numbers, epoch ids, generations, sealed state) and
provider generation timestamps. The hash covers the **declaration only**.

**No-op rule.** An unchanged canonical service hash MUST NOT create a definition revision — the
exact analogue of §7's "MUST NOT call the semantic monitor update path". Moving a file may
update provenance separately.

**A service-only change does not bump any monitor's `execution_revision`** — and, per §6.2, it
**must** still create a definition revision and its matching epoch. The required test asserts
all three halves at once: every monitor's `execution_revision` unchanged; exactly one new
effective definition revision; exactly one matching effective epoch; and a fact in the new
range resolving to it. r2 stated only the first half, which would have left the unsatisfiable
foreign key of §6.2 in place.

**Ownership and provenance** are per resource, exactly as for monitors: a project may hold
file-owned services and UI-owned services side by side; reconciliation compares only against
services owned by the same provider and tenant; absence from a bundle never changes a UI-owned
or other-provider service.

**Adoption is refused, and this differs from monitors in consequence.** `func-monitoring-as-code`
§8 already forbids auto-adopting an unmanaged row by name. For monitors that is a policy; for
services it is also a physical constraint, because the slug is project-unique and the bundle
therefore cannot create a second row. A bundle declaring a slug already held by a UI-owned
service is **rejected with `service_slug_owned_by_ui`**, the bundle keeps its last-known-good,
and on a first apply with no last-known-good nothing is mutated. A slug collision with the
provider's **own** existing row — recognized by `(provider, tenant, kind, source_uid)` — is not
adoption and applies normally.

**Reference semantics** reuse the guarded-reference contract built for secret refs: normalized
ref rows carrying `project_id`, a deferred tenant-safe FK as a commit-time guard, `409` on
delete, and a per-project reconcile freeze with last-known-good on a dangling reference.
Inventing a second reference semantics would be a regression in consistency.

**Incident history** survives service deletion via a snapshot (`service_id` nullable +
`service_name_snapshot`); an active status-page projection blocks deletion until explicitly
detached.

### 15.3 The monitor slug, and its migration

A service must be able to name a monitor in YAML, and today it cannot: monitors have no
project-unique name — the only unique indexes on them are `push_token_hash` and, on heartbeats,
`(monitor_id, ts)`. UUIDs are unusable as an authoring contract, and restricting membership to
file-owned monitors would contradict the coexistence matrix. Phase 1 therefore adds a
**project-unique, immutable monitor slug**, by expand → backfill → contract:

1. add `slug` nullable;
2. **deterministic, collision-safe backfill** that changes no monitor id, display name or
   history, and that prefers a portable source over a local one:
   - a **file-owned** row takes its provider source UID, which is exactly the key its bundle
     already uses — so a Git-tracked bundle yields the same slug on every installation;
   - a UI-owned row takes its normalized display name, with a stable short suffix derived from
     the monitor UUID only where a normalized name would collide;
   - collisions and the suffixes they produced are reported in the migration output rather than
     applied silently.

   The assigned result is visible in the API, the UI and bundle export, because an operator
   cannot author a format-2 service against a slug they cannot see;
3. unique `(project_id, slug)`, then `NOT NULL`;
4. new monitors created through the API, the UI or a bundle receive or declare a slug once;
5. **renaming the display name never changes the slug.** The slug is immutable in phase 1:
   making it renameable turns it into a guarded declaration mutation across every referencing
   service, which is a larger contract than this phase needs.

Format-1 bundles stay valid; their rows receive backfilled slugs by the rule above. Upgrading
such a bundle to format 2 binds the already-owned row **by its provider source UID** and may
only **CONFIRM** the existing slug — r3 wrote "sets or confirms", and "sets" is impossible once
the column is `NOT NULL` and the slug is immutable, so that phrasing contradicted its own
contract. A one-time correction of a backfilled slug is a separate, guarded migration performed
**before** the contract phase, never a side effect of an apply.

### 15.4 One lock order for the whole product

r2 proposed a global order of "monitors, then services". That would have **redefined an already
approved contract**: FR-020 §4.3 and `internal/store/monitors.go` fix the order as *referenced
secret rows by id ascending, then monitor rows*, and `planSecretBindings` takes a whole-plan
id-ordered lock precisely to prevent the deadlock that order exists for. FR-021 extends it
rather than replacing it:

```
project `service_membership` advisory xact lock                 (new — §10.9; only on paths
                                                                 that change the affected-set
                                                                 predicate)
→ project `service_graph` advisory xact lock                    (phase 3 — §14.1; only on
                                                                 edge-mutating paths)
→ referenced secret rows,  id ascending                         (FR-020 §4.3 — unchanged)
→ referenced routing rows (escalation policy, on-call schedule),
                         id ascending, FOR KEY SHARE            (new — see below)
→ monitor rows,          id ascending                           (FR-020 §4.3 — unchanged)
→ service rows,          id ascending                           (new)
→ that service's declaration, epoch and range rows              (new)
→ service_bucket_ingest and fact rows, by (service_id, bucket_start) ascending   (new)
→ ownership / provenance / bundle state                         (as required)
```

**Every path FR-021 introduces or changes takes them in this direction**: Service CRUD, the
service-`owner` write, epoch fan-out, maintenance mutation and preview/confirm, seal, ingress,
range processing and format-2 apply. Existing paths keep the order FR-020 already fixed for
them — secret rotation, monitor create/update/delete/move — which is the same direction for
every class they touch.

**One legacy exception is named rather than asserted away.** `UpdateMonitor` acquires the
monitor row before its FK check reaches the escalation policy, while deleting a policy acquires
the policy first; those two disagree, as the analysis below shows. r5's sentence claimed every
path including `UpdateMonitor` follows the order and then documented `UpdateMonitor` violating
it — a direct contradiction in one section. The order above is asserted for FR-021's paths;
`UpdateMonitor` versus policy deletion is recorded as **pre-existing FR-012 backlog**, out of
scope here, and FR-021 is required only not to widen it.

**Routing rows take no EXPLICIT lock — but referential integrity takes implicit ones, and the
analysis has to include them.** r4 said "no routing lock class exists", and that was too strong:
absence of `FOR UPDATE` is not absence of locking.

Verified against the code: `escalation_policies` and `oncall_schedules` (migration `00035`) are
taken under no explicit row lock by any write path — the only `FOR UPDATE` in
`internal/store/escalation.go` is on `incidents` during the escalation sweep — and
`monitors.escalation_policy_id` is protected by `ON DELETE SET NULL`. What that leaves is
implicit RI locking, in **two opposite directions**:

```
UpdateMonitor          locks the monitor row FOR UPDATE (monitors.go), then the UPDATE's
                       FK check takes FOR KEY SHARE on the referenced policy
                       →  monitor, then policy

DELETE a policy        locks the policy row, then ON DELETE SET NULL locks and updates
                       every referencing monitor
                       →  policy, then monitor
```

That is a pre-existing deadlock hazard in the product, not one FR-021 invents — and FR-021 adds
a second identical owner FK, which would widen it. The rule that keeps FR-021 out of it:

> **A write that sets or changes a service `owner` takes `FOR KEY SHARE` on the referenced
> escalation-policy and on-call-schedule rows EXPLICITLY, by id ascending, in the
> referenced-targets class — before locking the service row.** Both directions then agree, and
> the service path never contributes to the cycle.

The monitor path is left as it is: changing it is a fix to FR-012's write path, not a change
FR-021 is entitled to make silently. It is recorded here so the hazard is written down where the
next person looks for it.

Acceptance therefore includes concurrency tests under a low `lock_timeout`: `UpdateMonitor`
against `DeleteEscalationPolicy`, and a service-owner write against `DeleteEscalationPolicy` and
`DeleteOncallSchedule`. The service tests must show no deadlock; the monitor test documents the
existing behaviour rather than asserting it is safe.

**The `(service_id, bucket_start)` ordering is mandatory for ingress and seal**, not advisory: a
historical agent batch can touch many buckets across many services at once, and two overlapping
batches taking those keys in opposite orders deadlock against each other. Both sides iterate the
keys ascending.

## 16. Tenancy

r2 carried `project_id` on facts and revision members and forgot it everywhere else. Global
leader work does not make a cross-project reference safe; it makes it invisible.

- Every table this feature introduces carries `project_id`: services, definition revisions,
  evaluation epochs, member snapshots, facts, `service_bucket_ingest`, `service_late_arrival`,
  `service_repair_ranges` and cursors, and reference rows.
- Relationships use **composite tenant-safe FKs** — `(id, project_id)` unique targets, the
  pattern migration `00061` established for `monitor_secret_refs` — so a row can never point
  across tenants even under a bug in application-level filtering.
- Owner references (escalation policy, on-call schedule) are validated to be in the same project.
- **Every background query is project-scoped**, including leader materialization, range resume,
  seal, repair and retention.
- Required negative tests, all cross-tenant: API read and write; range resume after restart;
  MaC reference resolution; owner reference assignment; and delete/cancel cascades.
- **Phase 3 joins this enumeration**: `service_dependencies` and `incident_service_impacts`
  each carry one shared `project_id` under composite FKs on BOTH endpoints; `incidents` gains
  the `(id, project_id)` unique target and its `monitor_id` anchor becomes the composite
  `(monitor_id, project_id)` FK (§14.3); the correlation traversal is a project-scoped
  background query like every other; and the cross-tenant negative tests extend to the
  direct-SQL/store layer for links and anchors, not only the API.

## 17. Backward compatibility (acceptance criterion, not a footnote)

```
- zero Services is a valid installation state
- every existing Monitor remains valid without a service
- bundle format 1 remains valid
- existing status pages render unchanged
- existing composites retain behavior
- existing incidents and monitor SLOs retain semantics
```

Zero services must also cost nothing at runtime: with no service declaring a monitor as a
member, the ingress handshake of §10.4 writes no rows and the leader schedules no service work.

Service-aware UX begins only after explicit adoption. Upgrade day must not present an empty
"Services" screen as the product's new front door.

**Phase 4 keeps every line above, with THREE deliberate exceptions, enumerated here rather than
discovered by a customer.** All three are the same inherited defect — the unknown shipped as
health — and each is the point of the change, not a regression:

1. a **`pending`** monitor component rendered `operational` (`ComponentStatusFromMonitor`) →
   renders `no_data`;
2. an **EMPTY page** rendered `operational` (`SummaryStatus(nil)`) → reports "no components
   configured";
3. a **MANUAL component with an empty `manual_status`** rendered `operational` (the render's
   default) → renders `no_data`; an operator who wants the previous appearance sets the status
   explicitly to `operational`, which is now the only way to assert it.

Everything else holds: manual components stay first-class, service projection is opt-in per
component, no component's source changes without an explicit previewed action (with the one
inherited exception of a deleted active monitor, §15.0), composites keep behaviour and
visibility (§15.5), and no historical number is recomputed or reinterpreted.

## 18. Delivery phases

| Phase | Content |
|---|---|
| 1 | Domain + storage foundation: Service resource, definition revisions and evaluation epochs, the evaluation-semantics projection, effective-state model and piecewise reducer, duration-weighted fact schema, seal/ingest handshake, durable ranges, maintenance mutation contract, monitor slug, bundle format 2, bounds. No alerting, no status projection, no correlation. |
| 2 | Reliability reporting: service SLO, error budget, burn-rate computation, both coverage axes, revision and epoch segmentation, insufficient-history UX, two-layer health card. |
| 3 | Dependency impact graph: same-project service DAG (schema-enforced tenancy, bounded, outside the declaration — no revisions, no epochs), symmetric open-time incident correlation into structured incident-service links with 🕸 annotations, list+badge UI. Annotates and links; never suppresses, merges or hides. Specified in §14. |
| 4 | Status-page service projection: a third component source (`service_id` under ONE active discriminator, dormant bindings retained), the SLI layer alone projected into the public vocabulary plus the new `no_data` status, sealed-facts-only 90-day strips, explicit previewed reversible conversion in both directions, the corrected `pending` mapping, and composite retire tooling with two-ended links. Impact links stay non-public. Specified in §15.0/§15.5. |
| 5 | *Intent only.* Alerting ownership: service burn alerts and monitor delegation/suppression rules. |

Phase 5 is **deliberately not specified** (phases 3 and 4 now are, in §14 and §15.0/§15.5). Its
UX depends on facts phases 1–4 produce — real `UNKNOWN` density, late-arrival behaviour,
recompute cost, and how often a service's public state actually differs from its monitors' —
and specifying it now would encode assumptions the data may refute.

## 19. Acceptance invariants

**Phase 1** (accepted only when all hold):

1. the evaluator is a PURE function of `(observations, epoch snapshot, maintenance spans,
   range)` — it reads no clock (§7.2);
2. buckets are UTC half-open `[start, end)`; the reducer is piecewise over the breakpoint set of
   §7.2; **both** duration axes are integer microseconds and each sums **exactly** to
   `bucket_size` on every stored row (§9.1, §10.2);
3. every member is normalized by its monitor TYPE — a stale enabled `push` member is `BAD`, a
   disabled member is EXCLUDED — and the precedence order of §7.1 is followed;
4. `missing_data_policy: ignore` has its own variable `i` in the algebra (§9.3), never moves
   time into `excluded_duration`, and when it removes the last source of information the
   interval is UNKNOWN rather than EXCLUDED; provenance distinguishes an ignore-weakened quorum
   from a declaration-weakened one (§8.1, §9.4);
5. aggregation is TOTAL under the tables of §9.3 and §9.4; thresholds are clamped to eligible
   cardinality and `declaration_weakened` is recorded in provenance whenever the clamp bites;
6. threshold validation runs against the DECLARED cardinality and is never triggered by a
   momentary exclusion (§9.5);
7. every fact references exactly one **effective** epoch, which resolves to exactly one
   **effective** definition revision, and no two effective epochs cover one bucket;
8. **every** definition-revision transaction creates a matching epoch; execution-driven epochs
   are skipped only when the evaluation-semantics hash is unchanged; and an execution-driven
   epoch resolves the winning declaration **at its own `effective_at`**, including a pending
   same-boundary revision — proven for both interleavings (§6.2);
9. the evaluation-semantics projection is exhaustively classified beside the `execution_revision`
   field set, with no default; target-only, condition-only and secret-generation changes each
   produce an epoch, and rename-only, status and ciphertext re-encryption each produce none
   (§6.3);
10. `effective_at = ceil_to_bucket(created_at)`; a write exactly on a boundary uses that
    boundary; only first adoption is retroactive (§6.4, §6.6);
11. at most one **effective** row per `(service_id, effective_at)` on both axes; the loser is
    `superseded_before_effect`, is retained for audit, is referenced by no fact, and its durable
    ranges are cancelled in the same transaction (§6.5);
12. a first-adoption backfill is labelled a declared reconstruction in storage, API and UI
    (§6.6);
13. an execution-property change creates epochs regardless of service ownership and leaves every
    declaration, provider hash and provenance untouched (§15.1);
14. the seal/ingest handshake of §10.4 holds: the handshake fires only for a heartbeat that was
    actually INSERTED, so a duplicate redelivery is a full no-op and never a false late arrival;
    membership is resolved from `sli[]` **as of the heartbeat's own bucket**, not as of now; the
    seal UPSERTS every bucket's row before locking it, so a bucket with no prior heartbeat has no
    phantom; a late arrival is recorded in an AGGREGATED, idempotent, bounded record; and a
    monitor in no service's SLI at that instant writes nothing;
15. `late_arrival_grace` is its own bounded setting and is never `result.allowed_skew`;
16. sealed facts are rewritten only by a recompute over the target's validity range or by an
    audited administrative operation with an explicit range — recorded with before/after
    availability;
17. `sealed_through` is contiguity-defined — a materialization hole holds it rather than being
    jumped over — and never moves backwards except under an audited retraction (§10.5);
18. recompute is bounded by raw availability; older facts keep their epoch and revision;
19. durable work is a set of ranges: a newer epoch or revision never cancels unfinished
    historical work; supersession is legal only by atomically assuming the union; the epoch is
    resolved per bucket; resume continues from the cursor; a periodic resync makes a lost
    `NOTIFY` cost latency and never correctness (§10.8);
20. an incremental run over any **bucket-aligned** partition of a range produces the same fact
    CONTENT as one batch run over that range, on both duration axes, proven by a property test
    that also asserts both conservation checks; and, separately, the piecewise reducer is checked
    against an independent oracle over arbitrary sub-bucket breakpoints (§10.8);
21. every service-fact batch transaction runs on the leader's lock-owning connection, so a
    deposed leader cannot commit — the existing `check()` callback does not satisfy this;
22. maintenance `archive` never rewrites sealed past and never requires raw data; a create or
    `annul` intersecting sealed time is a retroactive repair carrying a **persisted preview
    token** whose confirm is a CAS over the project's maintenance generation and the
    `definition_generation` of **every** affected service — held in a complete server-side
    relation, never a truncated sample — and over the affected SET itself, re-resolved inside the
    mutating transaction while holding the project `service_membership` advisory lock that every
    set-changing path also takes, and required to be exactly equal, so a service created, deleted
    or newly referencing the monitor between preview and confirm makes the token stale; with
    `raw_floor` checked as the monotonic predicate
    "the requested sealed start is still ≥ the current repairable floor"; an `approximate`
    preview cannot be confirmed at all; a stale token fails closed with `409 preview_stale` and
    an unrecomputable range with `409 unrecomputable_range`, nothing is partially applied, and
    the audit records the repair's own before/after rather than the preview's estimate (§10.9);
23. a maintenance mutation invalidates in its own transaction — generation bump, coalesced
    ranges, `repairing` reads and any required watermark retraction — and every repair batch
    commits only under an unchanged `maintenance_generation` (§10.9);
24. an archived maintenance window **keeps its effective span** — the evaluator reads a retained
    row over `[starts_at, min(ends_at, cancel_effective_at))` regardless of `archived_at`, so a
    later recompute cannot silently turn an archive into an annul; `cancel_effective_at` is the
    exact database statement time, not a rounded boundary; and archived rows stay resolvable from
    provenance for at least the facts' retention (§10.9);
25. every bound in §10.10 has a configured default and a hard maximum, `min_decidable_coverage`
    is fixed at 0.95 and not operator-settable in phase 1, and the fan-out caps are checked under
    a lock so concurrent writers cannot both pass a stale count;
26. the cadence bounds derive from ONE mechanism — a per-slice deadline of
    `min(remaining cycle budget, max_dispatch_delay)` enforced **caller-side over the whole
    slice**, `BEGIN` through `commit` or `rollback`, with `statement_timeout` and `lock_timeout`
    re-derived from the REMAINING budget before every statement and an adaptive batch size — and
    the test asserts total slice elapsed within `max_dispatch_delay + max_scheduling_tolerance`
    across **at least two consecutive statements that each finish just under the sub-deadline**,
    plus a lock-wait path and a rollback path, not merely one slow query (§10.10);
27. derived facts, late-arrival records and provenance are not pruned by heartbeat retention
    (§10.6);
28. `service_reliability_buckets` uses native RANGE partitions with identical semantics in both
    storage modes (§3);
29. no generic time-series primitives are introduced (§3 non-goals);
30. a deletion or move that invalidates a declaration follows the §15.1 matrix, in one
    transaction, under the lock order of §15.4, with a system audit entry;
31. a service-`owner` write takes `FOR KEY SHARE` on the referenced routing rows explicitly, by
    id ascending, before locking the service row, so the implicit RI locks of §15.4 cannot form
    a cycle; proven by concurrency tests against policy and schedule deletion under a low
    `lock_timeout`;
32. every table carries `project_id` with composite tenant-safe FKs, every background query is
    project-scoped, and the cross-tenant negative tests of §16 pass;
33. the fact carries BOTH result axes as durations, so a bucket that was HEALTHY throughout is
    distinguishable from one that was HEALTHY for half of it and DEGRADED for the rest, and both
    axes roll up as exact sums (§9.1, §10.2);
34. a Service with an empty `sli[]` creates its definition revision and a matching empty epoch
    and creates no facts, no handshake rows, no watermark and no SLO; the threshold validation of
    §9.5 does not apply to it;
35. provenance carries a bounded, overflow-counted cause set for `unknown_duration` as well as
    for `bad_duration`, so a `partial` window is still explainable after raw retention (§10.3);
36. format-2 monitor entries declare a slug that the same bundle resolves identically on every
    installation; a declared slug differing from the stored immutable one is rejected by name
    (§15.2, §15.3);
37. late-arrival records are aggregated per `(service, bucket, monitor)`, idempotent under
    redelivery, and bounded by `max_late_examples` with an overflow count (§10.4).

**Phase 2** adds:

38. a window is `[sealed_through − window, sealed_through)` and every response carries `as_of`,
    `sealed_through`, the summed durations of both axes, the coverage fraction and any
    `repairing` interval;
39. storage continuity and decidable coverage are checked independently; below
    `min_decidable_coverage` the objective, budget and burn are `partial` with the fraction and
    reason, and at zero decidable time they are `unavailable` — an almost-entirely-UNKNOWN
    window is never `100%`;
40. `excluded_duration` enters neither denominator, and a zero denominator yields `unavailable`,
    never `100%` and never `0×`;
41. a Service without SLI has no SLO and no budget — never 100%;
42. adding a monitor to `monitors[]` does not change the SLO until it is explicitly added to
    `sli[]`;
43. a window spanning definition revisions is returned as segments with no aggregate across the
    boundary; epochs are segmented in the payload and may only be grouped visually;
44. segments produced by a first-adoption backfill are labelled as declared reconstructions;
45. every budget states the objective that produced it, and an objective change is annotated;
46. existing monitor-level SLO behaviour is unchanged;
47. service SLOs do not alert, and a service-scoped `burn_alert_enabled` is rejected.

Invariant 42 is the single sharpest test of whether this design succeeded: if it holds, Service
is a reliability-domain object; if it fails, Service is still a grouping abstraction.

**Phase 3** adds:

48. service dependency edges AND impact links are same-project by SCHEMA: one shared
    `project_id` under composite FKs on BOTH endpoints of each relation, `incidents` carries
    the `(id, project_id)` unique target, and `incidents.monitor_id` is the composite
    `(monitor_id, project_id)` FK — a cross-tenant edge, link or anchor is unrepresentable,
    proven by direct-SQL/store negative tests, not only API ones;
49. an edge-only mutation creates no definition revision and no epoch, enters no canonical
    hash, and moves no sealed fact — proven by a regression that counts revisions and epochs
    and compares hashes before and after;
50. edge mutation goes through the dedicated `/dependencies` routes (or create-with-edges, or
    the bundle's edge track — one validator, one mutator) guarded by `graph_generation`: a
    stale token is a 409 and the two-operators lost-update is impossible; every non-no-op
    delta writes a tenant audit row (actor, added, removed) in the SAME transaction, and a
    no-op bumps nothing and audits nothing;
51. a desired file edge pins its target: an incoming bundle with an unresolvable `depends_on`
    slug is rejected keeping a last-known-good that is still literally true; deleting a
    service named in an applied file `depends_on` is a 409 naming the provider; one provider
    may remove its own target and edge atomically; cross-provider dangling freezes per §15.1;
52. correlation is carried by its OWN outbox topic (`incident_correlation`, enqueued in the
    incident's opening transaction): webhook death never blocks correlation, a correlation
    failure never blocks incident delivery, and both keep the outbox retry/dead-letter
    envelope; correlation never suppresses recording or delivery and never merges or hides an
    incident;
53. the correlation attempt is ONE store transaction: counterpart incident rows locked FOR
    UPDATE in ascending id order, open-state RECHECKED under the lock, links inserted, and the
    🕸 notes for newly gained rows inserted in that same transaction — link-without-note and
    note-without-link are both unrepresentable, and a resolve racing the attempt loses to the
    barrier, never to check-then-act;
54. symmetry, qualified to the mechanism AND the witness bound: for two incidents both still
    open when either's attempt commits, at least one attempt observes the other — proven for
    both interleavings; relation content is complete over the BOUNDED selected witness set
    (per REACHABLE endpoint service, the oldest `max_correlation_witnesses_per_service` by
    `(started_at, id)` — deterministic, SQL-capped and endpoint-scoped reads, anchor never
    counted, overflow counted and logged for reachable endpoints only, service-level marking
    unaffected); an incident resolved before any attempt reaches it is skipped by design; a
    redelivery inserts zero rows and zero notes, and relation content is byte-identical
    across redelivery; the attempt reads membership, graph and witnesses under the
    membership→graph advisory locks, so every row encodes ONE committed state;
55. `probable_root` marks EVERY upstream service on a path with an open incident and
    `affected` every downstream one; each row carries `computed_at` and its CANONICAL path —
    shortest, then lexicographic slug tie-break, endpoint-inclusive and root-first, bounded
    text[] of at most depth cap + 1 = 11 slugs (a direct edge stores 2, a depth-10 chain 11,
    and a back-fill row carries the identical array as its `affected` counterpart) —
    deterministic under diamonds and equal-length ties; the relation never elects a single
    culprit — ranking is presentation only;
56. resolved incidents are never annotated; membership resolves via the current effective
    `monitors[]` (never `sli[]`) at attempt time, and that drift window is stated;
57. the 🕸 note is written only for a batch that inserted at least one new link row, its prose
    is bounded (8 names + `+N more`) over a complete relation, and it survives service
    deletion as immutable timeline text while the link rows cascade;
58. every edge-mutating path — create-with-edges, UI replace-set, format-2 apply, service
    delete — serializes on the per-project `service_graph` advisory lock, ordered after
    `service_membership`, and the cycle check is sound only under that lock (asserted by a
    concurrent-writers test); the depth cap is pinned at 9 (valid), 10 (valid), 11 (the
    rejected write);
59. impacts are an authenticated-detail enrichment: absent from the shared incident model,
    absent from the incident LIST, and absent from every public and internal status-page
    projection until phase 4 opts in — proven by a raw unauthenticated JSON regression over a
    project with correlated incidents;
60. read-side work is bounded and tested: no per-row impact query on the incident list, and
    the service-detail dependency health is ONE batched snapshot query at the maximum
    neighbour set;
61. the new topic is safe under a mixed-version fleet BY SCHEMA and stays safe for the row's
    WHOLE lifetime: an immutable `fenced` column set at enqueue plus the demotion CHECK
    (`NOT fenced OR status <> 'pending'`) make the legacy-claimable state unrepresentable for
    a fenced row through claim, failure, dead and BOTH replay paths — a failed capable attempt
    restores `pending_fenced`, replays restore it, and the old replay SQL against a fenced
    dead row fails closed on the constraint; the capable claim adds fenced rows only for
    topics in its own dispatch set; the enqueue topic→class mapping is one store-side map
    pinned by test; proven by the mixed-owner, forced-failure, replay-both-paths,
    legacy-replay-rejected and legacy-topic-unchanged regressions.

**Phase 4** adds:

62. every component row carries its OWN tenant identity — `org_id NOT NULL`, nullable
    `project_id`, composite FKs to the page, the project, the monitor and the service — so a
    cross-org or cross-project component is unrepresentable to a DIRECT SQL writer, not merely
    unreachable through the API; a project-scoped page's components carry that project;
63. the source is an explicit discriminator (`source`), never the presence of a column: the
    active binding is required by CHECK, inactive bindings stay DORMANT, and every transition in
    the §15.0 matrix is therefore reversible without a staging table; the backfill of shipped
    rows is total (`monitor_id` present ⇒ monitor, else manual) and changes no rendered output;
64. a monitor-backed component keeps its inherited `manual_status` override (D-0021), a
    service-backed component refuses one, and `manual_status = 'no_data'` is refused by CHECK —
    `no_data` is computed, never typed;
65. the public projection reads the SLI layer ALONE — the diagnostics layer and the §14 impact
    links never appear on any public or internal status-page projection, proven by a raw
    unauthenticated JSON regression over a page whose service has BOTH failing diagnostics and
    impact links; a service component's project joins the page's incident/maintenance scope
    exactly as a monitor component's does, and that too is pinned by a raw-JSON regression;
66. the projection reads ONE authoritative batched DTO at ONE `as_of` carrying `sli`, `excluded`,
    `reason` and `sealed_in_window`, over the existing evaluator — no second semantics owner — and
    the §15.0 precedence is TOTAL: maintenance outranks absence, `sealed_in_window` governs the
    STRIP and never the status, so a live-healthy service before its first seal publishes
    `operational` with no strip; required tests: active maintenance, generic unknown, no `sli[]`,
    healthy-before-first-seal, first seal. The batch is PAGE-scoped over `(project_id, service_id)`
    pairs, not per project: an org-level page legitimately spans projects, so a per-project batch
    is neither one snapshot nor a constant statement count;
67. `no_data` does not join the severity ladder: the summary is `worst-of MEASURED` plus an
    `unmeasured` count, and the §15.0 headline table is total — operational+unmeasured never
    reads "all systems operational", all-`no_data` is not operational, an EMPTY page reports
    "no components configured" instead of today's `operational`, and `severity()`'s unknown
    default becomes the WORST measured rank instead of `operational` — the function orders measured
    statuses and returns an int, so "the default becomes `no_data`" (§15.0) was not implementable
    as written; what it protects against is a future enum value silently ranking as health, which
    the worst-rank default delivers;
68. THREE intentional changes to existing public output, each recorded in §15.0, §17 and D-0167 and
    each with a regression that pins the OLD behaviour as REMOVED rather than merely unasserted:
    a `pending` monitor component renders `no_data`; a manual component with no stated status
    renders `no_data`; and an EMPTY page reports "no components configured" instead of
    `operational`. §17 enumerates three, so an invariant naming one was an undercount;
69. the 90-day history is bounded and honest: `daily[]` is UTC days from SEALED buckets only,
    at most 90 points, days without data ABSENT (never zeros, never interpolation) and partial
    days carrying their covered fraction; `uptime_90d` follows §11.2/§11.3 — withheld with its
    reason below decidable coverage and withheld across a revision boundary, never synthesized;
    ONE batched snapshot per page with a constant statement count, and `max_components_per_page`
    = 50 fixed, with over-bound pages still rendering, refusing new components and logging the
    overage;
70. conversion is explicit in BOTH directions, previewed, audited in the MUTATING transaction,
    and structurally CAS-guarded: `components.revision` and `status_pages.component_generation`
    — the latter bumped by ANY component mutation on the page, neighbours included — are compared
    inside the confirm transaction and a mismatch is 409 `page_configuration_stale`
    (first-committer-wins), while live health is disclosed via `as_of` rather than locked;
    two-admin stale-preview regressions cover a source change, a component-set change and a
    NEIGHBOUR component's edit;
71. deletion is specified per case: an actively-bound SERVICE is `RESTRICT` → 409 naming the page
    and the component; an actively-bound MONITOR keeps the shipped `SET NULL (monitor_id)` and
    the same statement sets `source='manual'` — an inherited automatic exception, recorded as one
    rather than denied; a dormant binding is cleared with no source change; and project/org
    cascade (D-0150/FR-019) stays ONE statement, with affected components enumerated in its
    deletion preview and covered by tests on both a project-scoped and an org-level page;
71a. the projection's inputs are window-scoped and error-honest: the 90-day window ends at
    `sealed_through` (never at the page's `as_of`) with UTC day boundaries converted in SQL and the
    NEWEST day retained under the 90-point cap, `sealed_in_window` — not "any fact" — decides
    whether a strip exists, and an ACTIVE service that the projection cannot find degrades the
    render with a logged, COUNTED failure plus a PUBLIC `unavailable` marker instead of publishing
    the calm statement `no_data` — a failed read whose bytes match a calm value is the confusion
    this forbids. `uptime_90d` and its `withheld_reason` come from the SAME §11.2/§11.3 decision the
    authenticated report uses, never a second implementation;
71b. the public render is bounded in ROWS, MEMORY and TIME, not merely in statement count: a
    per-page ceiling persisted as `max(50, current)` that can only shrink — LOWERED by every
    removal and never raisable above `max(50, current count)`, so no write creates headroom — an
    absolute fail-closed public ceiling of 500 above which the page returns
    `status_page_over_safe_limit` naming the count and the limit (never a truncated subset posing
    as the whole page) while the authenticated view still lists everything, and a short-TTL render
    cache keyed to the exact access shape (page + public/authenticated + unlisted token) as the
    RATE bound, since a ceiling bounds one request and not repetition. Both counters — the page
    generation and each component's revision — are DB-owned, because FK actions are part of the
    contract and a cascade has no application on its path;
72. a converted composite keeps probing, keeps its SLO, stays listed at full strength and is
    never hidden by default; the link is ONE stored fact (`monitors.superseded_by_service_id`,
    tenant-composite FK, `SET NULL` on service deletion) rendered from both ends;
73. `retire` is ONE transaction setting BOTH `retired_at` (the lifecycle statement) and
    `enabled = false` (the existing execution semantics), together with the `execution_revision`
    bump and its epoch fan-out for referencing services and the `state_sequence` advance that
    makes queued transition deliveries stale — a retired monitor therefore stops probing, paging
    and materializing, which `retired_at` alone would not achieve; it is audited, reversible, and
    refused for a file-managed composite except by its provider's authority;
74. composite conversion is one serialized transaction (`FOR UPDATE` on the composite, then
    service + link + audit atomically), so two simultaneous confirms cannot create two services;
    a slug collision is a 409 naming the existing slug rather than a silently suffixed twin, and
    re-confirming an already-converted composite is an idempotent no-op returning the existing
    service.

## 20. Adversarial cases — answered

The FR-020 audit found that two of its three blockers were holes in the SPECIFICATION,
faithfully implemented, because its acceptance matrix never asked for the tests that would have
caught them. This section exists so the same class of gap is closed before code, not after a
live incident.

### 20.1 Membership and lifecycle

| Case | Ruling |
|---|---|
| **SLI member deleted** | Deleting a monitor referenced by an `sli[]` changes the DECLARATION, so it follows the §15.1 matrix: a system-authored definition revision (with its matching epoch) where the same authority owns both, a `409` or a provider freeze where it does not. It must NOT silently remain and emit UNKNOWN forever: under `missing_data_policy: bad` that would tank the budget, under `ignore` it would quietly inflate it — and §8.1 forbids the latter from even being cheap. Prior facts keep the revision that produced them. |
| **SLI member moved to another project** | Membership is project-scoped (§5), so a move out is identical to deletion and takes the same path. This is a DECLARATION change and may therefore block, unlike an execution-property change, which never does. |
| **A monitor in the SLI of two services** | Explicitly supported (§5): one measurement source feeding two independent definitions. No double counting, because each service aggregates independently. The ingest handshake writes one row per service, bounded by `max_services_per_monitor`. |
| **Service with empty `sli[]`** | Valid (§5). Operational context, no SLO, no budget. Reported as *unavailable* — never as 100%. |
| **UI-managed Service referencing a file-managed monitor** | Allowed. Membership is a reference by monitor SLUG resolved inside the project (§15.3), and ownership of the service is independent of ownership of the monitor. |
| **Bundle removes a monitor still referenced by any service's `sli[]`** | Per §15.1: rejected with last-known-good preserved when a UI-owned service references it, and permitted only as one atomic desired state when the same bundle owns both. |
| **Bundle declares a service slug already held by a UI-owned service** | `service_slug_owned_by_ui`, freeze with last-known-good, no mutation (§15.2). Never adoption — and here the slug's uniqueness makes it physically impossible as well as forbidden. |

### 20.2 Declaration change vs execution change

The dividing line: **a declaration change creates a `definition_revision` and its matching
epoch; an execution-property change creates an `evaluation_epoch` only.** Both are recorded,
both are reproducible, and neither is ever inferred at read time.

| Case | Ruling |
|---|---|
| **SLI set changed mid-window** | New definition revision plus matching epoch. The window is returned as segments across the boundary (§12.1); no aggregate is offered across it. |
| **Member interval changed 30s → 5m** | New evaluation epoch, not a definition revision. The interval is not a statement about what availability means, but it is an input the evaluator reads, so it is snapshotted (§6.3) and a recompute of an older range uses the interval in force then. The consequence is stated rather than hidden: a coarser interval yields more held state and, past the deadline, more UNKNOWN, which moves the coverage denominator without changing the definition. |
| **Member target changed** | New evaluation epoch. r2 would have produced none, because its snapshot did not contain the target — §6.3 exists for exactly this case, and §12.1 relies on it. |
| **Credential rotated** | New evaluation epoch if the referenced secret's generation or identity changed; none if only the ciphertext was re-encrypted. Secret material never enters the snapshot or the hash. |
| **Region added or removed** | `region_policy` is a declaration; a member's own region is an execution property and therefore an epoch. Adding a vantage point creates no revision, and the expected region set is recomputed from the new snapshot (§9.4). |
| **One region has no data** | Its members are UNKNOWN there, and the region combination treats it as UNKNOWN rather than absent: silently dropping it would let two dark regions look like unanimous health. |
| **Confirmation / failure thresholds changed** | Neither axis. The evaluator reads raw `heartbeat.up` (§6.7). |
| **Rename that bumps `execution_revision` but changes no evaluation input** | Snapshot hash unchanged, so no epoch is created (§6.2). |
| **Service edit at 12:00:10, monitor edit at 12:00:40, both effective 12:01** | The monitor edit's epoch wins the boundary and resolves the winning DECLARATION at that boundary — `rev2`, the pending same-boundary revision — not the revision that happened to be active when it was written. r3 left this unstated, and the natural reading ("the revision active now") produced a boundary governed by `rev2` whose only epoch pointed at `rev1`: a foreign key that holds and a meaning that does not (§6.2). |
| **The same two edits in the opposite order** | Same outcome by the same rule; both interleavings are required tests. |

### 20.3 Aggregation and eligibility

| Case | Ruling |
|---|---|
| **Two SLI members disagree** | This is the case `aggregation_policy` exists for (§9). No implicit tie-break: `all`, `any` and `quorum(degraded_min, healthy_min)` are the stated options and one is always in force. |
| **`degraded_min = 3`, one of three members in maintenance** | The threshold clamps to the eligible cardinality (§9.3), so two good members are GOOD, and `declaration_weakened` is recorded and surfaced. r2 returned BAD here — a failure verdict manufactured by a planned exclusion, which would also let an execution change invalidate a file-owned declaration. |
| **All declared members excluded** | The region is EXCLUDED (declared out of scope) and contributes `excluded_duration`; the bucket is never GOOD (§9.4). |
| **All members ignored as unknown, none known** | UNKNOWN, and specifically **not** EXCLUDED (§8.1). Coverage must feel this. |
| **Member GOOD 20s then UNKNOWN 40s inside one bucket** | The reducer emits `good_duration = 20s`, `unknown_duration = 40s` (§7.2). r2 had no answer here at all: two defensible readings gave two different budgets. |
| **Member in maintenance for 40s of a 60s bucket** | `excluded_duration = 40s`; the remaining 20s is evaluated normally. Crediting a whole GOOD bucket from 20s of evidence, or discarding the bucket entirely, are both wrong by up to 40s. |
| **SLI member is a `push` monitor** | Normalization belongs to the monitor type (§7.1): past its dead-man TTL a push member is BAD, not UNKNOWN. Mixing push and active probes in one SLI is legal and means exactly what each type already means. |
| **SLI member is itself a composite** | Permitted (§5, §9.5). The composite's own `all|any` produces its normalized state, which enters as one member. The UI must render the nesting. |
| **`mode: all`, members GOOD and UNKNOWN, `missing_data_policy: ignore`** | GOOD. The ignored member leaves via `i`, so `n = 1` and the known GOOD decides the interval (§9.3). r3's table had no `i`: the member stayed in `u` and the table returned UNKNOWN, contradicting §8.1 in the same document. |
| **Every member ignored, none known** | UNKNOWN, and specifically not EXCLUDED — the distinction `i` exists to preserve (§9.4). |
| **Service with an empty `sli[]`** | Short-circuits ahead of the aggregation: a revision and a matching empty epoch for declaration history, no facts, no handshake, no watermark, no SLO, and the threshold validation does not apply (§9.5). r3 both permitted this state in §5 and outlawed it in §9.4's "`d = 0` cannot occur". |
| **Bucket HEALTHY throughout vs HEALTHY for 30s then DEGRADED for 30s** | Distinguishable: both have `good_duration = 60s` on the availability axis and differ on the health axis (§9.1, §10.2). Under r3's single axis they were byte-identical, so the degraded half was unrecoverable once the raw heartbeats aged out. |

### 20.4 Storage, sealing and time

| Case | Ruling |
|---|---|
| **SLO window longer than raw retention** | Derived facts are not pruned by heartbeat retention (§10.6), so the window is a question about FACTS. It is offered only if storage continuity and decidable coverage both pass (§11.2); otherwise `partial`/`unavailable` with the reason and the available-since date. |
| **Ingest commit racing a seal** | Decided by the row-lock handshake of §10.4, not by a bare visibility claim. The loser is written as a durable late-arrival record under the same lock, so it is renderable months later — the exact gap r2 could not close. |
| **Duplicate redelivery of a heartbeat already counted, arriving after the seal** | A full no-op. The raw insert affects zero rows, so the handshake never runs (§10.4). r3 said "every heartbeat ingress" and would have filed an already-counted heartbeat as data the seal excluded — false evidence in exactly the surface built to explain a disagreement. |
| **Historical heartbeat for a bucket whose SLI membership has since changed** | Membership is resolved as of `bucket_start(ts)` (§10.4). A member removed from the SLI afterwards still routes the arrival, because the bucket's fact was produced by an epoch containing it; a member added afterwards does not, because it was not a member then. r3 read today's reference rows and got both directions wrong. |
| **One historical agent batch of thousands of rows landing after a seal** | Recorded as aggregated per `(service, bucket, monitor)` with counts and bounded examples, not one retained row per event (§10.4), and the batch takes `(service_id, bucket_start)` keys in ascending order so two overlapping batches cannot deadlock (§15.4). |
| **Late heartbeat arriving after seal** | Still recorded as raw data; the sealed fact does not change. A sealed bucket and raw history can legitimately disagree, and the disagreement always has a stored explanation. The knob is `late_arrival_grace` alone — never `result.allowed_skew`, which bounds a fast worker clock and says nothing about arrival lateness. |
| **Recompute changes the budget by 80%** | Not blocked — a correction that large usually means the previous number was wrong — but never silent: leader-only, audited, with the affected range and before/after availability (§10.6). |
| **Leader failover mid-recompute** | Batches are bounded (§10.10) and each commits its own bucket range, so a failover loses at most the in-flight batch; the successor resumes from the durable cursor (§10.8). Fencing is the ownership migration of §10.7 — not a reuse of `LeaderSession`, which r1 claimed and which is false about this codebase. |
| **Two replicas attempting recompute** | Only the elected leader computes (§10.7). The election mechanism stays the existing advisory lock; what changes is that lock ownership and the writing connection become the same thing. |
| **Revision requested at `12:00:30`** | `effective_at = ceil_to_bucket(12:00:30) = 12:01:00` (§6.4). A bucket never has two epochs. |
| **Two writes at `12:00:10` and `12:00:40`** | Both target `12:01:00`; the later wins under the per-service lock and the earlier becomes `superseded_before_effect`, retained for audit, referenced by no fact, its ranges cancelled in the same transaction (§6.5). |
| **New epoch arrives while an older backfill is half done** | The new epoch enqueues its own disjoint range; the unfinished historical range is NOT cancelled (§10.8). r2 superseded it and stalled `sealed_through` at the hole forever. |
| **Missed `NOTIFY`** | Durable ranges plus mandatory periodic resync (§10.8): latency, never a lost recompute. |
| **Scheduler loses the lock mid-batch** | The batch runs on the lock-owning connection, so it aborts (§10.7). A pooled write would have committed behind the new leader. |
| **Service or project deleted mid-work** | Ranges are cancelled inside the deleting transaction, and every cascade is project-scoped (§16). |

### 20.5 Maintenance

| Case | Ruling |
|---|---|
| **Old expired window deleted for hygiene** | `archive`. Sealed past keeps its exclusion and its provenance; no raw data is needed; always permitted (§10.9). |
| **Active window ended early** | `archive` with `cancel_effective_at` at the **exact database statement time** of the cancelling transaction — never rounded to a bucket boundary (§10.9). Already-sealed time is untouched. |
| **Window created over last night, already sealed** | Retroactive repair by range: preview, audit, before/after, bounded by raw availability. It is never silently accepted with no effect. |
| **Window created over a range older than raw retention** | `409 unrecomputable_range` with the earliest repairable instant. Fail-closed is honest here because the operator explicitly asked to rewrite the past; a partial application that fixes 30 days and claims 90 is not. |
| **Window spanning sealed and future time** | One operation: the raw-availability fence failing rolls the whole thing back (§10.9). |
| **`annul` of a window that excluded a real outage** | Permitted as the privileged retroactive action; the outage returns to the number, the audit records before/after, and reads in the range return `repairing` until the repair completes rather than serving the old number. |
| **Project-wide window on a large project** | Generation bump plus durable ranged repairs; no unbounded synchronous lock set inside the mutating transaction (§10.9). |
| **Two maintenance mutations during one repair** | The batch CAS on `maintenance_generation` fails and the remaining range is re-enqueued (§10.9). Without the CAS, two batches of one range could read two different "current" declarations. |
| **Preview covers services A and B; an affected service C is created before confirm** | `409 preview_stale`, naming C. The set is re-resolved inside the mutating transaction under its own locks and must be exactly equal (§10.9) — neither `maintenance_generation` nor A's and B's `definition_generation` would have moved, so a row-only CAS would have confirmed and mutated a service nobody previewed. |
| **Preview covers A and B; an existing service C adds the monitor to its `sli[]` before confirm** | Same ruling by the same rule: C enters the affected set, the set differs, the token is stale. |
| **Preview covers A and B; B is deleted before confirm** | Stale, not a silently smaller mutation. Set equality is required in both directions. |
| **Preview is monitor-scoped; an unrelated service elsewhere in the project changes its definition** | The token stays valid — that service is not in the affected set. This is why the set is re-resolved rather than guarded by a project-wide counter, which could not tell the two cases apart. |
| **Service C created *concurrently*, between confirm's re-resolution and its commit** | Impossible, and this is the difference between narrowing the race and closing it. Row locks protect rows that exist; C does not exist yet, so no row of C could have been locked. Confirm holds the project `service_membership` advisory lock across the re-resolution and the commit, and Service create takes the same lock first (§10.9) — so the creating transaction blocks until confirm commits, and then cannot slip into a mutation that has already been applied. The acceptance test is a barrier: hold confirm after re-resolution, start a concurrent create or SLI-add, assert it blocks, release confirm, assert the creator observes the committed mutation rather than joining it. Delete and SLI-removal are the mirror cases. |
| **Window archived, then an unrelated audited recompute covers a bucket it excluded** | The exclusion stands: the evaluator reads the retained row's effective span regardless of `archived_at` (§10.9). r3 had the evaluator read "only the current, non-archived declaration", which would have turned every archive into an annul on the next recompute — a sealed bucket changing with no preview, no raw fence and no audited intent. |
| **Active window cancelled at 12:00:37** | The exclusion ends at 12:00:37 exactly. The piecewise reducer handles arbitrary maintenance edges, so `cancel_effective_at` is the database statement time and is not rounded to a bucket boundary — r3 said both "ends now" and "from the mutation's bucket boundary" in one section. |

### 20.6 Coverage and reporting

| Case | Ruling |
|---|---|
| **All UNKNOWN except one GOOD** | Storage continuity passes, decidable coverage fails, and the window is `partial`/`unavailable` with the fraction (§11.2). Never 100%. |
| **Short burn window entirely after `sealed_through`** | `insufficient_sealed_coverage` (§11.3), not zero burn. |
| **Window spanning two definition revisions** | Segments with no aggregate across the boundary (§12.1). |
| **Backfilled 90 days on a service created today** | Reported, but every backfilled segment is labelled a declared reconstruction (§6.6) — it is what the window looks like under today's declaration, not evidence about the past configuration. |
| **Objective raised from 99.9 to 99.99** | The budget changes instantly over identical facts; the response states which objective produced it and the timeline annotates the change (§11.3). |

### 20.7 Coexistence

| Case | Ruling |
|---|---|
| **Zero Services after upgrade** | Nothing changes: no service rows, no facts, no ingest handshake rows, no scheduler work, and monitor-level SLO behaviour is untouched (invariant 46). The feature costs nothing until a service exists. |
| **Format-1 bundle after the upgrade** | Still valid, declares no services, and its monitors receive server-assigned slugs (§15.3). |
| **Composite converted to Service** | Phase 4 (§15.5): conversion creates the service and CHANGES NOTHING about the composite — it keeps probing, keeps its SLO, stays listed at full strength, and carries a two-ended link. Only an explicit `retire` disables it, behind a warning naming the loss of its SLO and alerts. A composite remains a monitor and may itself be an SLI member. |
| **Existing manual status component preserved** | Phase 4 (§15.0): manual stays first-class; a component's source changes only through an explicit previewed action, and `manual_status` is preserved so the revert restores exactly what customers saw before. |
| **Service dependency cycle** | Does not arise in phase 1/2: dependency edges are phase 3 (§14) and no edge type exists yet. Cycle rejection is a precondition of that phase, not an afterthought. |

## 21. Resource and write contract (phase 1)

**Service.** Stable `id`; project-unique immutable `slug` (the MaC reference key and the URL
segment); `name`; optional `description` bounded in length; `owner` as a reference to an
escalation policy and/or an on-call schedule (§4), never free text; `tier` admitted only if
phase 1 gives it behaviour, otherwise absent. Deletion is explicit, audited, and blocked while
a status-page projection exists (§15); incident history survives via the existing snapshot
columns.

**Declaration writes.** Creating or editing membership, `sli[]` or any policy produces a new
definition revision and its matching epoch — there is no in-place edit of a revision.
Concurrency is optimistic: the caller submits the revision it observed and a mismatch is a
`409`, so two operators editing an SLI cannot silently interleave. Every revision carries
`created_by` and an audit entry written in the same transaction.

**Policy schemas** are strict and closed: unknown keys rejected, every field with a stated
default (§10.10), and `aggregation`/`region` thresholds validated at write time under §9.5. The
same validator serves the API and the file provider.

**Read API.** Service detail returns the current declaration, the current epoch, and the
reliability payload of §11.2 (`as_of`, `sealed_through`, both axes' summed durations, coverage,
segments, `repairing` intervals, reconstruction labels). A progress and error surface exposes
durable range state (§10.8) so a running backfill or repair is visible rather than looking like
missing data.

**Facts.** Primary key `(service_id, bucket_start)`, `project_id` carried for tenant-safe
composite FKs, partitioned by `bucket_start` (§3), retention independent of heartbeats and at
least the longest supported window. Deleting a service removes its facts, ingest rows,
late-arrival records and ranges in one transaction.

**Metrics and readiness.** Low-cardinality counters for late-excluded arrivals, repair and
recompute outcomes, range states, epoch fan-out and `unrecomputable_range` rejections; the
scheduler's readiness must not report healthy while service work is wedged.

## 22. Deliverables (process)

Spec review (adversarial design pass, per the practice that found four P0s in FR-020 before a
line of code) → **UI mock approval** (done — recorded in
`docs/design/notes.md`, implement 1:1) → phase-1 implementation on its own branch → `-race` in **both** storage modes → E2E on
a live stack → decision record **D-0159** (the two-axis model, the evaluation-semantics
projection, the canonical bucket and duration-weighted facts, sealing with the ingest handshake,
maintenance archive versus annul, leader fencing on the lock-owning connection, bundle format 2
with the monitor slug, the native-RANGE storage decision and the TSDB boundary) → iteration
report `docs/iterations/iter-NNNN.md` → a row in `docs/traceability.md` → `docs/overview.md` when
behavior or stack changes.

**Phase 4 process** (commissioned 2026-08-17): this §15.0/§15.5 amendment reviewed as a DESIGN
round → UI mock approval for the component-source picker, the conversion preview dialog and the
composite links **before** any frontend code → implementation → `-race` in both storage modes →
E2E on a live stack including the PUBLIC page rendering `no_data` → decision record **D-0167**
(the third component source and its exclusivity; the SLI-only projection with impacts staying
non-public; `no_data` as a public status and its summary semantics; the corrected `pending`
mapping as an intentional change to shipped public behaviour; sealed-facts-only strips;
reversible previewed conversion; composite retire) → iteration report + status + traceability
rows at completion.

**Phase 3 process** (commissioned 2026-08-17): this §14 amendment reviewed as a DESIGN round →
UI mock approval for the §14.5 surface **before** any frontend code → implementation on
`feat/service-reliability` → `-race` in both storage modes → E2E cascade scenario on a live
stack → decision record **D-0166** (edges outside the declaration; the `service_graph` lock;
the dedicated `incident_correlation` outbox topic, its one-transaction attempt and the
whole-lifecycle claim fence for mixed-version fleets (immutable `fenced` column + demotion
CHECK + class-restoring retry/replay); the `graph_generation` token and
same-transaction edge audit; the desired-edge delete guard; the canonical endpoint-inclusive
root-first shortest-lexicographic path) → iteration report + status + traceability rows at
completion, per the standing convention.
