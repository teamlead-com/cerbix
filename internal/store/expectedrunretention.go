package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
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

// expectedRunRetainedFloor is the lower bound of the oldest DATED partition still retained, or nil
// when none exists.
//
// The DEFAULT partition is deliberately not consulted. It has no lower bound — that is what makes
// it the default — so a row sitting in it says nothing about how far back the table can answer, and
// treating its contents as a floor would let one stray row claim the whole of time.
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
	return oldest, nil
}

// ErrExpectedRunCursorInvalid is a cursor that is not exactly what encodeExpectedRunCursor
// produces. Strict decode, no tolerance: a bad version prefix, bad base64 or an unparseable instant
// all fail the same way (§13a).
var ErrExpectedRunCursorInvalid = errors.New("store: expected-run cursor is invalid")

// expectedRunCursorVersion prefixes the encoded keyset so a LATER change to the cursor's shape is
// DETECTABLE rather than misread. §13a asks for exactly this, and the gate ledger's own cursor —
// which predates the rule — is the reason: it encodes `<micros>:<id>` with no version, so a future
// shape change there can only be discovered by a caller getting wrong pages.
const expectedRunCursorVersion = "v1"

// encodeExpectedRunCursor renders the keyset as URL-safe base64 of "v1:<due_at RFC3339Nano>".
//
// RFC3339Nano rather than the gate cursor's microseconds, because §13a specifies it and because the
// keyset is a timestamptz whose full precision is the thing being compared: truncating to
// microseconds would be safe TODAY (PostgreSQL stores microseconds) and would silently start
// dropping rows the day the column's precision changed.
//
// Exported alongside the decoder, and the symmetry is deliberate. A cursor is opaque to its CLIENT
// — the caller of the HTTP endpoint — and that opacity is a property of the wire, not of this
// package: `internal/store` is not a published surface, and the handler's own tests need to build
// the one shape the decoder must refuse to guess at. Keeping the encoder unexported would have
// left them constructing base64 by hand, which is a second copy of the format and exactly the
// drift the version prefix exists to make detectable.
func EncodeExpectedRunCursor(dueAt time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte(expectedRunCursorVersion + ":" + dueAt.UTC().Format(time.RFC3339Nano)))
}

// DecodeExpectedRunCursor parses an encoded cursor strictly.
func DecodeExpectedRunCursor(s string) (time.Time, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, ErrExpectedRunCursorInvalid
	}
	version, instant, ok := strings.Cut(string(raw), ":")
	if !ok || version != expectedRunCursorVersion {
		return time.Time{}, ErrExpectedRunCursorInvalid
	}
	at, err := time.Parse(time.RFC3339Nano, instant)
	if err != nil {
		return time.Time{}, ErrExpectedRunCursorInvalid
	}
	return at.UTC(), nil
}

// ExpectedRunPage is one page of a monitor's windows plus the two facts that bound what the answer
// MEANS (§13a, invariant 26b).
//
// `LedgerFrom` and `GapTruncatedBefore` travel with every page rather than being fetched separately,
// because a caller that has the rows and not the bounds can read an empty range as "nothing was due"
// when the truth is "the ledger cannot say".
type ExpectedRunPage struct {
	Windows            []domain.ExpectedRun
	LedgerFrom         time.Time
	HasLedgerFrom      bool
	GapTruncatedBefore *time.Time
	NextCursor         string
}

// ExpectedRunQuery is one page request over a monitor's windows.
type ExpectedRunQuery struct {
	ProjectID string
	MonitorID string
	From, To  time.Time
	Limit     int
	// CursorDueAt is the keyset of the LAST returned item; the next page is bound STRICTLY below
	// it, so a key returned once is never returned again (§13a).
	CursorDueAt *time.Time
}

// ListExpectedRuns returns one page of a monitor's windows in `due_at` DESC order.
//
// The project predicate is in the SQL and not only in the handler, and invariant 26a is explicit
// about why: a handler check alone is one refactor away from being bypassed, and a query that is
// safe only because of its caller is not safe. The composite `(monitor_id, project_id)` predicate
// here is the same shape as the composite foreign key that protects storage integrity — the two
// answer different questions and both are needed.
//
// No tiebreak is needed on the ORDER BY and the reason is structural: the query is monitor-scoped
// and the primary key is `(monitor_id, due_at)`, so `due_at` is unique within one monitor. If this
// is ever widened to project scope a `monitor_id` tiebreak becomes MANDATORY, because without one
// the keyset would start dropping rows.
func (s *Store) ListExpectedRuns(ctx context.Context, q ExpectedRunQuery) (ExpectedRunPage, error) {
	if q.ProjectID == "" || q.MonitorID == "" {
		return ExpectedRunPage{}, errors.New("store: expected-run query needs a project and a monitor")
	}
	if q.Limit < 1 {
		return ExpectedRunPage{}, fmt.Errorf("store: expected-run query limit must be positive, got %d", q.Limit)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT due_at, job_id::text, carrier_generation, execution_revision, region,
		       interval_seconds, interval_assumed, issued_at, claimed_at, terminal_at,
		       outcome, refused_at, refused_reason, skip_reason
		  FROM expected_runs
		 WHERE monitor_id = $1 AND project_id = $2
		   AND due_at >= $3 AND due_at < $4
		   AND ($5::timestamptz IS NULL OR due_at < $5)
		 ORDER BY due_at DESC
		 LIMIT $6`,
		q.MonitorID, q.ProjectID, q.From.UTC(), q.To.UTC(), q.CursorDueAt, q.Limit)
	if err != nil {
		return ExpectedRunPage{}, fmt.Errorf("store: list expected runs: %w", err)
	}
	defer rows.Close()
	page := ExpectedRunPage{Windows: make([]domain.ExpectedRun, 0, q.Limit)}
	for rows.Next() {
		var (
			w       domain.ExpectedRun
			jobID   *string
			carrier *int
			outcome *string
			reason  *string
			skip    *string
		)
		if err := rows.Scan(&w.DueAt, &jobID, &carrier, &w.ExecutionRevision, &w.Region,
			&w.IntervalSeconds, &w.IntervalAssumed, &w.IssuedAt, &w.ClaimedAt, &w.TerminalAt,
			&outcome, &w.RefusedAt, &reason, &skip); err != nil {
			return ExpectedRunPage{}, fmt.Errorf("store: scan expected run: %w", err)
		}
		w.MonitorID = q.MonitorID
		if jobID != nil {
			w.JobID = *jobID
		}
		if carrier != nil {
			w.CarrierGeneration = *carrier
		}
		if outcome != nil {
			w.Outcome = *outcome
		}
		if reason != nil {
			w.RefusedReason = *reason
		}
		if skip != nil {
			w.SkipReason = *skip
		}
		page.Windows = append(page.Windows, w)
	}
	if err := rows.Err(); err != nil {
		return ExpectedRunPage{}, fmt.Errorf("store: iterate expected runs: %w", err)
	}
	// The bounds travel with the page. A monitor with no schedule row answers for nothing, and the
	// caller renders the whole range as not stored rather than as an empty answer.
	from, ok, err := s.LedgerFrom(ctx, q.ProjectID, q.MonitorID)
	if err != nil {
		return ExpectedRunPage{}, err
	}
	page.LedgerFrom, page.HasLedgerFrom = from, ok
	if err := s.pool.QueryRow(ctx,
		`SELECT gap_truncated_before FROM monitor_schedule WHERE monitor_id = $1 AND project_id = $2`,
		q.MonitorID, q.ProjectID).Scan(&page.GapTruncatedBefore); err != nil && !noRows(err) {
		return ExpectedRunPage{}, fmt.Errorf("store: read truncation fence: %w", err)
	}
	// `next_cursor` is null on the LAST page, which is the convention the gate decision ledger
	// publishes: a full page means there may be more, and a short one is the end.
	if len(page.Windows) == q.Limit {
		page.NextCursor = EncodeExpectedRunCursor(page.Windows[len(page.Windows)-1].DueAt)
	}
	return page, nil
}

// ExpectedRunHOTSampleFloor is the number of updates below which the gate is not evaluated. Below
// it the sample says nothing, and a ratio computed from three updates would be noise presented as
// a measurement.
const ExpectedRunHOTSampleFloor = 1000

// ExpectedRunHOTThreshold is the ratio at or above which the current storage shape passes. Below
// it, §11's append-only variant is SELECTED rather than absorbed — the alternative exists and is
// specified precisely so a poor measurement has somewhere to go.
const ExpectedRunHOTThreshold = 0.90

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
