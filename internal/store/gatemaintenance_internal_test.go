package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Decision-ledger maintenance (func-reliability-gate D10; §7 "Partitions"). These tests run the
// REAL pass — session, statements, catalog — against the test database and observe the catalog
// and the registry, never the code's own bookkeeping alone. Helpers here are shared by the
// timeline, ownership and release files.

// gateMaintCfg is the default configuration of these tests: retention 7 d, lead 7 d, create 3,
// a 5 m cadence, 8 stage-ops.
var gateMaintCfg = GateMaintenanceConfig{
	RetentionDays: 7, LeadDays: 7, CreateMax: 3, PurgeEvery: 5 * time.Minute, PurgeMaxPartitions: 8,
}

// gateMaintStore opens the test database and restores the ledger to its migration baseline —
// [today, today+7] attached and registered, nothing else — before and after the test.
func gateMaintStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run gate maintenance tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	resetGateLedger(t, st, ctx)
	t.Cleanup(func() { resetGateLedger(t, st, ctx) })
	return st, ctx
}

// resetGateLedger finalizes any pending detach, drops every ledger partition (attached or
// standalone), empties the registry and re-runs the migration's bootstrap block.
func resetGateLedger(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		DO $$
		DECLARE r record;
		BEGIN
			FOR r IN SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			          WHERE i.inhparent = 'service_gate_decisions'::regclass AND i.inhdetachpending LOOP
				EXECUTE format('ALTER TABLE service_gate_decisions DETACH PARTITION %I FINALIZE', r.relname);
			END LOOP;
			FOR r IN SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			          WHERE i.inhparent = 'service_gate_decisions'::regclass LOOP
				EXECUTE format('DROP TABLE %I', r.relname);
			END LOOP;
			FOR r IN SELECT c.relname FROM pg_class c
			          WHERE c.relnamespace = (SELECT relnamespace FROM pg_class WHERE oid = 'service_gate_decisions'::regclass)
			            AND c.relkind = 'r'
			            AND (c.relname ~ '^service_gate_decisions_p' OR obj_description(c.oid, 'pg_class') LIKE 'cerbix:gate-ledger:%') LOOP
				EXECUTE format('DROP TABLE %I', r.relname);
			END LOOP;
			DELETE FROM service_gate_decision_partitions;
		END $$`); err != nil {
		t.Fatalf("reset ledger: %v", err)
	}
	src, err := migrationsFS.ReadFile("migrations/00093_reliability_gate.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	begin := strings.Index(text, "-- gate-ledger-bootstrap:begin")
	end := strings.Index(text, "-- gate-ledger-bootstrap:end")
	if begin < 0 || end < begin {
		t.Fatal("the bootstrap block markers are gone from 00093")
	}
	if _, err := st.pool.Exec(ctx, text[begin:end]); err != nil {
		t.Fatalf("re-run bootstrap: %v", err)
	}
}

// gateMetricsFake counts the maintenance error family.
type gateMetricsFake struct {
	mu     sync.Mutex
	counts map[string]int
}

func (m *gateMetricsFake) RecordGateMaintenanceError(kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch kind {
	case "lock_timeout", "statement_timeout", "partition_identity", "error":
	default:
		return fmt.Errorf("kind %q outside the closed set", kind)
	}
	if m.counts == nil {
		m.counts = map[string]int{}
	}
	m.counts[kind]++
	return nil
}

func (m *gateMetricsFake) get(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[kind]
}

// fakeClock is an injectable clock advanced by the test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(at time.Time) *fakeClock { return &fakeClock{now: at} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// offsetClock is the real clock shifted by a constant: real blocking with a fake position in the
// timeline.
func offsetClock(d time.Duration) func() time.Time {
	return func() time.Time { return time.Now().Add(d) }
}

// runGatePass runs one full lifecycle (acquire, work, release proof) with passStart = clock().
func runGatePass(t *testing.T, st *Store, ctx context.Context, cfg GateMaintenanceConfig, clock func() time.Time, m *gateMetricsFake, hooks *gateMaintenanceHooks) (GateMaintenanceReport, bool, error) {
	t.Helper()
	if clock == nil {
		clock = time.Now
	}
	return st.runGateLedgerMaintenancePass(ctx, clock(), cfg, clock, m, hooks)
}

// gateToday is the DATABASE's UTC day.
func gateToday(t *testing.T, st *Store, ctx context.Context) time.Time {
	t.Helper()
	var d time.Time
	if err := st.pool.QueryRow(ctx, `SELECT (now() AT TIME ZONE 'UTC')::date`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

// plantGateDay builds a day EXACTLY as the migration does (standalone LIKE + day CHECK, local
// unique, marker, registry `created`) and then brings it to `state`: attached (attach + state),
// detached (standalone, detached_at = detachedAt or now()), or created (left standalone).
func plantGateDay(t *testing.T, st *Store, ctx context.Context, day time.Time, state string, detachedAt *time.Time) string {
	t.Helper()
	rel := gateRelname(day)
	lo, hi := gateDayBounds(day)
	var token string
	if err := st.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&token); err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE %s (LIKE service_gate_decisions INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES, `+
			`CONSTRAINT %s_day_chk CHECK (evaluated_at >= '%s'::timestamptz AND evaluated_at < '%s'::timestamptz))`, rel, rel, lo, hi),
		fmt.Sprintf(`CREATE UNIQUE INDEX %s_id_uniq ON %s (id)`, rel, rel),
		fmt.Sprintf(`COMMENT ON TABLE %s IS 'cerbix:gate-ledger:%s'`, rel, token),
		fmt.Sprintf(`INSERT INTO service_gate_decision_partitions (day, relname, owner_token, relid, state, created_at)
			VALUES ('%s', '%s', '%s', to_regclass('%s')::oid, 'created', now())`, day.Format("2006-01-02"), rel, token, rel),
	}
	switch state {
	case "attached":
		stmts = append(stmts,
			fmt.Sprintf(`ALTER TABLE service_gate_decisions ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')`, rel, lo, hi),
			fmt.Sprintf(`UPDATE service_gate_decision_partitions SET state = 'attached', attached_at = now() WHERE day = '%s'`, day.Format("2006-01-02")))
	case "detached":
		at := "now()"
		if detachedAt != nil {
			at = "'" + detachedAt.UTC().Format(time.RFC3339Nano) + "'::timestamptz"
		}
		stmts = append(stmts,
			fmt.Sprintf(`UPDATE service_gate_decision_partitions SET state = 'detached', attached_at = now() - interval '1 day', detached_at = %s WHERE day = '%s'`, at, day.Format("2006-01-02")))
	case "created":
	default:
		t.Fatalf("plantGateDay: unknown state %q", state)
	}
	for _, q := range stmts {
		if _, err := st.pool.Exec(ctx, q); err != nil {
			t.Fatalf("plant %s (%s): %v\n%s", rel, state, err, q)
		}
	}
	return token
}

// dropGateDay removes a day from the ledger and the registry entirely (a "missing day").
func dropGateDay(t *testing.T, st *Store, ctx context.Context, day time.Time) {
	t.Helper()
	rel := gateRelname(day)
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, rel)); err != nil {
		t.Fatalf("drop %s: %v", rel, err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_gate_decision_partitions WHERE day = $1::date`, day.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
}

// gateCatalog is one day's catalog truth.
type gateCatalog struct {
	exists, attached, pending bool
	state                     string // registry state, "" when unregistered
	relid, oid                uint32
}

func gateLookup(t *testing.T, st *Store, ctx context.Context, day time.Time) gateCatalog {
	t.Helper()
	rel := gateRelname(day)
	var c gateCatalog
	var state *string
	var relid *uint32
	if err := st.pool.QueryRow(ctx, `
		SELECT to_regclass($1) IS NOT NULL,
		       COALESCE((SELECT true FROM pg_inherits WHERE inhrelid = to_regclass($1) AND inhparent = 'service_gate_decisions'::regclass), false),
		       COALESCE((SELECT inhdetachpending FROM pg_inherits WHERE inhrelid = to_regclass($1) AND inhparent = 'service_gate_decisions'::regclass), false),
		       (SELECT state FROM service_gate_decision_partitions WHERE day = $2::date),
		       (SELECT relid FROM service_gate_decision_partitions WHERE day = $2::date),
		       COALESCE(to_regclass($1)::oid, 0)`,
		rel, day.Format("2006-01-02")).Scan(&c.exists, &c.attached, &c.pending, &state, &relid, &c.oid); err != nil {
		t.Fatalf("lookup %s: %v", rel, err)
	}
	if state != nil {
		c.state = *state
	}
	if relid != nil {
		c.relid = *relid
	}
	return c
}

// gateRegistryAgreesWithCatalog fails when any non-dropped registry row disagrees with the
// catalog: relation present with our marker, relid current, attached iff state = attached.
func gateRegistryAgreesWithCatalog(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT p.day, p.state,
		       to_regclass(p.relname) IS NOT NULL,
		       to_regclass(p.relname)::oid IS NOT DISTINCT FROM p.relid,
		       obj_description(p.relid, 'pg_class') IS NOT DISTINCT FROM 'cerbix:gate-ledger:' || p.owner_token::text,
		       EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = p.relid AND inhparent = 'service_gate_decisions'::regclass),
		       COALESCE((SELECT inhdetachpending FROM pg_inherits WHERE inhrelid = p.relid), false)
		  FROM service_gate_decision_partitions p WHERE p.state <> 'dropped' ORDER BY p.day`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var day time.Time
		var state string
		var exists, relidOK, marked, attached, pending bool
		if err := rows.Scan(&day, &state, &exists, &relidOK, &marked, &attached, &pending); err != nil {
			t.Fatal(err)
		}
		d := day.Format("2006-01-02")
		if !exists || !relidOK || !marked {
			t.Errorf("%s (%s): exists=%v relid-current=%v marked=%v", d, state, exists, relidOK, marked)
		}
		if pending {
			t.Errorf("%s (%s): detach pending left in the catalog", d, state)
		}
		switch state {
		case "attached":
			if !attached {
				t.Errorf("%s: registered attached but not a partition", d)
			}
		case "created", "detached":
			if attached {
				t.Errorf("%s: registered %s but attached", d, state)
			}
		default:
			t.Errorf("%s: non-terminal state %q after convergence", d, state)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// holdLock opens a transaction on a fresh connection holding `mode` on the named relation(s)
// and returns a release func. The lock is what a blocked ATTACH/DETACH waits behind.
func holdLock(t *testing.T, st *Store, ctx context.Context, mode string, rels ...string) func() {
	t.Helper()
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`LOCK TABLE %s IN %s MODE`, strings.Join(rels, ", "), mode)); err != nil {
		t.Fatalf("lock %v %s: %v", rels, mode, err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback(context.Background())
			conn.Release()
		})
	}
	t.Cleanup(release)
	return release
}

// pgCodeOf extracts a SQLSTATE.
func pgCodeOf(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// §7 Partitions — with lead 7, five days missing and create_max = 3, the pass creates the three
// NEAREST days (today+3, +4, +5) and the next pass the other two; every created day is built
// standalone (its day CHECK is on the child) and ends attached AND registered attached.
func TestGateMaintCreatesNearestMissingDaysFirst(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	for i := 3; i <= 7; i++ {
		dropGateDay(t, st, ctx, today.AddDate(0, 0, i))
	}
	m := &gateMetricsFake{}
	rep, acquired, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil || !acquired {
		t.Fatalf("pass 1: acquired=%v err=%v", acquired, err)
	}
	if rep.Created != 3 || rep.Attached != 3 {
		t.Fatalf("pass 1 created=%d attached=%d, want 3/3: %+v", rep.Created, rep.Attached, rep)
	}
	for i := 3; i <= 5; i++ {
		c := gateLookup(t, st, ctx, today.AddDate(0, 0, i))
		if !c.exists || !c.attached || c.state != "attached" || c.relid != c.oid {
			t.Fatalf("today+%d after pass 1: %+v, want attached and registered attached with a current relid", i, c)
		}
	}
	for i := 6; i <= 7; i++ {
		if c := gateLookup(t, st, ctx, today.AddDate(0, 0, i)); c.exists || c.state != "" {
			t.Fatalf("today+%d was created ahead of nearer days: %+v", i, c)
		}
	}
	// The standalone build left the day CHECK on each new child, and no DEFAULT partition exists.
	var dayChecks, defaults int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint con JOIN service_gate_decision_partitions p ON p.relid = con.conrelid
		 WHERE con.contype = 'c' AND con.conname = p.relname || '_day_chk' AND p.day > $1::date`, today.AddDate(0, 0, 2).Format("2006-01-02")).Scan(&dayChecks); err != nil {
		t.Fatal(err)
	}
	if dayChecks != 3 {
		t.Fatalf("%d of 3 created children carry their day CHECK", dayChecks)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'service_gate_decisions'::regclass AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 0 {
		t.Fatal("a DEFAULT partition appeared")
	}
	rep, _, err = runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil || rep.Created != 2 || rep.Attached != 2 {
		t.Fatalf("pass 2 created=%d attached=%d err=%v, want the remaining two", rep.Created, rep.Attached, err)
	}
	gateRegistryAgreesWithCatalog(t, st, ctx)
	if len(rep.Refusals) != 0 || m.get("partition_identity") != 0 {
		t.Fatalf("refusals on a healthy ledger: %v", rep.Refusals)
	}
	if !rep.GaugesValid || rep.Gauges.WritableHorizonSeconds < 7*86400 {
		t.Fatalf("horizon after refill: %+v", rep.Gauges)
	}
}

// §7 — the attach needs no ACCESS EXCLUSIVE: while a decision insert holds its row lock in an
// open transaction (ROW EXCLUSIVE on the parent), the standalone build + ATTACH completes.
func TestGateMaintAttachCompletesWhileADecisionInsertHoldsItsRowLock(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	proj, _, svc := seedService(t, st, ctx)
	dropGateDay(t, st, ctx, today.AddDate(0, 0, 7))

	// The insert stays uncommitted for the whole pass.
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	at := today.Add(time.Hour)
	allow := "ALLOW"
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_gate_decisions (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
		     policy_revision, window_name, policy_snapshot, evaluated_at)
		VALUES ($1, $2, $3, 'checkout', 'Checkout', 'ALLOW', $4, '[]', '{}', 1, '30d', '{}', $5)`,
		gateUUIDv7(t, gateMs(at)), proj, svc, allow, at); err != nil {
		t.Fatalf("open insert: %v", err)
	}
	done := make(chan struct{})
	var rep GateMaintenanceReport
	var perr error
	go func() {
		defer close(done)
		rep, _, perr = runGatePass(t, st, ctx, gateMaintCfg, nil, &gateMetricsFake{}, nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass did not complete while a decision insert was open: the attach waited on it")
	}
	if perr != nil || rep.Attached != 1 || len(rep.Failures) != 0 {
		t.Fatalf("pass: attached=%d failures=%v err=%v", rep.Attached, rep.Failures, perr)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the insert could not commit after the attach: %v", err)
	}
	if c := gateLookup(t, st, ctx, today.AddDate(0, 0, 7)); !c.attached {
		t.Fatalf("today+7 not attached: %+v", c)
	}
}

// §7 — CREATE … PARTITION OF (ACCESS EXCLUSIVE on the parent) is not in the gate code: the
// production gate*.go files and migration 00093 are grepped.
func TestGateMaintNoPartitionOfAnywhereInTheGateCode(t *testing.T) {
	files, err := filepath.Glob("gate*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "migrations/00093_reliability_gate.sql")
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Comments may NAME the forbidden statement; code may not contain it. DDL in this
		// codebase is uppercase (CREATE TABLE … PARTITION OF); the legitimate lowercase uses are
		// error-message substrings ("no partition of relation", "a partition of the ledger"), so
		// the check is case-SENSITIVE over comment-stripped source.
		if strings.Contains(stripComments(string(src), strings.HasSuffix(f, ".sql")), "PARTITION OF") {
			t.Errorf("%s contains CREATE … PARTITION OF in code; a day is built standalone and ATTACHed (D10)", f)
		}
		checked++
	}
	if checked < 2 {
		t.Fatalf("only %d files checked; the gate store code is not where this test looks", checked)
	}
}

// §7 — retention 7 d: a partition whose upper bound is 8 d old is DETACHED CONCURRENTLY; the one
// at 6 d is not; afterwards no attached partition has an upper bound at or before the cutoff. The
// detached table still exists (NOT dropped on the pass that detached it).
func TestGateMaintDetachesOnlyPastTheCutoff(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	old := today.AddDate(0, 0, -9)    // upper bound today-8: ≤ now − 7 d
	recent := today.AddDate(0, 0, -7) // upper bound today-6: > now − 7 d
	plantGateDay(t, st, ctx, old, "attached", nil)
	plantGateDay(t, st, ctx, recent, "attached", nil)

	m := &gateMetricsFake{}
	rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Detached != 1 || rep.Dropped != 0 || len(rep.Failures) != 0 {
		t.Fatalf("detached=%d dropped=%d failures=%v", rep.Detached, rep.Dropped, rep.Failures)
	}
	if c := gateLookup(t, st, ctx, old); !c.exists || c.attached || c.state != "detached" {
		t.Fatalf("old day after the pass: %+v, want standalone + detached", c)
	}
	if c := gateLookup(t, st, ctx, recent); !c.attached || c.state != "attached" {
		t.Fatalf("recent day after the pass: %+v, want still attached", c)
	}
	var stale int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_gate_decision_partitions p
		  JOIN pg_inherits i ON i.inhrelid = p.relid AND i.inhparent = 'service_gate_decisions'::regclass
		 WHERE ((p.day + 1)::timestamp AT TIME ZONE 'UTC') <= now() - interval '7 days'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d attached partitions still have an upper bound at or before the cutoff", stale)
	}
	if !rep.GaugesValid || rep.Gauges.PendingDrop != 1 || rep.Gauges.OldestAgeSeconds != 0 {
		t.Fatalf("gauges after detach: %+v (want pending_drop 1 — the detached table — and oldest age 0)", rep.Gauges)
	}
}

// §7 — logical readability ends at the DETACH (the row answers through the parent up to the
// pass after eligibility, not after it); physical retention ends at the deferred DROP one pass
// later, measured against purge_every. A planted transaction older than the detach holds the
// drop back until it ends.
func TestGateMaintLogicalThenPhysicalRetentionAndTheSnapshotGate(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	proj, _, svc := seedService(t, st, ctx)
	old := today.AddDate(0, 0, -9)
	plantGateDay(t, st, ctx, old, "attached", nil)
	at := old.Add(12 * time.Hour)
	allow := "ALLOW"
	id := gateUUIDv7(t, gateMs(at))
	if err := insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, at, `{}`); err != nil {
		t.Fatalf("insert into the old day: %v", err)
	}
	readable := func() bool {
		var n int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decisions WHERE id = $1 AND evaluated_at = $2`, id, at).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !readable() {
		t.Fatal("the row is not readable before any pass")
	}
	// A transaction older than the detach, kept open across the detaching pass.
	older, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer older.Release()
	otx, err := older.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	cfg := gateMaintCfg
	cfg.PurgeEvery = time.Second
	m := &gateMetricsFake{}
	rep, _, err := runGatePass(t, st, ctx, cfg, nil, m, nil)
	if err != nil || rep.Detached != 1 || rep.Dropped != 0 {
		t.Fatalf("detaching pass: detached=%d dropped=%d err=%v", rep.Detached, rep.Dropped, err)
	}
	if readable() {
		t.Fatal("the row is still readable through the parent after the detach")
	}
	if c := gateLookup(t, st, ctx, old); !c.exists {
		t.Fatal("the detached table was dropped on the pass that detached it")
	}
	time.Sleep(1100 * time.Millisecond) // past purge_every: the drop is due
	rep, _, err = runGatePass(t, st, ctx, cfg, nil, m, nil)
	if err != nil || rep.Dropped != 0 {
		t.Fatalf("a drop ran while a transaction older than the detach was open: dropped=%d err=%v", rep.Dropped, err)
	}
	if c := gateLookup(t, st, ctx, old); !c.exists || c.state != "detached" {
		t.Fatalf("the snapshot gate did not hold the table: %+v", c)
	}
	if err := otx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rep, _, err = runGatePass(t, st, ctx, cfg, nil, m, nil)
	if err != nil || rep.Dropped != 1 {
		t.Fatalf("drop pass after the old transaction ended: dropped=%d err=%v failures=%v", rep.Dropped, err, rep.Failures)
	}
	if c := gateLookup(t, st, ctx, old); c.exists || c.state != "dropped" {
		t.Fatalf("after the drop: %+v, want the table gone and the registry dropped", c)
	}
	if m.get("error") != 0 || m.get("partition_identity") != 0 {
		t.Fatalf("errors counted on a healthy sequence: %v", m.counts)
	}
}

// §7 — the drop runs on the pass where detached_at is EXACTLY now − purge_every and not one
// second before (inclusive boundary, on the database clock; the registry's detached_at is the
// fake database clock).
func TestGateMaintDropBoundaryIsInclusiveOnTheDatabaseClock(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	day := today.AddDate(0, 0, -20)
	plantGateDay(t, st, ctx, day, "detached", nil)
	cfg := gateMaintCfg // purge_every 5 m

	// One second before the boundary: not due.
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_decision_partitions SET detached_at = now() - interval '5 minutes' + interval '1 second' WHERE day = $1::date`, day.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	rep, _, err := runGatePass(t, st, ctx, cfg, nil, &gateMetricsFake{}, nil)
	if err != nil || rep.Dropped != 0 {
		t.Fatalf("one second before the boundary: dropped=%d err=%v", rep.Dropped, err)
	}
	if c := gateLookup(t, st, ctx, day); !c.exists {
		t.Fatal("dropped one second early")
	}
	// Exactly the boundary (now() moves on by the time the pass compares, so this is the
	// inclusive edge or just past it — never before).
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_decision_partitions SET detached_at = now() - interval '5 minutes' WHERE day = $1::date`, day.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	rep, _, err = runGatePass(t, st, ctx, cfg, nil, &gateMetricsFake{}, nil)
	if err != nil || rep.Dropped != 1 {
		t.Fatalf("at the boundary: dropped=%d err=%v failures=%v", rep.Dropped, err, rep.Failures)
	}
	if c := gateLookup(t, st, ctx, day); c.exists || c.state != "dropped" {
		t.Fatalf("after the boundary drop: %+v", c)
	}
}

// makeDetachPending drives a day into the catalog's detach-pending state the way an interrupted
// detach does: a transaction holds ACCESS SHARE on the parent, so DETACH … CONCURRENTLY marks
// the partition pending, then waits for that transaction and is cancelled by lock_timeout.
func makeDetachPending(t *testing.T, st *Store, ctx context.Context, day time.Time) {
	t.Helper()
	release := holdLock(t, st, ctx, "ACCESS SHARE", "service_gate_decisions")
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET lock_timeout = 500`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE service_gate_decisions DETACH PARTITION %s CONCURRENTLY`, gateRelname(day)))
	if code := pgCodeOf(err); code != "55P03" {
		t.Fatalf("the detach was not cancelled by lock_timeout while waiting: %v", err)
	}
	if _, err := conn.Exec(ctx, `RESET lock_timeout`); err != nil {
		t.Fatal(err)
	}
	release()
	if c := gateLookup(t, st, ctx, day); !c.pending {
		t.Fatalf("the interrupted detach did not leave the partition detach-pending: %+v", c)
	}
}

// §7 — an interrupted detach (detach-pending in the catalog, `detaching` in the registry) is
// FINALIZEd on the next pass; the row ends `detached` with the relation standalone.
func TestGateMaintInterruptedDetachIsFinalizedNextPass(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	day := today.AddDate(0, 0, -12)
	plantGateDay(t, st, ctx, day, "attached", nil)
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_decision_partitions SET state = 'detaching' WHERE day = $1::date`, day.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	makeDetachPending(t, st, ctx, day)

	m := &gateMetricsFake{}
	rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil || rep.Finalized != 1 {
		t.Fatalf("finalized=%d err=%v failures=%v refusals=%v", rep.Finalized, err, rep.Failures, rep.Refusals)
	}
	if c := gateLookup(t, st, ctx, day); c.attached || c.pending || c.state != "detached" || !c.exists {
		t.Fatalf("after FINALIZE: %+v, want standalone + detached", c)
	}
	if m.get("partition_identity") != 0 {
		t.Fatal("a crash outcome was refused as an identity problem")
	}
}

// §7 — stage accounting: with purge_max = 3 and one finalize, one open drop and two eligible
// detaches pending, the pass does finalize, drop, the OLDEST detach, and stops.
func TestGateMaintStageAccountingFinalizeDropOldestDetachThenStop(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	pendingDay := today.AddDate(0, 0, -30)
	dropDay := today.AddDate(0, 0, -25)
	olderDetach := today.AddDate(0, 0, -20)
	newerDetach := today.AddDate(0, 0, -15)
	plantGateDay(t, st, ctx, pendingDay, "attached", nil)
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_decision_partitions SET state = 'detaching' WHERE day = $1::date`, pendingDay.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	makeDetachPending(t, st, ctx, pendingDay)
	ago := time.Now().Add(-time.Hour)
	plantGateDay(t, st, ctx, dropDay, "detached", &ago)
	plantGateDay(t, st, ctx, olderDetach, "attached", nil)
	plantGateDay(t, st, ctx, newerDetach, "attached", nil)

	cfg := gateMaintCfg
	cfg.PurgeMaxPartitions = 3
	rep, _, err := runGatePass(t, st, ctx, cfg, nil, &gateMetricsFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Finalized != 1 || rep.Dropped != 1 || rep.Detached != 1 || len(rep.Failures) != 0 {
		t.Fatalf("finalized=%d dropped=%d detached=%d failures=%v, want 1/1/1", rep.Finalized, rep.Dropped, rep.Detached, rep.Failures)
	}
	if c := gateLookup(t, st, ctx, pendingDay); c.state != "detached" || c.attached {
		t.Fatalf("finalize target: %+v", c)
	}
	if c := gateLookup(t, st, ctx, dropDay); c.exists || c.state != "dropped" {
		t.Fatalf("drop target: %+v", c)
	}
	if c := gateLookup(t, st, ctx, olderDetach); c.attached || c.state != "detached" {
		t.Fatalf("the oldest eligible detach did not run: %+v", c)
	}
	if c := gateLookup(t, st, ctx, newerDetach); !c.attached || c.state != "attached" {
		t.Fatalf("the fourth stage-op ran past the budget: %+v", c)
	}
	// pending_drop counts the two detached tables and the still-attached eligible one.
	if !rep.GaugesValid || rep.Gauges.PendingDrop != 3 || rep.Gauges.OldestAgeSeconds <= 0 {
		t.Fatalf("gauges: %+v (want pending_drop 3, a positive oldest age for the eligible attached day)", rep.Gauges)
	}
}

// §7 — a detach held behind a lock is refused by lock_timeout within 2 s, counted lock_timeout,
// and a decision insert during the wait commits (the parent is never locked by the pass). Two
// holders: ACCESS EXCLUSIVE on the child stops the day at its shape read (pg_get_expr needs
// ACCESS SHARE); SHARE UPDATE EXCLUSIVE on the parent lets the shape through and stops the
// DETACH … CONCURRENTLY itself. Both are one lock_timeout, both leave the day recoverable. The
// blocker is released the instant the refusal is counted, so the pass's later gauge read (which
// would also queue behind the held lock) does not inflate the measured statement wait.
func TestGateMaintLockTimeoutRefusalIsCountedAndInsertsCommitMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		rel   func(day string) string // what to lock: the child or the parent
		stage string
	}{
		{"ACCESS EXCLUSIVE", func(child string) string { return child }, "shape"},
		{"SHARE UPDATE EXCLUSIVE", func(string) string { return "service_gate_decisions" }, "detach"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			st, ctx := gateMaintStore(t)
			today := gateToday(t, st, ctx)
			proj, _, svc := seedService(t, st, ctx)
			day := today.AddDate(0, 0, -9)
			plantGateDay(t, st, ctx, day, "attached", nil)
			release := holdLock(t, st, ctx, tc.mode, tc.rel(gateRelname(day)))

			started := make(chan time.Time, 1)
			hooks := &gateMaintenanceHooks{beforeStatement: func(_ context.Context, stage string, _ dbConn) error {
				if stage == tc.stage {
					select {
					case started <- time.Now():
					default:
					}
				}
				return nil
			}}
			m := &gateMetricsFake{}
			type result struct {
				rep GateMaintenanceReport
				err error
			}
			res := make(chan result, 1)
			go func() {
				rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, hooks)
				res <- result{rep, err}
			}()
			var startedAt time.Time
			select {
			case startedAt = <-started:
			case <-time.After(10 * time.Second):
				t.Fatalf("the %s statement never started", tc.stage)
			}
			// The insert lands in today's partition while the statement waits on its lock.
			at := today.Add(2 * time.Hour)
			allow := "ALLOW"
			if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{}`); err != nil {
				t.Fatalf("a decision insert did not commit during the blocked %s: %v", tc.stage, err)
			}
			// The moment the refusal is counted, the individual statement has finished waiting.
			waitFor(t, 5*time.Second, "the lock_timeout refusal to be counted", func() bool { return m.get("lock_timeout") >= 1 })
			refusedAt := time.Now()
			release() // so the later gauge read does not queue behind the same lock
			r := <-res
			if r.err != nil {
				t.Fatalf("pass error: %v", r.err)
			}
			if m.get("lock_timeout") != 1 || r.rep.Detached != 0 {
				t.Fatalf("lock_timeout=%d detached=%d failures=%v, want one refusal", m.get("lock_timeout"), r.rep.Detached, r.rep.Failures)
			}
			if wait := refusedAt.Sub(startedAt); wait > 2*time.Second+800*time.Millisecond {
				t.Fatalf("the %s refusal took %s from the statement's start; lock_timeout is 2 s", tc.stage, wait)
			}
			c := gateLookup(t, st, ctx, day)
			if !c.attached {
				t.Fatalf("the partition was detached through a refused statement: %+v", c)
			}
			if tc.stage == "detach" && c.state != "detaching" {
				t.Fatalf("after a refused DETACH the committed intent is missing: %+v", c)
			}
			if tc.stage == "shape" && c.state != "attached" {
				t.Fatalf("a refused shape check moved the registry: %+v", c)
			}
		})
	}
}

// Invariant 19 — with the horizon exhausted the decision insert fails with 23514 "no partition";
// the store's decision code maps that to ledger_unwritable, here the SQLSTATE is the assertion.
func TestGateMaintExhaustedHorizonFailsTheInsertWith23514(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	proj, _, svc := seedService(t, st, ctx)
	for i := 1; i <= 7; i++ {
		dropGateDay(t, st, ctx, today.AddDate(0, 0, i))
	}
	at := today.AddDate(0, 0, 2).Add(time.Hour)
	allow := "ALLOW"
	err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{}`)
	if pgCodeOf(err) != "23514" || !strings.Contains(err.Error(), "no partition") {
		t.Fatalf("insert past the horizon: %v, want 23514 no partition", err)
	}
	// One pass with create_max 3 restores three days; the same insert now lands.
	rep, _, perr := runGatePass(t, st, ctx, gateMaintCfg, nil, &gateMetricsFake{}, nil)
	if perr != nil || rep.Attached != 3 {
		t.Fatalf("refill: attached=%d err=%v", rep.Attached, perr)
	}
	if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{}`); err != nil {
		t.Fatalf("insert after the refill: %v", err)
	}
}

// D10 — the four gauges are computed from the refreshed relid, never from names: renaming a
// partition by hand changes none of them, and the survey still owns it by marker.
func TestGateMaintGaugesComeFromRelidAndSurviveARename(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil) // eligible: detached by pass 1
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -20), "detached", nil)
	m := &gateMetricsFake{}
	before, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil || !before.GaugesValid {
		t.Fatalf("pass 1: %v %+v", err, before)
	}
	if before.Gauges.PendingDrop != 2 || before.Gauges.Bytes <= 0 || before.Gauges.WritableHorizonSeconds <= 7*86400 {
		t.Fatalf("baseline gauges: %+v", before.Gauges)
	}
	rel := gateRelname(today.AddDate(0, 0, 2))
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO renamed_by_hand`, rel)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE IF EXISTS renamed_by_hand RENAME TO %s`, rel))
	})
	after, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil || !after.GaugesValid {
		t.Fatalf("pass 2: %v", err)
	}
	if after.Gauges.PendingDrop != before.Gauges.PendingDrop || after.Gauges.OldestAgeSeconds != before.Gauges.OldestAgeSeconds ||
		after.Gauges.Bytes != before.Gauges.Bytes {
		t.Fatalf("a rename changed the gauges: before %+v after %+v", before.Gauges, after.Gauges)
	}
	if d := before.Gauges.WritableHorizonSeconds - after.Gauges.WritableHorizonSeconds; d < 0 || d > 30 {
		t.Fatalf("horizon moved by %.1f s across a rename", d)
	}
	if m.get("partition_identity") != 0 || len(after.Refusals) != 0 {
		t.Fatalf("a renamed partition with our marker and shape was refused: %v", after.Refusals)
	}
}

// D10 — the three cerbix-namespace advisory keys are pairwise distinct: this package sees the
// migration key and the gate key; the scheduler's key is 0x…0001 and is asserted against both
// in internal/scheduler (TestGateAdvisoryKeysDistinctFromSchedulerLeadership).
func TestGateAdvisoryKeysPairwiseDistinct(t *testing.T) {
	const schedulerLockKeyLiteral int64 = 0x6365726269780001 // internal/scheduler advisoryLockKey
	keys := map[string]int64{"scheduler": schedulerLockKeyLiteral, "migrate": migrateLockKey, "gate": GateMaintenanceLockKey}
	for a, ka := range keys {
		for b, kb := range keys {
			if a < b && ka == kb {
				t.Fatalf("%s and %s share advisory key %#x", a, b, ka)
			}
		}
	}
	if GateMaintenanceLockKey>>16 != migrateLockKey>>16 || GateMaintenanceLockKey&0xffff != 3 {
		t.Fatalf("gate key %#x is not slot 3 of the namespace %#x", GateMaintenanceLockKey, migrateLockKey>>16)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// backendPidOf reads the backend pid of a pass connection from inside a hook.
func backendPidOf(ctx context.Context, conn dbConn) (int, error) {
	var pid int
	err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	return pid, err
}

// backendAlive reports whether pid is still a backend of this database.
func backendAlive(t *testing.T, st *Store, ctx context.Context, pid int) bool {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// gateLockHeldBy reports which backend holds the gate advisory lock, 0 when nobody.
func gateLockHeldBy(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	var pid *int
	if err := st.pool.QueryRow(ctx, `
		SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 1
		   AND ((classid::bigint << 32) | objid::bigint) = $1 LIMIT 1`, GateMaintenanceLockKey).Scan(&pid); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if pid == nil {
		return 0
	}
	return *pid
}

// waitGateLockFree polls until nobody holds the gate advisory lock. A terminated backend
// disappears from pg_stat_activity BEFORE ProcKill releases its locks (the shutdown hooks run in
// LIFO order), so a lock assertion right after "the backend is gone" has a window to lose.
func waitGateLockFree(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	waitFor(t, 5*time.Second, "the gate advisory lock to be released", func() bool { return gateLockHeldBy(t, st, ctx) == 0 })
}

// rawPool opens a pgxpool with the given min/max for the pool-cardinality assertions the Store's
// own pool (MinConns floor) cannot make.
func rawPool(t *testing.T, ctx context.Context, minConns, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("CERBIX_TEST_DATABASE_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MinConns, cfg.MaxConns = minConns, maxConns
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// stripComments removes Go (// and /* */) or SQL (--) comments so a grep sees code only.
func stripComments(src string, sql bool) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		marker := "//"
		if sql {
			marker = "--"
		}
		if i := strings.Index(line, marker); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	text := out.String()
	if !sql {
		for {
			i := strings.Index(text, "/*")
			if i < 0 {
				break
			}
			j := strings.Index(text[i:], "*/")
			if j < 0 {
				text = text[:i]
				break
			}
			text = text[:i] + text[i+j+2:]
		}
	}
	return text
}
