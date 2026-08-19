package store

import (
	"context"
	"testing"
	"time"
)

// FR-021 §16.7 / invariant 85 — the evaluators are FENCED to the leader's lock-owning connection,
// not merely gated by a leader check.
//
// The scenario this exists for: a leader passes its watchdog sample, its pinned lock connection then
// dies, a successor takes leadership — and the old process, still inside the window before its next
// sample, finishes an evaluation. Gating cannot see that. A generation/lease CAS cannot fix it
// either, because the episode and its outbox row are written BEFORE the latch upsert: by the time a
// CAS loses, somebody has already been paged, and a stale CLOSE is the worst of it — delivery
// deliberately never drops closes, so recipients would be told an alert ended while the real leader
// keeps it firing.
//
// The proof runs against real Postgres and kills the leader's backend mid-transaction.
func TestADeposedLeaderCannotCommitAnEvaluation(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60) // over the threshold: this pass WOULD fire

	// A unique key per run: a fixed one leaks across runs when a test process is killed while a
	// pooled connection still holds the session-scoped advisory lock.
	key := time.Now().UnixNano()
	session, ok, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		t.Fatalf("elect: %v ok=%v", err, ok)
	}
	// The deposed session still holds a pooled connection, dead or not. Releasing it is what lets
	// the pool close at the end of the test — a leader that loses its lock must give the connection
	// back exactly like one that stepped down cleanly.
	defer session.Release()
	var leaderPID int
	if err := session.conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&leaderPID); err != nil {
		t.Fatalf("leader pid: %v", err)
	}

	// Block the evaluation from inside: an uncommitted row on the exact primary key the evaluator
	// must upsert holds it on the unique index, in the middle of its transaction — after it has read
	// its snapshot and written its episode, before it can commit.
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	// Released on every path, including a t.Fatal below: a blocker left holding the row would keep
	// the evaluation's connection checked out and the pool would never close.
	defer blocker.Rollback(context.Background()) //nolint:errcheck // best effort on the failure path
	if _, err := blocker.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, last_verdict,
		   target_generation, config_generation, evaluated_at, lease_until)
		VALUES ($1,$2,$3,$4,'clear',0,0, now(), now())`,
		f.serviceID, f.projectID, f.targetID, oneBurnRuleKey); err != nil {
		t.Fatalf("blocker insert: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := session.EvaluateServiceBurnAlerts(ctx, burnCadence)
		done <- err
	}()

	// Find WHICH backend the evaluation is running on, by looking for the one now waiting on the
	// blocker's row lock. This is the fence assertion itself: an evaluation on a pool connection
	// waits on some other backend, and then killing the leader's connection would not touch it.
	deadline := time.Now().Add(10 * time.Second)
	var waiterPID int
	for waiterPID == 0 {
		if err := st.pool.QueryRow(ctx, `
			SELECT COALESCE(max(pid), 0) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiterPID); err != nil {
			t.Fatalf("probe waiters: %v", err)
		}
		if waiterPID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the evaluation never reached the contended write")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if waiterPID != leaderPID {
		t.Fatalf("the evaluation is running on backend %d, not the leader's %d: it is NOT fenced to "+
			"the lock-owning connection, so losing the lock would not stop it", waiterPID, leaderPID)
	}

	// The lock-owning connection dies. This is exactly what a network partition or a Postgres
	// restart does to a leader, and it is what makes the lock available to a successor.
	if _, err := st.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, leaderPID); err != nil {
		t.Fatalf("terminate leader: %v", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("rollback blocker: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the deposed leader's evaluation reported success")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the evaluation neither failed nor returned after its connection died")
	}

	// NOTHING of it survives: no episode, no page, no latch. If the evaluator had run on a pool
	// connection, the transaction would have been untouched by the lock connection's death and
	// would have committed an onset behind the successor.
	assertEmpty(t, st, ctx, "service_alert_episodes")
	assertEmpty(t, st, ctx, "service_burn_alert_state")
	var events int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE topic = 'service_alert'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("a deposed leader published %d service alerts", events)
	}

	// And the successor — the process that actually holds leadership now — evaluates normally.
	next, ok, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		t.Fatalf("successor election: %v ok=%v", err, ok)
	}
	defer next.Release()
	got, err := next.EvaluateServiceBurnAlerts(ctx, burnCadence)
	if err != nil {
		t.Fatalf("successor evaluation: %v", err)
	}
	if got.Onsets != 1 {
		t.Fatalf("the successor produced %d onsets, want the one the deposed leader could not", got.Onsets)
	}
}

func assertEmpty(t *testing.T, st *Store, ctx context.Context, table string) {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if n != 0 {
		t.Fatalf("%s has %d rows after a deposed leader's evaluation, want none", table, n)
	}
}

// The CAS is the second half of the same protection, and its RESULT has to be acted on. The episode
// and the outbox row are written BEFORE the latch upsert, so a CAS that silently changed nothing
// would leave the one pairing nobody can defend: a page that went out and a latch that never moved,
// after which the next pass announces the same edge again while the sequence stands still.
func TestAnEvaluationThatLosesTheCASPublishesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60) // this pass would fire

	// A NEWER verdict already exists for the rule — what a successor writes while a slower
	// evaluator is still mid-pass.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict,
		   target_generation, config_generation, evaluated_at, lease_until)
		VALUES ($1,$2,$3,$4,false,'clear',0,0, now() + interval '1 hour', now() + interval '2 hours')`,
		f.serviceID, f.projectID, f.targetID, oneBurnRuleKey); err != nil {
		t.Fatalf("newer verdict: %v", err)
	}

	if _, err := st.evaluateServiceBurnAlertsOn(ctx, st.pool, burnCadence); err == nil {
		t.Fatal("an evaluation that lost the CAS reported success")
	}
	if events := allServiceAlerts(t, st, ctx); len(events) != 0 {
		t.Fatalf("it published %+v anyway", events)
	}
	assertEmpty(t, st, ctx, "service_alert_episodes")

	// The newer verdict is untouched: the loser rolled back whole.
	var verdict string
	var firing bool
	if err := st.pool.QueryRow(ctx,
		`SELECT last_verdict, firing FROM service_burn_alert_state`).Scan(&verdict, &firing); err != nil {
		t.Fatalf("read latch: %v", err)
	}
	if verdict != "clear" || firing {
		t.Fatalf("the newer verdict became %q/firing=%v", verdict, firing)
	}
}
