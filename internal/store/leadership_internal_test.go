package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLeaderSessionFencing proves §17 fencing: the advisory lock and the apply transaction
// share ONE connection, so if that connection dies mid-apply (a) another candidate can win
// the lock (failover) and (b) the former leader's transaction can no longer commit — it can
// never commit after losing leadership. DB-gated.
func TestLeaderSessionFencing(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run the fencing test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	const key int64 = 0x6365726269781999 // test-only key

	s1, ok, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		t.Fatalf("s1 must acquire: ok=%v err=%v", ok, err)
	}
	// Second candidate is excluded while s1 holds the lock.
	if _, ok2, _ := st.TryBecomeLeaderSession(ctx, key); ok2 {
		t.Fatal("two leaders held the lock simultaneously")
	}

	// Simulate an in-flight apply: begin a transaction ON THE LEADER CONNECTION.
	var pid int
	if err := s1.conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	tx, err := s1.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin on leader conn: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("tx write: %v", err)
	}

	// Kill the leader's backend from a different pool connection — this both releases the
	// session-scoped advisory lock AND aborts the in-flight transaction.
	if _, err := st.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate backend: %v", err)
	}

	// (a) Failover: a new candidate can now acquire the freed lock.
	acquired := false
	for i := 0; i < 50; i++ {
		if s2, ok2, _ := st.TryBecomeLeaderSession(ctx, key); ok2 {
			acquired = true
			s2.Release()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !acquired {
		t.Fatal("failover did not occur after the leader's backend died")
	}

	// (b) Fencing: the former leader's transaction can NOT commit after losing leadership.
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("former leader committed after its backend/lock was lost — fencing broken")
	}
	// And its liveness check now fails.
	if held, _ := s1.Check(ctx); held {
		t.Fatal("former leader still reports holding the lock after backend death")
	}
	s1.Release() // best-effort; conn already dead
}
