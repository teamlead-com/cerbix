package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase D — retention, `ledger_from`, and the paged read.
//
// The direction is what every assertion here is about: a dropped span must read as NOTHING. Not
// `covered`, and not `expected_never_issued` either — a ledger that forgot a span and then answered
// for it would be over-claiming through the one mechanism nobody looks at.

// plantPartitionDay creates a dated partition for one UTC day, so a test can age the table without
// waiting. It uses the SAME name shape the maintainer produces, because a partition this test
// invented under a different name would not be dropped by the pass under test.
func plantPartitionDay(t *testing.T, st *Store, ctx context.Context, day time.Time) string {
	t.Helper()
	name := expectedRunPartitionPrefix + day.UTC().Format("20060102")
	_, err := st.pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF expected_runs FOR VALUES FROM ('%s') TO ('%s')
		   WITH (fillfactor = %d)`,
		name, day.UTC().Format(pgTimestamp), day.UTC().AddDate(0, 0, 1).Format(pgTimestamp),
		expectedRunFillfactor))
	if err != nil {
		t.Fatalf("plant partition %s: %v", name, err)
	}
	return name
}

// Invariant 16 — dropping a partition never converts unproven time into proven time, and never
// invents a missed run. And invariant 24c — `ledger_from` moves WITH the drop, because it is
// computed from persisted inputs rather than stored.
//
// The mutation that must kill this: store `ledger_from` on the schedule row. Every assertion about
// windows still passes, and only the bound's movement catches it — which is exactly the drift
// revision 2 would have shipped.
func TestDroppingAPartitionMovesLedgerFromAndClaimsNothingBeforeIt(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "retention")
	m := ledgerMonitor(t, st, ctx, proj, "aged", 60)

	today := time.Now().UTC().Truncate(24 * time.Hour)
	old := today.AddDate(0, 0, -20)
	plantPartitionDay(t, st, ctx, old)
	// A window in the old partition, and one today. The old one is real evidence until the moment
	// its partition is dropped.
	oldDue := old.Add(12 * time.Hour)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO expected_runs (project_id, monitor_id, due_at, execution_revision, region, interval_seconds)
		 VALUES ($1, $2, $3, $4, $5, 60)`,
		proj, m.ID, oldDue, m.ExecutionRevision, m.Region); err != nil {
		t.Fatalf("plant an old window: %v", err)
	}
	// The schedule row was created NOW by the monitor's creation, so it would bound `ledger_from`
	// on its own. Backdating it isolates the partition floor, which is the input under test.
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitor_schedule SET schedule_created_at = $2 WHERE monitor_id = $1`,
		m.ID, old.AddDate(0, 0, -5)); err != nil {
		t.Fatalf("backdate schedule_created_at: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitor_execution_revisions SET effective_from = $2 WHERE monitor_id = $1`,
		m.ID, old.AddDate(0, 0, -5)); err != nil {
		t.Fatalf("backdate the revision timeline: %v", err)
	}

	before, ok, err := st.LedgerFrom(ctx, proj, m.ID)
	if err != nil || !ok {
		t.Fatalf("ledger_from before the drop: %v (ok=%v)", err, ok)
	}
	if !before.Equal(old) {
		t.Fatalf("ledger_from is %s, want the oldest retained partition's lower bound %s", before, old)
	}

	// Retention runs with a cutoff after that partition's whole range.
	cutoff := today.AddDate(0, 0, -14)
	dropped, err := st.PurgeOldExpectedRuns(ctx, cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if dropped < 1 {
		t.Fatalf("the pass dropped %d partitions, want at least the one entirely before %s", dropped, cutoff)
	}
	if _, exists := readRow(t, st, ctx, m.ID, oldDue); exists {
		t.Error("a window in a dropped partition survived")
	}
	after, ok, err := st.LedgerFrom(ctx, proj, m.ID)
	if err != nil || !ok {
		t.Fatalf("ledger_from after the drop: %v (ok=%v)", err, ok)
	}
	if !after.After(before) {
		t.Fatalf("ledger_from is still %s after dropping the partition that bounded it: a STORED "+
			"bound would read exactly like this, and it would over-claim the dropped span", after)
	}
	if after.Before(cutoff) {
		t.Errorf("ledger_from is %s, before the retention cutoff %s — the ledger claims to answer "+
			"for time whose windows are gone", after, cutoff)
	}
}

// A partition is dropped only once its WHOLE range is past the cutoff.
//
// Dropping one that still holds answerable windows would move `ledger_from` forward past evidence
// that exists. That is the withholding direction — safe, and still wrong: it discards facts the
// ledger paid to keep.
func TestAPartitionSurvivesUntilItsWholeRangeIsPastTheCutoff(t *testing.T) {
	st, ctx := ledgerStore(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := today.AddDate(0, 0, -3)
	name := plantPartitionDay(t, st, ctx, day)

	// A cutoff INSIDE the partition's range: it still covers answerable time.
	if _, err := st.PurgeOldExpectedRuns(ctx, day.Add(6*time.Hour)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !partitionExists(t, st, ctx, name) {
		t.Fatalf("%s was dropped while its range still covered time after the cutoff", name)
	}
	// Exactly at its upper bound: now its whole range is before the cutoff.
	if _, err := st.PurgeOldExpectedRuns(ctx, day.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if partitionExists(t, st, ctx, name) {
		t.Fatalf("%s survived a cutoff at its own upper bound", name)
	}
}

// Invariant 16a's second half — retention purges the DEFAULT partition too.
//
// This is what the gate ledger's retention never has to do, and the divergence is deliberate:
// `expected_runs` HAS a default partition because a lost insert would erase the very fact the
// ledger keeps, and having one means retention must reach into it. A pass that dropped only dated
// partitions would leave the default growing forever.
//
// The mutation that must kill this: delete the `DELETE FROM expected_runs_default` statement. Every
// partition assertion still passes.
func TestRetentionPurgesTheDefaultPartitionToo(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "defaultpurge")
	m := ledgerMonitor(t, st, ctx, proj, "leaked", 60)

	today := time.Now().UTC().Truncate(24 * time.Hour)
	// Far enough back that no dated partition exists or ever did — the row can only be in the
	// default, which is the case this is about.
	stranded := today.AddDate(0, 0, -60)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO expected_runs (project_id, monitor_id, due_at, execution_revision, region, interval_seconds)
		 VALUES ($1, $2, $3, $4, $5, 60)`,
		proj, m.ID, stranded, m.ExecutionRevision, m.Region); err != nil {
		t.Fatalf("plant a stranded window: %v", err)
	}
	var landedIn string
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM expected_runs WHERE monitor_id = $1 AND due_at = $2`,
		m.ID, stranded).Scan(&landedIn); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if landedIn != "expected_runs_default" {
		t.Fatalf("the fixture landed in %s, not the default partition", landedIn)
	}

	if _, err := st.PurgeOldExpectedRuns(ctx, today.AddDate(0, 0, -14)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, exists := readRow(t, st, ctx, m.ID, stranded); exists {
		t.Fatal("a row past retention survived in the DEFAULT partition: dated partitions are " +
			"dropped whole and the default is not, so a pass that ignores it leaves the table " +
			"growing forever")
	}
	// And a row INSIDE retention in the default partition is untouched — the pass purges by age,
	// not by partition.
	fresh := today.Add(-time.Hour)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO expected_runs (project_id, monitor_id, due_at, execution_revision, region, interval_seconds)
		 VALUES ($1, $2, $3, $4, $5, 60)`,
		proj, m.ID, fresh, m.ExecutionRevision, m.Region); err != nil {
		t.Fatalf("plant a fresh window: %v", err)
	}
	if _, err := st.PurgeOldExpectedRuns(ctx, today.AddDate(0, 0, -14)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, exists := readRow(t, st, ctx, m.ID, fresh); !exists {
		t.Error("a window inside retention was purged")
	}
}

// Invariant 15 — before `ledger_from` NO verdict is emitted, and the FENCE is one of its inputs.
//
// The fence is what §7.1 wrote when a cap or the retention clip stopped it materializing windows it
// had already advanced past. Without it those windows are simply absent, and absent reads as
// "nothing was due" — the over-claim the fence exists to prevent.
func TestTheTruncationFenceBoundsWhatTheLedgerWillAnswerFor(t *testing.T) {
	st, ctx := ledgerStore(t)
	st.expectedRunGapCap = 3
	proj := seedLedgerProject(t, st, ctx, "fencebound")
	m := ledgerMonitor(t, st, ctx, proj, "fenced", 60)

	now := time.Now().UTC().Truncate(time.Second)
	expectation := now.Add(-10 * time.Minute)
	backdateSchedule(t, st, ctx, m.ID, expectation)
	// Backdate the schedule's CREATION too, so the fence is the input under test rather than the
	// row's own age.
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitor_schedule SET schedule_created_at = $2 WHERE monitor_id = $1`,
		m.ID, now.Add(-time.Hour)); err != nil {
		t.Fatalf("backdate schedule_created_at: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitor_execution_revisions SET effective_from = $2 WHERE monitor_id = $1`,
		m.ID, now.Add(-time.Hour)); err != nil {
		t.Fatalf("backdate the revision timeline: %v", err)
	}
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: expectation,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	sched := readSchedule(t, st, ctx, m.ID)
	if sched.Truncated == nil {
		t.Fatal("the capped gap wrote no fence")
	}
	from, ok, err := st.LedgerFrom(ctx, proj, m.ID)
	if err != nil || !ok {
		t.Fatalf("ledger_from: %v (ok=%v)", err, ok)
	}
	if !from.Equal(sched.Truncated.UTC()) {
		t.Fatalf("ledger_from is %s, want the fence %s: the windows before it were advanced past "+
			"and never written, so the span is claimable as NOTHING", from, sched.Truncated)
	}
}

// A monitor with no schedule row answers for NOTHING, and `ok == false` says so.
//
// A push monitor is the ordinary case: §15 excludes it, so it has no expectation and never had one.
// Returning a zero instant with ok == true would let a caller render its whole history as covered
// time nothing was due in, which is the same over-claim from the other end.
func TestAMonitorWithNoScheduleAnswersForNothing(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "nosched")
	push, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj, Name: "pinged", Type: domain.MonitorPush,
		IntervalSeconds: 60, TimeoutSeconds: 5, GraceSeconds: 30, Enabled: true,
		PushToken: "push-token-retention",
	})
	if err != nil {
		t.Fatalf("create push monitor: %v", err)
	}
	from, ok, err := st.LedgerFrom(ctx, proj, push.ID)
	if err != nil {
		t.Fatalf("ledger_from: %v", err)
	}
	if ok {
		t.Fatalf("a push monitor answered for time from %s; §15 excludes it and it has no "+
			"expectation to bound", from)
	}
}

func partitionExists(t *testing.T, st *Store, ctx context.Context, name string) bool {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_class WHERE relname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("check partition %s: %v", name, err)
	}
	return n > 0
}

// §13a's pagination, against the convention this repository publishes rather than against taste.
//
// The keyset is `due_at` alone and needs no tiebreak, which is structural: the query is
// monitor-scoped and the primary key is `(monitor_id, due_at)`, so `due_at` is unique within one
// monitor. If this is ever widened to project scope a `monitor_id` tiebreak becomes MANDATORY —
// noted in the code because that change would otherwise silently start dropping rows.
func TestTheWindowListPagesStrictlyBelowItsCursorAndEndsWithANullOne(t *testing.T) {
	st, ctx := ledgerStore(t)
	proj := seedLedgerProject(t, st, ctx, "paging")
	m := ledgerMonitor(t, st, ctx, proj, "paged", 60)

	now := time.Now().UTC().Truncate(time.Second)
	// Five windows, one minute apart.
	for i := 1; i <= 5; i++ {
		due := now.Add(-time.Duration(i) * time.Minute)
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO expected_runs (project_id, monitor_id, due_at, execution_revision, region, interval_seconds)
			 VALUES ($1, $2, $3, $4, $5, 60)`,
			proj, m.ID, due, m.ExecutionRevision, m.Region); err != nil {
			t.Fatalf("plant window %d: %v", i, err)
		}
	}
	from, to := now.Add(-time.Hour), now

	first, err := st.ListExpectedRuns(ctx, ExpectedRunQuery{
		ProjectID: proj, MonitorID: m.ID, From: from, To: to, Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Windows) != 2 {
		t.Fatalf("first page has %d windows, want 2", len(first.Windows))
	}
	// Newest first.
	if !first.Windows[0].DueAt.After(first.Windows[1].DueAt) {
		t.Errorf("the page is not in due_at DESC order: %s then %s",
			first.Windows[0].DueAt, first.Windows[1].DueAt)
	}
	if first.NextCursor == "" {
		t.Fatal("a full page carried no cursor")
	}

	seen := map[time.Time]bool{}
	for _, w := range first.Windows {
		seen[w.DueAt] = true
	}
	cursor := first.NextCursor
	pages := 1
	for cursor != "" {
		at, err := DecodeExpectedRunCursor(cursor)
		if err != nil {
			t.Fatalf("decode cursor: %v", err)
		}
		page, err := st.ListExpectedRuns(ctx, ExpectedRunQuery{
			ProjectID: proj, MonitorID: m.ID, From: from, To: to, Limit: 2, CursorDueAt: &at})
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
		for _, w := range page.Windows {
			if seen[w.DueAt] {
				t.Fatalf("window %s was returned twice: the next page must be bound STRICTLY below "+
					"the last returned key", w.DueAt)
			}
			seen[w.DueAt] = true
			if !w.DueAt.Before(at) {
				t.Errorf("window %s is not strictly below the cursor %s", w.DueAt, at)
			}
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("the traversal did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("the traversal saw %d of 5 windows: %v", len(seen), seen)
	}

	// The cursor is VERSIONED, so a later change to its shape is detectable rather than misread,
	// and the decode is STRICT — a bad prefix, bad base64 or an unparseable instant all fail the
	// same way.
	for _, bad := range []string{"", "not-base64!!", encodeBadVersionCursorForTest(now), encodeBadInstantCursorForTest()} {
		if _, err := DecodeExpectedRunCursor(bad); err == nil {
			t.Errorf("the decoder accepted %q", bad)
		}
	}
}

// Invariant 26a — the project predicate is in the SQL, proven at the STORE layer and not only
// through the handler.
//
// A handler check alone is one refactor away from being bypassed, and a query that is safe only
// because of its caller is not safe. The mutation that must kill this: drop `AND project_id = $2`
// from the listing — the handler still refuses the request, and the store stops being safe on its
// own.
func TestAMismatchedProjectAndMonitorReturnNothingAtTheStoreLayer(t *testing.T) {
	st, ctx := ledgerStore(t)
	mine := seedLedgerProject(t, st, ctx, "mine")
	theirs := seedLedgerProject(t, st, ctx, "theirs")
	m := ledgerMonitor(t, st, ctx, mine, "private", 60)

	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO expected_runs (project_id, monitor_id, due_at, execution_revision, region, interval_seconds)
		 VALUES ($1, $2, $3, $4, $5, 60)`,
		mine, m.ID, now.Add(-time.Minute), m.ExecutionRevision, m.Region); err != nil {
		t.Fatalf("plant a window: %v", err)
	}

	page, err := st.ListExpectedRuns(ctx, ExpectedRunQuery{
		ProjectID: theirs, MonitorID: m.ID, From: now.Add(-time.Hour), To: now, Limit: 50})
	if err != nil {
		t.Fatalf("cross-project list: %v", err)
	}
	if len(page.Windows) != 0 {
		t.Fatalf("another project's query returned %d windows of this monitor", len(page.Windows))
	}
	if page.HasLedgerFrom {
		t.Error("another project's query got a ledger_from for this monitor")
	}
	// The converse, so the exclusion is not satisfied by a query that returns nothing to anyone.
	own, err := st.ListExpectedRuns(ctx, ExpectedRunQuery{
		ProjectID: mine, MonitorID: m.ID, From: now.Add(-time.Hour), To: now, Limit: 50})
	if err != nil {
		t.Fatalf("own list: %v", err)
	}
	if len(own.Windows) != 1 {
		t.Fatalf("the owning project got %d windows, want 1", len(own.Windows))
	}
}

// Invariant 26b — every answer carries `ledger_from` and `gap_truncated_before`.
//
// A caller holding the rows and not the bounds reads an empty range as "nothing was due" when the
// truth is "the ledger cannot say". The bounds travel with EVERY page rather than being fetched
// separately, because a caller that has to ask twice is a caller that will forget to.
func TestEveryPageCarriesTheBoundsThatSayWhatItMeans(t *testing.T) {
	st, ctx := ledgerStore(t)
	st.expectedRunGapCap = 2
	proj := seedLedgerProject(t, st, ctx, "bounds")
	m := ledgerMonitor(t, st, ctx, proj, "bounded", 60)

	now := time.Now().UTC().Truncate(time.Second)
	backdateSchedule(t, st, ctx, m.ID, now.Add(-10*time.Minute))
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: now.Add(-10 * time.Minute),
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})

	page, err := st.ListExpectedRuns(ctx, ExpectedRunQuery{
		ProjectID: proj, MonitorID: m.ID, From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !page.HasLedgerFrom || page.LedgerFrom.IsZero() {
		t.Fatal("the page carries no ledger_from")
	}
	if page.GapTruncatedBefore == nil {
		t.Fatal("the capped gap wrote a fence and the page did not carry it: a caller cannot then " +
			"tell an unanswerable span from one nothing was due in")
	}
	// An EMPTY page carries them too — that is the case they exist for.
	empty, err := st.ListExpectedRuns(ctx, ExpectedRunQuery{
		ProjectID: proj, MonitorID: m.ID,
		From: now.Add(-72 * time.Hour), To: now.Add(-71 * time.Hour), Limit: 50})
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if len(empty.Windows) != 0 {
		t.Fatalf("the range holds %d windows; the fixture is not the empty case", len(empty.Windows))
	}
	if !empty.HasLedgerFrom {
		t.Fatal("an EMPTY page carried no ledger_from — which is exactly the page where a caller " +
			"would otherwise conclude that nothing was due")
	}
	if empty.NextCursor != "" {
		t.Errorf("an empty page carried a cursor %q; null on the last page is the convention", empty.NextCursor)
	}
}

// §11's measurement: the ratio is UNDEFINED with no updates, and the gauge is then not published.
//
// A gauge reporting 0 or 1 for "no data" is a lie in whichever direction happens to be convenient.
// The two INPUTS are still exported: "nothing has updated yet" is a true thing to say, and the gate
// reads it as "not enough sample". They are gauges rather than `_total` counters — the exposition
// side is asserted by `TestTheLedgerFamiliesDeclareTheTypesTheyActuallyAre` in `internal/metrics`,
// because this test covers the SAMPLER and nothing here can see what a scrape contains.
func TestTheHOTRatioIsUndefinedUntilSomethingHasUpdated(t *testing.T) {
	st, ctx := ledgerStore(t)
	stat, err := st.ExpectedRunHOTRatio(ctx)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if stat.Updates == 0 && stat.Defined {
		t.Fatal("the ratio is DEFINED with a zero denominator")
	}
	if stat.Updates > 0 && !stat.Defined {
		t.Fatalf("the ratio is undefined with %d updates", stat.Updates)
	}

	// Now update rows and sample again. `pg_stat_user_tables` is refreshed asynchronously, so this
	// asserts the SHAPE — a defined ratio inside [0,1] once updates exist — rather than a specific
	// value, which would be a test of PostgreSQL's statistics collector rather than of this code.
	proj := seedLedgerProject(t, st, ctx, "hotratio")
	m := ledgerMonitor(t, st, ctx, proj, "updated", 60)
	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-time.Minute)
	backdateSchedule(t, st, ctx, m.ID, due)
	mustAdvance(t, st, ctx, now, ExpectationAdvance{
		MonitorID: m.ID, JobID: ledgerJobA, NextDue: now.Add(time.Minute), IntervalInForce: 60,
		CarrierGeneration: domain.LedgerMinCarrier, ExpectedDue: due,
		ExpectedRevision: m.ExecutionRevision, Region: m.Region})
	if _, err := st.RecordRunClaim(ctx, claimHeartbeat(m, due, ledgerJobA, now)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	after, err := st.ExpectedRunHOTRatio(ctx)
	if err != nil {
		t.Fatalf("sample after an update: %v", err)
	}
	if after.Defined && (after.Ratio() < 0 || after.Ratio() > 1) {
		t.Fatalf("the ratio is %f, outside [0,1]", after.Ratio())
	}
	if after.HOTUpdates > after.Updates {
		t.Fatalf("HOT updates (%d) exceed all updates (%d)", after.HOTUpdates, after.Updates)
	}
}

func encodeBadVersionCursorForTest(at time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v2:" + at.Format(time.RFC3339Nano)))
}

func encodeBadInstantCursorForTest() string {
	return base64.RawURLEncoding.EncodeToString([]byte(expectedRunCursorVersion + ":not-a-time"))
}
