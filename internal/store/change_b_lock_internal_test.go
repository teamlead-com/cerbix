package store

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D4 under real contention (func-change-intelligence §7 *Phases*, invariant 4; iter-0165
// task 2, Agent B). Agent A proved two racers and a planted holder; these tests put N racers
// with DIFFERENT phase scripts on one identity, observe the lock in pg_locks while a write is
// parked mid-transaction, and check that the key does not depend on how the caller spells the
// service id.

// N goroutines, each replaying its own phase script for ONE identity in a shuffled order: the
// survivors are at most one `started` and exactly one terminal (every script ends in one), a
// terminal never predates the started, every refusal is `phase_order` by name, and every
// surviving row has exactly one non-replay winner.
func TestChangeIdentityLockUnderContentionLeavesAtMostOneStartedAndOneTerminalInOrder(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	startedAt, terminalAt := now.Add(-10*time.Minute), now.Add(-9*time.Minute)
	const (
		started   = domain.ChangePhaseStarted
		succeeded = domain.ChangePhaseSucceeded
		failed    = domain.ChangePhaseFailed
		cancelled = domain.ChangePhaseCancelled
	)
	scripts := [][]domain.ChangePhase{
		{started, succeeded}, {failed}, {started, cancelled}, {succeeded}, {cancelled, started},
		{started}, {failed, succeeded}, {started, failed}, {cancelled}, {succeeded, started},
		{started, started}, {failed, failed},
	}
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)

	type outcome struct {
		phase    domain.ChangePhase
		replayed bool
		err      error
	}
	for storm := 0; storm < 6; storm++ {
		ext := fmt.Sprintf("storm-%d", storm)
		results := make(chan outcome, 4*len(scripts))
		var wg sync.WaitGroup
		for _, i := range rng.Perm(len(scripts)) {
			wg.Add(1)
			go func(script []domain.ChangePhase) {
				defer wg.Done()
				for _, p := range script {
					at := terminalAt
					if p == started {
						at = startedAt
					}
					_, replayed, err := st.RecordChangePhase(ctx, changeInput(f, ext, p, at))
					results <- outcome{p, replayed, err}
				}
			}(scripts[i])
		}
		wg.Wait()
		close(results)

		startedWins, terminalWins := 0, 0
		for r := range results {
			switch {
			case r.err == nil && r.replayed:
				// An identical replay is 200 with the original: nothing to count.
			case r.err == nil && r.phase == started:
				startedWins++
			case r.err == nil:
				terminalWins++
			default:
				var ce *domain.ChangeError
				if !errors.As(r.err, &ce) || ce.Code != domain.ChangeErrPhaseOrder {
					t.Fatalf("%s: %s refused with %v; the only refusal the lock allows is phase_order", ext, r.phase, r.err)
				}
			}
		}
		rows, err := changeIdentityRowsTx(ctx, st.pool, f.projectID, f.serviceID, "github-actions", ext)
		if err != nil {
			t.Fatal(err)
		}
		var nStarted, nTerminal int
		var startedRow, terminalRow *domain.ChangePhaseRow
		for i := range rows {
			if rows[i].Phase == started {
				nStarted++
				startedRow = &rows[i]
			} else {
				nTerminal++
				terminalRow = &rows[i]
			}
		}
		if nStarted > 1 || nTerminal != 1 {
			t.Fatalf("%s: %d started and %d terminal rows survive; want at most one and exactly one", ext, nStarted, nTerminal)
		}
		if nStarted != startedWins || nTerminal != terminalWins {
			t.Fatalf("%s: %d/%d rows but %d/%d non-replay winners (started/terminal): a write was reported twice or lost", ext, nStarted, nTerminal, startedWins, terminalWins)
		}
		if startedRow != nil && terminalRow.OccurredAt.Before(startedRow.OccurredAt) {
			t.Fatalf("%s: terminal at %s predates started at %s", ext, terminalRow.OccurredAt, startedRow.OccurredAt)
		}
	}
}

// The advisory lock is held for the WHOLE write transaction: with the store's INSERT parked on
// an uncommitted conflicting row, a competing pg_try_advisory_xact_lock on the key answers false
// and pg_locks shows the one granted lock; when the planted row commits, the store's INSERT hits
// the unique key and answers `phase_exists` (the honest branch); the lock leaves with the
// transaction. A rolled-back plant lets the parked write win.
func TestChangeIdentityLockIsHeldUntilTheWriteTransactionEnds(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	plant, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer plant.Rollback(ctx) //nolint:errcheck // no-op after the explicit commit below
	planted := plantUncommittedPhase(t, plant, ctx, f.projectID, f.serviceID, "github-actions", "held", "succeeded", now.Add(-9*time.Minute))

	done := make(chan error, 1)
	go func() {
		_, _, err := st.RecordChangePhase(ctx, changeInput(f, "held", domain.ChangePhaseSucceeded, now.Add(-9*time.Minute)))
		done <- err
	}()
	blocked := waitForBlockedBackends(t, st, ctx, 1)
	if !strings.Contains(blocked[0].query, "INSERT INTO service_changes") || blocked[0].waitEvent != "transactionid" {
		t.Fatalf("the writer is parked at %+v, want its INSERT waiting on the planted transaction", blocked[0])
	}
	if advisoryLockFree(t, st, ctx, f.serviceID, "github-actions", "held") {
		t.Fatal("pg_try_advisory_xact_lock succeeded from another connection while the write was mid-flight: the lock is not held for the transaction")
	}
	if n := advisoryLocksGranted(t, st, ctx, f.serviceID, "github-actions", "held"); n != 1 {
		t.Fatalf("pg_locks shows %d granted advisory lock(s) on the key, want 1", n)
	}
	if _, ok := completedWithin(done, 200*time.Millisecond); ok {
		t.Fatal("the write returned while its INSERT should be parked")
	}
	// An unrelated identity is not queued behind the parked one (D4: per identity).
	if _, _, err := st.RecordChangePhase(ctx, changeInput(f, "elsewhere", domain.ChangePhaseSucceeded, now.Add(-9*time.Minute))); err != nil {
		t.Fatalf("unrelated identity: %v", err)
	}

	if err := plant.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err, ok := completedWithin(done, 10*time.Second)
	if !ok {
		t.Fatal("the write never returned after the planted row committed")
	}
	var ce *domain.ChangeError
	if !errors.As(err, &ce) || ce.Code != domain.ChangeErrPhaseExists || ce.Field != "phase" {
		t.Fatalf("after the plant committed: %v, want phase_exists on phase (the unique-key branch)", err)
	}
	if !advisoryLockFree(t, st, ctx, f.serviceID, "github-actions", "held") || advisoryLocksGranted(t, st, ctx, f.serviceID, "github-actions", "held") != 0 {
		t.Fatal("the advisory lock outlived the write transaction")
	}
	rows, err := changeIdentityRowsTx(ctx, st.pool, f.projectID, f.serviceID, "github-actions", "held")
	if err != nil || len(rows) != 1 || rows[0].ID != planted {
		t.Fatalf("rows = %+v (%v), want exactly the planted row", rows, err)
	}

	// The rollback path: the parked write proceeds and wins.
	plant2, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer plant2.Rollback(ctx) //nolint:errcheck
	plantUncommittedPhase(t, plant2, ctx, f.projectID, f.serviceID, "github-actions", "held-2", "failed", now.Add(-9*time.Minute))
	done2 := make(chan error, 1)
	go func() {
		_, _, err := st.RecordChangePhase(ctx, changeInput(f, "held-2", domain.ChangePhaseFailed, now.Add(-9*time.Minute)))
		done2 <- err
	}()
	waitForBlockedBackends(t, st, ctx, 1)
	if err := plant2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err, ok := completedWithin(done2, 10*time.Second); !ok || err != nil {
		t.Fatalf("after the plant rolled back: ok=%v err=%v, want the write to succeed", ok, err)
	}
	rows, err = changeIdentityRowsTx(ctx, st.pool, f.projectID, f.serviceID, "github-actions", "held-2")
	if err != nil || len(rows) != 1 || rows[0].ActorLabel != "token:ci" {
		t.Fatalf("held-2 rows = %+v (%v), want the store's own row", rows, err)
	}
}

// The lock KEY is the identity, not the caller's spelling of it: a writer that names the
// service by an upper-case uuid must queue behind one that names it lower-case. Both writers
// are parked at their INSERT by planted rows for both terminals; if the second took a different
// key it would reach its INSERT beside the first, and the rollback of the plants would let TWO
// terminals land.
func TestChangeIdentityLockKeyDoesNotDependOnTheCaseOfTheServiceID(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	mustRecord(t, st, ctx, changeInput(f, "case", domain.ChangePhaseStarted, now.Add(-10*time.Minute)))

	plant, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer plant.Rollback(ctx) //nolint:errcheck
	plantUncommittedPhase(t, plant, ctx, f.projectID, f.serviceID, "github-actions", "case", "succeeded", now.Add(-9*time.Minute))
	plantUncommittedPhase(t, plant, ctx, f.projectID, f.serviceID, "github-actions", "case", "failed", now.Add(-9*time.Minute))

	lower := make(chan error, 1)
	go func() {
		_, _, err := st.RecordChangePhase(ctx, changeInput(f, "case", domain.ChangePhaseSucceeded, now.Add(-9*time.Minute)))
		lower <- err
	}()
	waitForBlockedBackends(t, st, ctx, 1)

	upper := make(chan error, 1)
	go func() {
		in := changeInput(f, "case", domain.ChangePhaseFailed, now.Add(-9*time.Minute))
		in.ServiceID = strings.ToUpper(f.serviceID)
		_, _, err := st.RecordChangePhase(ctx, in)
		upper <- err
	}()
	blocked := waitForBlockedBackends(t, st, ctx, 2)
	atInsert, atLock := 0, 0
	for _, b := range blocked {
		switch {
		case strings.Contains(b.query, "INSERT INTO service_changes"):
			atInsert++
		case b.waitEvent == "advisory":
			atLock++
		}
	}
	if atInsert != 1 || atLock != 1 {
		t.Fatalf("parked backends = %+v: want one at its INSERT and one on the advisory lock; two at their INSERT means the upper-case spelling took another key", blocked)
	}

	if err := plant.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	errLower, ok := completedWithin(lower, 10*time.Second)
	if !ok {
		t.Fatal("the lower-case writer never returned")
	}
	errUpper, ok := completedWithin(upper, 10*time.Second)
	if !ok {
		t.Fatal("the upper-case writer never returned")
	}
	var ce *domain.ChangeError
	if errLower != nil || !errors.As(errUpper, &ce) || ce.Code != domain.ChangeErrPhaseOrder {
		t.Fatalf("lower=%v upper=%v; want the first to win and the second refused phase_order", errLower, errUpper)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE service_id = $1 AND external_id = 'case' AND phase <> 'started'`, f.serviceID); n != 1 {
		t.Fatalf("%d terminal rows for one identity", n)
	}
}
