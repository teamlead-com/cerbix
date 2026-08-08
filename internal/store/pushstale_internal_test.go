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

// TestPushUpdatePreservesLiveness covers the push-watermark semantics: a plain edit
// of a live push monitor must NOT wipe its real-ping liveness (the false-DOWN-after-
// update blocker), and a disabled→enabled transition must re-arm the dead-man from the
// enable moment (push_armed_at) without fabricating a last_result_ts.
func TestPushUpdatePreservesLiveness(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	inStale := func(id string) bool {
		got, err := st.StalePushMonitors(ctx)
		if err != nil {
			t.Fatalf("stale push: %v", err)
		}
		for _, m := range got {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	readArmed := func(id string) *time.Time {
		var at *time.Time
		if err := st.pool.QueryRow(ctx, `SELECT push_armed_at FROM monitors WHERE id=$1`, id).Scan(&at); err != nil {
			t.Fatalf("read push_armed_at: %v", err)
		}
		return at
	}
	readLast := func(id string) *time.Time {
		var at *time.Time
		if err := st.pool.QueryRow(ctx, `SELECT last_result_ts FROM monitors WHERE id=$1`, id).Scan(&at); err != nil {
			t.Fatalf("read last_result_ts: %v", err)
		}
		return at
	}

	// --- Blocker: editing a LIVE push monitor must not turn it stale. ---
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "cron", Type: domain.MonitorPush, IntervalSeconds: 60, GraceSeconds: 0, Enabled: true, PushToken: "cbxp_live",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Created long ago, but a real ping landed 10s ago → currently live.
	realPing := time.Now().Add(-10 * time.Second)
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET created_at = $2, last_result_ts = $3 WHERE id = $1`,
		mon.ID, time.Now().Add(-time.Hour), realPing); err != nil {
		t.Fatalf("seed liveness: %v", err)
	}
	if inStale(mon.ID) {
		t.Fatal("precondition: live push should not be stale before the edit")
	}
	mon.Name = "cron-renamed"
	upd, err := st.UpdateMonitor(ctx, mon)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.ExecutionRevision != mon.ExecutionRevision+1 {
		t.Fatalf("revision not bumped: got %d", upd.ExecutionRevision)
	}
	if last := readLast(mon.ID); last == nil || last.Before(realPing.Add(-time.Second)) || last.After(realPing.Add(time.Second)) {
		t.Fatalf("edit must PRESERVE the real-ping last_result_ts, got %v (want ~%v)", last, realPing)
	}
	if inStale(mon.ID) {
		t.Fatal("BLOCKER: a plain edit of a live push monitor turned it stale (false DOWN)")
	}

	// --- Re-arm: disabled→enabled starts a fresh liveness window from the enable moment. ---
	dorm, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "dormant", Type: domain.MonitorPush, IntervalSeconds: 60, GraceSeconds: 0, Enabled: true, PushToken: "cbxp_dorm",
	})
	if err != nil {
		t.Fatalf("create dormant: %v", err)
	}
	oldPing := time.Now().Add(-time.Hour)
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET created_at = $2, last_result_ts = $3, status = 'up' WHERE id = $1`,
		dorm.ID, time.Now().Add(-2*time.Hour), oldPing); err != nil {
		t.Fatalf("seed dormant: %v", err)
	}
	// Disable (no re-arm; status/last preserved; excluded from dead-man while disabled).
	dorm.Enabled = false
	if _, err := st.UpdateMonitor(ctx, dorm); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if a := readArmed(dorm.ID); a != nil {
		t.Fatalf("disable must not arm: push_armed_at=%v", a)
	}
	// Re-enable → re-arm.
	dorm.Enabled = true
	reUpd, err := st.UpdateMonitor(ctx, dorm)
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if reUpd.Status != domain.StatusPending {
		t.Fatalf("re-enable must reset live state to pending, got %q", reUpd.Status)
	}
	if reUpd.ConsecutiveFailures != 0 {
		t.Fatalf("re-enable must clear confirmation counter, got %d", reUpd.ConsecutiveFailures)
	}
	if a := readArmed(dorm.ID); a == nil || time.Since(*a) > time.Minute {
		t.Fatalf("re-enable must stamp push_armed_at ~now, got %v", a)
	}
	// last_result_ts is the REAL ping — never fabricated by re-arm.
	if last := readLast(dorm.ID); last == nil || last.After(oldPing.Add(time.Second)) {
		t.Fatalf("re-arm must NOT fabricate last_result_ts, got %v (want the old real ping ~%v)", last, oldPing)
	}
	// Freshly armed → not stale despite the hour-old ping.
	if inStale(dorm.ID) {
		t.Fatal("re-armed push must not be immediately stale (fresh window from enable)")
	}
	// Simulate the liveness window elapsing (armed 2min ago, interval 60s, grace 0) → stale.
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET push_armed_at = $2 WHERE id = $1`,
		dorm.ID, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("age push_armed_at: %v", err)
	}
	if !inStale(dorm.ID) {
		t.Fatal("re-armed push with no ping past the window must become stale")
	}
	// And the dead-man actually applies DOWN under the current (re-enable-bumped) revision:
	// freshness (aged push_armed_at) is past the cutoff, so the CAS passes.
	cutoff := time.Now().Add(-90 * time.Second)
	if o, err := st.RecordDeadmanResult(ctx, dorm.ID, reUpd.ExecutionRevision, cutoff); err != nil || !o.Applied || o.Cur != domain.StatusDown {
		t.Fatalf("dead-man after re-arm window: %+v err=%v, want applied down", o, err)
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
