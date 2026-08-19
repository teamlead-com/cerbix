package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LeaderSession is a held file-provider leadership bound to ONE pinned connection: the
// advisory lock AND the apply transaction both run on this connection. If it is lost
// (network blip, Postgres restart, pooler eviction) the session-scoped lock is released by
// Postgres AND any in-flight transaction on it is aborted — so a former leader can never
// commit after losing leadership (true fencing, spec §17). Distinct from TryBecomeLeader
// (used by the scheduler), which hands back bare callbacks and lets writes use the pool.
type LeaderSession struct {
	store *Store
	conn  *pgxpool.Conn
	key   int64
}

// TryBecomeLeaderSession acquires the advisory lock on a pinned connection and returns a
// session whose apply transactions run on that same connection (fencing). ok=false → the
// lock is held elsewhere. The caller MUST call Release().
func (s *Store) TryBecomeLeaderSession(ctx context.Context, key int64) (*LeaderSession, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store: acquire leader conn: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("store: advisory lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return &LeaderSession{store: s, conn: conn, key: key}, true, nil
}

// Check reports whether this session still holds the lock on its live connection (same
// liveness-probe semantics as TryBecomeLeader's check).
func (ls *LeaderSession) Check(ctx context.Context) (bool, error) {
	var held bool
	if err := ls.conn.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM pg_locks
		   WHERE locktype = 'advisory' AND granted
		     AND pid = pg_backend_pid()
		     AND objsubid = 1
		     AND ((classid::bigint << 32) | objid::bigint) = $1
		 )`, ls.key).Scan(&held); err != nil {
		return false, fmt.Errorf("store: leadership check: %w", err)
	}
	return held, nil
}

// Release relinquishes the lock and returns the pinned connection to the pool (best-effort
// unlock on a fresh context so shutdown still releases).
func (ls *LeaderSession) Release() {
	_, _ = ls.conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, ls.key)
	ls.conn.Release()
}

// TryBecomeLeader attempts to acquire a Postgres session-level advisory lock on
// key, holding a dedicated pooled connection for the lock's lifetime. If ok is
// true the caller is the leader and must call release() to relinquish; if ok is
// false the lock is held elsewhere. This is the scheduler's leader election.
//
// It also returns check(): a liveness probe the leader runs periodically on the
// SAME held connection. It reports whether this session still holds the lock —
// false or error means the connection (and thus the session-scoped lock) was lost
// (network blip, Postgres restart, pooler eviction), at which point another node
// can win the lock. Without this probe the old leader kept publishing jobs against
// a lock it no longer held → two leaders (split-brain: double dispatch, double
// renotify/escalation). The leader steps down on a failed check and re-contends.
func (s *Store) TryBecomeLeader(ctx context.Context, key int64) (release func(), check func(context.Context) (bool, error), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: acquire leader conn: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, nil, false, fmt.Errorf("store: advisory lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, nil, false, nil
	}
	check = func(cctx context.Context) (bool, error) {
		// Runs on the held connection, so it doubles as a liveness probe: if the
		// connection died the query errors (→ step down). The pg_locks predicate
		// reconstructs the original bigint key from (classid, objid) and requires
		// the lock still be granted to THIS backend, catching the (rare) case of a
		// live connection whose lock was released out from under us.
		var held bool
		if err := conn.QueryRow(cctx,
			`SELECT EXISTS(
			   SELECT 1 FROM pg_locks
			   WHERE locktype = 'advisory' AND granted
			     AND pid = pg_backend_pid()
			     AND objsubid = 1
			     AND ((classid::bigint << 32) | objid::bigint) = $1
			 )`, key).Scan(&held); err != nil {
			return false, fmt.Errorf("store: leadership check: %w", err)
		}
		return held, nil
	}
	release = func() {
		// Best-effort unlock on a fresh context so shutdown still releases.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}
	return release, check, true, nil
}

// RunServiceRepairSlice claims one repair range and works it until the deadline, entirely on
// the lock-owning connection.
//
// This is the ownership migration §10.7 requires, and the reason it is a method on the
// session rather than on the Store: a batch that ran through the pool would commit happily
// after this node had already lost leadership and another had taken over. Here, losing the
// connection releases the advisory lock and aborts the in-flight transaction together, so a
// deposed leader cannot write behind its successor. Passing a `check()` callback into the
// computation would NOT achieve this — it only narrows the window between the check and the
// commit.
//
// It reports whether it found anything to do, so the caller can back off when the queue is
// empty instead of spinning.
func (ls *LeaderSession) RunServiceRepairSlice(ctx context.Context, deadline time.Time) (bool, error) {
	r, ok, err := ls.store.claimRepairRangeBounded(ctx, ls.conn, deadline)
	if err != nil || !ok {
		return false, err
	}
	// leaderLifecycle: every lifecycle write on this connection fits INSIDE the slice
	// deadline — the scheduler's cadence rides on it, and a write that mints its own time
	// after the budget ran out is how a 250ms slice measured ~350ms.
	if err := ls.store.runRepairRangeOn(ctx, ls.conn, r, deadline, leaderLifecycle); err != nil {
		return true, err
	}
	return true, nil
}

// RunServiceSlice is the whole service-reliability share of one leader sub-tick.
//
// Durable repair comes FIRST and forward materialization second, deliberately. Repair exists
// because something already recorded is wrong or missing; keeping up with the clock can
// wait a slice, a window known to be wrong cannot. The forward pass then runs only when the
// repair queue is empty, which is also what stops a busy repair backlog from being starved
// by a service adopting ninety days of history.
func (ls *LeaderSession) RunServiceSlice(ctx context.Context, deadline time.Time) (bool, error) {
	// A tail too short to claim, work and release is skipped WHOLE, before either phase. An
	// earlier revision put this guard inside the repair slice returning "nothing to do" — and
	// false there means "the repair queue is empty", so a short tail ran the forward pass
	// with a repair backlog still pending, inverting the repair-first rule this function
	// exists to enforce. Undersized means: less than the claim's own worst case plus the
	// closing lifecycle write's reserve — a claim there could only park the range under a
	// 60-second lease that exists for crashes, not for tail-of-slice claims.
	if time.Until(deadline) < lifecycleWriteBound+lifecycleReserve {
		return false, nil
	}
	worked, err := ls.RunServiceRepairSlice(ctx, deadline)
	if err != nil || worked {
		return worked, err
	}
	return ls.store.AdvanceServiceMaterializationOn(ctx, ls.conn, deadline)
}

// EvaluateServiceAlerts runs the LIVE service-alert slice (FR-021 §16.3) on the LOCK-OWNING
// connection.
//
// It lives on the session, not on the Store, and that is the fence: the whole slice — snapshot,
// state, episodes and outbox rows — runs inside one transaction on the connection holding the
// advisory lock, so a leader that has lost the lock cannot commit an evaluation behind its
// successor. Leader-GATING alone does not give this: a watchdog samples every few seconds, and in
// the window between two samples a deposed process would otherwise happily commit on any pool
// connection. The latch CAS is not a substitute either — the episode and its outbox row are written
// BEFORE it, so by the time the CAS lost, somebody has already been paged.
func (ls *LeaderSession) EvaluateServiceAlerts(
	ctx context.Context, cadence time.Duration,
) (ServiceAlertEvaluation, error) {
	return ls.store.evaluateServiceAlertsOn(ctx, ls.conn, cadence)
}

// EvaluateServiceBurnAlerts runs the SEALED service-alert slice (§16.4) on the same fenced
// connection, for the same reason.
func (ls *LeaderSession) EvaluateServiceBurnAlerts(
	ctx context.Context, cadence time.Duration,
) (ServiceBurnEvaluation, error) {
	return ls.store.evaluateServiceBurnAlertsOn(ctx, ls.conn, cadence)
}
