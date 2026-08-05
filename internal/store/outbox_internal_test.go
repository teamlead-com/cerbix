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

	// One delivered (terminal), one failed at max (dead).
	if err := st.MarkOutboxDelivered(ctx, claimed[0].ID); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if err := st.FailOutbox(ctx, claimed[1].ID, "boom", 1); err != nil { // attempts(1) >= max(1) → dead
		t.Fatalf("fail: %v", err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "delivered"); got != 1 {
		t.Fatalf("delivered rows = %d, want 1", got)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "dead"); got != 1 {
		t.Fatalf("dead rows = %d, want 1", got)
	}
}
