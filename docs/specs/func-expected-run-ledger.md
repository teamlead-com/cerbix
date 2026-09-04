# Spec: The fact that a run was expected (func-expected-run-ledger)

> **Lifecycle: DESIGNED — revision 1, 2026-09-04. AWAITING DESIGN REVIEW; NOT IMPLEMENTED.**
> Opened by `D-0235` at iter-0174 as the requirement that must exist before any surface may draw a
> value across an interval it did not observe. §1–§3 are the problem and the facts a solution must
> carry; §4a are the reviewer's constraints, recorded when they were given. **§5 onward is the
> design**, written after the owner chose the expectation model on 2026-09-04 (§5.1).
>
> Nothing here is built. No requirement row moves, no migration exists, and the FR-031 panel keeps
> drawing points with no stroke until §9's acceptance gate is met by working code.

## 1. The problem, in one sentence

cerbix records what happened. It does not record what was **supposed** to happen — so it cannot
tell the difference between "no check was due here" and "a check was due here and never ran".

## 2. Why this is a requirement and not a chart detail

FR-031 wanted a connected line on the monitor's Response time panel: a stroke between two
recorded checks, broken wherever a check was due and missing. Two designs for deriving that from
existing facts were proposed and both were rejected, the second at party [166] on three readings
of the tree that were verified before acceptance:

- `internal/prober/prober.go` runs `Retries + 1` attempts, each under its own
  `context.WithTimeout(m.Timeout())`, so one run may occupy up to `(retries+1) × timeout`.
- `result.allowed_skew` is only step 4b's clock test in `internal/store/monitors.go` —
  `ts.Before(hb.JobIssuedAt.Add(-skew))`, a bound on how far *before* its job's issue an
  observation may claim to be. It bounds neither queue delay nor delivery.
- `job_issued_at` is **not a column of `heartbeats`**. It rides the wire for correlation and is
  gone after ingest.

So two received heartbeats bound **observed spacing** and nothing more. Under broker or worker
trouble the queue wait is unbounded; leader absence leaves no trace at all; and cerbix cannot
witness its own absence. No allowance closes that, because the subject of the allowance — an
expected run — is not in the data.

The consequence for FR-031 is recorded in `func-truthful-rendering.md` §6.2: the panel draws
points, no stroke, no fill, and renders absence positively through an observation ruler. That is
honest with today's facts. **This requirement is what would make a stroke defensible.**

## 3. The facts a solution must carry

Specified by the reviewer at party [166] and [169] as the minimum, and recorded here so a later
design cannot quietly ship less:

1. **Materialized due windows** — the intervals in which a run was expected, as a durable fact
   rather than a recomputation from current configuration.
2. **Issue, claim and terminal-outcome timestamps** per expected run, so a run that was issued and
   never completed is distinguishable from one that was never issued.
3. **Historical configuration binding** — the interval, retry count and timeout in force for
   *that* run. Current monitor fields may not stand in for history: a monitor whose interval
   changed last week says nothing true about the week before.
4. **Retention semantics** — how long the ledger is kept, and what a surface may claim about a
   span whose ledger rows have been dropped.

Only acceptance criteria over these facts may permit a `covered` verdict, and only a `covered`
verdict may permit a stroke between adjacent windows.

## 4. Requirement

- **FR-032** — cerbix records that a run was expected, so a surface can distinguish an interval
  nothing was due in from an interval whose due run never happened. Status `TODO`.

## 4a. Constraints from the reviewer, before any design (party [192])

Recorded when they were given, so a later design starts inside them rather than rediscovering them.
**No status change and no design approval is implied by any of this.**

- The requirement **fits the existing role boundaries** — it does not need new ones.
- **The scheduler and control plane own the durable expected-run facts.** That is where "a run was
  due" and "it was never issued" can be known.
- **`worker` and `agent` stay DB-less executors.** They return **idempotent** claim and terminal
  events over the existing transport; they do not gain a database. This answers §5's DB-less
  question: the two facts an executor owns already ride the path a result rides.
- **Do not overload `heartbeats`.** This rules out the cheap-looking move of persisting
  `job_issued_at` on the heartbeat row and calling it a ledger: **absence is the case the ledger
  exists to preserve**, and a heartbeat only exists where something happened. A row that only
  appears when a run produced a result cannot record a run that produced none.
- **AMQP claim ordering and crash semantics are the key design risk**, and they must be resolved in
  the design record before any code.

## 5. The model — durable expectation

### 5.1 The decision and who made it

Three models can carry "a run was expected". The owner chose the second on **2026-09-04** after
being shown all three with their operational cost, and the deciding factor was named in the
question: the other two either change when every monitor is probed, or weaken §3.1.

| Model | How a missed run becomes visible | Why not / why |
| --- | --- | --- |
| **Absolute grid** — windows are `floor(unix / interval)`, as `domain.CanaryRunKeyAt` already does for canaries | Derivable from the grid alone, with no recorded state at all | **Rejected.** Strongest detection, and the mechanics are already in production for FR-029 — but dispatch would have to move onto the grid, which changes the probe instant of every monitor in every existing installation, needs deterministic jitter so that every `interval: 60s` monitor does not fire at `:00`, and doubles or drops one run at the cutover |
| **Durable `next_due`** — the leader persists the instant it already computes in memory | The persisted `next_due_at` stays in the past while nothing advances it | **CHOSEN.** The expectation is written by the component that owns it, the probe instant does not change at all, and the cost is one row per monitor rather than per run |
| **Gap inference** — keep only `last_issued_at` and divide the gap by the interval | Counted as `floor(gap / interval)` | **Rejected.** Cheapest, but window boundaries are inferred rather than recorded, and an interval change inside the gap makes even the count approximate — weaker than §3.1 asks for |

### 5.2 What the chosen model replaces

`nextRun` in `internal/scheduler/scheduler.go:1111` is a `map[string]time.Time` created **empty**
each time leadership is taken, and consulted at `:1418`. Three consequences, all verified in the
tree before this design was written:

- The schedule is **emergent, not a grid**: every advance is `nextRun[m.ID] = now.Add(iv)`
  (`:1442`, `:1473`, `:1487`), chained from the actual publish instant.
- On leader loss the map is gone, so **every active monitor is due on the first tick** after
  failover — and no trace of the interval that went unprobed survives anywhere.
- The advance is deliberately **skipped on dispatch failure** (`:1466-1471`, `:1480-1486`) so the
  next tick retries promptly, and deliberately **taken on a policy skip** (`:1442`, `:1449` — no
  capable canary runner, no in-flight slot).

This design persists that map and changes none of those three rules. That is the whole point of
the model the owner chose: the durable fact is the one the scheduler already computes, so the
probe instant of every existing monitor is untouched.

## 6. Schema

Three tables. None of them is heartbeat-order at rest, and `heartbeats` is not touched — §4a
forbids it, and the reason is exactly right: absence is the case the ledger exists to preserve.

### 6.1 `monitor_schedule` — the durable expectation (one row per monitor)

```sql
CREATE TABLE monitor_schedule (
    monitor_id           uuid PRIMARY KEY REFERENCES monitors (id) ON DELETE CASCADE,
    next_due_at          timestamptz NOT NULL,
    interval_in_force    int         NOT NULL,   -- seconds; the interval that produced next_due_at
    confirm_phase        boolean     NOT NULL DEFAULT false,
    execution_revision   bigint      NOT NULL,
    last_issued_at       timestamptz,            -- set only after a dispatch SUCCEEDED
    last_issued_job_id   text,
    last_claimed_at      timestamptz,            -- executor said it was about to probe
    last_terminal_at     timestamptz,
    ledger_from          timestamptz NOT NULL,   -- earliest instant this row can answer for (§8)
    updated_at           timestamptz NOT NULL DEFAULT statement_timestamp()
);
```

Expectation is a statement about the **future**, so one current row is the whole fact; the history
of expectation is reconstructed from §6.2 and §6.3 rather than stored twice. `interval_in_force`
and `confirm_phase` are carried on the row because the confirm phase substitutes
`m.ConfirmInterval()` for a run (`scheduler.go:1428-1434`) — reading the monitor's current
interval would misdate every window that was probed under acceleration.

### 6.2 `monitor_execution_revisions` — the configuration timeline (§3.3)

```sql
CREATE TABLE monitor_execution_revisions (
    monitor_id               uuid   NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    execution_revision       bigint NOT NULL,
    interval_seconds         int    NOT NULL,
    confirm_interval_seconds int    NOT NULL,
    timeout_seconds          int    NOT NULL,
    retries                  int    NOT NULL,
    effective_from           timestamptz NOT NULL,
    PRIMARY KEY (monitor_id, execution_revision)
);
```

Written in the **same transaction** that bumps `monitors.execution_revision`. `UpdateMonitor`
already bumps that column on **any** write — a deliberately coarse fence, documented in
`internal/domain/execsemantics.go` — so one row per monitor write is the volume, and rows are
identical-but-renumbered whenever a write touched nothing cadence-related. That is accepted: a
duplicate row costs a few bytes, and deduplicating would reintroduce the allowlist that file warns
against.

This table is what makes `execution_revision` mean anything historically. Today it is a bare
counter: nothing anywhere records what configuration a given generation *was*.

**Limitation, stated rather than discovered later.** Revisions that already exist cannot be
reconstructed. The migration backfills exactly one row per monitor — its current revision, its
current fields, `effective_from = monitors.updated_at` — and the timeline therefore begins at
migration time. Every window before that is `unknown` by §8, not `covered`.

### 6.3 `expected_runs` — the sparse run ledger (§3.2)

```sql
CREATE TABLE expected_runs (
    monitor_id         uuid        NOT NULL,
    due_at             timestamptz NOT NULL,   -- the expectation this run answers
    job_id             text        NOT NULL,
    execution_revision bigint      NOT NULL,
    region             text        NOT NULL,
    issued_at          timestamptz,            -- core: dispatch returned success
    claimed_at         timestamptz,            -- executor: taken off the transport, about to probe
    terminal_at        timestamptz,            -- executor/ingest: an outcome exists
    outcome            text CHECK (outcome IN ('result', 'probe_error')),
    PRIMARY KEY (monitor_id, due_at, job_id)
) PARTITION BY RANGE (due_at);

CREATE INDEX expected_runs_job_idx ON expected_runs (monitor_id, job_id);
```

**A row is written only when expectation and outcome disagree.** In normal operation the heartbeat
*is* the terminal fact and no row is written at all: the in-flight state lives in the four
`last_*` columns of §6.1, and a row is materialized here only when the next dispatch finds that
the previous run never reached a terminal outcome. So at-rest volume is proportional to
**problems**, not to runs, and §5's volume question is answered without a rollup.

No verdict is stored. `covered`, `issued_never_claimed`, `claimed_never_finished` and
`expected_never_issued` are **computed** from which timestamps are present. A stored verdict would
be a fifth thing to keep consistent with the four facts under it, and a late event would make it
wrong.

`expected_runs_job_idx` exists for exactly one query: a terminal event arriving after its row was
flushed. That is §5's reconciliation question, and the answer is in §7.4.

## 7. Ordering, transactions and crash semantics

§4a named **AMQP claim ordering and crash semantics** as the key design risk and required it
resolved here, before code. This section is that resolution.

### 7.1 Every executor event is an idempotent single-column fill

A claim or terminal event **writes only its own column**, never reads another's, and never
regresses a value that is already set:

```sql
UPDATE monitor_schedule
   SET last_claimed_at = $2
 WHERE monitor_id = $1 AND last_issued_job_id = $3
   AND (last_claimed_at IS NULL OR last_claimed_at > $2);
```

This makes the transport's ordering irrelevant instead of requiring a guarantee it does not offer.
A duplicate claim is a no-op. A **terminal event that overtakes its own claim is legal** and loses
nothing, because the claim will fill its own column when it lands. There is no state machine to
violate, so no reordering can violate one.

### 7.2 The claim event is best-effort and never invalidates a terminal

The executor emits the claim **after** taking the job off the transport and **before** the first
attempt. If that publish fails, the probe still runs. A missing claim therefore means
"unwitnessed", never "did not happen", and **a terminal outcome always outranks a missing claim**.

`worker` and `agent` gain no database and no ack concept: the claim rides the existing result path
as a typed message, which is what §4a required.

### 7.3 The AMQP loss this makes visible for the first time

`internal/dispatch/amqp.go:191-194` states the policy plainly: *"a delivery is acked once handed to
the in-process channel. Losing a single check on a hard crash is acceptable — the scheduler
re-emits on the next interval."*

That trade is defensible and this design does not change it. What it changes is that the loss stops
being invisible: a worker that dies between ack and probe leaves `issued_at` set, `claimed_at`
NULL, `terminal_at` NULL — `issued_never_claimed`. The justification "the scheduler re-emits on the
next interval" is exactly the reasoning FR-031 could not verify, and after this it is a queryable
fact rather than an assurance.

### 7.4 Late events, and what reconciles with what

A row flushed as unfinished is **never deleted when its terminal event finally arrives**;
`terminal_at` is filled in and the row then reads as *completed late*, which is true and worth
keeping. This is §5's reconciliation question answered: nothing is retracted, one more column
becomes non-NULL, and the verdict changes because verdicts are computed (§6.3).

### 7.5 The publish is not in the transaction, and the failure direction is chosen

An AMQP publish cannot join a Postgres transaction, so one of two orderings must be chosen and its
failure mode accepted. **The schedule advance and `issued_at` are written after a successful
dispatch**, matching today's in-memory rule exactly.

- Crash *after* the publish, *before* the write: the run happened and the ledger does not know it.
  The arriving terminal event is proof of issue and reconciles it (§7.1).
- Crash *before* the publish: nothing was issued and the ledger says so, which is TRUE.

The residual error is therefore always in the direction of **withholding** — the ledger may briefly
under-claim coverage, and can never claim a run that did not happen. For a requirement whose only
purpose is to let a surface refuse to draw, that is the only acceptable direction, and it is why
this design does **not** move check dispatch onto the transactional outbox: doing so would put the
outbox worker's latency in front of every probe to fix an error that already fails safe.

### 7.6 Volume on the hot path

The schedule advance is one statement per **tick**, covering that tick's whole due set — not one
statement per monitor. The precedent is `internal/store/materialize.go:99`, which batches the
credentialed set as `WHERE m.id = ANY($1::uuid[])`; that is a batched READ and the advance is a
batched WRITE, so the shape is `UPDATE … FROM (VALUES …)` or `unnest`, not the same statement.
Named precisely because a design that cites a mechanism it does not actually reach is how a
plausible-looking claim survives review.

## 8. Retention, and what a surface may claim about a dropped span

`expected_runs` is daily-partitioned and dropped by age, the mechanism the reliability gate's
decision ledger already uses (`decision_retention_days`, D10, `PARTITION BY RANGE (evaluated_at)`).
`monitor_schedule` and `monitor_execution_revisions` are current-state and are not aged out.

**One divergence from that precedent, decided rather than inherited.** The gate ledger has **no
default partition** — a decision outside a known window is refused. `heartbeats` in
declarative-partition mode has one precisely so *inserts never lose data* (migration 00043).
`expected_runs` follows **`heartbeats`, not the gate**: it takes a DEFAULT partition, because a row
here is evidence that a run did not complete, and an insert lost to a missing partition would erase
exactly the fact the ledger exists to keep — silently, and in the dangerous direction. Retention
must therefore also purge rows out of the default partition, which the gate's mechanism has no need
to do.

`monitor_schedule.ledger_from` is the earliest instant the ledger can answer for:

```
ledger_from = max(oldest retained expected_runs partition lower bound,
                  earliest effective_from in the revision timeline,
                  the instant this monitor's schedule row was created)
```

Before `ledger_from` a surface may claim **nothing**: not `covered`, and not
`expected_never_issued`. It renders as **not stored**, which is an encoding FR-031 already has and
already draws. Dropping ledger rows must never silently promote unproven time to proven, and must
never invent a missed run out of retention.

## 9. What this unlocks, and the gate it must pass first

FR-031 §6.2 draws points with no stroke because nothing could defend a line. A stroke between two
adjacent drawn points becomes permissible only when **every** expectation between them resolves to
`covered`, and the whole span is at or after `ledger_from`. Any `unknown`, any
`expected_never_issued`, any span before `ledger_from` — no stroke.

Nothing else is claimed here. Coverage reporting and the reliability gate plausibly want these
facts too, and this document still does not claim that benefit, because nothing has been analysed.

## 10. Non-goals

- **No new role and no new deployment topology.** §4a settled this.
- **No ack concept at the `Dispatcher` seam.** `amqp.go:191-194` keeps its policy; the ledger
  observes the consequence instead of changing the transport contract.
- **Not a job queue.** `expected_runs` is evidence, never work to be claimed; nothing reads it to
  decide what to probe.
- **No change to any monitor's probe instant.** This is the property the owner selected the model
  for, and any implementation that drifts a probe instant has failed the design, not merely
  deviated from it.
- **No row per run in normal operation.**
- **No retroactive backfill.** History before the migration is `unknown`, per §6.2.

## 11. Phases

Each is separately reviewable, and every one before D is invisible to any surface.

| Phase | Content | Gate |
| --- | --- | --- |
| **A** | `monitor_execution_revisions` + write it in the revision-bump transaction; backfill one row per monitor | `-race`; a revision bump with no timeline row fails a test |
| **B** | `monitor_schedule`; the leader persists the advance it already computes; **job identity (`JobID`, `IssuedAt`) stamped on EVERY dispatch path**, which today only the credentialed path has (`scheduler.go:1456`/`:1476` carry none, `materialize.go:126` does) | `-race`; a leader restart leaves a past `next_due_at` |
| **C** | The claim event: `Dispatcher` grows a typed claim message; `worker` and `agent` emit it; the idempotent fills of §7.1 | `-race` + a live distributed stack; the fakes in `internal/api`, `internal/outbox`, `internal/scheduler` break on interface growth, which is intended |
| **D** | `expected_runs`, the flush-on-next-dispatch rule, partitions, retention, `ledger_from`, and the read API that returns verdicts | `-race`; both storage modes; E2E |
| **E** | The FR-031 stroke, behind §9's gate — and only if §12 is discharged | E2E on a live stack |

## 12. Acceptance invariants (FR-032)

Discharged as a SET in `docs/traceability.md`.

1. No monitor's probe instant changes as a result of this requirement.
2. The scheduler's three advance rules are preserved exactly: advance on successful dispatch,
   no advance on dispatch failure, advance on a deliberate policy skip.
3. `next_due_at` survives leader loss, and a leader that returns after a gap finds it in the past.
4. A window is `covered` only if a terminal outcome exists for it; a terminal outcome is never
   inferred from a heartbeat's mere presence at a nearby instant.
5. A run issued and never claimed is distinguishable from a run never issued.
6. A run claimed and never finished is distinguishable from both.
7. An executor event fills only its own column and never regresses a set value.
8. A terminal event that arrives before its claim event loses nothing.
9. A duplicate claim or terminal event changes nothing.
10. A terminal outcome outranks a missing claim and a missing `issued_at`.
11. `worker` and `agent` hold no database handle and no ack concept after this change.
12. `heartbeats` gains no column.
13. The interval, timeout and retry count attributed to a run are those in force for THAT run,
    read from the revision timeline and never from the monitor's current fields.
14. A monitor whose interval changed mid-gap has its missed windows counted against the timeline,
    not against either endpoint's interval alone.
15. Before `ledger_from` no verdict is emitted — neither `covered` nor `expected_never_issued`.
16. Dropping a partition never converts unproven time into proven time, and never invents a
    missed run.
16a. An `expected_runs` insert is never lost to a missing partition: the DEFAULT partition accepts
    it, and retention purges the default too (§8).
17. `expected_runs` holds no row for a run that was expected, issued and completed.
18. A row flushed as unfinished is not deleted by a late terminal event; it reads as completed late.
19. No verdict is stored; every verdict is computed from the timestamps present.
20. A stroke on the Response time panel is permitted only for a span that is entirely `covered`
    and entirely at or after `ledger_from`.
21. Nothing in the ledger is read to decide what to probe.

## 13. Open items, and the one ruling this design needs

**The ruling.** §3.1 asks for "materialized due windows … as a durable fact rather than a
recomputation from current configuration". This design materializes **one** durable expectation per
monitor (`next_due_at`) and, for a gap spanning many intervals, **derives** the individual windows
from that instant plus the revision timeline of §6.2. It is not a recomputation from *current*
configuration — the timeline is historical, which is the clause's stated concern — but it is also
not one stored row per window. **The reviewer should rule on whether that satisfies §3.1**, and I
would rather be told to store the windows than have this read as if the question were not there.
Storing them would make §5's volume answer heartbeat-order again, which is the trade.

**Still open, and not blocking the design review:**

- The exact claim-message shape on the wire, and whether it travels the results queue or its own.
  §4a fixes that it uses the existing transport; which queue is a phase-C detail.
- Whether `expected_runs` should also record the region's live-executor state at `due_at`, so a
  missed run in a region with no worker reads differently from one in a healthy region. Attractive,
  unanalysed, deliberately out of revision 1.
- Whether push monitors participate. They are never dispatched (`scheduler.go:1415`) and their
  staleness already has its own mechanism (`checkStalePush`), so revision 1 excludes them — but
  "a push that did not arrive" is a genuinely expected run in a different sense, and excluding it
  is a decision worth challenging.

## 14. Process

Its own design review — this revision is what goes to it — its own mock if any UI surface grows
beyond FR-031's existing panel, and a migration plan. Migrations start at `00100`.
