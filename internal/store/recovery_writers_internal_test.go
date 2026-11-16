package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-043 §4.1 "What resets the run without a check": every write that sets `status` outside
// recordCheckStatusTx resets the run in the same statement (I12). Each test seeds a run directly so
// that what it proves is the writer's own reset.

func TestRecoveryReEnableResetsTheRun(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-reenable")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-reenable", 3, 1)
	seedRun(t, st, ctx, m.ID, 2)
	seeded := m.row()

	// Disabling changes no status today and does not reset the run (§4.1): the reset is the
	// enable's, which is when the status is written.
	cur, err := st.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Enabled = false
	if _, err := st.UpdateMonitor(ctx, cur); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if w := m.row(); w.Status != "down" || w.Successes != 2 || !sameInstant(w.StartedAt, seeded.StartedAt) {
		t.Fatalf("disable touched the run: %+v", w)
	}

	cur.Enabled = true
	updated, err := st.UpdateMonitor(ctx, cur)
	if err != nil {
		t.Fatalf("re-enable mid-run: %v", err)
	}
	w := m.row()
	if w.Status != "pending" || w.Successes != 0 || w.StartedAt != nil {
		t.Fatalf("re-enable: %+v, want pending/0/NULL (I12)", w)
	}
	if updated.ConsecutiveSuccesses != 0 || updated.RecoveryStartedAt != nil {
		t.Fatalf("the returned monitor still carries the run: %d/%v", updated.ConsecutiveSuccesses, updated.RecoveryStartedAt)
	}
}

// recoveryComposite builds the composite RetireMonitor and ReactivateMonitor are scoped to (§15.5).
// A composite's threshold is 1 (D8), so its run is seeded: the writers' reset is a property of the
// statement, not of the threshold.
func recoveryComposite(t *testing.T, st *Store, projectID string) string {
	t.Helper()
	ctx := t.Context()
	child := newRecoveryMonitor(t, st, ctx, projectID, "rt-child", 1, 1)
	comp, err := st.CreateMonitor(ctx, domain.Monitor{
		RecoveryThreshold: 1,
		ProjectID:         projectID, Name: "rt-composite", Type: domain.MonitorComposite,
		IntervalSeconds: 60, Region: domain.DefaultRegion, Enabled: true,
		Config: map[string]string{"children": child.ID, "mode": "all"},
	})
	if err != nil {
		t.Fatalf("composite: %v", err)
	}
	return comp.ID
}

func TestRecoveryRetireResetsTheRun(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-retire")
	id := recoveryComposite(t, st, projectID)
	seedRun(t, st, ctx, id, 2)

	m, err := st.RetireMonitor(ctx, projectID, id, GraphActor{Label: "ops@example.com"})
	if err != nil {
		t.Fatalf("retire mid-run: %v", err)
	}
	if w := readRecoveryRow(t, st, ctx, id); w.Status != "pending" || w.Successes != 0 || w.StartedAt != nil {
		t.Fatalf("retire: %+v, want pending/0/NULL (I12)", w)
	}
	if m.ConsecutiveSuccesses != 0 || m.RecoveryStartedAt != nil {
		t.Fatalf("the returned monitor still carries the run: %d/%v", m.ConsecutiveSuccesses, m.RecoveryStartedAt)
	}
}

func TestRecoveryReactivateResetsTheRun(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-reactivate")
	id := recoveryComposite(t, st, projectID)
	actor := GraphActor{Label: "ops@example.com"}
	if _, err := st.RetireMonitor(ctx, projectID, id, actor); err != nil {
		t.Fatalf("retire: %v", err)
	}
	// A run on a retired row cannot arise through ingest (a disabled monitor's results are
	// refused); it is seeded to prove the reactivate statement resets whatever it finds.
	seedRun(t, st, ctx, id, 3)

	m, err := st.ReactivateMonitor(ctx, projectID, id, actor)
	if err != nil {
		t.Fatalf("reactivate mid-run: %v", err)
	}
	if w := readRecoveryRow(t, st, ctx, id); w.Status != "pending" || w.Successes != 0 || w.StartedAt != nil {
		t.Fatalf("reactivate: %+v, want pending/0/NULL (I12)", w)
	}
	if m.ConsecutiveSuccesses != 0 || m.RecoveryStartedAt != nil {
		t.Fatalf("the returned monitor still carries the run: %d/%v", m.ConsecutiveSuccesses, m.RecoveryStartedAt)
	}
}

// SetMonitorStatus (test support, no production caller): the run survives a write that keeps the
// monitor down and is reset by any other status (§4.1).
func TestRecoverySetMonitorStatusResetsTheRunOffDown(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-setstatus")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-setstatus", 3, 1)
	for _, to := range []domain.MonitorStatus{domain.StatusUp, domain.StatusPending} {
		seedRun(t, st, ctx, m.ID, 2)
		seeded := m.row()
		if _, err := st.SetMonitorStatus(ctx, m.ID, domain.StatusDown); err != nil {
			t.Fatalf("set down: %v", err)
		}
		if w := m.row(); w.Successes != 2 || !sameInstant(w.StartedAt, seeded.StartedAt) {
			t.Fatalf("a write that keeps the monitor down reset the run: %+v", w)
		}
		if _, err := st.SetMonitorStatus(ctx, m.ID, to); err != nil {
			t.Fatalf("set %s mid-run: %v", to, err)
		}
		if w := m.row(); string(w.Status) != string(to) || w.Successes != 0 || w.StartedAt != nil {
			t.Fatalf("set %s: %+v, want %s/0/NULL", to, w, to)
		}
	}
}

// I13 — the database refuses every row whose run state is inconsistent, whoever writes it.
func TestRecoveryRunStateCheckRefusesAnInconsistentRow(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-check")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-check", 3, 1)
	for _, bad := range []struct{ name, set string }{
		{"a count without a stamp", `status = 'down', consecutive_successes = 1, recovery_started_at = NULL`},
		{"a stamp without a count", `status = 'down', consecutive_successes = 0, recovery_started_at = now()`},
		{"a run on an up monitor", `status = 'up', consecutive_successes = 1, recovery_started_at = now()`},
		{"a run on a pending monitor", `status = 'pending', consecutive_successes = 2, recovery_started_at = now()`},
		{"a negative count", `status = 'down', consecutive_successes = -1, recovery_started_at = now()`},
	} {
		_, err := st.pool.Exec(ctx, `UPDATE monitors SET `+bad.set+` WHERE id = $1`, m.ID)
		if pgErrCode(err) != "23514" || !strings.Contains(err.Error(), "monitors_recovery_run_state") {
			t.Errorf("%s: got %v, want the run-state CHECK (23514)", bad.name, err)
		}
	}
}

// I10 at the database — the CHECKs refuse what Monitor.Validate refuses, independently of it. The
// store's own insert and update paths write the value they are given (§4.5: nothing clamps), so
// they reach the CHECK directly.
func TestRecoveryThresholdChecksRefuseOutOfRangeAndTypeRule(t *testing.T) {
	st, ctx := outboxTestStore(t)
	projectID := recoveryProject(t, st, ctx, "rt-constraints")
	child := newRecoveryMonitor(t, st, ctx, projectID, "rt-cons-child", 1, 1)

	tcp := func(name string, threshold int) domain.Monitor {
		return domain.Monitor{RecoveryThreshold: threshold, ProjectID: projectID, Name: name,
			Type: domain.MonitorTCP, Target: "10.0.0.2:80", IntervalSeconds: 60, TimeoutSeconds: 5,
			FailureThreshold: 1, Enabled: true}
	}
	push := func(name string, threshold int) domain.Monitor {
		return domain.Monitor{RecoveryThreshold: threshold, ProjectID: projectID, Name: name,
			Type: domain.MonitorPush, IntervalSeconds: 60, FailureThreshold: 1, Enabled: true}
	}
	composite := func(name string, threshold int) domain.Monitor {
		return domain.Monitor{RecoveryThreshold: threshold, ProjectID: projectID, Name: name,
			Type: domain.MonitorComposite, IntervalSeconds: 60, Region: domain.DefaultRegion, Enabled: true,
			Config: map[string]string{"children": child.ID, "mode": "all"}}
	}
	refused := func(err error, constraint string) bool {
		return err != nil && pgErrCode(err) == "23514" && strings.Contains(err.Error(), constraint)
	}

	for _, c := range []struct {
		name       string
		m          domain.Monitor
		constraint string
	}{
		{"zero", tcp("rt-zero", 0), "monitors_recovery_threshold_range"},
		{"eleven", tcp("rt-eleven", 11), "monitors_recovery_threshold_range"},
		{"push above 1", push("rt-push2", 2), "monitors_recovery_threshold_type"},
		{"composite above 1", composite("rt-comp2", 2), "monitors_recovery_threshold_type"},
	} {
		if _, err := st.CreateMonitor(ctx, c.m); !refused(err, c.constraint) {
			t.Errorf("insert %s: got %v, want %s", c.name, err, c.constraint)
		}
	}

	for _, c := range []struct {
		name       string
		m          domain.Monitor
		to         int
		constraint string
	}{
		{"zero", tcp("rt-u-zero", 1), 0, "monitors_recovery_threshold_range"},
		{"eleven", tcp("rt-u-eleven", 1), 11, "monitors_recovery_threshold_range"},
		{"push above 1", push("rt-u-push", 1), 2, "monitors_recovery_threshold_type"},
		{"composite above 1", composite("rt-u-comp", 1), 2, "monitors_recovery_threshold_type"},
	} {
		created, err := st.CreateMonitor(ctx, c.m)
		if err != nil {
			t.Fatalf("create %s at 1: %v", c.name, err)
		}
		got, err := st.GetMonitor(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		got.RecoveryThreshold = c.to
		if _, err := st.UpdateMonitor(ctx, got); !refused(err, c.constraint) {
			t.Errorf("update %s to %d: got %v, want %s", c.name, c.to, err, c.constraint)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE monitors SET recovery_threshold = $2 WHERE id = $1`, created.ID, c.to); !refused(err, c.constraint) {
			t.Errorf("raw update %s to %d: got %v, want %s", c.name, c.to, err, c.constraint)
		}
	}
	// The boundaries themselves are accepted.
	for _, ok := range []int{1, 10} {
		if _, err := st.CreateMonitor(ctx, tcp(fmt.Sprintf("rt-ok-%d", ok), ok)); err != nil {
			t.Errorf("threshold %d refused: %v", ok, err)
		}
	}
}

// §5 — the migration is additive: a monitor that exists before 00117 reads 1 / 0 / NULL after it,
// which is today's behaviour exactly (I1). Migrated on a probe database stopping AT 00117, so a later
// migration touching these columns cannot make this test's subject drift.
func TestRecoveryMigrationGivesExistingRowsTodaysBehaviour(t *testing.T) {
	st, ctx := outboxTestStore(t)
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL (%q): %v", dsn, err)
	}
	name := fmt.Sprintf("cerbix_test_upgrade117_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := st.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create probe database (this gate requires CREATEDB): %v", err)
	}
	defer dropProbeDatabase(t, st.pool, name)
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 116); err != nil {
		t.Fatalf("migrate to 116: %v", err)
	}
	var id string
	if err := db.QueryRowContext(ctx, `
		WITH org AS (INSERT INTO organizations (slug, name) VALUES ('acme','Acme') RETURNING id),
		     proj AS (INSERT INTO projects (org_id, slug, name) SELECT id, 'api', 'API' FROM org RETURNING id)
		INSERT INTO monitors (project_id, name, type, slug, target, status)
		SELECT id, 'legacy', 'tcp', 'legacy', '10.0.0.1:80', 'down' FROM proj
		RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("insert a pre-upgrade monitor: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 117); err != nil {
		t.Fatalf("migrate to 117: %v", err)
	}
	var threshold, run int
	var started *time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT recovery_threshold, consecutive_successes, recovery_started_at FROM monitors WHERE id = $1`, id).
		Scan(&threshold, &run, &started); err != nil {
		t.Fatalf("read the upgraded monitor: %v", err)
	}
	if threshold != 1 || run != 0 || started != nil {
		t.Fatalf("upgraded row = %d / %d / %v, want 1 / 0 / NULL", threshold, run, started)
	}
}
