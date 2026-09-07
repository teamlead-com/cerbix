package store

import (
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// A7 — the pull TEST carrier has a ceiling of generation 3, and 00104 restores it.
//
// 00101 widened both pull tables to admit protocol version 4 and only one of them needed it: the
// job path really rides generation 4, and no generation-4 test carrier exists on any surface. A row
// carrying a generation nobody declares is invisible to every agent, because the claim predicate
// selects by `protocol_version <= capability` — the silent blackhole 00101's own header says the
// CHECK exists to prevent.
//
// Every case below runs the SHIPPED migration through goose against a probe database, not a copy of
// its SQL: the sibling suite for 00101 records why, and it holds here for the same reason — a test
// that restates the intended statement passes for a draft that never ran.

// The upgrade-path statement, and the reason this is a NEW migration rather than an edit to 00101:
// 00101 is applied everywhere, so rewriting its text would move only the fresh-install schema.
func TestThePullTestCeilingRejectsGenerationFourAndTheJobPathDoesNot(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "a7ceiling", 104)
	defer cleanup()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO pull_tests (region, payload, expires_at, protocol_version)
		 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', 4)`); err == nil {
		t.Error("pull_tests accepted a generation-4 row: no build emits a generation-4 test carrier, " +
			"so such a row can be claimed by no agent and would sit until its TTL")
	}
	// The asymmetry IS the item: generation 4 is real on the job path.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO pull_jobs (region, payload, expires_at, protocol_version)
		 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', 4)`); err != nil {
		t.Errorf("pull_jobs rejected a generation-4 row: %v — the narrowing took the job path with it, "+
			"and the ledger carrier has no transport on pull any more", err)
	}
	// And the generations a test carrier does ride are untouched.
	for _, generation := range []int{1, 2, 3} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO pull_tests (region, payload, expires_at, protocol_version)
			 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', $1)`, generation); err != nil {
			t.Errorf("pull_tests rejected generation %d: %v", generation, err)
		}
	}
}

// The narrowing FAILS CLOSED with a count, and deletes nothing.
//
// ADD CONSTRAINT validates the rows already present, so the narrowing would fail on its own — and
// the bare message names no table, no count and no procedure. Mutation killed: delete the guarded
// DO block and let the constraint fail by itself. The upgrade still fails, so an assertion of "it
// errored" passes, while the operator is left with "check constraint ... is violated by some row".
func TestTheNarrowingRefusesWithACountWhenAGenerationFourTestRowExists(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "a7refuse", 103)
	defer cleanup()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO pull_tests (region, payload, expires_at, protocol_version)
		 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', 4)`); err != nil {
		t.Fatalf("seed a generation-4 pull_tests row at version 103: %v", err)
	}

	err := goose.UpToContext(ctx, db, "migrations", 104)
	if err == nil {
		t.Fatal("the narrowing SUCCEEDED with a generation-4 row present")
	}
	msg := err.Error()
	for _, want := range []string{
		"refusing to narrow the pull_tests carrier ceiling",
		"1 row(s) at protocol_version 4",
		"delete them explicitly",
		"will not discard them for you",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q; an operator reading it cannot act.\nmessage: %s", want, msg)
		}
	}
	// Mutation killed: "clean up first" — DELETE the offending rows and then narrow. The upgrade
	// then succeeds and the evidence is gone, which no assertion on the error can see.
	if n := pullCarrierCount(t, ctx, db, "pull_tests", 4); n != 1 {
		t.Errorf("pull_tests holds %d generation-4 row(s) after the refusal, want 1: the migration "+
			"discarded a row it was asked to diagnose", n)
	}
	// The drop and the add are ONE statement, so a failed narrowing rolls back atomically and the
	// WIDE constraint survives: a refused upgrade never leaves the table unconstrained.
	if def := pullCarrierConstraintDef(t, ctx, db, "pull_tests_protocol_version_check"); !strings.Contains(def, "4") {
		t.Errorf("pull_tests_protocol_version_check is %q after the refusal: the failed narrowing "+
			"dropped the boundary and left the table unconstrained", def)
	}
}

// Widening validates nothing and can never fail, so the Down is the easy direction — worth pinning
// precisely because 00101's Down is the hard one and a reader arriving from it expects a guard.
func TestThePullTestCeilingDownWidensUnconditionallyAndConverges(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "a7down", 104)
	defer cleanup()

	if err := goose.DownToContext(ctx, db, "migrations", 103); err != nil {
		t.Fatalf("the Down refused: %v — widening admits a superset and has nothing to validate", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO pull_tests (region, payload, expires_at, protocol_version)
		 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', 4)`); err != nil {
		t.Errorf("pull_tests still rejects generation 4 after the Down: %v", err)
	}
	// And the boundary still rejects the unknown, which is what widening rather than dropping means.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO pull_tests (region, payload, expires_at, protocol_version)
		 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', 5)`); err == nil {
		t.Error("pull_tests accepted generation 5 after the Down: the CHECK was dropped, not widened")
	}
}
