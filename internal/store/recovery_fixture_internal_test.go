package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-043 fixtures shared by the recovery_*_internal_test.go files. Every assertion about the run
// reads the ROW, not the scanned monitor: these tests are about what the statements write, and a
// scan that dropped a column would otherwise hide a write that never happened.

// recoveryRow is the live state of one monitor as the database holds it.
type recoveryRow struct {
	Status        string
	Successes     int
	StartedAt     *time.Time
	LastNotified  *time.Time
	Failures      int
	StateSequence int64
}

// recoveryMonitor drives one monitor through RecordScheduledResult with strictly increasing
// timestamps and the monitor's CURRENT revision, so the freshness and revision gates always pass
// unless a step means to trip them.
type recoveryMonitor struct {
	t     *testing.T
	st    *Store
	ctx   context.Context
	ID    string
	next  time.Time
	lastT time.Time
}

// newRecoveryMonitor creates a TCP monitor with the given recovery and failure thresholds. The
// result policy is widened so that a test which feeds a dozen results one second apart stays
// inside both the skew and the retention window.
func newRecoveryMonitor(t *testing.T, st *Store, ctx context.Context, projectID, name string, recovery, failure int) *recoveryMonitor {
	t.Helper()
	st.WithResultPolicy(5*time.Minute, 30*time.Minute)
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		RecoveryThreshold: recovery,
		ProjectID:         projectID, Name: name, Type: domain.MonitorTCP, Target: "10.0.0.1:80",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: failure, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor %s: %v", name, err)
	}
	return &recoveryMonitor{t: t, st: st, ctx: ctx, ID: m.ID, next: time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)}
}

func (r *recoveryMonitor) revision() int64 {
	r.t.Helper()
	var rev int64
	if err := r.st.pool.QueryRow(r.ctx, `SELECT execution_revision FROM monitors WHERE id = $1`, r.ID).Scan(&rev); err != nil {
		r.t.Fatalf("read revision: %v", err)
	}
	return rev
}

// feed records one fresh verdict and returns what the store did with it.
func (r *recoveryMonitor) feed(up bool) ResultOutcome {
	r.t.Helper()
	r.next = r.next.Add(time.Second)
	r.lastT = r.next
	code := 0
	if !up {
		code = 1
	}
	o, err := r.st.RecordScheduledResult(r.ctx, domain.Heartbeat{
		MonitorID: r.ID, Ts: r.next, Up: up, Code: code, ExecutionRevision: r.revision(),
	})
	if err != nil {
		r.t.Fatalf("record result: %v", err)
	}
	if !o.Applied {
		r.t.Fatalf("a fresh result was not applied: %+v", o)
	}
	return o
}

func (r *recoveryMonitor) row() recoveryRow {
	r.t.Helper()
	return readRecoveryRow(r.t, r.st, r.ctx, r.ID)
}

func readRecoveryRow(t *testing.T, st *Store, ctx context.Context, id string) recoveryRow {
	t.Helper()
	var w recoveryRow
	if err := st.pool.QueryRow(ctx,
		`SELECT status, consecutive_successes, recovery_started_at, last_notified_at,
		        consecutive_failures, state_sequence
		   FROM monitors WHERE id = $1`, id).
		Scan(&w.Status, &w.Successes, &w.StartedAt, &w.LastNotified, &w.Failures, &w.StateSequence); err != nil {
		t.Fatalf("read monitor row: %v", err)
	}
	// I13 on every read: a run exists exactly when the stamp does, and only on a DOWN monitor. The
	// CHECK enforces it too; asserting it here names the step that would have broken it.
	if (w.Successes > 0) != (w.StartedAt != nil) || (w.Successes > 0 && w.Status != "down") {
		t.Fatalf("I13 broken: status=%s successes=%d started_at=%v", w.Status, w.Successes, w.StartedAt)
	}
	return w
}

// seedRun puts a monitor into the middle of a run directly. The writer tests use it so that what
// they prove is the writer's reset, independent of how a run is started.
func seedRun(t *testing.T, st *Store, ctx context.Context, id string, run int) {
	t.Helper()
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET status = 'down', consecutive_successes = $2,
		        recovery_started_at = statement_timestamp() - interval '1 minute'
		  WHERE id = $1`, id, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// transitionPayloads returns the raw monitor_transition payloads for one monitor, oldest first.
func transitionPayloads(t *testing.T, st *Store, ctx context.Context, monitorID string) []map[string]any {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT payload FROM outbox_events WHERE topic = $1 AND payload->>'monitor_id' = $2
		  ORDER BY created_at, (payload->>'seq')::bigint`,
		domain.TopicMonitorTransition, monitorID)
	if err != nil {
		t.Fatalf("read transitions: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan transition: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode transition: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func decodeTransition(t *testing.T, m map[string]any) domain.MonitorTransition {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode transition: %v", err)
	}
	var tr domain.MonitorTransition
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("decode transition: %v", err)
	}
	return tr
}

func recoveryProject(t *testing.T, st *Store, ctx context.Context, slug string) string {
	t.Helper()
	org, err := st.CreateOrganization(ctx, slug, slug)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return proj.ID
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// withoutRunGuard switches off migration 00118's trigger for the rest of the test. The trigger clears
// the run on any row leaving `down`, which is the guard for writers that do not know about the run
// (I13a); with it on, a test of this release's OWN resets — or of the run-state CHECK itself — would
// pass even if the reset or the CHECK were gone. Store tests that touch the database run one at a
// time, so the table-wide switch is restored before anything else runs.
func withoutRunGuard(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `ALTER TABLE monitors DISABLE TRIGGER monitors_recovery_run_guard_trg`); err != nil {
		t.Fatalf("disable the run guard: %v", err)
	}
	t.Cleanup(func() {
		if _, err := st.pool.Exec(context.Background(), `ALTER TABLE monitors ENABLE TRIGGER monitors_recovery_run_guard_trg`); err != nil {
			t.Errorf("re-enable the run guard: %v", err)
		}
	})
}
