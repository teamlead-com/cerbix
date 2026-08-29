package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Agent B — adversarial pass over changeset 2 (func-reliability-gate §7 *Override*,
// *Attribution*; D9, D13a; invariants 8, 9, 17). The lock-wait cases the reviewer asked for
// ([24]) after b7518b9 moved every gate write onto statement_timestamp(): the clock an override
// is judged against is the instant the statement RUNS, after the service-row lock, never the
// instant its transaction queued.

// holdServiceRow takes the service row FOR UPDATE on a dedicated connection and returns the
// release function. Every gate write serializes on this row (lockServiceRowTx).
func holdServiceRow(t *testing.T, st *Store, ctx context.Context, serviceID string) (release func()) {
	t.Helper()
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM services WHERE id = $1 FOR UPDATE`, serviceID); err != nil {
		t.Fatal(err)
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		_ = tx.Rollback(ctx)
		conn.Release()
	}
}

// waitForRowLockWaiter polls pg_stat_activity until a backend waits on a lock while running a
// statement containing `fragment`.
func waitForRowLockWaiter(t *testing.T, st *Store, ctx context.Context, fragment string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var n int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'`, fragment).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend waited on a lock while running %q within %s", fragment, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sleepPastOnDBClock waits until the database clock is past `t` by `margin`.
func sleepPastOnDBClock(t *testing.T, st *Store, ctx context.Context, until time.Time, margin time.Duration) {
	t.Helper()
	for {
		if gateDBNow(t, st, ctx).After(until.Add(margin)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Reviewer [24] (a): an ACTIVE override expires WHILE a second create waits on the service
// row. When the waiter finally runs, the expired-slot closure must judge expiry at THAT
// instant: the old row is closed as `expired` and the new create succeeds — never a false
// `override_active` for a row that is already dead. With `now()` (frozen at the waiter's BEGIN,
// before the expiry) the closure sees a live row and the create is refused.
func TestGateBOverrideExpiringDuringALockWaitReleasesItsSlotToTheWaitingCreate(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))

	shortLived := gateDBNow(t, st, ctx).Add(700 * time.Millisecond)
	a, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "A: short", shortLived, gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, a); err != nil || got.Status != domain.GateOverrideActive {
		t.Fatalf("A must be active at creation: %+v %v", got, err)
	}

	release := holdServiceRow(t, st, ctx, f.serviceID)
	defer release()
	type res struct {
		id  string
		err error
	}
	done := make(chan res, 1)
	longLived := gateDBNow(t, st, ctx).Add(time.Hour)
	go func() {
		id, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "B: waited behind the lock", longLived, gateActorToken)
		done <- res{id, err}
	}()
	waitForRowLockWaiter(t, st, ctx, "FOR UPDATE", 3*time.Second)
	sleepPastOnDBClock(t, st, ctx, shortLived, 200*time.Millisecond) // A expires while B is queued
	release()

	var r res
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the waiting create did not return")
	}
	if r.err != nil {
		t.Fatalf("B after the wait = %v, want success: A had expired before B could run, so its slot was free", r.err)
	}
	closedA, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, a)
	if err != nil || closedA.Status != domain.GateOverrideExpired || closedA.RevokedAt == nil || closedA.RevokedReason != domain.GateRevokedExpired ||
		closedA.RevokedByLabel != nil || closedA.RevokedViaToken != nil || closedA.RevokedByUserID != nil {
		t.Errorf("A after B's create: %+v %v, want closed as expired with attribution NULL", closedA, err)
	}
	if closedA.RevokedAt != nil && closedA.RevokedAt.Before(shortLived) {
		t.Errorf("A was closed at %s, before it expired at %s: the closure ran on a pre-expiry clock", closedA.RevokedAt, shortLived)
	}
	active, err := st.ActiveGateOverride(ctx, f.projectID, f.serviceID)
	if err != nil || active.ID != r.id || active.Status != domain.GateOverrideActive {
		t.Errorf("active after the wait = %+v %v, want B", active, err)
	}
	var open int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_overrides WHERE service_id = $1 AND revoked_at IS NULL`, f.serviceID).Scan(&open); err != nil || open != 1 {
		t.Errorf("open rows = %d %v, want exactly 1 (B)", open, err)
	}
}

// Reviewer [24] (b): a revoke that waits on the service row while its target EXPIRES must find
// the override `expired`, not `active`: ErrGateOverrideNotActive, the row untouched (no manual
// closure, no revoker triple). With `now()` frozen at the revoker's BEGIN the status would still
// read active and the row would be recorded as revoked by hand after it had already lapsed.
func TestGateBRevokeAfterALockWaitOnAnOverrideThatExpiredDuringTheWaitIsNotActive(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))

	shortLived := gateDBNow(t, st, ctx).Add(700 * time.Millisecond)
	a, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "A: short", shortLived, gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	audits := gateAuditCount(t, st, ctx)

	release := holdServiceRow(t, st, ctx, f.serviceID)
	defer release()
	done := make(chan error, 1)
	revoker := GateActor{ViaToken: true, Label: "token:late-revoker"}
	go func() { done <- st.RevokeGateOverride(ctx, f.projectID, f.serviceID, a, revoker) }()
	waitForRowLockWaiter(t, st, ctx, "FOR UPDATE", 3*time.Second)
	sleepPastOnDBClock(t, st, ctx, shortLived, 200*time.Millisecond) // A expires while the revoke is queued
	release()

	var rerr error
	select {
	case rerr = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the waiting revoke did not return")
	}
	if !errors.Is(rerr, ErrGateOverrideNotActive) {
		t.Fatalf("revoke after the wait = %v, want ErrGateOverrideNotActive (the override expired while the revoke was queued)", rerr)
	}
	got, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, a)
	if err != nil || got.Status != domain.GateOverrideExpired || got.RevokedReason == domain.GateRevokedManual ||
		got.RevokedByLabel != nil || got.RevokedViaToken != nil || got.RevokedByUserID != nil {
		t.Errorf("A after the refused revoke: %+v %v, want expired with no manual closure and no revoker triple", got, err)
	}
	if n := gateAuditCount(t, st, ctx); n != audits {
		t.Errorf("a refused revoke wrote an audit row (%d → %d)", audits, n)
	}
}

// §7 *Attribution*: an override created by an API token reads back — on the override AND on a
// decision that applied it — with actor_label `token:<name>`, after the token row is deleted
// too. The label is the evidence; the typed half (NULL + via_token) could never name the token.
func TestGateBOverrideAttributionByTokenSurvivesTokenDeletion(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateLatch(t, st, ctx, f, gatePageKey, true, 90*time.Second) // BLOCK, so an override applies
	gateLatch(t, st, ctx, f, gateTicketKey, false, 90*time.Second)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))

	var orgID string
	if err := st.pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, f.projectID).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateApiToken(ctx, domain.ApiToken{OrgID: orgID, ProjectID: f.projectID, Name: "release-bot", Role: domain.RoleProjectAdmin}, "hash-"+f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	actor := GateActor{ViaToken: true, Label: "token:" + tok.Name}
	ovID, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "release 4.2", gateDBNow(t, st, ctx).Add(time.Hour), actor)
	if err != nil {
		t.Fatal(err)
	}
	dec := gateDecide(t, st, ctx, f)
	if dec.State != domain.GateStateBlock || dec.Override == nil || dec.Override.ID != ovID {
		t.Fatalf("the override did not apply: state=%s override=%+v", dec.State, dec.Override)
	}

	if err := st.DeleteApiToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	var tokens int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE id = $1`, tok.ID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("token row still present: %d %v", tokens, err)
	}
	ov, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, ovID)
	if err != nil || ov.ActorLabel != "token:release-bot" || !ov.ViaToken || ov.ActorUserID != nil {
		t.Errorf("override after token deletion: label=%q via_token=%v user=%v err=%v", ov.ActorLabel, ov.ViaToken, ov.ActorUserID, err)
	}
	got, err := st.GetGateDecision(ctx, f.projectID, dec.DecisionID)
	if err != nil || got.Override == nil || got.Override.ActorLabel != "token:release-bot" {
		t.Errorf("decision after token deletion: override=%+v err=%v", got.Override, err)
	}
	hist, err := st.ListGateOverrides(ctx, f.projectID, f.serviceID)
	if err != nil || len(hist) != 1 || hist[0].ActorLabel != "token:release-bot" {
		t.Errorf("history after token deletion: %+v %v", hist, err)
	}
	var viaToken bool
	var userID *string
	if err := st.pool.QueryRow(ctx, `SELECT via_token, actor_user_id FROM audit_logs WHERE action = 'gate.override.create' ORDER BY created_at DESC LIMIT 1`).Scan(&viaToken, &userID); err != nil || !viaToken || userID != nil {
		t.Errorf("audit row typed half: via_token=%v user=%v err=%v", viaToken, userID, err)
	}
}
