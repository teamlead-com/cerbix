package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestAuditRetentionPostgreSQLMatrix(t *testing.T) {
	st, dsn, ctx := auditRetentionTestStore(t)
	cfg := AuditRetentionConfig{RetentionDays: 30, PurgeEvery: time.Hour, PurgeBatchRows: 100}

	t.Run("cutoff equality and mixed scopes", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		orgID := insertAuditRetentionOrg(t, st, ctx)

		session, acquired, err := st.TryBecomeLeaderSession(ctx, AuditRetentionMaintenanceLockKey)
		if err != nil || !acquired {
			t.Fatalf("acquire retention owner: acquired=%v err=%v", acquired, err)
		}
		defer session.Release()

		var cutoff time.Time
		if err := session.conn.QueryRow(ctx,
			`SELECT clock_timestamp() - make_interval(days => $1)`, cfg.RetentionDays).Scan(&cutoff); err != nil {
			t.Fatalf("database cutoff: %v", err)
		}
		insertAuditRetentionRow(t, st, ctx, &orgID, "org-expired", cutoff.Add(-time.Microsecond))
		insertAuditRetentionRow(t, st, ctx, nil, "global-expired", cutoff.Add(-time.Second))
		insertAuditRetentionRow(t, st, ctx, &orgID, "org-boundary", cutoff)
		insertAuditRetentionRow(t, st, ctx, nil, "global-boundary", cutoff)
		insertAuditRetentionRow(t, st, ctx, nil, "global-fresh", cutoff.Add(time.Microsecond))

		report, owned, result, err := runAuditRetentionBatches(ctx, session.conn, cfg, cutoff)
		if err != nil || !owned || result != "deleted" {
			t.Fatalf("retention pass: owned=%v result=%q report=%+v err=%v", owned, result, report, err)
		}
		if report.Deleted != 2 {
			t.Fatalf("deleted = %d, want 2", report.Deleted)
		}
		assertAuditTargets(t, st, ctx, "global-boundary", "global-fresh", "org-boundary")
	})

	t.Run("batch continuation", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		seedAuditRetentionRows(t, st, ctx, nil, "batch", 251, 31*24*time.Hour)

		report, owned, result, err := st.RunAuditRetentionPass(ctx, cfg)
		if err != nil || !owned || result != "deleted" {
			t.Fatalf("retention pass: owned=%v result=%q report=%+v err=%v", owned, result, report, err)
		}
		if report.Deleted != 251 {
			t.Fatalf("deleted = %d, want 251 across at least three batches", report.Deleted)
		}
		assertAuditCount(t, st, ctx, 0)
	})

	t.Run("row lock contention resumes on the next pass", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		seedAuditRetentionRows(t, st, ctx, nil, "locked", 1, 40*24*time.Hour)
		seedAuditRetentionRows(t, st, ctx, nil, "free", 3, 35*24*time.Hour)

		locker, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin locker: %v", err)
		}
		defer func() { _ = locker.Rollback(ctx) }()
		if _, err := locker.Exec(ctx, `SELECT id FROM audit_logs WHERE target = 'locked-1' FOR UPDATE`); err != nil {
			t.Fatalf("lock oldest row: %v", err)
		}

		first, owned, result, err := st.RunAuditRetentionPass(ctx, cfg)
		if err != nil || !owned || result != "deleted" {
			t.Fatalf("contended pass: owned=%v result=%q report=%+v err=%v", owned, result, first, err)
		}
		if first.Deleted != 3 || first.OldestExpiredSecond <= 0 {
			t.Fatalf("contended report = %+v, want three deletes and visible backlog", first)
		}
		assertAuditTargets(t, st, ctx, "locked-1")
		if err := locker.Commit(ctx); err != nil {
			t.Fatalf("release row lock: %v", err)
		}

		second, owned, result, err := st.RunAuditRetentionPass(ctx, cfg)
		if err != nil || !owned || result != "deleted" || second.Deleted != 1 {
			t.Fatalf("resume pass: owned=%v result=%q report=%+v err=%v", owned, result, second, err)
		}
		assertAuditCount(t, st, ctx, 0)
	})

	t.Run("delete statement rolls back atomically", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		seedAuditRetentionRows(t, st, ctx, nil, "rollback-ok", 1, 33*24*time.Hour)
		seedAuditRetentionRows(t, st, ctx, nil, "rollback-fail", 1, 32*24*time.Hour)
		if _, err := st.pool.Exec(ctx, `
			CREATE OR REPLACE FUNCTION audit_retention_test_delete_guard() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				IF OLD.target = 'rollback-fail-1' THEN
					RAISE EXCEPTION 'injected audit retention delete failure';
				END IF;
				RETURN OLD;
			END $$;
			CREATE TRIGGER audit_retention_test_delete_guard
			BEFORE DELETE ON audit_logs
			FOR EACH ROW EXECUTE FUNCTION audit_retention_test_delete_guard()`); err != nil {
			t.Fatalf("install rollback fault: %v", err)
		}
		t.Cleanup(func() {
			_, _ = st.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS audit_retention_test_delete_guard ON audit_logs`)
			_, _ = st.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS audit_retention_test_delete_guard()`)
		})

		report, owned, result, err := st.RunAuditRetentionPass(ctx, cfg)
		if err == nil || !owned || result != "error" || report.Deleted != 0 {
			t.Fatalf("rollback pass: owned=%v result=%q report=%+v err=%v", owned, result, report, err)
		}
		assertAuditCount(t, st, ctx, 2)

		if _, err := st.pool.Exec(ctx, `DROP TRIGGER audit_retention_test_delete_guard ON audit_logs`); err != nil {
			t.Fatalf("drop rollback trigger: %v", err)
		}
		if _, err := st.pool.Exec(ctx, `DROP FUNCTION audit_retention_test_delete_guard()`); err != nil {
			t.Fatalf("drop rollback function: %v", err)
		}
		retry, owned, result, err := st.RunAuditRetentionPass(ctx, cfg)
		if err != nil || !owned || result != "deleted" || retry.Deleted != 2 {
			t.Fatalf("post-rollback retry: owned=%v result=%q report=%+v err=%v", owned, result, retry, err)
		}
	})

	t.Run("two node fencing", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		seedAuditRetentionRows(t, st, ctx, nil, "fenced", 1, 31*24*time.Hour)
		other, err := Open(ctx, dsn)
		if err != nil {
			t.Fatalf("open second node: %v", err)
		}
		defer other.Close()

		owner, acquired, err := st.TryBecomeLeaderSession(ctx, AuditRetentionMaintenanceLockKey)
		if err != nil || !acquired {
			t.Fatalf("first node acquire: acquired=%v err=%v", acquired, err)
		}
		blocked, owned, result, err := other.RunAuditRetentionPass(ctx, cfg)
		if err != nil || owned || result != "lock_busy" || blocked.Deleted != 0 {
			t.Fatalf("second node: owned=%v result=%q report=%+v err=%v", owned, result, blocked, err)
		}
		assertAuditCount(t, st, ctx, 1)
		owner.Release()

		winner, owned, result, err := other.RunAuditRetentionPass(ctx, cfg)
		if err != nil || !owned || result != "deleted" || winner.Deleted != 1 {
			t.Fatalf("successor node: owned=%v result=%q report=%+v err=%v", owned, result, winner, err)
		}
	})

	t.Run("poisoned owner connection is discarded", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		seedAuditRetentionRows(t, st, ctx, nil, "poisoned", 1, 31*24*time.Hour)
		other, err := Open(ctx, dsn)
		if err != nil {
			t.Fatalf("open takeover node: %v", err)
		}
		defer other.Close()

		owner, acquired, err := st.TryBecomeLeaderSession(ctx, AuditRetentionMaintenanceLockKey)
		if err != nil || !acquired {
			t.Fatalf("first node acquire: acquired=%v err=%v", acquired, err)
		}
		var backendPID int
		if err := owner.conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			t.Fatalf("owner backend pid: %v", err)
		}
		if _, err := other.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, backendPID); err != nil {
			t.Fatalf("terminate owner backend: %v", err)
		}
		if err := owner.conn.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err == nil {
			t.Fatal("terminated owner connection remained usable")
		}
		owner.Release()

		report, owned, result, err := other.RunAuditRetentionPass(ctx, cfg)
		if err != nil || !owned || result != "deleted" || report.Deleted != 1 {
			t.Fatalf("takeover pass: owned=%v result=%q report=%+v err=%v", owned, result, report, err)
		}
		if err := st.Ping(ctx); err != nil {
			t.Fatalf("original pool did not replace poisoned connection: %v", err)
		}
	})

	t.Run("live backlog drain", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		orgID := insertAuditRetentionOrg(t, st, ctx)
		seedAuditRetentionRows(t, st, ctx, &orgID, "drain-org", 1250, 60*24*time.Hour)
		seedAuditRetentionRows(t, st, ctx, nil, "drain-global", 1250, 45*24*time.Hour)
		seedAuditRetentionRows(t, st, ctx, nil, "survivor", 3, 10*24*time.Hour)

		started := time.Now()
		report, owned, result, err := st.RunAuditRetentionPass(ctx, cfg)
		if err != nil || !owned || result != "deleted" {
			t.Fatalf("live drain: owned=%v result=%q report=%+v err=%v", owned, result, report, err)
		}
		if report.Deleted != 2500 || report.OldestExpiredSecond != 0 {
			t.Fatalf("live drain report = %+v, want 2500 deleted and zero backlog", report)
		}
		if elapsed := time.Since(started); elapsed >= auditRetentionPassBudget {
			t.Fatalf("live drain took %s, pass budget is %s", elapsed, auditRetentionPassBudget)
		}
		assertAuditTargets(t, st, ctx, "survivor-1", "survivor-2", "survivor-3")
	})

	t.Run("migration 107 round trip preserves audit rows", func(t *testing.T) {
		resetAuditRetentionTestData(t, st, ctx)
		insertAuditRetentionRow(t, st, ctx, nil, "migration-survivor", time.Now().UTC().Add(-90*24*time.Hour))
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open migration connection: %v", err)
		}
		defer func() { _ = db.Close() }()
		goose.SetBaseFS(migrationsFS)
		if err := goose.SetDialect("postgres"); err != nil {
			t.Fatalf("goose dialect: %v", err)
		}
		if err := goose.DownToContext(ctx, db, "migrations", 106); err != nil {
			t.Fatalf("migrate down through audit retention index: %v", err)
		}
		assertAuditCount(t, st, ctx, 1)
		var indexAfterDown *string
		if err := st.pool.QueryRow(ctx, `SELECT to_regclass('audit_logs_created_at_id_idx')::text`).Scan(&indexAfterDown); err != nil {
			t.Fatalf("read index after down: %v", err)
		}
		if indexAfterDown != nil {
			t.Fatalf("retention index survived down migration: %q", *indexAfterDown)
		}
		if err := goose.UpToContext(ctx, db, "migrations", 109); err != nil {
			t.Fatalf("migrate back up after retention down: %v", err)
		}
		assertAuditCount(t, st, ctx, 1)
		var indexAfterUp string
		if err := st.pool.QueryRow(ctx, `SELECT to_regclass('audit_logs_created_at_id_idx')::text`).Scan(&indexAfterUp); err != nil {
			t.Fatalf("read index after up: %v", err)
		}
		if indexAfterUp != "audit_logs_created_at_id_idx" {
			t.Fatalf("retention index after re-up = %q", indexAfterUp)
		}
	})
}

func auditRetentionTestStore(t *testing.T) (*Store, string, context.Context) {
	t.Helper()
	baseDSN := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if baseDSN == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run audit retention PostgreSQL tests")
	}
	parsed, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	admin, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		cancel()
		t.Fatalf("open admin database: %v", err)
	}
	databaseName := fmt.Sprintf("cerbix_test_audit_retention_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+databaseName); err != nil {
		admin.Close()
		cancel()
		t.Fatalf("create audit retention probe database: %v", err)
	}
	parsed.Path = "/" + databaseName
	dsn := parsed.String()
	if err := Migrate(ctx, dsn); err != nil {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+databaseName+` WITH (FORCE)`)
		admin.Close()
		cancel()
		t.Fatalf("migrate audit retention probe: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+databaseName+` WITH (FORCE)`)
		admin.Close()
		cancel()
		t.Fatalf("open audit retention probe: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, `DROP DATABASE IF EXISTS `+databaseName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop audit retention probe: %v", err)
		}
		admin.Close()
		cancel()
	})
	return st, dsn, ctx
}

func resetAuditRetentionTestData(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS audit_retention_test_delete_guard ON audit_logs;
		DROP FUNCTION IF EXISTS audit_retention_test_delete_guard();
		TRUNCATE audit_logs, organizations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset audit retention data: %v", err)
	}
}

func insertAuditRetentionOrg(t *testing.T, st *Store, ctx context.Context) string {
	t.Helper()
	var id string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name) VALUES ('audit-retention', 'Audit Retention') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert audit retention org: %v", err)
	}
	return id
}

func insertAuditRetentionRow(
	t *testing.T,
	st *Store,
	ctx context.Context,
	orgID *string,
	target string,
	createdAt time.Time,
) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO audit_logs (org_id, action, target, created_at)
		VALUES ($1::uuid, 'retention.test', $2, $3)`, orgID, target, createdAt); err != nil {
		t.Fatalf("insert audit row %s: %v", target, err)
	}
}

func seedAuditRetentionRows(
	t *testing.T,
	st *Store,
	ctx context.Context,
	orgID *string,
	prefix string,
	count int,
	age time.Duration,
) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO audit_logs (org_id, action, target, created_at)
		SELECT $1::uuid, 'retention.test', $2 || '-' || g::text,
		       clock_timestamp() - $3::bigint * interval '1 microsecond'
		  FROM generate_series(1, $4) AS g`, orgID, prefix, age.Microseconds(), count); err != nil {
		t.Fatalf("seed audit rows %s: %v", prefix, err)
	}
}

func assertAuditCount(t *testing.T, st *Store, ctx context.Context, want int) {
	t.Helper()
	var got int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&got); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if got != want {
		t.Fatalf("audit row count = %d, want %d", got, want)
	}
}

func assertAuditTargets(t *testing.T, st *Store, ctx context.Context, want ...string) {
	t.Helper()
	rows, err := st.pool.Query(ctx, `SELECT target FROM audit_logs ORDER BY target`)
	if err != nil {
		t.Fatalf("list audit targets: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect audit targets: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audit targets = %v, want %v", got, want)
	}
}
