package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase B2 — §7.1's primitive, the ONE statement that moves an expectation forward.
//
// Every case here is specified in §17.1 before the code existed, and each names the mutation it
// must fail. The reason the mutations are named rather than left to taste is party [225]: the
// reviewer found a batch-wide LIMIT by reading SQL that the surrounding prose contradicted, and a
// test that survives that mutation has not reached the mechanism.

func ledgerStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run expected-run ledger store tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Daily partitions exist in production because the scheduler's maintenance pass creates them.
	// A test that relied on the DEFAULT partition alone would be testing a fallback, so the same
	// call production makes runs here.
	if err := st.EnsureExpectedRunPartitions(ctx, 1); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	return st, ctx
}

func seedLedgerProject(t *testing.T, st *Store, ctx context.Context, slug string) string {
	t.Helper()
	org, err := st.CreateOrganization(ctx, slug+"-org", "Ledger Org")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, slug, "Ledger Project")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return proj.ID
}

// ledgerMonitor creates a participating monitor. Creation writes its schedule row through the same
// `syncMonitorScheduleTx` production uses, so no fixture invents one.
func ledgerMonitor(t *testing.T, st *Store, ctx context.Context, projectID, name string, intervalSeconds int) domain.Monitor {
	t.Helper()
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: name, Type: domain.MonitorHTTP,
		Target: "https://example.com/" + name, Method: "GET",
		IntervalSeconds: intervalSeconds, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor %s: %v", name, err)
	}
	return m
}

// backdateSchedule puts a monitor's standing expectation `intervals` intervals in the past, which
// is what a leader absence looks like from the ledger's side. It writes `next_due_at` DIRECTLY
// because production has exactly one writer for it and that writer is the thing under test.
func backdateSchedule(t *testing.T, st *Store, ctx context.Context, monitorID string, at time.Time) {
	t.Helper()
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitor_schedule SET next_due_at = $2 WHERE monitor_id = $1`, monitorID, at); err != nil {
		t.Fatalf("backdate schedule: %v", err)
	}
}

type ledgerWindow struct {
	DueAt      time.Time
	JobID      *string
	Carrier    *int
	Interval   int
	Revision   int64
	Region     string
	IssuedAt   *time.Time
	SkipReason *string
}

func readWindows(t *testing.T, st *Store, ctx context.Context, monitorID string) []ledgerWindow {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT due_at, job_id::text, carrier_generation, interval_seconds, execution_revision,
		        region, issued_at, skip_reason
		   FROM expected_runs WHERE monitor_id = $1 ORDER BY due_at`, monitorID)
	if err != nil {
		t.Fatalf("read windows: %v", err)
	}
	defer rows.Close()
	var out []ledgerWindow
	for rows.Next() {
		var w ledgerWindow
		if err := rows.Scan(&w.DueAt, &w.JobID, &w.Carrier, &w.Interval, &w.Revision, &w.Region,
			&w.IssuedAt, &w.SkipReason); err != nil {
			t.Fatalf("scan window: %v", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate windows: %v", err)
	}
	return out
}

type ledgerSchedule struct {
	NextDueAt    time.Time
	Interval     int
	Revision     int64
	LastIssuedAt *time.Time
	Truncated    *time.Time
	ConfirmPhase bool
}

func readSchedule(t *testing.T, st *Store, ctx context.Context, monitorID string) ledgerSchedule {
	t.Helper()
	var s ledgerSchedule
	if err := st.pool.QueryRow(ctx,
		`SELECT next_due_at, interval_in_force, execution_revision, last_issued_at,
		        gap_truncated_before, confirm_phase
		   FROM monitor_schedule WHERE monitor_id = $1`, monitorID).
		Scan(&s.NextDueAt, &s.Interval, &s.Revision, &s.LastIssuedAt, &s.Truncated, &s.ConfirmPhase); err != nil {
		t.Fatalf("read schedule for %s: %v", monitorID, err)
	}
	return s
}

// mustAdvance RESERVES (§7.4). The name is kept because every caller of it is asserting what the
// statement writes rather than when it runs, and because the phase-F cases that care about the
// ordering call the store directly.
//
// An item carrying a job gets `ReservedAt` defaulted to the tick's own instant, which is what the
// scheduler passes when the mint and the tick are the same moment. A case that needs the two to
// DIFFER — an identity minted before the instant the reserve commits — sets the field itself.
func mustAdvance(t *testing.T, st *Store, ctx context.Context, now time.Time, items ...ExpectationAdvance) int {
	t.Helper()
	for i := range items {
		if items[i].JobID != "" && items[i].ReservedAt.IsZero() {
			items[i].ReservedAt = now
		}
	}
	reserved, err := st.ReserveExpectations(ctx, now, items)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return len(reserved)
}

// mustDispatch is the WHOLE tick for one window: reserve, then confirm, which is what a successful
// publish looks like from the ledger's side (§7.4). Fixtures whose subject is an ISSUED window use
// it, so the two statements are not repeated at every site and — more importantly — so a fixture
// that deliberately stops at the reserve is visible as a different call.
func mustDispatch(t *testing.T, st *Store, ctx context.Context, now time.Time, item ExpectationAdvance) {
	t.Helper()
	mustAdvance(t, st, ctx, now, item)
	mustConfirm(t, st, ctx, now, ExpectationConfirm{
		MonitorID: item.MonitorID, DueAt: item.ExpectedDue, JobID: item.JobID})
}

// mustConfirm is the second half of the tick: the publish returned, so the window becomes an issued
// one. Tests that assert on `issued_at` go through here, because after phase F the reserve alone
// never writes it.
func mustConfirm(t *testing.T, st *Store, ctx context.Context, now time.Time, items ...ExpectationConfirm) int {
	t.Helper()
	n, err := st.ConfirmExpectations(ctx, now, items)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return n
}

const ledgerJobA = "11111111-1111-4111-8111-111111111111"
const ledgerJobB = "22222222-2222-4222-8222-222222222222"
const ledgerJobC = "33333333-3333-4333-8333-333333333333"

// §17.1 (a) — invariants 24a and 24b. TWO monitors in one batch, cap 3, both ten intervals in the
// past. Each must get ITS OWN most recent three windows and its OWN fence.
//
// The mutation this must fail is revision 2's structure: replace the per-monitor
// `row_number() ... PARTITION BY monitor_id` with a batch-wide `LIMIT`. The assertions are written
// so the failure NAMES the monitor whose windows vanished, because a test that reports only "wrong
// count" sends the reader after the wrong bug.
func TestTheGapCapIsPerMonitorAndEachMonitorFencesItsOwn(t *testing.T) {
	st, ctx := ledgerStore(t)
	st.expectedRunGapCap = 3
	proj := seedLedgerProject(t, st, ctx, "gapcap")
	first := ledgerMonitor(t, st, ctx, proj, "first", 60)
	second := ledgerMonitor(t, st, ctx, proj, "second", 60)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-10 * time.Minute)
	for _, m := range []domain.Monitor{first, second} {
		backdateSchedule(t, st, ctx, m.ID, expectation)
	}

	advanced := mustAdvance(t, st, ctx, now,
		ExpectationAdvance{MonitorID: first.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute),
			IntervalInForce: 60, CarrierGeneration: domain.LedgerMinCarrier,
			ExpectedDue: expectation, ExpectedRevision: first.ExecutionRevision, Region: first.Region},
		ExpectationAdvance{MonitorID: second.ID, JobID: ledgerJobB, NextDue: now.Add(time.Minute),
			IntervalInForce: 60, CarrierGeneration: domain.LedgerMinCarrier,
			ExpectedDue: expectation, ExpectedRevision: second.ExecutionRevision, Region: second.Region})
	if advanced != 2 {
		t.Fatalf("advanced %d schedule rows, want 2", advanced)
	}

	for _, m := range []domain.Monitor{first, second} {
		windows := readWindows(t, st, ctx, m.ID)
		// 1 answered window + exactly 3 missed ones, for THIS monitor.
		if len(windows) != 4 {
			t.Fatalf("monitor %s has %d windows, want 4 (its own 3 missed plus the one it answered): %+v",
				m.Name, len(windows), windows)
		}
		missed := 0
		for _, w := range windows {
			if w.JobID == nil {
				missed++
				if w.Carrier != nil {
					t.Errorf("monitor %s: a never-issued window carries a carrier generation %d", m.Name, *w.Carrier)
				}
			}
		}
		if missed != 3 {
			t.Errorf("monitor %s got %d missed windows, want its own 3", m.Name, missed)
		}
		// The fence is THIS monitor's oldest materialized window, not the batch's.
		sched := readSchedule(t, st, ctx, m.ID)
		if sched.Truncated == nil {
			t.Fatalf("monitor %s: nothing was fenced, so the cap dropped windows nobody can see", m.Name)
		}
		oldestMissed := windows[1].DueAt // windows[0] is the answered expectation itself
		if !sched.Truncated.Equal(oldestMissed) {
			t.Errorf("monitor %s fence is %s, want its own oldest materialized window %s",
				m.Name, sched.Truncated, oldestMissed)
		}
		if !sched.NextDueAt.Equal(now.Add(time.Minute)) {
			t.Errorf("monitor %s next_due_at is %s, want %s", m.Name, sched.NextDueAt, now.Add(time.Minute))
		}
	}
}

// §17.1 (b) — a returning leader whose FIRST action is a POLICY SKIP. Revision 4 argued its way out
// of gap materialization on this path and lost the gap exactly as the [225] P0 had.
//
// The mutation that must fail it is revision 4's structure itself: give the skip path its own
// statement without the gap and fence CTEs.
func TestALeaderGapWhoseFirstActionIsAPolicySkipStillMaterializesTheGap(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "skipgap")
	m := ledgerMonitor(t, st, ctx, proj, "skipped", 60)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-10 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, expectation)

	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, SkipReason: SkipNoCapableRunner, NextDue: now.Add(time.Minute),
		IntervalInForce: 60, ExpectedDue: expectation, ExpectedRevision: m.ExecutionRevision,
		Region: m.Region})

	windows := readWindows(t, st, ctx, m.ID)
	if len(windows) != 10 {
		t.Fatalf("got %d windows, want 10: the standing expectation plus the nine it advanced past", len(windows))
	}
	// The standing expectation carries the reason and no job; the nine before `now` carry neither.
	standing := windows[0]
	if standing.SkipReason == nil || *standing.SkipReason != SkipNoCapableRunner {
		t.Errorf("the standing window's skip_reason is %v, want %q", standing.SkipReason, SkipNoCapableRunner)
	}
	if standing.JobID != nil || standing.IssuedAt != nil {
		t.Errorf("a skipped window carries a job or an issue instant: %+v", standing)
	}
	for _, w := range windows[1:] {
		if w.JobID != nil {
			t.Errorf("window %s carries a job id, but nothing ran in it", w.DueAt)
		}
		if w.SkipReason != nil {
			t.Errorf("window %s carries skip_reason %q, but cerbix never decided anything about it — "+
				"it is a window nothing ran in, which is a different fact", w.DueAt, *w.SkipReason)
		}
	}
	sched := readSchedule(t, st, ctx, m.ID)
	if sched.LastIssuedAt != nil {
		t.Errorf("last_issued_at moved on a skip: %v — nothing was issued", sched.LastIssuedAt)
	}
	if !sched.NextDueAt.Equal(now.Add(time.Minute)) {
		t.Errorf("next_due_at is %s, want %s", sched.NextDueAt, now.Add(time.Minute))
	}
	// Nothing was truncated: nine windows is far below the cap and inside retention.
	if sched.Truncated != nil {
		t.Errorf("a fence was written for a gap that was fully materialized: %v", sched.Truncated)
	}
	// And every one of those windows reads as expected-never-issued rather than covered.
	for _, w := range windows {
		row := domain.ExpectedRun{MonitorID: m.ID, DueAt: w.DueAt, IntervalSeconds: w.Interval}
		if v := row.Verdict(); v != domain.VerdictExpectedNeverIssued {
			t.Errorf("window %s reads %q, want %q", w.DueAt, v, domain.VerdictExpectedNeverIssued)
		}
	}
}

// §17.1 (c) — the same gap whose first action is a BACKOFF. Everything from (b), plus the property
// a delayed next_due must not corrupt: `interval_in_force` stays the monitor's real interval.
//
// The mutation that must fail it is deriving one from the other — passing the backoff delay as the
// interval. Every later gap window would then be spaced by a retry timer instead of by the
// monitor's cadence, invisibly and permanently (invariant 2b).
func TestABackoffDelayNeverBecomesTheIntervalInForce(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "backoff")
	m := ledgerMonitor(t, st, ctx, proj, "backed-off", 60)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-10 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, expectation)

	const backoff = 7 * time.Minute
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, SkipReason: SkipCredentialUnresolved, NextDue: now.Add(backoff),
		IntervalInForce: 60, ExpectedDue: expectation, ExpectedRevision: m.ExecutionRevision,
		Region: m.Region})

	sched := readSchedule(t, st, ctx, m.ID)
	if sched.Interval != 60 {
		t.Fatalf("interval_in_force is %d, want 60: the backoff delay became the interval, so every "+
			"later gap window will be spaced by a retry timer", sched.Interval)
	}
	if !sched.NextDueAt.Equal(now.Add(backoff)) {
		t.Errorf("next_due_at is %s, want the delayed %s", sched.NextDueAt, now.Add(backoff))
	}
	if sched.LastIssuedAt != nil {
		t.Errorf("last_issued_at moved on a backoff: %v", sched.LastIssuedAt)
	}
	windows := readWindows(t, st, ctx, m.ID)
	if len(windows) != 10 {
		t.Fatalf("got %d windows, want 10", len(windows))
	}
	if windows[0].SkipReason == nil || *windows[0].SkipReason != SkipCredentialUnresolved {
		t.Errorf("the standing window's skip_reason is %v, want %q", windows[0].SkipReason, SkipCredentialUnresolved)
	}
	// Every window is spaced by 60 seconds, which is what makes the interval column checkable
	// rather than merely present.
	for i := 1; i < len(windows); i++ {
		if gap := windows[i].DueAt.Sub(windows[i-1].DueAt); gap != time.Minute {
			t.Errorf("windows %d and %d are %s apart, want 1m", i-1, i, gap)
		}
		if windows[i].Interval != 60 {
			t.Errorf("window %s records interval_seconds %d, want 60", windows[i].DueAt, windows[i].Interval)
		}
	}
}

// Invariant 25e — the fence has TWO predicates. Revision 8 fenced on `next_due_at` alone, which is
// the ONE datum §10 guarantees does not change, so a configuration write between publish and here
// PASSED the old check while having changed the revision.
//
// The mutation that must fail this is revision 8's single-predicate fence: drop
// `m.execution_revision = v.expected_revision` and the stale item is accepted.
func TestAConfigWriteBetweenPublishAndAdvanceFencesTheWholeItem(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "fence")
	m := ledgerMonitor(t, st, ctx, proj, "fenced", 60)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-5 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, expectation)
	publishedRevision := m.ExecutionRevision

	// A configuration write commits, bumping the revision and — per §10 — leaving next_due_at
	// exactly where it was. That is what makes the single-predicate fence pass.
	m.Name = "fenced-renamed"
	updated, err := st.UpdateMonitor(ctx, m)
	if err != nil {
		t.Fatalf("update monitor: %v", err)
	}
	if updated.ExecutionRevision == publishedRevision {
		t.Fatalf("the update did not create a generation, so this test cannot fence anything")
	}
	afterConfig := readSchedule(t, st, ctx, m.ID)
	if !afterConfig.NextDueAt.Equal(expectation) {
		t.Fatalf("the config write moved next_due_at from %s to %s, which invariant 14a forbids",
			expectation, afterConfig.NextDueAt)
	}
	windowsAfterConfig := len(readWindows(t, st, ctx, m.ID))

	advanced := mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: expectation,
		ExpectedRevision: publishedRevision, Region: m.Region})
	if advanced != 0 {
		t.Fatalf("the fence admitted an item published under generation %d while the monitor is on %d",
			publishedRevision, updated.ExecutionRevision)
	}
	// On mismatch NOTHING happens at all: no advance, no window, no fence write.
	if got := len(readWindows(t, st, ctx, m.ID)); got != windowsAfterConfig {
		t.Errorf("the fenced item wrote %d new windows", got-windowsAfterConfig)
	}
	sched := readSchedule(t, st, ctx, m.ID)
	if !sched.NextDueAt.Equal(expectation) {
		t.Errorf("next_due_at advanced past the fence: %s", sched.NextDueAt)
	}
	if sched.LastIssuedAt != nil {
		t.Errorf("last_issued_at moved for a fenced item: %v", sched.LastIssuedAt)
	}

	// The converse, without which the exclusion proves nothing: republished at the CURRENT
	// generation the same window is materialized and is the only eligible row for that instant.
	if n := mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: expectation,
		ExpectedRevision: updated.ExecutionRevision, Region: m.Region}); n != 1 {
		t.Fatalf("the fence refused an item published at the current generation")
	}
	windows := readWindows(t, st, ctx, m.ID)
	var answered *ledgerWindow
	for i := range windows {
		if windows[i].DueAt.Equal(expectation) {
			answered = &windows[i]
		}
	}
	if answered == nil {
		t.Fatal("the standing expectation has no window")
	}
	if answered.JobID == nil || *answered.JobID != ledgerJobB {
		t.Errorf("the window records job %v, want the republished %s", answered.JobID, ledgerJobB)
	}
	// Invariant 25d: every RUN fact comes from the PUBLISHED job, never re-read from the schedule.
	if answered.Revision != updated.ExecutionRevision {
		t.Errorf("the window records revision %d, want the published job's %d",
			answered.Revision, updated.ExecutionRevision)
	}
}

// Invariant 20c — the interval a window records is the one that SPACED it, never the caller's new
// interval, which spaces the NEXT window.
//
// The mutation that must fail it is writing `new_interval` into the current window's
// `interval_seconds`: with the two values different, a window answered on time would be judged
// against a threshold it was never spaced by.
func TestTheCurrentWindowRecordsTheIntervalThatSpacedItAndNotTheNextOne(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "spacing")
	m := ledgerMonitor(t, st, ctx, proj, "accelerating", 60)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-90 * time.Second)
	backdateSchedule(t, st, ctx, m.ID, expectation)

	// The monitor enters confirm acceleration: the NEXT window is spaced at 10s, and the window
	// being answered right now was spaced at 60.
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(10 * time.Second),
		IntervalInForce: 10, CarrierGeneration: domain.LedgerMinCarrier,
		ExpectedDue: expectation, ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	windows := readWindows(t, st, ctx, m.ID)
	if len(windows) == 0 {
		t.Fatal("no window was written")
	}
	answered := windows[0]
	if !answered.DueAt.Equal(expectation) {
		t.Fatalf("the first window is %s, want the standing expectation %s", answered.DueAt, expectation)
	}
	if answered.Interval != 60 {
		t.Fatalf("the answered window records interval_seconds %d, want the 60 that spaced it — "+
			"the accelerated 10 spaces the window AFTER it", answered.Interval)
	}
	if sched := readSchedule(t, st, ctx, m.ID); sched.Interval != 10 {
		t.Errorf("interval_in_force is %d, want the new 10", sched.Interval)
	}
}

// A window and its fence commit TOGETHER or not at all. §17.1 (a) assertion 5.
//
// The advance is one statement, so a rolled-back transaction leaves neither the windows nor the
// moved expectation. Asserted through an explicit transaction rather than by killing a connection,
// because the property is atomicity and not crash recovery — and a test that cannot state which of
// the two it proves is worth less than one that can.
func TestTheWindowsTheFenceAndTheAdvanceShareOneTransaction(t *testing.T) {
	st, ctx := ledgerStore(t)
	st.expectedRunGapCap = 3
	proj := seedLedgerProject(t, st, ctx, "atomic")
	m := ledgerMonitor(t, st, ctx, proj, "atomic", 60)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-10 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, expectation)

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, reserveExpectationsSQL,
		[]string{m.ID}, []*string{&[]string{ledgerJobA}[0]}, []*string{nil},
		[]time.Time{now.Add(time.Minute)}, []int32{60},
		now, 3, now.Add(-14*24*time.Hour), []*int32{&[]int32{domain.LedgerMinCarrier}[0]},
		[]time.Time{expectation}, []int64{m.ExecutionRevision}, []string{m.Region},
		[]bool{false}, []*time.Time{&now}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("reserve in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if windows := readWindows(t, st, ctx, m.ID); len(windows) != 0 {
		t.Errorf("%d windows survived the rollback", len(windows))
	}
	sched := readSchedule(t, st, ctx, m.ID)
	if !sched.NextDueAt.Equal(expectation) {
		t.Errorf("the advance survived the rollback: next_due_at is %s", sched.NextDueAt)
	}
	if sched.Truncated != nil {
		t.Errorf("the fence survived the rollback: %v", sched.Truncated)
	}
}

// The retention CLIP is the primary bound, and it fences too. A monitor unprobed for longer than
// retention must not materialize windows that would be dropped unread, and the span before the
// oldest one it did materialize must be fenced — otherwise a reader could claim it.
func TestTheRetentionClipBoundsTheGapAndWritesItsOwnFence(t *testing.T) {
	st, ctx := ledgerStore(t)
	// A tight retention so the clip, and not the cap, is the bound that bites.
	st.expectedRunRetentionDays = 2
	st.expectedRunGapCap = 100000
	proj := seedLedgerProject(t, st, ctx, "clip")
	m := ledgerMonitor(t, st, ctx, proj, "long-absent", 3600)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-10 * 24 * time.Hour)
	backdateSchedule(t, st, ctx, m.ID, expectation)

	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Hour), IntervalInForce: 3600,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: expectation,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	windows := readWindows(t, st, ctx, m.ID)
	floor := now.Add(-48 * time.Hour)
	for _, w := range windows {
		if w.JobID == nil && w.DueAt.Before(floor) {
			t.Errorf("window %s is older than the retention floor %s and would be dropped unread",
				w.DueAt, floor)
		}
	}
	sched := readSchedule(t, st, ctx, m.ID)
	if sched.Truncated == nil {
		t.Fatal("the clip dropped ten days of windows and wrote no fence, so a reader could claim them")
	}
	if sched.Truncated.Before(floor) {
		t.Errorf("the fence is at %s, before the retention floor %s", sched.Truncated, floor)
	}
}

// An item the caller could not have meant is refused by NAME, before the statement runs. A single
// bad element aborts one statement for the whole tick, so it would take every other monitor's
// evidence down with it — and the error has to say which monitor.
func TestAnUnrepresentableAdvanceIsRefusedByName(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "validate")
	m := ledgerMonitor(t, st, ctx, proj, "valid", 60)
	now := time.Now().UTC()
	base := ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: now,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region,
	}
	for _, tc := range []struct {
		name   string
		mutate func(a *ExpectationAdvance)
	}{
		{"both a job and a skip", func(a *ExpectationAdvance) { a.SkipReason = SkipNoCapableRunner }},
		{"neither a job nor a skip", func(a *ExpectationAdvance) { a.JobID = "" }},
		{"a job with no carrier", func(a *ExpectationAdvance) { a.CarrierGeneration = 0 }},
		{"a skip with a carrier", func(a *ExpectationAdvance) { a.JobID = ""; a.SkipReason = SkipNoInflightSlot }},
		{"a non-positive interval", func(a *ExpectationAdvance) { a.IntervalInForce = 0 }},
		{"no region", func(a *ExpectationAdvance) { a.Region = "" }},
		{"no expected revision", func(a *ExpectationAdvance) { a.ExpectedRevision = 0 }},
		// Phase F: a job whose identity carries no minted instant would be reserved at NULL, which
		// §6.1 cannot express and which would hand the one instant the core owns back to the
		// executor's echo (§7.4).
		{"a job with no minted instant", func(a *ExpectationAdvance) { a.ReservedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := base
			item.ReservedAt = now
			tc.mutate(&item)
			_, err := st.ReserveExpectations(ctx, now, []ExpectationAdvance{item})
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), m.ID) {
				t.Errorf("the refusal does not name the monitor: %v", err)
			}
		})
	}
}

// The fence moves FORWARD and is never erased — the monotonicity `GREATEST` carries, and the one
// property no other assertion in this file reaches.
//
// It earned its own test by surviving a planted mutation: replacing
// `GREATEST(s.gap_truncated_before, f.truncated_before)` with the new value alone left every
// other test green, because each of them writes a fence exactly once. In PostgreSQL GREATEST
// IGNORES NULL arguments, so a tick with no truncation keeps the fence it had; in standard SQL the
// NULL would propagate and erase it, which is precisely the failure the statement exists to
// prevent. That is a dependency on a PostgreSQL-specific semantic, so it is asserted rather than
// assumed.
func TestTheFenceOnlyEverMovesForwardAndIsNeverErased(t *testing.T) {
	st, ctx := ledgerStore(t)
	st.expectedRunGapCap = 3
	proj := seedLedgerProject(t, st, ctx, "monotone")
	m := ledgerMonitor(t, st, ctx, proj, "monotone", 60)

	now := time.Now().UTC().Truncate(time.Second)
	backdateSchedule(t, st, ctx, m.ID, now.Add(-10*time.Minute))
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: now.Add(-10 * time.Minute),
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	first := readSchedule(t, st, ctx, m.ID)
	if first.Truncated == nil {
		t.Fatal("the capped gap wrote no fence")
	}

	// An ORDINARY tick: the expectation is one interval away, nothing is truncated, and
	// `f.truncated_before` is NULL. The fence must survive it.
	later := now.Add(time.Minute)
	if n := mustAdvance(t, st, ctx, later, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: later.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: now.Add(time.Minute),
		ExpectedRevision: m.ExecutionRevision, Region: m.Region}); n != 1 {
		t.Fatalf("the ordinary advance was fenced out")
	}
	kept := readSchedule(t, st, ctx, m.ID)
	if kept.Truncated == nil {
		t.Fatalf("a tick that truncated NOTHING erased the fence set at %s — the span before it is "+
			"now claimable, which is the over-claim the fence exists to prevent", first.Truncated)
	}
	if !kept.Truncated.Equal(*first.Truncated) {
		t.Errorf("the fence moved on a tick with no truncation: %s then %s", first.Truncated, kept.Truncated)
	}

	// A LATER truncation moves it forward.
	ahead := later.Add(30 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, ahead.Add(-10*time.Minute))
	mustAdvance(t, st, ctx, ahead, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: ahead.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: ahead.Add(-10 * time.Minute),
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	moved := readSchedule(t, st, ctx, m.ID)
	if moved.Truncated == nil || !moved.Truncated.After(*kept.Truncated) {
		t.Fatalf("a later truncation did not move the fence forward: %v then %v", kept.Truncated, moved.Truncated)
	}

	// And an EARLIER one cannot move it back. The column is written directly here because the only
	// production writer is monotone by construction — which is the property under test, so a
	// fixture is the only way to present it with an earlier candidate.
	standing := moved.Truncated.Add(2 * time.Hour)
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitor_schedule SET gap_truncated_before = $2 WHERE monitor_id = $1`, m.ID, standing); err != nil {
		t.Fatalf("plant a later fence: %v", err)
	}
	behind := ahead.Add(time.Hour)
	backdateSchedule(t, st, ctx, m.ID, behind.Add(-10*time.Minute))
	mustAdvance(t, st, ctx, behind, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobB, NextDue: behind.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: behind.Add(-10 * time.Minute),
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	final := readSchedule(t, st, ctx, m.ID)
	if final.Truncated == nil || !final.Truncated.Equal(standing) {
		t.Errorf("the fence regressed from %s to %v", standing, final.Truncated)
	}
}
