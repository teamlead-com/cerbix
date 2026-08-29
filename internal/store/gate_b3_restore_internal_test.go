package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// FR-024 discharge row 20 — the pg_dump/pg_restore case on a REAL restored database, not a
// registry whose relids a test set stale by hand (that is TestGateMaintStaleRelidsAreRebindedByMarker).
// The operator's flow is: `pg_dump -Fc` of a running instance, `CREATE DATABASE` of an EMPTY
// database, `pg_restore` into it. Every relation then has an OID the new catalog issued, while
// `service_gate_decision_partitions.relid` — plain data — comes back verbatim from the dump. The
// first maintenance pass must rebind every row by its marker (ownership is the COMMENT, never
// the OID — D10 §7 Ownership) and refuse nothing.
//
// Gated on CERBIX_TEST_RESTORED_DSN, which must name the RESTORED copy: the test never truncates,
// never plants, and writes nothing the pass itself does not write. It is NOT a
// CERBIX_TEST_DATABASE_DSN test — pointing it at a live instance's database would run a real
// maintenance pass there.
func TestGateMaintRestoredDatabaseRebindsEveryRelid(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_RESTORED_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_RESTORED_DSN to a pg_restore'd copy to run the restore test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	defer st.Close()

	// Migrate on a restored schema is a NO-OP: goose_db_version came back with the data, so
	// every version is already recorded. The count and the top version must not move.
	var versionsBefore, versionsAfter int
	var topBefore, topAfter int64
	if err := st.pool.QueryRow(ctx, `SELECT count(*), max(version_id) FROM goose_db_version`).Scan(&versionsBefore, &topBefore); err != nil {
		t.Fatalf("goose_db_version before: %v", err)
	}
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate on the restored schema: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*), max(version_id) FROM goose_db_version`).Scan(&versionsAfter, &topAfter); err != nil {
		t.Fatalf("goose_db_version after: %v", err)
	}
	if versionsBefore != versionsAfter || topBefore != topAfter {
		t.Fatalf("Migrate was not a no-op on the restored schema: versions %d→%d, top %d→%d", versionsBefore, versionsAfter, topBefore, topAfter)
	}
	t.Logf("restored schema: %d goose versions, top %d; Migrate was a no-op", versionsAfter, topAfter)

	// The registry against the LIVE catalog: `to_regclass(relname)::oid`, never registry text.
	type row struct {
		relname, state string
		relid          uint32
		catalog        *uint32
	}
	read := func() []row {
		rows, err := st.pool.Query(ctx, `
			SELECT relname, state, relid, to_regclass(relname)::oid
			  FROM service_gate_decision_partitions
			 WHERE state <> 'dropped'
			 ORDER BY day`)
		if err != nil {
			t.Fatalf("read registry: %v", err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.relname, &r.state, &r.relid, &r.catalog); err != nil {
				t.Fatalf("scan registry: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("registry rows: %v", err)
		}
		return out
	}
	var dropped int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decision_partitions WHERE state = 'dropped'`).Scan(&dropped); err != nil {
		t.Fatal(err)
	}

	// BEFORE: every registered relation exists in the restored catalog, and EVERY registry relid
	// differs from the catalog's oid — the restore renumbered them. A row that already agrees
	// means this is not a restored database, and the test refuses to pretend it is.
	before := read()
	if len(before) == 0 {
		t.Fatal("the restored registry has no non-dropped rows; nothing to rebind")
	}
	beforeByName := map[string]uint32{}
	for _, r := range before {
		if r.catalog == nil {
			t.Fatalf("%s (%s): registered but absent from the restored catalog", r.relname, r.state)
		}
		if r.relid == *r.catalog {
			t.Fatalf("%s (%s): registry relid %d already equals the live catalog oid — not a restored database", r.relname, r.state, r.relid)
		}
		beforeByName[r.relname] = r.relid
		t.Logf("before: %-45s state=%-8s registry relid=%-10d catalog oid=%d", r.relname, r.state, r.relid, *r.catalog)
	}
	t.Logf("before: %d non-dropped rows, all stale; %d dropped rows (no relation to bind)", len(before), dropped)

	// ONE pass with the shipped defaults (internal/config defaults(): retention 90 d, lead 7 d,
	// create 3, purge every 1 h, 8 stage-ops) and the same fake counter the ownership tests use.
	cfg := GateMaintenanceConfig{
		RetentionDays: 90, LeadDays: 7, CreateMax: 3, PurgeEvery: time.Hour, PurgeMaxPartitions: 8,
	}
	m := &gateMetricsFake{}
	rep, acquired, err := st.RunGateLedgerMaintenancePass(ctx, time.Now(), cfg, nil, m)
	if err != nil {
		t.Fatalf("maintenance pass: %v", err)
	}
	if !acquired {
		t.Fatal("the pass did not acquire the maintenance lock on the restored database")
	}
	if got := m.get("partition_identity"); got != 0 || len(rep.Refusals) != 0 {
		t.Fatalf("the restored registry was refused: partition_identity=%d refusals=%v", got, rep.Refusals)
	}
	if len(rep.Failures) != 0 {
		t.Fatalf("statement failures on a healthy restored database: %v", rep.Failures)
	}
	t.Logf("pass: created=%d attached=%d detached=%d finalized=%d dropped=%d refusals=%d failures=%d gauges_valid=%v",
		rep.Created, rep.Attached, rep.Detached, rep.Finalized, rep.Dropped, len(rep.Refusals), len(rep.Failures), rep.GaugesValid)

	// AFTER: every row the pass saw now carries the catalog's oid — rebound by marker, not by
	// recreation (the relation's oid is the same one the catalog held BEFORE the pass).
	after := read()
	catalogBefore := map[string]uint32{}
	for _, r := range before {
		catalogBefore[r.relname] = *r.catalog
	}
	rebound := 0
	for _, r := range after {
		if r.catalog == nil {
			t.Fatalf("after: %s (%s) vanished from the catalog", r.relname, r.state)
		}
		if r.relid != *r.catalog {
			t.Fatalf("after: %s (%s) still carries relid %d, catalog oid is %d", r.relname, r.state, r.relid, *r.catalog)
		}
		if oldCatalog, seen := catalogBefore[r.relname]; seen {
			if oldCatalog != *r.catalog {
				t.Fatalf("after: %s was recreated (catalog oid %d → %d) instead of rebound", r.relname, oldCatalog, *r.catalog)
			}
			if beforeByName[r.relname] == r.relid {
				t.Fatalf("after: %s relid %d did not move", r.relname, r.relid)
			}
			rebound++
		}
		t.Logf("after:  %-45s state=%-8s registry relid=%-10d catalog oid=%d", r.relname, r.state, r.relid, *r.catalog)
	}
	if rebound != len(before) {
		t.Fatalf("rebound %d of %d pre-existing rows", rebound, len(before))
	}
	var stale int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_gate_decision_partitions p
		 WHERE p.state <> 'dropped' AND p.relid IS DISTINCT FROM to_regclass(p.relname)::oid`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d registry rows still disagree with the catalog after the pass", stale)
	}
	t.Logf("after: %d rows rebound to live catalog oids, %d created by the pass, 0 stale", rebound, len(after)-rebound)
}
