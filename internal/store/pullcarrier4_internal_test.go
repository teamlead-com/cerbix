package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// FR-032 invariant 10j — migration 00101 widens the pull carrier CHECK to generation 4, and its
// Down REFUSES while generation-4 rows are pending rather than discarding them (D-0160).
//
// Every case here runs the SHIPPED Down through goose against a probe database migrated to 101,
// not a copy of its SQL: 00063's own comment is that its first draft was "a destructive write-off
// wearing the words of a safe rollback", and a test that re-states the intended SQL would have
// passed for that draft too. The neighbouring 00094 tests put it as "a Down nobody runs is a Down
// nobody knows works".

func pullCarrierConstraintDef(t *testing.T, ctx context.Context, db *sql.DB, name string) string {
	t.Helper()
	var def string
	if err := db.QueryRowContext(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, name).Scan(&def); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return def
}

func pullCarrierCount(t *testing.T, ctx context.Context, db *sql.DB, table string, generation int) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM `+table+` WHERE protocol_version = $1`, generation).Scan(&n); err != nil {
		t.Fatalf("count %s at generation %d: %v", table, generation, err)
	}
	return n
}

// insertPullRow writes one row at the given generation. It deliberately does NOT go through
// EnqueuePullJob: that helper stamps the generation the emitter currently selects, and this test
// needs a generation-4 row to exist before anything selects generation 4.
func insertPullRow(t *testing.T, ctx context.Context, db *sql.DB, table string, generation int) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO `+table+` (region, payload, expires_at, protocol_version)
		 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', $1)`, generation); err != nil {
		t.Fatalf("insert generation-%d row into %s: %v", generation, table, err)
	}
}

// The 10j proof. Rows pending in BOTH tables: the Down fails, both rows are still there, the
// message carries the counts and the drain procedure, and the widened CHECK is intact — the
// refusal is atomic, not a partial rollback that left the boundary somewhere in between.
func TestTheGenerationFourDownRefusesWhilePullRowsArePending(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "carrier4refuse", 101)
	defer cleanup()

	insertPullRow(t, ctx, db, "pull_jobs", 4)
	insertPullRow(t, ctx, db, "pull_tests", 4)

	err := goose.DownToContext(ctx, db, "migrations", 100)
	if err == nil {
		t.Fatal("the Down SUCCEEDED with generation-4 rows pending: a rollback issued before the " +
			"TTL would have discarded queued jobs and in-flight Test Connections")
	}
	msg := err.Error()

	// Mutation killed: delete the guarded DO-block and let ADD CONSTRAINT fail on its own. It still
	// fails closed, so an assertion of "the Down errored" passes — while the operator is left with
	// "check constraint ... is violated by some row" and no procedure. These are the words that
	// make the refusal actionable, so they are asserted, not the mere failure.
	for _, want := range []string{
		"refusing to roll back carrier generation 4",
		"2 pending row(s)",
		"1 in pull_jobs",
		"1 in pull_tests",
		"wait out the job TTL",
		"purge them explicitly",
		"D-0160",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q; an operator reading it cannot act.\nmessage: %s", want, msg)
		}
	}

	// Mutation killed: "drain first" — DELETE the pending rows and then narrow. The Down then
	// SUCCEEDS and the jobs are gone, which no assertion on the error can see.
	if n := pullCarrierCount(t, ctx, db, "pull_jobs", 4); n != 1 {
		t.Errorf("pull_jobs holds %d generation-4 row(s) after the refusal, want 1: the Down discarded work", n)
	}
	if n := pullCarrierCount(t, ctx, db, "pull_tests", 4); n != 1 {
		t.Errorf("pull_tests holds %d generation-4 row(s) after the refusal, want 1: the Down discarded work", n)
	}

	// The refusal is atomic: goose runs the migration in a transaction, so the RAISE rolls the whole
	// Down back and the boundary is still the widened one. A half-applied rollback would leave rows
	// that no longer satisfy their own table's CHECK.
	for _, name := range []string{"pull_jobs_protocol_version_check", "pull_tests_protocol_version_check"} {
		if def := pullCarrierConstraintDef(t, ctx, db, name); !strings.Contains(def, "4") {
			t.Errorf("%s is %q after the refusal: the failed Down narrowed the boundary anyway", name, def)
		}
	}
}

// Only one table holds rows. The count must still name BOTH, including the zero — an operator who
// reads "1 pending row" and does not learn where it is has to go looking, and jobs and Test
// Connections drain differently.
func TestTheRefusalTellsTheOperatorWhichTableHoldsTheRows(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "carrier4which", 101)
	defer cleanup()

	insertPullRow(t, ctx, db, "pull_tests", 4)

	err := goose.DownToContext(ctx, db, "migrations", 100)
	if err == nil {
		t.Fatal("the Down succeeded with a generation-4 row pending in pull_tests")
	}
	msg := err.Error()
	for _, want := range []string{"1 pending row(s)", "0 in pull_jobs", "1 in pull_tests"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q.\nmessage: %s", want, msg)
		}
	}
}

// The converse, without which the guard would be indistinguishable from one that always refuses:
// with nothing pending the Down runs, narrows both CHECKs so generation 4 is rejected again, and
// the Up converges back.
func TestTheGenerationFourDownSucceedsWhenNothingIsPending(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "carrier4drained", 101)
	defer cleanup()

	// Generations that are NOT 4 must not hold the rollback: the guard counts generation 4 alone.
	insertPullRow(t, ctx, db, "pull_jobs", 3)
	insertPullRow(t, ctx, db, "pull_tests", 1)

	if err := goose.DownToContext(ctx, db, "migrations", 100); err != nil {
		t.Fatalf("the Down refused with nothing pending at generation 4: %v", err)
	}
	for _, table := range []string{"pull_jobs", "pull_tests"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO `+table+` (region, payload, expires_at, protocol_version)
			 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', 4)`); err == nil {
			t.Errorf("%s accepted a generation-4 row after the Down: the boundary was dropped, not narrowed", table)
		}
	}
	// The rows at other generations survived a Down that had every reason not to touch them.
	if n := pullCarrierCount(t, ctx, db, "pull_jobs", 3); n != 1 {
		t.Errorf("the Down removed a generation-3 pull_jobs row it does not own (%d left)", n)
	}
	if n := pullCarrierCount(t, ctx, db, "pull_tests", 1); n != 1 {
		t.Errorf("the Down removed a generation-1 pull_tests row it does not own (%d left)", n)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 101); err != nil {
		t.Fatalf("goose up 100 → 101 after the Down: %v", err)
	}
	for _, name := range []string{"pull_jobs_protocol_version_check", "pull_tests_protocol_version_check"} {
		if def := pullCarrierConstraintDef(t, ctx, db, name); !strings.Contains(def, "4") {
			t.Errorf("%s is %q after Down and Up: the schema did not converge", name, def)
		}
	}
}

// 00063's stated reason for widening rather than dropping: a row carrying a generation nobody
// declares would be invisible to every agent, because the claim predicate selects by
// `protocol_version <= capability`. The boundary has to keep rejecting the unknown.
func TestTheWidenedCheckStillRejectsAnUnknownGeneration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "carrier4unknown", 101)
	defer cleanup()

	for _, table := range []string{"pull_jobs", "pull_tests"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO `+table+` (region, payload, expires_at, protocol_version)
			 VALUES ('core', '{}'::jsonb, now() + interval '10 minutes', 5)`); err == nil {
			t.Errorf("%s accepted generation 5: the CHECK was dropped rather than widened, and a row "+
				"no agent can claim is a silent blackhole", table)
		}
	}
}

// A source scan, because "no destructive delete" is a property of the FILE that a future edit can
// break while every behavioural test above still passes — the pending rows would simply be gone
// before the guard counted them. This is the shape 00063 warned about, asserted directly.
func TestTheGenerationFourDownContainsNoDestructiveStatement(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("migrations", "00101_pull_carrier_generation_4.sql"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(body), "-- +goose Down", 2)
	if len(parts) != 2 {
		t.Fatal("00101 has no `-- +goose Down` section, so it has no rollback to guard")
	}
	// Comments explain the destructive draft this migration refuses to be, so only executable
	// lines are scanned.
	for i, line := range strings.Split(parts[1], "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "--") {
			continue
		}
		upper := strings.ToUpper(code)
		for _, banned := range []string{"DELETE", "TRUNCATE", "DROP TABLE"} {
			if strings.Contains(upper, banned) {
				t.Errorf("00101's Down line %d contains %s: %q. D-0160 makes draining an explicit "+
					"operator step; a rollback that discards pending work is a write-off wearing "+
					"the words of a safe rollback (00063)", i+1, banned, code)
			}
		}
	}
}
