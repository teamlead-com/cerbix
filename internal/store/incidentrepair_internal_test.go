package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// D-0182 — the repair of rows the earlier fixes could not reach.
//
// Every other test in this arc proves a FUTURE write is correct. This one runs the REAL migration
// against a database holding the durable damage, because a defect that has already written customer
// history is not discharged by a test that only exercises the new code path.
//
// It migrates to 89, writes the damaged rows with the same SQL the old versions would have produced,
// then migrates to 90 and asks what happened to each. Re-typing the migration's statements here
// instead would pass just as happily after somebody deleted the original.
func TestTheRepairMigrationFixesWhatItCanAndLeavesWhatItCannot(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, notices, done := probeDatabaseAt(t, st, ctx, "repair", 89)
	defer done()

	var orgID, projID, svcID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO organizations (slug, name) VALUES ('acme','Acme') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id::text`,
		orgID).Scan(&projID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO services (project_id, slug, name) VALUES ($1,'checkout','Checkout') RETURNING id::text`,
		projID).Scan(&svcID); err != nil {
		t.Fatalf("service: %v", err)
	}

	// (1) The RESURRECTED row: resolved once, then walked backwards by a writer holding a stale read.
	var resurrected string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, title, status, impact, source, resolved_at)
		VALUES ($1, 'walked backwards', 'investigating', 'major', 'manual', now() - interval '2 hours')
		RETURNING id::text`, projID).Scan(&resurrected); err != nil {
		t.Fatalf("seed resurrected: %v", err)
	}

	// (2) Its mirror: resolved with no resolution time.
	var unstamped string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, title, status, impact, source, updated_at)
		VALUES ($1, 'resolved without a time', 'resolved', 'minor', 'manual', now() - interval '3 hours')
		RETURNING id::text`, projID).Scan(&unstamped); err != nil {
		t.Fatalf("seed unstamped: %v", err)
	}

	// (3) STRANDED: an auto-incident for a service whose alert ended without it. No open episode and
	// the service is not firing — the shape a disown left behind before the lifecycle close landed.
	var stranded string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, service_id, title, status, impact, source)
		VALUES ($1, $2, 'Checkout — service down', 'investigating', 'major', 'auto')
		RETURNING id::text`, projID, svcID).Scan(&stranded); err != nil {
		t.Fatalf("seed stranded: %v", err)
	}

	// The NEGATIVE control, and the reason the repair is narrow: an auto-incident for a service that
	// is STILL FIRING must be left exactly as it is. A repair that closes a live outage is worse than
	// the defect.
	var live string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO services (project_id, slug, name) VALUES ($1,'search','Search') RETURNING id::text`,
		projID).Scan(&live); err != nil {
		t.Fatalf("second service: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO service_alert_state
		  (service_id, project_id, observed_state, candidate_state, live_firing, config_generation,
		   evaluated_at, lease_until)
		VALUES ($1, $2, 'down', 'down', true, 1, now(), now() + interval '90 seconds')`,
		live, projID); err != nil {
		t.Fatalf("firing state: %v", err)
	}
	var stillDown string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, service_id, title, status, impact, source)
		VALUES ($1, $2, 'Search — service down', 'investigating', 'major', 'auto')
		RETURNING id::text`, projID, live).Scan(&stillDown); err != nil {
		t.Fatalf("seed live incident: %v", err)
	}

	// A HUMAN's open incident on the same service must also be untouched: a machine repairing what a
	// person opened is the rule §D1b forbids in the other direction.
	var human string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, service_id, title, status, impact, source)
		VALUES ($1, $2, 'typed by a person', 'identified', 'minor', 'manual')
		RETURNING id::text`, projID, svcID).Scan(&human); err != nil {
		t.Fatalf("seed human: %v", err)
	}

	// (4) A member snapshot on an incident whose service later took a new revision — the population
	// the migration BOUNDS and refuses to rewrite.
	//
	// Its own service, because `incidents_service_open_auto_idx` allows exactly one open auto-incident
	// per service and the stranded row above already holds Checkout's — the same index whose blocking
	// is the reason the resurrected class is repaired at all.
	var billing, snapshotted string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO services (project_id, slug, name) VALUES ($1,'billing','Billing') RETURNING id::text`,
		projID).Scan(&billing); err != nil {
		t.Fatalf("third service: %v", err)
	}
	// Firing, so the stranded-row repair does not close it and the two cases stay independent.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO service_alert_state
		  (service_id, project_id, observed_state, candidate_state, live_firing, config_generation,
		   evaluated_at, lease_until)
		VALUES ($1, $2, 'down', 'down', true, 1, now(), now() + interval '90 seconds')`,
		billing, projID); err != nil {
		t.Fatalf("billing state: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, service_id, title, status, impact, source, started_at)
		VALUES ($1, $2, 'Billing — service down', 'investigating', 'major', 'auto',
		        now() - interval '3 days')
		RETURNING id::text`, projID, billing).Scan(&snapshotted); err != nil {
		t.Fatalf("seed snapshotted incident: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO incident_member_snapshots (incident_id, project_id, members)
		VALUES ($1, $2, '[{"monitor_name":"checkout-http"}]'::jsonb)`, snapshotted, projID); err != nil {
		t.Fatalf("seed member snapshot: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO service_definition_revisions (service_id, project_id, revision, state, effective_at, created_by)
		VALUES ($1, $2, 1, 'effective', now() - interval '1 day', 'test')`, billing, projID); err != nil {
		t.Fatalf("seed later revision: %v", err)
	}

	// (5) An anchorless auto-incident: both the monitor and the service are gone.
	var anchorless string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, title, status, impact, source)
		VALUES ($1, 'about something that no longer exists', 'investigating', 'major', 'auto')
		RETURNING id::text`, projID).Scan(&anchorless); err != nil {
		t.Fatalf("seed anchorless: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 90); err != nil {
		t.Fatalf("migrate to 90: %v", err)
	}

	type row struct {
		status   string
		resolved sql.NullTime
	}
	read := func(id string) row {
		t.Helper()
		var r row
		if err := db.QueryRowContext(ctx,
			`SELECT status, resolved_at FROM incidents WHERE id = $1`, id).Scan(&r.status, &r.resolved); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return r
	}

	if r := read(resurrected); r.status != "resolved" {
		t.Fatalf("the resurrected row is %q, want resolved — it carries a resolution time, it "+
			"occupies the one-open-incident index, and the NEXT outage cannot open one", r.status)
	}
	if r := read(unstamped); !r.resolved.Valid {
		t.Fatal("a resolved row was left with no resolution time, and the CHECK below forbids it")
	}
	if r := read(stranded); r.status != "resolved" || !r.resolved.Valid {
		t.Fatalf("the stranded auto-incident is %q/%v: its alert ended hours ago and the operator "+
			"is still reading 'investigating'", r.status, r.resolved.Valid)
	}
	if r := read(stillDown); r.status != "investigating" {
		t.Fatalf("the repair closed an incident for a service that is STILL FIRING (%q): closing a "+
			"live outage is worse than the defect being repaired", r.status)
	}
	if r := read(human); r.status != "identified" {
		t.Fatalf("the repair touched a HUMAN's incident (%q)", r.status)
	}

	// Each repaired row says what happened to it.
	notes := notesFor(t, db)
	if notes != 2 {
		t.Fatalf("%d repair notes, want one for each of the two repaired rows", notes)
	}

	// The two REPORTED classes are left exactly as they were. This is the half the first version of
	// this test did not have, and the reviewer proved it by replacing both reporting queries with
	// `SELECT 0` and watching it stay green.
	if r := read(snapshotted); r.status != "investigating" {
		t.Fatalf("the snapshotted incident was modified (%q): its membership is not derivable from "+
			"the stored data, and rewriting immutable history on a guess is the thing this decision "+
			"refuses", r.status)
	}
	var members string
	if err := db.QueryRowContext(ctx,
		`SELECT members::text FROM incident_member_snapshots WHERE incident_id = $1`,
		snapshotted).Scan(&members); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !strings.Contains(members, "checkout-http") {
		t.Fatalf("the member snapshot was rewritten: %s", members)
	}
	if r := read(anchorless); r.status != "investigating" {
		t.Fatalf("an anchorless auto-incident was resolved (%q): nothing identifies what it was "+
			"about, so closing it attaches somebody else's conclusion to it", r.status)
	}

	// And both were REPORTED. A class that is silently skipped is indistinguishable from one nobody
	// thought about.
	joined := strings.Join(*notices, "\n")
	for _, want := range []string{
		"resurrected row(s) resolved",
		"stranded service incident(s) resolved",
		"member snapshot(s) COULD have been taken",
		"anchorless auto-incident(s) remain open",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the migration did not report %q — an operator upgrading past it learns nothing "+
				"about the rows it declined to touch. Reported:\n%s", want, joined)
		}
	}

	// RERUN. Down to 89 drops only the constraint; the repaired DATA stays, so coming back up runs
	// the whole DO block over rows that are already correct. It must add no second note and change
	// nothing — the `🔧 Repaired:` marker is the guard, and a marker nobody tests is a comment.
	before := notesFor(t, db)
	if err := goose.DownToContext(ctx, db, "migrations", 89); err != nil {
		t.Fatalf("down to 89: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 90); err != nil {
		t.Fatalf("re-migrate to 90: %v", err)
	}
	if after := notesFor(t, db); after != before {
		t.Fatalf("a rerun produced %d repair notes, want the original %d", after, before)
	}

	// The invariant is the DATABASE's from here on, in both directions.
	if _, err := db.ExecContext(ctx,
		`UPDATE incidents SET status = 'investigating' WHERE id = $1`, resurrected); err == nil {
		t.Fatal("un-resolving a resolved incident was accepted: the CHECK is what makes 'resolved is " +
			"terminal' true of the DATA and not only of the code that happens to write it")
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE incidents SET status = 'resolved' WHERE id = $1`, stillDown); err == nil {
		t.Fatal("resolving without a resolution time was accepted")
	}
}

// probeDatabaseAt creates a throwaway database migrated to `version`, so a test can write the shape
// an OLD release produced and then run the real migration over it.
func probeDatabaseAt(
	t *testing.T, st *Store, ctx context.Context, label string, version int64,
) (*sql.DB, *[]string, func()) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a URL (%q): %v", dsn, err)
	}
	name := fmt.Sprintf("cerbix_test_%s_%d_%d", label, os.Getpid(), time.Now().UnixNano())
	if _, err := st.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create probe database: %v", err)
	}
	u.Path = "/" + name
	// The migration REPORTS two classes it will not repair, and a report nobody can read is not a
	// discharge. `OnNotice` is how those RAISE lines reach a test at all; `sql.Open("pgx", dsn)`
	// throws them away, which is why the first version of this test could not tell whether the
	// reporting queries ran — and indeed stayed green when the reviewer replaced both with SELECT 0.
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatalf("parse probe dsn: %v", err)
	}
	var noticesMu sync.Mutex
	notices := &[]string{}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		noticesMu.Lock()
		defer noticesMu.Unlock()
		*notices = append(*notices, n.Message)
	}
	db := stdlib.OpenDB(*cfg)
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	return db, notices, func() {
		_ = db.Close()
		if _, err := st.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database: %v", err)
		}
	}
}

// notesFor counts the repair notes the migration writes, which is how a rerun is checked.
func notesFor(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM incident_updates WHERE body LIKE '🔧 Repaired:%'`).Scan(&n); err != nil {
		t.Fatalf("count repair notes: %v", err)
	}
	return n
}
