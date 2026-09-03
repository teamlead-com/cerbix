package store

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// legacyClaimShape is the EXACT claim SQL every pre-fence binary runs (no topic
// predicate, status = 'pending'). The fence contract (invariant 61) is that
// this shape can never see a fenced row — through enqueue, retry, dead and
// replay alike.
func legacyClaimShape(t *testing.T, st *Store) int {
	t.Helper()
	rows, err := st.pool.Query(t.Context(), `
		UPDATE outbox_events
		   SET attempts = attempts + 1,
		       next_attempt_at = now() + interval '10 seconds',
		       claim_token = gen_random_uuid(),
		       updated_at = now()
		 WHERE id IN (
		     SELECT id FROM outbox_events
		      WHERE status = 'pending' AND next_attempt_at <= now()
		      ORDER BY next_attempt_at
		      FOR UPDATE SKIP LOCKED
		      LIMIT 50)
		 RETURNING id`)
	if err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n
}

func outboxRow(t *testing.T, st *Store, topic string) (id, status string, fenced bool, attempts int) {
	t.Helper()
	if err := st.pool.QueryRow(t.Context(),
		`SELECT id, status, fenced, attempts FROM outbox_events WHERE topic = $1`, topic).
		Scan(&id, &status, &fenced, &attempts); err != nil {
		t.Fatalf("outbox row for %s: %v", topic, err)
	}
	return id, status, fenced, attempts
}

// The whole fenced lifecycle (invariant 61): enqueue class, legacy-claim
// invisibility with zero attempt burn, capable claim, class-restoring retry,
// class-restoring replay (single and all), and the legacy replay SQL failing
// CLOSED on the schema.
func TestFencedClaimLifecycle(t *testing.T) {
	st, ctx := serviceSchemaStore(t)

	if err := st.EnqueueOutbox(ctx, domain.TopicIncidentCorrelation, []byte(`{"incident_id":"x"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	id, status, fenced, _ := outboxRow(t, st, domain.TopicIncidentCorrelation)
	if status != "pending_fenced" || !fenced {
		t.Fatalf("enqueued as status=%s fenced=%v, want pending_fenced/true", status, fenced)
	}
	var attempts int

	// An old owner claims NOTHING and burns NOTHING.
	if n := legacyClaimShape(t, st); n != 0 {
		t.Fatalf("legacy claim shape selected %d fenced rows", n)
	}
	if _, _, _, attempts = outboxRow(t, st, domain.TopicIncidentCorrelation); attempts != 0 {
		t.Fatalf("legacy claim burned attempts: %d", attempts)
	}

	// The capable claim takes it.
	events, err := st.ClaimDueOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("capable claim: %v", err)
	}
	if len(events) != 1 || events[0].Topic != domain.TopicIncidentCorrelation {
		t.Fatalf("capable claim = %+v, want the fenced row", events)
	}

	// A failed attempt below max RESTORES the fenced class — the demotion the
	// design review caught ([267]): the pre-fix transition wrote 'pending' here.
	if _, err := st.FailOutbox(ctx, events[0].ID, events[0].ClaimToken, "boom", 10); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, status, _, _ = outboxRow(t, st, domain.TopicIncidentCorrelation); status != "pending_fenced" {
		t.Fatalf("failed fenced row demoted to %q", status)
	}
	if n := legacyClaimShape(t, st); n != 0 {
		t.Fatalf("legacy claim sees the retried fenced row (%d)", n)
	}

	// Park it dead, then both replay paths must restore the fenced class.
	if _, err := st.pool.Exec(ctx,
		`UPDATE outbox_events SET status = 'dead', attempts = 10 WHERE id = $1`, id); err != nil {
		t.Fatalf("park dead: %v", err)
	}
	if err := st.ReplayDeadOutbox(ctx, id); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, status, _, _ = outboxRow(t, st, domain.TopicIncidentCorrelation); status != "pending_fenced" {
		t.Fatalf("single replay demoted to %q", status)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE outbox_events SET status = 'dead' WHERE id = $1`, id); err != nil {
		t.Fatalf("park dead again: %v", err)
	}
	if _, err := st.ReplayAllDeadOutbox(ctx); err != nil {
		t.Fatalf("replay all: %v", err)
	}
	if _, status, _, _ = outboxRow(t, st, domain.TopicIncidentCorrelation); status != "pending_fenced" {
		t.Fatalf("replay-all demoted to %q", status)
	}

	// The LEGACY replay SQL against a fenced dead row fails closed on the schema.
	if _, err := st.pool.Exec(ctx,
		`UPDATE outbox_events SET status = 'dead' WHERE id = $1`, id); err != nil {
		t.Fatalf("park dead for legacy replay: %v", err)
	}
	_, err = st.pool.Exec(ctx, `
		UPDATE outbox_events
		   SET status = 'pending', attempts = 0, next_attempt_at = now(), last_error = '', updated_at = now()
		 WHERE id = $1 AND status = 'dead'`, id)
	if err == nil {
		t.Fatal("the legacy replay SQL silently unfenced the row — the demotion CHECK is missing")
	}
	if !strings.Contains(err.Error(), "outbox_events_fence_check") {
		t.Fatalf("legacy replay failed for another reason: %v", err)
	}
}

// Ordinary legacy topics keep their exact pre-fence behavior: 'pending' at
// enqueue, 'pending' after a failed attempt, 'pending' after replay.
func TestLegacyTopicLifecycleUnchanged(t *testing.T) {
	st, ctx := serviceSchemaStore(t)

	if err := st.EnqueueOutbox(ctx, domain.TopicSubscriberConfirm, []byte(`{"to":"a@b","subject":"s","body":"b"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	id, status, fenced, _ := outboxRow(t, st, domain.TopicSubscriberConfirm)
	if status != "pending" || fenced {
		t.Fatalf("legacy topic enqueued as status=%s fenced=%v", status, fenced)
	}
	events, err := st.ClaimDueOutbox(ctx, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("claim: %v (%d)", err, len(events))
	}
	if _, err := st.FailOutbox(ctx, events[0].ID, events[0].ClaimToken, "boom", 10); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, status, _, _ = outboxRow(t, st, domain.TopicSubscriberConfirm); status != "pending" {
		t.Fatalf("legacy retry status = %q", status)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE outbox_events SET status = 'dead', attempts = 10 WHERE id = $1`, id); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := st.ReplayDeadOutbox(ctx, id); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, status, _, _ = outboxRow(t, st, domain.TopicSubscriberConfirm); status != "pending" {
		t.Fatalf("legacy replay status = %q", status)
	}
}

// The producer pin (invariant 61): a monitor-anchored open enqueues its
// correlation event AS FENCED in the same transaction; a non-anchored open
// enqueues none.
func TestMonitorAnchoredOpenEnqueuesFencedCorrelation(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments")

	f.open(t, st, ctx, "payments")
	_, status, fenced, _ := outboxRow(t, st, domain.TopicIncidentCorrelation)
	if status != "pending_fenced" || !fenced {
		t.Fatalf("correlation enqueued as status=%s fenced=%v — the producer was demoted", status, fenced)
	}

	if _, err := st.CreateIncidentBySystem(ctx, domain.Incident{
		ProjectID: f.projectID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, "body", "t"); err != nil {
		t.Fatalf("manual incident: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE topic = $1`, domain.TopicIncidentCorrelation).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("non-anchored open enqueued a correlation event (%d total)", n)
	}
}
