package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase B2 — §8.3's terminal upsert and §8.3's refusal statement.
//
// The two are asymmetric on purpose and the asymmetry is the subject of half these tests: a
// terminal proves both that the run happened and that it produced an admissible outcome, so it may
// create its own row; a refusal proves only that something was DELIVERED, so it may only annotate
// the row recording its own job.

type ledgerRow struct {
	DueAt           time.Time
	JobID           *string
	Carrier         *int
	Interval        int
	IntervalAssumed bool
	IssuedAt        *time.Time
	ClaimedAt       *time.Time
	TerminalAt      *time.Time
	Outcome         *string
	RefusedAt       *time.Time
	RefusedReason   *string
	SkipReason      *string
	Revision        int64
	// Phase F (§7.4). The row reader carries them because a test that cannot SEE the reserved
	// state cannot tell it from an issue that vanished — which is the whole distinction.
	ReservedAt     *time.Time
	WithheldReason string
}

func readRow(t *testing.T, st *Store, ctx context.Context, monitorID string, dueAt time.Time) (ledgerRow, bool) {
	t.Helper()
	var r ledgerRow
	err := st.pool.QueryRow(ctx,
		`SELECT due_at, job_id::text, carrier_generation, interval_seconds, interval_assumed,
		        issued_at, claimed_at, terminal_at, outcome, refused_at, refused_reason,
		        skip_reason, execution_revision, reserved_at, withheld_reason
		   FROM expected_runs WHERE monitor_id = $1 AND due_at = $2`, monitorID, dueAt).
		Scan(&r.DueAt, &r.JobID, &r.Carrier, &r.Interval, &r.IntervalAssumed, &r.IssuedAt,
			&r.ClaimedAt, &r.TerminalAt, &r.Outcome, &r.RefusedAt, &r.RefusedReason,
			&r.SkipReason, &r.Revision, &r.ReservedAt, &r.WithheldReason)
	if noRows(err) {
		return ledgerRow{}, false
	}
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	return r, true
}

func (r ledgerRow) asExpectedRun(monitorID string) domain.ExpectedRun {
	out := domain.ExpectedRun{
		MonitorID: monitorID, DueAt: r.DueAt, IntervalSeconds: r.Interval,
		IntervalAssumed: r.IntervalAssumed, IssuedAt: r.IssuedAt, ClaimedAt: r.ClaimedAt,
		TerminalAt: r.TerminalAt, ExecutionRevision: r.Revision,
		ReservedAt: r.ReservedAt, WithheldReason: r.WithheldReason,
	}
	if r.JobID != nil {
		out.JobID = *r.JobID
	}
	if r.Carrier != nil {
		out.CarrierGeneration = *r.Carrier
	}
	if r.SkipReason != nil {
		out.SkipReason = *r.SkipReason
	}
	return out
}

// inTx runs one tx-scoped ledger statement and commits, so a test exercises the SAME function
// production calls rather than a second copy of its SQL.
func inTx(t *testing.T, st *Store, ctx context.Context, fn func(tx pgx.Tx) error) {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("statement: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// An answered window becomes COVERED, and the coverage test is the single condition
// `terminal_at IS NOT NULL` rather than a compound one a later reader could get wrong.
func TestATerminalOutcomeCoversTheWindowItAnswers(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "terminal")
	m := ledgerMonitor(t, st, ctx, proj, "answered", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-30 * time.Second)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustDispatch(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	before, ok := readRow(t, st, ctx, m.ID, due)
	if !ok {
		t.Fatal("the answered window has no row")
	}
	if v := before.asExpectedRun(m.ID).Verdict(); v != domain.VerdictIssuedNeverClaimed {
		t.Fatalf("an issued, unanswered window reads %q, want %q — this is the loss the AMQP ack "+
			"policy has always accepted and never made visible", v, domain.VerdictIssuedNeverClaimed)
	}

	// The result arrives through the REAL ingest path, so the terminal is proven to run behind the
	// same gate as the heartbeat rather than beside it.
	outcome, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: due,
	})
	if err != nil || !outcome.Applied {
		t.Fatalf("record result: outcome=%+v err=%v", outcome, err)
	}
	after, _ := readRow(t, st, ctx, m.ID, due)
	if after.TerminalAt == nil {
		t.Fatal("the result did not fill terminal_at, so the window it answered is not covered")
	}
	if after.Outcome == nil || *after.Outcome != ExpectedRunOutcomeResult {
		t.Errorf("outcome is %v, want %q", after.Outcome, ExpectedRunOutcomeResult)
	}
	if after.IntervalAssumed {
		t.Error("a window the core itself recorded is marked interval_assumed")
	}
	if v := after.asExpectedRun(m.ID).Verdict(); v != domain.VerdictCovered {
		t.Errorf("the answered window reads %q, want %q", v, domain.VerdictCovered)
	}
}

// The ORPHAN case (§7's crash-after-publish) and its threshold, from party [260].
//
// A monitor whose confirm interval is 10s and base interval 60s, a window spaced at the confirm
// interval, and a terminal 40s late. The row must be CREATED with `interval_assumed = true`,
// `interval_seconds = 10`, and it must read `covered_late`.
//
// Two mutations must fail it: using the BASE interval (60) for the threshold, which would read
// `covered`; and taking the value from the RESULT at all, which is what revisions 16–18 specified
// and what the reviewer's question at [258] killed — a residual pointing at over-claiming is not
// excused by being small.
func TestAnOrphanTerminalCreatesItsOwnRowWithTheConservativeThreshold(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "orphan")
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj, Name: "orphaned", Type: domain.MonitorHTTP,
		Target: "https://example.com/orphan", Method: "GET",
		IntervalSeconds: 60, ConfirmIntervalSeconds: 10, FailureThreshold: 3,
		TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-40 * time.Second)
	// Nothing advanced: this window was never entered into the ledger, which is what an orphan is.
	if _, exists := readRow(t, st, ctx, m.ID, due); exists {
		t.Fatal("the window already exists, so this is not the orphan case")
	}
	outcome, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: due,
	})
	if err != nil || !outcome.Applied {
		t.Fatalf("record result: outcome=%+v err=%v", outcome, err)
	}
	row, ok := readRow(t, st, ctx, m.ID, due)
	if !ok {
		t.Fatal("the orphan terminal created no row, so a run that happened is unrecorded forever")
	}
	if !row.IntervalAssumed {
		t.Error("interval_assumed is false on a row the core did not enter at issue")
	}
	if row.Interval != 10 {
		t.Fatalf("the orphan threshold is %ds, want the conservative 10 — the tightest of the "+
			"revision's two legitimate intervals, so an unrecorded window errs toward WITHHOLDING",
			row.Interval)
	}
	if row.Carrier == nil || *row.Carrier != domain.LedgerMinCarrier {
		t.Errorf("carrier_generation is %v, want %d: a row left at a column default reads unknown "+
			"forever, and §6.1's CHECK would have refused the insert outright",
			row.Carrier, domain.LedgerMinCarrier)
	}
	if row.JobID == nil || *row.JobID != ledgerJobA || row.IssuedAt == nil || row.TerminalAt == nil {
		t.Errorf("the orphan row is missing identity the wire carried: %+v", row)
	}
	if v := row.asExpectedRun(m.ID).Verdict(); v != domain.VerdictCoveredLate {
		t.Fatalf("a window spaced at 10s and answered 40s late reads %q, want %q", v, domain.VerdictCoveredLate)
	}
}

// A monitor with confirm acceleration DISABLED carries `confirm_interval_seconds = 0`, and the
// spec writes the orphan threshold as MIN(interval, confirm_interval). Taken literally that is 0,
// and a 0-second threshold makes every orphan window `covered_late` however promptly it was
// answered — over-withholding so total that the verdict stops meaning anything.
//
// Found in implementation, not in review, and it is the reason `LEAST(interval, NULLIF(confirm, 0))`
// is in the statement: LEAST ignores NULL arguments, so nulling the disabled value takes the
// minimum over the LEGITIMATE intervals, which is what "its two legitimate values" meant.
func TestTheOrphanThresholdIgnoresAConfirmIntervalThatIsDisabled(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "noconfirm")
	m := ledgerMonitor(t, st, ctx, proj, "no-confirm", 60)
	if m.ConfirmIntervalSeconds != 0 {
		t.Fatalf("fixture carries a confirm interval of %d; this test needs it disabled", m.ConfirmIntervalSeconds)
	}

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-5 * time.Second)
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: due,
	}); err != nil {
		t.Fatalf("record result: %v", err)
	}
	row, ok := readRow(t, st, ctx, m.ID, due)
	if !ok {
		t.Fatal("no orphan row")
	}
	if row.Interval != 60 {
		t.Fatalf("the threshold is %ds, want the base 60: a disabled confirm interval is not a "+
			"legitimate spacing and must not become the minimum", row.Interval)
	}
	if v := row.asExpectedRun(m.ID).Verdict(); v != domain.VerdictCovered {
		t.Errorf("a window answered 5s late against a 60s interval reads %q, want %q", v, domain.VerdictCovered)
	}
}

// §8.3 proof 3 — ADOPTION is deliberate and narrow, and a deliberately SKIPPED window is outside
// it. A window the gap logic recorded as never-issued may be adopted by a terminal that proves a
// run DID happen; a window carrying a `skip_reason` may never be, because that would convert "we
// chose not to run this" into "this ran" (invariant 10b).
func TestATerminalAdoptsANeverIssuedWindowAndNeverASkippedOne(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "adopt")
	m := ledgerMonitor(t, st, ctx, proj, "adopted", 60)

	now := time.Now().UTC().Truncate(time.Second)
	standing := now.Add(-3 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, standing)
	// A skip advances past two windows: the standing one carries the reason, the two after it
	// carry nothing.
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, SkipReason: SkipNoCapableRunner, NextDue: now.Add(time.Minute),
		IntervalInForce: 60, ExpectedDue: standing, ExpectedRevision: m.ExecutionRevision,
		Region: m.Region})

	neverIssued := standing.Add(time.Minute)
	if row, ok := readRow(t, st, ctx, m.ID, neverIssued); !ok || row.JobID != nil || row.SkipReason != nil {
		t.Fatalf("fixture: the window at %s is not a never-issued one: %+v (found=%v)", neverIssued, row, ok)
	}

	// Adoption: a terminal for a run that DID happen in that window fills it and moves the verdict
	// from expected_never_issued to covered, which is the truth.
	inTx(t, st, ctx, func(tx pgx.Tx) error {
		return fillExpectedRunTerminalTx(ctx, tx, expectedRunTerminal{
			MonitorID: m.ID, DueAt: neverIssued, JobID: ledgerJobA,
			Revision: m.ExecutionRevision, TerminalAt: now, IssuedAt: neverIssued.Add(time.Second),
			Outcome: ExpectedRunOutcomeResult})
	})
	adopted, _ := readRow(t, st, ctx, m.ID, neverIssued)
	if adopted.TerminalAt == nil || adopted.JobID == nil {
		t.Fatalf("a never-issued window was not adopted by the terminal that proves a run happened: %+v", adopted)
	}
	if adopted.Carrier == nil || *adopted.Carrier != domain.LedgerMinCarrier {
		t.Errorf("adoption left carrier_generation %v; COALESCE(existing, new) must fill it on a "+
			"no-job window", adopted.Carrier)
	}
	if v := adopted.asExpectedRun(m.ID).Verdict(); v != domain.VerdictCovered {
		t.Errorf("the adopted window reads %q, want %q", v, domain.VerdictCovered)
	}

	// The skipped window refuses the same terminal, however it is addressed.
	inTx(t, st, ctx, func(tx pgx.Tx) error {
		return fillExpectedRunTerminalTx(ctx, tx, expectedRunTerminal{
			MonitorID: m.ID, DueAt: standing, JobID: ledgerJobB,
			Revision: m.ExecutionRevision, TerminalAt: now, IssuedAt: standing.Add(time.Second),
			Outcome: ExpectedRunOutcomeResult})
	})
	skipped, _ := readRow(t, st, ctx, m.ID, standing)
	if skipped.TerminalAt != nil || skipped.JobID != nil {
		t.Fatalf("a window carrying skip_reason %v was adopted: %+v — a run cerbix chose not to "+
			"make was reported as one that happened", skipped.SkipReason, skipped)
	}
	if v := skipped.asExpectedRun(m.ID).Verdict(); v != domain.VerdictExpectedNeverIssued {
		t.Errorf("the skipped window reads %q, want %q", v, domain.VerdictExpectedNeverIssued)
	}
}

// Invariant 25b — a CROSSED pair updates nothing. One run's job id with another window's due
// instant must correlate to NO window rather than to the wrong one.
func TestACrossedJobAndWindowPairUpdatesNothing(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "crossed")
	m := ledgerMonitor(t, st, ctx, proj, "overlapping", 30)

	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-60 * time.Second)
	backdateSchedule(t, st, ctx, m.ID, older)
	mustAdvance(t, st, ctx, now.Add(-30*time.Second), ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now, IntervalInForce: 30,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: older,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: now.Add(30 * time.Second), IntervalInForce: 30,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: now,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	// Two runs outstanding at once are two rows, which is the direct answer to the P0 that killed
	// revision 1: one mutable row per monitor could not hold them.
	newer := now
	if _, ok := readRow(t, st, ctx, m.ID, older); !ok {
		t.Fatal("the older run has no row")
	}
	if _, ok := readRow(t, st, ctx, m.ID, newer); !ok {
		t.Fatal("the newer run has no row")
	}

	// job A with window B, and job B with window A: neither may touch anything.
	for _, tc := range []struct {
		name  string
		jobID string
		dueAt time.Time
	}{
		{"older run's job on the newer window", ledgerJobA, newer},
		{"newer run's job on the older window", ledgerJobB, older},
	} {
		inTx(t, st, ctx, func(tx pgx.Tx) error {
			return fillExpectedRunTerminalTx(ctx, tx, expectedRunTerminal{
				MonitorID: m.ID, DueAt: tc.dueAt, JobID: tc.jobID,
				Revision: m.ExecutionRevision, TerminalAt: now, IssuedAt: tc.dueAt,
				Outcome: ExpectedRunOutcomeResult})
		})
		row, _ := readRow(t, st, ctx, m.ID, tc.dueAt)
		if row.TerminalAt != nil {
			t.Errorf("%s: the crossed pair covered a window it does not answer", tc.name)
		}
	}

	// The converse, without which the exclusion proves nothing: the MATCHED pairs each fill their
	// own row and cannot reach the other.
	inTx(t, st, ctx, func(tx pgx.Tx) error {
		return fillExpectedRunTerminalTx(ctx, tx, expectedRunTerminal{
			MonitorID: m.ID, DueAt: older, JobID: ledgerJobA,
			Revision: m.ExecutionRevision, TerminalAt: now, IssuedAt: older,
			Outcome: ExpectedRunOutcomeResult})
	})
	if row, _ := readRow(t, st, ctx, m.ID, older); row.TerminalAt == nil {
		t.Error("the matched pair did not fill the older window")
	}
	if row, _ := readRow(t, st, ctx, m.ID, newer); row.TerminalAt != nil {
		t.Error("filling the older window also filled the newer one")
	}
}

// Reversed arrival, from party [241]: both PAIRS, because the reported instance had a twin nobody
// named. An event that contributes more than one column must have every one of them chosen by the
// SAME comparison, so a row never carries one delivery's timestamp beside another's attribute.
//
// The mutation that must fail it: revert either attribute to `COALESCE(existing, new)`, which is
// the shape both statements shipped with through revision 10.
func TestReversedArrivalNeverMixesTwoDeliveriesAttributes(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "reversed")

	now := time.Now().UTC().Truncate(time.Second)
	earlier, later := now.Add(-20*time.Second), now.Add(-5*time.Second)

	// The terminal pair: terminal_at with outcome.
	for _, order := range []struct {
		name  string
		first time.Time
		firstOutcome,
		second string
		secondAt time.Time
	}{
		{"later first", later, ExpectedRunOutcomeProbeError, ExpectedRunOutcomeResult, earlier},
		{"earlier first", earlier, ExpectedRunOutcomeResult, ExpectedRunOutcomeProbeError, later},
	} {
		t.Run("terminal/"+order.name, func(t *testing.T) {
			m := ledgerMonitor(t, st, ctx, proj, "terminal-"+order.name[:5], 60)
			due := now.Add(-30 * time.Second)
			backdateSchedule(t, st, ctx, m.ID, due)
			mustAdvance(t, st, ctx, now, ExpectationAdvance{
				MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
				CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
				ExpectedRevision: m.ExecutionRevision, Region: m.Region})
			for _, d := range []struct {
				at      time.Time
				outcome string
			}{{order.first, order.firstOutcome}, {order.secondAt, order.second}} {
				inTx(t, st, ctx, func(tx pgx.Tx) error {
					return fillExpectedRunTerminalTx(ctx, tx, expectedRunTerminal{
						MonitorID: m.ID, DueAt: due, JobID: ledgerJobA,
						Revision: m.ExecutionRevision, TerminalAt: d.at, IssuedAt: due,
						Outcome: d.outcome})
				})
			}
			row, _ := readRow(t, st, ctx, m.ID, due)
			if row.TerminalAt == nil || !row.TerminalAt.Equal(earlier) {
				t.Fatalf("terminal_at is %v, want the EARLIEST observation %s", row.TerminalAt, earlier)
			}
			// The EARLIER delivery carries `result` in both orders, which is the point: the
			// answer must not depend on arrival order at all.
			const wantOutcome = ExpectedRunOutcomeResult
			if row.Outcome == nil || *row.Outcome != wantOutcome {
				t.Fatalf("outcome is %v, want %q — the earliest timestamp's own attribute, not the "+
					"other delivery's", row.Outcome, wantOutcome)
			}
		})
	}

	// The refusal pair: refused_at with refused_reason. This is the instance [241] reported.
	for _, order := range []struct {
		name     string
		firstAt  time.Time
		firstWhy string
		lastAt   time.Time
		lastWhy  string
	}{
		{"later first", later, ReasonFutureTimestamp, earlier, ReasonStaleRevision},
		{"earlier first", earlier, ReasonStaleRevision, later, ReasonFutureTimestamp},
	} {
		t.Run("refusal/"+order.name, func(t *testing.T) {
			m := ledgerMonitor(t, st, ctx, proj, "refusal-"+order.name[:5], 60)
			due := now.Add(-30 * time.Second)
			backdateSchedule(t, st, ctx, m.ID, due)
			mustAdvance(t, st, ctx, now, ExpectationAdvance{
				MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
				CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
				ExpectedRevision: m.ExecutionRevision, Region: m.Region})
			for _, d := range []struct {
				at  time.Time
				why string
			}{{order.firstAt, order.firstWhy}, {order.lastAt, order.lastWhy}} {
				inTx(t, st, ctx, func(tx pgx.Tx) error {
					return noteExpectedRunRefusalTx(ctx, tx, m.ID, due, ledgerJobA,
						m.ExecutionRevision, d.at, d.why)
				})
			}
			row, _ := readRow(t, st, ctx, m.ID, due)
			if row.RefusedAt == nil || !row.RefusedAt.Equal(earlier) {
				t.Fatalf("refused_at is %v, want the EARLIEST %s", row.RefusedAt, earlier)
			}
			if row.RefusedReason == nil || *row.RefusedReason != ReasonStaleRevision {
				t.Fatalf("refused_reason is %v, want %q — the earliest refusal's own reason",
					row.RefusedReason, ReasonStaleRevision)
			}
			// A refusal is NOT coverage: it fills refused_*, never terminal_at.
			if row.TerminalAt != nil {
				t.Errorf("a refusal filled terminal_at, so a result refused as evidence for a " +
					"heartbeat became evidence for a stroke")
			}
		})
	}
}

// Invariant 10a — a result the gate REFUSES fills no terminal column, and the statement is
// unreachable for one. Invariant 10f — the refusal is an UPDATE and never an INSERT, and a
// stale-revision refusal can never annotate a newer run's row.
func TestARefusedResultAnnotatesItsOwnRunAndNothingElse(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "refused")
	m := ledgerMonitor(t, st, ctx, proj, "refused", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-30 * time.Second)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustDispatch(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	// A result from a STALE generation, through the real ingest path.
	outcome, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision + 1, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: due,
	})
	if err != nil {
		t.Fatalf("record stale result: %v", err)
	}
	if outcome.Reason != ReasonStaleRevision || outcome.Inserted {
		t.Fatalf("the stale result was not refused: %+v", outcome)
	}
	row, _ := readRow(t, st, ctx, m.ID, due)
	if row.TerminalAt != nil {
		t.Fatal("a refused result filled terminal_at")
	}
	// It refused under a generation the ledger has no row for, so it annotates NOTHING — the
	// admissibility predicate compares execution_revision, which is what stops a rev5 refusal from
	// landing on a rev6 run's row.
	if row.RefusedAt != nil {
		t.Errorf("a refusal from generation %d annotated the row recording generation %d",
			m.ExecutionRevision+1, row.Revision)
	}

	// A refusal for a job the ledger DID enter, at the matching generation, annotates its own row.
	inTx(t, st, ctx, func(tx pgx.Tx) error {
		return noteExpectedRunRefusalTx(ctx, tx, m.ID, due, ledgerJobA, m.ExecutionRevision,
			now, ReasonFutureTimestamp)
	})
	annotated, _ := readRow(t, st, ctx, m.ID, due)
	if annotated.RefusedAt == nil || annotated.RefusedReason == nil ||
		*annotated.RefusedReason != ReasonFutureTimestamp {
		t.Fatalf("the refusal did not annotate its own run's row: %+v", annotated)
	}
	if annotated.TerminalAt != nil {
		t.Error("the refusal filled terminal_at")
	}
	// Coverage stays the single test `terminal_at IS NOT NULL`, so an annotated window is still
	// issued-never-claimed and licenses no stroke.
	if v := annotated.asExpectedRun(m.ID).Verdict(); v != domain.VerdictIssuedNeverClaimed {
		t.Errorf("an annotated window reads %q, want %q", v, domain.VerdictIssuedNeverClaimed)
	}

	// And a refusal for a job the ledger never entered creates NOTHING: zero rows affected is the
	// normal outcome, not an error.
	unknownDue := now.Add(-10 * time.Minute)
	inTx(t, st, ctx, func(tx pgx.Tx) error {
		return noteExpectedRunRefusalTx(ctx, tx, m.ID, unknownDue, ledgerJobB,
			m.ExecutionRevision, now, ReasonStaleRevision)
	})
	if _, exists := readRow(t, st, ctx, m.ID, unknownDue); exists {
		t.Error("a refusal INSERTED a row, inventing an issued run whose only evidence is inadmissible")
	}

	// A refusal may never ADOPT a window either, and this is the half the earlier assertions did
	// not reach: adding §8.1's no-job disjunct to the refusal's guard survived every one of them,
	// because none presented it with an existing never-issued row. Adoption asserts that a run
	// HAPPENED there, and a refusal is evidence only that something was delivered — so the guard
	// is `job_id = $3` alone (invariant 10f).
	gapDue := now.Add(-2 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, gapDue)
	mustAdvance(t, st, ctx, now.Add(2*time.Second), ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: gapDue,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	neverIssued := gapDue.Add(time.Minute)
	if row, ok := readRow(t, st, ctx, m.ID, neverIssued); !ok || row.JobID != nil {
		t.Fatalf("fixture: %s is not a never-issued window: %+v (found=%v)", neverIssued, row, ok)
	}
	inTx(t, st, ctx, func(tx pgx.Tx) error {
		return noteExpectedRunRefusalTx(ctx, tx, m.ID, neverIssued, ledgerJobA,
			m.ExecutionRevision, now, ReasonFutureTimestamp)
	})
	adopted, _ := readRow(t, st, ctx, m.ID, neverIssued)
	if adopted.RefusedAt != nil || adopted.RefusedReason != nil {
		t.Fatalf("a refusal ADOPTED a never-issued window: %+v — the row now says a run was "+
			"attempted in a window nothing was ever issued for", adopted)
	}
}

// Invariant 25 — five bad result shapes, each correlating to NO window while the heartbeat still
// lands. The shapes are the ones §13.1's validation table names, and the point of asserting the
// heartbeat separately is that a correlation failure must never cost a measurement.
func TestABadlyIdentifiedResultCorrelatesToNoWindowAndStillLands(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "badident")

	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name string
		make func(m domain.Monitor, due time.Time) domain.Heartbeat
	}{
		{"no job id", func(m domain.Monitor, due time.Time) domain.Heartbeat {
			return domain.Heartbeat{MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision,
				Ts: now, Up: true, JobIssuedAt: now, DueAt: due}
		}},
		{"a job id that is not a uuid", func(m domain.Monitor, due time.Time) domain.Heartbeat {
			return domain.Heartbeat{MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision,
				Ts: now, Up: true, JobID: "not-a-uuid", JobIssuedAt: now, DueAt: due}
		}},
		{"no due instant", func(m domain.Monitor, _ time.Time) domain.Heartbeat {
			return domain.Heartbeat{MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision,
				Ts: now, Up: true, JobID: ledgerJobA, JobIssuedAt: now}
		}},
		{"a due instant that postdates its own dispatch", func(m domain.Monitor, _ time.Time) domain.Heartbeat {
			return domain.Heartbeat{MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision,
				Ts: now, Up: true, JobID: ledgerJobA, JobIssuedAt: now, DueAt: now.Add(time.Minute)}
		}},
		{"a due instant outside retention", func(m domain.Monitor, _ time.Time) domain.Heartbeat {
			return domain.Heartbeat{MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision,
				Ts: now, Up: true, JobID: ledgerJobA, JobIssuedAt: now,
				DueAt: now.Add(-100 * 24 * time.Hour)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := ledgerMonitor(t, st, ctx, proj, "bad-"+tc.name[:6], 60)
			due := now.Add(-30 * time.Second)
			before := len(readWindows(t, st, ctx, m.ID))
			hb := tc.make(m, due)
			outcome, err := st.RecordScheduledResult(ctx, hb)
			if err != nil {
				t.Fatalf("record: %v", err)
			}
			if !outcome.Inserted {
				t.Fatalf("the heartbeat was not recorded: %+v — a correlation failure must never "+
					"cost a measurement", outcome)
			}
			if got := len(readWindows(t, st, ctx, m.ID)); got != before {
				t.Errorf("%d windows were written for a result carrying no usable identity", got-before)
			}
		})
	}
}

// A probe_error is a TERMINAL outcome, and the two halves of that are asserted separately: the
// diagnostic and the window it closes commit together, and a REFUSED diagnostic annotates without
// covering.
func TestAProbeErrorTerminatesItsWindowAndARefusedOneOnlyAnnotates(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "probeerr")
	m := ledgerMonitor(t, st, ctx, proj, "probe-error", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-30 * time.Second)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	out, err := st.RecordProbeError(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: due,
		ProbeError: &domain.ProbeError{Reason: domain.ProbeErrorNoDispatchKey, JobID: ledgerJobA},
	})
	if err != nil || !out.Recorded {
		t.Fatalf("record probe error: out=%+v err=%v", out, err)
	}
	row, _ := readRow(t, st, ctx, m.ID, due)
	if row.TerminalAt == nil {
		t.Fatal("a probe_error left the window unanswered: the run FINISHED, which is what coverage means")
	}
	if row.Outcome == nil || *row.Outcome != ExpectedRunOutcomeProbeError {
		t.Errorf("outcome is %v, want %q", row.Outcome, ExpectedRunOutcomeProbeError)
	}

	// A STALE diagnostic annotates and never covers, on a second window so the two are separable.
	second := now.Add(time.Minute)
	backdateSchedule(t, st, ctx, m.ID, second)
	mustAdvance(t, st, ctx, second.Add(time.Second), ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: second.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: second,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	stale, err := st.RecordProbeError(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision + 5,
		JobID: ledgerJobB, JobIssuedAt: second.Add(time.Second), DueAt: second,
		ProbeError: &domain.ProbeError{Reason: domain.ProbeErrorUnknownKeyID, JobID: ledgerJobB},
	})
	if err != nil {
		t.Fatalf("record stale probe error: %v", err)
	}
	if stale.Recorded || stale.Reason != ReasonStaleRevision {
		t.Fatalf("the stale diagnostic was not refused: %+v", stale)
	}
	staleRow, _ := readRow(t, st, ctx, m.ID, second)
	if staleRow.TerminalAt != nil {
		t.Error("a refused diagnostic covered its window")
	}
}
