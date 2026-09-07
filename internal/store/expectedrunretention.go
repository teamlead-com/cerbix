package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/metrics"
)

// FR-032 phase D — retention, and `ledger_from`: what a surface may claim about a span whose
// windows have been dropped.
//
// §3.4 made retention semantics one of the four facts a solution must carry, and the reason is the
// direction: a dropped span must read as NOTHING — not `covered`, and not `expected_never_issued`
// either. A ledger that forgot a span and then answered for it would be over-claiming through the
// one mechanism nobody looks at.

// expectedRunPartitionNames lists the DATED partitions this store manages, mirroring
// `heartbeatPartitionNames`. The catalog is the source and the regex is the filter, so an
// operator's own partition — or the DEFAULT one — is never dropped by this pass.
func (s *Store) expectedRunPartitionNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'expected_runs' AND c.relname ~ '^expected_runs_p[0-9]{8}$'
		 ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("store: list expected-run partitions: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: scan expected-run partition: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate expected-run partitions: %w", err)
	}
	return names, nil
}

// PurgeOldExpectedRuns drops every dated partition whose whole range is before the retention
// cutoff, and deletes rows that leaked into the DEFAULT partition and are now past it.
//
// The default half is what the gate ledger's retention never has to do, and §12.3 says why the two
// diverge: `expected_runs` HAS a default partition because a lost insert would erase the very fact
// the ledger keeps, and having one means retention must reach into it. A pass that dropped only
// dated partitions would leave the default growing forever — the exact failure `EnsureServiceFactPartitions`
// records for the service facts, one table over.
//
// There is no TimescaleDB branch, and that is not an omission: `expected_runs` is declaratively
// partitioned in BOTH storage modes, so `drop_chunks` has nothing to drop and this is the only
// mechanism. Branching on `s.timescale` here would create a second retention path whose absence of
// coverage nobody would notice.
func (s *Store) PurgeOldExpectedRuns(ctx context.Context, cutoff time.Time) (int, error) {
	cutoff = cutoff.UTC()
	names, err := s.expectedRunPartitionNames(ctx)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, name := range names {
		day, err := time.ParseInLocation("20060102", strings.TrimPrefix(name, expectedRunPartitionPrefix), time.UTC)
		if err != nil {
			continue // not a dated partition we manage
		}
		// The partition covers [day, day+1), so it is dropped only once its WHOLE range is before
		// the cutoff. Dropping one that still holds answerable windows would move `ledger_from`
		// forward past evidence that exists, which is withholding — safe, and still wrong.
		if !day.AddDate(0, 0, 1).After(cutoff) {
			if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
				return dropped, fmt.Errorf("store: drop expected-run partition %s: %w", name, err)
			}
			dropped++
		}
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM expected_runs_default WHERE due_at < $1`, cutoff); err != nil {
		return dropped, fmt.Errorf("store: purge expected-run default partition: %w", err)
	}
	return dropped, nil
}

// LedgerFrom is the earliest instant a monitor can be answered for (§12.3).
//
// COMPUTED, never stored, and the reason is a defect found while fixing the P0 at party [225]:
// revision 2 had it as a stored NOT NULL column AND as a formula, and nothing in the design updated
// the column when a partition was dropped — so the two would have drifted apart in the direction of
// OVER-claiming. Storing it would also have contradicted invariant 19.
//
// Four inputs, and the answer is the LATEST of them, because each is a separate reason the ledger
// cannot speak for earlier time:
//
//   - the oldest retained partition's lower bound — before it, the windows were dropped;
//   - the earliest `effective_from` in the monitor's revision timeline — before it, no configuration
//     generation is describable, so a window's interval and retry count cannot be attributed (§6.3);
//   - `schedule_created_at` — before it, nothing recorded an expectation at all;
//   - `gap_truncated_before` — the fence §7.1 wrote when a cap or a clip stopped it materializing
//     windows it had advanced past.
//
// A monitor with NO schedule row has no answerable time at all and returns `ok == false`: it does
// not participate (a push monitor), or it was disabled, and either way a caller must render the
// whole range as not stored rather than as an empty answer.
func (s *Store) LedgerFrom(ctx context.Context, projectID, monitorID string) (time.Time, bool, error) {
	var (
		scheduleCreated time.Time
		fence           *time.Time
		earliestRev     *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT s.schedule_created_at, s.gap_truncated_before,
		       (SELECT min(r.effective_from) FROM monitor_execution_revisions r
		         WHERE r.monitor_id = s.monitor_id AND r.project_id = s.project_id)
		  FROM monitor_schedule s
		 WHERE s.monitor_id = $1 AND s.project_id = $2`,
		monitorID, projectID).Scan(&scheduleCreated, &fence, &earliestRev)
	if noRows(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: ledger_from: %w", err)
	}
	// The partition floor is a property of the TABLE rather than of the monitor, and it is read
	// here rather than cached because dropping a partition must move this answer immediately —
	// that is the whole reason the value is computed (invariant 24c).
	floor, err := s.expectedRunRetainedFloor(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	from := scheduleCreated
	for _, candidate := range []*time.Time{fence, earliestRev, floor} {
		if candidate != nil && candidate.After(from) {
			from = *candidate
		}
	}
	return from.UTC(), true, nil
}

// expectedRunRetainedFloor is the earliest instant the table can still answer for: the lower bound
// of the oldest DATED partition, pulled back to the oldest row in the DEFAULT partition when one is
// older. Nil when the table holds nothing.
//
// C6. The default partition used to be skipped, on the reasoning that it "has no lower bound — that
// is what makes it the default — so a row sitting in it says nothing about how far back the table
// can answer, and treating its contents as a floor would let one stray row claim the whole of
// time". The first half is true of the partition's DEFINITION and false of its CONTENTS, and the
// consequence was a contradiction an operator could see on one screen.
//
// The shape that produces it: an instance whose partition maintenance has not run for longer than
// its lead. On restart the gap machinery materializes the windows it advanced past, and every one
// whose day has no partition lands in the default. Those rows are stored, and `ListExpectedRuns`
// reads `FROM expected_runs`, so they are LISTED — while the floor, computed from the oldest dated
// partition alone, jumped forward and declared their whole span unanswerable. The read API returned
// rows for a span `ledger_from` said the ledger could not speak for.
//
// So it is the oldest ROW and never "the whole of time": a span with no rows in it is not claimed
// by this. The residual is named rather than left implicit — a single stray row far in the past
// does pull the floor back to itself, and the empty span between it and the dated partitions then
// renders as never-issued rather than as not-stored. That row is already listed by the read API, so
// the alternative is not "no claim" but "two answers", and one of them was provably wrong.
//
// The cost, stated because this runs on every monitor-detail read: `min(due_at)` over the default
// partition is a sequential scan, since the local primary-key index leads with `monitor_id`. The
// partition is empty in the ordinary case — retention deletes from it by name — and bounded by the
// retention window when it is not.
func (s *Store) expectedRunRetainedFloor(ctx context.Context) (*time.Time, error) {
	names, err := s.expectedRunPartitionNames(ctx)
	if err != nil {
		return nil, err
	}
	var oldest *time.Time
	for _, name := range names {
		day, err := time.ParseInLocation("20060102", strings.TrimPrefix(name, expectedRunPartitionPrefix), time.UTC)
		if err != nil {
			continue
		}
		if oldest == nil || day.Before(*oldest) {
			d := day
			oldest = &d
		}
	}
	var defaultOldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(due_at) FROM expected_runs_default`).Scan(&defaultOldest); err != nil {
		return nil, fmt.Errorf("store: expected-run default partition floor: %w", err)
	}
	if defaultOldest != nil && (oldest == nil || defaultOldest.Before(*oldest)) {
		d := defaultOldest.UTC()
		oldest = &d
	}
	return oldest, nil
}

// ExpectedRunHOTRatio sums `pg_stat_user_tables` across the RETAINED partitions.
//
// ONE unlabelled gauge over the whole retention window, and no partition label: partition names are
// unbounded over time, so labelling by them is exactly the high-cardinality mistake §11 names. The
// DEFAULT partition is included, because rows really do live there and a ratio that ignored them
// would describe a table nobody has.
//
// No `pg_stat_user_tables` metric existed in this repository before — `pg_stat_activity` is read
// once, in `gatemaintenance.go` — so this is new work rather than a precedent being followed.
func (s *Store) ExpectedRunHOTRatio(ctx context.Context) (metrics.ExpectedRunHOTStat, error) {
	var stat metrics.ExpectedRunHOTStat
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(t.n_tup_upd), 0), COALESCE(sum(t.n_tup_hot_upd), 0)
		  FROM pg_stat_user_tables t
		  JOIN pg_class c ON c.relname = t.relname AND c.relnamespace = t.schemaname::regnamespace
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'expected_runs'`).Scan(&stat.Updates, &stat.HOTUpdates)
	if err != nil {
		return metrics.ExpectedRunHOTStat{}, fmt.Errorf("store: expected-run hot ratio: %w", err)
	}
	stat.Defined = stat.Updates > 0
	return stat, nil
}
