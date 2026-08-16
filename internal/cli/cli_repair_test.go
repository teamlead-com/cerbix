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

func TestEnqueueServiceRepairValidatesItsInput(t *testing.T) {
	if code := Main([]string{"enqueue-service-repair"}); code != 2 {
		t.Fatalf("without flags = %d, want 2", code)
	}
	cfg := writeConfig(t, "database:\n  dsn: \"postgres://x:y@127.0.0.1:1/none?sslmode=disable\"\n")
	base := []string{"enqueue-service-repair", "--config", cfg,
		"--project", "p", "--service", "s"}
	if code := Main(append(base, "--from", "yesterday", "--to", "2026-08-16T00:00:00Z")); code != 2 {
		t.Fatalf("prose --from = %d, want 2", code)
	}
	if code := Main(append(base, "--from", "2026-08-16T01:00:00Z", "--to", "2026-08-16T00:00:00Z")); code != 2 {
		t.Fatalf("inverted range = %d, want 2", code)
	}
	if code := Main(append(base, "--from", "2026-08-16T00:00:00Z", "--to", "2026-08-16T00:00:00Z")); code != 2 {
		t.Fatalf("empty range = %d, want 2", code)
	}
}

// The iter-0139 operator artifact for restating history, tested AS the artifact: the real
// command line enqueues a durable admin repair through the store's own coalescing path — a
// second overlapping invocation folds into ONE pending union row rather than a duplicate.
func TestEnqueueServiceRepairCommandEndToEnd(t *testing.T) {
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
	probeName := fmt.Sprintf("cerbix_test_repaircli_%d_%d", os.Getpid(), time.Now().UnixNano())
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

	var svcID, projectID string
	if err := db.QueryRow(ctx, `
		WITH org AS (INSERT INTO organizations (slug, name) VALUES ('e2e-repair', 'e2e') RETURNING id),
		proj AS (INSERT INTO projects (org_id, slug, name) SELECT id, 'e2e-repair', 'e2e' FROM org RETURNING id),
		svc AS (INSERT INTO services (project_id, slug, name) SELECT id, 'e2e-repair', 'e2e' FROM proj RETURNING id, project_id)
		SELECT id, project_id FROM svc`).Scan(&svcID, &projectID); err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	run := func(from, to string) {
		t.Helper()
		if code := Main([]string{"enqueue-service-repair", "--config", cfg,
			"--project", projectID, "--service", svcID, "--from", from, "--to", to}); code != 0 {
			t.Fatalf("enqueue-service-repair %s..%s = %d, want 0", from, to, code)
		}
	}
	run("2026-06-01T10:00:00Z", "2026-06-01T11:00:00Z")
	// Overlapping second invocation: the store's pending same-reason coalescing must fold
	// both into ONE union row — the command preserves the product's enqueue semantics.
	run("2026-06-01T10:30:00Z", "2026-06-01T12:00:00Z")

	var rows int
	var reason, state string
	var from, to time.Time
	if err := db.QueryRow(ctx, `
		SELECT count(*), min(reason), min(state), min(range_start), max(range_end)
		  FROM service_repair_ranges WHERE service_id = $1`, svcID).Scan(&rows, &reason, &state, &from, &to); err != nil {
		t.Fatalf("inspect queue: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d repair rows after two overlapping enqueues, want 1 coalesced union", rows)
	}
	if reason != "admin" || state != "pending" {
		t.Fatalf("row is %s/%s, want admin/pending", reason, state)
	}
	wantFrom := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("union = [%s, %s), want [%s, %s)", from, to, wantFrom, wantTo)
	}
}
