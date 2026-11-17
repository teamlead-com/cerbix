package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-043 D1, D2, D9, I7, I8 — what pauses while a recovery run is in progress, and what the run's
// end releases. The pauses are asserted on a run that is SEEDED onto a down monitor, so what they
// prove is the query's own condition rather than how the run was started; the breaks and the
// completions go through RecordScheduledResult.

// D1, I7, I8 — no "still DOWN" reminder while a run is in progress; when the run breaks, reminders
// continue on the schedule they already had, without any new down transition.
func TestRecoveryRunPausesRemindersAndABreakResumesThem(t *testing.T) {
	st, ctx := outboxTestStore(t)
	projectID := recoveryProject(t, st, ctx, "rt-remind")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-remind", 3, 1)
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET renotify_seconds = 60 WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	m.feed(false) // the down verdict stamps last_notified_at
	backdate := func() {
		t.Helper()
		if _, err := st.pool.Exec(ctx,
			`UPDATE monitors SET last_notified_at = now() - interval '10 minutes' WHERE id = $1`, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	reminders := func() int {
		t.Helper()
		n := 0
		for _, p := range transitionPayloads(t, st, ctx, m.ID) {
			if decodeTransition(t, p).Reminder {
				n++
			}
		}
		return n
	}

	// The pause, on a seeded run: the reminder is due, and the run alone keeps it from going out.
	backdate()
	seedRun(t, st, ctx, m.ID, 1)
	if n, err := st.EnqueueRenotifyReminders(ctx); err != nil || n != 0 {
		t.Fatalf("a due reminder during a run: enqueued %d (err %v), want 0 (D1)", n, err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET consecutive_successes = 0, recovery_started_at = NULL WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}

	// The same through ingest: a success starts a run and must not clear last_notified_at (§4.2) …
	backdate()
	m.feed(true)
	if w := m.row(); w.Status != "down" || w.Successes != 1 || w.LastNotified == nil {
		t.Fatalf("after the first success: %+v, want down / run 1 / last_notified_at kept", w)
	}
	if n, err := st.EnqueueRenotifyReminders(ctx); err != nil || n != 0 {
		t.Fatalf("reminder during an ingested run: %d (err %v), want 0", n, err)
	}
	seq := m.row().StateSequence

	// … so that when the run breaks the reminder is due again, with no new down transition (I7).
	m.feed(false)
	if w := m.row(); w.StateSequence != seq {
		t.Fatalf("the broken run moved state_sequence %d→%d: that would be a new transition", seq, w.StateSequence)
	}
	if n, err := st.EnqueueRenotifyReminders(ctx); err != nil || n != 1 {
		t.Fatalf("after the run broke: enqueued %d (err %v), want 1 (I7)", n, err)
	}
	if got := reminders(); got != 1 {
		t.Fatalf("%d reminder events, want 1", got)
	}
}

// escalatingRecoveryMonitor builds a monitor at threshold 3 with an escalation ladder, takes it down
// through ingest and opens its auto-incident the way the reconciler does.
func escalatingRecoveryMonitor(t *testing.T, st *Store, ctx context.Context, slug string, policy domain.EscalationPolicy) (*recoveryMonitor, domain.Incident) {
	t.Helper()
	projectID := recoveryProject(t, st, ctx, slug)
	ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: projectID, Type: domain.ChannelWebhook, Name: slug,
		Config: map[string]string{"url": "https://example.com/" + slug}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	policy.ProjectID = projectID
	policy.Name = slug
	for i := range policy.Steps {
		policy.Steps[i].Targets = []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: ch.ID}}
	}
	p, err := st.CreateEscalationPolicy(ctx, policy)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	m := newRecoveryMonitor(t, st, ctx, projectID, slug, 3, 1)
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET escalation_policy_id = $2, auto_incident = true, renotify_seconds = 300 WHERE id = $1`,
		m.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	m.feed(false)
	inc, err := st.CreateIncidentBySystem(ctx, domain.Incident{
		ProjectID: projectID, MonitorID: m.ID, Title: slug + " is down",
		Status: domain.IncidentInvestigating, Source: domain.SourceAuto,
	}, "auto opened", "system")
	if err != nil {
		t.Fatalf("incident: %v", err)
	}
	return m, inc
}

func monitorSteps(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	pass, err := st.AdvanceEscalations(ctx)
	if err != nil {
		t.Fatalf("advance escalations: %v", err)
	}
	return pass.MonitorSteps
}

// D2, D9, I8 — a step that falls due during a run does not fire; the run's break fires every step
// that fell due during it in ONE pass; nothing is re-dated.
func TestRecoveryRunPausesTheLadderAndABreakFiresEveryOverdueStep(t *testing.T) {
	st, ctx := outboxTestStore(t)
	m, inc := escalatingRecoveryMonitor(t, st, ctx, "rt-ladder", domain.EscalationPolicy{Steps: []domain.EscalationStep{
		{AfterSeconds: 0}, {AfterSeconds: 600}, {AfterSeconds: 1200},
	}})
	if n := monitorSteps(t, st, ctx); n != 1 {
		t.Fatalf("step 0: fired %d, want 1", n)
	}

	seedRun(t, st, ctx, m.ID, 1)
	// Steps 1 and 2 fall due during the run.
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET started_at = now() - interval '30 minutes' WHERE id = $1`, inc.ID); err != nil {
		t.Fatal(err)
	}
	var before domain.Incident
	if err := st.pool.QueryRow(ctx, `SELECT started_at, escalation_step FROM incidents WHERE id = $1`, inc.ID).
		Scan(&before.StartedAt, &before.EscalationStep); err != nil {
		t.Fatal(err)
	}
	if n := monitorSteps(t, st, ctx); n != 0 {
		t.Fatalf("two steps due during a run: fired %d, want 0 (D2)", n)
	}
	var after domain.Incident
	if err := st.pool.QueryRow(ctx, `SELECT started_at, escalation_step FROM incidents WHERE id = $1`, inc.ID).
		Scan(&after.StartedAt, &after.EscalationStep); err != nil {
		t.Fatal(err)
	}
	if !after.StartedAt.Equal(before.StartedAt) || after.EscalationStep != before.EscalationStep {
		t.Fatalf("the paused pass rewrote the ladder: %+v → %+v (D9: no timestamp is rewritten)", before, after)
	}

	m.feed(false) // the run breaks
	if w := m.row(); w.Successes != 0 || w.Status != "down" {
		t.Fatalf("after the failure: %+v, want down with no run", w)
	}
	if n := monitorSteps(t, st, ctx); n != 2 {
		t.Fatalf("first pass after the break: fired %d, want both overdue steps at once (D9)", n)
	}
}

// D2, D9 — the repetition of the last step pauses the same way and resumes after a break.
func TestRecoveryRunPausesRepeatLastAndABreakResumesIt(t *testing.T) {
	st, ctx := outboxTestStore(t)
	m, inc := escalatingRecoveryMonitor(t, st, ctx, "rt-repeat", domain.EscalationPolicy{
		RepeatLast: true, Steps: []domain.EscalationStep{{AfterSeconds: 0}},
	})
	if n := monitorSteps(t, st, ctx); n != 1 {
		t.Fatalf("step 0: fired %d, want 1", n)
	}
	seedRun(t, st, ctx, m.ID, 2)
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET last_escalated_at = now() - interval '10 minutes' WHERE id = $1`, inc.ID); err != nil {
		t.Fatal(err)
	}
	if n := monitorSteps(t, st, ctx); n != 0 {
		t.Fatalf("repeat_last during a run: fired %d, want 0", n)
	}
	m.feed(false)
	if n := monitorSteps(t, st, ctx); n != 1 {
		t.Fatalf("repeat_last after the break: fired %d, want 1", n)
	}
}

// I8 — a run that completes resolves the episode before the ladder ever fires again.
func TestRecoveryRunThatCompletesNeverFiresThePausedStep(t *testing.T) {
	st, ctx := outboxTestStore(t)
	m, inc := escalatingRecoveryMonitor(t, st, ctx, "rt-complete", domain.EscalationPolicy{Steps: []domain.EscalationStep{
		{AfterSeconds: 0}, {AfterSeconds: 600},
	}})
	if n := monitorSteps(t, st, ctx); n != 1 {
		t.Fatalf("step 0: fired %d, want 1", n)
	}
	m.feed(true)
	m.feed(true)
	if w := m.row(); w.Status != "down" || w.Successes != 2 {
		t.Fatalf("mid-run: %+v, want down / run 2", w)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET started_at = now() - interval '30 minutes' WHERE id = $1`, inc.ID); err != nil {
		t.Fatal(err)
	}
	if n := monitorSteps(t, st, ctx); n != 0 {
		t.Fatalf("step 1 due during the run: fired %d, want 0", n)
	}
	if o := m.feed(true); o.Cur != domain.StatusUp {
		t.Fatalf("the third success: %+v, want up", o)
	}
	if n := monitorSteps(t, st, ctx); n != 0 {
		t.Fatalf("after the confirmed recovery: fired %d, want 0", n)
	}
}

// I8 — a service's ladder is driven by the service, not by one member's run (§4.3).
func TestRecoveryRunDoesNotPauseAServiceLadder(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f, _ := escalatingService(t, st, ctx)
	setFiring(t, st, ctx, f.serviceID, true, true)
	seedRun(t, st, ctx, f.monitorID, 1)
	pass, err := st.AdvanceEscalations(ctx)
	if err != nil || pass.ServiceSteps != 1 {
		t.Fatalf("service ladder with a member mid-run: fired %d (err %v), want 1", pass.ServiceSteps, err)
	}
}

// waitForLockWait blocks until a backend other than the holder is waiting on a lock that the holder
// owns — the barrier §7 asks for before the database clock is read.
func waitForRowLockWaitOn(t *testing.T, st *Store, ctx context.Context, holderPID int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := st.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                 WHERE wait_event_type = 'Lock' AND $1 = ANY (pg_blocking_pids(pid)))`,
			holderPID).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the submission was never observed waiting on the monitor's row lock")
}

// I17, D6 — the first-OK instant is read after the monitor's row lock is held. For each entry point
// that can start a run, a first success is submitted while another transaction holds the row, the
// submission is observed WAITING on that lock, and only then the database clock is read. The stamp
// may not be earlier than that reading. Stamped with now() (the transaction's start) or with the
// timestamp of the statement that acquires the lock, both of which are taken before the wait, it is.
func TestRecoveryFirstOKIsReadAfterTheRowLock(t *testing.T) {
	st, ctx := outboxTestStore(t)
	projectID := recoveryProject(t, st, ctx, "rt-clock")

	for _, entry := range []struct {
		name   string
		submit func(m *recoveryMonitor) error
	}{
		{"RecordScheduledResult", func(m *recoveryMonitor) error {
			m.next = m.next.Add(time.Second)
			_, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
				MonitorID: m.ID, Ts: m.next, Up: true, ExecutionRevision: m.revision(),
			})
			return err
		}},
		{"Store.RecordCheckStatus", func(m *recoveryMonitor) error {
			_, _, _, err := st.RecordCheckStatus(ctx, m.ID, true)
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			m := newRecoveryMonitor(t, st, ctx, projectID, "rt-clock-"+entry.name, 3, 1)
			m.t = t
			m.feed(false)

			holder, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx) //nolint:errcheck // released below; no-op then
			var holderPID int32
			if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(ctx, `SELECT 1 FROM monitors WHERE id = $1 FOR UPDATE`, m.ID); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- entry.submit(m) }()
			waitForRowLockWaitOn(t, st, ctx, holderPID)

			var reading time.Time
			if err := st.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&reading); err != nil {
				t.Fatal(err)
			}
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("submission: %v", err)
			}
			w := m.row()
			if w.Successes != 1 || w.StartedAt == nil {
				t.Fatalf("after the first success: %+v, want a run of 1 with a stamp", w)
			}
			if w.StartedAt.Before(reading) {
				t.Fatalf("first OK %s is earlier than the clock read while the submission waited on the lock (%s) — "+
					"the stamp was taken before the row lock (I17)", w.StartedAt.Format(time.RFC3339Nano), reading.Format(time.RFC3339Nano))
			}
		})
	}
}

// I17 on one ingest consumer — the verdict, then the auto-incident opened after its commit, then the
// first success: the first-OK is not earlier than the incident's started_at.
func TestRecoveryFirstOKFollowsTheIncidentOnTheSequentialPath(t *testing.T) {
	st, ctx := outboxTestStore(t)
	projectID := recoveryProject(t, st, ctx, "rt-seq")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-seq", 3, 1)
	m.feed(false)
	inc, err := st.CreateIncidentBySystem(ctx, domain.Incident{
		ProjectID: projectID, MonitorID: m.ID, Title: "rt-seq is down",
		Status: domain.IncidentInvestigating, Source: domain.SourceAuto,
	}, "auto opened", "system")
	if err != nil {
		t.Fatal(err)
	}
	m.feed(true)
	w := m.row()
	if w.StartedAt == nil {
		t.Fatalf("no first-OK stamp: %+v", w)
	}
	if w.StartedAt.Before(inc.StartedAt) {
		t.Fatalf("first OK %s precedes the incident's started_at %s", w.StartedAt, inc.StartedAt)
	}
}
