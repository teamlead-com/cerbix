package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 §13a — the expected-run READ API: its keyset cursor and the page it returns.
//
// G4: this half used to live in `expectedrunretention.go`, whose name describes one of the three
// things that file carried — retention, this cursor codec and pagination, and a metrics sampler.
// A file named for one of its subjects is a file whose other subjects are found by grep, and the
// two here share nothing but the table they read: retention decides what the ledger still HOLDS,
// and this decides what one caller may see of it.

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
		       outcome, refused_at, refused_reason, skip_reason,
		       -- Phase F (§7.4). Without these two the read path can never produce the RESERVED
		       -- verdict at all: the verdict function would see a job, no issue instant and no
		       -- claim, and answer issued_never_claimed — a publish this window cannot prove. A
		       -- state the store writes and the reader cannot see is the wiring boundary §17.12's
		       -- third finding was made of.
		       reserved_at, withheld_reason
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
			&outcome, &w.RefusedAt, &reason, &skip, &w.ReservedAt, &w.WithheldReason); err != nil {
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
