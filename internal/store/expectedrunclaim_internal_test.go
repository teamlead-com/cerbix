package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase C — the claim event (§8.4) and §8.1's merge rule.
//
// The claim is the fact that makes invariant 6 observable: a run CLAIMED and never finished,
// distinct from one issued and never claimed and from a window nothing was issued for. Everything
// here is asserted against `RecordRunClaim`, which is the statement production calls, rather than
// against a second copy of its SQL.

// Invariant 7 and 9 — a repeated event resolves to the EARLIEST observation, and a duplicate
// changes nothing. "Earliest wins" rather than "first writer wins" because the first CLAIM is the
// real one: a duplicate delivery arriving later must not redate it.
//
// The mutation that must kill this: `GREATEST` for `LEAST`, or plain assignment. Both make the
// value depend on arrival order, which is what §8.2 exists to prevent.
func TestARepeatedClaimResolvesToTheEarliestObservation(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "claimmerge")
	m := ledgerMonitor(t, st, ctx, proj, "claimed", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-30 * time.Second)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	earlier, later := due.Add(time.Second), due.Add(9*time.Second)
	// Both arrival orders, because the property is that order does not matter.
	for _, order := range []struct {
		name          string
		first, second time.Time
	}{
		{"later first", later, earlier},
		{"earlier first", earlier, later},
	} {
		t.Run(order.name, func(t *testing.T) {
			if _, err := st.pool.Exec(ctx,
				`UPDATE expected_runs SET claimed_at = NULL WHERE monitor_id = $1`, m.ID); err != nil {
				t.Fatalf("reset: %v", err)
			}
			for _, at := range []time.Time{order.first, order.second, order.second} {
				if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, due, ledgerJobA, at)); err != nil {
					t.Fatalf("record claim: %v", err)
				}
			}
			row, _ := readRow(t, st, ctx, m.ID, due)
			if row.ClaimedAt == nil || !row.ClaimedAt.Equal(earlier) {
				t.Fatalf("claimed_at is %v, want the EARLIEST observation %s", row.ClaimedAt, earlier)
			}
			// Invariant 7's other half: no event reads or writes another event's column.
			if row.TerminalAt != nil || row.RefusedAt != nil {
				t.Errorf("the claim touched a column that is not its own: %+v", row)
			}
		})
	}
}

// Invariants 5, 6 and 8, and the state §8.5 makes visible for the first time.
//
// Three windows, three readings: issued and never claimed, claimed and never finished, and
// finished. The AMQP ack policy has always accepted losing a check on a hard crash — *"the
// scheduler re-emits on the next interval"* — and what changes here is that the loss becomes a
// queryable fact rather than an assurance.
//
// Invariant 8 is asserted in the same case because it is the same mechanism read backwards: a
// terminal that OVERTAKES its claim loses nothing, since the claim fills its own column whenever it
// lands and there is no state machine for a reordering to violate.
func TestTheThreeRunStatesAreDistinguishableAndOrderIndependent(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "claimstates")
	m := ledgerMonitor(t, st, ctx, proj, "staged", 60)

	now := time.Now().UTC().Truncate(time.Second)
	first, second, third := now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-time.Minute)
	backdateSchedule(t, st, ctx, m.ID, first)
	for i, due := range []time.Time{first, second, third} {
		jobID := []string{ledgerJobA, ledgerJobB, ledgerJobC}[i]
		// Reserved AND confirmed: all three windows here are ones whose publish returned, which
		// is what makes "issued and never claimed" a state they can be in at all (§7.4).
		mustDispatch(t, st, ctx, due.Add(time.Second), ExpectationAdvance{
			MonitorID: m.ID, JobID: jobID, NextDue: due.Add(time.Minute), IntervalInForce: 60,
			CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
			ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	}
	// Window 1: issued, never claimed. Window 2: claimed, never finished. Window 3: the terminal
	// arrives BEFORE the claim.
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, second, ledgerJobB, second.Add(time.Second))); err != nil {
		t.Fatalf("claim second: %v", err)
	}
	inTx(t, st, ctx, func(tx pgx.Tx) error {
		return fillExpectedRunTerminalTx(ctx, tx, expectedRunTerminal{
			MonitorID: m.ID, DueAt: third, JobID: ledgerJobC, Revision: m.ExecutionRevision,
			TerminalAt: third.Add(2 * time.Second), IssuedAt: third.Add(time.Second),
			Outcome: ExpectedRunOutcomeResult})
	})
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, third, ledgerJobC, third.Add(time.Second))); err != nil {
		t.Fatalf("claim third after its terminal: %v", err)
	}

	for _, tc := range []struct {
		name string
		due  time.Time
		want domain.ExpectedRunVerdict
	}{
		{"issued and never claimed", first, domain.VerdictIssuedNeverClaimed},
		{"claimed and never finished", second, domain.VerdictClaimedNeverFinished},
		{"finished, its terminal ahead of its claim", third, domain.VerdictCovered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := readRow(t, st, ctx, m.ID, tc.due)
			if !ok {
				t.Fatalf("no window at %s", tc.due)
			}
			if got := row.asExpectedRun(m.ID).Verdict(); got != tc.want {
				t.Fatalf("reads %q, want %q", got, tc.want)
			}
		})
	}
	// The late claim filled its own column and changed nothing about the verdict — invariant 8.
	third3, _ := readRow(t, st, ctx, m.ID, third)
	if third3.ClaimedAt == nil {
		t.Error("a claim arriving after its own terminal recorded nothing; it fills its own column " +
			"whenever it lands, and there is no state machine for it to violate")
	}
	// And the three readings are pairwise DISTINCT, asserted as a set: "none of them is covered"
	// would pass with all three collapsed into one verdict.
	a, _ := readRow(t, st, ctx, m.ID, first)
	b, _ := readRow(t, st, ctx, m.ID, second)
	if a.asExpectedRun(m.ID).Verdict() == b.asExpectedRun(m.ID).Verdict() {
		t.Fatal("issued-never-claimed and claimed-never-finished are not distinguishable")
	}
}

// Invariant 7b — §8.1's two negatives, which lived in the spec's prose and in no test.
//
// A claim whose `execution_revision` does not match the row changes nothing; and a claim naming the
// `due_at` of a window that carries a `skip_reason` changes nothing, however late it arrives. The
// second is the one with teeth: it is what stops a stale or forged claim from marking a window
// cerbix CHOSE not to run as claimed, which is the defect the shared predicate exists to prevent.
func TestAClaimIsRefusedByTheRevisionAndByADeliberateSkip(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "claimguard")
	m := ledgerMonitor(t, st, ctx, proj, "guarded", 60)

	now := time.Now().UTC().Truncate(time.Second)
	standing := now.Add(-3 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, standing)
	// A skip advances past two windows: the standing one carries the reason, the ones after it
	// carry nothing at all.
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, SkipReason: SkipNoCapableRunner, NextDue: now.Add(time.Minute),
		IntervalInForce: 60, ExpectedDue: standing, ExpectedRevision: m.ExecutionRevision,
		Region: m.Region})

	// A claim for the deliberately skipped window, however it is addressed.
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, standing, ledgerJobA, now)); err != nil {
		t.Fatalf("record claim: %v", err)
	}
	skipped, _ := readRow(t, st, ctx, m.ID, standing)
	if skipped.ClaimedAt != nil {
		t.Fatalf("a window carrying skip_reason %v was CLAIMED: a run cerbix chose not to make is "+
			"now reported as one that started", skipped.SkipReason)
	}

	// A stale-revision claim on a window that would otherwise admit it.
	neverIssued := standing.Add(time.Minute)
	if row, ok := readRow(t, st, ctx, m.ID, neverIssued); !ok || row.SkipReason != nil {
		t.Fatalf("fixture: %s is not a never-issued window: %+v", neverIssued, row)
	}
	stale := claimHeartbeat(m, neverIssued, ledgerJobA, now)
	stale.ExecutionRevision = m.ExecutionRevision + 5
	if _, err := st.RecordRunClaim(ctx, stale); err != nil {
		t.Fatalf("record stale claim: %v", err)
	}
	if row, _ := readRow(t, st, ctx, m.ID, neverIssued); row.ClaimedAt != nil {
		t.Fatalf("a claim from generation %d marked a window recording generation %d",
			stale.ExecutionRevision, row.Revision)
	}

	// The CONVERSE, without which the two exclusions prove nothing: at the matching generation the
	// same never-issued window accepts it. This is the narrow adoption §8.3 permits — a window the
	// gap logic recorded, for which an executor now reports a run really did start.
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, neverIssued, ledgerJobA, now)); err != nil {
		t.Fatalf("record admissible claim: %v", err)
	}
	if row, _ := readRow(t, st, ctx, m.ID, neverIssued); row.ClaimedAt == nil {
		t.Fatal("an admissible claim on a never-issued window recorded nothing, so the exclusions " +
			"above could be satisfied by a statement that never works")
	}
}

// A claim correlating to no window records NOTHING and is not an error.
//
// Zero rows affected is the normal outcome for the crash-after-publish case: the ledger never
// entered the run, the terminal will create the row later and adopt it, and a claim that INSERTED
// would invent an issued run whose only evidence is that somebody started it — and would occupy the
// window's primary key against the legitimate materialization.
func TestAClaimForAWindowTheLedgerNeverEnteredRecordsNothing(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "claimorphan")
	m := ledgerMonitor(t, st, ctx, proj, "orphan-claim", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-30 * time.Second)
	before := len(readWindows(t, st, ctx, m.ID))
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, due, ledgerJobA, due)); err != nil {
		t.Fatalf("record claim: %v", err)
	}
	if got := len(readWindows(t, st, ctx, m.ID)); got != before {
		t.Fatalf("a claim INSERTED %d window(s): a claim proves an executor started, not that a "+
			"run produced an admissible outcome", got-before)
	}

	// And the terminal that follows still creates the row and reads `covered` — the claim's
	// absence costs a diagnostic, never a verdict (invariant 10).
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: due,
	}); err != nil {
		t.Fatalf("record result: %v", err)
	}
	row, ok := readRow(t, st, ctx, m.ID, due)
	if !ok {
		t.Fatal("the terminal created no row")
	}
	if got := row.asExpectedRun(m.ID).Verdict(); got != domain.VerdictCovered {
		t.Fatalf("the adopted window reads %q, want %q", got, domain.VerdictCovered)
	}
}

// Invariant 25, for the claim path: a claim carrying no usable identity correlates to no window and
// is refused BY NAME where the caller could act on it, silently where it could not.
func TestABadlyIdentifiedClaimCorrelatesToNoWindow(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "claimident")
	m := ledgerMonitor(t, st, ctx, proj, "identified", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-30 * time.Second)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	// Shapes the caller COULD have got right are errors it must see.
	for _, tc := range []struct {
		name   string
		mutate func(hb *domain.Heartbeat)
	}{
		{"no claim member at all", func(hb *domain.Heartbeat) { hb.Claim = nil }},
		{"a claim with no instant", func(hb *domain.Heartbeat) { hb.Claim = &domain.RunClaim{} }},
		{"no monitor", func(hb *domain.Heartbeat) { hb.MonitorID = "" }},
		{"no revision", func(hb *domain.Heartbeat) { hb.ExecutionRevision = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hb := claimHeartbeat(m, due, ledgerJobA, now)
			tc.mutate(&hb)
			if _, err := st.RecordRunClaim(ctx, hb); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
	// Shapes that are ordinary rolling-upgrade facts refuse CORRELATION and nothing else.
	for _, tc := range []struct {
		name   string
		mutate func(hb *domain.Heartbeat)
	}{
		{"a job id that is not a uuid", func(hb *domain.Heartbeat) { hb.JobID = "not-a-uuid" }},
		{"no window", func(hb *domain.Heartbeat) { hb.DueAt = time.Time{} }},
		{"a window outside retention", func(hb *domain.Heartbeat) { hb.DueAt = now.Add(-100 * 24 * time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hb := claimHeartbeat(m, due, ledgerJobA, now)
			tc.mutate(&hb)
			if _, err := st.RecordRunClaim(ctx, hb); err != nil {
				t.Fatalf("%s was an error; it is the ordinary case for an executor older than the "+
					"ledger carrier: %v", tc.name, err)
			}
			if row, _ := readRow(t, st, ctx, m.ID, due); row.ClaimedAt != nil {
				t.Fatalf("%s still marked the window claimed", tc.name)
			}
		})
	}
}

// claimHeartbeat is the message an executor sends, assembled through the ONE owner so a test
// cannot pass against a shape production never produces.
func claimHeartbeat(m domain.Monitor, due time.Time, jobID string, at time.Time) domain.Heartbeat {
	return domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision,
		JobID: jobID, JobIssuedAt: due, DueAt: due,
		Claim: &domain.RunClaim{At: at},
	}
}

// Invariant 18 — a window written as UNFINISHED is never deleted when its terminal finally
// arrives; `terminal_at` is filled and the row reads as completed late.
//
// Verdicts are computed and never stored (invariant 19), so one more non-NULL column changes the
// reading with nothing to keep consistent. That is why the design can afford to write a window
// before it knows how it ends — and it is what makes "delete and re-insert" the wrong shape here:
// the row's identity is the WINDOW, and a re-insert would lose the claim instant that says the run
// started at all.
//
// The mutation that must kill this: make the terminal DELETE the row and insert a fresh one. Every
// coverage assertion still passes — the window ends up `covered` either way — and only the claim
// instant's survival catches it.
func TestALateTerminalCompletesAnUnfinishedWindowRatherThanReplacingIt(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "latefinish")
	m := ledgerMonitor(t, st, ctx, proj, "unfinished", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-5 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, due.Add(time.Second), ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: due.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	// The publish returned, so the window is an ISSUED one (§7.4). Without this it is `reserved`,
	// and the lateness this test is about is measured from an instant that would not exist.
	mustConfirm(t, st, ctx, due.Add(time.Second), ExpectationConfirm{
		MonitorID: m.ID, DueAt: due, JobID: ledgerJobA})
	claimedAt := due.Add(2 * time.Second)
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, due, ledgerJobA, claimedAt)); err != nil {
		t.Fatalf("record claim: %v", err)
	}
	unfinished, _ := readRow(t, st, ctx, m.ID, due)
	if got := unfinished.asExpectedRun(m.ID).Verdict(); got != domain.VerdictClaimedNeverFinished {
		t.Fatalf("the fixture reads %q, want %q", got, domain.VerdictClaimedNeverFinished)
	}

	// The run finishes four and a half minutes after the window was due. Its ISSUE was on time, and
	// that distinction is the point of the assertions below.
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: due.Add(time.Second), DueAt: due,
	}); err != nil {
		t.Fatalf("late terminal: %v", err)
	}

	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM expected_runs WHERE monitor_id = $1 AND due_at = $2`, m.ID, due).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows exist for the window, want exactly 1: a late terminal completes a window, "+
			"it does not replace it", rows)
	}
	completed, _ := readRow(t, st, ctx, m.ID, due)
	if completed.TerminalAt == nil {
		t.Fatal("the late terminal did not fill terminal_at")
	}
	if completed.ClaimedAt == nil || !completed.ClaimedAt.Equal(claimedAt) {
		t.Fatalf("claimed_at is %v, want the %s the executor reported — the row was replaced rather "+
			"than completed, and the evidence that the run started is gone", completed.ClaimedAt, claimedAt)
	}
	if completed.IssuedAt == nil {
		t.Error("issued_at was lost")
	}
	// It reads `covered`, and the reason is worth stating because §8.6's phrase "completed late"
	// and §14.1's verdict `covered_late` are DIFFERENT things — I wrote this assertion the wrong
	// way round first, and the code was right.
	//
	// §8.6 is about the ROW: a window written as unfinished is not deleted when its terminal
	// finally arrives, and it then reads as a completed window rather than an abandoned one.
	// §14.1's `covered_late` is about the WINDOW's evidence: `issued_at - due_at` above the
	// interval that spaced it, which is the case where the observation is too far from the window
	// to prove it. This run was ISSUED a second after its window was due and merely took a long
	// time — the observation answers the window it was meant to.
	//
	// Conflating them would have been an over-claim in the withholding direction, which is the
	// safe one and still wrong: it would have made every slow probe forfeit its stroke.
	if got := completed.asExpectedRun(m.ID).Verdict(); got != domain.VerdictCovered {
		t.Fatalf("the completed window reads %q, want %q: it was ISSUED on time and simply took a "+
			"long time, and lateness is measured from the ISSUE (§14.1)", got, domain.VerdictCovered)
	}

	// The converse, so the distinction is pinned rather than asserted: a window whose run was
	// ISSUED later than the interval that spaced it does read `covered_late`, and its claim
	// survives the same way.
	lateDue := now.Add(-20 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, lateDue)
	mustDispatch(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: lateDue,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, lateDue, ledgerJobB, now)); err != nil {
		t.Fatalf("claim the late window: %v", err)
	}
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobB, JobIssuedAt: now, DueAt: lateDue,
	}); err != nil {
		t.Fatalf("terminal for the late window: %v", err)
	}
	late, _ := readRow(t, st, ctx, m.ID, lateDue)
	if got := late.asExpectedRun(m.ID).Verdict(); got != domain.VerdictCoveredLate {
		t.Fatalf("a window ISSUED nineteen minutes after it was due reads %q, want %q", got, domain.VerdictCoveredLate)
	}
	if late.asExpectedRun(m.ID).LicensesStroke() {
		t.Error("a covered_late window licenses a stroke")
	}
	if late.ClaimedAt == nil {
		t.Error("the late window lost its claim")
	}
}
