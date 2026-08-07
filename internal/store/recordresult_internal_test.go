package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestRecordResultDedupAndFreshness proves the single-transaction result path:
//   - a fresh result inserts a heartbeat and applies the status change;
//   - a duplicate (same monitor+ts) is a no-op for both the heartbeat table and
//     live status (it must not re-bump the failure counter);
//   - a stale/out-of-order result is still recorded as a heartbeat (SLA is never
//     lost) but must not override newer live state;
//   - a strictly newer result applies again.
func TestRecordResultDedupAndFreshness(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "r", Type: domain.MonitorTCP, Target: "10.0.0.1:80",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	hbCount := func() int {
		t.Helper()
		var n int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id = $1`, mon.ID).Scan(&n); err != nil {
			t.Fatalf("count heartbeats: %v", err)
		}
		return n
	}

	t2 := time.Now().UTC().Truncate(time.Millisecond)
	t1 := t2.Add(-time.Minute) // strictly older
	t3 := t2.Add(time.Minute)  // strictly newer

	// 1) Fresh DOWN at t2 → applied, flips down (threshold 1).
	prev, cur, _, applied, err := st.RecordResult(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: t2, Up: false, Code: 500})
	if err != nil || !applied || cur != domain.StatusDown {
		t.Fatalf("fresh down: prev=%v cur=%v applied=%v err=%v", prev, cur, applied, err)
	}
	if hbCount() != 1 {
		t.Fatalf("after fresh down, heartbeats = %d, want 1", hbCount())
	}

	// 2) Duplicate of t2 → no heartbeat, not applied, counter untouched.
	_, _, _, applied, err = st.RecordResult(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: t2, Up: false, Code: 500})
	if err != nil || applied {
		t.Fatalf("duplicate: applied=%v err=%v, want applied=false", applied, err)
	}
	if hbCount() != 1 {
		t.Fatalf("after duplicate, heartbeats = %d, want 1 (no re-insert)", hbCount())
	}
	if got, _ := st.GetMonitor(ctx, mon.ID); got.ConsecutiveFailures != 1 {
		t.Fatalf("duplicate must not re-bump failures: got %d, want 1", got.ConsecutiveFailures)
	}

	// 3) Stale UP at t1 (older than t2) → heartbeat kept, but status stays DOWN.
	_, _, _, applied, err = st.RecordResult(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: t1, Up: true, Code: 200})
	if err != nil || applied {
		t.Fatalf("stale up: applied=%v err=%v, want applied=false", applied, err)
	}
	if hbCount() != 2 {
		t.Fatalf("stale result must still be recorded: heartbeats = %d, want 2", hbCount())
	}
	if got, _ := st.GetMonitor(ctx, mon.ID); got.Status != domain.StatusDown {
		t.Fatalf("stale up must not override live state: status = %v, want down", got.Status)
	}

	// 4) Fresh UP at t3 (newer than t2) → applied, recovers.
	prev, cur, _, applied, err = st.RecordResult(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: t3, Up: true, Code: 200})
	if err != nil || !applied || prev != domain.StatusDown || cur != domain.StatusUp {
		t.Fatalf("fresh up: prev=%v cur=%v applied=%v err=%v", prev, cur, applied, err)
	}
	if hbCount() != 3 {
		t.Fatalf("after fresh up, heartbeats = %d, want 3", hbCount())
	}
}
