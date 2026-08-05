package store

import (
	"context"
	"fmt"
)

// TryBecomeLeader attempts to acquire a Postgres session-level advisory lock on
// key, holding a dedicated pooled connection for the lock's lifetime. If ok is
// true the caller is the leader and must call release() to relinquish; if ok is
// false the lock is held elsewhere. This is the scheduler's leader election.
func (s *Store) TryBecomeLeader(ctx context.Context, key int64) (release func(), ok bool, err error) {
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
	release = func() {
		// Best-effort unlock on a fresh context so shutdown still releases.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}
	return release, true, nil
}
