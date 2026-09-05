-- +goose Up
-- FR-032 phase B2 (D-0237): the fact that a run was EXPECTED.
--
-- cerbix records what happened. Until this migration it does not record what was SUPPOSED to
-- happen, so it cannot tell "no check was due here" from "a check was due here and never ran".
-- `heartbeats` is deliberately NOT touched (spec §4a): absence is the case this ledger exists to
-- preserve, and a row that only appears when a run produced a result cannot record a run that
-- produced none.
--
-- Every table carries `project_id` and the tenant-safe COMPOSITE foreign key this repository
-- already uses in five places (00060, 00061, 00064, 00080, 00100). A single-column reference would
-- let a row outlive its tenant's isolation boundary (invariant 26).

-- The durable expectation: one row per PARTICIPATING monitor. Push monitors are excluded by the
-- owner's ruling of 2026-09-04 (§15) — `checkStalePush` already detects and records a push that
-- did not arrive, and a ledger window would be a second mechanism for one obligation.
CREATE TABLE IF NOT EXISTS monitor_schedule (
    project_id            uuid        NOT NULL,
    monitor_id            uuid        PRIMARY KEY,
    next_due_at           timestamptz NOT NULL,
    -- The interval that PRODUCED next_due_at, in seconds. It is on the row because the confirm
    -- phase substitutes ConfirmInterval() for a run: reading the monitor's current interval would
    -- misdate every window probed under acceleration (§6.2).
    interval_in_force     int         NOT NULL,
    confirm_phase         boolean     NOT NULL DEFAULT false,
    execution_revision    bigint      NOT NULL,
    last_issued_at        timestamptz,
    schedule_created_at   timestamptz NOT NULL DEFAULT statement_timestamp(),
    -- Windows strictly before this were NOT materialized — the cap or the retention clip cut them
    -- (§9.3). It is an INPUT to ledger_from, so an unanswerable span is visible to every reader
    -- rather than silently claimable.
    gap_truncated_before  timestamptz,
    updated_at            timestamptz NOT NULL DEFAULT statement_timestamp(),
    -- Not in the spec's DDL and added deliberately. §7.1 spaces a gap with
    -- `generate_series(..., make_interval(secs => interval_in_force))`, and PostgreSQL raises
    -- `step size cannot equal zero` for a zero step — which would abort the whole tick's advance,
    -- for every monitor in the batch, on one bad row. Refusing the value at the write is the
    -- fail-closed direction; discovering it at dispatch time is not.
    CONSTRAINT monitor_schedule_interval_positive CHECK (interval_in_force > 0),
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE
);

-- One immutable row per due window. The WINDOW, not the job, is the identity: `due_at` comes from
-- the persisted `next_due_at`, which advances monotonically, so two concurrently outstanding runs
-- necessarily have DIFFERENT `due_at` — which is what makes overlap representable and is the direct
-- answer to the P0 that killed revision 1 (§5.2).
CREATE TABLE IF NOT EXISTS expected_runs (
    project_id         uuid        NOT NULL,
    monitor_id         uuid        NOT NULL,
    due_at             timestamptz NOT NULL,   -- the EXPECTED instant; the window's identity
    job_id             uuid,                   -- NULL: no job was ever issued for this window
    execution_revision bigint      NOT NULL,
    region             text        NOT NULL,
    -- The carrier the PUBLISHER selected, NULL when no job was issued. `DEFAULT 0` was wrong twice
    -- over (reviewer P0 at party [233]): it manufactured a value nobody set, and it made "never
    -- dispatched" indistinguishable from "dispatched on an old carrier".
    carrier_generation int,
    -- The interval that SPACED this window, so lateness needs no join (§14.1).
    interval_seconds   int         NOT NULL,
    -- True only on an orphan row, where the core did not know which interval spaced the window and
    -- used the conservative timeline minimum instead (§14.2). EXPLANATORY ONLY: invariant 20g
    -- forbids any verdict, gate or numerator from reading it as licence to promote a window.
    interval_assumed   boolean     NOT NULL DEFAULT false,
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
    CONSTRAINT expected_runs_carrier_iff_job
        CHECK ((job_id IS NULL) = (carrier_generation IS NULL)),
    -- Same reasoning as monitor_schedule's: a non-positive threshold makes the lateness test
    -- meaningless, and the conservative orphan minimum LEAST(interval, NULLIF(confirm, 0)) is
    -- exactly where a zero could have been introduced.
    CONSTRAINT expected_runs_interval_positive CHECK (interval_seconds > 0),
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE
) PARTITION BY RANGE (due_at);

-- The DEFAULT partition, following `heartbeats` (00017) and NOT the gate ledger, whose own tests
-- assert no DEFAULT partition exists anywhere in it. The reason is directional: a row here is
-- evidence that a run did not complete, and an insert lost to a missing partition would erase
-- exactly the fact the ledger exists to keep — silently, and toward OVER-claiming (invariant 16a).
--
-- `fillfactor` is set on the PARTITION and not only on the parent: in PostgreSQL storage parameters
-- are per-relation and `CREATE TABLE ... PARTITION OF` does not inherit them, so a `WITH` clause on
-- the partitioned parent would apply to nothing that ever holds a row (invariant 23a).
CREATE TABLE IF NOT EXISTS expected_runs_default PARTITION OF expected_runs DEFAULT
    WITH (fillfactor = 70);

-- The partial index exists for exactly one query: a terminal event whose `due_at` is not trusted.
-- Normally the result carries both `due_at` and `job_id`, so the write is a primary-key hit; this
-- is the fallback and stays out of the hot path. `claimed_at`, `terminal_at`, `outcome`,
-- `refused_at`, `refused_reason` and `skip_reason` appear in NO index, which is what makes an
-- update to them eligible for HOT (invariant 12).
CREATE INDEX IF NOT EXISTS expected_runs_job_idx ON expected_runs (monitor_id, job_id)
    WHERE job_id IS NOT NULL;

-- Backfill the expectation for every participating monitor. `next_due_at = statement_timestamp()`
-- is the only instant this row can honestly claim: nothing recorded an expectation before now, so
-- the ledger's history begins here and `schedule_created_at` bounds `ledger_from` (§12.3). No
-- window before migration time is ever `covered`.
--
-- The participation predicate is `enabled AND type <> 'push'`: `MonitorType.Active()` is every
-- valid type except `push`, so one condition expresses both §15's exclusion and the
-- non-dispatchable-type rule, and there is only ONE place it can drift from.
--
-- `confirm_phase` is derived from the monitor's CURRENT state here and never again: after this row
-- exists it is written only by the scheduler's confirm-acceleration statement (spec rule 5) and
-- READ by the configuration write, which must not recompute it. The first draft of this migration
-- spelled `InConfirmPhase()` out twice — once inside the CASE and once as the boolean — which is
-- the two-expressions-of-one-rule shape that has cost this design three revisions, so the
-- predicate is computed once in a LATERAL and used twice.
INSERT INTO monitor_schedule
    (project_id, monitor_id, next_due_at, interval_in_force, confirm_phase, execution_revision)
SELECT m.project_id, m.id, statement_timestamp(),
       CASE WHEN c.confirming THEN m.confirm_interval_seconds ELSE m.interval_seconds END,
       c.confirming,
       m.execution_revision
  FROM monitors m
  CROSS JOIN LATERAL (
      -- domain.Monitor.InConfirmPhase(): confirm CONFIGURED (a positive confirm interval, a
      -- threshold above one, and a type that can confirm at all) AND currently mid-confirmation
      -- (still up, at least one failure counted, no verdict reached).
      SELECT m.confirm_interval_seconds > 0
         AND m.failure_threshold > 1
         AND m.type <> 'composite'
         AND m.status = 'up'
         AND m.consecutive_failures > 0
         AND m.consecutive_failures < m.failure_threshold AS confirming
  ) c
 WHERE m.enabled AND m.type <> 'push' AND m.interval_seconds > 0
ON CONFLICT (monitor_id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS expected_runs;
DROP TABLE IF EXISTS monitor_schedule;
