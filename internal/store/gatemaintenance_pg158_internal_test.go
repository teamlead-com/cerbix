package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// D10 — the deferred, snapshot-gated DROP exists for PostgreSQL < 15.9, whose release notes fix
// "possible crashes and 'could not open relation' errors in queries on a partitioned table
// occurring concurrently with a DETACH CONCURRENTLY and immediate drop of a partition". On a
// throwaway postgres:15.8-alpine (CERBIX_TEST_PG158_DSN), a PREPARED statement against the
// parent, planned before the detach, is executed between the detach and the deferred drop and
// again after the drop: rows, no error.
func TestGateMaintPreparedParentQuerySurvivesDetachAndDeferredDropOn158(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_PG158_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_PG158_DSN (a postgres:15.8 database) to run the cached-plan test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	var version int
	if err := st.pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 150008 {
		t.Fatalf("this database is PostgreSQL %d, the test wants 15.8 — the last 15.x before the fix", version)
	}
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	resetGateLedger(t, st, ctx)
	defer resetGateLedger(t, st, ctx)

	today := gateToday(t, st, ctx)
	proj, _, svc := seedService(t, st, ctx)
	old := today.AddDate(0, 0, -9)
	plantGateDay(t, st, ctx, old, "attached", nil)
	allow := "ALLOW"
	for _, at := range []time.Time{old.Add(time.Hour), today.Add(time.Hour)} {
		if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{}`); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// The prepared statement is planned against the parent BEFORE the detach.
	reader, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Release()
	if _, err := reader.Conn().Prepare(ctx, "ledger_count", `SELECT count(*) FROM service_gate_decisions WHERE project_id = $1 AND evaluated_at >= $2`); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	execute := func(stage string, want int) {
		var n int
		if err := reader.QueryRow(ctx, "ledger_count", proj, today.AddDate(0, 0, -30)).Scan(&n); err != nil {
			t.Fatalf("%s: the prepared parent query failed: %v", stage, err)
		}
		if n != want {
			t.Fatalf("%s: prepared query returned %d rows, want %d", stage, n, want)
		}
	}
	execute("before the detach", 2)

	cfg := gateMaintCfg
	cfg.PurgeEvery = time.Second
	rep, _, err := runGatePass(t, st, ctx, cfg, nil, &gateMetricsFake{}, nil)
	if err != nil || rep.Detached != 1 || rep.Dropped != 0 {
		t.Fatalf("detaching pass: detached=%d dropped=%d err=%v failures=%v", rep.Detached, rep.Dropped, err, rep.Failures)
	}
	execute("between the detach and the deferred drop", 1)

	time.Sleep(1100 * time.Millisecond)
	rep, _, err = runGatePass(t, st, ctx, cfg, nil, &gateMetricsFake{}, nil)
	if err != nil || rep.Dropped != 1 {
		t.Fatalf("drop pass: dropped=%d err=%v failures=%v", rep.Dropped, err, rep.Failures)
	}
	execute("after the drop", 1)
	if c := gateLookup(t, st, ctx, old); c.exists || c.state != "dropped" {
		t.Fatalf("after the drop: %+v", c)
	}
}
