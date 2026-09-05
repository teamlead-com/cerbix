package store

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase B2 — the schema assertions. Six invariants in this file are properties of the
// SCHEMA rather than of any code path, and the audit (§17.2) classifies them that way because a
// behavioural test cannot see them: a CHECK that must REJECT, a column that must be ABSENT, an
// index that must not cover a set of columns, a storage parameter on a relation nobody has
// inserted into yet.

// Invariant 10e — `carrier_generation IS NULL` exactly when `job_id IS NULL`, enforced by a CHECK.
// A window never dispatched has no carrier, which is a DIFFERENT fact from an old one, and
// `DEFAULT 0` was wrong twice over: it manufactured a value nobody set and it collapsed the two.
//
// Both directions are asserted, and then the two LEGAL combinations, because a CHECK that refuses
// everything would pass a test that only tried the illegal pairs.
func TestTheCarrierIsPresentExactlyWhenAJobIs(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "carrieriff")
	m := ledgerMonitor(t, st, ctx, proj, "iff", 60)
	now := time.Now().UTC().Truncate(time.Second)

	insert := func(dueAt time.Time, jobID *string, carrier *int) error {
		_, err := st.pool.Exec(ctx,
			`INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id, execution_revision,
			                            region, carrier_generation, interval_seconds)
			 VALUES ($1, $2, $3, $4::uuid, $5, $6, $7, 60)`,
			proj, m.ID, dueAt, jobID, m.ExecutionRevision, m.Region, carrier)
		return err
	}
	job, carrier := ledgerJobA, domain.LedgerMinCarrier

	if err := insert(now.Add(-4*time.Minute), &job, nil); err == nil {
		t.Error("a job with no carrier generation was accepted: the row cannot say which carrier it rode")
	} else if !strings.Contains(err.Error(), "expected_runs_carrier_iff_job") {
		t.Errorf("the refusal is not the biconditional CHECK: %v", err)
	}
	if err := insert(now.Add(-3*time.Minute), nil, &carrier); err == nil {
		t.Error("a carrier with no job was accepted: the row claims a dispatch that never happened")
	} else if !strings.Contains(err.Error(), "expected_runs_carrier_iff_job") {
		t.Errorf("the refusal is not the biconditional CHECK: %v", err)
	}
	// The converses. Without them, a CHECK of `false` would pass everything above.
	if err := insert(now.Add(-2*time.Minute), &job, &carrier); err != nil {
		t.Errorf("a dispatched window was refused: %v", err)
	}
	if err := insert(now.Add(-1*time.Minute), nil, nil); err != nil {
		t.Errorf("a never-dispatched window was refused: %v", err)
	}
}

// Invariant 19 — NO verdict is stored. Every verdict is computed from the timestamps present,
// which is what lets a late terminal change a window's reading with nothing to keep consistent.
//
// Asserted as an absence over the column SET rather than by naming one column, because the way
// this rule breaks is someone ADDING a convenient `verdict` or `status` column, and a check for a
// specific name would not see `state`.
func TestNoVerdictIsStoredOnAWindow(t *testing.T) {
	st, ctx := ledgerStore(t)
	rows, err := st.pool.Query(ctx,
		`SELECT column_name FROM information_schema.columns
		  WHERE table_name = 'expected_runs' ORDER BY column_name`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	// The ENUMERATED set, not a blocklist of names someone might have chosen. A count would be a
	// proxy that a same-count edit walks through.
	want := []string{
		"carrier_generation", "claimed_at", "due_at", "execution_revision", "interval_assumed",
		"interval_seconds", "issued_at", "job_id", "monitor_id", "outcome", "project_id",
		"refused_at", "refused_reason", "region", "skip_reason", "terminal_at",
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected_runs columns changed.\n got: %v\nwant: %v\n"+
			"A new column here is either a stored verdict, which invariant 19 forbids, or a fact "+
			"that needs its own place in §6.1 — either way it is a decision and not a detail.", got, want)
	}
}

// Invariant 12 — `heartbeats` gains NO column, and the six fill columns appear in NO index.
//
// The first half is what §4a required: absence is the case the ledger exists to preserve, and a row
// that only exists where something happened cannot record a run that produced none. The second is
// what makes an update to those columns eligible for HOT at all — a `fillfactor` with an indexed
// fill column is free space nothing can use.
func TestTheLedgerTouchesNoHeartbeatColumnAndIndexesNoFillColumn(t *testing.T) {
	st, ctx := ledgerStore(t)
	for _, forbidden := range []string{"due_at", "job_id", "job_issued_at", "expected_run_id"} {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'heartbeats' AND column_name = $1`, forbidden).Scan(&n); err != nil {
			t.Fatalf("read heartbeat columns: %v", err)
		}
		if n != 0 {
			t.Errorf("heartbeats grew a %q column: the ledger must not be built on a table that "+
				"only has rows where something happened", forbidden)
		}
	}
	// The fill columns, read from pg_index rather than from a name convention.
	rows, err := st.pool.Query(ctx,
		`SELECT i.indexrelid::regclass::text, a.attname
		   FROM pg_index i
		   JOIN pg_class c ON c.oid = i.indrelid
		   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		  WHERE c.relname = 'expected_runs'`)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	defer rows.Close()
	fill := map[string]bool{
		"claimed_at": true, "terminal_at": true, "outcome": true,
		"refused_at": true, "refused_reason": true, "skip_reason": true,
	}
	for rows.Next() {
		var index, column string
		if err := rows.Scan(&index, &column); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		if fill[column] {
			t.Errorf("index %s covers the fill column %q, so an update to it can never be HOT and "+
				"the fillfactor buys nothing", index, column)
		}
	}
}

// Invariant 26 — every ledger row is reachable only within its tenant, through the COMPOSITE
// `(monitor_id, project_id)` foreign key this repository already uses in five migrations. A
// single-column reference would let a row outlive its tenant's isolation boundary.
//
// The assertion reads the constraint's COLUMN SET, and the negative half is stated as the
// mutation: a single-column FK has one column and fails the same assertion.
func TestTheLedgerForeignKeysAreComposite(t *testing.T) {
	st, ctx := ledgerStore(t)
	for _, table := range []string{"expected_runs", "monitor_schedule"} {
		rows, err := st.pool.Query(ctx,
			`SELECT c.conname, array_agg(a.attname ORDER BY a.attname)
			   FROM pg_constraint c
			   JOIN pg_class t ON t.oid = c.conrelid
			   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
			  WHERE t.relname = $1 AND c.contype = 'f'
			  GROUP BY c.conname`, table)
		if err != nil {
			t.Fatalf("read constraints for %s: %v", table, err)
		}
		found := 0
		for rows.Next() {
			var name string
			var columns []string
			if err := rows.Scan(&name, &columns); err != nil {
				rows.Close()
				t.Fatalf("scan constraint: %v", err)
			}
			found++
			sort.Strings(columns)
			if strings.Join(columns, ",") != "monitor_id,project_id" {
				t.Errorf("%s.%s references (%v); want the composite (monitor_id, project_id) — a "+
					"single-column reference lets a row outlive its tenant boundary",
					table, name, columns)
			}
		}
		rows.Close()
		if found == 0 {
			t.Errorf("%s has no foreign key to monitors at all", table)
		}
	}
}

// Invariant 16a — an `expected_runs` insert is NEVER lost to a missing partition: the DEFAULT
// partition accepts it. The direction is required rather than convenient: a row here is evidence
// that a run did not complete, so a lost insert would erase exactly the fact the ledger exists to
// keep — silently, and toward OVER-claiming.
//
// This is where `expected_runs` deliberately diverges from the gate decision ledger, whose own
// tests assert no DEFAULT partition exists anywhere in it. The two are opposite for a reason and
// the reason is directional, so the divergence is asserted rather than assumed.
func TestAWindowFarOutsideEveryDailyPartitionStillLands(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "defaultpart")
	m := ledgerMonitor(t, st, ctx, proj, "far-future", 60)

	// Two years out: no daily partition exists or ever will before retention drops it.
	far := time.Now().UTC().AddDate(2, 0, 0).Truncate(time.Second)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO expected_runs (project_id, monitor_id, due_at, execution_revision, region,
		                            interval_seconds)
		 VALUES ($1, $2, $3, $4, $5, 60)`,
		proj, m.ID, far, m.ExecutionRevision, m.Region); err != nil {
		t.Fatalf("a window with no matching daily partition was REFUSED: %v — the evidence that a "+
			"run did not complete would be lost at the moment it is created", err)
	}
	var landedIn string
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM expected_runs WHERE monitor_id = $1 AND due_at = $2`,
		m.ID, far).Scan(&landedIn); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if landedIn != "expected_runs_default" {
		t.Errorf("the row landed in %s, want the DEFAULT partition", landedIn)
	}
}

// Invariant 23a — every PARTITION is created with `fillfactor = 70`, asserted by reading
// `pg_class.reloptions` on a NEWLY created one.
//
// Parent DDL proves nothing: in PostgreSQL storage parameters are per-relation and
// `CREATE TABLE ... PARTITION OF` does not inherit them, so a `WITH (fillfactor = 70)` on the
// partitioned parent applies to nothing that ever holds a row. That defect was found in the spec's
// own DDL, which is why the assertion is on a partition the test just made rather than on the
// migration's output.
func TestANewlyCreatedPartitionCarriesTheFillfactor(t *testing.T) {
	st, ctx := ledgerStore(t)
	// A day well beyond the maintenance window, so this test creates the partition itself and is
	// not reading one the migration or an earlier run made.
	day := time.Now().UTC().AddDate(0, 0, 30).Truncate(24 * time.Hour)
	name := expectedRunPartitionPrefix + day.Format("20060102")
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
		t.Fatalf("clean up a previous run's partition: %v", err)
	}
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF expected_runs FOR VALUES FROM ('%s') TO ('%s')
		   WITH (fillfactor = %d)`,
		name, day.Format(pgTimestamp), day.AddDate(0, 0, 1).Format(pgTimestamp),
		expectedRunFillfactor)); err != nil {
		t.Fatalf("create partition: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name))
	})

	var options []string
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE(reloptions, '{}') FROM pg_class WHERE relname = $1`, name).Scan(&options); err != nil {
		t.Fatalf("read reloptions: %v", err)
	}
	want := fmt.Sprintf("fillfactor=%d", expectedRunFillfactor)
	for _, opt := range options {
		if opt == want {
			return
		}
	}
	t.Errorf("partition %s carries reloptions %v, want %q — a fillfactor set only on the "+
		"partitioned parent applies to no relation that holds a row", name, options, want)
}

// The partition MAINTAINER is what production relies on, so it is asserted through the exported
// call rather than through hand-written DDL: `EnsureExpectedRunPartitions` must produce partitions
// carrying the storage parameter, or the assertion above is testing the test's own SQL.
func TestThePartitionMaintainerSetsTheFillfactorItself(t *testing.T) {
	st, ctx := ledgerStore(t)
	if err := st.EnsureExpectedRunPartitions(ctx, 2); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	day := time.Now().UTC().AddDate(0, 0, 2).Truncate(24 * time.Hour)
	name := expectedRunPartitionPrefix + day.Format("20060102")
	var options []string
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE(reloptions, '{}') FROM pg_class WHERE relname = $1`, name).Scan(&options); err != nil {
		t.Fatalf("the maintainer did not create %s: %v", name, err)
	}
	want := fmt.Sprintf("fillfactor=%d", expectedRunFillfactor)
	for _, opt := range options {
		if opt == want {
			return
		}
	}
	t.Errorf("the maintainer created %s with reloptions %v, want %q", name, options, want)
}

// A non-positive interval can never reach the schedule or a window, and the reason is not
// tidiness: §7.1 spaces a gap with `make_interval(secs => interval_in_force)` and PostgreSQL
// raises `step size cannot equal zero` for a zero step — which would abort ONE statement for the
// whole tick, taking every other monitor's evidence with it. Refusing the value at the write is
// the fail-closed direction; discovering it at dispatch time is not.
func TestANonPositiveIntervalIsRefusedAtTheWrite(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "zerointerval")
	m := ledgerMonitor(t, st, ctx, proj, "zero", 60)
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitor_schedule SET interval_in_force = 0 WHERE monitor_id = $1`, m.ID); err == nil {
		t.Error("a zero interval_in_force was accepted; the next advance would abort the whole tick")
	} else if !strings.Contains(err.Error(), "monitor_schedule_interval_positive") {
		t.Errorf("the refusal is not the interval CHECK: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO expected_runs (project_id, monitor_id, due_at, execution_revision, region,
		                            interval_seconds)
		 VALUES ($1, $2, statement_timestamp(), $3, $4, 0)`,
		proj, m.ID, m.ExecutionRevision, m.Region); err == nil {
		t.Error("a window with a zero lateness threshold was accepted; every run would read covered_late")
	} else if !strings.Contains(err.Error(), "expected_runs_interval_positive") {
		t.Errorf("the refusal is not the interval CHECK: %v", err)
	}
}
