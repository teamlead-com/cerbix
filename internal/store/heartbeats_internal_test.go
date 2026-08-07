package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestInsertHeartbeatsBulkIdempotent(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "m", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	base := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	hbs := []domain.Heartbeat{
		{MonitorID: mon.ID, Ts: base, Up: false, Msg: "down"},
		{MonitorID: mon.ID, Ts: base.Add(time.Minute), Up: true},
	}
	if n, _, err := st.RecordHistoricalResults(ctx, hbs); err != nil || n != 2 {
		t.Fatalf("first bulk = %d err=%v (want 2)", n, err)
	}
	// Re-sending the same buffer is a no-op (idempotent on monitor_id, ts).
	if n, _, err := st.RecordHistoricalResults(ctx, hbs); err != nil || n != 0 {
		t.Fatalf("replay bulk = %d err=%v (want 0)", n, err)
	}
	// A live insert of the same (monitor, ts) also dedupes rather than double-count.
	if err := st.InsertHeartbeat(ctx, hbs[0]); err != nil {
		t.Fatalf("live insert: %v", err)
	}
	got, err := st.ListRecentHeartbeats(ctx, mon.ID, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("heartbeats = %d, want 2 (idempotent)", len(got))
	}
}

// TestRecordHistoricalResultsBounds proves backfill applies the same per-row timestamp
// bounds as scheduled ingest — a future/1970/missing row is skipped, never inserted, so it
// cannot pollute an SLA `ts >= since` query — while a valid historical row lands SLA-only.
func TestRecordHistoricalResultsBounds(t *testing.T) {
	st, ctx := outboxTestStore(t)
	st.WithResultPolicy(5*time.Minute, 30*time.Minute)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "m", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	inserted, skipped, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{
		{MonitorID: mon.ID, Ts: now.Add(-10 * time.Minute), Up: true}, // valid historical
		{MonitorID: mon.ID, Ts: now.Add(10 * time.Minute), Up: true},  // future beyond skew → skip
		{MonitorID: mon.ID, Ts: time.Unix(0, 0).UTC(), Up: true},      // 1970 outside retention → skip
		{MonitorID: mon.ID, Up: true},                                 // missing ts → skip
	})
	if err != nil || inserted != 1 || skipped != 3 {
		t.Fatalf("bounds: inserted=%d skipped=%d err=%v, want 1/3", inserted, skipped, err)
	}
	// Live state untouched (SLA-only): status stays pending, no future row exists.
	if got, _ := st.GetMonitor(ctx, mon.ID); got.Status == domain.StatusUp || got.Status == domain.StatusDown {
		t.Fatalf("backfill must not change live status: %v", got.Status)
	}
	var maxTs time.Time
	_ = st.pool.QueryRow(ctx, `SELECT max(ts) FROM heartbeats WHERE monitor_id=$1`, mon.ID).Scan(&maxTs)
	if maxTs.After(now) {
		t.Fatalf("a future-dated row leaked into heartbeats: max ts %v > now %v", maxTs, now)
	}
	// Idempotent replay.
	if ins2, _, _ := st.RecordHistoricalResults(ctx, []domain.Heartbeat{{MonitorID: mon.ID, Ts: now.Add(-10 * time.Minute), Up: true}}); ins2 != 0 {
		t.Fatalf("replay inserted %d, want 0 (idempotent)", ins2)
	}
}
