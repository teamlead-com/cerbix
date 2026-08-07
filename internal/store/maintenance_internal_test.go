package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestMaintenanceDownEnqueuesAndStamps proves the iter-0076 fix: a monitor that goes
// down DURING a maintenance window still records the transition event and stamps
// last_notified_at (so the renotify job re-enqueues it and delivery resumes once the
// window closes). Suppression is deferred to the outbox worker, which is why the flip
// is still reported as suppressed (incident opening stays muted) but the event exists.
func TestMaintenanceDownEnqueuesAndStamps(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, FailureThreshold: 1, RenotifySeconds: 60,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	// No window yet.
	if in, err := st.MonitorInMaintenance(ctx, mon.ID); err != nil || in {
		t.Fatalf("no window: in=%v err=%v, want false", in, err)
	}

	now := time.Now()
	if _, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: proj.ID, Reason: "maint", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create maintenance: %v", err)
	}
	if in, err := st.MonitorInMaintenance(ctx, mon.ID); err != nil || !in {
		t.Fatalf("active window: in=%v err=%v, want true", in, err)
	}

	// Down flip during the window: suppressed=true (incident muted) but the transition
	// event is enqueued and last_notified_at is stamped.
	_, cur, sup, err := st.RecordCheckStatus(ctx, mon.ID, false)
	if err != nil || cur != domain.StatusDown || !sup {
		t.Fatalf("down in maintenance: cur=%q sup=%v err=%v, want down + suppressed", cur, sup, err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicMonitorTransition, "pending"); got != 1 {
		t.Fatalf("transition events pending = %d, want 1 (enqueued even in maintenance)", got)
	}
	var stamped bool
	if err := st.pool.QueryRow(ctx,
		`SELECT last_notified_at IS NOT NULL FROM monitors WHERE id = $1`, mon.ID).Scan(&stamped); err != nil {
		t.Fatalf("read last_notified_at: %v", err)
	}
	if !stamped {
		t.Fatal("last_notified_at must be stamped on a down that starts in maintenance (renotify baseline)")
	}

	// And the renotify job can therefore pick it up once it's due — set the stamp into
	// the past to make it due immediately.
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET last_notified_at = now() - interval '2 hours' WHERE id = $1`, mon.ID); err != nil {
		t.Fatalf("age last_notified_at: %v", err)
	}
	if n, err := st.EnqueueRenotifyReminders(ctx); err != nil || n != 1 {
		t.Fatalf("renotify = (%d,%v), want 1 (a down-in-maintenance monitor is still eligible)", n, err)
	}
}
