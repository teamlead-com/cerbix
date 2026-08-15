# func-service-reliability — Service as a reliability-domain resource (FR-021 / NFR-016)

Status: **DRAFT r1 — design captured from the owner/architecture discussion; NOT yet
reviewed, NOT approved, no implementation.** Phases 1–2 are specified to implementable
depth; phases 3–5 are recorded as intent only and are deliberately left unspecified until
phase-1/2 data exists in a production-like workload.

> **Terminology.** A Cerbix **Service** is a reliability-domain resource representing an
> operational service. It is **not** a Kubernetes Service, a systemd service, or a
> general-purpose service-catalog entry.

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
- **NFR-016**: Reported service reliability is **explainable and reproducible**. Every stored
  reliability fact carries the SLI-definition revision that produced it; changing what
  counts as availability never silently rewrites history; a window is never reported unless
  it is fully covered by materialized facts; identical input, revision and policy reproduce
  identical output; and the absence of reliability inputs is never rendered as perfect
  reliability.

## 3. Scope, non-goals, and the time-series tripwire

**In scope (phases 1–2):** the Service resource; monitor membership; SLI definition with
immutable revisions; effective-state model; aggregation policies (including region-aware and
quorum); the service reliability fact store with sealing; leader-owned materialization,
recompute and backfill; service SLO / error budget / burn-rate reporting; window-availability
honesty; two-layer health presentation; optional adoption.

**Explicit non-goals.** Cerbix materializes **exactly one derived reliability timeline per
Service per SLI revision**, retains only what the supported SLO windows require, and computes
a fixed set of reliability aggregates. Cerbix does **not**: serve arbitrary time-series
queries; store generic telemetry; support user-defined downsampling; expose a query language;
act as a metrics backend; provide a service catalog (repository links, documentation, tech
stack, deployment metadata, generic scorecards belong to an external catalog).

**Tripwire.** The moment this feature requires arbitrary series, configurable downsampling or
a query language, it has left its scope — that is a signal to stop, not to add a sprint.

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

A Service belongs to exactly one project (`org_id`, `project_id` carried for tenant-safe
composite FKs, as elsewhere in the schema).

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
INVARIANT:  monitors[] ≠ sli[]
```

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

## 6. SLI definition revisions

The SLI definition is an **immutable, versioned tuple**, not a mutable field set:

```
SLIRevision
├── revision          (monotonic per service)
├── effective_at
├── members[]         (monitor refs)
├── aggregation_policy
├── region_policy
├── missing_data_policy
├── maintenance_policy
└── created_by / audit
```

Every stored reliability fact is stamped with the revision that produced it — the revision is
part of the **fact**, not audit metadata about the service:

```
not:  "Service currently has sli_revision 5"
but:  "this reliability fact was produced under revision 5"
```

Changing any element of the tuple creates a new revision. The prior revisions remain readable
so that "why did the budget change?" always has an answer:

```
rev 4  2026-08-10  sli: [checkout-http]
rev 5  2026-08-14  sli: [checkout-http, checkout-grpc]
```

**Recompute on revision change** is bounded by physics (§10.4): facts are recomputed where raw
heartbeats still exist; older facts keep the revision under which they were produced. A window
therefore may legitimately span revisions —

```
90d window:  [ rev 3 ][ rev 4 ][ rev 5 ]
```

— and the UI must render the revision boundaries explicitly. A segmented, labelled window is
honest; a single number silently composed of two different definitions of availability is not.

## 7. Effective monitor state (sample-and-hold)

Service aggregation samples **member state at an instant**, not per-bucket counters. Counters
cannot express cross-monitor simultaneity, and members have heterogeneous intervals (30s and
5m in one service is normal).

```
monitor observation
    ↓  normalization owned by the monitor TYPE
normalized state: GOOD | BAD | UNKNOWN
    ↓  held effective until
min(next_observation, stale_deadline)
    ↓
service aggregation samples effective member states
```

**Normalization is the monitor type's responsibility**, not the service's: an active probe and
a dead-man's-switch (`push`) disagree on what a missing observation means — for the former it
is uncertainty, for the latter it is often the failure itself. The service layer never
re-guesses this; it consumes `GOOD|BAD|UNKNOWN`.

**Freshness invariant:** Cerbix never holds `GOOD`/`BAD` indefinitely. Each member has a
`stale_deadline` derived from its type and interval (active probes: a small multiple of the
interval; `push`: its own TTL rule). Past the deadline the effective state becomes `UNKNOWN`.

Because state is *held*, a canonical fixed bucket is workable: a 5-minute-interval member has
a defined state in every intervening 60s bucket until its freshness deadline. This also makes
the service timeline and the SLO math the same model rather than two parallel interpretations.

## 8. Missing data and maintenance

- `missing_data_policy` (part of the revision) decides how `UNKNOWN` members are treated by
  the aggregation: `unknown` (bucket may become undecidable → excluded), `bad`, or `ignore`.
  The default must be consistent with the existing monitor SLI semantics.
- `maintenance_policy` (part of the revision): a member inside a maintenance window is
  excluded, mirroring today's rule that maintenance heartbeats leave both numerator and
  denominator. If exclusion leaves the bucket undecidable under the policy, the bucket is
  `UNKNOWN` and is excluded from the window (never counted as good).

## 9. Aggregation semantics

`aggregation_policy` and `region_policy` are part of the revision, because changing them
changes the meaning of history exactly as changing members does.

Policies must cover Cerbix's multi-region reality: the same logical check frequently runs from
`core`, `geo1`, `geo2`, and neither `all` nor `any` is correct there (`all` marks a service
down when one vantage point breaks; `any` calls it healthy when two of three regions are
dark). Phase 1 therefore specifies a policy object rather than a boolean mode, e.g.:

```yaml
aggregation:
  mode: quorum          # all | any | quorum
  healthy_min: 3
  degraded_min: 1
region:
  mode: per_region      # members sampled per region, then combined
  healthy_min_regions: 2
```

`2-of-3` and "one region down ⇒ degraded but available" are different statements and both must
be expressible. Nested members (a `composite` inside the SLI) are permitted: the composite's
own `all|any` produces its normalized state, which then enters the service policy. The UI must
show that nesting, otherwise a budget derived from `quorum(any(A,B,C), HTTP, synthetic)` is
unexplainable to the person reading it.

## 10. Storage and computability (the phase-1 core)

### 10.1 Why a new fact store is required

`heartbeats_daily(monitor_id, day, up, total)` cannot serve service aggregation: knowing
"A was up 1400/1440" and "B was up 1430/1440" does not say whether they were down *at the same
time*. Boolean cross-monitor logic needs bucket alignment, not daily counters. Raw heartbeats
are retention-bounded (`heartbeats.retention_days`, default 30) while `StandardWindows`
includes 90d. Therefore service reliability needs its **own materialized facts**.

### 10.2 Fact shape

```
service_reliability_buckets
├── service_id, project_id        (tenant-safe composite FK)
├── sli_revision                  (the fact's producing definition)
├── bucket_start, bucket_size     (canonical 60s in phase 1)
├── verdict                       GOOD | BAD | UNKNOWN
└── state                         PROVISIONAL | SEALED
```

Hour/day rollups are derived from sealed canonical buckets; the canonical granularity must
stay fine enough for the shortest supported burn window, otherwise future burn semantics are
foreclosed by the storage choice.

### 10.3 Bucket lifecycle and sealing

```
OPEN → PROVISIONAL → (bucket_end + result.allowed_skew + grace) → SEALED
```

Late-arriving results (ingest tolerates skew; pull agents post asynchronously) may change a
**provisional** bucket. A **sealed** bucket is never mutated by ordinary ingest. Sealing is
part of the correctness model, not an optimization: without it the budget silently drifts
between two viewings and the number becomes untrustworthy.

A sealed bucket may be rewritten in exactly two cases, both audit-visible:

1. an explicit SLI-revision recompute;
2. an administrative repair/backfill operation.

### 10.4 Recompute, retention and backfill

- **Recompute range is bounded by raw heartbeat availability.** Beyond raw retention, prior
  facts keep their original revision (§6).
- **Derived facts are NOT pruned by heartbeat retention.** They must outlive raw data — that
  is the only way a 90d window exists at all. Their retention is independent and at least as
  long as the longest supported window (the `heartbeats_daily` precedent: frozen rollup rows
  survive raw partition drops).
- **Backfill on service creation**: a new service whose members already have history is
  backfilled within raw retention, as a bounded leader-run background job with visible
  progress. Without it, a freshly created service reports "insufficient history" while the
  data sits one table away — a bad and avoidable first impression.
- **Window availability invariant**: a window is offered only if materialized facts cover it
  fully. `90d` on 14 days of history is reported as unavailable/partial with the available-
  since date — never rendered as a complete `99.95% / 90d`.

### 10.5 Ownership, fencing, single computation path

Only the **elected scheduler leader** materializes, seals, recomputes and backfills service
facts. API and MaC only commit a new SLI revision and publish a change event; they never
compute. Fencing reuses the existing `LeaderSession` (the advisory-lock-owning connection, so
losing leadership aborts in-flight work) — no new epoch mechanism is introduced.

```
API / MaC → commit SLI revision → NOTIFY → scheduler leader
          → recompute eligible range → write revision-tagged facts
```

**One computation path.** The incremental pass MUST be literally "recompute this bucket
range", i.e. the same implementation the batch recompute uses, invoked with different bounds.
Two code paths (streaming vs batch) inevitably diverge at the edges, and the divergence is
undetectable in normal operation. The determinism invariant is therefore stated as: *the
incremental result is byte-identical to a batch recompute of the same range*.

### 10.6 Bounded work

Phase 1 carries explicit bounds in the spirit of the file provider's `max_managed_monitors`,
reconcile semaphore and statement/lock timeouts: caps on services per project and members per
SLI; recompute and backfill executed in bounded batches with an explicit budget; statement
timeouts; and a guarantee that recompute never starves the scheduler's own cadence loop.

## 11. SLO, error budget, burn rate

Per sealed bucket the aggregation yields `GOOD | BAD | UNKNOWN`. Over a window:

```
availability = good / (good + bad)          UNKNOWN excluded from both terms
error budget = (1 − objective) semantics, reusing internal/sla
burn rate    = multi-window, reusing the existing rule shapes
```

`sla_targets` gains `service_id` as a third exclusive scope alongside `monitor_id` /
`project_id` (the existing CHECK becomes a three-way exclusivity).

**Reporting only in phase 2.** Service SLOs are computed and displayed but do **not** alert
(§13).

## 12. Health vs reliability presentation

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
service is wrong. Labels must make the SLI layer read as the authoritative customer-facing
one.

**Public projection invariant:** a public Service status is derived **only** from declared SLI
semantics. A diagnostic monitor can never degrade a public component unless it participates in
the SLI.

## 13. Alerting boundary

Phase 1–2: service SLOs **calculate = yes, display = yes, alert = no**.

Monitor-level burn alerting already exists and pages through the same channels. Turning on
service burn alerts without an ownership rule would page twice for one failure — the exact
noise this specification claims to reduce. Alert ownership (which level is the single source
of paging semantics, and how a monitor in an SLI delegates or suppresses its own burn alert)
is phase 5 and is deliberately unspecified here.

## 14. Dependencies and incident correlation (phase 3 intent)

Three behaviors must never be conflated:

| Behavior | Source | Effect |
|---|---|---|
| alert suppression | monitor `depends_on` | mutes **delivery** only (exists today, fail-open) |
| incident correlation | service dependencies | **links** facts |
| impact presentation | service dependencies | UI / status representation |

**Invariant:** the service dependency graph never suppresses the recording of facts or the
creation of incidents. It annotates (`root cause probable: payments`, `affected: checkout,
subscriptions`). This preserves the project's existing philosophy — facts always keep
recording, only delivery is muted — and prevents a real second incident from being hidden
under a "root" one. The service graph is the canonical *impact* graph; monitor `depends_on`
remains the low-level *suppression* override; the former may recommend the latter but they are
not required to be identical.

Detailed semantics are deferred (§17).

## 15. Coexistence and migration

**Status pages.** Service projection becomes the default *managed* component type; **manual
components remain first-class** (the "third-party provider: degraded" case that Cerbix does
not monitor). Existing components are never converted automatically: an explicit
"convert to Service-backed component" action with a preview of the resulting public change —
status pages are customer-visible artifacts.

**Composite.** After Service exists, `composite` narrows to what it actually is: a logical
monitor. Conversion is explicit and non-destructive:

```
Convert composite → Service
  creates: Service (monitors = children, aggregation = all/any)
  keeps:   the composite, its historical SLO, incidents, timeline
  marks:   composite deprecated / superseded
  requires: explicit confirmation of sli[] — never silently "all children"
```

No historical data is migrated or reinterpreted in v1.

**Monitoring as Code.** Services are declared in bundles as a new top-level resource map
(explicitly anticipated by `func-monitoring-as-code` §3.2). Reference semantics reuse the
existing guarded-reference contract built for secret refs — normalized ref rows, deferred
tenant-safe FK as a commit-time guard, `409` on delete, and a per-project reconcile freeze
with last-known-good on a dangling reference. Inventing a second reference semantics would be
a regression in consistency.

```
Resource ownership determines who may MUTATE a resource, not who may REFERENCE it.
```

A UI-managed Service may reference a file-managed monitor; it may not modify or delete it. A
bundle that removes a referenced monitor does not silently tear a dependency out of UI
configuration: the delete is blocked while the reference exists, or reconciliation enters the
frozen/failed state with a bounded, explicit reason.

**Incident history** survives service deletion via a snapshot (`service_id` nullable +
`service_name_snapshot`); an active status-page projection blocks deletion until explicitly
detached.

## 16. Backward compatibility (acceptance criterion, not a footnote)

```
- zero Services is a valid installation state
- every existing Monitor remains valid without a service
- bundle format v1 remains valid
- existing status pages render unchanged
- existing composites retain behavior
- existing incidents and monitor SLOs retain semantics
```

Service-aware UX begins only after explicit adoption. Upgrade day must not present an empty
"Services" screen as the product's new front door.

## 17. Delivery phases

| Phase | Content |
|---|---|
| 1 | Domain + storage foundation: Service resource, SLI revisions, effective-state model, bucket schema, materialization/seal/recompute/backfill pipeline, bounds. No alerting, no status projection, no correlation. |
| 2 | Reliability reporting: service SLO, error budget, burn-rate computation, revision boundaries, insufficient-history UX, two-layer health card. |
| 3 | *Intent only.* Dependency impact graph, incident correlation/annotation. |
| 4 | *Intent only.* Status-page service projection, manual-component coexistence, composite conversion tooling. |
| 5 | *Intent only.* Alerting ownership: service burn alerts and monitor delegation/suppression rules. |

Phases 3–5 are **deliberately not specified**. Their UX depends on facts phase 1/2 will
produce — real `UNKNOWN` density, late-arrival behavior, recompute cost — and specifying them
now would encode assumptions that the data may refute.

## 18. Acceptance invariants

**Phase 1** (accepted only when all hold):

1. effective monitor state is built by sample-and-hold with a finite freshness TTL;
2. every member is normalized to `GOOD|BAD|UNKNOWN` by its monitor type before aggregation;
3. every stored service fact carries its `sli_revision`;
4. late data may change only unsealed buckets;
5. sealed buckets are never mutated by ordinary ingest;
6. sealed history is recomputed only by the scheduler leader, only on revision change or an
   audited repair;
7. recompute is bounded by raw heartbeat availability; older facts keep their revision;
8. the incremental pass is byte-identical to a batch recompute of the same range;
9. a former leader cannot write after losing leadership (existing `LeaderSession` fencing);
10. materialization, recompute and backfill are bounded and never starve the scheduler cadence;
11. derived facts are not pruned by heartbeat retention;
12. no generic time-series primitives are introduced (§3 non-goals).

**Phase 2** adds:

13. windows longer than available materialized history are reported unavailable/partial, never
    as a complete SLO;
14. a Service without SLI has no SLO and no budget — never 100%;
15. adding a monitor to `monitors[]` does not change the SLO until it is explicitly added to
    `sli[]`;
16. an SLI change produces a new revision and a visible boundary on the budget timeline;
17. existing monitor-level SLO behavior is unchanged;
18. service SLOs do not alert.

Invariant 15 is the single sharpest test of whether this design succeeded: if it holds,
Service is a reliability-domain object; if it fails, Service is still a grouping abstraction.

## 19. Adversarial cases (must be answered by the spec before code)

SLI member deleted · SLI member moved to another project · SLI set changed mid-window ·
member interval changed 30s→5m · region added/removed · one region has no data · maintenance
on a single SLI member · two SLI members disagree · SLI member is itself a composite (nested
aggregation) · SLI member is a `push` monitor (time-based up semantics) · a monitor in the SLI
of two services · service with empty `sli[]` · SLO window longer than retention · service
dependency cycle · UI-managed Service referencing a file-managed monitor · bundle removes a
referenced monitor · composite converted to Service · existing manual status component
preserved · zero Services after upgrade · recompute changes the budget by 80% · late heartbeat
arriving after seal · leader failover mid-recompute · two replicas attempting recompute.

## 20. Deliverables (process)

Spec review (adversarial design pass, per the practice that found four P0s in FR-020 before a
line of code) → phase-1 implementation on its own branch → `-race` in **both** storage modes →
decision record **D-0159** (the storage/computability model, revision-as-fact, sealing,
ownership and the TSDB boundary) → iteration report → `status.md` (`FR-021`, `NFR-016`) →
`traceability.md` → cross-reference from `func-sla-sli.md` (service scope) and
`func-monitoring-as-code.md` §3.2 (new top-level resource map).
