package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 — a window ANSWERED EARLY, which the correlation boundary used to refuse outright.
//
// The leader dispatches from its in-memory `nextRun`, not from `monitor_schedule.next_due_at`, and
// two ordinary states put the first EARLIER than the second: leadership acquisition, where the map
// is created empty so every monitor is due at once while its standing expectation may still be in
// the future, and confirm acceleration, which pulls `nextRun` in and by §7.3 writes nothing to the
// schedule. Both mint a job whose `DueAt` postdates its own `IssuedAt` — the core minting both from
// one statement — and the old rule "an expectation cannot postdate its own dispatch" then refused
// the run its own window was written for. Every such window read `issued_never_claimed` forever,
// which is what invariant 25 says a result must never produce.

// earlyWindowFixture puts a monitor's standing expectation in the FUTURE and dispatches against it,
// which is exactly the row a returning leader writes. It returns the window's due instant and the
// issue instant, in that order, because the whole point is that the first is after the second.
func earlyWindowFixture(t *testing.T, st *Store, ctx context.Context, m domain.Monitor,
	issuedAt time.Time, ahead time.Duration, jobID string) (due time.Time) {
	t.Helper()
	due = issuedAt.Add(ahead)
	backdateSchedule(t, st, ctx, m.ID, due) // the setter is generic; here it moves the row FORWARD
	mustAdvance(t, st, ctx, issuedAt, ExpectationAdvance{
		MonitorID: m.ID, JobID: jobID, NextDue: issuedAt.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	// The publish returned, so the window carries an ISSUE instant (§7.4). This fixture is about
	// the relationship between that instant and the due one, which the reserve alone does not
	// establish — a reserved window has no issue instant at all, and that is the point of the
	// state rather than an accident of it.
	mustConfirm(t, st, ctx, issuedAt, ExpectationConfirm{MonitorID: m.ID, DueAt: due, JobID: jobID})
	row, ok := readRow(t, st, ctx, m.ID, due)
	if !ok || row.IssuedAt == nil || !row.IssuedAt.Before(row.DueAt) {
		t.Fatalf("fixture: %s is not a window issued BEFORE its due instant: %+v (found=%v)", due, row, ok)
	}
	return due
}

// The regression, stated as the state it prevents: the run that answers an early window covers it.
func TestAWindowAnsweredBeforeItsDueInstantIsCovered(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "early")
	m := ledgerMonitor(t, st, ctx, proj, "early-answer", 60)

	now := time.Now().UTC().Truncate(time.Second)
	due := earlyWindowFixture(t, st, ctx, m, now, 30*time.Second, ledgerJobA)

	out, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: due,
	})
	if err != nil || !out.Inserted {
		t.Fatalf("record result: out=%+v err=%v", out, err)
	}
	row, ok := readRow(t, st, ctx, m.ID, due)
	if !ok || row.TerminalAt == nil {
		t.Fatalf("the window the core minted this job for was never answered: %+v (found=%v).\n"+
			"That is a false issued_never_claimed for a run whose heartbeat landed — invariant 25.", row, ok)
	}
	if got := row.asExpectedRun(m.ID).Verdict(); got != domain.VerdictCovered {
		t.Errorf("verdict is %q, want %q: answering a window EARLY is not lateness, and Verdict "+
			"already reads a negative issued_at - due_at as covered", got, domain.VerdictCovered)
	}
	// The claim takes the same path and must reach the same row, or `claimed_never_finished` stays
	// unreachable for exactly the windows a failover produces.
	if matched, err := st.RecordRunClaim(ctx,
		claimHeartbeat(m, due, ledgerJobA, now)); err != nil || !matched {
		t.Fatalf("claim on an early window: matched=%v err=%v", matched, err)
	}
	if row, _ := readRow(t, st, ctx, m.ID, due); row.ClaimedAt == nil {
		t.Error("the claim for an early window recorded nothing")
	}
}

// The other half, and the reason correlation can be relaxed at all: an early window may be FILLED
// and never INVENTED. Without this the relaxation would let an executor-supplied instant in the
// future create a window for time nothing was dispatched for.
func TestAnEarlyWindowIsAdoptedButNeverInvented(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "earlyorphan")
	m := ledgerMonitor(t, st, ctx, proj, "early-orphan", 60)

	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(10 * time.Minute)
	before := len(readWindows(t, st, ctx, m.ID))

	out, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobB, JobIssuedAt: now, DueAt: future,
	})
	if err != nil || !out.Inserted {
		t.Fatalf("record result: out=%+v err=%v", out, err)
	}
	if got := len(readWindows(t, st, ctx, m.ID)); got != before {
		t.Fatalf("a result carrying a FUTURE due instant invented %d window(s). The orphan insert "+
			"must require the row to exist already — only the core can put one there.", got-before)
	}

	// The same result, once the core HAS written the window, fills it. Same statement, same
	// heartbeat shape: the only thing that changed is that the expectation now exists.
	due := earlyWindowFixture(t, st, ctx, m, now, 10*time.Minute, ledgerJobB)
	if due != future {
		t.Fatalf("fixture drift: %s != %s", due, future)
	}
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now.Add(time.Second), Up: true,
		JobID: ledgerJobB, JobIssuedAt: now, DueAt: future,
	}); err != nil {
		t.Fatalf("record second result: %v", err)
	}
	if row, ok := readRow(t, st, ctx, m.ID, future); !ok || row.TerminalAt == nil {
		t.Errorf("the window was not adopted once it existed: %+v (found=%v)", row, ok)
	}
}

// A revision below 1 names no generation, and the terminal fill was the ONE ledger write without
// the guard its two siblings already have. It is reachable through `result.revision_mode: observe`,
// which accepts a result carrying `execution_revision: 0` — and the orphan insert would then stamp
// a window `execution_revision = 0`, a generation no timeline row can ever describe, reading
// `covered`.
func TestAResultWithNoRevisionWritesNoWindowEvenInObserveMode(t *testing.T) {
	st, ctx := ledgerStore(t)
	st.WithResultRevisionMode("observe")
	proj := seedLedgerProject(t, st, ctx, "norev")
	m := ledgerMonitor(t, st, ctx, proj, "no-revision", 60)

	now := time.Now().UTC().Truncate(time.Second)
	before := len(readWindows(t, st, ctx, m.ID))
	out, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: 0, Ts: now, Up: true,
		JobID: ledgerJobC, JobIssuedAt: now, DueAt: now.Add(-30 * time.Second),
	})
	if err != nil {
		t.Fatalf("record result: %v", err)
	}
	if !out.Inserted || !out.MissingRevisionObserved {
		t.Fatalf("observe mode did not accept the heartbeat: %+v — the fixture no longer reaches "+
			"the statement under test", out)
	}
	if got := len(readWindows(t, st, ctx, m.ID)); got != before {
		t.Errorf("a result with no execution revision wrote %d window(s) stamped "+
			"execution_revision = 0", got-before)
	}
}

// §9.3's cap says it "exists to bound one statement's work", and the retention clip is the primary
// bound. Both are asserted here through the property that made them checkable at all: every window
// lands on the monitor's OWN grid.
//
// `generate_series(start, stop, step)` emits `start + k*step`, so a series whose start was the
// retention floor — which it was, whenever the clip bit — produced windows at instants no
// expectation ever fell on. They were never adopted and never claimable, so nothing broke; what
// broke was `due_at` meaning "the instant a run was expected". The index form fixes that AND lets
// the cap bound the series rather than only the rows it writes.
func TestClippedGapWindowsStayOnTheMonitorsOwnGrid(t *testing.T) {
	st, ctx := ledgerStore(t)
	// Two days of retention (the enforced minimum) against a standing expectation five days old,
	// so the clip certainly bites and the series cannot start at `first_expected`.
	st.WithExpectedRunPolicy(2, domain.DefaultExpectedRunGapWindowsMax)
	proj := seedLedgerProject(t, st, ctx, "gridclip")
	m := ledgerMonitor(t, st, ctx, proj, "grid-clip", 3600)

	now := time.Now().UTC().Truncate(time.Second)
	standing := now.Add(-5 * 24 * time.Hour)
	backdateSchedule(t, st, ctx, m.ID, standing)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Hour), IntervalInForce: 3600,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: standing,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	windows := readWindows(t, st, ctx, m.ID)
	if len(windows) < 10 {
		t.Fatalf("only %d windows materialized; the clip fixture no longer produces a gap", len(windows))
	}
	firstExpected := standing.Add(time.Hour)
	for _, w := range windows {
		if w.DueAt.Equal(standing) {
			continue // the window the advance ANSWERED, at the standing instant itself
		}
		if off := w.DueAt.Sub(firstExpected) % time.Hour; off != 0 {
			t.Fatalf("window %s is %s off the monitor's grid. A clipped series that starts at the "+
				"retention floor rather than at first_expected materializes windows at instants no "+
				"expectation ever fell on.", w.DueAt, off)
		}
	}
	// The clip is visible where §9.3 requires it to be visible, and not only in the row count.
	if s := readSchedule(t, st, ctx, m.ID); s.Truncated == nil {
		t.Error("a clipped span left no gap_truncated_before: the fence is what makes the " +
			"unmaterialized part claimable as nothing (invariant 24a)")
	}
}

// The cap, as a bound on the SERIES. It is asserted through what an observer can see — the kept
// windows are the most RECENT `cap` of them, on the grid, with the fence set — because the work the
// statement avoided is not itself observable from SQL.
func TestTheGapCapKeepsTheMostRecentWindowsOnTheGrid(t *testing.T) {
	st, ctx := ledgerStore(t)
	st.WithExpectedRunPolicy(domain.DefaultExpectedRunRetentionDays, domain.MinExpectedRunGapWindowsMax)
	proj := seedLedgerProject(t, st, ctx, "gridcap")
	m := ledgerMonitor(t, st, ctx, proj, "grid-cap", 60)

	now := time.Now().UTC().Truncate(time.Second)
	standing := now.Add(-500 * time.Minute) // 500 windows at a 60s interval, cap is 100
	backdateSchedule(t, st, ctx, m.ID, standing)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: standing,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	windows := readWindows(t, st, ctx, m.ID)
	// The cap's worth of gap windows, plus the one the advance answered.
	if want := domain.MinExpectedRunGapWindowsMax + 1; len(windows) != want {
		t.Fatalf("%d windows written, want %d — the per-monitor cap is not being applied",
			len(windows), want)
	}
	firstExpected := standing.Add(time.Minute)
	for _, w := range windows {
		if w.DueAt.Equal(standing) {
			continue
		}
		if off := w.DueAt.Sub(firstExpected) % time.Minute; off != 0 {
			t.Fatalf("capped window %s is %s off the grid", w.DueAt, off)
		}
	}
	if s := readSchedule(t, st, ctx, m.ID); s.Truncated == nil {
		t.Error("the cap cut windows and wrote no fence")
	}
}

// Invariant 20e's second sentence — "no executor-supplied value can move a window from
// covered_late to covered" — asserted against the merge rule rather than against the threshold.
//
// The THRESHOLD is safe: `interval_seconds` is written by the core and never read from a result.
// The MEASUREMENT was not. §8.3's upsert took
// `issued_at = LEAST(COALESCE(expected_runs.issued_at, $6), $6)`, where `$6` is the executor's echo
// of `JobIssuedAt` — so a result echoing an earlier instant LOWERED the core's own `issued_at`,
// shrinking `issued_at - due_at` and promoting the verdict in the one direction this design
// refuses. `issued_at` has ONE writer on a window the core recorded, and it is the core.
func TestAnExecutorCannotLowerTheIssueInstantItWasHanded(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "issuedat")
	m := ledgerMonitor(t, st, ctx, proj, "issued-at", 60)

	now := time.Now().UTC().Truncate(time.Second)
	// A window answered LATE: due five minutes ago, issued now, against a 60-second interval.
	due := now.Add(-5 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	mustConfirm(t, st, ctx, now, ExpectationConfirm{MonitorID: m.ID, DueAt: due, JobID: ledgerJobA})
	before, ok := readRow(t, st, ctx, m.ID, due)
	if !ok || before.IssuedAt == nil {
		t.Fatalf("fixture: no issued window at %s", due)
	}

	// The result echoes an issue instant of its own choosing — here the due instant itself, which
	// would make the run look perfectly punctual.
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.ExecutionRevision, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: due, DueAt: due,
	}); err != nil {
		t.Fatalf("record result: %v", err)
	}

	after, _ := readRow(t, st, ctx, m.ID, due)
	if after.IssuedAt == nil || !after.IssuedAt.Equal(*before.IssuedAt) {
		t.Errorf("issued_at moved from %v to %v on a result's say-so. The core wrote that instant; "+
			"a result may fill it only where the core never did (the orphan path).",
			before.IssuedAt, after.IssuedAt)
	}
	if got := after.asExpectedRun(m.ID).Verdict(); got != domain.VerdictCoveredLate {
		t.Errorf("verdict is %q, want %q — an executor promoted a late window by echoing an "+
			"earlier issue instant, which is invariant 20e", got, domain.VerdictCoveredLate)
	}
}
