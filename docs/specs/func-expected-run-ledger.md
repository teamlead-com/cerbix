# Spec: The fact that a run was expected (func-expected-run-ledger)

> **Lifecycle: DESIGNED — revision 5, 2026-09-04. AWAITING DESIGN REVIEW; NOT IMPLEMENTED.**
> Opened by `D-0235` at iter-0174 as the requirement that must exist before any surface may draw a
> value across an interval it did not observe. §1–§3 are the problem and the facts a solution must
> carry; §4a are the reviewer's constraints, recorded when they were given. **§5 onward is the
> design.**
>
> **Two revisions were REJECTED on P0s, and both rejections are kept rather than deleted**, because
> each killed a design a reader would otherwise propose again. **Revision 1** ([218]) held in-flight
> state in one mutable row per monitor and could not represent two outstanding runs — §5.2.
> **Revision 2** ([225]) capped gap materialization with a batch-wide `LIMIT` while advancing every
> monitor, and never wrote the truncation fence its own prose promised, so an unanswerable span
> became invisible to `ledger_from` and a later query could over-claim it — §7. Revision 3 is dense
> per-window rows with a deterministic per-monitor cap and the fence written in the same statement as
> the advance. **Revision 4** answered three P1s from [227] — the five-rule advance audit (§7.2), the
> terminal upsert's SQL and validity rules (§8.3), and the configuration write that must leave
> `next_due_at` untouched (§10) — and was itself **REJECTED at [229] on a P0**: it had TWO
> forward-moving statements, and the policy-skip one argued its way out of gap materialization, so a
> leader whose first post-failover action was a skip lost the gap exactly as [225] had. **Revision 5
> has ONE forward-moving primitive** (§7.1) with every caller passing parameters, because two
> statements sharing an obligation diverge on it. §5.4 records the constraints from party [222].
>
> Nothing here is built. No requirement row moves, no migration exists, and the FR-031 panel keeps
> drawing points with no stroke until §14's gate is met by working code.
>
> **The participation scope in §5.3 is MY recommendation and is NOT an owner decision.** The owner
> has made exactly one ruling here — the expectation model, §5.1 — and the reviewer declined to rule
> on scope at [222] because it is a product and operating-cost decision. Nothing in this document
> may be read as though scope were settled.

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

## 5. The model

### 5.1 The owner's ruling — how expectation is anchored

Three models can carry "a run was expected". The owner chose the second on **2026-09-04**, and the
deciding factor was named in the question: the other two either change when every monitor is
probed, or weaken §3.1.

| Model | How a missed run becomes visible | Verdict |
| --- | --- | --- |
| **Absolute grid** — `floor(unix / interval)`, as `domain.CanaryRunKeyAt` already does for canaries | Derivable from the grid alone, with no recorded state | **Rejected by the owner.** Dispatch would move onto the grid, changing the probe instant of every monitor in every existing installation, needing deterministic jitter so that every `interval: 60s` monitor does not fire at `:00`, and doubling or dropping one run at the cutover |
| **Durable `next_due`** — the leader persists the instant it already computes in memory | The persisted `next_due_at` stays in the past while nothing advances it | **CHOSEN.** Expectation is written by the component that owns it, and no probe instant changes |
| **Gap inference** — keep only `last_issued_at` and divide the gap by the interval | Counted as `floor(gap / interval)` | **Rejected.** Boundaries inferred rather than recorded, and an interval change inside the gap makes even the count approximate |

`nextRun` in `internal/scheduler/scheduler.go:1111` is a `map[string]time.Time` created **empty**
each time leadership is taken, and consulted at `:1418`. Three consequences, all verified in the
tree: the schedule is **emergent, not a grid** (every advance is `nextRun[m.ID] = now.Add(iv)` at
`:1442`, `:1473`, `:1487`); on leader loss the map is gone, so **every active monitor is due on the
first tick** after failover and nothing records the interval that went unprobed; and the advance is
deliberately **skipped on dispatch failure** (`:1466-1471`, `:1480-1486`) and deliberately **taken
on a policy skip** (`:1442`, `:1449`). This design persists that map and changes none of the three.

### 5.2 What revision 1 got wrong, kept so it is not proposed again

Revision 1 wrote **no row in normal operation**: in-flight state lived in four `last_*` columns on
one row per monitor, and a run row was materialized only when the next dispatch found the previous
run unfinished. That was rejected as a P0 at party [218], and the reasoning is worth keeping:

- **One mutable row cannot hold two outstanding runs.** A following dispatch overwrites the single
  job identity, and the older run's terminal event then has no durable row to land on.
- **Overlap is not an edge case, it is a supported configuration.** `internal/domain/monitor.go:288`
  scopes the `interval_seconds >= timeout_seconds` rule to `async_canary` **only**, and the comment
  says why: *"an ordinary http monitor with a 30 s interval and a 60 s timeout is legal today and
  common, so writing this as a general rule would refuse configurations nobody meant it to."* A
  single attempt outlasting its interval is normal, before `Retries + 1` attempts
  (`internal/prober/prober.go:122`) are even considered.
- **The invariant set contradicted itself.** Revision 1 asserted all three of: no terminal is
  inferred from a nearby heartbeat; no row exists for a completed run; a stroke requires a fully
  `covered` span. The first forbids inference, the second forbids evidence, so the third could
  never be satisfied for any historical span. The headline volume answer made the headline feature
  unreachable.
- **Therefore §3.1 is NOT satisfied** by one `next_due` plus configuration history. That was the
  reviewer's explicit ruling on the question revision 1 asked.

### 5.3 Participation scope — MY RECOMMENDATION, NOT A DECISION

Dense rows make the cost explicit, so who participates became a real question. **I recommend
universal participation with retention as its own configuration value** (§12.3). The reviewer
declined to rule, correctly, because it is a product and operating-cost decision. The owner has not
ruled. Until they do, this document specifies universal participation because a design has to
specify something, and every place it matters says so.

The two alternatives, so the decision has something to compare against: a per-monitor opt-in
(default off — zero cost, but two different semantics in one product and a field, a migration, a UI,
a MaC key and an openapi change), or automatic participation for SLI members of a service only
(nothing to configure, but a monitor's ledger appears and disappears as someone else edits service
membership). At the owner's ~50 monitors neither buys anything; at 10,000 the opt-in earns its
place (§12.4).

### 5.4 Constraints from the reviewer at party [222], and where each is discharged

| Constraint | Discharged in |
| --- | --- |
| Storage figures are estimates until a model or measurement supports them; the gate needs a retention default, min/max and a capacity calculation | §12.1 states the model term by term; §12.2 the capacity; §12.3 the bounds; invariant 22 makes the measurement a gate |
| `<=1` issue statement is not established by batching: dense issue needs the run rows AND the advance. Give the single transaction, or state the real count. **A `next_due` advance without its matching run rows is not acceptable** | §7 — one statement, given in full |
| Unindexed fill columns are necessary but not sufficient for HOT; state `fillfactor`, partition layout, and a metric or evidence | §11, including the arithmetic that shows why `fillfactor` alone does not settle it, and the named alternative |
| `uuid` is sound only if every producer is converted and the boundary refuses non-UUID legacy ids, with a rolling-upgrade policy | §13 |

## 6. Schema

Three tables. `heartbeats` is not touched — §4a forbids it, and the reason is exactly right: absence
is the case the ledger exists to preserve.

Every table carries `project_id` and uses the tenant-safe composite foreign key this repository
already uses in four places (`FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id,
project_id) ON DELETE CASCADE` — migrations 00060, 00061, 00064, 00080). A single-column FK would
let a row outlive its tenant's isolation boundary.

### 6.1 `expected_runs` — one immutable row per due window

```sql
CREATE TABLE expected_runs (
    project_id         uuid        NOT NULL,
    monitor_id         uuid        NOT NULL,
    due_at             timestamptz NOT NULL,   -- the EXPECTED instant; the window's identity
    job_id             uuid,                   -- NULL: no job was ever issued for this window
    execution_revision bigint      NOT NULL,
    region             text        NOT NULL,
    issued_at          timestamptz,            -- core: dispatch returned success
    claimed_at         timestamptz,            -- executor: off the transport, about to probe
    terminal_at        timestamptz,            -- an ADMISSIBLE outcome exists; coverage iff NOT NULL
    outcome            text CHECK (outcome IN ('result', 'probe_error')),
    refused_at         timestamptz,            -- a result arrived and the revision/skew gate refused it
    refused_reason     text,                   -- ResultOutcome.Reason, verbatim
    skip_reason        text CHECK (skip_reason IN ('no_capable_runner', 'no_inflight_slot',
                                                   'credential_unresolved', 'no_capable_executor',
                                                   'transport_backoff')),
    PRIMARY KEY (monitor_id, due_at),
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE
) PARTITION BY RANGE (due_at) WITH (fillfactor = 70);

CREATE INDEX expected_runs_job_idx ON expected_runs (monitor_id, job_id)
    WHERE job_id IS NOT NULL;
```

**The window, not the job, is the identity.** `due_at` comes from the persisted `next_due_at`, which
advances monotonically, so two concurrently outstanding runs necessarily have **different**
`due_at` — which is what makes overlap representable and is the direct answer to [218]. A window
that was never issued is a row with `job_id IS NULL` and `issued_at IS NULL`, so §3.1's windows are
**stored rows**, not a read-time derivation.

`due_at` is the expectation and `issued_at` the actuality, so `issued_at - due_at` is **lateness** —
a fact nothing in cerbix records today, available here for free.

The partial index exists for exactly one query: a terminal event whose `due_at` is not trusted.
Normally the result carries both `due_at` and `job_id`, so the write is a primary-key hit and
`job_id` merely confirms the row belongs to the same run; the index is the fallback and stays out of
the hot path.

### 6.2 `monitor_schedule` — the durable expectation (one row per monitor)

```sql
CREATE TABLE monitor_schedule (
    project_id            uuid        NOT NULL,
    monitor_id            uuid        PRIMARY KEY,
    next_due_at           timestamptz NOT NULL,
    interval_in_force     int         NOT NULL,   -- seconds; produced next_due_at
    confirm_phase         boolean     NOT NULL DEFAULT false,
    execution_revision    bigint      NOT NULL,
    last_issued_at        timestamptz,
    schedule_created_at   timestamptz NOT NULL DEFAULT statement_timestamp(),
    gap_truncated_before  timestamptz,            -- windows before this were NOT materialized (§9.3)
    updated_at            timestamptz NOT NULL DEFAULT statement_timestamp(),
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE
);
```

`interval_in_force` and `confirm_phase` are on the row because the confirm phase substitutes
`m.ConfirmInterval()` for a run (`scheduler.go:1428-1434`); reading the monitor's current interval
would misdate every window probed under acceleration.

### 6.3 `monitor_execution_revisions` — the configuration timeline (§3.3)

```sql
CREATE TABLE monitor_execution_revisions (
    project_id               uuid   NOT NULL,
    monitor_id               uuid   NOT NULL,
    execution_revision       bigint NOT NULL,
    interval_seconds         int    NOT NULL,
    confirm_interval_seconds int    NOT NULL,
    timeout_seconds          int    NOT NULL,
    retries                  int    NOT NULL,
    effective_from           timestamptz NOT NULL,
    PRIMARY KEY (monitor_id, execution_revision),
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE
);
```

Written in the **same transaction** that bumps `monitors.execution_revision`. `UpdateMonitor`
already bumps that column on **any** write — a deliberately coarse fence documented in
`internal/domain/execsemantics.go` — so the volume is one row per monitor write, and rows are
identical-but-renumbered when a write touched nothing cadence-related. Accepted: deduplicating would
reintroduce the allowlist that file warns against.

This is what makes `execution_revision` mean anything historically. Today it is a bare counter:
nothing records what a generation *was*.

**Limitation, stated rather than discovered later.** Existing revisions cannot be reconstructed. The
migration backfills one row per monitor — current revision, current fields, `effective_from =
monitors.updated_at` — so the timeline begins at migration time and every window before it is
`unknown` by §12.3, never `covered`.

## 7. The issue statement — one transaction, rows and advance together

The reviewer's requirement is the section title: *a `next_due` advance without its matching run rows
is not acceptable*. It is stronger than it first reads. The advance **overwrites the only record of
the old expectation**, so if the gap rows are not written in the same statement, the evidence of
which windows were missed is destroyed by the very write that ends the gap. It can never be
recovered afterwards.

**There is exactly ONE statement that moves an expectation forward.** Revision 4 had two — the issue
path and the policy-skip path — and was rejected at party [229] because they diverged: §7.2 argued
that a skip needs no gap materialization "because rules 3 and 4 fire on a live leader", which is
false for precisely the case this requirement exists for. After a leader absence `next_due_at` is
already in the past, and if the returning leader's FIRST action for a monitor is a skip or a
backoff, revision 4 advanced past every intervening window while writing neither the windows nor
the fence. The gap became invisible to `ledger_from` again — the [225] defect, reintroduced by the
fix for [227], violating invariant 24a which revision 3 had itself added.

The lesson is structural, not local: **two statements sharing one obligation will diverge on it.**
So the gap-and-fence logic exists once, and every forward-moving caller passes parameters rather
than repeating the reasoning.

### 7.1 The primitive

Callers differ only in their arguments. `job_id` set means a dispatch succeeded; `skip_reason` set
means it did not happen and why. `next_due` and `interval_in_force` are passed **separately and
deliberately**: for a backoff they differ, and deriving one from the other is invariant 2b's defect.

```sql
-- $1 monitor_id[]  $2 job_id[]  $3 skip_reason[]  $4 next_due[]  $5 interval_in_force[]
-- $6 now  $7 cap  $8 retention_floor
WITH picked AS (
    SELECT s.monitor_id, s.project_id, s.next_due_at AS due_at, s.interval_in_force,
           s.execution_revision, m.region,
           v.job_id, v.skip_reason, v.next_due, v.new_interval,
           s.next_due_at + make_interval(secs => s.interval_in_force) AS first_expected
      FROM monitor_schedule s
      JOIN unnest($1::uuid[], $2::uuid[], $3::text[], $4::timestamptz[], $5::int[])
             AS v(monitor_id, job_id, skip_reason, next_due, new_interval)
        ON v.monitor_id = s.monitor_id
      JOIN monitors m ON m.id = s.monitor_id
     ORDER BY s.monitor_id
       FOR UPDATE OF s
),
-- Windows strictly between the old expectation and now, clipped at the retention floor because a
-- window older than that would be dropped unread. Numbered PER MONITOR, newest first, so the cap
-- keeps the most recent and the choice is deterministic.
candidate AS (
    SELECT p.monitor_id, p.project_id, p.execution_revision, p.region,
           p.first_expected, w.due_at,
           row_number() OVER (PARTITION BY p.monitor_id ORDER BY w.due_at DESC) AS rn
      FROM picked p
      CROSS JOIN LATERAL generate_series(
              GREATEST(p.first_expected, $8),
              $6 - interval '1 microsecond',
              make_interval(secs => p.interval_in_force)) AS w(due_at)
),
-- The window this action answers. ONE shape for both callers: issued rows carry a job and an
-- issued_at, skipped rows carry a reason and neither.
current_window AS (
    INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id, execution_revision,
                               region, issued_at, skip_reason)
    SELECT p.project_id, p.monitor_id, p.due_at, p.job_id, p.execution_revision, p.region,
           CASE WHEN p.job_id IS NOT NULL THEN $6 END,
           p.skip_reason
      FROM picked p
    ON CONFLICT (monitor_id, due_at) DO NOTHING
),
missed AS (
    INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id,
                               execution_revision, region)
    SELECT c.project_id, c.monitor_id, c.due_at, NULL, c.execution_revision, c.region
      FROM candidate c
     WHERE c.rn <= $7
    ON CONFLICT (monitor_id, due_at) DO NOTHING
),
-- ONE rule covers BOTH truncation causes: if the oldest window materialized is later than the
-- first window expected, something older was skipped — cap or clip, it does not matter which.
fence AS (
    SELECT c.monitor_id, min(c.due_at) AS truncated_before
      FROM candidate c
     WHERE c.rn <= $7
     GROUP BY c.monitor_id
    HAVING min(c.due_at) > min(c.first_expected)
)
UPDATE monitor_schedule s
   SET next_due_at          = p.next_due,
       interval_in_force    = p.new_interval,
       last_issued_at       = CASE WHEN p.job_id IS NOT NULL THEN $6 ELSE s.last_issued_at END,
       gap_truncated_before = GREATEST(s.gap_truncated_before, f.truncated_before),
       updated_at           = statement_timestamp()
  FROM picked p
  LEFT JOIN fence f ON f.monitor_id = p.monitor_id
 WHERE s.monitor_id = p.monitor_id;
```

**Windows, fence and advance commit together or not at all.** An advance that outruns its evidence
is impossible, not discouraged — and it is impossible for every caller, because there is only one
place it could happen.

Two things the primitive deliberately does not do. **`last_issued_at` is untouched by a skip**:
nothing was issued. And **`interval_in_force` never receives a backoff delay** — the caller passes
the monitor's real interval alongside a delayed `next_due`, so later gap windows stay spaced by the
interval rather than by a retry timer, an error that would otherwise be invisible and permanent.

`GREATEST` carries the fence's monotonicity and relies on a PostgreSQL-specific semantic worth
naming: it **ignores NULL arguments**, returning NULL only when all are NULL. A monitor with no
truncation this tick keeps the fence it had, a first truncation sets it, and a later one can only
move it forward. In standard SQL the NULL would propagate and erase the fence — exactly the failure
the statement exists to prevent — so the dependency is stated rather than assumed.

**The honest statement count**, since "batched" was rejected as an answer: **one** statement per tick
for the advance, the current window, the gap windows and the fence, whatever mix of issues and skips
that tick contains, plus the one credentialed materialization that already exists. It does not scale
with monitor count — `scheduler.go:637` sets `tick: time.Second`, so 1000 monitors means ~17 array
elements, not 17 statements. What scales per run is the claim event (§8.4) and the terminal fill,
and the terminal costs no round trip because it joins the transaction that already inserts the
heartbeat (`internal/store/monitors.go:1376`).

**Ordering against the publish.** For an issuing caller the statement runs **after** a successful
dispatch, matching today's in-memory rule. A crash after the publish and before the statement leaves
a run that happened unrecorded, and the arriving terminal event reconciles it (§8.3). A crash before
the publish records nothing, which is true. The residual error is therefore always toward
**withholding**, never toward claiming a run that did not happen — the only acceptable direction
here, and why dispatch does not move onto the transactional outbox to fix an error that already fails
safe.

### 7.2 Every advance, audited — FIVE rules, and which are callers

Revisions 1 to 3 asserted a "three-rule contract". The audit required at [227] found **five**. Every
write to `nextRun` in `internal/scheduler/scheduler.go`, classified, with what each does about the
ledger:

| # | Cause | In-memory advance | Sites | Ledger |
| --- | --- | --- | --- | --- |
| 1 | **Dispatch succeeded** | `now + iv` | `:1473` pull, `:1487` AMQP, `:1680` credentialed | §7.1 with `job_id` |
| 2 | **Dispatch failed** | *none* — retried next tick | `:1466-1471`, `:1480-1486` | Nothing: the expectation is unchanged, so there is nothing to preserve |
| 3 | **Policy skip** — no capable canary runner, no in-flight slot | `now + iv` | `:1442`, `:1449` plain; `:1644`, `:1648` credentialed | §7.1 with `skip_reason` |
| 4 | **Backoff** — credential unresolved, no capable executor, publish failure | `now + credentialFailureRetry(...)` | `:1576`, `:1604`, `:1631`, `:1674` | §7.1 with `skip_reason`, a delayed `next_due` and the UNCHANGED interval |
| 5 | **Confirm acceleration** — moves the due instant EARLIER | `fast` | `:1757` (`enterConfirm`) | §7.3 — never a caller |

Rules 1, 3 and 4 are the forward-moving callers and all three go through §7.1, on **both** the plain
and credentialed branches. FR-029 shipped an in-flight claim on one branch only, and that finding is
the same mistake this structure is arranged to make unrepeatable.

A sixth path, `checkStalePush` at `:1805`, writes `nextRun` for **push** monitors, which §15 excludes
from the ledger. Named so it is a stated exclusion rather than an unhandled path.

### 7.3 Confirm acceleration cannot lose a gap

Rule 5 is not a caller, and the reason is in the code rather than in an argument.
`enterConfirm` (`scheduler.go:1756-1757`) writes `nextRun[m.ID] = fast` only
`if due, ok := nextRun[m.ID]; !ok || due.After(fast)` — that is, only when the standing expectation
is **later** than the accelerated one. An expectation already in the past is therefore never moved,
so no intervening window can be skipped and there is nothing to materialize or fence.

What rule 5 must still do is set `interval_in_force = ConfirmInterval()` and `confirm_phase = true`,
and restore both when the acceleration expires (`:1428-1434` deletes the entry). An acceleration
that left `interval_in_force` at the base interval would misdate every window probed under
confirm — the exact misdating §6.2 puts that column on the row to prevent. This is a separate,
non-advancing statement, and invariant 2d requires that it never move `next_due_at` forward.

## 8. Idempotency, ordering and crash semantics

§4a named **AMQP claim ordering and crash semantics** as the key design risk to resolve before code.

### 8.1 One writer per column, and the merge rule stated once

Revision 1 asserted "never regresses a set value" in an invariant and then wrote SQL that overwrote
a set value with an earlier timestamp — the reviewer's second P0 at [218]. Both were wrong in
different directions, so revision 2 states one rule and shows the SQL that implements it:

> **A repeated event resolves to the EARLIEST observation of that event, deterministically. No event
> reads or writes another event's column.**

```sql
UPDATE expected_runs
   SET claimed_at = LEAST(COALESCE(claimed_at, $3), $3)
 WHERE monitor_id = $1 AND due_at = $2
   AND (job_id IS NULL OR job_id = $4);
```

`LEAST(COALESCE(...))` is the whole merge: idempotent, commutative, and monotone downward, so
replays and reorderings converge on the same value. "Earliest wins" rather than "first writer wins"
because the first *claim* is the real one; a duplicate delivery arriving later must not redate it.

### 8.2 Ordering is made irrelevant, not guaranteed

A terminal event that overtakes its own claim loses nothing: the claim fills its own column whenever
it lands. There is no state machine, so no reordering can violate one. A duplicate of either changes
nothing.

### 8.3 The terminal upsert, and why it cannot manufacture coverage

Revision 3 called this load-bearing and gave neither SQL nor validity rules — the reviewer's P1-2 at
[227]. Both are here, because the guard is one `WHERE` clause and every property below is a
consequence of it rather than a separate check.

**Where it runs is the first rule.** The fill lives in the SAME transaction as the heartbeat insert
and **behind the same gate**. `internal/store/monitors.go:1337-1344` rejects a result whose
`execution_revision` is missing (outside `observe` mode) or mismatched, and the comment is explicit:
*"A reject inserts nothing."* Steps 4 and 4b then reject a future timestamp, one outside retention,
and one that precedes its job's issue beyond `allowed_skew`. A refused result must therefore fill
**nothing** — otherwise a result refused as evidence for a heartbeat would become evidence for a
stroke. Making it structural rather than a remembered condition is the point: the statement is
unreachable for a refused result.

A refusal is still recorded, because "a result arrived and was inadmissible" is a different fact
from silence — in `refused_at` and `refused_reason`, which are **not** `terminal_at`. That keeps
coverage a single unambiguous test (`terminal_at IS NOT NULL`) instead of a compound condition a
later reader could get wrong.

```sql
INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id, execution_revision,
                           region, issued_at, claimed_at, terminal_at, outcome)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (monitor_id, due_at) DO UPDATE
   SET job_id      = COALESCE(expected_runs.job_id, $4),
       issued_at   = LEAST(COALESCE(expected_runs.issued_at,  $7), $7),
       claimed_at  = LEAST(COALESCE(expected_runs.claimed_at, $8), $8),
       terminal_at = LEAST(COALESCE(expected_runs.terminal_at, $9), $9),
       outcome     = COALESCE(expected_runs.outcome, $10)
 WHERE (expected_runs.job_id = $4
        OR (expected_runs.job_id IS NULL AND expected_runs.skip_reason IS NULL))
   AND expected_runs.execution_revision = $5;
```

What the guard proves, item by item against P1-2's list:

1. **`due_at` + `job_id` must match an issued row.** `expected_runs.job_id = $4` is the match. A
   terminal for run X reaching run Y's window updates nothing.
2. **An orphan terminal creates exactly its own row.** No conflict means the `INSERT` stands, with
   the `job_id`, `issued_at` and `due_at` the CORE minted at materialization and the executor copied
   (`dispatch.StampResult`) — never values the executor invented.
3. **A no-job window cannot be turned into coverage by another run's terminal.** For a window
   materialized by the gap logic, `job_id IS NULL`, so `job_id = $4` is NULL — not true — and the
   second disjunct admits it ONLY when `skip_reason IS NULL`. A **deliberately skipped** window
   (§7.1, a `skip_reason` row) can therefore never be adopted, which is the case that would
   otherwise convert "we chose
   not to run this" into "this ran".
   The remaining adoption is deliberate and narrow: a window the gap logic recorded as never-issued,
   for which a terminal later proves a run DID happen — §7's crash-after-publish. Adoption is the
   reconciliation, and it moves the verdict from `expected_never_issued` to `covered`, which is the
   truth.
4. **Duplicates and reordering converge.** `LEAST(COALESCE(...))` is idempotent, commutative and
   monotone downward, so a replay cannot redate an event and arrival order cannot change the result.
5. **A stale-revision result cannot become terminal evidence.** Twice: it never reaches the
   statement (the gate above), and `expected_runs.execution_revision = $5` would refuse it anyway if
   it did. The second condition is not redundant — it is what stops a result that was admissible for
   the monitor's *current* revision from filling a window materialized under a different one.

**The trust boundary, stated rather than assumed.** Adoption trusts that `due_at`, `job_id` and
`issued_at` came from the core. They did: all three are minted by the database at materialization
(`internal/store/materialize.go:84`) and copied verbatim by the executor. A forged triple is bounded
by the same revision fence and skew checks that already gate every heartbeat, and by the boundary in
§13 that refuses a non-UUID id. This is the same trust the product already extends to an executor
for the heartbeat itself; it is not new surface, and pretending the ledger could verify more than
the heartbeat path does would be a false assurance.

### 8.4 The claim event, and what it costs

The executor emits the claim **after** taking the job off the transport and **before** the first
attempt. It is best-effort: if the publish fails the probe still runs, so a missing claim means
"unwitnessed", never "did not happen", and **a terminal outcome always outranks a missing claim**.

`worker` and `agent` gain no database handle and no ack concept — the claim rides the existing result
path as a typed message, which is what §4a required. This is the one genuinely new per-run round
trip: 0.83/s at 50 monitors, 16.7/s at 1000.

**Folding the claim into the result was considered and rejected.** It would remove the round trip
entirely — the executor knows when it started probing — but the claim would then only ever arrive
attached to a result, and a run that crashed mid-probe produces no result. Invariant 6 would be
unobservable, which is most of the point.

### 8.5 The AMQP loss this makes visible for the first time

`internal/dispatch/amqp.go:191-194` states the policy plainly: *"a delivery is acked once handed to
the in-process channel. Losing a single check on a hard crash is acceptable — the scheduler re-emits
on the next interval."*

That trade is defensible and this design does not change it. What changes is that the loss stops
being invisible: a worker dying between ack and probe leaves `issued_at` set, `claimed_at` NULL,
`terminal_at` NULL — `issued_never_claimed`. The justification "the scheduler re-emits on the next
interval" becomes a queryable fact rather than an assurance.

### 8.6 Late events retract nothing

A window already written as unfinished is **never deleted** when its terminal event finally arrives;
`terminal_at` is filled and the row reads as *completed late*. Verdicts are computed, never stored
(§6.1), so one more non-NULL column changes the verdict with nothing to keep consistent.

## 9. Gap materialization

### 9.1 Where it happens

Inside §7's statement, in the same transaction as the advance. There is no separate backfill job,
because a separate job could not run at all in the case that matters — the leader was absent.

### 9.2 Fencing

Correctness does not depend on leader uniqueness, which is the only defensible position when the
leader is a Postgres advisory lock and a partitioned process may believe it still holds one:

- `FOR UPDATE` on the `monitor_schedule` rows serializes concurrent writers, in `monitor_id` order.
- The advance is **monotone**: a second writer reads the already-advanced `next_due_at` and its
  `generate_series` is therefore empty.
- Every insert is `ON CONFLICT (monitor_id, due_at) DO NOTHING`, so a window is materialized at most
  once regardless of how many writers try.

A zombie leader can therefore issue a duplicate probe — which it can today, and which is not this
requirement's problem — but it cannot corrupt or duplicate a window.

### 9.3 The bound, and the fence that must accompany it

A leader absent for a month against a 30-second monitor implies ~86,400 windows for that monitor
alone. Materializing them is neither useful nor free, so §7's statement bounds the work two ways:

- **A per-monitor cap** (`$5`), applied through `row_number() OVER (PARTITION BY monitor_id ORDER BY
  due_at DESC)`, so the most recent windows are kept and the choice is deterministic. It is per
  monitor, not per batch — the [225] P0 was precisely a batch-wide `LIMIT`.
- **A retention clip** (`$6`): the series never starts before the retention floor, because a window
  older than that would be dropped unread.

**Either bound obliges a fence, and the SQL — not the prose — sets it.** The rule is one condition:
if the oldest window actually materialized is later than the first window that was expected, then
something older was skipped, and that instant becomes `gap_truncated_before`. It does not matter
which bound caused it.

Before the fence, the span is treated exactly as pre-`ledger_from`: **not stored**, claimable as
nothing. No verdict is derived for it, so this is neither a rollup nor inference — it is an explicit
statement that the ledger cannot answer, which is the only honest thing to record about time whose
windows were never written.

The cap's value still has no analysis behind it (§18), and it is the last constant in this design
chosen by feel rather than by argument.

## 10. The configuration boundary

A window's spacing depends on the interval in force, and a configuration change can land while the
scheduler leader is absent — the API role is a different process. If a gap spanned a revision
change, §7's `generate_series` would space the whole gap at the pre-change interval.

**So no gap is allowed to span a revision change.** The transaction that bumps
`monitors.execution_revision` also, for that monitor:

1. materializes the windows from `next_due_at` up to the change instant, at the OLD
   `interval_in_force` (identical logic to §7's `missed` CTE, same cap);
2. writes the new `monitor_execution_revisions` row;
3. sets `monitor_schedule.interval_in_force`, `execution_revision` and `confirm_phase` from the new
   configuration — and **leaves `next_due_at` untouched.**

**`next_due_at` is not a function of the config write, and that is required, not incidental.**
Revision 3 said "and `next_due_at` from the new configuration" without defining the value, which the
reviewer flagged at [227] as P1-3: `now`, `now + new interval`, and an old due rescaled to the new
interval each move the probe instant differently, and **invariant 1 promises that no monitor's probe
instant changes.** The transition equation is therefore:

```
next_due_at_after_config_write = next_due_at_before_config_write        -- unchanged, always
interval_in_force_after        = new IntervalSeconds (or ConfirmInterval while confirm_phase)
```

This is also what the code does today: a config write does not touch the leader's in-memory
`nextRun`, so the pending probe fires when it was already going to, and the new interval governs from
the FOLLOWING advance. Any other equation would be a behaviour change smuggled in by a bookkeeping
requirement.

The window standing at `next_due_at` is stamped with the NEW revision, and that is correct rather
than a compromise: `next_due_at` is in the future, so that window occurs after the change and the new
configuration is the one in force for it.

**The race with the scheduler is settled by the lock, not by ordering.** Both this transaction and
§7's take `FOR UPDATE` on the same `monitor_schedule` row, so they serialize. If §7 commits first,
the config write's segment close sees the already-advanced `next_due_at` and materializes nothing.
If the config write commits first, §7 reads the new `interval_in_force`. Both orders leave the same
invariants true, which is why no ordering is prescribed.

**Test at each side of a due instant** (required by [227]): an interval change and a confirm-interval
change committed (a) just before and (b) just after a due instant, asserting in all four cases that
the pending probe fires at its original instant and that the first window spaced by the new interval
is the one after it.

The config write therefore CLOSES the open window segment, and every segment is spaced by exactly
the interval that was in force for it. This costs one more statement on a path taken once per
monitor edit, and it removes an approximation that would otherwise be permanent and invisible.

Enabling and disabling are the same boundary: disabling closes the segment and deletes the schedule
row, so a disabled monitor expects nothing; enabling creates it with `next_due_at = now` and
`ledger_from = now`.

## 11. HOT, `fillfactor` and the evidence for it

The reviewer's constraint, accepted: unindexed fill columns are **necessary but not sufficient** for
HOT, because HOT also needs free space in the same heap page, and without it daily partitions bound
eventual reclamation while saying nothing about write amplification inside the retained window.

**Layout.** `expected_runs` is `PARTITION BY RANGE (due_at)`, one partition per day, plus a DEFAULT
partition (§12.3). `fillfactor = 70` on every partition. `claimed_at`, `terminal_at` and `outcome`
appear in **no** index — invariant 12 — so an update to them is eligible for HOT.

**The arithmetic, which does not fully settle it, stated plainly rather than asserted away.** At
~128 bytes per tuple an 8 KB page holds ~60 tuples at `fillfactor = 100` and ~42 at 70, leaving
~18 tuples' worth of free space per page. Each row takes up to two updates, so a page whose rows all
update twice would need ~84 new versions against ~18 slots. **`fillfactor` alone is therefore not a
proof.** What closes the gap is HOT pruning: a dead version in a HOT chain is reclaimed on page
access without vacuum, so the requirement is not two spare versions per row simultaneously but
enough headroom for the versions live at one moment — which depends on arrival spread and access
rate, and cannot be computed from the schema.

**So it is measured, not assumed.** Invariant 23 makes the HOT ratio an acceptance gate:
`n_tup_hot_upd / n_tup_upd` from `pg_stat_user_tables`, per partition, exposed as a cerbix gauge.
No `pg_stat_user_tables` metric exists in this repository today — `pg_stat_activity` is read once,
in `internal/store/gatemaintenance.go:1153` — so this is new work and is named as such in phase D
rather than presented as following a precedent.

**The named alternative, specified so the decision is reversible.** If the measured ratio is poor,
the drop-in replacement is an append-only event table — `(monitor_id, due_at, kind, at, …)` with
`kind IN ('expected','claimed','terminal')`, folded at read time — which has no updates, no
`fillfactor` question and no vacuum churn, at the cost of ~3 narrower rows per window instead of one
wider row, and a fold on every read. It is a storage-layer swap behind the same read API, not a
redesign, and revision 2 does not adopt it because ~50 rows/s at 1000 monitors does not justify
paying the read-side complexity before a measurement says so.

## 12. Storage, capacity and retention

### 12.1 The model, term by term, so it can be checked

Every figure below is **computed, not measured**. The model is stated so the arithmetic can be
disputed instead of trusted:

| Term | Bytes | Note |
| --- | --- | --- |
| tuple header + null bitmap, aligned | 24 | |
| `project_id`, `monitor_id` | 32 | two `uuid` |
| `due_at`, `issued_at`, `claimed_at`, `terminal_at` | 32 | four `timestamptz` |
| `job_id` | 16 | `uuid`, not `text` — §13 |
| `execution_revision` | 8 | |
| `region` | 8 | short varlena (`core` is 4 chars) |
| `outcome` | 8 | short varlena, nullable |
| **heap tuple** | **~128** | aligned |
| PK `(monitor_id, due_at)` entry | ~40 | 24-byte key + index tuple and line-pointer overhead |
| partial `(monitor_id, job_id)` entry | ~44 | only rows with a job |
| **total per window** | **~212** | at `fillfactor = 100` |
| **total per window at `fillfactor = 70`** | **~267** | heap portion inflated by 1/0.7 |

### 12.2 Capacity

Runs per day is exact arithmetic; bytes carry the model's uncertainty.

| | windows/day | at 14 days | at 30 days |
| --- | --- | --- | --- |
| 50 monitors @ 60s (the owner's installation) | 72,000 | ~269 MB | ~577 MB |
| 1000 monitors @ 60s | 1,440,000 | ~5.4 GB | ~11.5 GB |
| `heartbeats` today at 1000 monitors, for comparison | same | ~2.5 GB | ~5.4 GB |

### 12.3 Retention bounds and what a dropped span may claim

`expected_run_retention_days`: **default 14, minimum 2, maximum 90.** Fourteen days covers a
fortnight of incident review, is where the stroke is actually wanted, and bounds 1000 monitors to
~5.4 GB. The `heartbeats` default is 30 (`internal/config/config.go:516`) and the gate ledger's is 90
with bounds 7–365 (`:529`, `:691`); this sits below both deliberately, because a window's evidentiary
value decays faster than a heartbeat's.

`expected_runs` takes a **DEFAULT partition**, following `heartbeats` — created in migration 00017
(`CREATE TABLE heartbeats_default PARTITION OF heartbeats DEFAULT`) and re-created by 00043's
declarative branch — and **not** the gate ledger, whose own tests assert there is no DEFAULT
partition anywhere in it. The reason is directional: a row here is evidence that a run did not
complete, and an insert lost to a missing partition would erase exactly the fact the ledger exists to
keep — silently, and toward over-claiming. Retention must therefore also purge rows out of the
DEFAULT partition, which the gate's mechanism never has to do.

`ledger_from` is the earliest instant a monitor can be answered for. It is **computed, never
stored** — a second defect found while fixing the P0 at [225]: revision 2 had it as a stored NOT NULL
column *and* as a formula, and nothing in the design updated the column when a partition was
dropped, so the two would have drifted apart in the direction of over-claiming. Storing it would
also have contradicted invariant 19.

```
ledger_from(monitor) = max(oldest retained expected_runs partition lower bound,
                           earliest effective_from in that monitor's revision timeline,
                           monitor_schedule.schedule_created_at,
                           monitor_schedule.gap_truncated_before)   -- NULLs ignored
```

The fence is therefore an INPUT to `ledger_from`, persisted by the same statement that advanced past
the windows it fences off, which is what makes the unanswerable span visible to every reader.

Before it, a surface may claim **nothing** — not `covered`, and not `expected_never_issued`. It
renders as **not stored**, an encoding FR-031 already has and already draws.

### 12.4 Where this design stops being right

At ~10,000 monitors on 60-second intervals — ~167 windows/s, on the order of 100 GB at 30 days — the
append-only variant of §11 and the participation limits of §5.3 both start earning their cost. That
is beyond both this installation and what cerbix is currently built for, and it is recorded so the
boundary is a known number rather than a surprise.

## 13. `job_id` as `uuid`, and the upgrade that makes it safe

`job_id` is `uuid`, not `text`: it saves ~42 bytes per window across the tuple and the partial index
(~17%), and the value is **already** a UUID — `gen_random_uuid()::text` merely stringifies it.

The reviewer's condition is accepted in full: every producer converted, the database boundary
refusing a non-UUID id, and a written rolling-upgrade policy. What that means concretely, stated
rather than hidden behind "convert every producer":

- **Three mint sites exist**, all in `internal/store/materialize.go` — `:84`, `:356`, `:377` — and
  all are already `gen_random_uuid()::text`. Converting them is dropping the `::text`.
- **Two dispatch paths mint nothing at all.** `scheduler.go:1456` (pull) and `:1476` (AMQP) publish
  `dispatch.CheckJob{Monitor: m}` with no `JobID`, no `IssuedAt` and no `ProtocolVersion`. So phase B
  does not convert a producer on those paths — it **creates** one. That is the larger half of the
  work and calling it a conversion would understate it.
- **The wire type stays `string`.** `dispatch.CheckJob.JobID` is JSON on a queue that may hold
  messages published by the previous version across a rolling upgrade, so the boundary parses and
  validates: a `job_id` that is absent or not a UUID means **this result correlates to no window**,
  and it is recorded as a heartbeat exactly as today and contributes nothing to the ledger. It is
  never coerced, never defaulted, and never silently dropped.
- **Rolling-upgrade policy.** During an upgrade, old executors return results with no `job_id`; those
  windows keep `terminal_at` NULL and read as `issued_never_claimed`, which would be a false
  accusation. So a window whose `execution_revision` predates the ledger's own activation instant —
  recorded per monitor as `ledger_from` — is `unknown`, not missing. The ledger begins claiming
  coverage only for windows issued after every executor in the region reports the new protocol
  version, which the region-worker liveness data already tracks.

## 14. What this unlocks, and the gate it must pass

FR-031 §6.2 draws points with no stroke because nothing could defend a line. A stroke between two
adjacent points becomes permissible only when **every** window between them is `covered` and the
whole span is at or after `ledger_from`. Any `unknown`, any `expected_never_issued`, any part of the
span before `ledger_from` — no stroke.

Nothing else is claimed. Coverage reporting and the reliability gate plausibly want these facts too,
and this document still does not claim that benefit, because nothing has been analysed.

## 15. Non-goals

- **No new role and no new deployment topology.** §4a settled this.
- **No ack concept at the `Dispatcher` seam.** `amqp.go:191-194` keeps its policy; the ledger
  observes the consequence rather than changing the transport contract.
- **Not a job queue.** `expected_runs` is evidence; nothing reads it to decide what to probe.
- **No change to any monitor's probe instant.** This is the property the owner selected the model
  for. An implementation that drifts a probe instant has failed the design, not deviated from it.
- **No stored verdicts.**
- **No retroactive backfill.** History before the migration is `unknown` (§6.3).
- **Push monitors are out of revision 2.** They are never dispatched (`scheduler.go:1415`) and
  `checkStalePush` already owns their staleness — but "a push that did not arrive" is an expected run
  in a different sense, and §18 keeps that exclusion open to challenge rather than closing it.

## 16. Phases

| Phase | Content | Gate |
| --- | --- | --- |
| **A** | `monitor_execution_revisions`; written in the revision-bump transaction; §10's segment close; backfill one row per monitor | `-race`; a revision bump with no timeline row fails a test |
| **B** | `monitor_schedule`; §7's statement; **job identity created on the two dispatch paths that mint none** and converted on the three that do (§13) | `-race`; a leader restart leaves a past `next_due_at`, and the gap rows exist |
| **C** | The claim event: `Dispatcher` grows a typed claim message; `worker` and `agent` emit it; §8.1's merge | `-race` + a live distributed stack; the fakes in `internal/api`, `internal/outbox` and `internal/scheduler` break on interface growth, which is intended |
| **D** | Partitions, DEFAULT partition, retention, `ledger_from`, `gap_truncated_before`, the read API returning computed verdicts, and the HOT-ratio gauge | `-race`; BOTH storage modes; E2E; the capacity measurement of invariant 22 |
| **E** | The FR-031 stroke, behind §14's gate — only if §17 is discharged | E2E on a live stack |

## 17. Acceptance invariants (FR-032)

Discharged as a SET in `docs/traceability.md`.

1. No monitor's probe instant changes as a result of this requirement.
2. The scheduler's advance rules are preserved exactly — **all five of them, enumerated in §7.2**.
   Revision 3 and earlier claimed there were three; the audit the reviewer demanded at [227] found
   five, and a rule discovered later must be added to §7.2 and to this invariant together.
2a. **There is exactly ONE statement that moves an expectation forward** (§7.1), and rules 1, 3 and
    4 are all callers of it. A second forward-moving path is the [229] defect by construction: two
    statements sharing the gap obligation will diverge on it, and did.
2d. Rule 5 never moves `next_due_at` forward, and its statement is incapable of doing so.
2b. A backoff delay never becomes `interval_in_force`. It moves `next_due_at` and nothing else.
2c. Confirm acceleration sets `interval_in_force` and `confirm_phase`, and restores both when the
    acceleration expires, so no window probed under confirm is misdated.
3. `next_due_at` survives leader loss, and a leader returning after a gap finds it in the past.
4. A window is `covered` only if a terminal outcome exists for it; a terminal outcome is never
   inferred from a heartbeat's presence at a nearby instant.
5. A run issued and never claimed is distinguishable from a window never issued.
6. A run claimed and never finished is distinguishable from both.
7. A repeated event resolves to the earliest observation of that event, and no event reads or writes
   another event's column.
8. A terminal event that arrives before its claim event loses nothing.
9. A duplicate claim or terminal event changes nothing.
10. A terminal outcome outranks a missing claim and a missing `issued_at`.
10a. A result the revision or timestamp gate REFUSES fills nothing: the terminal statement is
    unreachable for it. The refusal is recorded in `refused_at` and `refused_reason`, never in
    `terminal_at`, so coverage stays the single test `terminal_at IS NOT NULL`.
10b. A window carrying a `skip_reason` is never adopted by any terminal event: a run cerbix chose
    not to make can never be reported as one that happened.
11. `worker` and `agent` hold no database handle and no ack concept after this change.
12. `heartbeats` gains no column, and `claimed_at`, `terminal_at`, `outcome`, `refused_at`,
    `refused_reason` and `skip_reason` appear in no index.
13. The interval, timeout and retry count attributed to a window are those in force for THAT window,
    read from the revision timeline, never from the monitor's current fields.
14. No gap spans a revision change: the configuration write closes the open segment (§10).
14a. A configuration write leaves `next_due_at` UNCHANGED. The pending probe fires at the instant it
    would have fired anyway, and the new interval governs from the following advance — the only
    equation compatible with invariant 1.
15. Before `ledger_from` no verdict is emitted — neither `covered` nor `expected_never_issued`.
16. Dropping a partition never converts unproven time into proven time, and never invents a missed
    run.
16a. An `expected_runs` insert is never lost to a missing partition: the DEFAULT partition accepts
    it, and retention purges the default too.
17. Two runs outstanding at the same instant are two rows, and a terminal event reaches the right
    one.
18. A window written as unfinished is not deleted by a late terminal event; it reads as completed
    late.
19. No verdict is stored; every verdict is computed from the timestamps present.
20. A stroke on the Response time panel is permitted only for a span entirely `covered` and entirely
    at or after `ledger_from`.
21. Nothing in the ledger is read to decide what to probe.
22. The capacity model of §12.1 is verified by measurement against a populated table before phase D
    closes, and the retention default, minimum and maximum are configuration with enforced bounds.
23. The HOT ratio `n_tup_hot_upd / n_tup_upd` is exposed per partition and measured; a poor ratio
    selects §11's append-only variant rather than being absorbed silently.
24. A `next_due_at` advance never commits without the run row and the missed-window rows for the
    span it closes.
24a. When ANY window in that span is not materialized — by the per-monitor cap or by the retention
    clip — `gap_truncated_before` is written **in the same statement** as the advance, and it moves
    only forward. An advance that leaves an unfenced, unmaterialized span is the [225] P0 and must
    fail a test by name.
24b. The cap and the clip are deterministic PER MONITOR: a batch containing several monitors with
    gaps fences each one independently, and no monitor's windows are lost to another's volume.
24c. `ledger_from` is computed from persisted inputs, never stored, so dropping a partition cannot
    leave it stale in the over-claiming direction.
25. A result whose `job_id` is absent or not a UUID correlates to no window, is still recorded as a
    heartbeat, and never produces a false `issued_never_claimed`.
26. Every ledger row is reachable only within its tenant: the composite `(monitor_id, project_id)`
    foreign key, not a single-column one.

### 17.1 The test case invariants 24a and 24b are discharged by

Required by the reviewer at [225] before resubmission, so it is specified here rather than left to
implementation taste.

**Setup.** Two monitors in the same project, `interval_seconds = 60`, cap `$5 = 3`. Both
`monitor_schedule` rows carry `next_due_at` ten intervals in the past. One tick picks both.

**Assertions.**

1. Each monitor has exactly **3** missed rows, and they are ITS OWN most recent three — not three
   rows shared between them, and not monitor A's three plus none for B.
2. Each monitor's `gap_truncated_before` equals **its own** oldest materialized window.
3. Both `next_due_at` values advanced to `now + 60s`.
4. `ledger_from` for each monitor now sits at its own fence, so a query over the truncated span
   returns `unknown` and NOT `covered`.
5. The advance and the fence are in **one** transaction: killing the connection mid-statement
   leaves neither.

**Two further cases, required at [229], and they are the ones revision 4 failed.**

**(b) Leader gap, first action a POLICY SKIP.** One monitor, `interval 60s`, `next_due_at` ten
intervals in the past. The returning leader's first tick finds no capable canary runner. Assert: the
nine intervening windows exist with `job_id IS NULL` and no `skip_reason`; the tenth — the standing
expectation — exists with `skip_reason = 'no_capable_runner'` and no `job_id`; `next_due_at` is
`now + 60s`; and a query over the gap returns `expected_never_issued`, never `covered`.

**(c) Leader gap, first action a BACKOFF.** Same setup, but the first tick fails credential
resolution. Assert everything from (b) with `skip_reason = 'credential_unresolved'`, plus:
`next_due_at` is `now + credentialFailureRetry(...)` while **`interval_in_force` is still 60** — the
backoff must not become the interval, or every later gap window is misspaced.

In both, assert `last_issued_at` is unchanged: nothing was issued.

**The mutation that must fail (b) and (c)** is revision 4's structure itself: give the skip path its
own statement without the gap and fence CTEs. If the tests survive that, they have not reached the
mechanism — and this defect has now been introduced twice, once as [225] and once as [229], so a
test that cannot catch it is worth nothing here.

**The mutation that must fail (a).** Replace the per-monitor `row_number() … PARTITION BY monitor_id`
cap with a batch-wide `LIMIT $5`, exactly as revision 2 had it. The test must fail **by name**,
identifying the monitor whose windows vanished and the fence that was never written. A test that
passes against that mutation has not reached the mechanism and is worthless here — this is the
defect the reviewer found by reading SQL that my own prose contradicted, and a regression of it must
be caught by a test rather than by another review round.

## 18. Open items

- **Participation scope (§5.3) is unresolved** and is the owner's decision. Nothing else in this
  document depends on the answer except §12.2's totals.
- The exact claim-message shape on the wire, and whether it travels the results queue or its own.
  §4a fixes that it uses the existing transport; which queue is a phase-C detail.
- Whether a window should record the region's live-executor state at `due_at`, so a missed run in a
  region with no worker reads differently from one in a healthy region. Attractive, unanalysed,
  deliberately out of revision 2.
- Whether push monitors participate (§15). Excluding them is a decision worth challenging, not a
  fact.
- The `generate_series` cap value of §9.3 has no analysis behind it yet; it needs one, or a rule tied
  to retention rather than a constant.

## 19. Process

This revision goes to design review. Its own mock if any UI surface grows beyond FR-031's existing
panel, and a migration plan. Migrations start at `00100`.
