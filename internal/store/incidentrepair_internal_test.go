package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

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
	db, done := probeDatabaseAt(t, st, ctx, "repair", 89)
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

	// Each repaired row says what happened to it, and a second run adds no second note.
	var notes int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM incident_updates WHERE body LIKE '🔧 Repaired:%'`).Scan(&notes); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if notes != 2 {
		t.Fatalf("%d repair notes, want one for each of the two repaired rows", notes)
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
) (*sql.DB, func()) {
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
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	return db, func() {
		_ = db.Close()
		if _, err := st.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database: %v", err)
		}
	}
}
