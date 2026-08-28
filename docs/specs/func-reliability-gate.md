# func-reliability-gate — a deploy asks whether the error budget allows it (FR-024 / NFR-019)

> **Lifecycle: DEFERRED by the owner on 2026-08-28 (D-0202) — design approved at revision 13 (party [47]),
> UI mock static-contract approved (party [59]), no implementation scheduled.** The requirement was proposed
> by the reviewer as a continuation of FR-021, not asked for by cerbix's users; the owner parked it until a
> CI-side ask exists. Everything below stays the approved design and needs no re-approval to resume — only
> a fresh focused review of the codebase it lands on. The
> reviewer re-read the working tree independently: no findings remain. Conditions carried forward: the
> owner-approved UI mock gate precedes any SPA code; implementation must discharge all 21 invariants of §6
> and the full §7 matrix with named tests and mutations; the PostgreSQL 15.8 prepared-statement case is a
> required implementation gate to RUN, not a claim; FR-024/NFR-019 stay TODO until then. Revisions 10–13
> were committed together as one accepted design phase, per the owner. Revision 1
> (`46379fa`, `c65ea5d`) was rejected with 4 P0, 9 P1, 3 P2 (party [15]); revision 2 (`113308f`) with
> 2 P0, 5 P1, 4 P2 (party [20]); revision 3 (`a16ea70`) with 4 P1, 4 P2 (party [25]); revision 4
> (`d540ae4`) with 3 P1, 5 P2 (party [27]); revision 5 (`3771622`) with 3 P1, 3 P2 (party [30]);
> revision 6 (`4e5bd15`) with 5 P1, 2 P2 (party [33]); revision 7 (`476643a`) with 1 P0, 7 P1, 3 P2
> (party [35]); revision 8 (`63c1d57`) with 8 P1 and a P2 set (party [37]); revision 9 (`80a89ff`) with
> 6 P1 and a P2 set (party [39]); revision 10 (working tree) with 7 P1, 2 P2 (party [41]); revision 11
> (working tree) with 4 P1, 3 P2 (party [43]); revision 12 (working tree) with 2 P1, 3 P2 (party [45]).
> Every finding of every round is addressed below and named where it changed the text. The owner closed
> D9–D13 (D-0188), the four questions of round 1 (D-0189), the seal-lag bound of round 2 (D-0190), the
> ledger's capacity contract of round 5 (D-0193) and its partition period of round 6 (D-0194); rounds 3,
> 4 and 7–12 raised no owner question (D-0191, D-0192, D-0195, D-0196, D-0197, D-0198, D-0199, D-0200). `make docs-check`
> refuses the retired spellings of every earlier revision, inside the normative schema block too, and
> its guard has fixture tests (§10). One gate remains before SPA code: the owner-approved UI mock of the policy editor and the decision
> history. Backend implementation may begin under the approved design; FR-024/NFR-019 stay TODO until
> implementation discharges every invariant of §6 and the §7 matrix. Change Intelligence is **FR-025** and is not
> part of this requirement.

## 1. What this is, in one paragraph

cerbix already computes, per service and per SLO window, everything a release decision needs: how much of
the error budget is consumed and whether it is burning (the reliability report and the burn evaluator,
FR-021 phase 2), whether a service incident is open (FR-022), and how fresh all of that is (the seal
watermark and the evaluators' leases). What it does not do is turn those facts into a **decision a
pipeline can act on**. The Reliability Gate is that: a deterministic, machine-readable answer — an
observed `state` and an effective `action` — derived from facts cerbix has already sealed and evaluated,
read in ONE database snapshot, with every reason and every timestamp attached, asked immediately before
a protected pipeline step.

It is a thin slice on purpose. It adds a policy table, an override table, a bounded decision ledger, one
decision endpoint and one CLI verb. It adds **no new computation of reliability** and **no store of
change events or telemetry** — the ledger is a new, bounded store of the gate's OWN decisions, and that
is the only new thing it keeps.

## 2. Requirements

- **FR-024 — Reliability Gate.** An operator declares, per service, a policy naming the SLO window it
  governs and which reliability clauses block a release, warn, or are ignored. A pipeline, authenticated
  with an API token, asks `POST /api/v1/projects/{p}/services/{s}/gate` (or `cerbix gate check`) and
  receives a decision with all matching reason codes and the evidence they rest on, recorded in a
  bounded, append-only decision ledger. A privileged operator may record a time-bounded override; the
  gate reports that it was used. Policy and override mutations are audited; decisions are not written to
  the general audit log — the ledger is their record.
- **NFR-019 — the gate never disagrees with the screen.** Budget, burn and coverage in a gate response are
  the values the service page and the alerting badge would show for the SAME snapshot instant, because
  they are produced by the same code paths (D1) inside one transaction (D6a). A gate that computes its
  own number, or composes numbers from different instants, is a second semantics owner — a defect class
  this project has already paid for (iter-0161 §34, §45).

## 3. The decisions

**D1 — the gate reads existing owners and computes nothing. DECIDED; corrected by review P0-1.**
Revision 1 named `decideServiceWindow` as the budget owner. It is not: it owns *withholding* (may this
number be quoted at all) and availability. The remaining budget is computed AFTER it, in the report path,
by `reportObjective` + `sla.ErrorBudget` for ONE window's service-scoped target, yielding
`BurnedPercent` and `RemainingRatio`. The owners the gate consumes, precisely:

- **budget, for the policy's window, and how stale it is** — the report path
  (`serviceReliabilityReportTx`, which already takes a transaction and an `asOf`): `BurnedPercent` is the
  budget fact; `decideServiceWindow` inside it decides whether the number may be quoted, and a withheld
  number is a withheld clause, never a zero. The report path is extended to state `seal_lag`
  (`as_of − sealed_through`) so the service page shows the same staleness the gate acts on (D8a) — the
  gate does not compute it.
- **burn, for the policy's window** — the burn latch `service_burn_alert_state` rows of THAT window's
  target only (`sla_targets` is `UNIQUE(service_id, window_name)`): `last_verdict`, `firing`,
  `lease_until`, per rule.
- **open service incident** — `incidents` where `service_id`, `source = 'auto'`, `status <> 'resolved'`.
- **coverage** — `serviceCoverageClauses` in `internal/store/servicealertstate.go`, as EVIDENCE only
  (D11): it says who is paging right now, not how reliable the service is.

The gate has no formula of its own for any of these. If a clause needs a fact none of the owners
exposes, the owner is extended and the page gains it too. **Invariant 1.**

**D2 — the window is part of the policy. DECIDED (owner, 2026-08-28; review P0-1).**
A policy names exactly one `window` (a `sla.Window` name the service has a target for). Every budget and
burn clause is evaluated against that window's target and no other; a policy for a window the service
has no target for is refused at write time and, if the target is later deleted, evaluates to `UNKNOWN`
with `reason: window_target_missing`. Worst-of-all-windows was considered and declined: the answer would
depend on which windows somebody happened to add, and the evidence would be a list rather than a fact.

**D3 — the budget threshold is a consumed percentage. DECIDED (owner, 2026-08-28; review P0-1).**
`RemainingRatio` is an absolute share of time (a 99.9 objective has a whole budget of 0.001), so a
threshold of "0.10 remaining" was an error of units — every fresh budget would already be "below". The
clause is `budget_consumed_percent >= N`, `N` an integer in 1..100 per policy, default 90, and
`BurnedPercent` already owns that quantity. `budget_exhausted` is the same fact at `N = 100`, exposed as
its own clause because it deserves its own default (block) and its own reason code.

**D4 — the states, and the split between what was OBSERVED and what to DO. DECIDED; review P0-2.**
Revision 1 put `UNKNOWN` outside the `BLOCK > WARN > ALLOW` order and then let the CLI exit non-zero for
it even when the operator had chosen `warn` — a shell would have blocked against the operator's choice.
The response therefore carries two fields:

- `state` — what was observed: `ALLOW`, `WARN`, `BLOCK`, or `UNKNOWN` (some clause the policy depends
  on could not be answered);
- `action` — what the pipeline should do: `ALLOW`, `WARN`, or `BLOCK`.

The algebra, total and deterministic, evaluated over ALL clauses:

1. A clause assigned `ignore` is evaluated for evidence but contributes nothing — its unavailability
   does NOT produce `UNKNOWN`.
2. If any clause assigned `block` is KNOWN and matches → `state = BLOCK`, `action = BLOCK`, whatever
   else is unknown. A known block is never softened by an unknown neighbour.
3. Else, if any clause assigned `block` or `warn` is UNAVAILABLE → `state = UNKNOWN`,
   `action = unknown_behavior` (`warn` or `block`, from the policy, no default).
4. Else, if any `warn` clause matches → `state = WARN`, `action = WARN`.
5. Else → `state = ALLOW`, `action = ALLOW`.

`reasons[]` lists every clause that matched or was unavailable, each with its assignment, its value and
its source. So a matching `warn` clause beside an UNAVAILABLE `warn`-assigned `ticket_burn_firing`
clause is `state = UNKNOWN` by step 3 — with both in `reasons[]` — and not a `WARN`; the algorithm wins
over any sentence that reads otherwise. `NOT_CONFIGURED` is a fifth `state`
with NO `action`: the service has no policy, the response says so with `reason: not_configured` and a
documentation link, and what to do with that is the integration's visible choice. It is never rendered
as `ALLOW` or `WARN`.

**D5 — `unknown_behavior` is mandatory and has no default. DECIDED.**
A policy is created with `unknown_behavior: warn | block`, explicitly, or it is refused. A silent
`block` breaks onboarding (a fresh installation has no sealed facts for hours); a silent `warn` is a
fail-open nobody chose.

**D6 — a decision is a point-in-time advisory input, never a reservation. DECIDED.**
The gate reserves no budget, holds no lock across the deploy, and does not know whether the deploy
happened. The pipeline asks **immediately before** the protected step and stores `decision_id` as
evidence. The TOCTOU window between answer and step is named, is the pipeline's, and v1 does not try to
close it — a gate that coordinates deployments has become a deployment system.

**D6a — one snapshot, one instant. DECIDED; review P0-3.**
Revision 1 would have composed a report (its own REPEATABLE READ snapshot), a coverage read (a separate
pool query), an incident read, a policy read and an override read — five instants presented as one, an
evidence set that never existed together. The gate has ONE linearization point: a single
`REPEATABLE READ` transaction, read-write because the decision row is inserted in it, whose
**first snapshot-bearing statement** is `SELECT statement_timestamp()` — that value is `evaluated_at`,
the same instant the report path already uses as its `as_of`. "Snapshot-bearing" is the exact word:
the transaction runs inside the store's deadline wrapper (`internal/store/deadlinetx.go`), which issues
`SET LOCAL statement_timeout` and `SET LOCAL lock_timeout` first, and `SET LOCAL` is a utility command
that establishes no snapshot under `REPEATABLE READ`. The clock read is therefore the first statement
that does, and the implementation is not asked to choose between the timeout and the linearization
point (review round 3 P2-3). Inside it, in this order: the service row (tenant check), the policy,
the active override, `serviceReliabilityReportTx(tx, …, evaluated_at)` for the policy's window, the
window's burn latches, the open-incident predicate, the coverage clauses, then the ledger INSERT and
commit. Each owner is consumed through a transaction-taking variant; where one does not exist yet, it is
added to the owner — the formula is never copied. A serialization failure retries the whole transaction
once; a second failure is a transport-class error (`exit 1`, HTTP 503 `snapshot_conflict`), never a
decision.

**D7 — the answer carries its own evidence, with correct semantics and a presence contract.
DECIDED; review P1-1, P1-3.**
Revision 2 said "every response includes" a list that some states cannot physically carry:
`NOT_CONFIGURED` has no policy revision, a policy whose target was deleted has no objective, a service
never sealed has no `sealed_through`. The presence contract is therefore explicit, and the ledger's
columns are nullable exactly where it says so:

| Field | Present |
|---|---|
| `schema_version`, `decision_id`, `evaluated_at`, `state`, `reasons[]` | **always** |
| `action`, `unoverridden_action` | every state except `NOT_CONFIGURED` |
| `policy_revision`, `window`, `unknown_behavior`, `max_seal_lag_seconds` | when a policy exists |
| `target_id`, `objective`, `objective_updated_at` | when the window's target exists; absent with reason `window_target_missing` |
| `sealed_through`, `seal_lag`, `fact_revisions` | when any fact has been sealed; absent with reason `never_sealed` |
| `governing_revision` | when a declaration governs at `evaluated_at`; absent with reason `no_governing_revision` |
| `burn_leases[]` | one per rule of the target; empty when the target has no rules |
| `coverage_lease_until`, `coverage_state` | when the service has been evaluated; absent with reason `never_evaluated` |
| `override` | when one was applied |
| `facts_fresh_until` | whenever any DECISION-CONSTRAINING horizon exists: the seal horizon (`sealed_through + max_seal_lag_seconds`) when a budget clause is assigned `block`/`warn`, or a burn lease of a rule whose clause is assigned `block`/`warn` |

The fields themselves:

- `decision_id`;
- `evaluated_at` — the snapshot instant (D6a). Revision 1 called this `as_of` "the seal watermark";
  in the code `as_of` is the statement timestamp and `sealed_through` is the end of the data, and the
  two are different facts;
- `sealed_through` — the end of the sealed data the budget rests on — and `seal_lag`, the distance
  from it to `evaluated_at`, stated by the report path (D1) and compared against the policy's
  `max_seal_lag_seconds` (D8a);
- `window`, `target_id`, `objective`, `objective_updated_at`;
- `governing_revision` — the service definition revision in force at `evaluated_at`, nullable when
  none is; and `fact_revisions` — `{count, first_id, last_id, digest}` over the revisions the sealed facts
  in the window were computed under: the COMPLETE surviving evidence, since the revisions themselves are
  deleted with their service (§5a),
  which may be several (the report's own `spans_definition_revisions` withholding exists precisely
  because they can be);
- `coverage_lease_until`, and `burn_leases[]` — one per rule of the policy's target, because a target
  may hold several rules with different leases;
- `policy_revision`; `override` (id, `actor_label`, reason, expires_at) when one was applied, and
  `unoverridden_action` — what the pipeline would have been told without it. There is no
  `original_state`: an override never changes `state` (D9), so there is nothing original to keep;
- `coverage_state` per signal, as evidence;
- `reasons[]` — every matching or unavailable clause, total.

There is **no `valid_until`**. Revision 1 offered one derived from leases; the review is right that the
incident, the policy and the override can all change a microsecond later regardless of any lease, so
the field would have promised a validity the decision does not have. `facts_fresh_until` is present
instead, with ONE formula over the DECISION-CONSTRAINING inputs only: the minimum of
`sealed_through + max_seal_lag_seconds` when any budget clause is assigned `block` or `warn`, and of the
`burn_leases[]` entries whose rule's clause is assigned `block` or `warn`. Coverage is evidence and never
a clause, so `coverage_lease_until` is NOT in the formula; an `ignore`d burn clause's lease is not either.
Revision 3 defined the field over leases alone, so a budget-only policy had an expiry the field could
omit; revision 4 then mixed in coverage and ignored rules, so an `ALLOW` could sit beside a
`facts_fresh_until` already in the past for a fact that decided nothing (review round 4 P2-1, option b).
It is present whenever any constraining horizon exists — absent for a policy whose every clause is
`ignore` — and the response documents it as "when the facts this decision RESTS ON go stale",
explicitly not as a lifetime of the decision.

**D8 — the sealed contract has a price, and the price is stated. DECIDED.**
The SLO window ends at the seal watermark, never at `now`. A gate asked at 14:03 answers with the budget
as of `sealed_through`, and an outage that began after it is not yet visible. This is accepted — within
the bound of D8a. It is **forbidden** to "fix" the gate by reading raw heartbeats from the decision path:
that would make it the one surface in the product that quotes an unsealed number. **Invariant 6.**

**D8a — the price is BOUNDED. DECIDED (owner, 2026-08-28; review round 2 P0-1).**
Revision 2 called the seal lag an accepted cost and did not limit it. That was wrong in the way that
matters most: a 30-day window stays quotable when the materializer has been stopped for a week, because
the window simply ends at an old watermark, so a gate would keep saying `ALLOW` on a budget nobody has
measured since. Burn rules may `HOLD` on their short windows, but a policy that ignores burn clauses has
nothing left to notice. Therefore every policy carries `max_seal_lag_seconds` — an integer number of
seconds, a whole number of minutes, on the wire and in storage; the UI renders it as a duration (review
round 3 P2-4) — with **default 900 (15 minutes)**, the owner's choice (D-0190).

**The floor is derived, not chosen** (review round 3 P1-1; formula corrected round 4 P1-1). Revision 3
allowed `1m`, which a healthy pipeline can never satisfy: `domain.CanonicalBucket` is 60 s,
`LateArrivalGrace` is 120 s, and the materializer seals up to `FloorToBucket(now − grace)`, so in steady
state the lag sits in `[2m, 3m)` before queueing and commit — a policy of one or two minutes would be
`seal_stale` forever on a system doing exactly what it should. Revision 4 wrote "bucket + grace + one
bucket of headroom = 300 s", which is 240 s: the arithmetic did not produce the number beside it, so a
future change to the constants would have moved the floor wrongly. The formula is
`MinSealLag = LateArrivalGrace + CanonicalBucket + 2 × CanonicalBucket = 120 + 60 + 120 = 300 s` — the
healthy upper bound (`< 180 s`) plus two buckets of headroom for queueing and commit — stated in the
domain as a derived constant next to the constants it depends on, and the domain test asserts the
FORMULA against the constants, not the literal 300. The maximum stays 86 400 s. **Ownership moves with
it** (review round 5 P2-2): today `CanonicalBucket` lives in `internal/domain` and `LateArrivalGrace` in
`internal/store`, and `store` already imports `domain`, so a domain constant derived from both would
either close an import cycle or copy the grace. `LateArrivalGrace` therefore moves to the domain beside
`CanonicalBucket` — the store consumes it from there — and `MinSealLag` is declared next to both, so the
domain test really does see every constant it is asserting over. The seal work itself runs every second
(`serviceSliceEvery`), so the floor is about DATA finality, not about how often the sealer runs — the
rationale revision 3 gave ("the seal cadence is one minute") described the daily heartbeat rollup, a
different mechanism, and is corrected in D-0191.

When `seal_lag > max_seal_lag_seconds`, every budget clause is UNAVAILABLE with reason `seal_stale` and
D4 step 3 applies. The lag is stated by the report path and shown on the service page, so the gate and
the screen cannot disagree about how stale the facts are. A live-materializer boundary test — a policy at
the floor on a healthy stack must NOT be `seal_stale` — guards the derivation. **Invariant 15.**

**D9 — override lifecycle. DECIDED; review P1-3.**
An override is created through `POST …/gate/override`, never as a field of the decision request. Its
actor is the request's authenticated principal, server-derived and stored TWICE on purpose: the typed
attribution the audit log already uses (`actor_user_id` nullable + `via_token`, via
`authz.Principal.AuditUserID()`) AND an immutable `actor_label` — the principal's `AuditLabel`
(`token:<name>` for an API token, the user for a person) — because for a machine principal the typed
half is `NULL + true`, which after commit reads as "some token", and the evidence has to name which one
for as long as the row exists (review round 3 P1-4). The revoker is stored as the same COMPLETE triple
— `revoked_by_user_id` (nullable), `revoked_via_token`, `revoked_by_label` — because revision 4 wrote
"the same way" and then gave the schema only two of the three columns, so a token revoker would have lost
its typed half exactly as the actor once did (review round 4 P2-3). Any client-supplied actor field on either endpoint is refused as
unknown-field. A `reason` (bounded, 1..500 chars) and an `expires_at` are mandatory; `expires_at` must be after the database's `now()` and at most **7 days**
ahead (a hard maximum, not a default — there is no default). At most ONE unrevoked override exists per
service at a time: creation locks the service row and refuses a second with 409 `override_active`;
revocation is its own audited mutation. **An override changes ONLY `action`, never `state` or
`reasons`** (review round 2 P0-2): the observed state stays `BLOCK` or `UNKNOWN`, the reasons stay
listed, and `action` becomes `ALLOW` with `unoverridden_action` carrying what it would have been.
Revision 2 flipped `state` too and kept an `original_state`; that rewrote the observed fact for the
operator's convenience, and the ledger would have recorded a history that did not happen. `WARN` and
`ALLOW` are left as they are; `NOT_CONFIGURED` is never overridden. The metric therefore sees the truth:
`cerbix_gate_decisions_total{state="BLOCK",action="ALLOW",overridden="true"}`.

The override is **bound to the policy revision it was created under**: a policy edit — and a policy
DELETE — revokes any active override in the same transaction (`revoked_reason: policy_changed` /
`policy_deleted`), because an override that outlives a tightening of the policy would silently allow what
the new policy forbids. **An expired override releases its slot** (review round 2 P1-1): a partial
unique index cannot consult `now()`, so the creation transaction, under the service lock, first closes
any unrevoked row whose `expires_at` has passed (`revoked_reason: expired`) and then inserts; without
that, one expired override would refuse every later one forever.

**D10 — decisions are a ledger, not audit rows. DECIDED (owner, 2026-08-28; review P0-4).**
Revision 1 promised "every decision audited", "decisions persisted" and "O(1) reads with no write" at
once; they are incompatible. The resolution: every decision is one row in `service_gate_decisions`, an
append-only ledger with a bounded retention — `gate.decision_retention_days`, configuration-validated
within `7..365`, default 90. **The ledger is RANGE-partitioned by `evaluated_at`, one partition per UTC
day, in BOTH storage modes** (our own small-row table that needs nothing from a hypertable; the precedent
is `heartbeats` in 00017), and retention is the removal of whole partitions — never a row-level DELETE
(owner decision D-0194; revision 6's month-sized partitions would have kept rows up to 31 days past the
knob — review round 6 P1-2). **Two boundaries, both stated** (review round 8 P1-3 — revision 8 gave one formula that was a cadence
short and let "lifetime" mean parent visibility while the gauges counted the detached table). *Logical
readability* — the row answers on the ledger routes — ends when its partition is DETACHED: under a healthy
pass, `retention + <1 day + ≤ decision_purge_every` after `evaluated_at` (a day of partition granularity,
then at most one cadence to the pass that detaches). *Physical retention* — the bytes exist, and
`partitions_pending_drop` and `bytes` count them — ends at the deferred DROP one pass later:
`retention + <1 day + ≤ 2 × decision_purge_every`. Beyond either boundary is BACKLOG, visible in the
gauges and alerted on in the runbook, not a bound the product can promise while a lock refusal or a lost
session can delay a pass (review round 7 P1-2).

*The maintenance pass and its fence* (review round 7 P0). The pass runs on the scheduler leader in its
own goroutine, off the dispatch loop, but its AUTHORITY is not the scheduler's leadership: the
scheduler's advisory lock lives on one pinned connection while its work uses the pool, so a node whose
lock connection has died can still issue statements for up to a watchdog interval, and "cancelled and
joined before `lead` returns" does not close that window. Gate maintenance instead takes **its own
`store.LeaderSession`** on key `gateMaintenanceLockKey = 0x6365726269780003` — slot 3 of the
repository's `"cerbix" + slot` advisory-key namespace (slot 1 scheduler leadership, slot 2 migrations),
with a test that the three constants are distinct — the fenced shape file-provider apply already uses
(`TryBecomeLeaderSession`, `RunServiceRepairSlice`): the session-level advisory lock and EVERY
statement of the pass — DDL and registry writes — run on that one pinned connection, autocommit, which
`DETACH … CONCURRENTLY` requires anyway. If the connection dies PostgreSQL releases the lock AND the
connection can carry no further statement, so a deposed node cannot detach or drop anything after the
loss — not by convention, by construction; a successor acquires only once the lock is free. A node
that fails to acquire the session skips the pass. **One timeline** (review round 11 P1-2; round 12 P2-1): the whole
lifecycle of a pass — acquire, work, cleanup — fits `subCadenceTimeout` (30 s) measured from `passStart`:
work has a deadline at `passStart + 27 s` (creation `[0, 12 s)`, removal `[12 s, 27 s)`), and the release
proof below owns `[27 s, 30 s]`; revision 11 let a 3 s cleanup follow a 30 s pass and quietly made 33.
Acquisition is INSIDE the timeline — `pool.Acquire` blocks even though `pg_try_advisory_lock` does not —
so the slices are stated relative to it: when authority is acquired before t = 12 s, creation runs to the
boundary and removal begins by it; when authority arrives later, creation is skipped for this pass and
removal begins at once, provided ≥ 500 ms remain before t = 27 s, otherwise the pass does only its
cleanup. The cleanup deadline is the ABSOLUTE `min(now + 3 s, passStart + 30 s)`, never `now + 3 s`
after a late work return.

**Session settings never leave the session** (review round 9 P1-1). `DETACH … CONCURRENTLY` and
`FINALIZE` run autocommit, so their `lock_timeout` and `statement_timeout` are session-level `SET`s —
`SET LOCAL` is unavailable outside a transaction — while the transactional steps (create, attach, drop)
use `SET LOCAL` inside their transactions. Every session-level `SET` is paired with `RESET lock_timeout;
RESET statement_timeout` immediately after the statement it guarded, on success and on every error
path, and the gate session's release resets both again before the pinned connection goes back to the
pool. **Release is a proof, under its own clock** (review round 10 P1-5): cleanup runs under its own deadline, `min(now + 3 s, passStart + 30 s)` — the `[27 s, 30 s]` slice of
the pass timeline, absolute, never `context.Background()`, never the work context that may already be
cancelled — and must observe BOTH `RESET`s succeed and `pg_advisory_unlock` return a boolean (`true`
released, `false` "not held": both are known outcomes; an error or a timeout is UNKNOWN). On any unknown
the connection is hijacked from the pool and closed, never `Release()`d back, so neither a lock-owning
nor a timeout-carrying connection can reach an unrelated borrower (the session-poisoning class
AC-0127-3 already treats as an invariant for `SET LOCAL` bounds), and a blackholed `RESET` or unlock
cannot hold pass shutdown past 3 s. Tested in §7: pass → release → reacquire → `SHOW lock_timeout` and
`SHOW statement_timeout` are `0` after a clean pass, after a lock-timeout refusal and after a
statement-timeout abort; the poisoned backend's `pg_backend_pid()` is never borrowed again and a
successor acquires the gate lock (the oracle is the pid and the acquisition, not pool cardinality —
`TotalConns` is asserted only under `MinConns = 0`, review round 10 P2-2); a `RESET` blackhole and an
unlock error each end shutdown within the deadline with the backend terminated; and the mutations that
restore `context.Background()` or ignore the unlock's result fail.

*Creation under the weaker lock* (review round 7 P1-1). `CREATE TABLE … PARTITION OF` takes `ACCESS
EXCLUSIVE` on the parent (PostgreSQL 15 `CREATE TABLE` reference: "creating a partition using PARTITION
OF requires taking an ACCESS EXCLUSIVE lock on the parent partitioned table"), which would hold decision
inserts behind the very pass D10 says never blocks them. A day's partition is therefore built STANDALONE
— `CREATE TABLE service_gate_decisions_pYYYYMMDD (LIKE service_gate_decisions INCLUDING DEFAULTS
INCLUDING CONSTRAINTS INCLUDING INDEXES)` plus a `CHECK (evaluated_at >= day AND evaluated_at < day + 1)`
so the attach needs no scan — and then `ALTER TABLE service_gate_decisions ATTACH PARTITION … FOR VALUES
FROM (day) TO (day + 1)`, which takes `SHARE UPDATE EXCLUSIVE`. Creation has its own budget (review
round 8 P1-8 — revision 8 left creation without a budget, and at the maximum lead that was 31
two-statement sequences whose lock refusals alone could exceed the pass): `gate.decision_partition_create_max`
(default 3, `1..8`) days per pass, **nearest horizon first** — contiguous from the newest attached day
forward, because the horizon is what fails inserts — each a create transaction and an attach transaction
and the work is split into a creation slice `[0, 12 s)` and a removal slice `[12 s, 27 s)` that are
ENFORCED per statement, not checked between them (review round 9 P1-5; round 10 P1-4 — a check of elapsed
time between operations lets an attach started at t = 11.9 s run its 10 s statement timeout into the
removal slice). The clamp is a WALL bound, not a sum of timers (review round 11 P1-1: PostgreSQL's
`statement_timeout` measures the whole command from arrival to completion, lock wait INCLUDED, and
`lock_timeout` is a narrower competing timer inside it, so revision 11's additive admission would have
starved the four-statement create transaction after its first statement): before each statement,
`statement_timeout = min(10 s, slice deadline − now)`, `lock_timeout = min(2 s, statement_timeout)`, and
the transaction's context deadline is the slice boundary itself, so a statement blocked at the boundary
is cancelled and its transaction rolled back by 12 s; a statement is not started at all when fewer than
500 ms remain, because a bound below that cannot complete useful work. Creation therefore ends by t = 12 s
and the first removal statement begins by then, with `[12 s, 27 s)` reserved for at least one stage-op; a
blocked attach can delay a due detach by at most the creation slice. The convergence arithmetic in this section is a
healthy, uncontended claim and is labelled as one. Load refuses `create_max × floor(86 400 / decision_purge_every_seconds) < 2` (one day's partition plus
catch-up) naming both keys. Removal is `ALTER TABLE … DETACH PARTITION … CONCURRENTLY`
(also `SHARE UPDATE EXCLUSIVE`), `FINALIZE` on the next pass for one left detach-pending, and a `DROP
TABLE` of the standalone table that is **deferred to a later pass and gated on snapshots** (review round
7 P1-7): PostgreSQL 15.9's release notes fix "possible crashes and 'could not open relation' errors in
queries on a partitioned table occurring concurrently with a DETACH CONCURRENTLY and immediate drop of a
partition", and cerbix's enforced floor is 15.0 (`minServerVersionNum`), so the drop runs only when
`detached_at <= now() − decision_purge_every` on the database clock — INCLUSIVE, so at exactly one cadence
the drop runs and physical retention is `≤ 2 × purge_every`, not a third pass (review round 9 P1-6) — AND
no backend in this database has
`xact_start < detached_at` (`pg_stat_activity`) — a sequence safe on every supported 15.x; the runbook
recommends ≥ 15.9 without raising the floor. **Stage accounting** (review round 8 P1-4): `FINALIZE`,
`DROP` and `DETACH` are each one stage-op; a pass performs at most `decision_purge_max_partitions`
stage-ops, in the order finalize → drops whose gates are open → detaches, oldest first. Steady state
consumes TWO stage-ops a day — detach today's eligible day, drop yesterday's detached one — so load
refuses `decision_purge_max_partitions × floor(86 400 / decision_purge_every_seconds) < 4`, strictly
more than steady state, at least two stage-ops a day of catch-up, naming both keys (revision 8 accepted
exactly steady state and called it "plus catch-up"). A retention shortened by many days converges at that
surplus; the runbook says to raise the budget temporarily for a large one rather than wait. Every DDL
statement runs under `SET lock_timeout = '2s'` and `SET statement_timeout = '10s'`; a refusal is logged,
counted in `cerbix_gate_maintenance_errors_total{kind="lock_timeout"|"statement_timeout"}` and retried
next pass, never escalated to a longer wait.

*Ownership: the registry* (review round 7 P1-3; round 8 P1-1 and P1-2). `IF NOT EXISTS` is not
idempotence — PostgreSQL does not guarantee an existing relation resembles the one that would have been
created — and a standalone table left by a crash has no parent link, so a name alone proves nothing.
Every partition has a row in `service_gate_decision_partitions` (§5): `day` (PK), the deterministic
`relname`, an `owner_token` (a random uuid minted at creation), `relid` as a runtime LOCATOR, and a
`state`. **Every transition PostgreSQL allows inside a transaction is atomic with its registry write**,
so a crash at those statement boundaries cannot leave the two disagreeing: `CREATE TABLE` standalone +
`CREATE UNIQUE INDEX (id)` + `COMMENT ON TABLE … IS 'cerbix:gate-ledger:<owner_token>'` + the registry
INSERT (`created`, `relid = to_regclass(relname)`) commit together; `ATTACH PARTITION` + `state =
attached` commit together; `DROP TABLE` + `state = dropped` commit together. Only `DETACH … CONCURRENTLY`
cannot run inside a transaction, and it alone gets a write-ahead intent: `state = detaching` commits
first, the DETACH runs autocommit, `state = detached` commits after — and the pass reconciles every
outcome of a crash in between: `detaching` and still attached (the DETACH never ran) → run it;
`detaching` and `pg_inherits.inhdetachpending` (interrupted) → `FINALIZE`; `detaching` and standalone
(ran, the state write was lost) → `state = detached`. Revision 8 had only `created → attached → detached
→ dropped` and called every disagreement terminal, so an ordinary crash could refuse a day forever and
eat the horizon; the test matrix now crashes the pass at EVERY statement boundary. **Ownership is the
marker, not the OID** (revision 8 kept `relid` as identity; `pg_dump`/restore gives every relation a new
OID and OIDs are reused after DROP, so a restored installation would have refused every partition).
Before every act the pass checks, on the same connection, `obj_description(to_regclass(relname),
'pg_class') = 'cerbix:gate-ledger:' || owner_token` and the shape — `pg_get_expr(relpartbound)` equal to
the day's bounds when attached, the bounds CHECK present when standalone — and REFRESHES `relid` when the
marker matches under a new OID (the restore case). Refusal is reserved for what no crash produces: a
relation under our name without our marker or with another token, a shape that does not match, a
registry row in a state the table above does not reconcile (`attached` while the catalog shows no
partition — a manual act). A refusal skips THAT day only, counts
`cerbix_gate_maintenance_errors_total{kind="partition_identity"}` and pages; the pass goes on with the
other days, so one bad relation cannot exhaust the horizon by itself (revision 8 also counted this in the
EVALUATION error family, moving an evaluation metric without an evaluation — review round 8 P2; the
maintenance family is `cerbix_gate_maintenance_errors_total{kind}` with exactly `lock_timeout |
statement_timeout | partition_identity | error`, no other labels). Gauges are computed from the registry
JOINed to the catalog by the refreshed `relid`, never from names.

*No DEFAULT partition* (unlike heartbeats): the decision row is written in the decision transaction
(D6a), so a decision the ledger cannot hold is not a decision — the insert fails with SQLSTATE 23514
("no partition of relation found"), the API answers 503 `ledger_unwritable`, no state is emitted, the
CLI exits 1. The migration creates and registers `[today, today + 7 d]`; the lead keeps
`decision_partition_lead_days` (default 7, `2..30`) days ahead; the gauge
`cerbix_gate_decisions_writable_horizon_seconds` (time until the upper bound of the newest attached
partition, from the registry and catalog) has a runbook alert at 2 days — and a leader absent that long
has already pushed every policy past `max_seal_lag_seconds`, so the gate is saying `UNKNOWN` before it
stops recording. Four gauges, none by counting rows: `cerbix_gate_decisions_partitions_pending_drop`
(attached past the cutoff plus detached not yet dropped), `cerbix_gate_decisions_oldest_partition_age_seconds`
(age of the oldest attached partition's upper bound; 0 when none is past the cutoff),
`cerbix_gate_decisions_writable_horizon_seconds`, and `cerbix_gate_decisions_bytes` (sum of
`pg_total_relation_size(relid)` over registry rows not `dropped`) — no labels. And a read endpoint that is **project-scoped, not service-nested**:
`GET /api/v1/projects/{p}/gate/decisions/{id}`, authorised by the row's persisted `project_id` under
`gate:evaluate`, with NO service-existence check on the path (review round 2 P1-2 — a service-nested
route would answer 404 forever once the service is deleted, which is exactly the moment the evidence is
wanted). Policy and override mutations go to the
tenant audit log; decisions do NOT — a busy pipeline would otherwise bury the audit log under its own
heartbeat. A decision row is an immutable snapshot: it stores the service slug and name, the policy
clauses and revision, and the full evidence at the time, so it remains readable after the service is
renamed and survives its deletion (`service_id` nullable, `ON DELETE SET NULL`, the pattern
`service_alert_episodes` already uses); it is cascaded with its project. A lost response is retried by
the pipeline and produces a second, equally honest row — no idempotency key, because a decision is
a fresh reading and two of them are two facts. (The review noted that a signed canonical response CAN
prove content; revision 1 said otherwise. It is still declined, because it brings a key lifecycle and
cannot be looked up later.)

**D11 — the clause vocabulary. DECIDED (owner, 2026-08-28); names per review P2-1.**
Closed, versioned (`schema_version: 1`), exhaustive: a policy write must assign every clause in the
version's set, or it is refused (D14). The "shipped defaults" below are the **UI's create template and
nothing more**: the API request must carry every assignment, the threshold and `max_seal_lag_seconds` explicitly,
and the server fills nothing in — one strict contract, the repository's rule against silent server
defaults, and the only way a stored policy means exactly what somebody typed (review round 2 P2-3). Each
clause is `block`, `warn` or `ignore`:

| Clause | Fact and source (D1) | Shipped default |
|---|---|---|
| `budget_exhausted` | `BurnedPercent >= 100` for the policy's window | block |
| `budget_consumed` | `BurnedPercent >= budget_consumed_percent` (policy field, 1..100, default 90) | warn |
| `page_burn_firing` | a `page`-severity rule of the window's target is FIRING in the latch | block |
| `ticket_burn_firing` | a `ticket`-severity rule of the window's target is FIRING | warn |
| `service_incident_open` | an unresolved auto-incident anchored to this service | warn |

Three choices inside it. An open incident WARNS, not blocks: the deploy is very often the fix, and a
gate that blocks the fix because of the incident it fixes works against its purpose; a stricter
installation sets `block`. The burn clauses are named by SEVERITY, not `fast`/`slow`: the domain allows
arbitrary windows, `page` and `ticket` are urgencies, and the default 1h/5m–6h/30m pair is only a
template. **`coverage_not_armed` is not in the vocabulary** (owner; review P1-2 concurs): "not armed"
means the service is NOT the one paging for its members right now, its causes are incommensurable
(`not_owned`, `state_not_pageable`, `unroutable`, `stale_lease`…), and none of them is a release-risk
fact. `coverage_state` stays in the response as evidence. If "do not deploy while paging is broken" is
ever wanted, it arrives as precise clauses (`alert_route_unroutable`, `evaluator_stale`), never as
`armed = false`.

Clauses whose fact is UNAVAILABLE rather than false — `budget_withheld` (the report withholds the
number), `seal_stale` (D8a), `facts_stale` (a burn lease expired or no evaluation exists),
`no_objective` / `window_target_missing`, `never_sealed` — are not assignments; they are the conditions
under which a `block`/`warn` clause is unavailable and D4 step 3 applies, each with its own reason code
in `reasons[]`. An `ignore`d burn clause whose latch is stale contributes nothing, by D4 step 1.

**D12 — authorisation. DECIDED (owner, 2026-08-28; review P1-5).**
Three central `authz.Action`s, checked where every other action is: `gate:evaluate` (also reads the
ledger), `gate:policy:write`, `gate:override`. In v1 they map to the existing project roles — `viewer`+,
`editor`+, `project_admin`+ — and an API token asks the gate with the role it already has. A
token-scoped capability ("this token may evaluate the gate and read nothing else") is **deliberately not
built here**: `domain.Role` is shared by memberships and tokens, so a narrower token model is a change to
RBAC rather than an addition to it, and it is recorded as a follow-up with its own requirement. No
ad-hoc role comparison anywhere in the gate handlers.

**D13 — policy scope and ownership. DECIDED (owner, 2026-08-28; review P2-2).**
Per service, no inheritance. And the policy is an **operational control, owned by the UI/API even for a
file-managed service**: it is not part of bundle format 2, it is excluded from the canonical hash, and it
is the one paging-adjacent field a file-managed service accepts through the API — an explicit, documented
exception to the 409 rule of §16.6a, because the team that deploys is not always the team that owns the
service's declaration. *The review recommended the opposite* — policy as a declarative part of format 2,
with file-managed services refusing API writes — and that position is recorded here as the alternative
the owner declined, so it can be revisited with evidence rather than rediscovered.

**D13a — the policy and override HTTP contract (review round 9 P1-2; round 10 P1-1, P1-2, P1-6).**
Revision 9 named the decision route and the ledger routes and left policy as "a write" and override as a
shorthand; a stale UI could have revoked "the current override" and hit a newer one. The routes, all
under the service with `serviceIDParam` semantics (400 malformed, 404 foreign or unknown); every request
body is decoded STRICTLY — an unknown field, a missing field or a wrong type is 400 naming the field, the
server fills nothing in (D11, D14):

```
GET    /api/v1/projects/{p}/services/{s}/gate/policy
       → 200 {schema_version, window, clauses, budget_consumed_percent, max_seal_lag_seconds, unknown_behavior,
              revision, updated_at, updated_by}  |  404 not_configured
PUT    /api/v1/projects/{p}/services/{s}/gate/policy
       body {expected_revision (null = "nothing configured"), schema_version, window,
             clauses (EVERY clause of the version), budget_consumed_percent, max_seal_lag_seconds, unknown_behavior}
       → 200 {revision}  |  409 revision_conflict  |  400 validation (names the field and the range)
DELETE /api/v1/projects/{p}/services/{s}/gate/policy?expected_revision=N
       → 204  |  409 revision_conflict  |  404 not_configured
       |  400 expected_revision_required (absent)  |  400 expected_revision_invalid (non-integer or negative)
GET    /api/v1/projects/{p}/services/{s}/gate/override                      -- the ACTIVE override only, not history
       → 200 {id, reason, expires_at, created_at, actor_label, policy_revision}  |  404 none_active
GET    /api/v1/projects/{p}/services/{s}/gate/overrides                     -- history: the newest 50, ORDER BY created_at DESC, id DESC
       → 200 {items: [override records as below]}
GET    /api/v1/projects/{p}/services/{s}/gate/overrides/{override_id}       -- one record, whatever its status
       → 200 {id, reason, expires_at, created_at, policy_revision,
              actor_label, actor_user_id, via_token,
              status: active | expired | revoked | inert,           -- the function below, computed at read time
              revoked_at, revoked_reason,                            -- lifecycle: null while active; set by ANY closure
              revoked_by_label, revoked_by_user_id, revoked_via_token} -- attribution: present only for `manual`
       |  404 unknown or foreign  |  400 malformed id (the `serviceIDParam` convention)
       -- every field is ALWAYS PRESENT, never absent: an active row carries the five closure fields as null;
       -- a system closure (expired, policy_changed, policy_deleted) carries revoked_at + revoked_reason and the
       -- three attribution fields null; a manual closure carries all five, with revoked_by_user_id null and
       -- revoked_via_token true when the revoker was an API token (the typed-token contract of D9)
POST   /api/v1/projects/{p}/services/{s}/gate/override
       body {policy_revision, reason, expires_at}                            -- no action: an override's only effect is D9's
       → 201 {id}  |  409 override_active  |  409 revision_conflict  |  400 validation (> 7 days, past expiry, empty reason)
DELETE /api/v1/projects/{p}/services/{s}/gate/overrides/{override_id}
       → 204  |  404 unknown or foreign  |  409 override_not_active
```

**`revision` is a generation, never reused** (review round 10 P1-1). Revision 10 let a first create be
revision 1 and a DELETE remove the row, so a delete-and-recreate produced revision 1 again and a stale
screen holding the old 1 could `PUT` over — or `POST` an override against — a different policy. The
policy row is never deleted: `DELETE` sets `deleted_at` and bumps `revision` in the same statement, and
a re-create is an `UPDATE` that clears `deleted_at` with `revision + 1`. `expected_revision` is REQUIRED
on `PUT` and `DELETE`; `null` means "I believe nothing is configured" and matches only a row that is
absent or tombstoned; any mismatch is 409 and changes nothing — and the CAS runs BEFORE the no-op
comparison of D14, so an identical body with a stale `expected_revision` is 409, never a silent 200
(review round 10's added mutation). Test: A reads revision N, the policy is deleted (N+1), B recreates
(N+2); A's `PUT` with N and A's `POST …/override` with `policy_revision: N` are both 409 and the new
policy and B's state are untouched.

**The bodies carry everything** (review round 10 P1-2): the policy body is the full D11/D14 document —
`schema_version`, `window`, every clause of the version, `budget_consumed_percent`,
`max_seal_lag_seconds`, `unknown_behavior` — and revision 10's fence, which omitted the version and the
threshold, was wrong against D11's "the server fills nothing in". The override body has NO `action`
field: D9 fixes what an override does — a `BLOCK` decision's `action` becomes `ALLOW`, `WARN` and `ALLOW`
are left alone — so a client-supplied action could only contradict it; the read shapes carry no `action`
either.

Deleting the policy makes later decisions `NOT_CONFIGURED` (exit 4), leaves the ledger untouched, and
closes the active override as `policy_deleted` — it stays readable through the history routes and reads
as `inert`. An override is bound to the policy revision the caller saw (`policy_revision` in the
body; 409 `revision_conflict` if it is not current), one active per service (409 `override_active`), at
most 7 days. **Revocation is by the override's immutable id, never "the current one"**: a UI that held
override A — expired meanwhile, B created since — sends `DELETE …/overrides/{A}` and receives 409
`override_not_active`; B is untouched, and a revoke of an already-revoked or expired override is that
same 409, never a silent 204, so a stale screen learns it is stale.

**History is readable, and `status` is a function** (review round 10 P1-6; round 11 P1-3). Revision 10
promised that inert overrides "remain readable history" and offered only an active-only `GET`; revision
11 added the reads but let `status` overlap with the write lifecycle (a row D9 closes as
`policy_changed` was also called inert; a row B's creation closes as `expired` was expected with no
`revoked_at`). Every closure — human or system — sets `revoked_at` and `revoked_reason`; only a `manual`
closure carries attribution, and that attribution follows D9's typed-token contract — `revoked_by_label`
always, `revoked_by_user_id` null with `revoked_via_token = true` when the revoker was an API token
(review round 12 P2-2); the three attribution fields are null for `expired`, `policy_changed` and
`policy_deleted`. `status` is computed at read time over SEMANTIC FACTS rather than over which
housekeeping has run (review round 12 P1-1: revision 12 ranked a recorded `expired` reason above a
revision mismatch, so a row that read `inert` before B's creation read `expired` after it — the same
persisted override changing status because an unrelated override was created). The precedence, exhaustive
and table-tested with a mutation of the order:

| # | condition | status |
|---|---|---|
| 1 | `revoked_reason = manual` | `revoked` |
| 2 | `revoked_reason IN (policy_changed, policy_deleted)` OR `policy_revision ≠` the service's current live revision OR the policy is tombstoned | `inert` |
| 3 | `revoked_reason = expired` OR `expires_at <= now()` | `expired` |
| 4 | otherwise | `active` |

Rows 2 and 3 each join a recorded reason with the live fact it records, so the in-lock closure that later
writes `expired` or `policy_changed` onto a row cannot move it: a mismatched, expired, unrevoked row is
`inert` before the next `POST` closes it as `expired` and `inert` after. A status DOES move when a
semantic fact moves — a manual revoke, a policy edit, the clock passing `expires_at` — and on nothing
else.
The by-id route returns any override with the full actor triple, the revoker triple as above and
`status`; the list returns the newest 50 by `created_at DESC, id DESC` over a matching index
`(service_id, created_at DESC, id DESC)` (§5), so a hot service never sorts its history and equal
timestamps order deterministically. **Storage is unbounded and the product history is a window**
(review round 11 P1-4): nothing in the regime bounds rows — a project admin can create and revoke in a
loop — so the list is a bounded READ over an indexed order, the audit log is the record, and the
expiry window is not a row bound. Invariant 17's "later read of the override" is this by-id route. Authorization: `gate:policy` for the policy
`PUT`/`DELETE`, `gate:override` for the override `POST`/`DELETE`, the read action of D12 for every
`GET`; every mutation is audited with the actor triple (D-0188). File-managed services expose the same
routes — D13 makes gate policy UI/API-owned even there. The SPA's policy editor and override panel read
exactly these shapes and nothing else.

**D14 — policy evolution and concurrency. DECIDED; review P1-8.**
The policy document carries `schema_version`; the server accepts only versions it knows. A write must
assign EVERY clause of that version exactly once — unknown, missing or duplicate clauses are refused with
the offending name, so adding a clause in a later version cannot silently reinterpret stored policies:
they keep their version and are evaluated under it until rewritten. Thresholds must be finite integers
within bounds and are stored canonically; `max_seal_lag_seconds` is an integer, a whole number of minutes,
in `300..86400` (D8a). Nothing is filled
in server-side — an omitted field is a refused request, not a default. A write identical to the stored
policy is a no-op: no revision
bump, no audit row, no override revocation (the `alertPolicyDiff` pattern). Concurrent writes are
serialized by `expected_revision` in the body, 409 `revision_conflict` on mismatch — the declaration
PUT's pattern.

**D15 — tenant error contract. DECIDED; review P1-9.**
Revision 1 said "a foreign, unknown or malformed id answers 404, as the alerting endpoints do", and the
alerting endpoints do not: `serviceIDParam` refuses a malformed UUID with **400** before the store is
asked, and a well-formed foreign or unknown id is **404**. The gate keeps that contract exactly, on every
gate route alike. **Invariant 10.**

**D16 — the CLI is a security and operations surface, not a verb. DECIDED; review P1-6.**
`cerbix gate check --project <p> --service <s> [--json] [--timeout 10s]`. The server is `CERBIX_URL`, the
credential is `CERBIX_TOKEN` — **environment only, never a flag**, because flags land in shell history
and process lists. TLS verifies by default; `CERBIX_CA_FILE` adds a CA; there is no skip-verify option in
v1. The decision goes to stdout — one line of text, or the response JSON with `--json` — and reasons and
diagnostics go to stderr. The JSON is the API response verbatim and carries `schema_version`. Exit
codes follow `action`: **0** `ALLOW` and `WARN`; **2** `BLOCK`; **4** `NOT_CONFIGURED`; **1** transport,
auth, timeout, snapshot conflict or a 429 from the bounds of §5a — the CLI does NOT retry on 429 itself
(a pipeline that retries into a rate limit is the load the limit exists to shed); it prints the
`Retry-After` value (whole seconds, `ceil`ed, never below 1 — §5a) on stderr and exits 1. `UNKNOWN` therefore exits 0 or 2 by the operator's declared
`unknown_behavior`, and the word `UNKNOWN` is in the output regardless — revision 1's exit 3 would have
had a plain shell block against an operator who chose `warn`.

**D17 — Change Intelligence is FR-025 and is not started here. DECIDED.**
Deployment/rollback/flag events, the service timeline, incident correlation and before/after SLI
comparison extend `IncidentContext` (§14) with append-only phases keyed
`UNIQUE(service_id, source, external_id, phase)`, a domain-owned phase order, serialization per external
identity, and closed enums with no free payload. None of it is needed for a decision.

## 4. What must move WITH the implementation, not after it

- `docs/overview.md` — the gate endpoints and CLI verb; the one-snapshot rule and the owners under the
  reliability section.
- `docs/runbook.md` — "the gate says UNKNOWN" (it is about the facts, not the pipeline); the override
  procedure and the metric; the decision-ledger retention knob and the capacity table of §5a; the CLI's
  environment contract; and the alert thresholds for the new metrics — `writable_horizon_seconds < 2 d`
  (the maintenance goroutine or the leader is gone; the gate stops recording when it reaches 0),
  `partitions_pending_drop ≥ 2 for 6h` (removal refused by `lock_timeout` pass after pass),
  `evaluate_errors_total{kind="ledger_unwritable"} > 0` (page: the horizon was exhausted),
  `maintenance_errors_total{kind="partition_identity"} > 0` (page: a relation under our name lacks our
  marker or its shape is wrong — that day is left alone until a human looks; the other days proceed),
  `rate(maintenance_errors_total{kind="lock_timeout"}[1h]) > 0` (something holds `ACCESS EXCLUSIVE` on
  the ledger during maintenance; find it in `pg_locks`),
  `rate(evaluate_rejected_total[5m]) > 0 sustained for 15m` (a pipeline is looping into the limit),
  `evaluate_errors_total{kind="snapshot_conflict"}` rising (contention the retry is not absorbing); disk
  sizing from `cerbix_gate_decisions_bytes` after a week, 3× the §5a payload bound before; and the
  recommendation to run PostgreSQL ≥ 15.9, whose release notes fix crashes and "could not open relation"
  errors around `DETACH CONCURRENTLY` followed by an immediate drop — the deferred, snapshot-gated drop of
  D10 is what keeps every supported 15.x safe, the recommendation is belt to those braces.
- `internal/domain` — `LateArrivalGrace` moves here beside `CanonicalBucket` and `MinSealLag` is declared
  next to both; `internal/store` consumes them (D8a).
- `func-service-reliability.md` §16.8 — invariant 106 (the gate consumes the owners and derives
  nothing) and 107 (one snapshot); rows in `docs/traceability.md` in the same change, since
  `make docs-check` compares the two exactly.
- `func-service-reliability.md` §16.6a — the documented exception: gate policy is writable on a
  file-managed service (D13).
- `openapi.yaml` — the decision route, the eight policy/override routes of D13a, the by-id and listing ledger routes and their schemas; regenerate `frontend/src/api/schema.d.ts`.
- `docs/specs/README.md` — this file is listed (review P2-3, done in revision 2).
- README — one sentence: cerbix decides, the pipeline acts.

## 5. Schema (decided shape; column detail belongs to the implementation)

```
service_gate_policies   (service_id PK, project_id, window text, schema_version int,
                         clauses jsonb -- exhaustive {clause: block|warn|ignore} for the version,
                         budget_consumed_percent int CHECK 1..100,
                         max_seal_lag_seconds int CHECK 300..86400 AND % 60 = 0,
                         unknown_behavior CHECK IN (warn, block),
                         revision bigint DB-owned, MONOTONIC PER SERVICE, never reused: the row is never deleted —
                         DELETE sets deleted_at and bumps revision in the same statement; a re-create is an UPDATE
                         that clears deleted_at with revision + 1 (D13a);
                         deleted_at NULL, updated_at, updated_by)
service_gate_overrides  (id, service_id, project_id, policy_revision,
                         actor_user_id NULL, via_token bool, actor_label text NOT NULL,   -- typed + immutable label
                         reason text CHECK length 1..500, created_at, expires_at NOT NULL,
                         revoked_at NULL, revoked_reason NULL CHECK IN (manual, expired, policy_changed, policy_deleted),
                         revoked_by_user_id NULL, revoked_via_token bool NULL, revoked_by_label text NULL;
                         at most one row per service with revoked_at IS NULL — enforced under the service lock,
                         with expired rows closed as `expired` in the same lock before an insert;
                         INDEX (service_id, created_at DESC, id DESC)   -- the history list's order, no sort)
service_gate_decisions  (id uuid NOT NULL — UUIDv7 built from evaluated_at's DB-clock milliseconds + 74 random bits;
                         CHECK (gate_uuid_ms(id) = floor(extract(epoch from evaluated_at) * 1000)) binds id to row;
                         each day partition carries a LOCAL UNIQUE INDEX (id), created standalone before attach;
                         project_id NOT NULL → projects CASCADE,
                         service_id NULL → services SET NULL, service_slug, service_name,
                         state, action NULL, reasons jsonb, evidence jsonb,           -- action NULL only for NOT_CONFIGURED
                         policy_revision NULL, window NULL, policy_snapshot jsonb NULL, -- NULL for NOT_CONFIGURED
                         override_id NULL, evaluated_at NOT NULL, sealed_through NULL; -- per the D7 presence table
                         PARTITION BY RANGE (evaluated_at), one partition per UTC day, NO DEFAULT partition;
                         PRIMARY KEY (evaluated_at, id);                                -- the partition key must be in it
                         INDEX (project_id, evaluated_at DESC, id DESC);                -- the listing's keyset path
                         INDEX (project_id, service_id, evaluated_at DESC, id DESC);    -- the per-service listing path
                         -- no parent (id) index: day pruning + the per-partition local UNIQUE (id) make it redundant;
                         -- per child exactly FOUR indexes: PK (evaluated_at, id), local UNIQUE (id), the two above
                         CHECK (octet_length(evidence::text) <= 4096
                                AND octet_length(reasons::text) <= 1024
                                AND (policy_snapshot IS NULL OR octet_length(policy_snapshot::text) <= 4096));
                         retained `gate.decision_retention_days`, default 90, by partition removal (D10))

service_gate_decision_partitions  (day date PRIMARY KEY, relname text NOT NULL UNIQUE,
                         owner_token uuid NOT NULL UNIQUE,      -- the marker: COMMENT ON TABLE 'cerbix:gate-ledger:<token>'
                         relid oid NOT NULL,                    -- runtime locator, refreshed when the marker matches a new OID
                         state text NOT NULL CHECK (state IN ('created','attached','detaching','detached','dropped')),
                         created_at NOT NULL, attached_at NULL, detached_at NULL, dropped_at NULL;
                         the ownership registry of D10 — every act validates marker and shape against the catalog)
gate_uuid_ms(uuid) RETURNS bigint IMMUTABLE  -- the first 48 bits of a UUIDv7 as milliseconds (migration-defined SQL function)
```

**Identity on a partitioned ledger** (review round 6 P1-3; round 7 P1-5; round 8 P1-5, P1-6, P1-7).
PostgreSQL enforces a primary key on a RANGE-partitioned table only when the partition key is part of
it, so the key is `(evaluated_at, id)`. The id is a UUIDv7 whose 48-bit timestamp is the millisecond of
`evaluated_at` — the DB clock the row is partitioned by, read in the decision transaction before the id
is generated — with 74 random bits after version and variant. **The binding is enforced by the
database, not trusted to the writer**: `CHECK (gate_uuid_ms(id) = floor(extract(epoch from evaluated_at)
* 1000))`, `gate_uuid_ms` an IMMUTABLE SQL function over the first 48 bits defined by the migration, so an
id's day and its row's partition cannot disagree and a duplicate id across days is structurally
impossible; within a day, each partition carries a LOCAL `UNIQUE (id)` index created on the standalone
table before attach (a partitioned unique index would have to include the partition key; a per-child one
need not), so two rows with one id cannot exist anywhere and a colliding insert fails at the database.
Revision 8's 500 on "two rows share an id" is gone because the case is gone (review round 8 P1-5). The
project-scoped read derives the day from the id and queries `WHERE evaluated_at >= day AND evaluated_at
< day + 1 AND id = $1 AND project_id = $2`: PostgreSQL prunes to one child, or to none when that day's
partition is detached or was never created, and either way the answer is 404. There is NO
application-side date prefilter and no application clock anywhere in the read (review round 8 P1-6:
revision 8's application-side date prefilter needed a `today` that no API node owns and contradicted
backlog); logical readability ends exactly at detach, as D10 states. Ledger reads take the in-flight
permits of §5a (process, then principal), not the rate tokens.

**The listing contract** (review round 8 P1-7 — revision 8 mentioned "listing" in a query note and the
SPA's decision history had nothing to implement):

```
GET /api/v1/projects/{p}/gate/decisions?from=<RFC3339>&to=<RFC3339>[&service_id=<uuid>][&cursor=<opaque>][&limit=<n>]
```

`from` and `to` are REQUIRED, the range is half-open `[from, to)`, `from < to` and `to − from ≤ 31 days`
(400 `range_required` / `range_invalid` / `range_too_wide`); `limit` defaults to 50, must be a positive integer (400 `limit_invalid` for `0`, negative or
non-integer) and is capped at 200 (400 above it); order is `evaluated_at DESC, id DESC`; the cursor is an opaque base64 keyset
`(evaluated_at µs, id)` of the LAST RETURNED item and the next page is bound STRICTLY below it by row
comparison, `(evaluated_at, id) < ($ts, $id)`; a cursor that does not decode is 400 `cursor_invalid`.
The page fetches `LIMIT + 1` rows only to learn whether a next page exists: it returns the first `limit`
and encodes the cursor from the last of THOSE — never from the extra row, which is not returned and
would be skipped by the strict bound (review round 10 P1-3; the mutation encoding the extra row fails the
`limit = 2` over three rows test). Each page is ONE SQL statement (review round 9 P1-3; round 10 P2-1):
the planner prunes to at most 32 children and produces an Append or Merge Append with one scan per
surviving child — every one of those scans on the matching child index, `(project_id, evaluated_at
DESC, id DESC)` without `service_id` or `(project_id, service_id, evaluated_at DESC, id DESC)` with it,
and no post-filter over the other path's columns — so a sparse service on a busy project never filters
the project's rows to find its 50; the bound on one response is the page size, not a permit. The response
is `{items: [...], next_cursor}`, `next_cursor` `null` on the last page. **An item's presence contract is
the by-id response's** (review round 10 P1-7): the D7 always-present fields (`schema_version`,
`decision_id`, `evaluated_at`, `state`, `reasons[]`); `action` and `policy_revision` present exactly when
`state ≠ NOT_CONFIGURED`, absent otherwise, never `null`; `override_id` present exactly when an override
was applied; `service_slug` and `service_name` always present (they are snapshots); `service_id` always
present and `null` once the service has been deleted (`ON DELETE SET NULL`) — present-and-null means "the
referent is gone", absent means "never applied", and the two are not interchangeable. Tenant
behaviour: the route is project-scoped like the by-id read; a `service_id` foreign to the project yields
an EMPTY page, not 404, because the ledger outlives services. The action is the one the by-id read uses.
**The traversal is LIVE keyset, not a snapshot** (review round 12 P1-2 — revision 9 had "proved" that
concurrent inserts cannot be skipped because a new row's `evaluated_at` is later than page 1's first
item; `evaluated_at` is taken near the start of the decision transaction, not at commit, so a decision
begun before page 1 and committed after it lands INSIDE the traversal with no such guarantee, and a
partition detached between pages removes rows). The contract is exactly this: a key returned once is
never returned again; every row committed before the first page and still attached at the time its
page is read is returned exactly once; rows that commit during the traversal may appear on a later page
or not at all, and rows whose partition is detached during the traversal disappear — both explicitly
outside the guarantee. For an operator's history screen those live semantics are the honest ones; a
pipeline that needs a fixed set reads by decision id. The SPA's decision history is
this endpoint and nothing else.

Tenant-composite FKs to `(services.id, project_id)` where the service reference is NOT NULL, as every
service-scoped table since 00085.

## 5a. Resource bounds — the numbers, the algorithm, the capacity they imply, and what they mean on a cluster

Revision 4 named the bounds and left every number, key and algorithm to the implementer; two authors
would have built two different limiters and the config loader would not have known what to validate
(review round 4 P1-2). Revision 5 gave numbers that defeated their own purpose: a default of 600
decisions per minute permitted 77.8 million immutable rows per replica over the retention, and the hard
maximum permitted 7.8 billion (review round 5 P1-1). The numbers below are sized for what a deploy gate
IS — a few decisions per service per day, tens per minute across a busy installation — and the
capacity they imply is written next to them so nobody has to derive it (owner decision, D-0193).

| Key (`gate.*`) | Default | Min | Max | Semantics |
|---|---|---|---|---|
| `evaluate_inflight_process` | 8 | 1 | 64 | decisions in flight per PROCESS; the (n+1)th is 429 `process_inflight` before any transaction |
| `evaluate_inflight_principal` | 2 | 1 | 16 | decisions in flight per principal (token id or user id); 429 `principal_inflight` |
| `evaluate_rate_principal_per_minute` | 10 | 1 | 600 | token bucket per principal: capacity = the value, refill = value/60 tokens per second; drained → 429 `principal_rate` |
| `evaluate_rate_process_per_minute` | 60 | 1 | 600 | token bucket for the process, same algorithm; drained → 429 `process_rate` |
| `evaluate_tx_budget_ms` | 5 000 | 500 | 30 000 | begin-through-commit budget for the decision transaction, applied through the store's deadline wrapper with the remainder per statement |
| `decision_retention_days` | 90 | 7 | 365 | ledger retention (D10): daily partitions whose upper bound is at or before `now − retention` are detached, then dropped a pass later; readable until detach (`retention + <1 day + ≤ purge_every`), on disk until drop (`… + ≤ 2 × purge_every`) under a healthy pass |
| `decision_partition_lead_days` | 7 | 2 | 30 | days of partitions kept created ahead; the writable horizon the gauge measures against |
| `decision_partition_create_max` | 3 | 1 | 8 | days created (create + attach) per pass, nearest horizon first; load refuses `create_max × floor(86 400 / every_seconds) < 2` naming both keys |
| `decision_purge_every` | 1h | 5m | 24h | maintenance cadence, on the gate's own fenced session; a pass's whole lifecycle (work ≤ 27 s + cleanup ≤ 3 s) fits `subCadenceTimeout` |
| `decision_purge_max_partitions` | 8 | 1 | 48 | removal STAGE-OPS (finalize, drop, detach) per pass; steady state is two a day; load refuses `max × floor(86 400 / every_seconds) < 4` naming both keys |

**Acquisition order, so a rejection costs nothing it should not** (review round 5 P2-3): the in-flight
permit is taken FIRST — process, then principal — and a refusal there consumes no rate token; the two
rate buckets are then checked and debited under ONE lock, as a unit — if either is empty, neither is
debited (review round 7 P1-6: revision 7 debited the principal bucket before finding the process bucket
empty, and called that "never burns quota") — and a rate refusal RELEASES the permit already held.
`Retry-After` is `ceil(seconds until the next token)` for a rate refusal and `1` for an in-flight
refusal, and is never below 1, so two identical bursts see identical headers and a refused request never
burns quota. A 429 runs no report and writes no ledger row (invariant 14). All ten keys are validated
at configuration load; a value outside its range refuses to start, naming the key and the range.

**Bytes, not only rows** (review round 6 P2). A row is bounded by the CHECKs of §5: evidence ≤ 4 KiB,
reasons ≤ 1 KiB, policy snapshot ≤ 4 KiB, so a row is at most ≈ 10 KiB with its fixed columns and
typically ≈ 1.5 KiB. The one input that had no bound was the list of definition revisions overlapping the
window — a definition revised often enough would have grown every row; the evidence now carries
`fact_revisions: {count, first_id, last_id, digest}` with `digest` the SHA-256 over the sorted ids. That
object is the COMPLETE surviving evidence (review round 7 P1-4): `service_definition_revisions` cascades
with its service (00064, `ON DELETE CASCADE`) and `DeleteService` takes declarations, epochs and facts
with it, while D10 promises the decision outlives the service — so after a deletion nothing more than
count, ends and digest can exist, and the spec does not pretend otherwise; while the service exists the
digest is verifiable against the revisions the window names. Evidence JSON is canonical — sorted keys, no
whitespace — so equal facts give equal bytes and the bound is testable. The writer never truncates: a
row that would exceed a CHECK is a bug, fails the decision as error kind `error`, and a test proves it
with a synthetic 5 KiB evidence.

**Capacity at the permitted ingest**, per replica — what the table above actually allows, in rows and in
PAYLOAD bytes (typical 1.5 KiB / CHECK bound 10 KiB per row). Payload is not disk: tuple headers, TOAST,
the four indexes per partition (§5: the PK, the local unique id, the two listing paths — about 240 B
of index entries per 1.5 KiB row) and fill factor sit outside the three JSON CHECKs, so physical size is roughly 2–3×
payload (review round 7 P2); the runbook sizes disk from `cerbix_gate_decisions_bytes` after the first
week of real traffic, and from 3× the bound column before it.

| | rows / day | rows / 90 d | payload / 90 d typical → bound | rows / 365 d | payload / 365 d typical → bound |
|---|---|---|---|---|---|
| a real installation (≈ 200 decisions/day) | 200 | 18 000 | 27 MB → 180 MB | 73 000 | 110 MB → 730 MB |
| defaults saturated (`60/min` process) | 86 400 | 7 776 000 | 11.7 GB → 78 GB | 31 536 000 | 47 GB → 315 GB |
| hard maximum saturated (`600/min` process) | 864 000 | 77 760 000 | 117 GB → 778 GB | 315 360 000 | 473 GB → 3.2 TB |

Multiply by the replica count for a cluster (below). One daily partition at saturated defaults holds
86 400 rows ≈ 130 MB typical, and is removed in two catalog operations when it ages out. An installation
that raises `evaluate_rate_process_per_minute` is raising THIS table and should read it first; the
runbook says so.

**Observability of the bounds themselves** (review round 5 P1-3), all low-cardinality and none carrying a
principal: `cerbix_gate_evaluate_rejected_total{reason}` with exactly the four reasons above;
`cerbix_gate_decision_duration_seconds` as a histogram over fixed buckets (0.05 … 30 s); and
`cerbix_gate_evaluate_errors_total{kind}` with `snapshot_conflict | timeout | ledger_unwritable | error`
— evaluation errors only. Maintenance has its own bounded family, `cerbix_gate_maintenance_errors_total
{kind}` with `lock_timeout | statement_timeout | partition_identity | error` (review round 8 P2: an
evaluation metric must not move without an evaluation). Together with the four ledger gauges of D10 —
`partitions_pending_drop`, `oldest_partition_age_seconds`, `writable_horizon_seconds`, `bytes` — that is
the whole metric surface of this requirement; the runbook thresholds (§4) are written against these
names.

**These bounds are process-local, on purpose, and that is the v1 contract.** Every `api` or `all` replica
enforces its own copies, so the cluster-wide allowance scales with the replica count — and so does the
capacity table above. A cluster-wide limiter would need shared state — in the database, which is what the
bound protects — and is declined here; an installation that needs a hard cluster ceiling sets the
per-process numbers with its replica count in mind. The threat model (§8) is written against these
numbers, not against a limiter that does not exist.

## 6. Acceptance invariants (FR-024) — approved design contract, discharged on implementation

1. the gate computes NO reliability fact: budget comes from the report path for the policy's window,
   withholding from `decideServiceWindow` inside it, burn from that window's latches, coverage from
   `serviceCoverageClauses`; a response never quotes a number the service page would withhold;
2. `NOT_CONFIGURED` is distinct from every policy outcome, has no `action`, carries its own reason and
   exit code, and is never rendered as `ALLOW` or `WARN`;
3. `UNKNOWN` is produced only under a declared policy, only by an unavailable `block`/`warn` clause
   (never by an `ignore`d one), and resolves to the policy's explicit `unknown_behavior`; a policy
   without one cannot be created; a KNOWN block is never softened by an unknown neighbour;
4. `reasons[]` is total — every matching or unavailable clause — and `state`/`action` follow the
   algebra of D4 exactly;
5. the response follows the D7 presence table exactly: `schema_version`, `decision_id`, `evaluated_at`,
   `state` and `reasons[]` always; every other field present when its condition holds and absent with
   the named reason otherwise; `facts_fresh_until` follows the ONE formula of D7 (invariant 16) and there
   is no `valid_until`;
6. the decision path reads only SEALED facts and has no heartbeat access by construction;
7. all reads and the ledger INSERT happen in ONE `REPEATABLE READ` transaction whose first
   SNAPSHOT-BEARING statement — after the deadline wrapper's `SET LOCAL`s, which establish no snapshot —
   supplies `evaluated_at`; a serialization failure is retried once and then reported as a transport
   error, never as a decision;
8. an override is created only through its own endpoint by a principal holding `gate:override`, with
   server-derived actor, bounded reason and an expiry ≤ 7 days; at most one is active per service; an
   expired one releases its slot; it is never honoured from a decision request body; a policy edit or
   deletion revokes it;
9. an override changes ONLY `action`; `state` and `reasons[]` are the observed facts and are never
   altered by it; an applied override is named in the response with `unoverridden_action` and counted as
   `overridden="true"`; an expired or revoked one is not applied;
10. a malformed service id answers 400 before the store; a well-formed foreign or unknown id answers
    404; identically on every gate route;
11. policy writes are exhaustive over the version's clause set, refuse unknown/missing/duplicate
    clauses by name, bump a DB-owned revision only on real change, are CAS'd on `expected_revision`,
    and are audited with before/after and actor;
12. a decision row is immutable, survives service rename and deletion, lives in a daily RANGE
    partition on `evaluated_at` in both storage modes under the key `(evaluated_at, id)` with a UUIDv7
    id whose millisecond the database CHECKs against `evaluated_at` and a per-partition UNIQUE (id), so
    no two rows share an id anywhere; it is readable until its partition is detached
    (`retention + <1 day + ≤ purge_every` under a healthy pass) and on disk until the deferred drop
    (`… + ≤ 2 × purge_every`), never removed by row DELETE; and it is readable under `gate:evaluate`
    through the PROJECT-scoped by-id route — which prunes to the one partition the id names with no
    application clock or date prefilter — and the paginated listing route of §5, with no
    service-existence check — proven by an HTTP read after the service is deleted, not by a SELECT;
13. the gate policy of a file-managed service is writable through the API (D13) and is absent from the
    bundle hash;
14. a decision request is bounded in CONCURRENCY and in RATE by the ten keys of §5a — process and
    per-principal in-flight caps, per-principal and process token-bucket rates, a begin-through-commit
    transaction budget, and a validated retention enforced by removing whole daily partitions — all
    checked BEFORE a transaction opens, so a 429 runs no report, writes no ledger row, and carries a
    `Retry-After` of `ceil(seconds to the next token)`, never below 1; an in-flight refusal consumes no
    rate token, the two rate buckets are debited as a unit or not at all, and a rate refusal releases
    its permit; the bounds are process-local and the cluster allowance scales with replicas, by
    contract;
15. the seal lag is bounded: `evaluated_at − sealed_through > max_seal_lag_seconds` makes every budget
    clause UNAVAILABLE with `seal_stale`; the floor is the derived constant
    `MinSealLag = LateArrivalGrace + CanonicalBucket + 2 × CanonicalBucket = 300 s`, and the domain test
    asserts that FORMULA against the constants rather than the literal; the lag is stated by the report
    path and shown on the service page; a quotable 30-day report whose watermark is older than the bound
    never yields `ALLOW` on budget;
16. `facts_fresh_until` has ONE formula — the minimum of the seal horizon (when a budget clause is
    assigned `block`/`warn`) and of the burn leases of rules whose clause is assigned `block`/`warn`;
    coverage and `ignore`d clauses are never in it — and it is present whenever any such constraining
    horizon exists;
17. override actor and revoker are stored as typed attribution PLUS an immutable server-derived label,
    and a later read of the override or of a decision that applied it names the same label; no
    client-supplied actor is accepted.
18. ledger maintenance never holds dispatch and never outlives its authority: it runs in its own
    goroutine, every statement of a pass runs on the pinned connection that holds the gate's own
    session-level advisory lock, and a `DETACH` or `ATTACH` blocked behind a lock leaves the dispatch
    tick firing on cadence — proven by a scheduler test whose fake store blocks the pass while dispatch
    calls keep arriving, and by a fencing test in which the old side's backend is terminated while its
    detach is blocked, a successor acquires, and the old side can issue no further statement (its next
    one fails on the dead connection; the partition is removed exactly once, by the successor); and no
    session-level setting of the pass survives it — `RESET` after every guarded statement and at
    release, the cleanup owning the last 3 s of ONE 30 s lifecycle (work deadline 27 s), with
    `pg_advisory_unlock`'s boolean as the proof of release and the connection hijacked and closed on
    any unknown outcome — `SHOW` = 0 after reacquire on the clean, lock-timeout and statement-timeout
    paths, the poisoned pid never borrowed again, a successor able to acquire, and the WHOLE
    acquire→cleanup lifecycle measured at ≤ `subCadenceTimeout` even when removal spends its full
    budget and a reset blackholes;
19. a decision the ledger cannot hold is not a decision: with no partition for `evaluated_at` the
    insert fails, the API answers 503 `ledger_unwritable`, nothing is emitted, the CLI exits 1 — and the
    lead of `decision_partition_lead_days` plus the `writable_horizon_seconds` gauge make that failure
    need a leader absent for the whole lead time, which `max_seal_lag_seconds` has already turned into
    `UNKNOWN`;
20. the registry is crash-consistent and ownership is the marker: create, attach and drop each commit
    with their registry write in one transaction, `DETACH … CONCURRENTLY` alone runs behind a committed
    `detaching` intent and every outcome of a crash around it (never ran / detach-pending / ran with the
    state write lost) is reconciled on the next pass — tested by crashing at EVERY statement boundary;
    a relation is ours by `COMMENT 'cerbix:gate-ledger:<owner_token>'` plus shape, `relid` is refreshed
    when the marker matches a new OID (a stale-OID mutation and a `pg_dump`/`pg_restore` on the live
    stack both pass clean), and only a marker or shape mismatch or an unreconcilable state refuses — that
    day alone — counted in `cerbix_gate_maintenance_errors_total{kind="partition_identity"}` and paged; a
    drop happens once `detached_at <= now() − purge_every` (inclusive, on the database clock) and only
    when no backend holds a transaction older than the detach;
21. policy and override are managed through the eight routes of D13a and nothing else: `PUT`/`DELETE`
    of the policy require `expected_revision`, `revision` is a per-service generation that a
    delete-and-recreate never reuses, and a mismatch — including an identical body with a stale
    revision — changes nothing (409, CAS before no-op); bodies are decoded strictly and carry every
    D11/D14 field, the override body no `action`; an override binds to the policy revision the caller
    saw, one active per service, at most 7 days; revocation is by immutable override id and a revoke of
    an expired, revoked or foreign override is 409/404, never 204; any override is readable by id with
    both actor triples and its status — proven by the stale-UI race (A expired, B created, `DELETE
    …/overrides/{A}` leaves B active) and the delete-recreate race (A holds N, policy deleted and
    recreated, A's `PUT` and `POST` with N are both 409 and nothing moves);

## 7. Required test matrix (written before the code)

*Decision algebra:* intact budget → `ALLOW` with every evidence field · `page` rule firing → `BLOCK`
with `page_burn_firing`, AND the matching `ticket` clause reported too · `budget_consumed` only → `WARN`
· warn + block → `BLOCK`, both reasons · known `BLOCK` + one stale `warn` clause → `BLOCK`, the stale
clause in `reasons[]` as unavailable · only an `ignore`d clause unavailable → `ALLOW`, not `UNKNOWN` ·
a `warn` clause unavailable under `unknown_behavior: warn` → `state UNKNOWN`, `action WARN`, exit 0 ·
under `block` → `action BLOCK`, exit 2 · no policy → `NOT_CONFIGURED`, no action, exit 4 · a service
with two targets: the policy's `window` selects one, and the OTHER window's firing rule changes nothing
· `BurnedPercent` at exactly the threshold matches (`>=`), one below does not.

*Owners and parity:* the badge says `unroutable` and `coverage_state` says `unroutable` — the same
string from the same snapshot · the report withholds availability and the gate reports
`budget_withheld`, quotes no number · a gate that recomputes `BurnedPercent` locally fails the parity
test (mutation) · **a quotable 30d report whose `sealed_through` is older than `max_seal_lag_seconds` gives
`seal_stale`, never `ALLOW` on budget**; the same report one second inside the bound gives `ALLOW` ·
`ignore`d burn clauses with STALE burn latches do not produce `UNKNOWN` · **live materializer at the
floor:** a policy with `max_seal_lag_seconds = 300` on a healthy stack (sealer running, lag in
`[2m, 3m)`) is NOT `seal_stale`; a write of 240 is refused with the derived floor in the message ·
`facts_fresh_until` equals the seal horizon when that is earlier than every constraining burn lease, and
the earliest such lease otherwise · a policy whose burn clauses are all `ignore` and whose budget clauses
are `block`: `facts_fresh_until` = the seal horizon alone, whatever the burn leases say · a policy with
every clause `ignore`: no `facts_fresh_until` · a stale COVERAGE lease moves nothing.

*One snapshot:* an incident opened, a policy edited and a latch flipped BETWEEN what would be separate
reads all land on one side of `evaluated_at` (real Postgres, a blocker connection) · a forced
serialization failure retries once, then 503.

*Override:* created through its endpoint: `state` stays `BLOCK`, `reasons[]` unchanged, `action =
ALLOW`, `unoverridden_action = BLOCK`, `override.id` set, metric `state="BLOCK",action="ALLOW",
overridden="true"` · the same field in a decision body is refused as unknown-field · expired changes
nothing AND a new override can be created after it (the slot is released) · a second active override is
409 · two concurrent creates: exactly one wins · a policy edit revokes the active override and the next
decision is un-overridden · a policy DELETE revokes it too · `expires_at` of 8 days refused.

*Policy writes:* missing clause 400 naming it · unknown clause 400 · duplicate 400 · no
`unknown_behavior` 400 · a window the service has no target for 400 · identical rewrite: no revision
bump, no audit row · stale `expected_revision` 409 · file-managed service: policy write 200 while a
paging write is still 409.

*Presence:* raw JSON pinned for `NOT_CONFIGURED` (no `action`, no policy fields), for
`window_target_missing` (policy fields present, no target/objective) and for `never_sealed` (no
`sealed_through`, no `fact_revisions`) — the D7 table is the assertion.

*Tenant:* malformed id 400, foreign well-formed id 404, on decide, policy, override and ledger alike.

*Ledger:* a decision is readable by id through `GET /projects/{p}/gate/decisions/{id}` · after service
rename the row keeps the old slug and name · after service DELETE the row survives with `service_id
NULL` and the HTTP read still answers 200 · a row is readable until its partition is detached and gone
from disk after the deferred drop — the two boundaries of D10, each tested against the cadence.

*Bounds (§5a):* the begin-through-commit budget fires and is exit 1 · the per-principal in-flight cap
rejects the (n+1)th concurrent request from one token as 429 with `Retry-After: 1`, no transaction, no
ledger row · the process-wide cap rejects across TWO tokens the same way · **a sequential flood from one
principal at concurrency 1 is rate-limited** (429 with `Retry-After` = seconds to the next token, no
report run, no ledger row) — the mutation removing the rate bound lets it through and fails · each of the
ten keys refuses a value one below its minimum and one above its maximum at configuration load, naming
the key · the maintenance pass detaches and drops the eligible partitions and
`…_partitions_pending_drop`, `…_oldest_partition_age_seconds`, `…_writable_horizon_seconds` and
`…_bytes` move as the catalog changes, with no row count in any of the four queries · the CLI on a 429
prints `Retry-After` to stderr and exits 1 without retrying.

*Domain:* `MinSealLag == LateArrivalGrace + CanonicalBucket + 2*CanonicalBucket` asserted as an
expression over the constants, all three resolved from `internal/domain`; a write of
`max_seal_lag_seconds = 240` is refused naming 300.

*Limiter boundaries (§5a):* an in-flight refusal leaves the principal's token count unchanged · a rate
refusal releases the in-flight permit (the next request is not refused for concurrency) · `Retry-After`
for a rate refusal equals `ceil` of the time to the next token and is `1` at minimum · two identical
bursts against a fresh bucket receive identical `Retry-After` sequences · **process bucket empty,
principal bucket available: the principal's token count is unchanged after the 429** (the mutation
debiting the buckets one after the other fails this).

*Partitions:* a decision written on the last second of a UTC day lands in that day's partition · the
migration leaves `[today, today + 7 d]` attached AND registered · with lead 7, five days missing and
`create_max = 3`, the pass creates the three NEAREST days and the next pass the other two · a partition
is built standalone and attached: while a decision insert holds its row lock the attach completes (it
needs no `ACCESS EXCLUSIVE`), and a `CREATE … PARTITION OF` (the mutation) is not in the code (asserted
by grep over the store) · with retention 7 d and a pass every 5 m, a partition whose upper bound is 8 d
old is detached CONCURRENTLY, the one at 7 d is not, and after the pass no attached partition has an
upper bound at or before the cutoff · **logical readability**: a row is readable up to the pass after its
partition becomes eligible and 404 from the detach on; **physical**: its table is gone by the second pass
after eligibility — both measured against `purge_every`, never asserted as "never seen past" · an
interrupted detach (detach-pending in the catalog) is `FINALIZE`d on the next pass · a detached partition
is NOT dropped on the pass that detached it, is dropped on a later pass, is skipped while a planted
transaction with `xact_start` before the detach stays open, and — on a throwaway `postgres:15.8-alpine`,
the last 15.x before the fix — a PREPARED statement against the parent, planned before the detach and
executed between detach and drop and again after the drop, returns rows and no error (the cached-plan
path the 15.9 note names) · stage accounting: with `purge_max = 3` and one finalize, one open drop and
two eligible detaches pending, the pass does finalize, drop, oldest detach and stops · load refuses
`purge_every = 24h` with `purge_max_partitions` 1, 2 and 3, accepts 4, and refuses `create_max = 1` at
`24h` · a detach or attach held behind an `ACCESS EXCLUSIVE` lock is refused by `lock_timeout` in ≤ 2 s,
counted `maintenance_errors_total{kind="lock_timeout"}`, and a decision insert during it commits · the
drop runs on the pass where `detached_at` is EXACTLY `now − purge_every` and not on the pass one second
before (exact boundary and just-before, on the database clock) · with `create_max = 8` and every attach
blocked behind a held lock — INCLUDING the attach started immediately before the 12 s boundary — the
last creation statement is refused by its clamped timeout at the boundary, the first removal statement
begins by t = 12 s (+ one scheduler tick of slack), and an eligible detach completes in the same pass ·
a whole create transaction — CREATE, unique index, COMMENT, registry INSERT, COMMIT — completes with a
fake clock advanced between every statement, each statement's `statement_timeout` equal to the
remaining slice and `lock_timeout = min(2 s, that)` (the mutation that admits by the SUM of the two
timers starves the second statement and fails) · a statement blocked at the
boundary is cancelled by the transaction's context deadline and its transaction rolled back by t = 12 s ·
with fewer than 500 ms of slice left no statement is started · a pass whose removal spends the full
`[12 s, 27 s)` and whose `RESET` blackholes measures ≤ 30 s from `passStart` to cleanup end (the declared
total, not the cleanup alone) · `pool.Acquire` delayed to t = 13 s (fault-injected): creation is skipped,
the first removal statement begins immediately, and cleanup still ends by `passStart + 30 s`; delayed to
t = 26.8 s: no work statement starts and cleanup ends by 30 s ·
with the horizon exhausted the decision transaction fails, the API answers 503 `ledger_unwritable`, no
state is emitted and no row exists · in both storage modes.

*Ownership:* the pass is crashed (fault-injected exit) after EACH statement of create, attach, detaching,
detach, detached, drop, and the next pass converges every time with registry and catalog agreeing and
no day refused · a registry row `detaching` with the partition still attached → the DETACH runs; with
`inhdetachpending` set (the DETACH cancelled while waiting behind a held lock) → `FINALIZE`; with the
relation standalone → `state = detached` · every `relid` in the registry is set to a stale value: the
pass rebinds each by marker and shape and refuses nothing · on the live stack, `pg_dump` + `pg_restore`
of the ledger into a fresh database and one pass: zero `partition_identity`, all `relid`s refreshed · a
relation planted under next day's name WITHOUT the marker, or with another token, or with our marker
but a different bounds CHECK, refuses that day, counts `partition_identity`, leaves the relation
untouched, and the OTHER days are still created and removed in the same pass · a registry row
`attached` whose partition was detached by hand is refused, never re-created silently · the four gauges
are computed from the refreshed `relid`, and renaming a partition by hand changes none of them.

*Fencing and the loop:* the old side holds the gate session and is blocked in a detach behind an
`ACCESS EXCLUSIVE` holder; `pg_terminate_backend` on its pid; a successor acquires the gate session and
completes the removal; the old side's next statement fails on the dead connection and the partition was
removed exactly once · a scheduler test whose fake store blocks the pass for longer than one tick sees
the dispatch tick fire on cadence · after a clean pass, after a lock-timeout refusal and after a
statement-timeout abort, the released connection is reacquired and `SHOW lock_timeout` and `SHOW
statement_timeout` are both `0` · after a dead-connection release the poisoned backend's
`pg_backend_pid()` is never handed out again (compared over 50 reacquisitions), a successor acquires the
gate lock, and — under `MinConns = 0` only — `TotalConns` drops by one before any replenishment · a
`RESET` that blackholes (fault-injected wait) and an unlock that errors each end release within the 3 s
cleanup deadline, terminate the backend and let a successor acquire; the mutations that restore
`context.Background()` or ignore the unlock's boolean fail · the three `cerbix`-namespace advisory keys
(scheduler, migrations, gate maintenance) are pairwise distinct (a test over the constants).

*Policy and override routes (D13a):* `PUT` with `expected_revision` one behind → 409 and the policy
unchanged; an IDENTICAL body with a stale `expected_revision` → 409, not 200 (CAS before no-op); `PUT`
with `null` on a never-configured service creates revision 1; `DELETE` with the wrong `expected_revision`
→ 409; `DELETE` then evaluate → `NOT_CONFIGURED`, exit 4, and the earlier override reads as `inert`
through `GET …/overrides/{id}` with `revoked_reason: policy_deleted`, `revoked_at` set and a null
revoker triple · `DELETE` without `expected_revision` → 400 `expected_revision_required`; with `abc` or
`-1` → 400 `expected_revision_invalid`; a malformed `override_id` → 400 on every override route;
`limit=0` and `limit=-5` → 400 `limit_invalid` · **the delete-recreate race**: A reads N, `DELETE` (N+1), `PUT null`
recreates (N+2), A's `PUT` with N → 409 and A's `POST …/override` with `policy_revision: N` → 409, the
recreated policy and its override state untouched · a body missing `budget_consumed_percent`, a body
with an unknown field, a body with `clauses` missing one clause of the version, and an override body
carrying `action` → 400 naming the field · `POST …/override` with a stale `policy_revision` → 409
`revision_conflict`; a second active → 409 `override_active`; 7 days + 1 s → 400 · **the stale-UI race**:
A created, A expires, B created, `DELETE …/overrides/{A}` → 409 `override_not_active` and B is still
returned by `GET …/override`; `DELETE …/overrides/{B}` → 204 and a second `DELETE …/overrides/{B}` →
409 · `GET …/overrides/{A}` after that sequence → `status: expired`, `revoked_at` set by B's creating
transaction, `revoked_reason: expired`, the actor triple present and the revoker triple null;
`GET …/overrides/{B}` → `status: revoked` with both triples and `revoked_by_label` equal to the revoker's
`AuditLabel` (invariant 17's later read) · the status function is table-tested over every combination of
`revoked_reason` × expiry × revision match, and the overlapping case is asserted on BOTH sides of its
closure: an unrevoked, expired, revision-mismatched row reads `inert`, a `POST` then closes it as
`revoked_reason: expired`, and it still reads `inert` — the mutation that ranks a recorded `expired`
above the mismatch fails this stable-pair assertion, not merely one pre-closure read · raw JSON of a
manual revocation by an API token: `revoked_by_label: "token:<name>"`, `revoked_via_token: true`,
`revoked_by_user_id: null`, `revoked_at` and `revoked_reason: manual` set; of an active row: all five
closure fields present and `null`; of a system closure: two lifecycle fields set, three attribution
fields `null` — no field ever absent · `GET …/overrides` after 120
rapid create-and-revoke rounds, 40 of them sharing one `created_at` (fixed clock), returns exactly the
newest 50 in `created_at DESC, id DESC` order, identical across ten calls, and `EXPLAIN` on the analyzed
fixture shows the `(service_id, created_at DESC, id DESC)` index and no Sort node · a foreign or unknown override id → 404 on every override route · every `GET` shape equals the
fence of D13a field for field (raw-JSON schema test) · authz: `gate:policy` cannot `POST` an override
and `gate:override` cannot `PUT` a policy (403 each).

*Identity and reads:* an id's embedded millisecond equals `evaluated_at`'s for a row written 1 ms before
midnight with the application clock 5 s ahead (the id is built from the DB timestamp) · an INSERT whose
id millisecond differs from `evaluated_at` (planted) is refused by the CHECK · two INSERTs with one id
into one day are refused by the local unique index · the id-only read prunes to one partition (`EXPLAIN`
shows one child) and to zero children for a day that was detached or never created, answering 404 in
both cases with no application-side date check (the fake-store test asserts the query is issued) ·
the listing refuses a missing range, `from ≥ to` and a 32-day range, treats the range as `[from, to)` (a
row at exactly `to` is excluded, one at exactly `from` included), caps `limit` at 200, orders
`evaluated_at DESC, id DESC`, binds the next page strictly below the cursor's `(evaluated_at, id)` so a
row equal to the cursor never repeats, refuses a malformed cursor with 400 `cursor_invalid`, encodes
`next_cursor` from the LAST RETURNED row and sets it `null` when the `LIMIT + 1` probe finds nothing —
`limit = 2` over three fixed rows yields 2 + 1 with no gap, and the mutation encoding the extra row fails
— and, with a writer inserting throughout the traversal, yields every row committed before page 1 and
still attached exactly once and never repeats a returned key · a decision transaction that takes its
`statement_timestamp()` before page 1 is read and commits after it is NOT asserted to appear — the test
holds it open across page 1, commits it, and asserts only that the traversal has no duplicate and that
the row is readable by id afterwards, making the live-keyset limitation explicit rather than hidden · `EXPLAIN` of a 31-day page is one statement whose Append/Merge Append has ≤ 32 children (pruning
asserted on every fixture); the INDEX assertions run on populated, `ANALYZE`d representative children
only (review round 11 P2-3 — the planner may rightly seq-scan an empty or tiny child, and plan-node
choice for such a child is not a product invariant): with 100 000 project rows per child, each child
scans `(project_id, evaluated_at DESC, id DESC)` with no post-filter; with a `service_id` that owns 10
rows, each child scans `(project_id, service_id, evaluated_at DESC, id DESC)` with no filter on
`service_id` after the scan, and `pg_stat_statements` counts one statement; a range inside one day shows
exactly one child; semantic correctness (same rows, same order) is tested separately over empty and
tiny children · raw-JSON presence over four rows — `NOT_CONFIGURED`
(no `action`, no `policy_revision`, no `override_id`), non-overridden (`action`, `policy_revision`, no
`override_id`), overridden (all three), deleted-service (`service_id: null`, slug and name present) —
matches the by-id response byte for byte in field set · a foreign `service_id` yields an empty page, not
404 ·
ledger reads are refused with 429 `process_inflight` when the in-flight permits are exhausted and consume
no rate token · a synthetic 5 KiB evidence fails the decision at the CHECK and a canonical 1 KiB
evidence's bytes equal the fixture's byte for byte.

*Attribution:* an override created by an API token is read back — on the override and on a decision that
applied it — with `actor_label = token:<name>`, after the token is deleted too · a token REVOKER is read
back as `revoked_via_token = true`, `revoked_by_label = token:<name>` · a client `actor` field on either
endpoint is refused as unknown-field.

*CLI:* `CERBIX_TOKEN` absent → exit 1 with a message naming the variable · a `--token` flag is not
accepted · transport timeout → exit 1 · TLS with an unknown CA fails; with `CERBIX_CA_FILE` succeeds ·
`--json` output is byte-identical to the API response.

## 8. Threat model (facts corrected per review P1-7)

| Threat | Mitigation |
|---|---|
| Pipeline approves itself | override is not a request parameter (D9); it needs `gate:override`, leaves an audit row, is bound to the policy revision, and is counted |
| Stolen CI token | can evaluate the gate and read the ledger — facts the token could already read on the service page — within the bounds of §5a — evaluation under rate AND concurrency, ledger reads under concurrency only above, and nothing more unless it also holds `gate:override`. A narrower token capability is a recorded follow-up, not a v1 promise |
| Policy deleted or loosened mid-pipeline | deletion → `NOT_CONFIGURED`, which has no action and its own exit; every policy write is audited and bumps a revision the decision cites |
| Override outlives a policy tightening | a policy edit or deletion revokes the active override in the same transaction (D9) |
| Override rewrites history | an override changes only `action`; `state` and `reasons[]` in the response, the ledger and the metric stay the observed facts (D9) |
| Stopped materializer, stale budget quoted as good | `max_seal_lag_seconds` makes budget clauses unavailable past the bound (D8a); the lag is on the service page too |
| Time skew between pipeline and cerbix | every timestamp in the response is DATABASE time; the pipeline compares nothing against its own clock |
| Replaying an old `ALLOW` | a decision is not a capability and carries no authorisation; the pipeline asks immediately before the step (D6); a replayed id resolves in the ledger to its own `evaluated_at` and is visibly old |
| Reading unsealed facts to "improve" freshness | forbidden by D8, invariant 6; no heartbeat access from the decision path |
| Cross-tenant probing | 400 malformed / 404 foreign (D15), the existing transport contract |
| Load amplification from busy pipelines | **there is no API-token rate limiter today** (only the login IP limiter), and a decision is NOT one row: the report path runs bounded but real scans over sealed buckets and segments. A per-token cap alone is bypassed by a second token, a viewer cookie or an OIDC client. Concurrency caps alone do not bound WRITE amplification: one principal at concurrency 1 can still create expensive reports and immutable ledger rows without limit (review round 3 P1-3). Mitigations: the ten bounds of §5a — process and per-principal in-flight caps, per-principal and process token-bucket rates debited as a unit, a transaction budget, validated retention with bounded removal stage-ops and a bounded, horizon-first creation budget, ledger reads under the same in-flight permits, and a listing capped at 200 rows over at most 31 days — all checked BEFORE any transaction opens (429 with `Retry-After`, no report, no row); the window fixed by the policy so the scan is bounded by it; **no cache**, which would be a second semantics owner. The bounds are process-local and the cluster allowance scales with replicas, stated as the contract. Every bound is tested, and the rate bound has a mutation |
| `UNKNOWN` silently read as green | `unknown_behavior` is the operator's declared choice (D5); the CLI exit follows `action`; the word `UNKNOWN` is in the output regardless (D16) |

## 9. Non-goals of FR-024

Deployment events and their timeline, incident–change correlation, before/after SLI comparison (all
FR-025); budget reservation or any lease across a deploy (D6); automatic rollback or any action by
cerbix on an external system (an invariant, not a deferral); a project-level inherited policy (D13,
second step); a token-scoped gate capability (D12, follow-up requirement); worst-of-all-windows
evaluation (D2); vendor-specific CI integrations — one generic endpoint and one CLI verb are the whole
surface; reading raw heartbeats from the decision path (D8); a decision cache (§8); an override that
alters the observed state (D9).

## 10. Stale-spelling guard

Four review rounds each found a normative sentence still carrying the previous revision's contract —
the seal-lag field without its unit suffix after it gained one, the old duration range after the floor
was derived, a lease-only `facts_fresh_until` after the seal horizon joined it, "first statement" after
it became the first snapshot-bearing one. An implementer or the discharge map could legitimately have
picked the wrong sentence. `make docs-check` therefore refuses the spellings that earlier revisions used and that mean something
different now — revision 6's own vocabulary included, since a guard whose retired list stops at the
revision before last lets the last one through (review round 6 P1-4) — in three places (review round 5
P1-2 corrected the scope): **this file entirely,
including the normative schema fence of §5** — a fence is skipped ONLY when it opens with the literal
info-string `retired-spellings`, which marks the one fixture block below; **the FR-024 and NFR-019 rows
of `docs/status.md`**, because the live status outranks the decisions and had kept a retired spelling of
its own; and **`docs/decisions.md` inside any `## D-` section whose heading names FR-024**, scoped by
heading rather than by a rolling window of lines. A line is exempt only if it is a blockquote, or says
"at the time" or "renamed in revision" — the two phrases a supersession note uses to quote what it
supersedes — and not merely because it mentions a revision number.

```retired-spellings
max_seal_lag            (not followed by _seconds)
1m..24h
minimum of applicable leases / minimum of the applicable leases
first statement supplies
decision_purge_batch / purge_backlog_rows / oldest_eligible_seconds   (revision 6's row-DELETE purge)
partition per calendar month / monthly RANGE                          (revision 6's partition period)
fact_revision_ids                                                     (the unbounded revision list)
carries no time / 373 / full list is recoverable                     (revision 7's id, partition count, evidence claim)
CREATE TABLE IF NOT EXISTS … PARTITION OF                            (revision 7's creation under ACCESS EXCLUSIVE)
`retention + 1 day`   (with the closing backtick right after "day")   (revision 7's lifetime "bound")
retention + 1 day + purge_every / + decision_purge_every                (revision 8's one-cadence-short formula)
ledger_identity / without touching the database / unbudgeted            (revision 8's read and creation contract)
evaluate_errors_total{kind="partition_identity"}                        (revision 8 counted maintenance in the evaluation family)
the two indexes / INDEX (id)                                            (revision 9's index inventory)
older than one `decision_purge_every`                                   (revision 9's strict drop predicate)
plus `state`, `action`                                                  (revision 9's list item shape)
2 × 2 s × create_max                                                    (revision 9's count-only creation bound)
within the rate and concurrency bounds                                  (revision 9's threat row; reads take no rate token)
the extra row's existence / from the `LIMIT + 1` row                   (revision 10's cursor, which skipped a row)
policy_revision, action, reason                                         (revision 10's override body with a client action)
pool holds one connection fewer                                         (revision 10's unstable release oracle)
ONE index-range scan                                                    (revision 10's EXPLAIN claim over 32 children)
no later than t = 12 s with budget                                      (revision 10's between-operations check)
`GET …/override` history                                                (revision 10 called the active-only read history)
lock_timeout + statement_timeout                                        (revision 11's additive admission; the clamp is a wall bound)
six policy/override routes                                              (revision 11's §4 count; there are eight)
null until revoked                                                      (revision 11's revoker fields; system closures set revoked_at)
seven-day regime keeps that small                                       (revision 11's false row bound on override history)
each pass bounded to `subCadenceTimeout` / Each pass is bounded to      (revision 11's 30 s + 3 s timeline)
non-null for `manual` / has a human revoker                             (revision 12's attribution wording; token revokers have a null user id)
same status before and after / cannot duplicate or skip an item         (revision 12's status claim; revision 9's snapshot claim)
a new row has a later `evaluated_at`                                    (revision 9's false pagination proof)
```

It also refuses a duplicated table header in §5. **The guard has fixture tests**
(`scripts/check_docs_references_test.py`, run by `make docs-check`): a D14 sentence, a schema-fence
column, a status row and a late decision line each carrying a retired spelling are flagged; the fixture
fence, a non-FR-024 decision and a quoting blockquote are not. Its first draft skipped every fence,
which would have let a retired column name through in the very block that defines the column — and the
same author had just announced a revision whose gate was red because the gate's exit code was hidden
behind a pipe. A guard that is not itself tested is a sentence about a guard. The list lives in `scripts/check-docs-references.py` beside the invariant-set check and
grows when a later revision retires another spelling.
