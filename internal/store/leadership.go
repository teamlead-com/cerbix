package store

import (
	"context"
	"fmt"
)

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
