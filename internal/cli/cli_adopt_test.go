package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestAdoptFactMonthValidatesItsInput(t *testing.T) {
	if code := Main([]string{"adopt-fact-month"}); code != 2 {
		t.Fatalf("adopt-fact-month without flags = %d, want 2", code)
	}
	cfg := writeConfig(t, "database:\n  dsn: \"postgres://x:y@127.0.0.1:1/none?sslmode=disable\"\n")
	if code := Main([]string{"adopt-fact-month", "--config", cfg, "--month", "2001-13"}); code != 2 {
		t.Fatalf("adopt-fact-month with month 2001-13 = %d, want 2", code)
	}
	if code := Main([]string{"adopt-fact-month", "--config", cfg, "--month", "May 2001"}); code != 2 {
		t.Fatalf("adopt-fact-month with a prose month = %d, want 2", code)
	}
	if code := Main([]string{"adopt-fact-month", "--config", cfg, "--month", "2001-05", "--timeout", "-1s"}); code != 2 {
		t.Fatalf("adopt-fact-month with a negative timeout = %d, want 2", code)
	}
}

// The D-0161 operator artifact, tested AS the artifact: the real `cerbix adopt-fact-month`
// command line (through Main, config file and all) adopts a month of facts stranded in the
// DEFAULT partition of its own freshly migrated database, past the automatic gate's reach,
// and is idempotent on a rerun. The probe database is unique per run, created (never
// pre-dropped) by this test, and dropped by exact name.
func TestAdoptFactMonthCommandEndToEnd(t *testing.T) {
	gate := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if gate == "" {
		t.Skip("CERBIX_TEST_DATABASE_DSN not set")
	}
	u, err := url.Parse(gate)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL (%q): %v", gate, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	admin, err := pgx.Connect(ctx, gate)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx) //nolint:errcheck // test teardown
	probeName := fmt.Sprintf("cerbix_test_adoptcli_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+probeName); err != nil {
		t.Fatalf("create probe database %s (this end-to-end gate requires CREATEDB): %v", probeName, err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+probeName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database %s: %v", probeName, err)
		}
	}()
	u.Path = "/" + probeName
	probeDSN := u.String()
	cfg := writeConfig(t, fmt.Sprintf("database:\n  dsn: %q\n", probeDSN))

	if code := Main([]string{"migrate", "--config", cfg}); code != 0 {
		t.Fatalf("migrate probe database = %d, want 0", code)
	}

	db, err := pgx.Connect(ctx, probeDSN)
	if err != nil {
		t.Fatalf("connect probe: %v", err)
	}
	defer db.Close(ctx) //nolint:errcheck // test teardown

	// A month far in the past: no seeded partition covers it, so facts land in DEFAULT —
	// exactly the stranded state the command exists to recover.
	month := time.Date(2001, time.February, 1, 0, 0, 0, 0, time.UTC)
	var svcID, projectID, epochID string
	if err := db.QueryRow(ctx, `
		WITH org AS (INSERT INTO organizations (slug, name) VALUES ('e2e-adopt', 'e2e') RETURNING id),
		proj AS (INSERT INTO projects (org_id, slug, name) SELECT id, 'e2e-adopt', 'e2e' FROM org RETURNING id),
		svc AS (INSERT INTO services (project_id, slug, name) SELECT id, 'e2e-adopt', 'e2e' FROM proj RETURNING id, project_id),
		rev AS (INSERT INTO service_definition_revisions (service_id, project_id, revision, effective_at)
		        SELECT id, project_id, 1, $1 FROM svc RETURNING id, service_id, project_id),
		ep AS (INSERT INTO service_evaluation_epochs (service_id, project_id, epoch_seq, revision_id, effective_at, snapshot_hash)
		       SELECT service_id, project_id, 1, id, $1, 'e2e' FROM rev RETURNING id, service_id, project_id)
		SELECT service_id, project_id, id FROM ep`, month).Scan(&svcID, &projectID, &epochID); err != nil {
		t.Fatalf("seed domain chain: %v", err)
	}
	for day := 0; day < 3; day++ {
		if _, err := db.Exec(ctx, `
			INSERT INTO service_reliability_buckets
			  (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
			VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
			svcID, projectID, epochID, month.AddDate(0, 0, day)); err != nil {
			t.Fatalf("plant stranded fact: %v", err)
		}
	}
	var inDefault int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets_default`).Scan(&inDefault); err != nil {
		t.Fatalf("default count: %v", err)
	}
	if inDefault != 3 {
		t.Fatalf("planted facts are not stranded in DEFAULT (%d rows) — the scenario premise broke", inDefault)
	}

	if code := Main([]string{"adopt-fact-month", "--config", cfg, "--month", "2001-02", "--timeout", "30s"}); code != 0 {
		t.Fatalf("adopt-fact-month = %d, want 0", code)
	}

	var attached bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_inherits
		  WHERE inhparent = 'service_reliability_buckets'::regclass
		    AND inhrelid = to_regclass('service_reliability_buckets_200102'))`).Scan(&attached); err != nil {
		t.Fatalf("attach check: %v", err)
	}
	if !attached {
		t.Fatal("the command reported success but the month partition is not attached")
	}
	var visible int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM service_reliability_buckets
		 WHERE bucket_start >= $1 AND bucket_start < $2`,
		month, month.AddDate(0, 1, 0)).Scan(&visible); err != nil {
		t.Fatalf("parent count: %v", err)
	}
	if visible != 3 {
		t.Fatalf("parent sees %d facts after the operator adoption, want 3", visible)
	}
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets_default`).Scan(&inDefault); err != nil {
		t.Fatalf("default recount: %v", err)
	}
	if inDefault != 0 {
		t.Fatalf("%d rows left in DEFAULT after the operator adoption", inDefault)
	}

	// Idempotent: an already-attached month is a no-op success, so a nervous operator
	// re-running the command changes nothing and alarms nobody.
	if code := Main([]string{"adopt-fact-month", "--config", cfg, "--month", "2001-02", "--timeout", "30s"}); code != 0 {
		t.Fatalf("adopt-fact-month rerun = %d, want 0", code)
	}
}
