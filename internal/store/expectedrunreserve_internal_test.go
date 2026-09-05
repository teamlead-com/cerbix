package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase F (§7.4, §17.14) — the publish boundary, at the statements that carry it.
//
// The defect these exist for was measured on a running instance: `AdvanceExpectations` deadlocked
// against §8.3's own fill, the scheduler discarded a payload naming jobs it had already published,
// and a later gap asserted that no run had happened at an instant one had. Every
// `expected_never_issued` fact that instance held was false.
//
// The store's half of the answer is ordering plus ownership: the window is written BEFORE the job
// is handed to a transport, `issued_at` is written by the CONFIRM and by nothing else, and a reserve
// that fails writes NOTHING — so the instant is either recorded or the schedule never passed it.

// Invariant 27a — a gap may never cover an instant a job was published for.
//
// The old order made that reachable: the record of the publish was lost, the schedule stayed put,
// and the next successful statement materialized the answered instant as a window nothing ran in.
// With the reserve first the row exists before the job leaves the process, and the gap's own
// `ON CONFLICT DO NOTHING` cannot overwrite it — but that is a property of the STATEMENT, so it is
// asserted rather than argued.
func TestAGapNeverCoversAnInstantAJobWasPublishedFor(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "gapreserve")
	m := ledgerMonitor(t, st, ctx, proj, "reserved-instant", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-5 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, due)

	// The window a job WAS published for.
	mustDispatch(t, st, ctx, due.Add(time.Second), ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: due.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	// Now put the expectation BEHIND that window and catch up from there. This is the state the
	// old order produced — a schedule that did not move while a job was published — and it is the
	// only shape in which a gap can reach an instant that already has a row: `first_expected` is
	// now two minutes before `due`, so the materialized series runs straight over it.
	//
	// Without this the case proves nothing: the gap normally starts one interval AFTER the standing
	// expectation, so it cannot touch the window just written whatever the statement does. The
	// first version of this test was exactly that — a test naming a mechanism it never reached —
	// and the mutation below survived it.
	behind := due.Add(-2 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, behind)
	mustDispatch(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: behind,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	answered, ok := readRow(t, st, ctx, m.ID, due)
	if !ok {
		t.Fatal("the window a job was published for disappeared")
	}
	if answered.JobID == nil || *answered.JobID != ledgerJobA {
		t.Fatalf("the published window carries %v, want the job that was published — a gap "+
			"overwrote the record of a real dispatch", answered.JobID)
	}
	if answered.ReservedAt == nil {
		t.Error("the published window lost its reservation")
	}
	if got := answered.asExpectedRun(m.ID).Verdict(); got == domain.VerdictExpectedNeverIssued {
		t.Fatalf("a window a job was published for reads %q — this is the exact false fact "+
			"phase F exists to make unreachable", got)
	}
	// And the windows the gap DID materialize carry no job, because nothing ran in them.
	gaps := 0
	for _, w := range readWindows(t, st, ctx, m.ID) {
		if w.DueAt.Equal(due) || w.DueAt.Equal(behind) {
			continue
		}
		gaps++
		if w.JobID != nil {
			t.Errorf("a materialized gap at %s claims job %v", w.DueAt, *w.JobID)
		}
	}
	if gaps == 0 {
		t.Fatal("no gap was materialized at all, so this case proves nothing about gaps")
	}
}

// Invariant 27c — the retry of a failed reserve re-sends the SAME payload, and applying it twice
// leaves one window, one instant and one job identity.
//
// The mutation this kills is re-minting between attempts: a second identity for an instant that may
// already have been dispatched is the stale-identity half of the defect, arriving from the other
// side.
func TestTheReserveRetryIsIdempotentUnderTheSamePayload(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "retryreserve")
	m := ledgerMonitor(t, st, ctx, proj, "retried", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-time.Minute)
	backdateSchedule(t, st, ctx, m.ID, due)
	payload := ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region, ReservedAt: now,
	}

	first, err := st.ReserveExpectations(ctx, now, []ExpectationAdvance{payload})
	if err != nil || len(first) != 1 {
		t.Fatalf("first reserve: reserved=%+v err=%v", first, err)
	}
	if first[0].MonitorID != m.ID || !first[0].DueAt.Equal(due) {
		t.Fatalf("the reserve named %+v, want the window it was given", first[0])
	}
	// The retry. Same payload, unchanged — which is what the scheduler holds and re-submits.
	second, err := st.ReserveExpectations(ctx, now.Add(time.Second), []ExpectationAdvance{payload})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("the retry reserved %+v; the fence had already moved past this payload, so a "+
			"second application must be refused rather than move the instant twice", second)
	}

	windows := readWindows(t, st, ctx, m.ID)
	if len(windows) != 1 {
		t.Fatalf("%d windows exist, want exactly 1 — the retry created a second one", len(windows))
	}
	if windows[0].JobID == nil || *windows[0].JobID != ledgerJobA {
		t.Errorf("the window carries %v, want the ONE identity the payload named", windows[0].JobID)
	}
	sched := readSchedule(t, st, ctx, m.ID)
	if !sched.NextDueAt.Equal(payload.NextDue) {
		t.Errorf("next_due_at is %s, want %s — the retry moved the expectation a second time",
			sched.NextDueAt, payload.NextDue)
	}
}

// Invariant 27h — §8.3's fill may not write `issued_at` on a reserved row.
//
// `issued_at` is the core's instant and the CONFIRM is its only writer. A terminal that arrives
// before the confirm — ordinary on the in-process transport, where the probe can finish before the
// tick's third statement runs — must fill the terminal facts and leave that instant alone. The
// mutation is restoring `COALESCE(expected_runs.issued_at, $6)`, which hands it to the executor's
// echo and makes a late window promotable to a covered one.
func TestTheFillCannotWriteIssuedAtOnAReservedRow(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "reservedfill")
	m := ledgerMonitor(t, st, ctx, proj, "early-terminal", 60)

	now := time.Now().UTC().Truncate(time.Second)
	// A window LATE by five minutes: if the executor's echo could set `issued_at`, it would name
	// the due instant and the verdict would move from covered_late to covered.
	due := now.Add(-5 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	reserved, ok := readRow(t, st, ctx, m.ID, due)
	if !ok || reserved.ReservedAt == nil || reserved.IssuedAt != nil {
		t.Fatalf("fixture is not a reserved-and-unconfirmed window: %+v (found=%v)", reserved, ok)
	}
	if got := reserved.asExpectedRun(m.ID).Verdict(); got != domain.VerdictReserved {
		t.Fatalf("the fixture reads %q, want %q", got, domain.VerdictReserved)
	}

	// The result arrives before the confirm, echoing an issue instant of its own choosing.
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: due, DueAt: due,
	}); err != nil {
		t.Fatalf("record result: %v", err)
	}

	after, _ := readRow(t, st, ctx, m.ID, due)
	if after.TerminalAt == nil {
		t.Fatal("the terminal did not land: an early terminal must still complete its window")
	}
	if after.IssuedAt != nil {
		t.Fatalf("issued_at is %s on a reserved row. The CONFIRM owns that instant; a fill that "+
			"writes it hands the core's own measurement to the executor's echo, which is how a "+
			"covered_late window is promoted to covered", after.IssuedAt)
	}
	if got := after.asExpectedRun(m.ID).Verdict(); got != domain.VerdictCovered {
		t.Fatalf("the answered window reads %q, want %q — a terminal outranks a missing issue "+
			"instant, and with none to measure against there is no lateness to claim", got, domain.VerdictCovered)
	}
}

// Invariant 27d — a FORCED lock cycle between the ledger's two writers leaves no false absence.
//
// The cycle is produced deterministically rather than waited for: two transactions take the same
// two windows in opposite order, so PostgreSQL's detector must kill one of them. What matters is
// what the survivor and the victim leave behind — the victim's whole statement is rolled back, so
// its monitor's expectation has NOT moved and no window claims that a run was missed.
func TestATwoTransactionLockCycleLeavesNoFalseNeverIssued(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "deadlock")
	first := ledgerMonitor(t, st, ctx, proj, "cycle-one", 60)
	second := ledgerMonitor(t, st, ctx, proj, "cycle-two", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-time.Minute)
	for _, m := range []domain.Monitor{first, second} {
		backdateSchedule(t, st, ctx, m.ID, due)
		mustDispatch(t, st, ctx, due.Add(time.Second), ExpectationAdvance{
			MonitorID: m.ID, JobID: ledgerJobA, NextDue: due.Add(time.Minute), IntervalInForce: 60,
			CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
			ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	}

	// Two transactions, each locking one window and then reaching for the other's.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	ready := make([]chan struct{}, 2)
	for i := range ready {
		ready[i] = make(chan struct{})
	}
	order := [2][2]string{{first.ID, second.ID}, {second.ID, first.ID}}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				close(ready[i])
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx,
				`UPDATE expected_runs SET claimed_at = $3 WHERE monitor_id = $1 AND due_at = $2`,
				order[i][0], due, now); err != nil {
				errs[i] = err
				close(ready[i])
				return
			}
			close(ready[i])
			// Both hold their first row before either reaches for the second: that is what makes
			// the cycle certain rather than likely.
			for j := range ready {
				<-ready[j]
			}
			if _, err := tx.Exec(ctx,
				`UPDATE expected_runs SET claimed_at = $3 WHERE monitor_id = $1 AND due_at = $2`,
				order[i][1], due, now); err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit(ctx)
		}(i)
	}
	wg.Wait()

	deadlocked := 0
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), "40P01") {
			deadlocked++
		}
	}
	if deadlocked == 0 {
		t.Fatal("no transaction was killed by the deadlock detector, so this case exercised " +
			"nothing: the cycle it exists to force did not happen")
	}

	// The point of the case. Whatever the detector chose, no window claims that a run was missed at
	// an instant a job was published for — the state the shipped code produced twice on a live
	// stack, and produced it because a failure there discarded the record instead of the work.
	for _, m := range []domain.Monitor{first, second} {
		row, ok := readRow(t, st, ctx, m.ID, due)
		if !ok {
			t.Fatalf("%s lost its window entirely", m.Name)
		}
		if got := row.asExpectedRun(m.ID).Verdict(); got == domain.VerdictExpectedNeverIssued {
			t.Errorf("%s reads %q at an instant its job was published for", m.Name, got)
		}
		if row.JobID == nil {
			t.Errorf("%s lost the identity of the job that answered it", m.Name)
		}
	}
}

// The withholding vocabulary is CLOSED, at both boundaries (final-tree audit, party [64]).
//
// It was described as closed in three places — the `WithheldPublishFailed` constant, `openapi.yaml`'s
// enum and the runbook — while migration 00103 checked only that a reason implies a reservation and
// `ConfirmExpectations` accepted any non-empty string. An internal caller could therefore persist a
// value the API contract does not define and no reader could interpret. Prose closing a vocabulary
// the schema leaves open is this arc's own defect class.
//
// Both halves are asserted because they answer different questions: the DATABASE refuses the value
// whatever wrote it, and the METHOD refuses it before any SQL runs and names the offending item
// rather than aborting a batch.
func TestTheWithholdingVocabularyIsClosedAtBothBoundaries(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "withheld")
	m := ledgerMonitor(t, st, ctx, proj, "vocabulary", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-time.Minute)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	t.Run("the database refuses an undefined value whatever writes it", func(t *testing.T) {
		_, err := st.pool.Exec(ctx,
			`UPDATE expected_runs SET withheld_reason = 'broker_was_grumpy'
			  WHERE monitor_id = $1 AND due_at = $2`, m.ID, due)
		if err == nil {
			t.Fatal("an undefined withheld reason was persisted: the vocabulary is closed in the " +
				"constant, in openapi.yaml and in the runbook, and a schema that admits any string " +
				"makes all three of those statements false")
		}
		if !strings.Contains(err.Error(), "expected_runs_withheld_needs_reservation") {
			t.Errorf("the refusal came from somewhere else: %v", err)
		}
	})

	t.Run("the method refuses it before any SQL runs, and names the item", func(t *testing.T) {
		n, err := st.ConfirmExpectations(ctx, now, []ExpectationConfirm{{
			MonitorID: m.ID, DueAt: due, JobID: ledgerJobA, WithheldReason: "broker_was_grumpy",
		}})
		if err == nil {
			t.Fatal("the method accepted an undefined withheld reason")
		}
		if n != 0 {
			t.Errorf("it reported %d confirms for a batch it refused", n)
		}
		if !strings.Contains(err.Error(), m.ID) {
			t.Errorf("the refusal does not name the monitor: %v", err)
		}
		// And nothing was written: a refused batch is refused whole.
		row, _ := readRow(t, st, ctx, m.ID, due)
		if row.WithheldReason != "" || row.IssuedAt != nil {
			t.Errorf("the refused confirm still wrote something: %+v", row)
		}
	})

	t.Run("the defined value passes both", func(t *testing.T) {
		n, err := st.ConfirmExpectations(ctx, now, []ExpectationConfirm{{
			MonitorID: m.ID, DueAt: due, JobID: ledgerJobA, WithheldReason: WithheldPublishFailed,
		}})
		if err != nil || n != 1 {
			t.Fatalf("the defined reason was refused: n=%d err=%v", n, err)
		}
		row, _ := readRow(t, st, ctx, m.ID, due)
		if row.WithheldReason != WithheldPublishFailed {
			t.Fatalf("withheld_reason is %q, want %q", row.WithheldReason, WithheldPublishFailed)
		}
		if row.IssuedAt != nil {
			t.Error("a withheld window was given an issue instant, which is the publish it did not get")
		}
		if got := row.asExpectedRun(m.ID).Verdict(); got != domain.VerdictReserved {
			t.Fatalf("a withheld window reads %q, want %q", got, domain.VerdictReserved)
		}
	})
}
