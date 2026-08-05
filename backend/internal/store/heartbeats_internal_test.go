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
	if n, err := st.InsertHeartbeatsBulk(ctx, hbs); err != nil || n != 2 {
		t.Fatalf("first bulk = %d err=%v (want 2)", n, err)
	}
	// Re-sending the same buffer is a no-op (idempotent on monitor_id, ts).
	if n, err := st.InsertHeartbeatsBulk(ctx, hbs); err != nil || n != 0 {
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
