package store

import (
	"context"
	"go/ast"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 §10 — the configuration boundary. Invariants 14, 14a and 14b.
//
// A configuration change can land while the scheduler leader is absent, because the API role is a
// different process. If a gap spanned a revision change, §7.1's `generate_series` would space the
// WHOLE gap at the pre-change interval — so the transaction that creates the generation closes the
// open segment at the OLD interval first.

// readScheduleUpdatedAt is the CONFIG WRITE's own instant: `syncMonitorScheduleTx` stamps
// `updated_at` with `statement_timestamp()` in the same statement that closes the segment, so it
// is the change instant the half-open range was bounded by.
func readScheduleUpdatedAt(t *testing.T, st *Store, ctx context.Context, monitorID string) time.Time {
	t.Helper()
	var at time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT updated_at FROM monitor_schedule WHERE monitor_id = $1`, monitorID).Scan(&at); err != nil {
		t.Fatalf("read schedule updated_at: %v", err)
	}
	return at
}

// Invariant 14a — a configuration write leaves `next_due_at` UNCHANGED, so the pending probe fires
// at the instant it would have fired anyway and the new interval governs from the FOLLOWING
// advance. This is the only equation compatible with invariant 1, and the reviewer flagged its
// absence at party [227]: `now`, `now + new interval` and an old due rescaled to the new interval
// each move the probe instant differently.
//
// The mutation that must fail it is adding `next_due_at` to the segment close's SET list in ANY
// of those three forms. The column is absent from it entirely, so the guarantee is structural.
func TestAConfigurationWriteLeavesTheProbeInstantAlone(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "cfginstant")
	m := ledgerMonitor(t, st, ctx, proj, "steady", 60)

	// Both sides of a due instant, and both a base-interval and a confirm-interval change: the
	// four cases party [227] required.
	standing := time.Now().UTC().Truncate(time.Second).Add(30 * time.Second)
	for _, tc := range []struct {
		name     string
		expect   time.Time
		interval int
		confirm  int
	}{
		{"interval change just before the due instant", standing, 120, 0},
		{"interval change just after the due instant", standing.Add(-time.Minute), 120, 0},
		{"confirm change just before the due instant", standing, 60, 15},
		{"confirm change just after the due instant", standing.Add(-time.Minute), 60, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backdateSchedule(t, st, ctx, m.ID, tc.expect)
			live, err := st.GetMonitor(ctx, m.ID)
			if err != nil {
				t.Fatalf("read monitor: %v", err)
			}
			live.IntervalSeconds = tc.interval
			live.ConfirmIntervalSeconds = tc.confirm
			if tc.confirm > 0 {
				live.FailureThreshold = 3
			}
			updated, err := st.UpdateMonitor(ctx, live)
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			sched := readSchedule(t, st, ctx, m.ID)
			if !sched.NextDueAt.Equal(tc.expect) {
				t.Fatalf("next_due_at moved from %s to %s: the pending probe's instant changed as a "+
					"result of a bookkeeping write", tc.expect, sched.NextDueAt)
			}
			// The interval describing the STANDING instant is left alone — it produced that
			// instant, and the write that produced it is the only one entitled to change it. The
			// new configuration governs from the following advance, which writes the pair
			// together. What the configuration write does record is the new GENERATION.
			if sched.Interval != 60 {
				t.Errorf("interval_in_force is %d, want the 60 that produced next_due_at: a "+
					"configuration write that restamps it makes the standing window's lateness "+
					"threshold describe an interval it was never spaced by", sched.Interval)
			}
			if sched.Revision != updated.ExecutionRevision {
				t.Errorf("the schedule records generation %d, want the new %d",
					sched.Revision, updated.ExecutionRevision)
			}
			m = updated
		})
	}
}

// Invariant 14 and 14b, together, because they are the two halves of one write: no gap spans a
// revision change, and the range that closes it is HALF-OPEN.
//
// Required at party [231] with `next_due_at` ALREADY PAST — ten intervals back. The config write
// must materialize the NINE windows after `next_due_at` and NOT the one AT it; §7.1's later
// `current_window` insert for that instant must SUCCEED rather than conflicting; and exactly one
// row must exist for it, carrying the generation the run actually executed under.
//
// The mutation that must fail it is revision 5's inclusive lower bound: start the series AT
// `next_due_at` instead of one interval after it. The standing window then exists with the OLD
// generation and no job, and §7.1's insert hits `ON CONFLICT DO NOTHING` — so the run that
// answered it is silently unrecorded and the window reads expected_never_issued forever.
func TestAConfigWriteClosesTheOpenSegmentWithoutTakingTheStandingWindow(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "halfopen")
	m := ledgerMonitor(t, st, ctx, proj, "absent-leader", 60)

	now := time.Now().UTC().Truncate(time.Second)
	standing := now.Add(-10 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, standing)
	oldRevision := m.ExecutionRevision

	live, err := st.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatalf("read monitor: %v", err)
	}
	live.IntervalSeconds = 300 // the new interval must space neither the closed segment nor the
	//                            standing window, which was produced by the old one
	updated, err := st.UpdateMonitor(ctx, live)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	windows := readWindows(t, st, ctx, m.ID)
	// The change instant is the config transaction's OWN clock, so the count is derived from it
	// rather than from the test's `now`: an assertion written as a literal would be a proxy for
	// the property and would drift with the statement's latency.
	changeInstant := readScheduleUpdatedAt(t, st, ctx, m.ID)
	wantWindows := int(changeInstant.Sub(standing) / time.Minute)
	if changeInstant.Sub(standing)%time.Minute == 0 {
		// The range is half-open at the top: a window exactly AT the change instant is not in it.
		wantWindows--
	}
	if len(windows) != wantWindows {
		t.Fatalf("the config write materialized %d windows, want the %d strictly between the "+
			"standing expectation %s and the change instant %s: %+v",
			len(windows), wantWindows, standing, changeInstant, windows)
	}
	if !windows[0].DueAt.Equal(standing.Add(time.Minute)) {
		t.Fatalf("the segment starts at %s, want one interval AFTER the standing expectation %s — "+
			"an inclusive lower bound takes the standing window, whose only writer is §7.1",
			windows[0].DueAt, standing)
	}
	if last := windows[len(windows)-1].DueAt; !last.Before(changeInstant) {
		t.Errorf("the segment's last window %s is not strictly before the change instant %s", last, changeInstant)
	}
	for _, w := range windows {
		if w.DueAt.Equal(standing) {
			t.Fatal("the config write took the STANDING window, which has exactly one writer — " +
				"§7.1, when the expectation is finally acted on")
		}
		if w.JobID != nil {
			t.Errorf("window %s carries a job: nothing ran while the leader was away", w.DueAt)
		}
		// Every window in the closed segment is spaced by the interval that was in force FOR it.
		if w.Interval != 60 {
			t.Errorf("window %s records interval_seconds %d, want the old 60 — the new 300 spaces "+
				"the segment that starts now", w.DueAt, w.Interval)
		}
		if w.Revision != oldRevision {
			t.Errorf("window %s records generation %d, want the %d that was live while nothing ran",
				w.DueAt, w.Revision, oldRevision)
		}
	}
	// The half-open bound is what makes this next step possible: §7.1 writes the standing window
	// when it finally acts on it, and there is nothing there to conflict with.
	if n := mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(5 * time.Minute), IntervalInForce: 300,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: standing,
		ExpectedRevision: updated.ExecutionRevision, Region: m.Region}); n != 1 {
		// The advance carries the NEW interval because it produces the NEXT instant; the window
		// it answers keeps the old one, which is the distinction §10's table draws.
		t.Fatal("the advance was fenced out; the fence compares the monitor's CURRENT generation")
	}
	answered, ok := readRow(t, st, ctx, m.ID, standing)
	if !ok {
		t.Fatal("the standing window was never written by anyone")
	}
	if answered.JobID == nil || *answered.JobID != ledgerJobA {
		t.Fatalf("the standing window records job %v, want the one that answered it", answered.JobID)
	}
	// §10's table: `execution_revision` is the configuration the run EXECUTED under, which is the
	// new one, while `interval_seconds` is the spacing, which is the old one. Conflating the two is
	// what made revision 5 read as self-contradictory.
	if answered.Revision != updated.ExecutionRevision {
		t.Errorf("the answered window records generation %d, want the %d the run executed under",
			answered.Revision, updated.ExecutionRevision)
	}
	if answered.Interval != 60 {
		t.Errorf("the answered window records interval_seconds %d, want the 60 that SPACED it",
			answered.Interval)
	}
	var count int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM expected_runs WHERE monitor_id = $1 AND due_at = $2`,
		m.ID, standing).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("%d rows exist for the standing instant, want exactly 1", count)
	}
}

// Enabling CREATES the expectation and disabling CLOSES the segment and then removes it, which is
// §10's last paragraph. A disabled monitor expects nothing, and the span it was disabled for is
// claimable as nothing rather than as a run that never happened.
func TestDisablingClosesTheSegmentAndThenStopsExpectingAnything(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "lifecycle")
	m := ledgerMonitor(t, st, ctx, proj, "toggled", 60)

	now := time.Now().UTC().Truncate(time.Second)
	standing := now.Add(-5 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, standing)

	live, err := st.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	live.Enabled = false
	if _, err := st.UpdateMonitor(ctx, live); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// The segment CLOSED: the windows between the standing expectation and the disable instant are
	// recorded, because they are windows nothing ran in and that is a fact. The count comes from
	// the DISABLE instant rather than from the test's clock, for the reason the half-open test
	// gives — and the disable removes the schedule row, so the instant is read from the newest
	// window rather than from a row that no longer exists.
	closed := readWindows(t, st, ctx, m.ID)
	if len(closed) < 4 || len(closed) > 5 {
		t.Fatalf("disabling recorded %d windows for the ~5 minutes it was absent: %+v", len(closed), closed)
	}
	if !closed[0].DueAt.Equal(standing.Add(time.Minute)) {
		t.Errorf("the closed segment starts at %s, want one interval after the standing %s",
			closed[0].DueAt, standing)
	}
	for _, w := range closed {
		if !w.DueAt.After(standing) || w.DueAt.After(now.Add(time.Minute)) {
			t.Errorf("window %s is outside the span the monitor was absent for", w.DueAt)
		}
	}
	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM monitor_schedule WHERE monitor_id = $1`, m.ID).Scan(&rows); err != nil {
		t.Fatalf("count schedule: %v", err)
	}
	if rows != 0 {
		t.Fatal("a disabled monitor still has a schedule row, so it still expects runs")
	}

	// Re-enabling creates it at NOW, never at the instant it was disabled: nothing recorded an
	// expectation while it was off, so the span before this instant is claimable as nothing.
	live, err = st.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	live.Enabled = true
	if _, err := st.UpdateMonitor(ctx, live); err != nil {
		t.Fatalf("enable: %v", err)
	}
	sched := readSchedule(t, st, ctx, m.ID)
	if sched.NextDueAt.Before(now) {
		t.Errorf("re-enabling set next_due_at to %s, before the enable instant — the monitor is "+
			"immediately in arrears for time it was deliberately off", sched.NextDueAt)
	}
	if sched.ConfirmPhase {
		t.Error("a freshly created expectation is in confirm phase; enabling resets liveness, so " +
			"InConfirmPhase() cannot be true")
	}
	if sched.Interval != 60 {
		t.Errorf("interval_in_force is %d, want the base 60", sched.Interval)
	}
}

// A push monitor NEVER has a schedule row and never has a window, by every path (invariant 20b).
// `checkStalePush` remains the single mechanism that detects a push which did not arrive, and a
// ledger window would be a SECOND mechanism for one obligation — which is the defect [225], [229]
// and [241] all are.
func TestAPushMonitorNeverExpectsAWindow(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "pushexcl")
	push, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj, Name: "pinged", Type: domain.MonitorPush,
		IntervalSeconds: 60, TimeoutSeconds: 5, GraceSeconds: 30, Enabled: true,
		PushToken: "push-token-fixture",
	})
	if err != nil {
		t.Fatalf("create push monitor: %v", err)
	}
	assertNoSchedule := func(when string) {
		var rows int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM monitor_schedule WHERE monitor_id = $1`, push.ID).Scan(&rows); err != nil {
			t.Fatalf("count schedule: %v", err)
		}
		if rows != 0 {
			t.Errorf("%s: the push monitor has a schedule row", when)
		}
		if windows := readWindows(t, st, ctx, push.ID); len(windows) != 0 {
			t.Errorf("%s: the push monitor has %d windows", when, len(windows))
		}
	}
	assertNoSchedule("on creation")
	live, err := st.GetMonitor(ctx, push.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	live.GraceSeconds = 45
	if _, err := st.UpdateMonitor(ctx, live); err != nil {
		t.Fatalf("update: %v", err)
	}
	assertNoSchedule("after a configuration write")
	// Retire and reactivate are COMPOSITE-only doors — `assertCompositeMonitor` refuses a push
	// monitor by name — so the paths a push monitor actually has are creation, the configuration
	// write above, and a result arriving for it.
	//
	// That last one is the case with teeth, and it is the reason the ORPHAN insert carries the
	// participation predicate: a crafted result carrying a job id and a due instant would
	// otherwise CREATE a window for a monitor §15 excludes, and the orphan path is the one write
	// with no schedule row to consult.
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: push.ID, ExecutionRevision: 2, Ts: now, Up: true,
		JobID: ledgerJobA, JobIssuedAt: now, DueAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("record a scheduled result for a push monitor: %v", err)
	}
	assertNoSchedule("after a scheduled result carrying a window identity")
}

// The pairing guard. Every site that CREATES a generation must also sync the schedule in the same
// transaction, for the same reason phase A's guard exists: the compiler cannot see a missing
// statement, and a gap that spans a revision change is invisible and permanent.
//
// It reuses phase A's own AST helpers rather than building a second mechanism — `referencesIn` and
// `insertsMonitor` already detect the two halves of "creates a generation", and the only new
// question is which identifier must accompany them.
//
// The mutation that must kill it: delete any one `syncMonitorScheduleTx` call. The test names the
// function that lost it.
func TestEveryGenerationCreatingSiteSyncsTheSchedule(t *testing.T) {
	files := parseStorePackage(t)
	const scheduleIdent = "syncMonitorScheduleTx"
	var unpaired []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == scheduleIdent {
				continue
			}
			creates := referencesIn(fn, fenceIdent) || insertsMonitor(fn)
			if creates && !referencesIn(fn, scheduleIdent) {
				unpaired = append(unpaired, fn.Name.Name)
			}
		}
	}
	sort.Strings(unpaired)
	if len(unpaired) != 0 {
		t.Fatalf("these functions create a configuration generation and never close the window "+
			"segment it ends: %s\nEach one leaves a gap spaced by the wrong interval, invisibly "+
			"and permanently (FR-032 invariant 14).", strings.Join(unpaired, ", "))
	}
	// The converse, so the guard cannot pass by finding nothing: the sites it checks must be the
	// ones we know about, and a shrinking set is as much a failure as an unpaired one.
	got := fenceSites(files)
	if len(got) < 4 {
		t.Errorf("the guard found only %d fence sites (%v); the tree has four, and a guard that "+
			"stopped seeing its own subject reports green forever", len(got), got)
	}
}
