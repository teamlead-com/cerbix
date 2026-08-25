package store

import (
	"context"
	"errors"
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

// A resolved incident is TERMINAL, and terminal has to hold against a writer that decided before
// the resolve landed. The API checks the status outside this store's transaction, so the window is
// real: two editors, or an editor and the evaluator's auto-resolve, and the loser's write arrives
// after the incident closed. Reopening it would leave `status = investigating` with `resolved_at`
// stamped — a state no legal sequence produces — and for an auto-incident it would re-enter
// `incidents_service_open_auto_idx` and refuse the NEXT outage its own incident.
func TestAWriteThatRacedAResolveIsRefusedWithoutATrace(t *testing.T) {
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
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "fixed", Author: "first",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	resolvedAt := incidentResolvedAt(t, st, ctx, inc.ID)
	events := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "pending")
	timeline := incidentUpdateCount(t, st, ctx, inc.ID)

	// The loser of the race: it read `investigating` before the resolve committed.
	_, err = st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentInvestigating, Body: "still looking", Author: "second",
	})
	if !errors.Is(err, ErrIncidentTerminal) {
		t.Fatalf("write against a resolved incident: %v, want ErrIncidentTerminal", err)
	}

	var status string
	if err := st.pool.QueryRow(ctx,
		`SELECT status FROM incidents WHERE id = $1`, inc.ID).Scan(&status); err != nil {
		t.Fatalf("reread incident: %v", err)
	}
	if status != string(domain.IncidentResolved) {
		t.Fatalf("status = %q after a refused write, want it to stay resolved", status)
	}
	if got := incidentResolvedAt(t, st, ctx, inc.ID); !got.Equal(resolvedAt) {
		t.Fatalf("resolved_at moved from %v to %v", resolvedAt, got)
	}
	// A refusal writes NOTHING: no timeline entry, no lifecycle event for a change that did not
	// happen. Announcing an update nobody applied is the same lie in a different channel.
	if got := incidentUpdateCount(t, st, ctx, inc.ID); got != timeline {
		t.Fatalf("timeline rows = %d after a refused write, want %d", got, timeline)
	}
	if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "pending"); got != events {
		t.Fatalf("incident_event rows = %d after a refused write, want %d", got, events)
	}

	// And a write to an incident that never existed is a different answer, not the same one.
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: "00000000-0000-0000-0000-000000000000",
		Status:     domain.IncidentInvestigating, Body: "ghost", Author: "second",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write against a missing incident: %v, want ErrNotFound", err)
	}
}

func incidentResolvedAt(t *testing.T, st *Store, ctx context.Context, id string) time.Time {
	t.Helper()
	var at *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT resolved_at FROM incidents WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read resolved_at: %v", err)
	}
	if at == nil {
		t.Fatal("resolved_at is NULL on a resolved incident")
	}
	return *at
}

func incidentUpdateCount(t *testing.T, st *Store, ctx context.Context, id string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_updates WHERE incident_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count timeline: %v", err)
	}
	return n
}

// The lifecycle is documented as forward-flowing, and until now only its LAST step was enforced.
// `resolved` was terminal; `monitoring → identified` was accepted without comment, sequentially,
// through the ordinary API. The public timeline would then read as though the operators had
// un-diagnosed the outage.
func TestAnIncidentCannotWalkBackwardsThroughItsLifecycle(t *testing.T) {
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
	post := func(status domain.IncidentStatus, body string) error {
		_, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
			IncidentID: inc.ID, Status: status, Body: body, Author: "author",
		})
		return err
	}

	if err := post(domain.IncidentIdentified, "cause found"); err != nil {
		t.Fatalf("forward to identified: %v", err)
	}
	if err := post(domain.IncidentMonitoring, "fix applied"); err != nil {
		t.Fatalf("forward to monitoring: %v", err)
	}
	// Staying put is legal: an update that adds information without moving the lifecycle.
	if err := post(domain.IncidentMonitoring, "still watching"); err != nil {
		t.Fatalf("same status: %v", err)
	}
	timeline := incidentUpdateCount(t, st, ctx, inc.ID)
	events := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "pending")
	for _, backward := range []domain.IncidentStatus{domain.IncidentIdentified, domain.IncidentInvestigating} {
		if err := post(backward, "back"); !errors.Is(err, ErrStatusRegression) {
			t.Fatalf("monitoring → %s: %v, want ErrStatusRegression", backward, err)
		}
		// A refusal writes NOTHING — not a timeline entry nobody can act on, and not an event
		// announcing a change that did not happen.
		if got := incidentUpdateCount(t, st, ctx, inc.ID); got != timeline {
			t.Fatalf("a refused %s left %d timeline rows, want %d", backward, got, timeline)
		}
		if got := st.countOutbox(ctx, t, domain.TopicIncidentEvent, "pending"); got != events {
			t.Fatalf("a refused %s enqueued an event: %d rows, want %d", backward, got, events)
		}
	}
	var status string
	if err := st.pool.QueryRow(ctx, `SELECT status FROM incidents WHERE id = $1`, inc.ID).Scan(&status); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if status != string(domain.IncidentMonitoring) {
		t.Fatalf("status = %q after two refused regressions, want monitoring", status)
	}
}

// A plain comment carries NO status and means "whatever the incident is when this lands". The store
// resolves that against the row it has locked, so a comment written while somebody else moves the
// incident forward cannot revert it. This is the race the API's own pre-check cannot cover: it reads
// the incident in a different transaction.
func TestAPlainCommentTakesTheStatusItLandsOnNotTheOneItRead(t *testing.T) {
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

	// The commenter has read `investigating` — that read is the whole setup, and it is why the old
	// handler would have sent that value back.
	seen := domain.IncidentInvestigating
	// Somebody else moves the incident on, and commits.
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentIdentified, Body: "cause found", Author: "other",
	}); err != nil {
		t.Fatalf("concurrent transition: %v", err)
	}
	// Now the comment lands, carrying no status.
	created, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Body: "adding a note", Author: "commenter",
	})
	if err != nil {
		t.Fatalf("plain comment: %v", err)
	}
	if created.Status != domain.IncidentIdentified {
		t.Fatalf("the comment recorded %q, want the status it landed on (identified); it read %q "+
			"before the transition", created.Status, seen)
	}
	var status string
	if err := st.pool.QueryRow(ctx, `SELECT status FROM incidents WHERE id = $1`, inc.ID).Scan(&status); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if status != string(domain.IncidentIdentified) {
		t.Fatalf("the incident is %q after a plain comment, want identified — the comment reverted a "+
			"transition it never saw", status)
	}
}

// A writer that WAITED on the incident lock must stamp what it writes after the wait, not before it.
//
// `now()` is the transaction's start time, so a writer blocked for a minute records a timeline entry
// dated a minute before the update it is actually following. `ListIncidentUpdates` orders by
// `created_at`, so the public timeline would then show the two in the wrong order — the record
// claiming a sequence the database never had. `statement_timestamp()` read in its own statement AFTER
// the lock is the first clock reading this writer is entitled to.
func TestAWaitingWriterStampsItsUpdateAfterTheWait(t *testing.T) {
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

	// Hold the INCIDENT row — not the service, not a table lock. That is the row the writer must
	// wait for, and a timestamp taken before this is released is a timestamp from the past.
	hold, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	var one int
	if err := hold.QueryRow(ctx, `SELECT 1 FROM incidents WHERE id = $1 FOR UPDATE`, inc.ID).Scan(&one); err != nil {
		t.Fatalf("hold the incident row: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
			IncidentID: inc.ID, Status: domain.IncidentIdentified, Body: "cause found", Author: "waiter",
		})
		done <- err
	}()

	// WAIT FOR THE WAIT, rather than sleeping and hoping. A sleep proves nothing about whether the
	// goroutine has even begun its transaction: under load it can reach the lock after the release
	// below, and then a `now()` mutant passes because there was no wait to be early about.
	waitForLockWait(t, st, ctx)
	// A moment of separation so the assertion is about the clock and not about scheduling noise.
	time.Sleep(200 * time.Millisecond)
	var released time.Time
	if err := st.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&released); err != nil {
		t.Fatalf("read release instant: %v", err)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the waiting writer failed: %v", err)
	}

	var createdAt, updatedAt time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT u.created_at, i.updated_at
		  FROM incident_updates u JOIN incidents i ON i.id = u.incident_id
		 WHERE u.incident_id = $1 AND u.author = 'waiter'`, inc.ID).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatalf("read the waiter's row: %v", err)
	}
	if createdAt.Before(released) {
		t.Fatalf("the timeline entry is stamped %s, before the lock was released at %s: a writer that "+
			"waited is claiming it wrote earlier than it did", createdAt, released)
	}
	if !updatedAt.Equal(createdAt) {
		t.Fatalf("incident.updated_at %s and the update's created_at %s disagree; one transaction "+
			"writes one instant", updatedAt, createdAt)
	}
	// The EVENT is scheduled on that instant too. `next_attempt_at` is what the claim orders by, so
	// an event left on the transaction-start default can come due before the one it followed.
	var eventCreated, nextAttempt time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT created_at, next_attempt_at FROM outbox_events
		 WHERE topic = $1 ORDER BY created_at DESC LIMIT 1`, domain.TopicIncidentEvent).
		Scan(&eventCreated, &nextAttempt); err != nil {
		t.Fatalf("read the queued event: %v", err)
	}
	if eventCreated.Before(released) || nextAttempt.Before(released) {
		t.Fatalf("the event is scheduled at %s/%s, before the lock was released at %s",
			eventCreated, nextAttempt, released)
	}
}

// An incident CREATED as resolved is stamped, because D-0020 promises `resolved_at` the first time an
// incident reaches Resolved and creation is one of those times. Without the stamp the row falls out of
// BOTH status-page lists at once: active filters `status <> 'resolved'`, recent filters `resolved_at IS
// NOT NULL`. The operator records a past outage and it appears nowhere.
func TestAnIncidentCreatedResolvedIsStampedAndStaysVisible(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "yesterday's outage", Status: domain.IncidentResolved,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, "recorded after the fact", "author")
	if err != nil {
		t.Fatalf("create resolved incident: %v", err)
	}
	if inc.ResolvedAt == nil {
		t.Fatal("an incident created as resolved has no resolved_at: it is invisible to the active " +
			"list AND to the recent list")
	}
	// And an ordinary open incident is NOT stamped — the stamp means "reached Resolved", not
	// "was written".
	open, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "live", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opening", "author")
	if err != nil {
		t.Fatalf("create open incident: %v", err)
	}
	if open.ResolvedAt != nil {
		t.Fatalf("an investigating incident carries resolved_at %v", open.ResolvedAt)
	}
}

// The acknowledgement writes the same row as a timeline update and had the same defect: a single
// UPDATE looks atomic, but its `now()` is fixed when its transaction began, so an acknowledgement that
// queued behind an update stamps the incident's modification time BEFORE the update it waited for.
func TestAWaitingAcknowledgementStampsAfterItsWait(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "down", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "opening", "system")
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}

	hold, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	var one int
	if err := hold.QueryRow(ctx, `SELECT 1 FROM incidents WHERE id = $1 FOR UPDATE`, inc.ID).Scan(&one); err != nil {
		t.Fatalf("hold the incident row: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.AcknowledgeIncident(ctx, inc.ID, "u1")
		done <- err
	}()
	waitForLockWait(t, st, ctx)
	time.Sleep(200 * time.Millisecond)
	var released time.Time
	if err := st.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&released); err != nil {
		t.Fatalf("read release instant: %v", err)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the waiting acknowledgement failed: %v", err)
	}

	var ackAt, updatedAt time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT acknowledged_at, updated_at FROM incidents WHERE id = $1`, inc.ID).Scan(&ackAt, &updatedAt); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if ackAt.Before(released) || updatedAt.Before(released) {
		t.Fatalf("acknowledged_at %s / updated_at %s predate the lock release at %s: the incident's "+
			"own modification time walked backwards", ackAt, updatedAt, released)
	}
}
