package store

import (
	"context"
	"testing"
	"time"
)

// FR-025 adversarial pass (iter-0165 task 2, Agent B) — the probes the `change_b_*` tests share.
// Every helper here OBSERVES the database's own bookkeeping (pg_locks, pg_stat_activity, a
// competing pg_try_advisory_xact_lock) so a test can say WHERE a concurrent writer is parked,
// not merely that it has not returned yet.

// blockedBackend is one backend of this database that is waiting on a heavyweight lock.
type blockedBackend struct {
	pid       int
	waitEvent string
	query     string
}

// blockedBackends lists the backends of the current database waiting on a Lock right now.
func blockedBackends(t *testing.T, st *Store, ctx context.Context) []blockedBackend {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT pid, wait_event, query FROM pg_stat_activity
		 WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`)
	if err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	defer rows.Close()
	var out []blockedBackend
	for rows.Next() {
		var b blockedBackend
		if err := rows.Scan(&b.pid, &b.waitEvent, &b.query); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// waitForBlockedBackends polls until `want` backends of this database wait on a Lock, or fails
// after ten seconds. It returns them.
func waitForBlockedBackends(t *testing.T, st *Store, ctx context.Context, want int) []blockedBackend {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := blockedBackends(t, st, ctx)
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 10 s only %d backend(s) wait on a lock, want %d: %+v", len(got), want, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// advisoryLockFree asks, from a throwaway transaction on another connection, whether the
// identity's advisory lock — the store's two-int key (changeIdentityLockSQL) — can be taken
// right now; false means somebody holds it.
func advisoryLockFree(t *testing.T, st *Store, ctx context.Context, serviceID, source, externalID string) bool {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the probe never commits
	var free bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1::uuid::text), hashtext($2 || '/' || $3))`,
		serviceID, source, externalID).Scan(&free); err != nil {
		t.Fatalf("pg_try_advisory_xact_lock: %v", err)
	}
	return free
}

// advisoryLocksGranted counts the GRANTED advisory locks on the identity's key in pg_locks. The
// store takes the two-int form, so key1 sits in classid, key2 in objid, with objsubid 2.
func advisoryLocksGranted(t *testing.T, st *Store, ctx context.Context, serviceID, source, externalID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_locks l
		 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2
		   AND l.classid = (hashtext($1::uuid::text)::bigint & 4294967295)::oid
		   AND l.objid = (hashtext($2 || '/' || $3)::bigint & 4294967295)::oid`,
		serviceID, source, externalID).Scan(&n); err != nil {
		t.Fatalf("pg_locks: %v", err)
	}
	return n
}

// completedWithin reports whether the channel yields before d elapses; the value is returned
// when it does.
func completedWithin(ch <-chan error, d time.Duration) (error, bool) {
	select {
	case err := <-ch:
		return err, true
	case <-time.After(d):
		return nil, false
	}
}

// plantUncommittedPhase writes a phase row for the fixture service through a caller-owned
// transaction and leaves it UNCOMMITTED, so a concurrent RecordChangePhase of the same phase
// parks on the unique key inside its own transaction — a way to hold the store's write
// mid-flight without a hook in the product.
func plantUncommittedPhase(t *testing.T, tx dbConn, ctx context.Context, projectID, serviceID, source, ext, phase string, at time.Time) string {
	t.Helper()
	id, err := newChangeID(at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_changes (id, project_id, service_id, source, external_id, kind, phase, ref, url,
		                             occurred_at, actor_label, via_token, recorded_at)
		VALUES ($1, $2, $3, $4, $5, 'deploy', $6, 'v1', '', $7, 'sql', true, $7)`,
		id, projectID, serviceID, source, ext, phase, at.UTC()); err != nil {
		t.Fatalf("plant uncommitted %s/%s %s: %v", source, ext, phase, err)
	}
	return id
}
