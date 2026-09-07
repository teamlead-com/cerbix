package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// FR-032 §10 — the configuration boundary.
//
// A window's spacing depends on the interval in force, and a configuration change can land while
// the scheduler leader is absent, because the API role is a different process. If a gap spanned a
// revision change, §7.1's generate_series would space the WHOLE gap at the pre-change interval. So
// no gap is allowed to span a revision change (invariant 14): the transaction that bumps
// `monitors.execution_revision` closes the open window segment at the OLD interval, and only then
// records the new configuration on the schedule row.
//
// `next_due_at` is NOT a function of the config write, and that is required rather than incidental
// (invariant 14a). `now`, `now + new interval` and an old due rescaled to the new interval each
// move the probe instant differently, and invariant 1 promises that no monitor's probe instant
// changes. It is also what the code already does: a config write does not touch the leader's
// in-memory nextRun, so the pending probe fires when it was already going to and the new interval
// governs from the FOLLOWING advance. The column is absent from the SET list below, so the
// guarantee is structural and not a predicate someone can relax.

// closeScheduleSegmentSQL closes the open window segment and records the new configuration.
//
// $1 monitor_id[]  $2 cap  $3 retention_days
//
// The change instant is `statement_timestamp()` and NOT a parameter, deliberately. It bounds the
// materialized segment AND stamps `updated_at`, so a caller-supplied instant would let the two
// disagree — by milliseconds on one host, and by however far the API role's clock trails the
// database's in a distributed deployment. This ledger's whole discipline is that its instants come
// from ONE clock; the advance in §7.1 keeps the leader's, because the value it persists is the one
// the leader already computed, and this write keeps the database's, because nothing here was
// computed anywhere else.
//
// The materialization range is HALF-OPEN, `(next_due_at, change_instant)`, and the exclusion of
// the standing `next_due_at` window is load-bearing (invariant 14b). Revision 5 said "from
// next_due_at" inclusively while also saying the standing window takes the NEW revision, and when
// `next_due_at` is already past during a leader absence the config write would have created that
// row itself — §7.1's `current_window` insert would then collide with it (reviewer P1-2 at party
// [231]). The standing expectation has exactly ONE writer: §7.1, when it is finally acted on.
var closeScheduleSegmentSQL = `
WITH picked AS (
    SELECT s.monitor_id, s.project_id,
           -- The segment's windows are attributed to the generation that was live WHILE nothing
           -- ran, which is the schedule's revision and not the one this transaction just created.
           s.execution_revision AS schedule_revision, m.region AS monitor_region,
           s.interval_in_force,
           s.next_due_at + make_interval(secs => s.interval_in_force) AS first_expected,
           -- An acceleration cannot survive a configuration that HAS no acceleration, so the flag
           -- is cleared when the new configuration cannot confirm at all. It is the only thing
           -- about the pair this write touches: "interval_in_force" describes the instant
           -- "next_due_at" already holds, and this write leaves that instant alone, so it must
           -- leave the interval alone too. §10 step 3 said to set it from the new configuration
           -- and that was an over-claim — a 60s→300s edit stamped a 300-second lateness threshold
           -- on a window spaced at 60, so a run four minutes late read "covered".
           (s.confirm_phase AND m.confirm_interval_seconds > 0 AND m.failure_threshold > 1)
               AS new_confirm_phase,
           m.execution_revision AS new_revision
      FROM monitor_schedule s
      JOIN monitors m ON m.id = s.monitor_id AND m.project_id = s.project_id
     WHERE s.monitor_id = ANY($1::uuid[])
     -- The race with §7.1 is settled by the LOCK, not by ordering. Both statements take FOR UPDATE
     -- on the same monitor_schedule row, so they serialize: if §7.1 commits first this segment
     -- close sees the already-advanced next_due_at and materializes nothing; if this commits first
     -- §7.1 still reads the interval that produced the instant it is fencing against, because
     -- this write does not touch it. Both orders leave the same invariants true, which is why no
     -- ordering is prescribed.
     ORDER BY s.monitor_id
       FOR UPDATE OF s
),` + expectedRunGapCTESQL("statement_timestamp()", "$2", "(statement_timestamp() - make_interval(days => $3))") + `
UPDATE monitor_schedule s
   SET confirm_phase        = p.new_confirm_phase,
       execution_revision   = p.new_revision,
       gap_truncated_before = GREATEST(s.gap_truncated_before, f.truncated_before),
       updated_at           = statement_timestamp()
  FROM picked p
  LEFT JOIN fence f ON f.monitor_id = p.monitor_id
 WHERE s.monitor_id = p.monitor_id`

// syncMonitorScheduleTx is the ledger's half of every configuration write, and it must be called
// from the SAME transaction that creates the generation.
//
// It is deliberately paired with `writeRevisionTimeline` at the same FIVE sites — the four
// `revisionFenceSetSQL` bumps plus `insertMonitorTx` — because both answer the same question about
// one write: phase A records what the new generation IS, and this records what the OLD one covered
// and what the new one expects. A source scan holds the pairing
// (`TestEveryGenerationCreatingSiteSyncsTheSchedule`), for the reason phase A's guard exists: the compiler
// cannot see a missing statement, and a gap that spans a revision change is invisible and
// permanent.
//
// Three things happen, in this order, and the order is the specification:
//
//  1. The open segment is CLOSED at the old interval — the one that produced the standing
//     `next_due_at` — and neither that instant nor the interval describing it is touched.
//  2. Rows for monitors that no longer participate are DELETED. Disabling therefore closes the
//     segment and then stops expecting anything, which is §10's last paragraph.
//  3. Rows for monitors that newly participate are CREATED at `next_due_at = now`.
func syncMonitorScheduleTx(ctx context.Context, tx pgx.Tx, s *Store, projectID string, monitorIDs ...string) error {
	if len(monitorIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, closeScheduleSegmentSQL,
		monitorIDs, s.expectedRunGapWindowsMax(), s.ExpectedRunRetentionDays()); err != nil {
		return fmt.Errorf("store: close schedule segment: %w", err)
	}
	// A monitor that stopped participating expects nothing. The predicate is the NEGATION of the
	// one below and is written as such, so the two cannot describe overlapping or disjoint sets by
	// accident.
	if _, err := tx.Exec(ctx,
		`DELETE FROM monitor_schedule s
		  USING monitors m
		  WHERE s.monitor_id = m.id AND s.project_id = m.project_id
		    AND m.id = ANY($1::uuid[]) AND m.project_id = $2
		    AND NOT (`+scheduleParticipantSQL+`)`,
		monitorIDs, projectID); err != nil {
		return fmt.Errorf("store: delete monitor schedule: %w", err)
	}
	// A monitor that newly participates starts expecting from NOW. `schedule_created_at` bounds
	// `ledger_from`, so the span before this instant is claimable as nothing — never `covered`,
	// and never `expected_never_issued` either (invariant 15).
	//
	// `confirm_phase` is false and `interval_in_force` is the base interval, PROVABLY and not by
	// assumption: every path that can create a participant resets liveness (`status = 'pending'`,
	// `consecutive_failures = 0`) — UpdateMonitor's re-arm, RestoreMonitor, and creation itself —
	// and `InConfirmPhase()` requires status `up` with a non-zero failure count. There is no
	// predicate here to keep in step with the migration's backfill because there is no state to
	// read.
	if _, err := tx.Exec(ctx,
		`INSERT INTO monitor_schedule
		     (project_id, monitor_id, next_due_at, interval_in_force, confirm_phase,
		      execution_revision)
		 SELECT m.project_id, m.id, statement_timestamp(), m.interval_seconds, false,
		        m.execution_revision
		   FROM monitors m
		  WHERE m.id = ANY($1::uuid[]) AND m.project_id = $2 AND `+scheduleParticipantSQL+`
		 ON CONFLICT (monitor_id) DO NOTHING`,
		monitorIDs, projectID); err != nil {
		return fmt.Errorf("store: ensure monitor schedule: %w", err)
	}
	return nil
}

// scheduleParticipantSQL is the ONE expression of "this monitor expects runs".
//
// `MonitorType.Active()` is every valid type except `push`, so one condition carries both §15's
// push exclusion — the owner's ruling of 2026-09-04, because `checkStalePush` already detects and
// records a push that did not arrive and a window would be a second mechanism for one obligation —
// and the non-dispatchable-type rule the scheduler applies at its own loop head. The interval
// guard is not decoration: §7.1 spaces a gap with `make_interval(secs => interval_in_force)` and
// PostgreSQL raises on a zero step, so a monitor that somehow carries a zero interval must never
// acquire a schedule row rather than aborting a whole tick's advance later.
const scheduleParticipantSQL = `m.enabled AND m.type <> 'push' AND m.interval_seconds > 0`
