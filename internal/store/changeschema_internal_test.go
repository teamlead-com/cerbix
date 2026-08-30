package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// FR-025 §5 — the schema migration 00094 (func-change-intelligence.md; iter-0165 changeset 1).
//
// These tests assert the constraints the migration exists FOR, and that its Down removes
// exactly what its Up created — no more (the incidents UNIQUE that 00080 owns must survive)
// and no less. A Down nobody runs is a Down nobody knows works.

func changeCountRelations(t *testing.T, ctx context.Context, db *sql.DB, pattern string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname LIKE $1`, pattern).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func changeHasConstraint(t *testing.T, ctx context.Context, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func changeHasColumn(t *testing.T, ctx context.Context, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`,
		table, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// Up, Down, Up on a probe database: the two tables, their indexes and the api_tokens column
// appear, disappear and reappear; the incidents UNIQUE (id, project_id) that 00080 created is
// untouched by the Down (this migration found it present and added nothing); the Up converges.
func TestChangeMigrationDownRemovesEverythingItCreated(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, notices, cleanup := probeDatabaseAt(t, st, ctx, "changedown", 94)
	defer cleanup()

	for _, want := range []string{"service_changes", "incident_changes", "service_changes_identity_phase_key",
		"service_changes_id_project_key", "service_changes_service_occurred_idx", "service_changes_project_occurred_idx",
		"incident_changes_change_idx"} {
		if changeCountRelations(t, ctx, db, want) != 1 {
			t.Fatalf("relation %s missing after Up", want)
		}
	}
	for _, c := range []string{"service_changes_source_chk", "service_changes_external_id_chk", "service_changes_kind_chk",
		"service_changes_phase_chk", "service_changes_ref_chk", "service_changes_url_chk", "service_changes_service_fkey",
		"incident_changes_role_chk", "incident_changes_lag_chk", "incident_changes_incident_fkey", "incident_changes_change_fkey",
		"incidents_id_project_key"} {
		if !changeHasConstraint(t, ctx, db, c) {
			t.Fatalf("constraint %s missing after Up", c)
		}
	}
	if changeHasConstraint(t, ctx, db, "incidents_id_project_change_key") {
		t.Fatal("00094 added a second UNIQUE (id, project_id) on incidents although 00080's is present")
	}
	if !changeHasColumn(t, ctx, db, "api_tokens", "actions") {
		t.Fatal("api_tokens.actions missing after Up")
	}
	said := false
	for _, n := range *notices {
		if strings.Contains(n, "already carries UNIQUE (id, project_id)") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the guarded block did not report the existing incidents key; notices: %v", *notices)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 93); err != nil {
		t.Fatalf("goose down 94 → 93: %v", err)
	}
	if n := changeCountRelations(t, ctx, db, "service_changes%") + changeCountRelations(t, ctx, db, "incident_changes%"); n != 0 {
		t.Fatalf("%d relations named for the change tables survive the Down", n)
	}
	if changeHasColumn(t, ctx, db, "api_tokens", "actions") {
		t.Fatal("api_tokens.actions survives the Down")
	}
	if !changeHasConstraint(t, ctx, db, "incidents_id_project_key") {
		t.Fatal("the Down removed 00080's incidents_id_project_key, which this migration did not add")
	}

	if err := goose.UpToContext(ctx, db, "migrations", 94); err != nil {
		t.Fatalf("goose up 93 → 94 after the Down: %v", err)
	}
	if changeCountRelations(t, ctx, db, "service_changes") != 1 || changeCountRelations(t, ctx, db, "incident_changes") != 1 ||
		!changeHasColumn(t, ctx, db, "api_tokens", "actions") {
		t.Fatal("after Down and Up the schema did not converge")
	}
}

// The guarded block ADDS the key when a database lacks it — and the Down then removes exactly
// that key. Proved on a probe at 93 whose 00080 key is dropped by hand first.
func TestChangeMigrationAddsTheIncidentsKeyOnlyWhenAbsent(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, notices, cleanup := probeDatabaseAt(t, st, ctx, "changekey", 93)
	defer cleanup()

	// Several composite FKs (00080's impacts and later tables) reference the key; CASCADE drops
	// them with it — this is a throwaway probe whose only question is the guarded block.
	if _, err := db.ExecContext(ctx, `ALTER TABLE incidents DROP CONSTRAINT incidents_id_project_key CASCADE`); err != nil {
		t.Fatalf("drop 00080 key: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 94); err != nil {
		t.Fatalf("goose up 93 → 94: %v", err)
	}
	if !changeHasConstraint(t, ctx, db, "incidents_id_project_change_key") {
		t.Fatal("the guarded block did not add UNIQUE (id, project_id) to an incidents table lacking it")
	}
	added := false
	for _, n := range *notices {
		if strings.Contains(n, "incidents_id_project_change_key UNIQUE (id, project_id) added") {
			added = true
		}
	}
	if !added {
		t.Fatalf("the guarded block did not report the addition; notices: %v", *notices)
	}
	if err := goose.DownToContext(ctx, db, "migrations", 93); err != nil {
		t.Fatalf("goose down 94 → 93: %v", err)
	}
	if changeHasConstraint(t, ctx, db, "incidents_id_project_change_key") {
		t.Fatal("the Down left the key this migration added")
	}
}
