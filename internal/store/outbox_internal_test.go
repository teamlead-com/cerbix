package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func outboxTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run outbox store tests")
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

func (s *Store) countOutbox(ctx context.Context, t *testing.T, topic, status string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE topic = $1 AND status = $2`, topic, status).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// TestIncidentEnqueuesOutboxInTx proves the incident event is written in the same
// transaction as the incident: creating and updating an incident leaves exactly
// the expected pending incident_event rows.
func TestIncidentEnqueuesOutboxInTx(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "down", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opening", "author")
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "pending"); got != 1 {
		t.Fatalf("after create: incident_event rows = %d, want 1", got)
	}

	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "fixed", Author: "author",
	}); err != nil {
		t.Fatalf("add update: %v", err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "pending"); got != 2 {
		t.Fatalf("after resolve: incident_event rows = %d, want 2", got)
	}
}

// TestStatusTransitionEnqueuesOnlyOnChange proves SetMonitorStatus enqueues a
// monitor_transition exactly on a real change, in the same transaction.
func TestStatusTransitionEnqueuesOnlyOnChange(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	if _, err := st.SetMonitorStatus(ctx, mon.ID, domain.StatusDown); err != nil { // pending→down
		t.Fatalf("set down: %v", err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicMonitorTransition, "pending"); got != 1 {
		t.Fatalf("after transition: transition rows = %d, want 1", got)
	}
	if _, err := st.SetMonitorStatus(ctx, mon.ID, domain.StatusDown); err != nil { // down→down, no change
		t.Fatalf("set down again: %v", err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicMonitorTransition, "pending"); got != 1 {
		t.Fatalf("no-change should not enqueue: transition rows = %d, want 1", got)
	}
}

// TestClaimBackoffDeliverDead exercises the claim/lease and terminal states.
func TestClaimBackoffDeliverDead(t *testing.T) {
	st, ctx := outboxTestStore(t)
	// Two raw pending events, due now.
	for i := 0; i < 2; i++ {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO outbox_events (topic, payload) VALUES ($1, '{}')`, domain.TopicIncidentEvent); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	claimed, err := st.ClaimDueOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d, want 2", len(claimed))
	}
	for _, e := range claimed {
		if e.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1 after first claim", e.Attempts)
		}
	}
	// The lease pushed next_attempt_at into the future, so an immediate re-claim is empty.
	if again, _ := st.ClaimDueOutbox(ctx, 10); len(again) != 0 {
		t.Fatalf("re-claim returned %d, want 0 (leased)", len(again))
	}

	// One delivered (terminal), one failed at max (dead). Each owns its claim token → applied.
	if applied, err := st.MarkOutboxDelivered(ctx, claimed[0].ID, claimed[0].ClaimToken); err != nil || !applied {
		t.Fatalf("mark delivered: applied=%v err=%v, want true", applied, err)
	}
	if applied, err := st.FailOutbox(ctx, claimed[1].ID, claimed[1].ClaimToken, "boom", 1); err != nil || !applied { // attempts(1) >= max(1) → dead
		t.Fatalf("fail: applied=%v err=%v, want true", applied, err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "delivered"); got != 1 {
		t.Fatalf("delivered rows = %d, want 1", got)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "dead"); got != 1 {
		t.Fatalf("dead rows = %d, want 1", got)
	}

	// Claim-token CAS: a STALE worker (wrong/old token) must NOT regress the row AND must
	// report applied=false so the worker doesn't count a phantom delivery/dead-letter.
	if applied, err := st.FailOutbox(ctx, claimed[0].ID, "00000000-0000-0000-0000-000000000000", "late failure", 1); err != nil || applied {
		t.Fatalf("stale fail: applied=%v err=%v, want applied=false", applied, err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "delivered"); got != 1 {
		t.Fatalf("stale FailOutbox regressed a delivered row: delivered=%d, want 1", got)
	}
}

// TestPurgeDeliveredOutbox proves only OLD DELIVERED rows are reclaimed — recent delivered
// and dead-lettered rows survive.
func TestPurgeDeliveredOutbox(t *testing.T) {
	st, ctx := outboxTestStore(t)
	seed := func(status string, updatedAgo time.Duration) {
		t.Helper()
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO outbox_events (topic, status, payload, next_attempt_at, updated_at)
			 VALUES ('monitor_transition', $1, '{}'::jsonb, now(), now() - make_interval(secs => $2))`,
			status, int(updatedAgo.Seconds())); err != nil {
			t.Fatalf("seed outbox: %v", err)
		}
	}
	seed("delivered", 30*24*time.Hour) // old delivered → purged
	seed("delivered", time.Hour)       // recent delivered → kept
	seed("dead", 30*24*time.Hour)      // old dead → kept (never auto-purged)

	n, err := st.PurgeDeliveredOutbox(ctx, 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge = %d err=%v, want 1", n, err)
	}
	var remaining int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&remaining)
	if remaining != 2 {
		t.Fatalf("remaining = %d, want 2 (recent delivered + dead)", remaining)
	}
}
