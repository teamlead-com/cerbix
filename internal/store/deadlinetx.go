package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// deadlineTx makes a transaction obey a caller-side deadline the way D-0159 demands:
// server-side bounds are derived FROM THE REMAINDER before every statement, and no statement
// starts inside the reserve at all.
//
// The three mechanisms, and why each exists:
//
//   - a CLIENT-SIDE check before every statement refuses to start work inside the reserve.
//     This is what stops accumulation — many statements each finishing just under a bound
//     that was set once can sum to any total, and only a per-statement gate breaks that.
//   - the SERVER-SIDE pair is re-issued whenever the stored bound has gone stale by more
//     than the drift allowance, so a blocked statement dies at (remaining − reserve) from
//     roughly its own start rather than at a bound minted when the budget was young. Fast
//     statements reuse the bound and pay nothing.
//   - the client context deadline is a NET behind all of this, never the mechanism: set to
//     the same instant as the server bounds it races them, and when the client cancel wins
//     pgx closes the connection — on the leader path, the connection that owns the advisory
//     lock. The server must always speak first.
//
// The reserve is time kept back for the caller's persistence and commit: work stops while
// there is still enough budget to write down what happened.
type deadlineTx struct {
	pgx.Tx
	deadline time.Time
	reserve  time.Duration
	// lastBoundMS is the server bound currently in force on the session, 0 = unknown. It is
	// deliberately invalidated after a savepoint completes: SET LOCAL issued inside a
	// savepoint survives RELEASE into the parent transaction (only ROLLBACK reverts it), so
	// the parent's notion of the session's bound is stale the moment a child releases.
	lastBoundMS int64
	parent      *deadlineTx
}

// errSliceBudget says the caller-side budget is exhausted: not a failure of the data, a
// refusal to start more work. Callers stop cleanly, keep what is committed-safe, and leave.
var errSliceBudget = errors.New("store: slice budget exhausted")

// boundedDriftMS is how stale the issued server bound may become before it is re-issued.
// It is also the cap on how far past its issue-time bound a fast sequence can drift.
const boundedDriftMS = 15

func newDeadlineTx(tx pgx.Tx, deadline time.Time, reserve time.Duration) *deadlineTx {
	return &deadlineTx{Tx: tx, deadline: deadline, reserve: reserve}
}

func (d *deadlineTx) ensureBounds(ctx context.Context) error {
	remaining := time.Until(d.deadline) - d.reserve
	if remaining <= 0 {
		return errSliceBudget
	}
	ms := remaining.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if d.lastBoundMS != 0 && d.lastBoundMS <= ms+boundedDriftMS {
		return nil
	}
	for _, stmt := range []string{
		fmt.Sprintf("SET LOCAL statement_timeout = %d", ms),
		fmt.Sprintf("SET LOCAL lock_timeout = %d", min64(ms, repairLockTimeout.Milliseconds())),
	} {
		if _, err := d.Tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: derive statement bounds: %w", err)
		}
	}
	d.lastBoundMS = ms
	return nil
}

// invalidateBounds marks the session's bound unknown, forcing a re-issue before the next
// statement. Called after any savepoint completes, because its SET LOCALs leaked here.
func (d *deadlineTx) invalidateBounds() { d.lastBoundMS = 0 }

func (d *deadlineTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := d.ensureBounds(ctx); err != nil {
		return pgconn.CommandTag{}, err
	}
	return d.Tx.Exec(ctx, sql, args...)
}

func (d *deadlineTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := d.ensureBounds(ctx); err != nil {
		return nil, err
	}
	return d.Tx.Query(ctx, sql, args...)
}

func (d *deadlineTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := d.ensureBounds(ctx); err != nil {
		return errRow{err}
	}
	return d.Tx.QueryRow(ctx, sql, args...)
}

// Begin opens a savepoint whose statements obey the same deadline. When the child finishes —
// released or rolled back — the parent's bound knowledge is invalidated, because the child's
// SET LOCALs either leaked into the parent (RELEASE) or were reverted (ROLLBACK) and either
// way the parent no longer knows what the session holds.
func (d *deadlineTx) Begin(ctx context.Context) (pgx.Tx, error) {
	child, err := d.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &deadlineTx{Tx: child, deadline: d.deadline, reserve: d.reserve, parent: d}, nil
}

// Commit is bounded like any other statement: COMMIT is a statement the server executes, a
// blocked or slow one holds the connection exactly as a blocked INSERT would, and an earlier
// revision that committed the raw transaction directly left the very last step of every
// slice outside the mechanism the slice was built on.
func (d *deadlineTx) Commit(ctx context.Context) error {
	if d.parent == nil {
		if err := d.ensureBounds(ctx); err != nil {
			return err
		}
	}
	err := d.Tx.Commit(ctx)
	if d.parent != nil {
		d.parent.invalidateBounds()
	}
	return err
}

func (d *deadlineTx) Rollback(ctx context.Context) error {
	err := d.Tx.Rollback(ctx)
	if d.parent != nil {
		d.parent.invalidateBounds()
	}
	return err
}

// persistPhase returns an envelope over the SAME transaction for the persistence phase: the
// reserve drops to zero, so the time the work phases were forbidden to touch is now spendable
// — but still through the mechanism, per statement, commit included. Switching back to the
// raw transaction here is how two consecutive review rounds re-created accumulation:
// statement_timeout is a duration restarted for EACH statement, so a fixed "reserve" bound
// across N statements bounds none of their sum.
func (d *deadlineTx) persistPhase() *deadlineTx {
	return &deadlineTx{Tx: d.Tx, deadline: d.deadline, reserve: 0}
}

// boundedLifecycleExec runs ONE statement in its own small transaction under an ABSOLUTE
// deadline, through the same envelope as every other slice statement: refused client-side
// once the deadline is spent, server bounds derived from the remainder before each statement,
// COMMIT included, the client net one scheduling tolerance behind. It exists for the repair
// lifecycle writes — release, complete, fail, park. Its previous shape took a DURATION and
// minted it fresh at call time, which bounded each write and not the slice: a release that
// runs after the budget expired then waited its own ~100ms on top of the ~250ms already
// spent, and §10.10 promises the scheduler the sum, not the parts.
func boundedLifecycleExec(ctx context.Context, db dbConn, deadline time.Time, sql string, args ...any) error {
	if time.Until(deadline) <= 0 {
		return errSliceBudget
	}
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(schedulingTolerance))
	defer cancel()
	rawTx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin lifecycle write: %w", err)
	}
	defer rawTx.Rollback(ctx) //nolint:errcheck // no-op after commit
	tx := newDeadlineTx(rawTx, deadline, 0)
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lifecycleWriteBound CAPS one repair-lifecycle write. It is a ceiling under the caller's
// deadline, never a fresh grant on top of it — which policy applies is the lifecyclePolicy
// below, and what recovers a write the deadline refused is the LEASE.
const lifecycleWriteBound = 100 * time.Millisecond

// lifecyclePolicy is who a lifecycle write answers to.
type lifecyclePolicy int

const (
	// leaderLifecycle fits the write inside the slice: min(slice deadline, now + cap),
	// refused once the slice is spent. The §10.10 cadence rides on the caller's connection —
	// a write that mints time past the caller's deadline is how a "250ms" slice measured
	// ~350ms, bounded at every statement and unbounded as a phase.
	leaderLifecycle lifecyclePolicy = iota
	// standaloneLifecycle mints the cap even when the caller's deadline is already spent. No
	// cadence rides on a pooled connection's next 100ms, and releasing NOW beats parking the
	// range under a 60-second lease that exists for crashes.
	standaloneLifecycle
)

// writeDeadline is the absolute deadline for one lifecycle write made now, under the caller's
// slice deadline.
func (p lifecyclePolicy) writeDeadline(slice time.Time) time.Time {
	capped := time.Now().Add(lifecycleWriteBound)
	if p == leaderLifecycle && slice.Before(capped) {
		return slice
	}
	return capped
}

// errRow is the QueryRow shape of a refused statement: the error surfaces at Scan, which is
// where every caller already looks.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
