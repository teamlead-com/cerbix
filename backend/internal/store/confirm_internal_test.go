package store

import (
	"context"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// TestRecordCheckStatusConfirmNotify proves the confirmation-phase wake signal:
// a counted failure below the threshold NOTIFYs monitor_confirm with the monitor
// id, the down verdict does not, and a recovery does not.
func TestRecordCheckStatusConfirmNotify(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "c", Type: domain.MonitorTCP, Target: "10.0.0.1:80",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 3, ConfirmIntervalSeconds: 10, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if mon.ConfirmIntervalSeconds != 10 {
		t.Fatalf("confirm interval round-trip = %d, want 10", mon.ConfirmIntervalSeconds)
	}

	// Hold a LISTEN connection like the scheduler's notifier does.
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+ConfirmChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}
	expectNotify := func(want bool, step string) {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		notif, err := conn.Conn().WaitForNotification(wctx)
		if want {
			if err != nil {
				t.Fatalf("%s: expected notify, got err %v", step, err)
			}
			if notif.Payload != mon.ID {
				t.Fatalf("%s: payload = %q, want monitor id", step, notif.Payload)
			}
			return
		}
		if err == nil {
			t.Fatalf("%s: unexpected notify %q", step, notif.Payload)
		}
	}

	// Failures 1 and 2 (below threshold 3) → confirmation phase → notify each.
	if _, cur, _, err := st.RecordCheckStatus(ctx, mon.ID, false); err != nil || cur == domain.StatusDown {
		t.Fatalf("fail 1: cur=%v err=%v", cur, err)
	}
	expectNotify(true, "fail 1")
	if _, cur, _, err := st.RecordCheckStatus(ctx, mon.ID, false); err != nil || cur == domain.StatusDown {
		t.Fatalf("fail 2: cur=%v err=%v", cur, err)
	}
	expectNotify(true, "fail 2")
	// Failure 3 → verdict down → no confirmation signal.
	if _, cur, _, err := st.RecordCheckStatus(ctx, mon.ID, false); err != nil || cur != domain.StatusDown {
		t.Fatalf("fail 3: cur=%v err=%v (want down)", cur, err)
	}
	expectNotify(false, "verdict")
	// Recovery resets the counter → no signal either.
	if _, cur, _, err := st.RecordCheckStatus(ctx, mon.ID, true); err != nil || cur != domain.StatusUp {
		t.Fatalf("recover: cur=%v err=%v", cur, err)
	}
	expectNotify(false, "recovery")

	// consecutive_failures is surfaced through the monitor read path.
	if _, _, _, err := st.RecordCheckStatus(ctx, mon.ID, false); err != nil {
		t.Fatalf("fail again: %v", err)
	}
	got, err := st.GetMonitor(ctx, mon.ID)
	if err != nil || got.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d err=%v, want 1", got.ConsecutiveFailures, err)
	}
	if !got.InConfirmPhase() {
		t.Fatalf("monitor should report confirm phase: %+v", got)
	}
}
