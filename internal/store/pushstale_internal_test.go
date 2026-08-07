package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestStalePushMonitors covers the batched dead-man's-switch query: only enabled,
// not-already-down push monitors with no heartbeat within their interval (or never
// reporting, past created_at) are returned; fresh, down, and non-push monitors are
// excluded.
func TestStalePushMonitors(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	mk := func(name string) domain.Monitor {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: name, Type: domain.MonitorPush, IntervalSeconds: 60, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create push %s: %v", name, err)
		}
		return m
	}
	set := func(id string, createdAgo time.Duration, status domain.MonitorStatus) {
		if _, err := st.pool.Exec(ctx,
			`UPDATE monitors SET created_at = $1, status = $2 WHERE id = $3`,
			time.Now().Add(-createdAgo), status, id); err != nil {
			t.Fatalf("update monitor: %v", err)
		}
	}
	// Staleness is keyed on last_result_ts (a REAL observation), not raw heartbeats — a
	// dead-man DOWN sample must not make a monitor look fresh (see StalePushMonitors).
	reported := func(id string, ago time.Duration) {
		if _, err := st.pool.Exec(ctx,
			`UPDATE monitors SET last_result_ts = $2 WHERE id = $1`, id, time.Now().Add(-ago)); err != nil {
			t.Fatalf("set last_result_ts: %v", err)
		}
	}

	staleNoReport := mk("stale-noreport") // never reported, created long ago → stale
	set(staleNoReport.ID, time.Hour, domain.StatusUp)

	fresh := mk("fresh") // reported just now → not stale
	set(fresh.ID, time.Hour, domain.StatusUp)
	reported(fresh.ID, 0)

	down := mk("down") // already down but still silent → INCLUDED (periodic dead-man sampling)
	set(down.ID, time.Hour, domain.StatusDown)

	staleOldReport := mk("stale-oldreport") // last report 90s ago, interval 60s → stale
	set(staleOldReport.ID, time.Hour, domain.StatusUp)
	reported(staleOldReport.ID, 90*time.Second)

	// A non-push monitor must never appear.
	if _, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "http", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("create http: %v", err)
	}

	got, err := st.StalePushMonitors(ctx)
	if err != nil {
		t.Fatalf("stale push: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, m := range got {
		gotIDs[m.ID] = true
	}
	// A down-but-silent monitor stays in the set now (periodic dead-man DOWN sampling for
	// sample-based SLA); only a freshly-reported one is excluded.
	if !gotIDs[staleNoReport.ID] || !gotIDs[staleOldReport.ID] || !gotIDs[down.ID] {
		t.Fatalf("stale monitors missing (incl. silent-down): got %v", gotIDs)
	}
	if gotIDs[fresh.ID] {
		t.Fatalf("freshly-reported monitor wrongly reported stale: got %v", gotIDs)
	}
	if len(got) != 3 {
		t.Fatalf("stale count = %d, want 3", len(got))
	}
}

// TestStalePushGrace covers grace_seconds extending the liveness window.
func TestStalePushGrace(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	tolerant, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "tolerant", Type: domain.MonitorPush, IntervalSeconds: 60, GraceSeconds: 3600, Enabled: true})
	strict, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "strict", Type: domain.MonitorPush, IntervalSeconds: 60, GraceSeconds: 0, Enabled: true})
	// Both last reported 120s ago (no heartbeat, past created_at).
	for _, id := range []string{tolerant.ID, strict.ID} {
		if _, err := st.pool.Exec(ctx, `UPDATE monitors SET created_at = $1 WHERE id = $2`, time.Now().Add(-120*time.Second), id); err != nil {
			t.Fatalf("update created_at: %v", err)
		}
	}
	stale, err := st.StalePushMonitors(ctx)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	got := map[string]bool{}
	for _, m := range stale {
		got[m.ID] = true
	}
	if got[tolerant.ID] {
		t.Fatal("monitor within grace should not be stale")
	}
	if !got[strict.ID] {
		t.Fatal("monitor past interval with no grace should be stale")
	}
}
