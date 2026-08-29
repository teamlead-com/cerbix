package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Review [19] on 8cb12d6: CreateGateOverride judged expires_at against the transaction's now(),
// which PostgreSQL freezes at BEGIN — so a caller that waited on the service row lock could commit
// an override that had already expired. Every gate write now reads statement_timestamp() AFTER the
// lock. The blocker below holds the service row for longer than the override's remaining life;
// the create must be refused, because at the instant it can finally run the expiry is in the past.
func TestGateOverrideExpiryIsJudgedAfterTheServiceLockWait(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 3*time.Minute, 59_990_000, 10_000)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))

	blocker, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err := blocker.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM services WHERE id = $1 FOR UPDATE`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	// Valid at the moment of the call: 700 ms of life on the database clock.
	expires := gateDBNow(t, st, ctx).Add(700 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "hotfix behind a lock", expires, gateActorToken)
		done <- err
	}()
	time.Sleep(1300 * time.Millisecond) // the waiter queues behind the lock past the expiry
	if _, err := blocker.Exec(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var ve *GateValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("create after the lock wait: err = %v, want a validation error on expires_at (the override had expired while queued)", err)
		}
		if ve.Field != "expires_at" {
			t.Fatalf("validation field = %q, want expires_at", ve.Field)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("create did not return after the lock was released")
	}
}

// The instant a gate write records is the instant it RAN, not the instant its transaction began:
// created_at of an override written behind a lock is at or after the moment the lock was released.
// With now() (frozen at BEGIN) it would predate the release by the whole wait.
func TestGateOverrideCreatedAtIsThePostLockInstant(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 3*time.Minute, 59_990_000, 10_000)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))

	blocker, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err := blocker.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM services WHERE id = $1 FOR UPDATE`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	type res struct {
		id  string
		err error
	}
	done := make(chan res, 1)
	go func() {
		id, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "hotfix behind a lock", gateDBNow(t, st, ctx).Add(time.Hour), gateActorToken)
		done <- res{id, err}
	}()
	time.Sleep(800 * time.Millisecond)
	var released time.Time
	if err := blocker.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("create: %v", r.err)
	}
	var createdAt time.Time
	if err := st.pool.QueryRow(ctx, `SELECT created_at FROM service_gate_overrides WHERE id = $1`, r.id).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	if createdAt.Before(released) {
		t.Fatalf("created_at %s is before the lock release %s: the row carries the transaction's BEGIN time, not the instant it was written",
			createdAt.Format(time.RFC3339Nano), released.Format(time.RFC3339Nano))
	}
}

// Review [20] on 8cb12d6: evaluate_tx_budget_ms is begin-through-commit (§5a), yet BeginTx ran on
// the caller's unbounded context and the deadline wrapper only existed after Begin succeeded — an
// exhausted pool could hold a decision past the budget before any bound applied. With every pool
// connection held, DecideGate must return a budget error within the budget plus a small net.
func TestGateDecisionBudgetBoundsPoolAcquisition(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 3*time.Minute, 59_990_000, 10_000)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))

	n := int(st.PoolMaxConns())
	held := make([]interface{ Release() }, 0, n)
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := st.pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d/%d: %v", i+1, n, err)
		}
		held = append(held, c)
	}
	const budget = 400 * time.Millisecond
	type res struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan res, 1)
	go func() {
		start := time.Now()
		_, err := st.DecideGate(ctx, f.projectID, f.serviceID, budget)
		done <- res{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, ErrGateBudgetExceeded) {
			t.Fatalf("DecideGate with the pool exhausted: err = %v, want ErrGateBudgetExceeded", r.err)
		}
		if r.elapsed > budget+1500*time.Millisecond {
			t.Fatalf("DecideGate took %s against a %s budget: BEGIN was not bounded by it", r.elapsed, budget)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DecideGate did not return: the budget did not bound pool acquisition")
	}
}

// Review [21] on 8cb12d6: policy audit rows carried before/after but not the actor's immutable
// label, so a token-made policy change read as "some token" in audit_logs (its typed half is
// NULL + via_token). Invariant 11: audited with before/after AND actor.
func TestGatePolicyAuditNamesTheActorLabelBesideBeforeAndAfter(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 3*time.Minute, 59_990_000, 10_000)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	var target string
	if err := st.pool.QueryRow(ctx,
		`SELECT target FROM audit_logs WHERE action = 'gate.policy.write' ORDER BY created_at DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"actor=" + gateActorToken.Label, "before=", "after=", "service=" + f.serviceID} {
		if !strings.Contains(target, want) {
			t.Fatalf("policy write audit target %q lacks %q", target, want)
		}
	}
	if err := st.DeleteGatePolicy(ctx, f.projectID, f.serviceID, rev, GateActor{ViaToken: true, Label: "token:release-bot"}); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT target FROM audit_logs WHERE action = 'gate.policy.delete' ORDER BY created_at DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(target, "actor=token:release-bot") || !strings.Contains(target, "after=none") {
		t.Fatalf("policy delete audit target %q lacks the revoker label or after=none", target)
	}
}
