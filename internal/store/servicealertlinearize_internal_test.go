package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.6a/§16.7 — an evaluation and a paging-config write are LINEARIZABLE.
//
// The dangerous order is the one nothing else covers: the evaluator takes its REPEATABLE READ
// snapshot, an operator's edit commits, and the evaluator — still holding the old configuration —
// publishes a page for a policy that no longer exists. Nothing closes it either, because the writer
// looked for an open episode BEFORE the evaluator opened one. Generation mismatch dis-arms the
// members' delegation afterwards, but it does not un-send the service's own stale page.
//
// The opposite order needs no barrier and is covered by the write-surface tests, which run the
// evaluator to an onset and then the writer, and assert exactly one close.

// startWriterHoldingTheService opens a transaction holding the service row exactly as both config
// writers do, and hands back a function that performs `change` and commits. Between the two the
// evaluator can be started: it will reach its own lock statement and wait there, which is the
// interleaving being tested.
func startWriterHoldingTheService(
	t *testing.T, st *Store, ctx context.Context, serviceID string,
) func(change string, args ...any) {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM services WHERE id = $1 FOR UPDATE`, serviceID); err != nil {
		t.Fatalf("writer lock: %v", err)
	}
	return func(change string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, change, args...); err != nil {
			t.Fatalf("writer change: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("writer commit: %v", err)
		}
	}
}

// waitForLockWait blocks until some backend of this database is waiting on a lock, which is how the
// test knows the evaluator has taken its snapshot and reached the linearization statement.
func waitForLockWait(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := st.pool.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			               WHERE datname = current_database() AND wait_event_type = 'Lock')`).
			Scan(&waiting); err != nil {
			t.Fatalf("probe waiters: %v", err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the evaluation never reached the linearization lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// BURN, disabled underneath a first evaluation that has no latch row at all — the case with no
// conflicting child UPDATE to save it, where the pass would otherwise insert an episode, a latch and
// a current-sequence onset for a target the operator has just switched off.
func TestBurnEvaluationCannotPublishUnderADisabledTarget(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60) // would fire
	assertEmpty(t, st, ctx, "service_burn_alert_state")

	commit := startWriterHoldingTheService(t, st, ctx, f.serviceID)
	done := make(chan error, 1)
	go func() {
		_, err := st.evaluateServiceBurnAlertsOn(ctx, st.pool, burnCadence)
		done <- err
	}()
	waitForLockWait(t, st, ctx)

	// The operator disables burn alerting while the evaluation holds the old configuration.
	commit(`UPDATE sla_targets SET burn_alert_enabled = false WHERE service_id = $1`, f.serviceID)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the evaluation published under a configuration that had already been replaced")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the evaluation neither failed nor returned")
	}
	if events := allServiceAlerts(t, st, ctx); len(events) != 0 {
		t.Fatalf("it published %+v", events)
	}
	assertEmpty(t, st, ctx, "service_alert_episodes")
	assertEmpty(t, st, ctx, "service_burn_alert_state")
}

// BURN, rule replaced underneath the evaluation: same shape, and the one where a stale onset would
// name a rule key nobody declares any more, so no later close could ever address it.
func TestBurnEvaluationCannotPublishAReplacedRule(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)

	commit := startWriterHoldingTheService(t, st, ctx, f.serviceID)
	done := make(chan error, 1)
	go func() {
		_, err := st.evaluateServiceBurnAlertsOn(ctx, st.pool, burnCadence)
		done <- err
	}()
	waitForLockWait(t, st, ctx)

	commit(`UPDATE sla_targets
	           SET burn_rules = '[{"long_window_seconds":300,"short_window_seconds":120,
	                               "threshold":25,"severity":"page"}]'::jsonb
	         WHERE service_id = $1`, f.serviceID)

	if err := <-done; err == nil {
		t.Fatal("the evaluation published a verdict about a rule that had already been replaced")
	}
	if events := allServiceAlerts(t, st, ctx); len(events) != 0 {
		t.Fatalf("it published %+v", events)
	}
	assertEmpty(t, st, ctx, "service_alert_episodes")
}

// LIVE, narrowed underneath the evaluation: `page_on` emptied is a legal declaration meaning "page
// for no state", and a pass that announced DOWN against the old policy would be a page the operator
// had just switched off.
//
// Honest note on what this test can and cannot distinguish: the property holds here for TWO reasons
// at once. The explicit lock aborts the pass before it does any work, and the episode INSERT would
// have taken a row lock on the same service row anyway through its composite foreign key — which
// under REPEATABLE READ raises the same serialization failure. So removing the explicit lock alone
// does not make this test fail. It stays because the PROPERTY is what matters, and because the
// implicit protection is not something to rely on: that foreign key did not exist at all until this
// phase added it, and an evaluation should not depend on which constraint happens to be attached to
// the row it publishes about.
func TestLiveEvaluationCannotPublishUnderANarrowedPolicy(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	// One pass short of the confirmation threshold, so the NEXT pass is the one that announces.
	evalOnce(t, st, ctx)
	if events := alertEvents(t, st, ctx); len(events) != 0 {
		t.Fatalf("the fixture announced too early: %+v", events)
	}

	commit := startWriterHoldingTheService(t, st, ctx, f.serviceID)
	done := make(chan error, 1)
	go func() {
		_, err := st.evaluateServiceAlertsOn(ctx, st.pool, evalCadence)
		done <- err
	}()
	waitForLockWait(t, st, ctx)

	commit(`UPDATE services SET page_on = '{}', page_on_unknown = false WHERE id = $1`, f.serviceID)

	if err := <-done; err == nil {
		t.Fatal("the evaluation announced under a policy that had already been narrowed")
	}
	if events := alertEvents(t, st, ctx); len(events) != 0 {
		t.Fatalf("it published %+v", events)
	}
	assertEmpty(t, st, ctx, "service_alert_episodes")
}

// The writer's clock is read AFTER it stops waiting, not when its transaction began.
//
// `now()` is transaction-START time, so a writer that queued behind an evaluation would stamp its
// close with an instant from before the wait — potentially before the episode that evaluation opened
// while it waited. An announcement whose ending precedes its beginning is not a record anybody can
// read, and it is the kind of defect that only ever appears under contention.
func TestAWaitingWriterStampsTheCloseAfterItsWait(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)
	plantBurn(t, st, ctx, f, 5, minute/60)
	if got := burnEvalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("onsets = %d, want a firing rule to close", got.Onsets)
	}

	// Hold the service row, so the writer's transaction starts and then WAITS.
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(ctx,
		`SELECT 1 FROM services WHERE id = $1 FOR UPDATE`, f.serviceID); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", false, nil, AlertActor{})
	}()
	waitForLockWait(t, st, ctx)

	// The instant the wait ends. Anything the writer stamps must be at or after this.
	var released time.Time
	if err := st.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&released); err != nil {
		t.Fatalf("read clock: %v", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("disable burn: %v", err)
	}

	var started, closed time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT started_at, closed_at FROM service_alert_episodes`).Scan(&started, &closed); err != nil {
		t.Fatalf("read episode: %v", err)
	}
	if closed.Before(started) {
		t.Fatalf("the announcement was closed at %s, BEFORE it opened at %s", closed, started)
	}
	if closed.Before(released) {
		t.Fatalf("the close is stamped %s, before the writer's wait even ended at %s: the clock was "+
			"read at transaction start rather than after the locks", closed, released)
	}
}

// §16.6a "omitted = unchanged" is a promise about the row at COMMIT time, not about the value the
// caller read a moment earlier.
//
// Two PATCHes that each mention ONE field must both survive, in either order. An earlier revision
// read the policy, merged in Go and handed the store a full value: both writers then read the same
// row, and whichever committed second wrote its stale copy of the field it never mentioned —
// silently restoring ownership after an explicit disown, or dropping the confirmation change,
// depending on which won. The merge now happens inside the write transaction, under the row lock.
func TestConcurrentPartialPatchesBothSurvive(t *testing.T) {
	for _, order := range []string{"disown first", "confirm first"} {
		t.Run(order, func(t *testing.T) {
			st, ctx := serviceSchemaStore(t)
			f := armedService(t, st, ctx)
			if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
				FullServiceAlertPolicyPatch(domain.ServiceAlertPolicy{
					OwnsPaging: true,
					PageOn:     []domain.ServiceAlertState{domain.ServiceAlertDown},
					// A distinctive starting value, so "unchanged" is falsifiable.
					ConfirmEvaluations: 2,
				}), AlertActor{}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			disown := func() error {
				no := false
				_, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
					ServiceAlertPolicyPatch{OwnsPaging: &no}, AlertActor{})
				return err
			}
			confirm := func() error {
				four := 4
				_, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
					ServiceAlertPolicyPatch{ConfirmEvaluations: &four}, AlertActor{})
				return err
			}

			// A blocker holds the row so BOTH writers have unquestionably begun before either can
			// commit — the interleaving the race needs, made deterministic.
			blocker, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin blocker: %v", err)
			}
			if _, err := blocker.Exec(ctx,
				`SELECT 1 FROM services WHERE id = $1 FOR UPDATE`, f.serviceID); err != nil {
				t.Fatalf("blocker lock: %v", err)
			}

			first, second := disown, confirm
			if order == "confirm first" {
				first, second = confirm, disown
			}
			firstDone, secondDone := make(chan error, 1), make(chan error, 1)
			go func() { firstDone <- first() }()
			waitForLockWait(t, st, ctx)
			go func() { secondDone <- second() }()
			// Both are queued behind the blocker; releasing it lets them run one after the other.
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatalf("release blocker: %v", err)
			}
			for _, done := range []chan error{firstDone, secondDone} {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("patch: %v", err)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("a patch never returned")
				}
			}

			got, err := st.ServiceAlertPolicy(ctx, f.projectID, f.serviceID)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.OwnsPaging {
				t.Fatalf("ownership came back after an explicit disown (%+v): the other patch "+
					"never mentioned it, so it wrote a stale copy of a field it did not own", got)
			}
			if got.ConfirmEvaluations != 4 {
				t.Fatalf("confirm_evaluations = %d, want 4: the disown wrote its stale copy of a "+
					"field it never mentioned", got.ConfirmEvaluations)
			}
		})
	}
}
