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
	beat := func(id string, ago time.Duration) {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO heartbeats (monitor_id, ts, up) VALUES ($1, $2, true)`, id, time.Now().Add(-ago)); err != nil {
			t.Fatalf("seed heartbeat: %v", err)
		}
	}

	staleNoReport := mk("stale-noreport") // never reported, created long ago → stale
	set(staleNoReport.ID, time.Hour, domain.StatusUp)

	fresh := mk("fresh") // reported just now → not stale
	set(fresh.ID, time.Hour, domain.StatusUp)
	beat(fresh.ID, 0)

	down := mk("down") // already down → excluded
	set(down.ID, time.Hour, domain.StatusDown)

	staleOldReport := mk("stale-oldreport") // last beat 90s ago, interval 60s → stale
	set(staleOldReport.ID, time.Hour, domain.StatusUp)
	beat(staleOldReport.ID, 90*time.Second)

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
	if !gotIDs[staleNoReport.ID] || !gotIDs[staleOldReport.ID] {
		t.Fatalf("stale monitors missing: got %v", gotIDs)
	}
	if gotIDs[fresh.ID] || gotIDs[down.ID] {
		t.Fatalf("fresh/down monitor wrongly reported stale: got %v", gotIDs)
	}
	if len(got) != 2 {
		t.Fatalf("stale count = %d, want 2", len(got))
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
