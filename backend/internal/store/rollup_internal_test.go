package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// rollupTestStore is an in-package (unexported-field) test harness, opt-in via
// CERBIX_TEST_DATABASE_DSN, so it can insert heartbeats with explicit timestamps
// (the public InsertHeartbeat stamps now()).
func rollupTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run rollup store tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	return st, ctx
}

func TestRollupDailyAvailability(t *testing.T) {
	st, ctx := rollupTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
	})

	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterdayNoon := today.Add(-12 * time.Hour)
	// Yesterday: 3 up, 1 down (distinct timestamps — (monitor_id, ts) is unique).
	for i, up := range []bool{true, true, true, false} {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg) VALUES ($1,$2,$3,10,200,'')`,
			mon.ID, yesterdayNoon.Add(time.Duration(i)*time.Minute), up); err != nil {
			t.Fatalf("insert yesterday hb: %v", err)
		}
	}
	// Today: 2 up (default now()).
	for i := 0; i < 2; i++ {
		if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Up: true, Code: 200}); err != nil {
			t.Fatalf("insert today hb: %v", err)
		}
	}

	if err := st.RollupDailyAvailability(ctx, today.Add(-95*24*time.Hour), today); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	// Idempotent.
	if err := st.RollupDailyAvailability(ctx, today.Add(-95*24*time.Hour), today); err != nil {
		t.Fatalf("rollup re-run: %v", err)
	}

	days, err := st.MonitorDailyAvailability(ctx, mon.ID, today.Add(-3*24*time.Hour))
	if err != nil {
		t.Fatalf("availability: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("expected 2 days (yesterday rollup + today raw), got %d: %+v", len(days), days)
	}
	if days[0].Up != 3 || days[0].Total != 4 || days[0].UptimePercent != 75 {
		t.Fatalf("yesterday bucket = %+v, want up3/total4/75%%", days[0])
	}
	if days[1].Up != 2 || days[1].Total != 2 || days[1].UptimePercent != 100 {
		t.Fatalf("today bucket = %+v, want up2/total2/100%%", days[1])
	}

	// A maintenance window over yesterday excludes those heartbeats on re-rollup.
	if _, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: proj.ID, MonitorID: mon.ID,
		StartsAt: yesterdayNoon.Add(-time.Hour), EndsAt: yesterdayNoon.Add(time.Hour), Reason: "mw",
	}); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if err := st.RollupDailyAvailability(ctx, today.Add(-95*24*time.Hour), today); err != nil {
		t.Fatalf("re-rollup after maintenance: %v", err)
	}
	days, _ = st.MonitorDailyAvailability(ctx, mon.ID, today.Add(-3*24*time.Hour))
	// Yesterday's heartbeats are all within the window → excluded → only today remains.
	if len(days) != 1 || days[0].Total != 2 {
		t.Fatalf("after maintenance, expected only today (total 2), got %+v", days)
	}
}

func TestProjectDailyAvailabilityLive(t *testing.T) {
	st, ctx := rollupTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	m1, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "a", Type: domain.MonitorHTTP, Target: "https://a", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true})
	m2, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "b", Type: domain.MonitorHTTP, Target: "https://b", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true})
	_ = st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: m1.ID, Up: true, Code: 200})
	_ = st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: m2.ID, Up: false, Code: 500})

	today := time.Now().UTC().Truncate(24 * time.Hour)
	days, err := st.ProjectDailyAvailability(ctx, proj.ID, today.Add(-3*24*time.Hour))
	if err != nil || len(days) != 1 {
		t.Fatalf("project availability = %+v (err %v), want 1 day (today)", days, err)
	}
	if days[0].Up != 1 || days[0].Total != 2 {
		t.Fatalf("today project bucket = %+v, want up1/total2", days[0])
	}
}
