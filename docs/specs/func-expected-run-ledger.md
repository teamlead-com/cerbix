# Spec: The fact that a run was expected (func-expected-run-ledger)

> **Lifecycle: DESIGN APPROVED AT REVISION 22 — approved range `9f46f40..9e114a4`, D-0237.**
> **Document revision 26: AMENDMENT UNDER REVIEW.** The approval is the RANGE; the revision number
> tracks the DOCUMENT, and later revisions never extend the range. Each amendment gets its own
> disposition, and this line states the CURRENT one only — it accumulated four superseded clauses
> before revision 26 rewrote it whole, which is the same defect `6b47bad` fixed in the status cell.
>
> | Revision | Kind | Disposition |
> | --- | --- | --- |
> | 20, 21-22 | design | APPROVED, parties [267] and [274] — the range above, `D-0237` |
> | 23 | docs/process — §17.2 totals, §17.4 entry gate | APPROVED [350], `6b47bad` |
> | 24 | docs/process — §13.0's third transport, invariants 10k and 10l | APPROVED [364], `7e86d15` |
> | 25 | **design** — §13.0's rollback-and-drain rule | APPROVED [410], `6f698d5` |
> | 26 | **design** — §16.1's transport applicability, 10h's evidence | UNDER REVIEW, scoped at [415] |
>
> **PHASE A and B1-M IMPLEMENTED (`aa46db8`, `2fcf2b9`); THE REST NOT IMPLEMENTED.** The owner
> authorized implementation by phases, and the reviewer admits each phase separately against §17.2's
> entry gate. B1 was sequenced migration-first; its transport and config slice is specified and NOT
> admitted. Push, tag and release remain unauthorized. `docs/status.md` carries the live state.
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
> statements sharing an obligation diverge on it. **Revision 6** answers six P1s from [231] with no
> new P0 outstanding: one shared admissibility predicate for every event (§8.1), a half-open config
> range (§10), carrier-generation eligibility replacing an unimplementable activation instant (§13),
> the read API's authorization contract (§13a), a testable HOT threshold with per-partition
> `fillfactor` (§11), and a retention-derived gap bound (§9.3) — and was **REJECTED at [233] on a
> P0**: its carrier-generation eligibility named a CONSUMER-side datum as the source, so at
> publication time there was nothing to write and every row would have taken a column default.
> **Revision 7** sources the carrier from the PUBLISHER's routing decision, which already exists on
> all three transports, and threads it through §7.1 — but left TWO contradictions the schema had just
> created, rejected at [235]: the orphan INSERT omitted the carrier column its own CHECK now
> required, and `ProtocolV4` promised a `DueAt` that existed in no wire type. **Revision 8** fixes the
> INSERT and defines `DueAt` end to end (§13.1) — but its optimistic fence checked only
> `next_due_at`, the ONE datum §10 guarantees does not change, so a config write passed it while
> crossing the generation ([237]). **Revision 9** sourced every run fact from the published job and
> fenced on the revision too, but claimed the fenced-out job's result would show as `refused_at` —
> which no single-row-per-window schema can represent, rejected at [239]. **Revision 10** withdraws
> that claim, writes the refusal statement revision 6 had only invented columns for, and states the
> boundary: a refusal annotates only the row recording its OWN job, and never inserts. **Revision 11**
> closes the last P1 ([241]): an event's attributes now travel with its timestamp, so a reversed
> arrival cannot leave one delivery's instant beside another's reason — fixed in the terminal
> statement too, which had the same defect unreported. **Revision 12** records the owner's scope
> ruling (§5.3). **Revision 13** answers the last two P1s ([244]): the `ProtocolV4` carrier rollout
> is specified operationally and phase B splits so it is an independently deployable gate (§13.0,
> §16), and the read API's pagination is written to this repository's published keyset convention
> (§13a). **Revision 14** closes the phase-boundary gap revision 13's own split created ([246]): a
> gate defaulting off means no V4 job is ever published before the payload that defines V4 exists,
> and §16.1 adds the mixed-version matrix. §5.4 records the constraints from party [222].
>
> Nothing here is built. No requirement row moves, no migration exists, and the FR-031 panel keeps
> drawing points with no stroke until §14's gate is met by working code.
>
> **All FOUR owner decisions are now made** (all 2026-09-04): the expectation model (§5.1); scope and
> retention — **all monitors, 14 days** (§5.3); **push monitors excluded** (§15); and a run late by
> more than one interval reads **`covered_late`**, licensing no stroke and excluded from the coverage
> numerator (§14.1). No product semantics remain open.

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

### 5.3 Participation scope — the owner's ruling, 2026-09-04

**Every monitor participates, and the ledger's retention is 14 days.** Ruled by the owner on
2026-09-04 after being shown the cost at their own scale and at a stress point (§12.2): ~0.5 GB and
under two statements per second at ~50 monitors, ~5.4 GB at 1000. The reviewer declined to rule at
[222], correctly, because it is a product and operating-cost decision rather than reviewer
authority.

There is therefore **no participation flag**: no column, no migration for one, no UI, no MaC key and
no openapi change. Two alternatives were rejected in the process and are recorded so the decision
is not silently revisited — a per-monitor opt-in (zero default cost, but two different semantics in
one product), and automatic participation for SLI members of a service only (nothing to configure,
but a monitor's ledger would appear and disappear as someone else edited service membership).

The bound where this ruling would need revisiting is stated in §12.4 rather than left to be
rediscovered: around 10,000 monitors on 60-second intervals.

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
    carrier_generation int,                 -- the carrier the PUBLISHER selected; NULL when no job
    interval_seconds   int         NOT NULL,   -- the interval this window's lateness is judged against
    interval_assumed   boolean     NOT NULL DEFAULT false,  -- true only on an orphan row (§14.2)
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
    -- A window with no job has no carrier. `DEFAULT 0` was wrong twice over (reviewer P0 at [233]):
    -- it manufactured a value nobody set, and it made "never dispatched" indistinguishable from
    -- "dispatched on an old carrier". The two are different facts and the schema now says so.
    CONSTRAINT expected_runs_carrier_iff_job
        CHECK ((job_id IS NULL) = (carrier_generation IS NULL)),
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
-- $6 now  $7 cap  $8 retention_floor  $9 carrier_generation[]  (NULL where job_id is NULL)
-- $10 expected_due[]  $11 expected_revision[]  $12 region[]  — what the job was PUBLISHED with
WITH picked AS (
    SELECT s.monitor_id, s.project_id, s.next_due_at AS due_at, s.interval_in_force,
           -- The two sources are projected under DISTINCT names on purpose: a single `region` or
           -- `execution_revision` in scope is how a "comes from the job" rule silently becomes
           -- "comes from whatever the planner resolved".
           s.execution_revision AS schedule_revision, m.region AS monitor_region,
           v.expected_revision  AS job_revision,      v.region AS job_region,
           v.job_id, v.skip_reason, v.next_due, v.new_interval, v.carrier,
           s.next_due_at + make_interval(secs => s.interval_in_force) AS first_expected
      FROM monitor_schedule s
      JOIN unnest($1::uuid[], $2::uuid[], $3::text[], $4::timestamptz[], $5::int[], $9::int[],
                  $10::timestamptz[], $11::bigint[], $12::text[])
             AS v(monitor_id, job_id, skip_reason, next_due, new_interval, carrier,
                  expected_due, expected_revision, region)
        ON v.monitor_id = s.monitor_id
      JOIN monitors m ON m.id = s.monitor_id
     -- The optimistic fence (§13.2). TWO predicates, because the first one alone fenced the only
     -- datum that provably does NOT change: §10 leaves next_due_at untouched on purpose, so a
     -- config write between publish and here PASSED the old check while having changed the
     -- revision, the interval and possibly the region (reviewer P0 at [237]).
     -- `m.execution_revision` is deliberately the column the ingest gate reads at
     -- `internal/store/monitors.go:1342`, so this fence and the result-rejection rule cannot drift.
     WHERE s.next_due_at = v.expected_due
       AND m.execution_revision = v.expected_revision
     ORDER BY s.monitor_id
       FOR UPDATE OF s
),
-- Windows strictly between the old expectation and now, clipped at the retention floor because a
-- window older than that would be dropped unread. Numbered PER MONITOR, newest first, so the cap
-- keeps the most recent and the choice is deterministic.
candidate AS (
    SELECT p.monitor_id, p.project_id, p.schedule_revision, p.monitor_region,
           p.interval_in_force, p.first_expected, w.due_at,
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
                               region, issued_at, skip_reason, carrier_generation,
                               interval_seconds)
    -- Every RUN fact comes from the PUBLISHED job (§13.2): revision, region, carrier. Only the
    -- WINDOW facts come from the schedule. That is what makes a crossed generation impossible
    -- rather than merely detected.
    SELECT p.project_id, p.monitor_id, p.due_at, p.job_id, p.job_revision, p.job_region,
           CASE WHEN p.job_id IS NOT NULL THEN $6 END,
           p.skip_reason,
           p.carrier,                     -- NULL for a skip, by the CHECK in §6.1
           -- The interval that SPACED this window is the one in force BEFORE this advance,
           -- never `new_interval`: that one spaces the NEXT window (§14.1).
           p.interval_in_force
      FROM picked p
    ON CONFLICT (monitor_id, due_at) DO NOTHING
),
missed AS (
    INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id,
                               execution_revision, region, interval_seconds)
    -- A never-issued window has no published job, so its revision and region come from the
    -- schedule and the monitor. That is correct: it is attributed to the configuration that was
    -- in force while nothing ran, not to a run that never happened.
    SELECT c.project_id, c.monitor_id, c.due_at, NULL, c.schedule_revision, c.monitor_region,
           c.interval_in_force        -- the step `generate_series` used, so spacing is self-describing
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
> reads or writes another event's column. And when an event contributes MORE THAN ONE column, every
> one of its columns is chosen by the SAME comparison, so a row never mixes two deliveries'
> attributes.**

The second sentence was added in revision 11, after reviewer P1 at [241]: the refusal statement took
`refused_at = LEAST(...)` but `refused_reason = COALESCE(...)`, so a later refusal arriving first
followed by an earlier one left the EARLIER timestamp beside the LATER reason. The pair stopped
describing one event. The identical shape was in the terminal statement's `terminal_at`/`outcome` —
which [241] did not name and which is fixed here too, because fixing only the reported instance
would leave its twin.

The pattern to write is therefore: **the attribute is selected by a CASE over the OLD timestamp, and
the timestamp is minimised**. Both `SET` expressions in an `UPDATE` see the pre-update row in
PostgreSQL, so the CASE reads the old timestamp regardless of clause order — a property worth naming,
because a reader who assumed sequential assignment would think the order matters and "fix" it into a
bug. On an exact tie the existing attribute is kept, so replay is stable.

#### The admissibility predicate — written once, used by every event

Revision 5 had two event statements with two different guards, and the claim one was wrong: it
admitted `job_id IS NULL`, which is **also true of a deliberately skipped window**, and it carried no
revision predicate at all. So a late or stale claim could mark a window cerbix chose not to run as
claimed — invariants 7 and 10b, broken by the SQL that was supposed to uphold them (reviewer P1-1 at
[231]).

That is the [229] lesson in a second place: **two statements sharing an obligation diverge on it.**
So the boundary is defined once, here, and both event statements use it verbatim:

```sql
-- ADMISSIBLE(due_at, job_id, revision) — an event may touch a row only if:
      (expected_runs.job_id = :job_id                                  -- the same run, or
       OR (expected_runs.job_id IS NULL                                -- a window with no run,
           AND expected_runs.skip_reason IS NULL))                     -- but NOT a deliberate skip
  AND expected_runs.execution_revision = :revision                     -- and the same generation
```

The claim merge is then that predicate plus one column:

```sql
UPDATE expected_runs
   SET claimed_at = LEAST(COALESCE(claimed_at, $3), $3)
 WHERE monitor_id = $1 AND due_at = $2
   AND (job_id = $4 OR (job_id IS NULL AND skip_reason IS NULL))
   AND execution_revision = $5;
```

`LEAST(COALESCE(...))` is the whole merge: idempotent, commutative, and monotone downward, so
replays and reorderings converge on the same value. "Earliest wins" rather than "first writer wins"
because the first *claim* is the real one; a duplicate delivery arriving later must not redate it.

**Two negatives this must fail** (invariants 7a, 7b): a claim whose `execution_revision` does not
match the row changes nothing; and a claim naming the `due_at` of a window that carries a
`skip_reason` changes nothing, however late it arrives.

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

A refusal is still recorded when it belongs to a run the ledger records, because "a result arrived
and was inadmissible" is a different fact from silence — in `refused_at` and `refused_reason`, which
are **not** `terminal_at`. That keeps coverage a single unambiguous test (`terminal_at IS NOT NULL`)
instead of a compound condition a later reader could get wrong.

**The refusal statement, which revision 6 invented the columns for and never wrote** (reviewer P0 at
[239]). It runs on the REJECT path, inside the transaction `commitOutcome`
(`internal/store/monitors.go:1482`) is about to commit:

```sql
UPDATE expected_runs
   SET refused_reason = CASE WHEN refused_at IS NULL OR $3 < refused_at THEN $6
                             ELSE refused_reason END,
       refused_at     = LEAST(COALESCE(refused_at, $3), $3)
 WHERE monitor_id = $1 AND due_at = $2
   AND job_id = $4                        -- ONLY the row that records THIS job
   AND execution_revision = $5;
```

**It is an UPDATE and never an INSERT, and the asymmetry with §8.3 is deliberate.** A terminal proves
both that the run happened and that it produced an admissible outcome, so it may create its own row.
A refusal proves only that something was delivered; creating a row from it would invent an issued run
whose sole evidence is inadmissible, AND it would occupy the window's primary key against a
legitimate re-dispatch — the second horn of [239]'s trilemma. Zero rows affected is the normal
outcome for a refusal whose job the ledger never entered, and the caller treats it as such rather
than as an error.

Note the guard is `job_id = $4` alone, without §8.1's no-job disjunct: a refusal must never adopt a
window, because adoption asserts that a run happened there and a refusal is not evidence of an
admissible run.

```sql
INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id, execution_revision,
                           region, carrier_generation, interval_seconds, interval_assumed,
                           issued_at, claimed_at, terminal_at, outcome)
-- $8 is NOT a wire value: it is MIN(interval_seconds, confirm_interval_seconds) for $5's revision,
-- read from §6.3's timeline by the core, and $9 is TRUE. See §14.2.
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9, $10, $11, $12)
ON CONFLICT (monitor_id, due_at) DO UPDATE
   SET job_id             = COALESCE(expected_runs.job_id, $4),
       carrier_generation = COALESCE(expected_runs.carrier_generation, $7),
       issued_at          = LEAST(COALESCE(expected_runs.issued_at,  $9), $9),
       claimed_at         = LEAST(COALESCE(expected_runs.claimed_at, $10), $10),
       outcome            = CASE WHEN expected_runs.terminal_at IS NULL
                                       OR $11 < expected_runs.terminal_at THEN $12
                                  ELSE expected_runs.outcome END,
       terminal_at        = LEAST(COALESCE(expected_runs.terminal_at, $11), $11)
 WHERE (expected_runs.job_id = $4
        OR (expected_runs.job_id IS NULL AND expected_runs.skip_reason IS NULL))
   AND expected_runs.execution_revision = $5;
```

`$7` is `carrier_generation` and `$8` is the CONSERVATIVE threshold of §14.2; both are in the column
list rather than only in the prose because revision 7 put the carrier only in the prose (reviewer
P0-1 at [235]) and revision 15 did the same to the interval — found by grepping every
`INSERT INTO expected_runs` against its column list rather than by a third rejection. §6.1's CHECK requires the column
non-NULL whenever `job_id` is non-NULL, so an orphan insert omitting it **fails the constraint**: the
reconciliation path would have errored on exactly the case it exists to serve. The `DO UPDATE` uses
`COALESCE(existing, new)` so **adoption fills the carrier on a no-job window** while a row that
already has one keeps it — the same first-writer-wins shape as `job_id`, since neither is an
observation that can arrive twice with different values.

**Conflict behaviour, stated because "upsert" is not a specification:** a conflict on a row that
fails the `WHERE` leaves the row untouched and reports **zero rows affected**, which the caller
treats as *not correlated* and never as an error. A conflict on a row that passes fills only the
columns above. A missing conflict inserts. All three outcomes are normal.

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
4. **Duplicates and reordering converge, attributes included.** The timestamp is minimised and
   `outcome` is chosen by a CASE over the OLD `terminal_at`, so the pair always describes the same
   delivery whichever order they arrive in (§8.1). A replay cannot redate an event, and arrival
   order cannot change either column.
5. **A stale-revision result cannot become terminal evidence.** Twice: it never reaches the
   statement (the gate above), and `expected_runs.execution_revision = $5` would refuse it anyway if
   it did. The second condition is not redundant — it is what stops a result that was admissible for
   the monitor's *current* revision from filling a window materialized under a different one.

**The orphan insert supplies `carrier_generation` too**, and from the one source trustworthy on that
side: `dispatch.DeliveredJob.CarrierGeneration`, which the **transport adapter** sets from the queue
or claimed row the job actually came from, and which `dispatch.go:41-47` keeps deliberately separate
from the payload's `ProtocolVersion`. So the issue path takes the publisher's selection and the
orphan path the adapter's observation — two server-owned sources for one fact, and the payload is
authoritative for neither. No row is ever left at a column default (reviewer P0 at [233]).

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

- **A retention clip**, which is the PRIMARY bound and is derived rather than chosen: the series
  never starts before the retention floor, because a window older than that would be dropped
  unread. The implied worst case is therefore computable —
  `expected_run_retention_days x 86400 / interval_seconds` windows per monitor, which at the
  14-day default and a 30-second monitor is 40,320 rows for a monitor that was unprobed for the
  entire retention window.
- **A per-monitor safety cap**, `expected_run_gap_windows_max`: **default 10,000, minimum 100,
  maximum 100,000**, applied through `row_number() OVER (PARTITION BY monitor_id ORDER BY due_at
  DESC)` so the most recent windows are kept and the choice is deterministic. It is per monitor, not
  per batch — the [225] P0 was precisely a batch-wide `LIMIT`. It exists to bound one statement's
  work, not to express a policy, which is why the retention clip is the rule and this is a valve.

Revisions 2 through 5 carried this as a bare constant with, in my own words, "no analysis behind it".
[231] refused that, correctly: a bound with no rule behind it is a number someone will change without
knowing what it protects.

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

1. materializes the windows in the **half-open range `(next_due_at, change_instant)`** at the OLD
   `interval_in_force` — that is, `generate_series(next_due_at + interval, change_instant -
   1 microsecond, interval)`, the same lower bound §7.1 calls `first_expected`. **The standing
   `next_due_at` row is EXCLUDED**, and that exclusion is load-bearing: revision 5 said "from
   `next_due_at`" inclusively while also saying the standing window takes the new revision, and
   when `next_due_at` is already past during a leader absence the config write would have created
   that row itself and §7.1's `current_window` insert would then collide with it (reviewer P1-2 at
   [231]). The standing expectation is written by §7.1 when it is finally acted on, by exactly one
   writer.
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

**What `execution_revision` on a row MEANS, stated because the apparent contradiction at [231] came
from leaving it implicit:** it is the configuration the run **executed under**, not the one that
computed the window's due instant. Those differ for a window whose due instant was computed under
the old interval but which is acted on after a change — including every window standing in the past
during a leader absence. The run genuinely executes under the new configuration, so recording the new
revision is correct, and it is not in tension with step 1 recording the old interval for **spacing**:

| Question | Answered by |
| --- | --- |
| How were these windows spaced? | `interval_in_force` at materialization — the old interval for a segment closed by a config write |
| Under what configuration did the run happen? | `execution_revision` on the row — the generation live when it was acted on |

Invariant 13 is about the second and §10 about the first; conflating them is what made revision 5
read as self-contradictory.

**The race with the scheduler is settled by the lock, not by ordering.** Both this transaction and
§7's take `FOR UPDATE` on the same `monitor_schedule` row, so they serialize. If §7 commits first,
the config write's segment close sees the already-advanced `next_due_at` and materializes nothing.
If the config write commits first, §7 reads the new `interval_in_force`. Both orders leave the same
invariants true, which is why no ordering is prescribed.

**Test at each side of a due instant** (required by [227]): an interval change and a confirm-interval
change committed (a) just before and (b) just after a due instant, asserting in all four cases that
the pending probe fires at its original instant and that the first window spaced by the new interval
is the one after it.

**Test with `next_due_at` ALREADY PAST** (required by [231]): a config write committed while the
monitor's expectation sits ten intervals in the past. Assert that the config write materializes the
nine windows **after** `next_due_at` and not the one **at** it; that §7.1's later
`current_window` insert for that instant succeeds rather than conflicting; and that exactly one row
exists for it, carrying the new `execution_revision`.

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

**So it is measured, not assumed — and "poor" is not a threshold, which [231] was right to refuse.**

| Datum | Value |
| --- | --- |
| Metric | `cerbix_expected_runs_hot_update_ratio`, a single gauge **aggregated over the current retention window**, with NO partition label — partition names are unbounded over time and would be exactly the high-cardinality mistake |
| Supporting counters | `cerbix_expected_runs_updates_total`, `cerbix_expected_runs_hot_updates_total` — monotonic, unlabelled |
| Source | `pg_stat_user_tables`, summed across the retained partitions |
| Sample window | The gate is evaluated only once at least **1,000** updates have accumulated; below that the sample says nothing |
| Threshold | ratio **>= 0.90** passes; below it, §11's append-only variant is selected rather than absorbed |
| Zero denominator | `n_tup_upd = 0` means the ratio is UNDEFINED: the gauge is **not published** and the gate neither passes nor fails. A gauge reporting 0 or 1 for "no data" is a lie in whichever direction happens to be convenient |

No `pg_stat_user_tables` metric exists in this repository today — `pg_stat_activity` is read once,
in `internal/store/gatemaintenance.go:1153` — so this is new work, named as such in phase D rather
than presented as following a precedent.

**`fillfactor` must be set on every PARTITION, not on the parent.** In PostgreSQL, storage
parameters are per-relation and `CREATE TABLE … PARTITION OF` does **not** inherit them, so the
`WITH (fillfactor = 70)` in §6.1's parent DDL would have applied to nothing that ever holds a row —
a defect [231] caught in the DDL itself. Every partition is therefore created
`WITH (fillfactor = 70)` by the partition-maintenance code, alongside `EnsureHeartbeatPartitions`,
and invariant 23a requires a test that reads `pg_class.reloptions` for a **newly created** partition
rather than trusting the parent.

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
| `interval_seconds` | 8 | `int` plus alignment; carried so lateness needs no join (§14.1) |
| `interval_assumed` | 0 | `boolean` is 1 byte and is absorbed by the alignment padding already counted above, so the total does not move. Stated rather than omitted, because §12.1 claims to be checkable term by term and a silently dropped term is how a model stops being that |
| **heap tuple** | **~136** | aligned |
| PK `(monitor_id, due_at)` entry | ~40 | 24-byte key + index tuple and line-pointer overhead |
| partial `(monitor_id, job_id)` entry | ~44 | only rows with a job |
| **total per window** | **~220** | at `fillfactor = 100` |
| **total per window at `fillfactor = 70`** | **~278** | heap portion inflated by 1/0.7 |

### 12.2 Capacity

Runs per day is exact arithmetic; bytes carry the model's uncertainty.

| | windows/day | at 14 days | at 30 days |
| --- | --- | --- | --- |
| 50 monitors @ 60s (the owner's installation) | 72,000 | ~280 MB | ~600 MB |
| 1000 monitors @ 60s | 1,440,000 | ~5.6 GB | ~12.0 GB |
| `heartbeats` today at 1000 monitors, for comparison | same | ~2.5 GB | ~5.4 GB |

### 12.3 Retention bounds and what a dropped span may claim

`expected_run_retention_days`: **14 by the owner's ruling of 2026-09-04** (§5.3), with minimum 2 and
maximum 90 as the enforced bounds the reviewer required at [222]. Fourteen days covers a fortnight
of incident review, is where the stroke is actually wanted, and bounds 1000 monitors to ~5.4 GB. The `heartbeats` default is 30 (`internal/config/config.go:516`) and the gate ledger's is 90
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
- **Rolling-upgrade policy — rebuilt, because revision 5's was not implementable.** It said a window
  whose `execution_revision` "predates the ledger's activation instant, recorded per monitor as
  `ledger_from`" is `unknown`. Three things wrong, all correctly named at [231]: `ledger_from` is
  computed from partitions, revisions, schedule and fence and is **not** an activation instant; an
  integer revision cannot predate an instant at all; and the existing liveness sources answer
  whether **some** capable consumer exists, never whether **every** regional executor is job-id
  aware — `LiveCredentialV3JobRegions` and `LiveCanaryJobRegions` are queue-consumption questions by
  design, and their own comment says a consumer on the ordinary queue is no evidence about the
  special one.

  **So the ledger does not census executors. It uses the isolation this project already uses for
  exactly this problem:** a new protocol gets its own carrier, and only capable executors consume it
  (FR-020 / D-0160, `carrierGeneration[region]` at `scheduler.go:1514`). Eligibility becomes a
  property of the ROW rather than a global instant, so no census, no activation timestamp and no
  `ledger_from` involvement is required.

  **Revision 6 then named the wrong source, which was a P0 at [233].** It said the row is stamped
  from `dispatch.DeliveredJob.CarrierGeneration`. That value exists on the **consumer** side: at
  publication time — which is when §7.1 runs — no `DeliveredJob` exists yet, and `worker`/`agent`
  hold no database handle and cannot fill it in later. Every row would have taken the column's
  `DEFAULT 0` and read `unknown` forever, or something would have had to trust the payload and break
  the boundary the design leans on. Invariant 10c had no implementation path.

  **The authoritative datum is the PUBLISHER's routing decision, and it already exists on all three
  transports:**

  | Transport | What selects the carrier | Server-owned? |
  | --- | --- | --- |
  | **pull** | which of `EnqueuePullJob` / `V2` / `V3` the leader called; the row's `protocol_version` is, in that file's words, "the row's carrier generation, **stamped by the server**" (`internal/store/pulljobs.go:67`, and `handlers_agent.go:219` calls it "the SERVER's stamp") | yes, and already persisted |
  | **AMQP** | which queue the leader published to, decided by `carrierGeneration[region]` (`scheduler.go:1514`, `:1529`, `:1552`) before the publish | yes |
  | **inproc** | the `ProtocolVersion` the core itself put on the job (`inproc.go:34`) | yes — see the caveat below |

  So `PublishJob` and the pull enqueue **return the carrier they used**, and the leader passes it into
  §7.1 as `$9` for every issued row. It is never recovered from a payload and never inferred.

  **The ledger minimum is its own generation, not a reused one.** `ProtocolV3` means "carries
  envelope v2" (`internal/dispatch/credentials.go:16-21`) — a credential capability. Reusing it
  would make a non-credentialed monitor's ledger eligibility depend on a credential carrier, which
  is a different question. Phase B introduces **`ProtocolV4` = "carries `JobID`, `IssuedAt` and
  `DueAt` on every dispatch path"**, and `LedgerMinCarrier = dispatch.ProtocolV4`. One generation per
  capability is this project's own pattern and the reason the generations are legible at all.

### 13.0 The `ProtocolV4` carrier rollout

Revision 11 named `ProtocolV4` and specified nothing operational about it — reviewer P1-1 at [244].
The code physically supports **1..3 only**, and every one of those surfaces has to grow:

| Surface | Today | V4 |
| --- | --- | --- |
| AMQP queues | three prefixes: `checks.jobs.`, `checks.jobs.v2.`, `checks.jobs.v3.` (`amqp.go:29-35`), mapped by `jobsQueueForGeneration` (`:152`) | a fourth prefix `checks.jobs.v4.<region>` and its mapping entry |
| Pull rows | `pull_jobs_protocol_version_check CHECK (protocol_version IN (1,2,3))`, and the same on `pull_tests` (00062, widened by 00063) | a migration **widening** both to `IN (1,2,3,4)` — 00063's own comment says why: *"CHECK is widened rather than dropped: an unknown generation must still be rejected at the boundary"* |
| Pull claim | `ClaimPullJobs`, `…V2`, `…V3`, each leasing "every generation at or below" its own (`pulljobs.go:77-97`) | `ClaimPullJobsV4`, and an `agentJobsV4` endpoint beside `agentJobsV3` (`handlers_agent.go:119`) |
| Scheduler admission | `carrierGeneration[region]` raised to 3 only for regions that ANNOUNCE the capability (`scheduler.go:1514`, `:1529`, `:1552`) | the same shape for 4, over a `LiveLedgerV4JobRegions`-style source |
| Enqueue | `EnqueuePullJob`/`V2`/`V3` | `EnqueuePullJobV4` |
| **In-process (`role=all`)** | no queue and no row: `CarrierGeneration` **is** the job's own `ProtocolVersion` (`inproc.go:34`, and `cli.go:255`, whose comment says "no wire between the materializer and this runner") | nothing to grow, and that is the hazard — the generation is whatever the producer stamped, so this transport's inertness is the FLAG, never isolation |

**`role=all` is a PRODUCTION topology** (`docker/docker-compose.prod.yml` line 1: "single-process
(--role all)"), not a development convenience, and revision 23 omitted it from this table entirely.
That omission has already cost this project twice on V3, and `scheduler.go` records both in its own
comments at the resolve-time admission site:

- **Raised when it should not have been.** `role=all` with `pull.regions: [core]` raised core to
  generation 3 "on the strength of a runner that will never see the job"; a capability-1 agent could
  not claim the v3 row, "and the monitor had no outcome at all until the row's TTL". The fix is the
  `if s.pullRegions[region] { continue }` exclusion — an in-process executor is **no evidence** about
  the agent that will actually claim.
- **Not raised when it should have been.** Without the converse rule — "a same-process executor IS
  this binary, so its capability is ours by construction" — "the default single-binary `role=all`
  never moved past generation 2, which meant the execution binding and field-set rules — the whole
  point of the amendment — were inert in the most common deployment while the worker inside the same
  process could open them perfectly well."

V4 needs both rules, at the same place: **resolve time**, "so neither builder order nor configuration
can restore it" (reviewer [102]). A ledger that is inert under `role=all` would be inert for most
installations, which is the V3 failure repeated against a requirement whose whole subject is knowing
that a run was expected.

**Isolation is physical, not a filter, and that distinction is the acceptance test — on the two
transports that HAVE a wire.** In-process has none: producer and consumer are the same binary, so
there is no version skew to discover and no queue to be unsubscribed from. Its guarantee is real but
different in kind, and it must not be borrowed to weaken the other two: "the generation is a field in
the payload" is true of `inproc` alone, and reasoning from it to AMQP or pull is how a filter gets
substituted for isolation. An old worker
subscribes only to the prefixes it was compiled to know, so it **cannot receive** a V4 delivery — it
is not subscribed, rather than subscribed-and-filtered. An old agent calls the V3 claim endpoint,
whose query leases generations ≤ 3, so a generation-4 row is **outside its result set** rather than
excluded after the fact. Invariant 10g requires the test to prove unreachability on both transports:
publish V4 with only a V3 consumer attached and assert the job is never delivered; claim through
`ClaimPullJobsV3` against a generation-4 row and assert it is never returned.

**Why V4 cannot reuse the credential capability**, which [244] named and is worth writing down: V3
means "carries envelope v2" and is asked only of monitors that have secrets. Job identity applies to
**every** monitor, so gating it on a credential capability would make an ordinary HTTP monitor's
ledger eligibility depend on a capability its dispatch never needs.

**Upgrade order, and the gate that makes it deploy-safe.** Revision 13 said the scheduler may select
V4 once a region announces it, while §16 put `JobID`/`IssuedAt`/`DueAt` in the NEXT phase — so a
B1-only deployment could have emitted V4 jobs before the payload that DEFINES V4 existed, or a V4
consumer could not have insisted on its defining fields. Reviewer P1 at [246], and it is a
contradiction my own phase split created.

The resolution is the conservative one of the two offered: **nothing is ever published on V4 until
its payload exists.** That is a stronger guarantee than deploying the payload alongside, which would
depend on a deploy order being respected.

`ledger.carrier_enabled` gates selection, defaulting **false** — the same shape as
`WithCredentialEnvelopes` (`scheduler.go:570`, `:603`) and `resultRevisionMode`
(`internal/store/store.go:197`), which is how the credential carrier was staged.

**A default is not a contract, and the difference is what a B1 binary does when an operator supplies
`true`** (reviewer P0 at [423]). Defaulting false says nothing about that input, and both plausible
readings are wrong: accepting it lets B1 select and publish V4 before `DueAt` exists, violating 10h;
coercing it to false silently is the self-healing that `AGENTS.md` forbids — "config is strictly
validated before business logic starts" and "runtime paths contain no self-healing or fallback for
invalid configuration". So the contract is versioned and explicit:

| Phase | `ledger.carrier_enabled` absent or `false` | `ledger.carrier_enabled: true` |
| --- | --- | --- |
| **B1** | accepted; the carrier is deployed and inert | **REFUSED at startup.** `(*Config).Validate` (`internal/config/config.go:552`) returns an error naming the key and saying that the V4 payload — `DueAt` and the identity it carries — arrives only in B2, so the carrier cannot be selected yet. The process exits before any business logic, exactly as an empty `server.listen` does today |
| **B2** | accepted; selection stays off | accepted; the SAME validated snapshot reaches selection, wired in the atomic change that adds the payload and the ledger together |

**Ownership is named so it cannot drift into a role-local branch or an environment read.** The value
is a field of the config struct, validated by `(*Config).Validate`, and reaches the leader through
the scheduler construction path — `scheduler.New(...)` at `internal/cli/cli.go:1067` (`case "all"`)
and `:1100` (`case "scheduler"`), the only two roles that build one. Not `os.Getenv`, not a check
inside `lead()`, and not a different answer per role: a gate that each role decides for itself is a
gate that a mixed deployment disagrees about, which is the failure §16.1 exists to describe.

1. **B1 — the transport, inert. IMPLEMENTED.** Queues, the widened CHECK, `ClaimPullJobsV4`, `agentJobsV4`,
   capability announcement, and `carrierGeneration`'s ability to reach 4 — all deployed, with the
   gate **off**, so selection never happens. Executors may announce V4 and sit on an empty queue.
   Provable end to end by direct tests against a live stack **without a single V4 job existing**.
2. **B2 — payload and ledger, atomically.** `DueAt`/`JobID` minting, `monitor_schedule`,
   `expected_runs`, §7.1's primitive, and the gate **on**. From the first V4 job ever published, V4
   means exactly what §13.0 says it means.
3. **Executors before selection, always.** Even with the gate on, `carrierGeneration[region]` rises
   to 4 only for a region that announces it, so a job is never published to a queue nobody consumes.

**A V4 consumer may therefore insist on its defining fields**, which it could not have done under
revision 13's order: a V4 delivery missing `JobID`, `IssuedAt` or `DueAt` is a protocol violation,
not a rolling-upgrade case, and is dead-lettered rather than probed. An older carrier missing them is
the ordinary case and is simply not ledger-eligible (§13, invariant 10c).

**Rollback and drain.** On rollback the scheduler stops selecting 4 and immediately resumes
publishing at the region's previous generation. In-flight V4 work then reaches a terminal state
through paths that already exist — and there are **two V4-capable pull surfaces with two paths
each**, which every revision through 24 collapsed into one job-shaped sentence (reviewer [387]):

| Surface | Terminal paths |
| --- | --- |
| `pull_jobs` | `AckPullJobs` deletes the row once the agent has reported; `PurgeExpiredPullJobs` removes it past its TTL |
| `pull_tests` | `GetPullTestResult` deletes the row as its caller CONSUMES the result; `PurgeExpiredPullTests` removes it past its TTL |

**TTL is the bounded fallback for unconsumed work, not the drain.** The earlier wording said "`pull_jobs`
rows TTL-expire" as though expiry were the mechanism, then generalized it to all "in-flight V4 work"
while naming only one surface. Both halves mislead: the ordinary path for a job is the agent
reporting and the row being acked, and for a test it is the caller consuming a result that has
already been written — expiry is what BOUNDS the wait when neither happens. On `pull_tests` that
distinction has teeth, because a row whose probe has finished holds an answer nobody has read yet, so
treating expiry as the drain describes discarding it as routine.

**Phase boundary.** This rule becomes live in **B2**, the first phase that can produce a V4 row at
all: B1 selects nothing, so a B1-era rollback drains nothing and this paragraph is vacuous there.
What protects B1 is a different property — invariant 10j's migration guard refuses to narrow the
`CHECK` while a generation-4 row exists, which is boundary safety rather than drain. Conflating the
two is how a rollback story gets written for a phase that cannot exercise it.

The paragraph's SET of surfaces and terminal paths is guarded: `check_fr032_drain_surfaces` derives
both from the tree — every table whose **effective CHECK admits generation 4**, read from each
migration's Up half, and every store function that DELETEs from one — and fails if this paragraph
omits any of them. `TRUNCATE` is scanned as well, with a single allowlisted exclusion: the exact
`(*Store).TruncateAll` test helper in `internal/store/store.go`. Any other truncate of an effective
V4 surface is reported, because it is either a terminal path this paragraph must describe or
operational code discarding V4 work — and "the scan pattern happens not to match it" is not a
decision (reviewer [392]). Carrying a `protocol_version` constraint is not the same as being able to hold a
V4 row: a surface capped at 1..3 is deliberately NOT demanded of this paragraph, and a pair of
fixtures pins the distinction in both directions (reviewer [390]). A drain rule that names one
surface is the defect this guard exists to prevent recurring.

Windows dispatched below `LedgerMinCarrier` read `unknown` — the honest verdict, and the reason a
rollback costs truth rather than correctness.

### 13.1 `DueAt` on the wire — the field revision 7 promised and never defined

`ProtocolV4` was declared to carry `DueAt` while no such field existed on `CheckJob`, `Heartbeat` or
`StampResult`, and nothing in the spec added one. §8.3 needs it for its primary-key hit, so the
generation had no wire path for the value that identifies the window — reviewer P0-2 at [235]. The
alternative the reviewer offered, a pure job-id lookup, does not close it: the **orphan** case has no
row to look up and must CREATE one, and creating it needs the window's identity. Guessing `due_at`
from `issued_at` would invent a window or collide with a real one. So the field is defined.

| Concern | Specification |
| --- | --- |
| Job field | `dispatch.CheckJob.DueAt time.Time`, `json:"due_at,omitempty"`, beside `JobID` and `IssuedAt` |
| Result field | `domain.Heartbeat.DueAt time.Time`, `json:"due_at,omitempty"`, copied by `dispatch.StampResult` alongside `JobID` and `JobIssuedAt` — the ONE owner of that copy, as its comment requires |
| ~~Spacing fields~~ | **REMOVED in revision 19.** Revisions 16–18 carried `EffectiveIntervalSeconds` on the job and the result so the orphan insert could fill `interval_seconds`. The reviewer's question at [258]/[260] — whether the base-versus-confirm residual is compatible with a truthful coverage claim — is answered NO, so the field, its ingress validation and the residual are all gone. §14.2 has the replacement, which needs no wire value at all |
| Not a table column | Like `JobID`, `JobIssuedAt` and `ExecutionRevision` (`internal/domain/monitor.go:618-629`), it is wire-only. §4a forbids overloading the `heartbeats` TABLE, which this does not touch |
| Mint owner | The **core**, from `monitor_schedule.next_due_at`, read by the leader's own batched read in the same tick — NOT from the 15-second snapshot (`refreshEvery`, `scheduler.go:236`), which would be stale by design |
| Validation | `due_at <= issued_at` (an expectation cannot postdate its own dispatch), and `due_at` inside the retention window. A violation refuses CORRELATION — the result is still recorded as a heartbeat — exactly as a non-UUID `job_id` is treated (§13) |
| Propagation | All three transports carry `CheckJob` verbatim: AMQP as the JSON body, pull as the JSON payload of a `pull_jobs` row, inproc as the struct itself. A new field therefore propagates by construction, and this is stated rather than assumed because it is the reason no per-transport work is needed |
| Rolling upgrade | An old executor drops the unknown field, so the result returns with `DueAt` zero. That is treated as **no correlation** — never as `due_at = epoch` — and the window stays `unknown`, which is precisely what `LedgerMinCarrier` gates |

### 13.3 Who owns the effective interval

**ONE function, because it is currently computed twice.** `iv := m.Interval()` followed by a possible
`iv = m.ConfirmInterval()` appears at `scheduler.go:1427` (plain path) and again at `:1635`
(credentialed path). Two sites computing one fact is the divergence this design has been bitten by
three times, so the effective interval gets a single owner — one helper both call, in the shape
`dispatch.StampResult` already uses for job identity ("one owner, because three executors publish
results and a stamp each applied separately would drift the first time one was edited"). The
credentialed path builds its job inside `internal/store/materialize.go`, so the value is **passed
in** rather than recomputed there; the store never guesses it.

It reaches `expected_runs` only through §7.1, from the leader's own state. **It is never carried on
the wire and never read from a result** — see §14.2 for why that changed in revision 19.

### 13.2 The fence, and why one predicate was the wrong one

The leader must know `due_at` **before** publishing, while §7.1 writes **after**, so something can
change in between. Revision 8 fenced on `s.next_due_at = expected_due` alone — and that is the one
datum §10 and invariant 14a **guarantee does not change**, because leaving it alone is what keeps
invariant 1 true. A configuration write between publish and write therefore PASSED the fence while
having changed `execution_revision`, `interval_in_force`, `confirm_phase` and possibly the monitor's
region. §7.1 then wrote the old job with the NEW revision; the result, carrying the old one, is
rejected at `internal/store/monitors.go:1342`; and the ledger claimed an issued run of a generation
that never ran. That was reviewer P0 at [237], and it is the exact crossing the fence existed to
prevent.

**Two answers, because there are two problems.**

**(a) Source every run fact from the published job.** The row's `execution_revision`, `region` and
`carrier_generation` now come from the job's own parameters, never re-read from the schedule. A
crossed generation becomes impossible by construction rather than detected after the fact. A
never-issued window has no job, so its revision and region come from the schedule — correct, because
it is attributed to the configuration in force while nothing ran.

**(b) Fence on the revision as well.** Sourcing from the job fixes what the row SAYS, but the
**advance** is still computed by the caller from the interval it read, so a stale interval would set
a wrong `next_due_at`. The fence therefore adds `m.execution_revision = v.expected_revision`, and
one predicate covers every monitor datum because `UpdateMonitor` bumps the revision on **any** write
— the deliberately coarse fence documented in `internal/domain/execsemantics.go`. Region and carrier
selection are covered transitively by it.

The column compared is `monitors.execution_revision`, deliberately the same one the ingest gate reads
at `monitors.go:1342`, so the ledger's fence and the result-rejection rule cannot drift apart.

**On mismatch nothing happens at all**: no advance, no window, no fence write. The monitor stays due
and the next tick republishes at the current revision.

**What becomes of the already-published job — and revision 9 claimed something impossible here.** It
said the old result "shows as `refused_at` on whichever window it correlates to". Reviewer P0 at
[239] showed that cannot be represented: the next tick materializes the rev6 job at the SAME
`(monitor_id, D)` primary key, so writing the rev5 refusal onto that row would attribute a rev5
rejection to the rev6 run, while inserting a rev5 row first would block the rev6 `current_window` by
primary key. One row cannot carry both, and I had promised a test that the schema makes impossible.

**The premise was wrong, not the schema.** A dispatch the ledger never entered has nothing in the
ledger to update, and it should not: the window's coverage story is about the run that ANSWERED it,
which is the rev6 run. The rev5 dispatch is a discarded attempt, and it is already observable where
discarded results are counted — `cerbix_result_ignored_total`,
`cerbix_result_missing_revision_total`, `cerbix_result_clock_skew_total`,
`cerbix_result_observed_before_issue_total`. The ledger adds nothing by duplicating that, and adding
it would require exactly the misattribution [239] identified.

So the rule is a boundary, not a mechanism: **a refusal can only ever annotate the row that records
its own job.** §8.1's admissibility predicate already enforces it — a rev5 refusal cannot match a
rev6 row, because the predicate compares `execution_revision`. The mechanism was right and the prose
was wrong, which is the sixth time in this design and the reason I now read the SQL against the
prose before sending rather than after.

**A stale-revision job can also never create a blocking orphan**, and that is worth stating because
it is the horn of [239]'s trilemma that looks most dangerous. The terminal path (§8.3) is reachable
only for an ADMISSIBLE result, and `monitors.go:1337-1344` refuses a stale revision before any
insert. So a fenced-out rev5 job cannot insert a row at `(monitor_id, D)` and cannot stand in the way
of the rev6 materialization.

I am doing (a) as well as the (b) you asked for, because a fence that detects a crossing is weaker
than a structure that cannot produce one, and (b) alone would have left the row's own facts sourced
from a place that can disagree with the job.

  **The one honest caveat.** For `inproc` the carrier comes from `job.ProtocolVersion`, which IS the
  payload — but publisher and consumer are the same process and the payload is the core's own, so
  there is no boundary to cross and nothing to forge. Writing "never read from the payload" without
  this exception would be false for one of three adapters, and a reader would find it.

## 13a. The read API — authorization is not the foreign key's job

Phase D promised a read API and gave no contract. The composite foreign key of §6 protects **storage
integrity**: it stops a row outliving its tenant. It says nothing about who may READ a row, and
[231] was right to separate the two.

**Route**, following the convention already in `openapi.yaml`
(`/api/v1/projects/{projectID}/monitors`):

```
GET /api/v1/projects/{projectID}/monitors/{monitorID}/expected-runs
      ?from=<RFC3339>&to=<RFC3339>&limit=<1..200>&cursor=<opaque>
```

**Authorization.** Mounted behind the session-auth middleware like every project route — never on
`PublicRouter`, never on `AgentRouter`. Project membership is checked exactly as
`internal/api/handlers_monitors.go` does it, and a monitor outside the caller's tenancy is **404
hidden**, not 403, matching that file's stated contract at `:15` ("writes 404 (hidden) or 403"). A
`projectID`/`monitorID` pair that does not belong together is also 404: the pair is validated, not
just each half.

**The project predicate is mandatory in the repository, not only in the handler.** Every ledger query
carries `AND project_id = $n` in its own SQL. A handler check alone is one refactor away from being
bypassed, and a query that is safe only because of its caller is not safe. Invariant 26a requires a
**cross-project negative test** that calls the store layer directly with a mismatched pair and gets
nothing back.

**Response.** Windows in `due_at` order with computed verdicts (never stored — invariant 19), plus
the two facts that bound what the answer means: `ledger_from` and `gap_truncated_before`. **Each
window also carries `interval_assumed`**, because a `covered_late` produced by §14.2's conservative
threshold may be an artefact of the assumption rather than real lateness, and a caller that cannot
tell the two apart has been handed a verdict without its confidence. This was missing until revision
20: the flag existed in the table and appeared in no response, which is a fact stored and never
read — the mirror image of the defects in §5.2. A range
extending before `ledger_from` returns its windows as `unknown` and says so in the payload rather
than silently starting later, so a caller cannot mistake a clipped range for a covered one.

**Pagination**, specified to the convention this repository already publishes for the gate decision
ledger (`openapi.yaml:1470-1495`) rather than left as "opaque cursor" — reviewer P1-2 at [244], and
it caught that my `limit=<1..1000>` contradicted the established bound:

| Element | Contract |
| --- | --- |
| Range | half-open `[from, to)`, both **REQUIRED**, `from < to`, at most the retention window (14 days) |
| Order | `due_at DESC`. **No tiebreak is needed and the reason is structural**: the route is monitor-scoped and the primary key is `(monitor_id, due_at)`, so `due_at` is unique within one monitor. If this route is ever widened to project scope a `monitor_id` tiebreak becomes mandatory — noted here because that change would otherwise silently start dropping rows |
| Cursor | opaque to the client, `base64url("v1:" + due_at as RFC3339Nano)`. The version prefix exists so a later change is DETECTABLE rather than misread |
| Comparison | the keyset of the LAST RETURNED item; the next page is bound **strictly below** it, so a key returned once is never returned again |
| `next_cursor` | **null on the last page** |
| Invalid cursor | 400 `cursor_invalid` — strict decode, no tolerance: a bad version prefix, bad base64 or an unparseable instant all fail the same way |
| Cursor outside the range | 400 `cursor_invalid`. A cursor whose `due_at` falls outside the requested `[from, to)` cannot belong to this traversal, and ignoring it would return a page from a different query than the caller asked for |
| `limit` | minimum 1, **maximum 200, default 50** — the same bounds as every other paged endpoint here; 400 `limit_invalid` for 0, negative, non-integer or above 200 |
| Empty page | `[]` with a null `next_cursor`. Never 404: an empty answer is a fact about the range, not a missing resource |
| Other errors | 400 `range_required` \| `range_invalid` \| `range_too_wide`; 404 for a project or monitor not visible (§13a's 404-hidden rule) |
| Traversal | LIVE, as the gate ledger's is: rows committed or dropped during a traversal may or may not appear, and each item's presence follows the single-window response |

One deliberate divergence from that precedent, stated so it does not read as an oversight: the gate
ledger is **project-scoped and never service-nested** because it *outlives* services. This ledger
does **not** outlive its monitor — `expected_runs` cascades on monitor deletion (§6.1) — so
monitor-nesting is correct here, and the two conventions differ for a reason.

**What `interval_assumed` on the response does and does not touch** (reviewer [265]):

- **The typed surface changes, and the repo has a rule for it.** The field is published in
  `openapi.yaml`, so `frontend/src/api/schema.d.ts` must be regenerated by `npm run gen:api` and
  committed — `CLAUDE.md` requires it after ANY `openapi.yaml` change, and `tests.yml` fails the
  build when the generated file is stale. Phase D carries that, not a later cleanup.
- **The cursor is untouched.** The keyset is `due_at` alone (§13a), a field of the WINDOW rather than
  of the item's shape, so adding a response field changes no cursor payload, no comparison and no
  page boundary. Stated because it was asked, not because it was in doubt.
- **It may never promote a verdict, and this is the part worth guarding.** `interval_assumed` is
  **explanatory only**. It exists so a caller can see that a `covered_late` may be an artefact of
  §14.2's conservative threshold. It must never be read as licence to treat such a window as
  `covered`: not by the stroke gate, not by a coverage numerator, not by a future surface arguing the
  lateness is "probably not real". Handing an implementer a flag that softens a truthfulness verdict
  is how the gate would be widened without anyone deciding to widen it, so invariant 20g forbids it
  outright rather than trusting judgement.

## 14. What this unlocks, and the gate it must pass

### 14.2 The threshold for a window the core did not record

The orphan path (§8.3) creates a row for a run the ledger never entered at issue, so the core does
not know which interval spaced that window. Revisions 16–18 solved this by carrying the value on the
wire and validating it against the revision's two legitimate values.

**The reviewer asked whether the base-versus-confirm residual is compatible with a truthful coverage
claim ([258], [260]). It is not, and my own defence of it was wrong on this design's own terms.** I
had argued the residual was bounded in magnitude. But every other trade here is chosen so the
residual error points at **withholding** (§7.5), and this one points the other way: a window spaced
at a 10-second confirm interval and answered 40 seconds late is `covered_late` in truth, while an
executor claiming the 60-second base interval turns it into `covered` — which **licenses a stroke**
and **counts in the coverage numerator**. That is over-claiming decided by an executor's choice. A
direction violation is not excused by a small magnitude.

**So the wire field is removed and the threshold is computed by the core, conservatively:**

```
interval_assumed = false  →  threshold is interval_seconds (the interval that SPACED the window)
interval_assumed = true   →  threshold is MIN(interval_seconds, confirm_interval_seconds)
                             for that row's execution_revision, from §6.3's timeline
```

`expected_runs` gains `interval_assumed boolean NOT NULL DEFAULT false`, set true **only** by the
orphan insert. The tightest of the two legitimate thresholds means an unrecorded window can be
judged `covered_late` when the truth might have been `covered` — an error toward withholding, which
is the only direction this design accepts.

What this buys beyond correctness: a wire field, its JSON tags, its `StampResult` clause, its
two-value ingress validation and an executor trust residual all **disappear**, in exchange for one
boolean. `interval_seconds` also keeps ONE meaning — the interval this window's lateness is judged
against — with `interval_assumed` saying whether it was observed or assumed, rather than the column
quietly meaning two different things on two paths.

### 14.1 `covered_late` — the owner's ruling of 2026-09-04

`due_at` is the expectation a run ANSWERS, not the instant it was picked, so `issued_at - due_at` is
**lateness**. The alternative — `due_at` as the pick instant — was not put to the owner as an equal
option because it cannot represent the requirement at all: a window nothing picked would have no
`due_at` to be keyed by, which collapses the very distinction FR-032 exists for.

**But `covered` alone would have overstated the facts, and this is a defect I found in my own design
while recommending it.** A leader absent from 10:01 to 10:42 leaves the 10:01 expectation answered by
a probe that observed the target at **10:42**. Calling that window `covered` lets a coverage number
count 10:01 as proven by an observation forty-one minutes away — precisely the over-claim FR-031
exists to prevent.

So the verdict is split, and the owner ruled for the split:

| Condition | Verdict |
| --- | --- |
| `terminal_at IS NOT NULL` and `issued_at - due_at <= interval_seconds` | `covered` |
| `terminal_at IS NOT NULL` and `issued_at - due_at > interval_seconds` | **`covered_late`** |

`covered_late` **licenses no stroke** (§14's gate demands `covered`) and is **excluded from the
coverage numerator**. It is not a failure — a run happened and produced an admissible outcome — it is
a statement that the observation is too far from the window to prove it.

**The threshold uses `expected_runs.interval_seconds`, which is why §6.1 carries that column.** The
monitor's current interval is forbidden by invariant 13, and the revision timeline gives a revision's
base and confirm intervals without saying which was in force for THIS window — a window probed under
confirm acceleration would be judged against the wrong one. The interval that spaced the window is a
property of the window, so the row states it and lateness needs no join.


FR-031 §6.2 draws points with no stroke because nothing could defend a line. A stroke between two
adjacent points becomes permissible only when **every** window between them is `covered` — plain
`covered`, never `covered_late` (§14.1) — and the whole span is at or after `ledger_from`. Any
`unknown`, any `expected_never_issued`, any `covered_late`, any part of the span before
`ledger_from` — no stroke.

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
- **Push monitors are excluded — the owner's ruling of 2026-09-04**, and the reason is the lesson
  this design learned seven times: *"a push that did not arrive"* is already **detected** and already
  **recorded as a fact**. `scheduler.go:1415` never dispatches them and `checkStalePush`
  (`:1780-1805`) owns their staleness, synthesising a DOWN that lands as a heartbeat. A ledger window
  for push would be a **second mechanism for one obligation**, and two mechanisms sharing an
  obligation diverge on it — [225], [229] and [241] are all that defect. If push coverage is wanted
  later it should be a requirement that **REPLACES** `checkStalePush`'s synthesis, not one that sits
  beside it.

## 16. Phases

| Phase | Content | Gate |
| --- | --- | --- |
| **A** | `monitor_execution_revisions`, written in the revision-bump transaction; backfill one row per monitor. **§10's segment close is NOT here** — it materializes windows and `expected_runs` does not exist until B2, which now owns it (§17.2) | `-race`; invariant 13a: a revision bump with no timeline row fails, the backfill writes exactly one row per monitor, and each row carries that revision's four cadence fields |
| **B1** | **The `ProtocolV4` carrier, INERT** (§13.0): the fourth AMQP prefix, the widened pull CHECK, `ClaimPullJobsV4`, `agentJobsV4`, capability announcement, and `carrierGeneration`'s ability to reach 4 — with `ledger.carrier_enabled` **false**, so selection never happens and **no V4 job is ever published**. Independently deployable, trivially revertible, and writes no ledger rows | `-race` + a live distributed stack. A V3 consumer must be PHYSICALLY unable to receive a V4 job; `ClaimPullJobsV3` must never return a generation-4 row; and with the gate off, **no V4 job is published even when a region announces V4** |
| **B2** | Payload and ledger, together and only together: `DueAt`/`JobID` minting on **every** dispatch path (created on the two that mint none, converted on the three that do — §13), `monitor_schedule`, `expected_runs`, §7.1's primitive, **§10's segment close**, and the gate **on** | `-race`; a leader restart leaves a past `next_due_at` with its gap rows; and the §16.1 mixed-version matrix |
| **C** | The claim event: `Dispatcher` grows a typed claim message; `worker` and `agent` emit it; §8.1's merge | `-race` + a live distributed stack; the fakes in `internal/api`, `internal/outbox` and `internal/scheduler` break on interface growth, which is intended |
| **D** | Partitions, DEFAULT partition, retention, `ledger_from`, `gap_truncated_before`, the read API returning computed verdicts, and the HOT-ratio gauge | `-race`; BOTH storage modes; E2E; the capacity measurement of invariant 22 |
| **E** | The FR-031 stroke, behind §14's gate — only if §17 is discharged | E2E on a live stack |

### 16.1 The mixed-version matrix

Required at [246], because a phase boundary that is only described is a phase boundary that will be
crossed in the wrong order by someone.

| Core | Executor | Must hold |
| --- | --- | --- |
| B1 | B1 | Gate off. Every job on carrier ≤3, no V4 published, no ledger tables read or written, nothing degraded |
| **B1** | **B2** | The executor announces V4 and sits on an empty queue. **The gate still forbids selection**, so no V4 job is published — announcement alone cannot promote a region |
| **B2** | **B1** | The executor does not announce V4, so `carrierGeneration` stays ≤3. Jobs still flow; their windows read `unknown` rather than being lost. The ledger degrades in truth, never in delivery |
| B2 | B2 | V4 selected, identity rides every path, windows materialize and become eligible |

The second and third rows are the ones that matter: each is a half-deployed cluster, which is the
normal state during a rollout rather than an exceptional one.

**Those two rows are not transport-general, and every revision through 25 left that unsaid** — the
same defect §13.0 had one section earlier (reviewer [415], confirming a finding raised at [413]). A
half-deployed cluster requires a WIRE between core and executor. Where there is none the state
cannot arise at all, and the safety argument is a different one:

| Transport | Half-deploy rows 2 and 3 | What makes V4 safe there |
| --- | --- | --- |
| AMQP | apply | announcement plus the gate: a region is promoted only when its workers announce V4 AND `ledger.carrier_enabled` is on, and an old worker is not subscribed to the v4 prefix |
| pull | apply | announcement plus the gate: the region is promoted only when its agents announce V4 and `ledger.carrier_enabled` is on. An agent then claims through its own generation's endpoint, whose predicate is `protocol_version <= its own`, so a generation-4 row is outside its result set |
| in-process (pure `role=all`) | **impossible** | the FLAG alone. Core and executor are one binary, so there is no version skew to discover and no announcement to withhold — `scheduler.go:1546-1552` raises the generation because "a same-process executor IS this binary". Invariant **10k** is the proof: with `ledger.carrier_enabled` false, `lead()`'s resolved map stamps nothing above 3 |
| `role=all` with `pull.regions` | apply, for those regions only | announcement plus the gate, exactly as pull: the region is promoted only when its AGENTS announce V4 and `ledger.carrier_enabled` is on. The local executor does not count toward it — `scheduler.go:1542` skips pull-served regions in that branch (`if s.pullRegions[region] { continue }`) because an in-process runner "is no evidence" about the agent that will claim the row |

**Reading row 3 in-process would be a category error.** It says the executor withholds its V4
announcement, so `carrierGeneration` stays ≤3 — but an in-process executor makes no announcement to
withhold. Stated generally, that row promises a protection which does not exist in the deployment
shape that is most common, which is exactly how §13.0's drain sentence went wrong.

`check_fr032_transport_matrix` guards the table above: the four contexts as a SET, each with an
explicit applicability verdict, and the in-process row obliged to cite both the flag and 10k.

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
7c. When an event contributes more than one column, all of them are chosen by the same comparison:
    the timestamp is minimised and each attribute is selected by a CASE over the OLD timestamp. A row
    never carries one delivery's timestamp beside another's attribute, in either arrival order.
7a. **Every** event statement uses the ONE admissibility predicate of §8.1 verbatim: the same run,
    or a window with no run AND no `skip_reason`, and the same `execution_revision`. A statement
    with its own variant of the boundary is the [231] P1-1 defect by construction.
7b. A claim naming the `due_at` of a window that carries a `skip_reason` changes nothing, however
    late it arrives; and a claim whose `execution_revision` does not match the row changes nothing.
8. A terminal event that arrives before its claim event loses nothing.
9. A duplicate claim or terminal event changes nothing.
10. A terminal outcome outranks a missing claim and a missing `issued_at`.
10a. A result the revision or timestamp gate REFUSES fills no terminal column: the terminal
    statement is unreachable for it. A refusal is recorded in `refused_at`/`refused_reason`, never
    in `terminal_at`, so coverage stays the single test `terminal_at IS NOT NULL`.
10f. The refusal statement is an UPDATE and never an INSERT, and matches only on `job_id` — never
    on the no-job disjunct. A refusal for a job the ledger did not enter changes nothing, and a
    stale-revision refusal can never annotate a newer run's row.
10b. A window carrying a `skip_reason` is never adopted by any terminal event: a run cerbix chose
    not to make can never be reported as one that happened.
10c. A window dispatched on a carrier below `LedgerMinCarrier` reads `unknown`, never
    `issued_never_claimed`.
10h. No V4 job is ever published before its defining payload exists: `ledger.carrier_enabled` gates
    selection and B2 turns it on in the same change that mints `JobID`/`IssuedAt`/`DueAt`. A region
    announcing V4 cannot promote itself while the gate is off. **Each safety claim cites the
    mechanism that carries it**: on AMQP and pull, announcement AND the gate, with a half-deployed
    cluster possible and covered by §16.1's rows 2 and 3; in-process, the gate ALONE, because there
    is no announcement to withhold and no version skew to discover — proven by 10k, not by §16.1's
    half-deploy rows, which cannot occur there.
10i. A V4 delivery missing `JobID`, `IssuedAt` or `DueAt` is a protocol violation and is
    dead-lettered, not probed. The same absence on an older carrier is ordinary and merely makes the
    window ledger-ineligible.
10g. Carrier isolation is PHYSICAL: an executor of an older generation cannot receive a V4 AMQP
    delivery because it is not subscribed to that queue, and cannot claim a generation-4 pull row
    because its endpoint's query does not select one. Proven by unreachability on both transports,
    not by a capability lookup returning false.
10d. `carrier_generation` is written at INSERT time from a server-owned source and never left to a
    column default: the PUBLISHER's routing decision on the issue path, the transport ADAPTER's
    observation on the orphan path. A forged payload `ProtocolVersion` cannot promote a row on any
    transport except `inproc`, where publisher and consumer are one process and there is no boundary
    to cross.
10e. `carrier_generation IS NULL` exactly when `job_id IS NULL`, enforced by a CHECK: a window never
    dispatched has no carrier, which is a different fact from an old one.
11. `worker` and `agent` hold no database handle: neither package imports a store package, before
    or after this change. **And the `dispatch.Dispatcher` INTERFACE gains no ack method** — which is
    what §4a and `amqp.go:191-194` actually require ("this keeps the transport seam free of an ack
    concept the Dispatcher interface does not expose").
11a. The unqualified claim "no ack concept" was FALSE about the tree and is withdrawn. The pull
    agent has had a lease-ack since it existed: `internal/agent/agent.go:290` acks only the jobs it
    processed, and `:329` buffers without acking so the leases lapse server-side and the jobs are
    re-delivered. That mechanism is pre-existing, load-bearing for the pull transport, and out of
    this requirement's scope. Found by trying to specify invariant 11's test — the second time
    §17.2's ENTRY gate caught an invariant asserting something the tree contradicts.
12. `heartbeats` gains no column, and `claimed_at`, `terminal_at`, `outcome`, `refused_at`,
    `refused_reason` and `skip_reason` appear in no index.
13. The interval, timeout and retry count attributed to a window are those in force for THAT window,
    read from the revision timeline, never from the monitor's current fields.
13a. The revision timeline is COMPLETE and self-describing: **no site can create a GENERATION**
    without writing the matching `monitor_execution_revisions` row in the SAME transaction. A
    generation is created by bumping the fence — four sites, not one — **and by CREATING a monitor**,
    which initializes generation 1 and never touches the fence. The first draft said "bump", and
    creation is not a bump: reviewer P0 at [304] found a freshly created monitor's initial
    generation indescribable until its first update (§17.3); the migration's backfill writes exactly one row per monitor; and every row
    carries that revision's `interval_seconds`, `confirm_interval_seconds`, `timeout_seconds` and
    `retries`. Nothing else in this list states the integrity of the table everything else reads —
    which is why phase A had a gate in §16 and no invariant of its own until revision 23.
14. No gap spans a revision change: the configuration write closes the open segment (§10).
14a. A configuration write leaves `next_due_at` UNCHANGED. The pending probe fires at the instant it
    would have fired anyway, and the new interval governs from the following advance — the only
    equation compatible with invariant 1.
14b. The configuration write's materialization range is HALF-OPEN, `(next_due_at,
    change_instant)`, excluding the standing `next_due_at` window. That row has exactly one
    writer — §7.1, when the expectation is finally acted on — so a config write committed while
    `next_due_at` is already past cannot collide with it.
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
20. A stroke on the Response time panel is permitted only for a span entirely plain `covered` and
    entirely at or after `ledger_from`. A single `covered_late` window forbids it.
20d. The effective interval has ONE owner — a single helper called by both the plain
    (`scheduler.go:1427`) and credentialed (`:1635`) paths, and passed into
    `materialize.go` rather than recomputed there.
20g. `interval_assumed` is EXPLANATORY ONLY. No verdict, gate, numerator or surface may use it to
    treat a `covered_late` window as `covered`. A stroke drawn because a window's lateness was
    "assumed rather than measured" is the truthful-rendering gate widened by accident, and this
    invariant exists so that requires deleting a rule rather than reinterpreting one — **discharged
    by §17.1's assumed-`covered_late` case and its two promotion mutations, without which the rule
    could be ignored rather than deleted** (reviewer [270]).
20f. `interval_assumed` is returned with every window the read API emits. A `covered_late` whose
    threshold was assumed is distinguishable from one measured against the interval that actually
    spaced the window.
20e. The lateness threshold is NEVER taken from a result. A window the core recorded uses its own
    `interval_seconds`; an orphan row uses `MIN(interval, confirm_interval)` for its revision and
    sets `interval_assumed`. No executor-supplied value can move a window from `covered_late` to
    `covered`.
20c. Every `INSERT INTO expected_runs` names `interval_seconds`, and the value is the interval that
    SPACED that window — the one in force BEFORE the advance for the current window, the
    `generate_series` step for a gap window, and the conservative timeline minimum for an orphan.
    Never the caller's `new_interval`, which spaces the NEXT window.
20a. A window whose run was late by MORE than `expected_runs.interval_seconds` reads `covered_late`,
    licenses no stroke, and is excluded from the coverage numerator. The threshold is read from the
    ROW, never from the monitor's current interval and never from a revision's base interval, so a
    window probed under confirm acceleration is judged against the interval that actually spaced it.
20b. Push monitors have no ledger windows at all (§15). `checkStalePush` remains the single mechanism
    that detects a push which did not arrive.
21. Nothing in the ledger is read to decide what to probe.
22. The capacity model of §12.1 is verified by measurement against a populated table before phase D
    closes, and the retention default, minimum and maximum are configuration with enforced bounds.
23. The HOT ratio is exposed as ONE unlabelled gauge over the current retention window, gated at
    `>= 0.90` after at least 1,000 updates, and left UNPUBLISHED when the denominator is zero. A
    ratio below the threshold selects §11's append-only variant rather than being absorbed silently.
23a. Every `expected_runs` PARTITION is created with `fillfactor = 70`, asserted by reading
    `pg_class.reloptions` on a newly created partition — storage parameters are not inherited from a
    partitioned parent, so parent DDL proves nothing.
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
25. A result whose `job_id` is absent or not a UUID, or whose `due_at` is absent, postdates its own
    `issued_at` or falls outside retention, correlates to NO window, is still recorded as a
    heartbeat, and never produces a false `issued_never_claimed`.
25a. `DueAt` is minted by the CORE from `monitor_schedule.next_due_at` in the dispatching tick, never
    from the leader's 15-second snapshot, and is copied onto the result by `dispatch.StampResult`
    alone.
25f. Every field the ledger correlates on names its type, its JSON tag, its mint source and its
    single copier. "And its result twin" is not a specification — the finding at [255] that removed
    the field this invariant was written for, and the rule outlives it.
25b. A crossed pair — one run's `job_id` with another window's `due_at` — updates nothing.
25c. Every write to `expected_runs` names `carrier_generation` in its column list, so §6.1's CHECK
    cannot be violated by omission on any path.
25d. Every RUN fact on a row — `execution_revision`, `region`, `carrier_generation` — comes from the
    PUBLISHED job, never re-read from the schedule at write time. A never-issued window, having no
    job, takes them from the configuration in force.
25e. The fence compares BOTH `next_due_at` and `monitors.execution_revision`, the latter being the
    same column the ingest gate reads, so a config write cannot pass the fence by leaving
    `next_due_at` alone. On mismatch nothing is written and nothing advances.
26. Every ledger row is reachable only within its tenant: the composite `(monitor_id, project_id)`
    foreign key, not a single-column one.
26a. Every ledger query carries its own `project_id` predicate in SQL, proven by a cross-project
    negative test against the STORE layer, not only through the handler. A monitor outside the
    caller's tenancy is 404 hidden, and a mismatched `projectID`/`monitorID` pair is 404 too.
26b. The read API returns `ledger_from` and `gap_truncated_before` with every answer, and a range
    reaching before `ledger_from` returns `unknown` windows rather than silently starting later.

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

**Carrier eligibility, required at [233], on every transport.** For AMQP, pull and inproc in turn: a
dispatch on a carrier below `LedgerMinCarrier` produces a row whose window reads `unknown` and never
`issued_never_claimed`; a dispatch at or above it produces an eligible row; and a result whose
**payload** `ProtocolVersion` claims a higher generation than the carrier it arrived on does **not**
promote the row. Plus the rolling-upgrade case: a region whose executors are still on the old carrier
yields `unknown` windows until its carrier moves, and no window silently becomes `covered`. A row
left at a column default fails all of these by construction, which is the [233] P0.

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

**Orphan terminal, required at [235].** A result arrives for a window with no row: assert the row is
CREATED with `job_id`, `carrier_generation`, `issued_at`, `due_at` and `terminal_at` all set from the
wire, and that it satisfies §6.1's CHECK. **The mutation that must fail it is revision 7's own SQL** —
drop `carrier_generation` from the INSERT's column list — which errors on the constraint rather than
silently misbehaving, and would have taken the reconciliation path down on the one case it exists for.

**The config interleaving, required at [237], with the outcome corrected at [239].** The leader reads
and publishes at revision 5; a config write commits **without changing `next_due_at`**, bumping the
revision to 6; §7.1 then runs for the old item. Assert: nothing is materialized, nothing advances, no
fence is written, and the monitor is still due. Then a job published at revision 6 materializes the
window at `(monitor_id, D)` and is the only eligible row.

**Both arrival orders of the stale rev5 result must be tested** — before and after the rev6
materialization — and in both the assertion is that the ledger is **untouched**: no
`refused_at` on the rev6 row, no rev5 row created, no window blocked, and the rev6 window's verdict
determined solely by the rev6 run. The rev5 refusal is counted in the existing result-outcome
metrics and nowhere in the ledger. Revision 9 asserted the opposite and the schema could not have
satisfied it.

**Two mutations must fail this.** Revision 8's single-predicate fence, and dropping
`execution_revision` from the refusal statement's guard — the latter would write a rev5 refusal onto
the rev6 row, which is the misattribution [239] named.

**Reversed arrival of two refusals, and of two terminals, required at [241].** Deliver refusal B
(later instant, reason `future_timestamp`) first, then refusal A (earlier instant, reason
`stale_revision`) for the same job. Assert `refused_at = A` **and** `refused_reason = A's reason` —
never A's timestamp beside B's reason. Repeat both orders and assert the same final row. Then the
identical pair for `terminal_at`/`outcome` with two terminal deliveries, because that statement had
the same defect unreported.

**The mutation that must fail it:** revert either attribute to `COALESCE(existing, new)`. That is the
shape both statements shipped with through revision 10.

**`interval_assumed` cannot promote a verdict, from [270] — the test invariant 20g lacked.** A window
whose row has `interval_assumed = true` and reads `covered_late`, fetched through §13a so the flag is
actually on the response a consumer sees. Assert: the FR-031 stroke gate refuses the span, and the
coverage numerator excludes the window while the denominator keeps it.

**Two mutations must fail it, and they are the two a consumer would plausibly write:**

1. In the FR-031 gate — treat `interval_assumed && covered_late` as `covered` and allow the stroke.
2. In the coverage numerator — the same promotion, counting the window as covered.

Both are the "the lateness was only assumed, so it probably was not real" argument expressed in
code, which is exactly how §13a says the gate would widen without anyone deciding to widen it.
Revision 21 stated the prohibition and shipped no test for it, so the rule could have been **ignored**
rather than deleted — weaker than either of the outcomes I claimed for it.

**The orphan threshold, from [260].** A monitor whose confirm interval is 10s and base interval 60s,
a window spaced at the confirm interval, and an orphan terminal 40s late. Assert the row is created
with `interval_assumed = true` and `interval_seconds = 10`, and that the window reads
**`covered_late`**. **Two mutations must fail it**: using the base interval (60s) for the threshold,
which would read `covered`; and taking the value from the result at all, which is what revisions
16–18 specified.

**Lateness, from the owner's ruling of 2026-09-04.** A leader gap of ten intervals, then a run that
answers the standing 10:01 expectation at 10:42. Assert the window reads **`covered_late`**, not
`covered`; that it licenses **no stroke**; and that it is **absent from the coverage numerator** while
still counting in the denominator. Then a run late by LESS than one interval: assert plain `covered`.
Then the same monitor under confirm acceleration, asserting the threshold is taken from
`expected_runs.interval_seconds` and not from the monitor's base interval — **the mutation that must
fail it is reading the interval from `monitors` or from the revision timeline's base value.**

**Push exclusion, from the same ruling.** A push monitor produces **no** ledger window at any point:
not on its schedule, not through `checkStalePush`, not through a late arrival. Assert
`checkStalePush` still synthesises its DOWN exactly as before, so the existing detector is untouched.

**Late and overlapping runs, required at [235].** Two runs outstanding with different `due_at`: the
terminal for the older must fill the older row and must be incapable of touching the newer, and vice
versa. Assert with the `job_id` of one and the `due_at` of the other that NOTHING is updated — a
crossed pair must correlate to no window rather than to the wrong one.

**The mutation that must fail (a).** Replace the per-monitor `row_number() … PARTITION BY monitor_id`
cap with a batch-wide `LIMIT $5`, exactly as revision 2 had it. The test must fail **by name**,
identifying the monitor whose windows vanished and the fence that was never written. A test that
passes against that mutation has not reached the mechanism and is worthless here — this is the
defect the reviewer found by reading SQL that my own prose contradicted, and a regression of it must
be caught by a test rather than by another review round.

### 17.2 The discharge audit — 69 invariants: 32 covered, 0 specified, 6 discharged, 1 withdrawn, 30 to specify

The owner authorized this audit on 2026-09-04 and the reviewer had insisted at [272] that it be
sized as its own scope rather than folded into the design approval. It asks one question of each
invariant: **is there something that DIES when this is violated?** Invariant 20g had nothing until
the reviewer found it at [270], which is why a count of 64 proves nothing on its own.

**Result: 69 invariants — 32 covered, 0 SPECIFIED, 6 DISCHARGED (13a by phase A, 10j by B1-M, and
10g, 10h, 10k and 11 by B1's transport slice), 1 withdrawn,
30 to specify (19 in B2, 9 in D, 2 in C).** Phase B1 now owns no undischarged row. Every number in this paragraph and in the heading above
it is now DERIVED from the table below, by `check_fr032_audit_totals`, because they were typed there
instead and drifted: they read "65 invariants ... 30 to specify" while the table already held 66 rows
and 28, through 13a's addition and §17.4's specifications. A total written beside its own table is
the one number nobody re-derives. And the shape of the gap is the finding, not the
number: every test in §17.1 was added in response to one of the eight P0 rejections, so coverage
tracked **the design's defects** rather than **the requirement's purpose**. The four invariants that
say what FR-032 is FOR had nothing testing them —

- **1** — no monitor's probe instant changes. The property the owner selected the model for.
- **4** — a window is `covered` only if a terminal exists, never inferred from a nearby heartbeat.
- **5** and **6** — issued-never-claimed, claimed-never-finished, and never-issued as three
  distinguishable states. This is the requirement's entire subject.

**Seven need a SOURCE SCAN rather than a behavioural test**, and that distinction would otherwise
have been discovered by writing the wrong test: "exactly one statement advances `next_due_at`",
"`worker` imports no store package", "one helper computes the effective interval", "no prober path
reads `expected_runs`". These are properties of the code's SHAPE, and a behavioural test cannot see
them — the pattern is NFR-025's idiom ratchet in `wallclock.spec.ts`, which counts occurrences per
file against an enumerated list.

Six need a **schema assertion** (a CHECK that must REJECT, an absent column, `pg_class.reloptions` on
a newly created partition), two a **measurement**, and one is a **document check**.

**It is an ENTRY gate, not a close gate** (reviewer [286]): before phase N's code begins, every row
owned by N must be upgraded from `TO SPECIFY` to a named test, a killed mutation and a check kind. A
close gate would let the code be written first and the test shaped to fit it, which is how the gap in
this table was produced in the first place. The whole map stays here now so omissions stay
visible, and later phases' test MECHANICS are deliberately not invented yet.

**Assigning a phase to each row surfaced a defect in §16's own plan.** Phase A was written as
"`monitor_execution_revisions` + write it in the revision-bump transaction + §10's segment close +
backfill" — but §10's segment close MATERIALIZES WINDOWS, and `expected_runs` does not exist until
B2. A cannot do it. The segment close belongs to B2, and §16 is corrected accordingly. Each row is
assigned the EARLIEST phase in which it becomes testable, which is what makes an entry gate
meaningful.

**The seven source scans have Go precedent here and I was wrong to say otherwise** ([286]).
`internal/api/monitordoors_test.go` (FR-026 — a requirement I cited repeatedly in this arc),
`internal/api/incidentdoors_test.go` and `internal/domain/canarybounds_test.go` all use
`go/parser`/`go/ast` over real files, and the last kills source-shape mutations. So each structural
guard goes **next to its owning package** — the advance-statement guard in `internal/scheduler`, the
`worker`/`agent` import boundary as a narrow architecture test — and **no second global mechanism is
built**.

**Reading the Status column.** `covered` means **a §17.1 case specifies a test for it** — not that
the test exists, because outside phase A no code does. `DISCHARGED` means the tests exist, run and
kill their mutations. `TO SPECIFY` means nothing yet describes how it would be proven. The
distinction matters: 34 `covered` rows are 34 specifications, and reading them as 34 tested
invariants is the same error as reading an invariant count as coverage — the error this audit exists
to correct.

| # | Phase | Check kind | Status | Where, or what is missing |
| --- | --- | --- | --- | --- |
| 1 | B2 | behavioural | TO SPECIFY | the OWNER's headline property, and nothing tests it: probe instants before and after the change, per dispatch path |
| 2 | B2 | behavioural | **covered** | §17.1 (b) and (c) exercise rules 1, 3, 4; rules 2 and 5 are not |
| 2a | B2 | source scan | TO SPECIFY | a source scan: exactly one statement advances `next_due_at`. A second one is the [229] defect |
| 2b | B2 | behavioural | **covered** | §17.1 (c) asserts `interval_in_force` still 60 under backoff |
| 2c | B2 | behavioural | TO SPECIFY | the RESTORE half is untested: acceleration expiring must put `interval_in_force` back |
| 2d | B2 | source scan | TO SPECIFY | rule 5's statement must be structurally incapable of moving `next_due_at` forward |
| 3 | B2 | behavioural | **covered** | §17.1 (b) and (c) both start from a persisted past `next_due_at` |
| 4 | B2 | behavioural | TO SPECIFY | core truthfulness and untested: a heartbeat at a nearby instant must NOT make a window `covered` |
| 5 | B2 | behavioural | TO SPECIFY | issued-never-claimed vs never-issued, asserted as two distinct verdicts |
| 6 | B2 | behavioural | TO SPECIFY | claimed-never-finished, distinct from both of the above |
| 7 | C | behavioural | **covered** | the reversed-arrival case from [241] |
| 7a | C | source scan | TO SPECIFY | a source scan over event statements for the ONE admissibility predicate, in the shape of NFR-025's idiom ratchet |
| 7b | C | behavioural | **covered** | §8.1's two negatives — but they live in §8.1's prose, not §17.1's matrix; move them |
| 7c | C | behavioural | **covered** | the reversed-arrival case from [241], both pairs |
| 8 | C | behavioural | **covered** | terminal-before-claim, in the ordering cases |
| 9 | C | behavioural | **covered** | duplicate delivery, in the reversed-arrival case |
| 10 | B2 | behavioural | TO SPECIFY | a terminal with no claim and no `issued_at` must still yield `covered` |
| 10a | C | behavioural | **covered** | the config interleaving asserts a refused result fills nothing |
| 10b | C | behavioural | **covered** | §8.3 proof 3, exercised by the skip cases |
| 10c | B2 | behavioural | **covered** | carrier eligibility, all three transports |
| 10d | B2 | behavioural | **covered** | the same case, plus the forged-payload mutation |
| 10e | B2 | schema assertion | TO SPECIFY | assert the CHECK REJECTS a job with no carrier and a carrier with no job |
| 10f | C | behavioural | **covered** | both arrival orders of the stale result |
| 10g | B1 | behavioural | **DISCHARGED** | 5 tests, 4 mutations killed (§17.6) — separate queues per generation, an unmapped generation routed nowhere, and a v3 claim leaving a generation-4 row outside its result set on a real database |
| 10h | B1 | behavioural | **DISCHARGED** | 4 tests, 3 mutations killed (§17.6) — the config refuses `true` without coercing it, and the v4 endpoint requires its own ledger declaration rather than the credential one |
| 10i | B2 | behavioural (live broker) | **TO SPECIFY** | reassigned from B1 by reviewer P0 at [342]: a V4 delivery is DEFINED by `DueAt`, which the wire does not carry until B2, so B1 has no absence to detect. §17.4 keeps both mutations named for B2's gate |
| 10j | B1 | migration | **DISCHARGED** | 5 tests, 8 mutations killed (§17.5) — the shipped goose Down, refusal plus row survival plus atomic still-widened constraints |
| 10k | B1 | behavioural | **DISCHARGED** | 4 tests, 1 mutation killed (§17.6) — `lead()`'s resolved map stays ≤ 3 on all three transports with an ANNOUNCING executor and the flag off |
| 10l | B2 | behavioural | **TO SPECIFY** | when the flag is true `role=all` reaches 4 and pull regions do NOT — the two V3 failures §13.0 quotes, one inert deployment and one unclaimable row |
| 11 | B1 | source scan | **DISCHARGED** | 3 tests, 1 mutation killed (§17.6) — one import scan beside each executor package, plus the exact five-method `Dispatcher` set |
| 11a | B1 | — | **n/a, a withdrawal** | records that the pull agent's lease-ack predates this requirement and is out of scope |
| 12 | B2 | schema assertion | TO SPECIFY | assert `heartbeats` columns unchanged, and that no index covers the six fill columns |
| 13 | B2 | behavioural | **covered** | the confirm-acceleration threshold case — it says "attributed to a WINDOW", and there are no windows before B2 |
| 13a | A | behavioural + **source scan** | **DISCHARGED** | 13 tests, 7 mutations killed (§17.3). Specifying it found FOUR bump sites, not one; implementing it found that retire and reactivate are COMPOSITE-only, that the fence lives in `updateMonitorTxPrepared`, and — via the reviewer — that CREATION makes a generation too |
| 14 | B2 | behavioural | **covered** | the config-boundary cases |
| 14a | B2 | behavioural | **covered** | the four-case each-side-of-a-due-instant test |
| 14b | B2 | behavioural | **covered** | the `next_due_at`-already-past case |
| 15 | D | behavioural | TO SPECIFY | a range before `ledger_from` must yield `unknown`, never `covered` and never `expected_never_issued` |
| 16 | D | behavioural | TO SPECIFY | drop a partition, then assert no window in the dropped span reads `covered` or `expected_never_issued` |
| 16a | D | schema assertion | TO SPECIFY | insert a row with no matching partition and assert the DEFAULT takes it; then that retention purges the default |
| 17 | B2 | behavioural | **covered** | the late-and-overlapping case |
| 18 | C | behavioural | TO SPECIFY | a flushed unfinished window plus a late terminal must read completed-late, not be deleted |
| 19 | B2 | schema assertion | TO SPECIFY | a schema assertion: no verdict column exists on `expected_runs` |
| 20 | E | behavioural | **covered** | 20g's gate case, extended to `expected_never_issued` and pre-`ledger_from` spans |
| 20a | E | behavioural | **covered** | lateness above and below the threshold |
| 20b | E | behavioural | **covered** | push exclusion by every path |
| 20c | B2 | behavioural | **covered** | the per-path interval value, with the `new_interval` mutation |
| 20d | B2 | source scan | TO SPECIFY | a source scan: one helper computes the effective interval; `:1427` and `:1635` both call it |
| 20e | D | behavioural | **covered** | the orphan-threshold case |
| 20f | D | behavioural | **covered** | the §13a round trip in 20g's case |
| 20g | E | behavioural | **covered** | added at [270], with its two promotion mutations |
| 21 | B2 | source scan | TO SPECIFY | a source scan: no scheduler or prober path reads `expected_runs` |
| 22 | D | measurement | TO SPECIFY | a measurement against a populated table, plus bounds enforcement on the config value |
| 23 | D | measurement | TO SPECIFY | the gauge's presence, its threshold, and that it is UNPUBLISHED on a zero denominator |
| 23a | D | schema assertion | TO SPECIFY | read `pg_class.reloptions` on a NEWLY created partition |
| 24 | B2 | behavioural | **covered** | §17.1 (a), (b), (c) |
| 24a | B2 | behavioural | **covered** | the same, asserting the fence |
| 24b | B2 | behavioural | **covered** | §17.1 (a) — two monitors, one cap |
| 24c | D | behavioural | TO SPECIFY | drop a partition and assert `ledger_from` moves with it, having been computed not stored |
| 25 | B2 | behavioural | TO SPECIFY | five bad-result shapes, each correlating to no window while the heartbeat still lands |
| 25a | B2 | source scan | TO SPECIFY | a source scan: `DueAt` minted from the schedule read, and `StampResult` the only copier |
| 25b | C | behavioural | **covered** | the crossed-pair case |
| 25c | B2 | source scan | **covered** | the grep over every `INSERT INTO expected_runs`, which found the revision-15 defect |
| 25d | B2 | behavioural | **covered** | the config interleaving proves the row takes the published job's facts |
| 25e | B2 | behavioural | **covered** | the same case; its mutation is revision 8's single-predicate fence |
| 25f | B2 | document check | TO SPECIFY | a document check: every correlated field names type, tag, mint source and copier |
| 26 | B2 | schema assertion | TO SPECIFY | assert the FK is composite, and that a single-column FK fails the assertion |
| 26a | D | behavioural | TO SPECIFY | a cross-project negative at the STORE layer, not through the handler |
| 26b | D | behavioural | TO SPECIFY | every response carries `ledger_from` and `gap_truncated_before` |

### 17.3 Phase A's entry gate — invariant 13a specified

The entry gate of §17.2 requires this before phase A's code begins. Writing it found something the
invariant as first drafted got wrong.

**And a FIFTH way to create a generation that is not a bump at all: creating the monitor.** A new
monitor starts at `execution_revision = 1`, and until revision 24 nothing wrote that generation's
row — so its configuration was indescribable until someone updated it. The reviewer found this at
[304] because the invariant said "every bump" while the property is "every generation", and a
fence-keyed guard is blind to initialization by construction.

Both create paths — `CreateMonitor` at `internal/store/monitors.go:650` and the file-apply loop at
`internal/store/fileapply.go:262` — call the SAME `insertMonitorTx`, so ONE write covers both and
they cannot diverge. That is a stronger fix than patching two sites, and it is why the guard's
creation half keys on `INSERT INTO monitors` rather than on either caller.

**The guard's two halves detect OPPOSITELY, deliberately.** The fence check ignores string literals,
because the identifier appears inside one as an SQL comment. The creation check inspects them,
because the `INSERT` lives inside one. Two properties, two detections, and neither is a variant of
the other.

**There are FOUR revision-bump sites, not one.** `revisionFenceSetSQL` — the D-0142 fence — appears
at `internal/store/monitors.go:878` (inside `updateMonitorTx`), `internal/store/monitorretire.go:153`
(retire: `retired_at`, `enabled = false`), `:219` (restore: `retired_at` cleared, `enabled = true`)
and `internal/store/secrets.go:434` (a secret rotation changes what the monitor executes). All four
are legitimate generation changes.

**The fourth is a BULK bump, which the other three are not.** `secrets.go:434` is the rotation fence:
`UPDATE monitors SET <fence> WHERE id = ANY($1::uuid[]) AND project_id = $2` — it bumps every monitor
that references the rotated secret, in one statement. Its timeline write must therefore insert **one
row per affected monitor**, not one row. A test that rotates a secret referenced by a single monitor
would pass against an implementation that writes exactly one row regardless, so the test rotates a
secret referenced by **three**.

**Seven occurrences of the identifier, four of them real uses**, and the difference is why the guard
must parse rather than grep: `monitors.go:45` is the doc comment, `:51` is the `const` definition,
and **`:874` is the identifier appearing inside a raw SQL string as an SQL comment**
(`-- fence (see revisionFenceSetSQL)`). A text scan counts that as a fifth use; a `go/ast` scan sees
a `BasicLit` and does not. The guard counts identifier REFERENCES in expressions, which is exactly
four today.

`updateMonitorTx`'s comment calls itself "the shared config-write contract (D-0142)", and revision 23
of this spec read that as "the only one". It is the shared contract for the USER and FILE paths; it
is not the only place a generation is created. **A generation with no timeline row is a window whose
configuration cannot be described, which is invariant 13 broken from underneath** — so phase A owns
all four, not one.

| Claim of 13a | Check kind | Test | The mutation that must kill it |
| --- | --- | --- | --- |
| Every bump writes its row in the SAME transaction | behavioural, per site | Four store tests, one per site — update, retire, restore, and a secret rotation touching THREE monitors: bump, then assert a `monitor_execution_revisions` row exists for the NEW revision of every affected monitor. Then force the row-write to fail and assert the BUMP rolled back: no generation without a row | Move the timeline insert after the transaction commits, at any one of the four sites — the forced-failure half must then leave a bumped revision with no row and fail BY NAME, naming the site. And for the rotation site specifically, write ONE row instead of one per monitor: the three-monitor test must fail where a one-monitor test would pass |
| No FUTURE site can bump without one | **source scan** | A `go/parser`/`go/ast` test over `internal/store`'s non-test files, in the shape of `internal/api/monitordoors_test.go`: every identifier REFERENCE to `revisionFenceSetSQL` — not every textual occurrence, see above — must be inside a function that also references the timeline insert. Asserts the count is exactly four and enumerates them, so a new site is a failure rather than a silent pass. Placed in `internal/store` beside the code it guards, per [286] | TWO mutations: add a fifth use in a function with no timeline insert (the scan must fail and NAME the function); and change the scan to a text match, which then counts `monitors.go:874`'s SQL comment as a use and reports five — a guard that miscounts its own subject is worse than none |
| The backfill writes exactly one row per monitor | behavioural | Seed monitors including a DISABLED one and a RETIRED one, migrate, assert `count(monitor_execution_revisions) == count(monitors)` and `effective_from = monitors.updated_at` | Restrict the backfill to `WHERE enabled` — the disabled monitor then has no row and the count assertion fails |
| Every row carries that revision's four cadence fields | behavioural | Assert `interval_seconds`, `confirm_interval_seconds`, `timeout_seconds` and `retries` each equal the monitor's value | Drop `confirm_interval_seconds` from the backfill's projection. A test asserting only `interval_seconds` would survive this, which is why all four are asserted individually |

**The tests now exist and are named here, so `make docs-check` validates them against the tree**
(they were described by their assertions until the implementing commit, because citing a
non-existent `Test*` name would either break that gate or force an `ALLOWED` entry that hides a real
absence):

| Claim | Tests |
| --- | --- |
| Same transaction, per site | `TestUpdateMonitorRecordsItsGenerationWithAllFourCadenceFields`, `TestRetireAndReactivateEachRecordTheirGeneration`, `TestASecretRotationRecordsAGenerationForEveryAffectedMonitor`, `TestTheTimelineRowAndTheBumpShareOneTransaction` |
| **Creation records generation 1**, on both paths | `TestCreatingAMonitorRecordsItsFirstGeneration`, `TestTheFileApplyCreatePathRecordsItsFirstGenerationToo` |
| **The write is project-scoped and REFUSES** a cross-project pair | `TestTheTimelineWriteRefusesAMonitorFromAnotherProject` |
| No future site | `TestEveryRevisionFenceWritesItsTimelineRow`, `TestTheRevisionFenceSitesAreTheFourWeKnowAbout`, `TestTheRevisionFenceGuardFailsOnAFixtureThatViolatesIt`, `TestTheRevisionFenceGuardCountsReferencesNotTextOccurrences`, `TestTheGuardFailsOnACreationThatWritesNoTimelineRow` |
| Backfill covers every monitor | `TestTheBackfillCoversEveryMonitorIncludingDisabledOnes` |
| Four cadence fields | `TestUpdateMonitorRecordsItsGenerationWithAllFourCadenceFields`, which asserts each field separately |

**`writeRevisionTimeline` takes an explicit `projectID` and its SQL carries
`AND m.project_id = $2`.** I first argued the predicate was unnecessary: the ids all come from
project-scoped queries, and a stray id could only ever record a generation that genuinely is in
force, with `ON CONFLICT DO NOTHING` absorbing a duplicate. The reviewer's counter is the one I had
myself named as decisive — **the AST guard protects against a fifth SITE, not against a future
CALLER handing the helper a list it did not scope.** The guard cannot see that, so the SQL does. And
a predicate with no test is a predicate that can be deleted with nothing failing, so the negative
test asserts BOTH halves: a cross-project pair writes nothing, and the same call with the right
project writes exactly one — otherwise the test would pass against a write that is broken for every
input.

**Mutations planted and killed: 7.** Enumerated as a list rather than restated as a word, so the
count in §17.2's row is checked against the ARTEFACT and not against another sentence — the drift
this list exists to prevent happened twice already, once for the test count and once because the
first guard checked only that number and left this one free (reviewer [318]).

1. Removing the **project predicate** — `a cross-project pair wrote 1 rows; the project predicate refuses nothing`.
2. Removing the **creation write** — the P0 exactly as it went to review. Dies THREE times: the guard names `insertMonitorTx`, and both behavioural tests name the missing generation-1 row.
3. Dropping **`confirm_interval_seconds`** from the write — `confirm_interval_seconds = 120, want 30`.
4. Writing **one row instead of one per monitor** on the rotation fence — which is why that test rotates a secret referenced by THREE monitors.
5. Deleting the **restore site's** write entirely. Dies TWICE: the AST guard names `ReactivateMonitor`, the behavioural test names the missing row.
6. Restricting the **backfill** to `WHERE m.enabled` — `backfill wrote 1 rows for 2 monitors`.
7. Turning the **AST scan into a text scan**, which then counts `monitors.go`'s SQL comment and reports five sites.

**Two facts phase A learned from the tree rather than from this document.** The four fence-bearing
functions are `updateMonitorTxPrepared` (the prepared variant, not `updateMonitorTx`),
`RetireMonitor`, `ReactivateMonitor` and `fenceSecretMonitors` — the guard caught its author guessing
all four names wrong on its first run. And **retire and reactivate apply to COMPOSITE monitors
only**, so those two generations exist only for composites; the first version of their test used an
HTTP monitor and was refused by name.

**`effective_from` is `statement_timestamp()` for a live bump and `monitors.updated_at` only in the
backfill.** The rotation fence sets *only* the two fence columns and never touches `updated_at`, so
reading it back would date a brand-new generation to the previous write — potentially days early.

### 17.4 Phase B1's entry gate — invariants 11, 10j and 10k specified; 10i reassigned to B2

B1 owns **six** rows — 10g, 10h, 10j, 10k, 11, 11a — and none is `TO SPECIFY`. 10g and 10h already
have §17.1 cases; 11, 10j and 10k are specified below; 11a is a recorded withdrawal; 10i turned out
not to be B1's to prove at all and is now B2's. Only two of these existed when this section was
written: 10j and 10k were both added by rejections of it.

Specifying this one gate has now caught **four** invariants contradicting the tree, or contradicting
the phase they were assigned to — 13a's four bump sites, 11's ack clause, 10i's phase, and 10k's own
second claim, struck below for exactly the reason 10i moved. Four in one section is not a sign the
gate is noisy; it is the argument for gating at ENTRY, where a wrong invariant costs a paragraph
instead of a test shaped to fit finished code.

**Invariant 11, corrected before it could be implemented.** The import half is true today and needs
pinning: `internal/worker` and `internal/agent` import only `dispatch` and `domain`, no store
package. The "no ack concept" half was **false** — `internal/agent/agent.go:290` acks only the jobs
it processed and `:329` deliberately does not ack so leases lapse and jobs are re-delivered. That
lease-ack is the pull transport's delivery guarantee, predates this requirement, and is out of
scope. What §4a and `amqp.go:191-194` actually require is that the **`Dispatcher` interface** expose
no ack, and it has exactly five methods today: `PublishJob`, `Jobs`, `PublishResult`, `Results`,
`Close`.

| Claim | Check kind | Test | The mutation that must kill it |
| --- | --- | --- | --- |
| Neither executor package imports a store package | source scan, one per package | A `go/ast` import scan in `internal/worker` and again in `internal/agent` — beside each owning package, per [286], rather than one cross-cutting test that neither team owns | Add `internal/store` to that package's imports. The scan must fail and NAME the offending import path, not merely the package |
| The `Dispatcher` interface exposes no ack | source scan | Parse `internal/dispatch/dispatch.go`, collect the interface's method names, assert the set is exactly the five above | Add `Ack(ctx, id) error` to the interface. The test must fail naming `Ack` — and an enumerated SET is required rather than "contains no method called Ack", because `Nack`, `Settle` or `Confirm` would all slip past a name check |
| **B1 adds neither** | source scan | The same two scans, run against B1's tree — B1 touches queues, claim endpoints and capability announcement, none of which needs a store handle in an executor | If B1's carrier work reaches for the store from `worker`/`agent`, the import scan fails on the change that introduces it, which is the point of placing the gate at ENTRY |

**Invariant 10i is not B1's to prove, and this gate asserting it WAS the defect** (reviewer P0 at
[342]). A V4 delivery is *defined* by `DueAt`, and `dispatch.CheckJob` carries `JobID` and `IssuedAt`
today but gains no `DueAt` until B2 (§13.1). So in B1 there is no field whose absence could be
detected, nothing publishes V4 at all while `ledger.carrier_enabled` is false, and the test would
have to hand-craft a publish that production cannot make — a gate naming a proof its own phase cannot
produce. Expanding B1 to meet it would mean smuggling B2's payload into a deliberately inert carrier
phase, and §16 already gives B2 the payload and the ledger atomically, so 10i moves to B2 instead.
**Both mutations stay named here**, so B2's gate inherits the analysis rather than re-deriving it:

| Mutation B2's gate must carry | Why it is the one that matters |
| --- | --- |
| Treat the missing field as a rolling-upgrade case and probe anyway | The plausible mistake: the code already tolerates absent identity on OLDER carriers, and the tolerant branch is one `if` away from covering V4 too. The test must distinguish carrier from payload |
| Drop the delivery silently instead of dead-lettering | Passes any assertion that only checks "no probe ran", which is why the dead-letter arrival is asserted separately. `AMQP.deadLetter` (`internal/dispatch/amqp.go:499`) already forwards a poison body to the durable queue "so it survives for inspection instead of vanishing", and `amqp_test.go` already skips unless `CERBIX_TEST_RABBITMQ_URL` is set |

**Invariant 10j, added by that same rejection: the migration must fail closed on ROLLBACK.** None of
the previous 66 invariants said anything about rolling back, which is how a migration that widens a
constraint acquires a DOWN that nobody specified. B1 widens `pull_jobs_protocol_version_check` and
its `pull_tests` twin to `IN (1, 2, 3, 4)`, so the DOWN narrows them again while generation-4 rows
may still be pending — and 00063 already wrote down what goes wrong there, in its own words: its
first draft "said 'drain first' and then unconditionally DELETEd them, which is a destructive
write-off wearing the words of a safe rollback". D-0160 makes draining an explicit OPERATOR step, so
the migration refuses rather than performing it silently.

| Claim | Check kind | Test | The mutation that must kill it |
| --- | --- | --- | --- |
| The DOWN refuses while generation-4 pull rows are pending | migration | Insert a generation-4 row into `pull_jobs` and another into `pull_tests`, run the migration's DOWN, and assert both that it errors AND that both rows are still there | Drain first — `DELETE ... WHERE protocol_version = 4`, then narrow. The DOWN now "succeeds" and the pending jobs are gone; only the row-survival assertion catches it, which is why it is asserted separately from the error |
| The refusal tells the operator what to DO | migration | Assert the error text names the pending COUNT and points at the drain procedure | Delete the guarded DO-block and let `ADD CONSTRAINT` fail on its own. It still fails closed, so an assertion of "the DOWN errored" passes — but the operator is left with `check constraint ... is violated by some row` and no procedure. The same lesson as the interface SET above: the weaker assertion admits the weaker code |

**Invariant 10k: the flag's inertness is asserted at the SOURCE, not at three sinks.** All three
transports read the generation the producer stamped — AMQP from the queue it published to, pull from
the row's column, in-process from the job's own field — so one assertion covers all three, and three
consumer-side assertions would still miss the producer. The assertion sits on **`lead()`'s resolved
map**: the three `carrierGeneration[region] = …` branches at `scheduler.go:1514`, `:1529` and `:1552`
all write one map built at `:1500` inside that function, so the gate reads its OUTPUT rather than any
branch. A branch-level assertion is precisely what lets one branch drift.

| Claim | Check kind | Test | The mutation that must kill it |
| --- | --- | --- | --- |
| While `ledger.carrier_enabled` is false, `lead()`'s resolved map stamps nothing above 3 | behavioural | Resolve admission for an AMQP region, a pull region and `role=all` with the flag false, and assert every value in the resolved map is ≤ 3 | Raise the generation in ONE branch only — the same-process branch is the tempting one, since "its capability is ours by construction" reads like a licence. Asserting the map's output catches it wherever it is introduced; a per-transport consumer test passes on the other two and reports green |

**Struck from 10k before implementation: "the flag is read at resolve time, not at build time."**
Reviewer P0-2 at [361], and he is right. That claim needs flag=true to be observable — flipping it
between two resolves and watching the second change — and B1 promises selection never happens, has no
`DueAt` and no V4 payload. It is B2 behaviour and belongs with 10l.

This is **the same defect as 10i**, which I had diagnosed two revisions earlier and then reproduced:
a gate naming a proof its own phase cannot produce. Worth recording rather than editing away, because
the pattern is now legible — when a phase is defined by a capability being OFF, every assertion about
that phase must be provable with it off, and any claim of the form "flipping it changes something"
belongs to the phase that flips it. B1 can prove only the false-default inert output.

**Invariant 10h's B1 half: the gate must REFUSE `true`, not absorb it.** A default only describes
absence. B1's proof is about the input an operator can actually supply.

| Claim | Check kind | Test | The mutation that must kill it |
| --- | --- | --- | --- |
| A B1 binary refuses `ledger.carrier_enabled: true` at startup | behavioural | Load a config with the key true and assert `(*Config).Validate` returns an error naming the key; assert no scheduler is constructed | Accept it. B1 would then select and publish V4 before `DueAt` exists, which is 10h's violation in one line of config |
| It refuses rather than coercing | behavioural | Assert the error mentions B2 and the missing payload, and that the loaded value is NOT rewritten to false | Coerce silently — `if !b2 { cfg.Ledger.CarrierEnabled = false }`. Every "is it inert?" assertion still passes, and the operator believes a gate is on that is off. This is the mutation the inertness tests CANNOT catch, which is why the refusal is asserted on the error and on the unmodified value |
| Absent or false stays inert on all three transports | behavioural | Resolve admission for an AMQP region, a pull region and `role=all` with the key absent, then again with it false; every value in `lead()`'s resolved map is ≤ 3 | Raise one branch — the same mutation 10k names, here run for both spellings of "off", because absent and false must be indistinguishable downstream |

**Not specified here, deliberately:** B1 writes no ledger rows, so nothing in this gate touches
`expected_runs`. The carrier's *eligibility* consequences are 10c and 10d, which belong to B2 where
rows exist.

### 17.5 Phase B1-M's discharge — invariant 10j

Landed at `2fcf2b9`, reviewed at party [369]. B1 was sequenced with the migration FIRST because 10j
is the only row in the phase with an irreversible failure mode: a rollback that discards pending work
cannot be undone by fixing the rollback afterwards.

**Every test runs the SHIPPED Down** through `goose.DownToContext` against a throwaway probe database
migrated to 101, using the existing `probeDatabaseAt` helper. Not a copy of the SQL, and that is the
whole point: 00063's own comment records that its first draft "said 'drain first' and then
unconditionally DELETEd them, which is a destructive write-off wearing the words of a safe rollback"
— a test that re-stated the intended SQL would have passed for that draft too. The 00094 tests next
door put it as "a Down nobody runs is a Down nobody knows works".

- `TestTheGenerationFourDownRefusesWhilePullRowsArePending` — rows in BOTH tables. The Down errors,
  both rows survive, the message carries the total, the per-table counts and the drain procedure, and
  the widened `CHECK` is INTACT: the refusal is atomic, not a partial rollback that leaves rows
  violating their own table's constraint.
- `TestTheRefusalTellsTheOperatorWhichTableHoldsTheRows` — only `pull_tests` holds a row; the message
  must still name both, the zero included: the split LOCATES the pending rows, telling an operator
  which of the two surfaces holds them. How either drains is the runbook's subject, not this one's.
- `TestTheGenerationFourDownSucceedsWhenNothingIsPending` — the converse, without which the guard is
  indistinguishable from one that always refuses: the Down runs, the narrowed `CHECK` rejects 4 again,
  rows at generations 1 and 3 are untouched, and the Up converges.
- `TestTheWidenedCheckStillRejectsAnUnknownGeneration` — generation 5 refused by both tables, which is
  00063's stated reason for widening rather than dropping.
- `TestTheGenerationFourDownContainsNoDestructiveStatement` — a source scan over the Down's
  executable lines, because "no destructive delete" is a property of the FILE that a future edit can
  break while every behavioural test above still passes.

**Mutations planted and killed: 8.** Enumerated as a list so §17.2's row count is checked against the
ARTEFACT rather than against another sentence.

1. The guarded `DO`-block **deleted entirely** — the Down still fails closed, because `ADD CONSTRAINT` validates existing rows, so only the message assertions catch it.
2. **Drain first**: `DELETE … WHERE protocol_version = 4`, then narrow. The Down now succeeds and the work is gone.
3. The guard counting **generation 3** instead of 4.
4. The Down **DROPping** the `CHECK` instead of narrowing it — caught by the drained converse, which then accepts a generation-4 row.
5. The **Up** admitting generation 5 as well.
6. The refusal reporting **only the total**, dropping the per-table split.
7. The **drain procedure** dropped from the message, leaving a refusal an operator cannot act on.
8. **`-- +goose NO TRANSACTION` plus a `DELETE` before the guard** — the Down still refuses, and the rows are already gone.

**Why the eighth exists, and why row survival is asserted separately from the error.** Mutation 2 was
killed by the *"the Down succeeded"* assertion, which means the row-survival assertion had proved
nothing yet — it was never reached. So the shape where only it can catch was built and run: the Down
refuses, and the rows are gone anyway. It reports `pull_jobs holds 0 generation-4 row(s) after the
refusal, want 1: the Down discarded work`. An assertion earns its place by killing something no other
assertion in the test kills.

**Not discharged by this slice:** 10g, 10h, 10k and 11 remain `SPECIFIED`. B1-M contains no transport
code, no config, no `ProtocolV4` selection and no ledger row, so nothing here touches them.

### 17.6 Phase B1's discharge — invariants 10g, 10h, 10k and 11

Landed with the transport slice. Each row's proof lives beside the code it guards rather than in one
cross-cutting file, which is why this section cites six files.

- **10g, the LIVE AMQP half.** `TestAV3ConsumerCannotReceiveAGenerationFourDelivery`
  (`internal/dispatch/amqpledger_test.go`), opt-in on `CERBIX_TEST_RABBITMQ_URL` as `amqp_test.go`
  is: a real v3 consumer bound, a generation-4 job published, and nothing delivered inside a
  bounded wait — then a ledger-capable consumer added, which DOES receive the next one while the v3
  consumer still receives nothing with both bound at once. The mapping test below proves the queue
  NAMES differ; a name is a fact about a function, and 10g is a fact about a wire.

  **It found two defects the mapping test could not see** (reviewer P0 at [448]). The publisher
  refused a generation-4 job with no credential envelope, because the guard read `generation >=
  ProtocolV2` — so a V4 job was unpublishable for any monitor without secrets, which is every
  ordinary HTTP monitor and the exact coupling §13.0 forbids. And no AMQP worker ever declared the
  ledger capability, so `LiveLedgerJobRegions` looked for a consumer that could never exist: the
  announcement was dead on that transport while looking implemented.

  **And the first fix for that reintroduced the coupling in the WIRING** (reviewer [450]): the
  worker's declaration was derived from the envelope capability, so an envelope-disabled deployment
  — the default, since `secrets.enabled: false` is meant to change nothing else — had no
  ledger-capable worker, `LiveLedgerJobRegions` stayed false, and the ordinary secretless monitor
  FR-032 exists for could never reach the carrier. It now follows the ROLE, in
  `executorLedgerCapability`, which takes no envelope argument at all so the coupling cannot be
  threaded back in. The pull agent's rule stays different on purpose and is not an inconsistency: a
  v4 CLAIM returns every generation at or below 4, envelope-bearing ones included; an AMQP v4 QUEUE
  carries generation-4 deliveries only.
- **10g, mapping.** `TestEveryCarrierGenerationHasItsOwnQueue` and
  `TestAnUnknownCarrierGenerationRoutesNowhere` (`internal/dispatch/ledgercarrier_test.go`) —
  distinct queues per generation, and an unmapped generation routed NOWHERE rather than falling back
  to an older queue. `TestAV3ClaimLeavesAGenerationFourRowUnclaimed`
  (`internal/store/pullcarrier4claim_internal_test.go`) is the pull half on a real database: the
  predicate leaves the generation-4 row outside a v3 claim's result set, and the converse v4 claim
  reaches it. `TestAV3ClaimCannotReachAGenerationFourRow` (`internal/api/api_agent_ledger_test.go`)
  is the same property through the endpoint.
- **10h, no V4 before its payload.** `TestABinaryOfThisGenerationRefusesLedgerCarrierEnabled`,
  `TestTheRefusalDoesNotRewriteTheValue` and `TestAbsentAndFalseAreBothAcceptedIdentically`
  (`internal/config/ledgercarrier_test.go`), plus
  `TestTheGenerationFourEndpointRequiresTheLedgerCapability`,
  `TestADeclaredGenerationFourClaimReachesTheRow` and
  `TestTheAnnouncedLedgerCapabilityIsAClosedDomain` (`internal/api/api_agent_ledger_test.go`) — the
  announced value is 0 or 1, refused at the boundary rather than stored and interpreted later — and
  the CLAIM header is closed the same way, by exact equality rather than a floor. The envelope
  capability is generational, so declaring more than an endpoint needs is legitimate there; the
  ledger capability has exactly one value in this generation, and a floor would have made
  `X-Cerbix-Ledger: 2` a silent yes for something nobody has defined. Half a closed domain is not
  a closed domain (reviewer [455]).
- **10k, the false flag stamps nothing above 3.**
  `TestTheFalseLedgerFlagStampsNothingAboveThreeOnAnyTransport`,
  `TestTheLedgerFlagOnRaisesTheAnnouncedRegion`,
  `TestAPullRegionIsRaisedOnlyByItsAgentsAndOnlyWithTheFlagOn` and
  `TestAPullRegionIsNotRaisedByTheLocalExecutor` (`internal/scheduler/ledgercarrier_test.go`),
  asserted on `lead()`'s resolved map as captured by the materializer. The announcement is asked as
  TWO questions with two sources, matching the transports: AMQP by a consumer on the v4 queue
  (`mqadmin`), pull by an agent declaring `capabilities->>'ledger'` on its heartbeat.
- **The pull chain end to end.** `TestACapableAgentClaimsGenerationFourAndExecutesTheRow`,
  `TestAnAgentNewerThanItsCoreFallsBackWithoutLosingThePoll`,
  `TestACoreUpgradedUnderARunningAgentIsFoundAgain`,
  `TestACoreRolledBackUnderAProvenAgentWithdrawsTheAnnouncement`,
  `TestAnAgentAnnouncesNothingBeforeItsFirstServedV4Claim`,
  `TestAnAgentThatCannotOpenEnvelopesAnnouncesNoLedgerCapability` and
  `TestTheTestEndpointIsNotDraggedToGenerationFour` (`internal/agent/ledgerclaim_test.go`) — the
  agent's own request path, its per-claim declaration, and the row it executes.
- **The construction path.** `TestTheLedgerAnnouncementFollowsTheRoleAndNotTheEnvelopeCapability`
  and `TestAnEnvelopeDisabledDeploymentStillAnnouncesTheLedgerCarrier`
  (`internal/cli/ledgercapability_test.go`) — the broker isolation test above proves the wire, and
  neither it nor any unit test exercised the wiring that decides what is declared on it.
- **11, the executor boundary.** `TestThisExecutorImportsNoStorePackage` in BOTH
  `internal/worker/storeboundary_test.go` and `internal/agent/storeboundary_test.go`, plus
  `TestTheDispatcherInterfaceExposesNoAck` and
  `TestTheLedgerCapabilityIsSeparateFromTheCredentialOne`
  (`internal/dispatch/ledgercarrier_test.go`).

**Announcement and consumption are ONE condition.** An agent announces `capabilities.ledger` only
when it can actually claim generation 4, and claiming it is what the announcement authorizes core to
select for. The reverse — announcing without claiming — is worse than announcing nothing: core is
authorized to place a v4 row in a region that will never claim it, and the monitor has no outcome
until the row's TTL. That is the generation-3 failure `scheduler.go` records, one generation later,
and B1's first draft contained it (reviewer P0 at [428]).

Three consequences the fix had to carry. **The envelope floor still applies to a v4 claim**, because
that claim returns every OLDER generation too, envelope-bearing ones included — not the ledger
capability being derived from the credential one, but a cumulative range needing what its widest
member needs. **An agent newer than its core must not die, and must not stay deaf either**: §16.1 calls "core at
B1, executor at B2" a normal rollout state, so a 404 from `agentJobsV4` downgrades, retries
immediately so no poll is lost, and **re-probes after a bounded window**. A PERMANENT downgrade
recreates the same defect one rollout step later — core upgraded underneath a running agent, the
agent still announcing while claiming v3 forever, the row unclaimable again (reviewer [432]).
The state has **three** positions, not two, because eligibility to TRY is not eligibility to
ANNOUNCE (reviewer [436]). An attempt costs one 404; an announcement costs an unclaimable row.

| Predicate | Meaning | Read by |
| --- | --- | --- |
| envelope floor | this agent can open what a v4 claim also returns | both |
| `ledgerAbsentUntil` | when the next v4 ATTEMPT may be made | the claim path |
| `ledgerProven` | a v4 claim actually returned 200 | the ANNOUNCEMENT, and nothing else |

Collapsing the last two made the false announcement **periodic instead of permanent**: the cooldown
would expire, the agent would advertise generation 4 before probing, core could select a row, and
the next claim would 404 back to v3. So the announcement follows PROOF — a fresh agent advertises
nothing until its first served v4 claim, a 404 withdraws the advertisement in the same statement
that starts the cooldown, and the recovery probe carries `X-Cerbix-Ledger` because the header
follows the ATTEMPTED endpoint rather than the published readiness. And **test RPC stays on the envelope-derived generation**, because
B1 ships no v4 test endpoint — the job and test paths are now derived separately so a future
generation cannot drag one along with the other.

**Mutations planted and killed: 25.**

1. Remove the `s.ledgerCarrier` guard from the admission block — the announcing region is promoted to 4 with the flag off, and all three transport cases fail.
2. Plant `_ "internal/store"` in `internal/worker` — the import scan names the file and the path.
3. Coerce the config value to false instead of refusing — every inertness assertion still passes, and only the unmodified-value assertion catches it.
4. Accept `ledger.carrier_enabled: true` — the refusal test fails on the first claim.
5. Map generation 4 to the v3 queue — the distinct-queue assertion fails, because a shared queue is a filter, not isolation.
6. Let an unmapped generation fall back to the v1 queue — the closed-boundary assertion fails.
7. Gate `agentJobsV4` on the credential capability instead of the ledger one — the "credential-envelope-is-not-a-ledger-declaration" case is accepted.
8. Give `ClaimPullJobsV4` the v3 ceiling — the converse claim returns nothing, so exclusion can no longer be distinguished from an empty table.
9. Add `Ack` to `Dispatcher` — the enumerated method SET fails and names it.
10. Announce the ledger capability but keep claiming v3 — the claim path assertion fails; this is the P0 as it was submitted.
11. Drop `X-Cerbix-Ledger` from the claim — the per-request declaration is gone and the header assertion fails.
12. Remove the 404 downgrade — an agent newer than its core stops claiming entirely.
13. Announce the ledger capability from an agent that cannot open envelope v2 — it would claim rows it cannot read.
14. Derive the TEST path from the job generation — test RPC is redirected to `/v4/tests`, which does not exist.
15. Make the downgrade permanent — a core upgraded underneath the agent is never found again.
16. Announce as soon as the cooldown expires, before any successful probe — the periodic false announcement.
17. Leave the announcement standing after a 404 — core rolled back under a proven agent keeps selecting rows it will not claim. This one SURVIVED at first: every test had 404'd an agent that never proved the endpoint, so clearing the proof was untested until the rollback case was written.
18. Stop declaring `X-Cerbix-Ledger` on the recovery probe — the endpoint refuses it and the agent concludes it is absent when it is merely undeclared.
19. Let the announcement ignore the downgrade state — the agent advertises a carrier it is not currently claiming, which is [428] restated.
20. Drop the envelope floor from `agentJobsV4` — a claim returning generation-3 rows is served to an agent that declared no envelope capability.
21. Route generation 4 to the v3 queue on a LIVE broker — the v3 consumer receives it, which the mapping test alone could not show.
22. Bind the v4 queue unconditionally — the same delivery reaches a consumer that declared nothing.
23. Stop the worker announcing the ledger carrier — `LiveLedgerJobRegions` can never be true and the region is never eligible.
24. Make the CLAIM header a floor rather than exact — `X-Cerbix-Ledger: 2` is accepted for a capability that does not exist.
25. Accept any integer as `capabilities.ledger` — it is persisted into an existential query, so a value nobody defined becomes a silent yes for a whole region.

**Why each proof pairs with its converse.** Every inertness assertion here has a partner that shows
the mechanism can fire: the flag ON raises the announced region, the v4 claim reaches the row a v3
claim cannot, and a declared ledger capability is accepted. Without them, deleting the feature
entirely would leave every "it does not happen" test green.

## 18. Open items

- ~~Participation scope is unresolved~~ — **RULED by the owner on 2026-09-04: all monitors, 14-day
  retention** (§5.3). No participation flag exists as a result.
- The exact claim-message shape on the wire, and whether it travels the results queue or its own.
  §4a fixes that it uses the existing transport; which queue is a phase-C detail.
- Whether a window should record the region's live-executor state at `due_at`, so a missed run in a
  region with no worker reads differently from one in a healthy region. Attractive, unanalysed,
  deliberately out of revision 2.
- ~~Whether push monitors participate~~ — **RULED by the owner on 2026-09-04: excluded** (§15).
- ~~`due_at` semantics for a late run~~ — **RULED by the owner on 2026-09-04**: the window keeps its
  expectation as `due_at`, and a run late by more than one interval reads `covered_late` (§14.1).
- ~~The `generate_series` cap value has no analysis behind it~~ — **closed in revision 6**: the
  retention clip is now the rule and the cap is a bounded configuration valve (§9.3).

## 19. Process

This revision goes to design review. Its own mock if any UI surface grows beyond FR-031's existing
panel, and a migration plan. Migrations start at `00100`.
