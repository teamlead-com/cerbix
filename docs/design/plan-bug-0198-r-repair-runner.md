# BUG-0198-R — repair runner classification and throughput: reviewed plan (revision 7, DEFERRED)

**Status:** plan only, no code. Reviewed in seven rounds by three independent reviewers before any
implementation; revision 7 was approved by two of them, the third's last findings (D4 rationale,
T14e wording) are incorporated here. The owner **deferred** the iteration on 2026-10-17: a read-only
check of a deployed installation found no repair backlog and no false backoff, so the defects are
real (reproduced in tests with injected delays) but have no observed user impact yet. Pick this up
when an installation shows a real repair queue (`service_repair_ranges` with `attempts > 0`,
`released` churn, or a slow-draining cursor). The base it was written against is `cdeb1a2`; re-check
every file:line reference before implementing.

The problem it solves is the `BUG-0198-R` row in [`status.md`](../status.md):

1. **False faults.** A `statement_timeout` on a one-bucket FIRST batch is counted as a range fault
   (`attempts + 1`, backoff 5 s … 5 min), but the first batch does not always get a full slice — the
   last slice of a scheduler cycle can be as short as 140 ms, and a slow claim eats into a full one.
   Reproduced on both storage modes (iter-0198 re-review #3: 160 ms slice with a 75 ms bucket; 80 ms
   claim in a 250 ms slice).
2. **Throughput.** Since D-0267 every slice opens at one bucket and the learned size is lost between
   slices; where the per-batch persistence cost is high (45 ms per cursor write in re-review #4), a
   slice commits 3–5 buckets instead of 60.

## A. Constants and measurements

- **A1.** `MaxServiceSlice` = 250 ms moves to `store` (exported). The scheduler takes it from there.
  The scheduler comment and the `max_dispatch_delay` row of §10.10 are updated.
- **A2.** `W` — the batch's work budget:

      W = time.Until(deadline) − lifecycleReserve (40 ms) − advanceCommitReserve (60 ms)

  taken before `BEGIN`; the batch transaction's deadline is derived from the same instant. `W`
  covers `BEGIN`, `SET LOCAL`, the generation read, the bucket loop and `advanceSealedThrough`.
- **A3.** The persist phase (generation CAS, cursor write, `COMMIT`) gets everything left before the
  batch deadline: at least `advanceCommitReserve` plus the unspent part of `W`. It is not a "fixed
  persist timeout", and messages do not call it one.
- **A4.** `repairFaultWorkBudget` = 120 ms. The §10.10 definition of a fault: *the first batch of a
  slice, of one bucket, exhausted a work budget W ≥ 120 ms.* `W` covers one bucket's work TOGETHER
  with the batch's fixed part, so a fault means "the range does not fit a full work budget", not
  proof that the bucket alone is expensive. `last_error` names where the stop happened (before, in,
  after the loop) and the loop time. Declared grey zone: from (120 ms − overhead) to 150 ms.
- **A5.** Measurements, stored on the range row (section E) and in the runner's memory:
  - `ĉ` (`bucket_cost_us`) = bucket-loop time / canonical slots traversed, where slots =
    `(end − cursor) / CanonicalBucket` (not facts written — with no epoch that can be 0). Rounded up
    to the microsecond, minimum 1.
  - `F̂` (`batch_overhead_us`) = work time − loop time, minimum 0. `NULL` is read as 0 when sizing.
  - After a COMPLETED work phase (commit B2, or a first-batch persist fault B6): `ĉ`, `F̂` are
    assigned this batch's exact measurements.
  - After a work phase that STOPPED (B4, B5): upward only — `ĉ := max(ĉ, loop / (k + 1))` only when
    the stop was inside the loop (otherwise `ĉ` is unchanged); `F̂ := max(F̂, work − loop)`.
- **A6.** `probe_cap` (section E, `NULL` = no cap). Batch size:

      size       = floor((0.6·W − F̂) / ĉ)        ; size = 1 while ĉ is unknown
      slots_left = (range_end − cursor) / CanonicalBucket

  First batch of a slice: `b = min(clamp(size, 1, 1440), probe_cap, slots_left)` (`NULL` does not
  cap). A batch after progress: if `size < 1` the slice ends as a tail (B7) without running a doomed
  batch; otherwise `b = min(size, 1440, slots_left)` (`probe_cap` is already `NULL` after B2).
  `size` is recomputed before every batch from the current `W` and the in-memory `ĉ`/`F̂`. Below,
  `b` is always the batch's actual slot count after the `slots_left` cut.
- **A7.** The failure phase is marked at the call site by a wrapper, not inferred from the error
  value: work-before-loop, work-in-loop, work-after-loop, persist. `k` (slots fully traversed) and
  the loop time come back with the error.
- **A8.** The work → persist boundary: before switching to the persist phase, check
  `time.Until(batch deadline) ≥ advanceCommitReserve`; if false, it is a work-after-loop stop
  (`errSliceBudget`, `k = b`), not a persist one. Reason: `ensureBounds` (`deadlinetx.go`) reuses a
  server bound up to `boundedDriftMS` = 15 ms stale, so the work phase's last statement can run into
  the reserve. Cost (recorded in the decision): a batch that finished its loop but ran into the
  reserve inside the drift window is rolled back; at `b = 1` and `W ≥ 120` that is a B3 fault "after
  loop" — consistent with A4.
- **A9.** `repairDryStreakLimit` = 3, held in `LeaderSession` memory (section C).
- **A10.** `adaptRepairBatch` and its unit test are removed; a unit test of the size function
  replaces it. `repairBackoff` and `TestRepairBackoffIsBounded` stay, as the oracle for B12's SQL.

## B. Transitions

A slice starts after the claim, with `progressed = false`.

- **B1.** First batch, `W ≤ 0`: no batch is opened. Outcome "no progress".
- **B2.** A batch commits. ONE `UPDATE` in the batch transaction (the statement that already moves
  the cursor) writes the cursor, `ĉ`, `F̂`, `attempts = 0`, `probe_cap = NULL`. In memory:
  `r.Attempts = 0`, `ĉ`/`F̂` updated, `progressed = true`. Resetting `attempts` on progress stops the
  grey zone from escalating backoff across different buckets.
- **B3.** First batch, work stop (any kind), `b = 1`, `W ≥ 120` → fault: `attempts + 1`, backoff per
  B12, `probe_cap` unchanged, outcome `failed`. `last_error`: "range exhausted <W> ms of work budget
  at one bucket <start> (threshold 120 ms; slice <S> ms, claim <c> ms; loop <t> ms;
  <before|in|after> loop; <57014|slice budget>)".
- **B4.** First batch, work stop, `b = 1`, `W < 120` → "no progress"; estimates upward only (A5).
- **B5.** First batch, work stop, `b > 1` → "no progress"; estimates upward only;
  `probe_cap := max(1, floor(0.6·b))`. Property: the next slice's first batch is ≤ `probe_cap`
  whatever the measurements and `W`. Without a commit `probe_cap` only decreases (B1, B3, B4, B6 do
  not touch it; B8 writes through `COALESCE`; the merge in D takes `MIN`; a lost write leaves the old
  value). From 1440 to 1 takes at most ⌈ln 1440 / ln(5/3)⌉ = 15 successfully written B5 transitions
  (13 in practice). This bounds B5 transitions, not every growth: the actual `b` can move below the
  cap as `W` or the estimates change. It is not a number of scheduler slices nor a wall-clock bound.
- **B6.** Persist-phase stop. First batch (`progressed = false`): infrastructure fault —
  `attempts + 1`, backoff per B12, `ĉ`/`F̂` assigned from the completed work phase, `probe_cap`
  unchanged, outcome `failed_persist`, `last_error`: "persist phase did not finish in the remaining
  <ms> ms (slice <S> ms, claim <c> ms; <57014|slice budget>)". After progress: a tail (B7), this
  batch's `ĉ`/`F̂` are not written.
- **B7.** After progress — a work stop, a persist stop, `W ≤ 0` or `size < 1` — is the slice's tail:
  release at the cursor; `ĉ`/`F̂` are not written in the release.
- **B8.** Outcome "no progress" (B1, B4, B5): release at the cursor; `next_attempt_at = now()` (to
  the back of the claim order); `ĉ`, `F̂`, `probe_cap` written through `COALESCE`; outcome
  `released_no_progress`; `attempts` unchanged; the function returns `nil` and reports "no commit"
  to C. `last_error`: "no progress (slice <S> ms, claim <c> ms, forward <f> ms, work budget <W> ms,
  batch <b>, loop <t> ms, <stop kind>)" — S is the slice budget on entry to `RunServiceSlice`, c the
  claim time, f the time of an empty forced-forward before the fallback (0 if none), so the cause of
  a reduced `W` is read from data, not inferred. Outside `RunServiceSlice`: S is the deadline
  remainder on entry to the function that received it (`RunServiceRepairSlice` or `RunRepairRange`);
  c = 0 when this call did not claim; f = 0 outside C3.
- **B9.** `55P03` and other errors: the failure path, backoff per B12.
- **B10.** `ErrRangeSuperseded`: release as today, estimates untouched; for C it is a slice without a
  commit if none happened.
- **B11.** `ErrEvidenceGone` and complete: as today.
- **B12.** Backoff is computed in SQL from the stored counter, with the exponent clamped BEFORE the
  shift (otherwise `attempts ≥ 62` raises 22008 and larger values 22003):

      next_attempt_at = now() + make_interval(
          secs => LEAST(300, 5 * (1 << LEAST(GREATEST(attempts, 0), 6))))
      attempts = attempts + 1

  The right-hand side of `SET` reads `attempts` before the increment. Values 0→5 s, 1→10, 2→20,
  3→40, 4→80, 5→160, ≥6→300 s — equal to `repairBackoff(attempts + 1)`. The claim-time copy
  `r.Attempts` no longer feeds the backoff (it is stale after B2). No branch passes a `nil` cause.
- **B13.** The cursor in fail and release `UPDATE`s is `cursor_at = GREATEST(cursor_at, $cursor)`:
  an ambiguous `COMMIT` (the database committed the batch and the new cursor, the client got an
  error and holds the old local cursor) must not move the cursor back.
- **B14.** Positive transitions are atomic with the batch commit. Negative ones can be lost when the
  closing write is refused by its deadline: the range comes back through its 60 s lease with the old
  values. The guarantees of B5 and C count successfully written transitions; there is no wall-clock
  bound under an arbitrary number of lost writes.

## C. Fairness to the forward pass (queue level)

- **C1.** `RunServiceRepairSlice` returns a separate "at least one batch committed" signal,
  independent of `err` and `worked`.
- **C2.** `LeaderSession.dryStreak`, updated BEFORE returning a result or an error to the scheduler:
  +1 when a range was claimed and no batch committed (including B3, B6, B9, B10 and errors); +1 when
  the claim itself returned an error; = 0 when a batch committed in the slice, even if an error
  followed; unchanged on a successful empty claim.
- **C3.** `RunServiceSlice`: (1) the existing short-tail guard (`leadership.go`); if too little is
  left, `return false, nil` as today. (2) If `dryStreak ≥ 3`: `dryStreak = 0` and
  `AdvanceServiceMaterializationOn` with the same absolute deadline — `worked` or `err` is returned;
  `worked = false, err = nil` (forward has nothing to do) re-runs the guard on the current remainder
  and, if enough is left, falls back to repair in the same slice with the same deadline. (3)
  Repair-first as today. After an empty forward: a fallback that claimed a range without a commit, or
  whose claim failed, ends the slice with `dryStreak = 1`; an empty fallback claim leaves it at 0.
- **C4.** Guarantee: in a stable `LeaderSession`, while repair commits nothing, forward gets an
  ATTEMPT (a call with the slice's budget) at least once every four slices that passed the guard. If
  forward has no work, the slice goes back to repair. It guarantees the opportunity, not forward
  progress. Each leader change can add up to three extra dry slices. A backlog that makes progress
  still blocks forward by design (repair first), as today. Per-range backoff could not provide this:
  with several non-progressing ranges the queue stays runnable and forward never runs (reproduced on
  both modes with eight ranges).

## D. Coalescing in `enqueueRepairRangeTx`

- **D1.** One statement:

      WITH gone AS (
          DELETE FROM service_repair_ranges
           WHERE service_id = $1 AND reason = $2 AND state = 'pending'
             AND range_start <= $4 AND range_end >= $3
          RETURNING *)
      INSERT INTO service_repair_ranges (...) SELECT ... FROM gone

  Bounds, provenance and inherited fields come from exactly the deleted rows (today's form —
  aggregate `SELECT`, then `DELETE`, then `INSERT` — loses a backoff written between the read and the
  delete). A concurrently claimed (`running`) row is not deleted and does not take part. The
  generation is read by a subquery inside the `INSERT`.
- **D2.** Aggregates over `gone`, including an empty `gone`:
  `range_start = LEAST($3, MIN(range_start))`, `range_end = GREATEST($4, MAX(range_end))`;
  `bucket_cost_us`, `batch_overhead_us` = `MAX` of non-null; `probe_cap` = `MIN` of non-null;
  `attempts = COALESCE(MAX(attempts), 0)`; `next_attempt_at = GREATEST(now(), MAX(next_attempt_at))`
  (today a merge erases a backoff — an existing defect); `last_error` from the row with the greatest
  `(updated_at, id)`, else `''`. The origin is passed as a TYPED nullable `uuid` (the Go side passes
  `nil` for ""), and SQL never casts an empty string to `uuid`:

      CASE WHEN $origin::uuid IS NOT NULL
            AND COALESCE(BOOL_AND(origin_id IS NOT DISTINCT FROM $origin::uuid), true)
           THEN $origin::uuid END

  The aggregate is evaluated over all of `gone` before the outer `CASE`, so a text parameter with
  `$origin <> ''` and `$origin::uuid` evaluates `''::uuid` on a generic plan and fails with 22P02 —
  after five executions on a connection under pgx's default `QueryExecModeCacheStatement` (reviewers
  reproduced it on the sixth). The origin survives only if EVERY absorbed row has the same one, as
  today.
- **D3.** A new cheap head may wait out an inherited backoff of up to 5 minutes — an accepted
  conservative policy; the first commit resets `attempts`.
- **D4.** Accepted policy: duplicates are allowed. Two CONCURRENT enqueues overlapping one pending
  row are not serialized: the second waits on the first's delete, then sees an empty `gone` and
  inserts its own row with initial values. Consequences, and nothing more is promised: at most one
  extra row per such race; it lives on its own schedule (its own `attempts` and B12 backoff, none on
  "no progress"), so while the original waits out a 5-minute backoff the duplicate may make about
  six attempts; it is absorbed only by a later enqueue that overlaps IT while it is pending; the work
  is idempotent. Why not serialize: a per-service advisory lock inside enqueue is NOT safe. Ingest
  locks one service's buckets one by one (`markBucket`) and calls enqueue between them
  (`repairIfBehindWatermark` → `enqueueRepairRangeTx`); with buckets a < b, T1 holds a and takes the
  service lock, T2 holds b and waits for it, T1 moves to b and waits for T2 — a cycle. A reviewer
  reproduced 40P01 on both modes through `RecordHistoricalResults`; without the lock both batches
  commit. Sorting by `(service_id, bucket_start)` does not prevent it, because the lock would be
  taken after the first bucket and before the next. Safe serialization needs its own analysis of the
  whole ingest transaction's lock order and is neither introduced nor recommended here. The
  duplicate policy is best effort: backoff is not guaranteed globally for all overlapping work.

## E. Migration (next free number)

    ALTER TABLE service_repair_ranges
      ADD COLUMN bucket_cost_us    bigint NULL CHECK (bucket_cost_us > 0),
      ADD COLUMN batch_overhead_us bigint NULL CHECK (batch_overhead_us >= 0),
      ADD COLUMN probe_cap         int    NULL CHECK (probe_cap >= 1);

No heap rewrite; the `CHECK`s validate existing rows by a scan under a brief `ACCESS EXCLUSIVE` lock
on `service_repair_ranges`. Applied at startup by any role; an old binary does not name the columns.
Down drops them. No upper bound of 1440 in the schema — the clamp lives in code.

## F. Observability and documents

- New outcomes `released_no_progress` and `failed_persist` in
  `cerbix_service_repair_outcomes_total`, counted only after the lifecycle write committed.
- The `last_error` texts of B3, B6, B8 distinguish a range fault, a persist fault and a reduced work
  budget.
- Runbook: how to tell those apart. The outcome counter has no range label, so localization is an
  SQL query over `service_repair_ranges` (`last_error`, `cursor_at`, `next_attempt_at`, the new
  columns, the lease). The cause of a reduced `W` is read from B8's `last_error` (S, c, f); without
  those values no cause is asserted.
- §10.10: the "Batch size adapts … targeting roughly 60% … shrinking on a timeout" bullet is
  rewritten for A6/B5; 120 ms, 3 and 0.6 are added to the bounds table as compile-time constants.
- A decision record (with the cost of A8, the D4 policy and the ambiguous-COMMIT consequence of
  T14g), the iteration report, `status.md`, `traceability.md`, a CHANGELOG entry with upgrade notes.

## Tests

General rules: RED first, then GREEN, on both storage modes. Bucket and cursor delays by `pg_sleep`
triggers; `errSliceBudget` at a chosen point deterministically through `deadlineTxStatementHook`.
Phase and stop kind are proven by assertions on `last_error`. A test that does not reach its
mechanism fails as a setup error. An internal seam `repairBatchObserved(W, b, phase, k, loop)`
records every batch.

- **T1.** RR3-1 (a): 160 ms slice, 75 ms bucket → `released_no_progress`, `attempts = 0`; the next
  full slice commits.
- **T2.** RR3-1 (b): +80 ms on `pending → running` only, 250 ms slice → no fault.
- **T3.** Boundary: claim +20 ms, 125 ms bucket → a fault per A4, then a fresh slice succeeds. A 1 s
  bucket with `W ≥ 120` → a fault.
- **T4.** Classification matrix: {57014, errSliceBudget} × {before, in, after the loop} at `b = 1`,
  `W ≥ 120` (B3) and at `b > 1` (B5); persist with `progressed = false` (B6) and after progress
  (B7); old `ĉ`/`F̂` set different from what is measured, exact new values asserted.
- **T5.** Eight ranges, claim +80 ms, 1 s buckets: within five cycles a prepared due service's
  `materialized_through` advances. Positive control.
- **T6.** `bucket_cost_us` = 1 µs, 5 ms buckets, a range of ≥ 1440 slots; assertions from the seam:
  slice 1 — actual `b = 1440`, stop in the loop (B5), stored `ĉ == ceil(loop / (k + 1))`,
  `probe_cap = 864`; slice 2 — first `b == min(size(ĉ, F̂, W), 864)` and it commits;
  `attempts = 0` throughout.
- **T7.** Throughput by mechanism: 4 ms per bucket, 45 ms per cursor write, a +20 ms hook on the
  generation read. (a) after slice 1, `ĉ < 20 ms` and `F̂ ≥ 20 ms`; (b) slice 2's first batch
  `b == size(ĉ, F̂, W)` from the seam and `b ≥ 2`; (c) a mutant without carry-over fails (b); (d) a
  tail after progress does not change `ĉ`/`F̂`. The expected 2–3× speed-up is a measurement for the
  report, not a test assertion.
- **T8.** Atomicity: progress → closing release not written → lease expired → claimed by another
  `LeaderSession`: cursor, `ĉ`, `F̂`, `attempts = 0` intact. No progress → release lost → reclaim:
  old values (the new measurement deliberately different from the old one).
- **T9.** Coalescing: (a) sequential — an absorbed pending row (cost 5000, overhead 3000, cap 7,
  attempts 3, `next_attempt_at` = now + 5 min, `last_error` X) yields a row with those values,
  not claimable before `next_attempt_at`; (b) concurrent claim and fail by another connection during
  enqueue — the backoff is not lost; (c) enqueue against enqueue (D4), the second `DELETE`'s wait
  confirmed by `pg_blocking_pids` — the original row with its backoff plus one duplicate with
  `attempts = 0`, claimable independently; after that check the duplicate is put back to `pending`;
  an enqueue overlapping only the original does not absorb it, one overlapping both absorbs both;
  (d) origin — caller without / with origin × empty `gone` / same / foreign / `NULL` origin, run on a
  pinned connection under `SET plan_cache_mode = force_generic_plan` AND separately ≥ 6 times on one
  connection without it (the product's real path: `store.Open` sets no `QueryExecMode`).
- **T10.** Denominator: no epoch (0 facts written, ≥ 1 slot) and a last partial batch.
- **T11.** Backoff: stored `attempts = 6` → commit → `55P03` in the same slice: `attempts = 1` and
  `next_attempt_at − updated_at` exactly 5 s (one `now()` in one `UPDATE`); a table of `attempts`
  before the update → delay: 0→5, 1→10, 2→20, 3→40, 4→80, 5→160, 6→300, 7→300, 29→300, 32→300,
  1000→300 s, checked against `repairBackoff(attempts + 1)`. 29 and 32 catch mutant 11 (no exponent
  clamp: 29 → 22003, 32 → 5 s).
- **T12.** Migration up and down on both modes.
- **T13.** Existing tests: `TestRepairTimeoutIsAReleaseAfterProgressAndAFaultOnAFreshBucket` changes
  by contract (slice 1 is B5, slice 2 a fault at `b = 1`; its guard deadline clearly full,
  `MaxServiceSlice + lifecycleWriteBound`); `TestLongRepairProgressesUnderProductionSlices` and
  `TestRepairTailTimeoutOnOneBucketIsNotAFault` keep their invariants or the change is argued in the
  report.
- **T14.** New: (a) a stop before the loop (`k = 0`) and after it (`advanceSealedThrough`, `k = b`)
  at `b > 1` — `probe_cap` decreases, convergence; (b) a short `W` then a full one — first batch ≤
  `probe_cap`; (c) A8 — work ran into the reserve → `k = b` and "after loop", not persist; (d)
  rotation — after B8 on the oldest range the next claim picks another waiting range; (e)
  `dryStreak ≥ 3` with no forward work → repair in the same slice, three variants with their
  expected return (fallback claimed without a commit → `true, nil`, the cycle continues,
  `dryStreak = 1`; empty fallback claim → `false, nil`, the cycle ends normally, `dryStreak = 0`; an
  empty forward that spent most of the slice → the guard again, no claim, `false, nil`); (f)
  `dryStreak` on errors — claim error (+1), superseded, a commit followed by a closing failure (= 0);
  (g) ambiguous `COMMIT` through an internal `deadlineTxCommitHook` that returns an error after the
  REAL commit, then a closing `UPDATE` with the old local cursor — the durable cursor did not move
  back. Consequence for the decision record: such a first batch goes through B6 — one false
  `failed_persist` and one backoff for a batch that did commit; the next commit resets `attempts`.

Mutations, each caught by at least one test: the 120 threshold removed; `errSliceBudget` not
classified; a persist stop classified as work; `ĉ` includes fixed costs; no carry-over of `ĉ`/`F̂`;
`probe_cap` not written; `ĉ` lowered on a stop; denominator = facts written; `attempts` not reset;
backoff from the claim copy; B12's exponent unclamped; `next_attempt_at` not updated in B8;
`dryStreak` not applied; a claim error not counted; forced-forward without the repair fallback;
fallback without the second guard; the merge not inheriting `next_attempt_at`; the merge through a
separate `SELECT`; the newest row's origin put on the union; a tail changing `ĉ`; the A8 check
removed; `GREATEST(cursor_at, $cursor)` replaced by `$cursor`; the origin as text `''` cast in SQL;
an empty fallback claim incrementing `dryStreak`.

Gates: `go vet`, `make docs-check`, full `-race` on both modes, `make dev-test`.
