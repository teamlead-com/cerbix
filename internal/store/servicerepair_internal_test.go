package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func rangeRows(t *testing.T, st *Store, ctx context.Context, serviceID string) []RepairRange {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT id, service_id, project_id, range_start, range_end, reason,
		        COALESCE(cursor_at, range_start), maintenance_generation, attempts
		   FROM service_repair_ranges WHERE service_id=$1 ORDER BY range_start`, serviceID)
	if err != nil {
		t.Fatalf("read ranges: %v", err)
	}
	defer rows.Close()
	var out []RepairRange
	for rows.Next() {
		var r RepairRange
		var reason string
		if err := rows.Scan(&r.ID, &r.ServiceID, &r.ProjectID, &r.From, &r.To, &reason,
			&r.Cursor, &r.Generation, &r.Attempts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.Reason = RepairReason(reason)
		out = append(out, r)
	}
	return out
}

func rangeState(t *testing.T, st *Store, ctx context.Context, id string) (string, time.Time) {
	t.Helper()
	var state string
	var cursor time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT state, COALESCE(cursor_at, range_start) FROM service_repair_ranges WHERE id=$1`,
		id).Scan(&state, &cursor); err != nil {
		t.Fatalf("range state: %v", err)
	}
	return state, cursor
}

// Overlapping and adjacent ranges of the same reason coalesce, and the merged row spans the
// UNION exactly. Losing an edge here means losing buckets nothing will ever recompute.
func TestEnqueueCoalescesPreservingTheUnion(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Overlapping.
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base.Add(30*time.Minute), base.Add(90*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("second: %v", err)
	}
	// Exactly adjacent — abutting ranges must merge too, or the seam becomes two claims.
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base.Add(90*time.Minute), base.Add(2*time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("third: %v", err)
	}

	rs := rangeRows(t, st, ctx, f.serviceID)
	if len(rs) != 1 {
		t.Fatalf("%d ranges, want 1 after coalescing: %+v", len(rs), rs)
	}
	if !rs[0].From.Equal(base) || !rs[0].To.Equal(base.Add(2*time.Hour)) {
		t.Errorf("union = [%s, %s), want [%s, %s)", rs[0].From, rs[0].To, base, base.Add(2*time.Hour))
	}
}

// Ranges of DIFFERENT reasons stay separate. Merging them would erase the origin story of
// work that is about to change a number someone will ask about, and materialization is
// idempotent so the overlap costs nothing but a little work.
func TestDifferentReasonsDoNotCoalesce(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Hour), ReasonMaintenance); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if rs := rangeRows(t, st, ctx, f.serviceID); len(rs) != 2 {
		t.Errorf("%d ranges, want 2 — different reasons were merged", len(rs))
	}
}

// A RUNNING range is never absorbed: it carries a cursor, and widening it under a worker
// would either replay finished buckets or skip unfinished ones.
func TestRunningRangeIsNotAbsorbed(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := st.ClaimRepairRange(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(2*time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	rs := rangeRows(t, st, ctx, f.serviceID)
	if len(rs) != 2 {
		t.Fatalf("%d ranges, want 2: the running range was absorbed", len(rs))
	}
	state, _ := rangeState(t, st, ctx, claimed.ID)
	if state != "running" {
		t.Errorf("the claimed range is %q, want running", state)
	}
}

// A claimed range runs to completion and leaves facts behind it.
func TestRunRepairRangeMaterializesAndCompletes(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	for i := 0; i < 5; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(5*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r, ok, err := st.ClaimRepairRange(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := st.RunRepairRange(ctx, r, time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("run: %v", err)
	}
	state, cursor := rangeState(t, st, ctx, r.ID)
	if state != "complete" {
		t.Errorf("state = %q, want complete", state)
	}
	if !cursor.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("cursor = %s, want the range end %s", cursor, base.Add(5*time.Minute))
	}
	var facts int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets WHERE service_id=$1`, f.serviceID).Scan(&facts); err != nil {
		t.Fatalf("count facts: %v", err)
	}
	if facts != 5 {
		t.Errorf("%d facts, want 5", facts)
	}
}

// A range that runs out of slice goes back to pending WITH ITS CURSOR, so the next claim
// resumes rather than restarts. That is the whole reason work is a table and not a goroutine.
func TestRangeOutOfSliceResumesFromItsCursor(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(2*time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r, ok, err := st.ClaimRepairRange(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// A deadline already in the past: the very first check must release rather than run.
	if err := st.RunRepairRange(ctx, r, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("run: %v", err)
	}
	state, cursor := rangeState(t, st, ctx, r.ID)
	if state != "pending" {
		t.Fatalf("state = %q, want pending so the next claim picks it up", state)
	}
	if !cursor.Equal(base) {
		t.Errorf("cursor = %s, want the range start %s", cursor, base)
	}

	// A claim resumes from the cursor, and the second run finishes the whole range.
	r2, ok, err := st.ClaimRepairRange(ctx)
	if err != nil || !ok {
		t.Fatalf("second claim: %v ok=%v", err, ok)
	}
	if !r2.Cursor.Equal(cursor) {
		t.Errorf("the claim resumed at %s, want the stored cursor %s", r2.Cursor, cursor)
	}
	if err := st.RunRepairRange(ctx, r2, time.Now().Add(60*time.Second)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if state, _ := rangeState(t, st, ctx, r2.ID); state != "complete" {
		t.Errorf("state = %q, want complete", state)
	}
}

// A maintenance mutation landing mid-range makes the batch stale: it read one declaration
// and the world now has another. The range goes back to pending, re-armed at the CURRENT
// generation, rather than finishing on a stale reading.
func TestMaintenanceMutationSupersedesARunningBatch(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(2*time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r, ok, err := st.ClaimRepairRange(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}

	// Somebody mutates maintenance while the range is claimed.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO project_maintenance_generation (project_id, generation) VALUES ($1, 1)
		 ON CONFLICT (project_id) DO UPDATE SET generation = project_maintenance_generation.generation + 1`,
		f.projectID); err != nil {
		t.Fatalf("bump generation: %v", err)
	}

	if err := st.RunRepairRange(ctx, r, time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("run: %v", err)
	}
	state, _ := rangeState(t, st, ctx, r.ID)
	if state != "pending" {
		t.Fatalf("state = %q, want pending — a stale batch must not complete", state)
	}
	var generation int64
	var lastError string
	if err := st.pool.QueryRow(ctx,
		`SELECT maintenance_generation, last_error FROM service_repair_ranges WHERE id=$1`,
		r.ID).Scan(&generation, &lastError); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if generation != 1 {
		t.Errorf("the released range carries generation %d, want the current 1", generation)
	}
	if lastError == "" {
		t.Error("the release recorded no reason; a range that stopped for a reason should say so")
	}
}

// Two claimants take different rows rather than fighting over one, so a leader handover
// does not stall the queue.
func TestConcurrentClaimsTakeDifferentRanges(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-5 * time.Hour).Truncate(time.Minute)

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base.Add(2*time.Hour), base.Add(3*time.Hour), ReasonMaintenance); err != nil {
		t.Fatalf("b: %v", err)
	}
	first, ok, err := st.ClaimRepairRange(ctx)
	if err != nil || !ok {
		t.Fatalf("first claim: %v ok=%v", err, ok)
	}
	second, ok, err := st.ClaimRepairRange(ctx)
	if err != nil || !ok {
		t.Fatalf("second claim: %v ok=%v", err, ok)
	}
	if first.ID == second.ID {
		t.Error("two claims returned the same range")
	}
	third, ok, err := st.ClaimRepairRange(ctx)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if ok {
		t.Errorf("a third claim returned %s with nothing pending", third.ID)
	}
}

// The batch size shrinks when a slice is tight and grows when it is not. A fixed large batch
// under a tight deadline times out every slice and commits nothing — every bound respected,
// progress zero.
func TestBatchSizeAdapts(t *testing.T) {
	tight := adaptRepairBatch(60, 900*time.Millisecond, time.Second)
	if tight >= 60 {
		t.Errorf("a batch that overran its target did not shrink: %d", tight)
	}
	roomy := adaptRepairBatch(60, 10*time.Millisecond, time.Second)
	if roomy <= 60 {
		t.Errorf("a batch that finished comfortably did not grow: %d", roomy)
	}
	if floor := adaptRepairBatch(1, time.Second, time.Millisecond); floor < 1 {
		t.Errorf("the batch size fell below one bucket: %d", floor)
	}
	if ceiling := adaptRepairBatch(maxRepairBatch, time.Nanosecond, time.Hour); ceiling > maxRepairBatch {
		t.Errorf("the batch size exceeded its cap: %d", ceiling)
	}
}

// Backoff has a floor and a cap, so a persistent fault is neither a hot loop nor a range
// that waits forever.
func TestRepairBackoffIsBounded(t *testing.T) {
	if got := repairBackoff(1); got != 5*time.Second {
		t.Errorf("first backoff = %s, want the 5s floor", got)
	}
	if got := repairBackoff(20); got > 5*time.Minute {
		t.Errorf("backoff = %s, want the 5m cap", got)
	}
	if repairBackoff(3) <= repairBackoff(1) {
		t.Error("backoff does not grow with attempts")
	}
}

// A leader that dies holding a claim must not take the work with it.
//
// This is the failure the shipped code could not recover from: the claim committed
// `state='running'`, the claim query looked only at `pending`, and nothing ever reset it. The
// range — and the watermark hole it existed to fill — stayed there forever, silently, because
// nothing counts or alerts on running work.
func TestACrashedLeadersClaimIsReclaimedAfterItsLease(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Hour), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Claim on a dedicated connection, then destroy that backend — a leader losing its node.
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var pid int32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("pid: %v", err)
	}
	claimed, ok, err := st.claimRepairRangeOn(ctx, conn)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if _, err := st.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	// Make the dead connection notice before handing it back, so the pool destroys it
	// instead of lending the corpse to the next caller.
	_, _ = conn.Exec(ctx, `SELECT 1`)
	conn.Release()

	if state, _ := rangeState(t, st, ctx, claimed.ID); state != "running" {
		t.Fatalf("state = %q after the leader died, want running (the claim was committed)", state)
	}

	// Nobody may steal it while the lease is live — otherwise a slow but healthy leader has
	// its own range worked concurrently by a second one.
	if _, ok, err := st.claimRepairRangeOn(ctx, st.pool); err != nil {
		t.Fatalf("claim during lease: %v", err)
	} else if ok {
		t.Fatal("a live lease was stolen; two leaders would work the same range at once")
	}

	// Let the lease lapse. Moving the expiry into the past stands in for the clock — the
	// assertion is about what the claim query does with an expired lease, not about waiting.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_repair_ranges SET lease_expires_at = now() - interval '1 second' WHERE id=$1`,
		claimed.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	reclaimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !ok {
		t.Fatal("an expired claim was never reclaimed; the range is stranded forever")
	}
	if reclaimed.ID != claimed.ID {
		t.Errorf("reclaimed %s, want the stranded %s", reclaimed.ID, claimed.ID)
	}
	// It resumes from the durable cursor rather than restarting.
	if !reclaimed.Cursor.Equal(claimed.Cursor) {
		t.Errorf("resumed at %s, want the durable cursor %s", reclaimed.Cursor, claimed.Cursor)
	}
}

// A same-boundary declaration race must displace only ITS OWN work.
//
// The first implementation cancelled every pending range starting at or after the boundary,
// which meant an operator's admin recompute, a confirmed maintenance repair or an adoption
// backfill was silently discarded by an unrelated declaration write — and left no complaint,
// because a superseded range says nothing to anyone.
func TestASameBoundaryDeclarationDoesNotDiscardUnrelatedWork(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	// Work owned by nobody in the declaration axis, starting in the future so any boundary
	// this test writes lands at or before it.
	from := domain.CeilToBucket(time.Now().UTC().Add(2 * time.Minute))
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, from, from.Add(time.Hour), ReasonAdmin); err != nil {
		t.Fatalf("enqueue admin range: %v", err)
	}
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, from.Add(2*time.Hour), from.Add(3*time.Hour), ReasonMaintenance); err != nil {
		t.Fatalf("enqueue maintenance range: %v", err)
	}

	// Two declarations in quick succession: the second claims the same boundary and
	// supersedes the first.
	for rev := int64(1); rev <= 2; rev++ {
		if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
			Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
		}, rev, DeclarationOptions{CreatedBy: "op"}); err != nil {
			t.Fatalf("declaration %d: %v", rev, err)
		}
	}

	var alive int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_repair_ranges
		  WHERE service_id=$1 AND reason IN ('admin','maintenance') AND state='pending'`,
		f.serviceID).Scan(&alive); err != nil {
		t.Fatalf("count: %v", err)
	}
	if alive != 2 {
		t.Fatalf("%d of 2 unrelated ranges survived a declaration write; the rest were discarded with no record", alive)
	}
}

// A long repair under PRODUCTION-sized slices must make progress (iter-0198 re-review, Important 1).
// A late heartbeat for a monitor that then went silent repairs up to the watermark; if the first
// batch of every slice is a fixed hour of buckets (the former initial size) and that one atomic
// batch does not fit, the
// slice rolls back, the cursor never moves, and the next slice starts with the same batch again —
// every bound respected, progress zero (§10.10's livelock). A 5 ms trigger on every bucket write
// models a database where small batches fit and a 60-bucket one does not. drainRepair's generous
// deadline hides this, so the slices here are the scheduler's own 250 ms.
func TestLongRepairProgressesUnderProductionSlices(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	leaderSliceFor(t, st, ctx, 80)

	base := domain.FloorToBucket(time.Now().UTC().Add(-90 * time.Minute))
	if n, _, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{
		{MonitorID: f.http, Ts: base.Add(30 * time.Second), Up: true},
	}); err != nil || n != 1 {
		t.Fatalf("late backfill: n=%d err=%v", n, err)
	}
	var rid string
	var from, to time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT id, range_start, range_end FROM service_repair_ranges
		  WHERE service_id=$1 AND reason='late_data' AND state='pending'`,
		f.serviceID).Scan(&rid, &from, &to); err != nil {
		t.Fatalf("read range: %v", err)
	}
	if to.Sub(from) < time.Hour {
		t.Fatalf("test setup: range %s is shorter than the hour-sized batch that cannot fit", to.Sub(from))
	}

	if _, err := st.pool.Exec(ctx, `
		CREATE FUNCTION iter0198_slow_bucket() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.005); RETURN NEW; END $$;
		CREATE TRIGGER iter0198_slow_bucket BEFORE UPDATE ON service_reliability_buckets
		   FOR EACH ROW EXECUTE FUNCTION iter0198_slow_bucket()`); err != nil {
		t.Fatalf("slow-bucket trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := st.pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS iter0198_slow_bucket ON service_reliability_buckets;
			DROP FUNCTION IF EXISTS iter0198_slow_bucket()`); err != nil {
			t.Errorf("drop slow-bucket trigger: %v", err)
		}
	})

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader session: ok=%v err=%v", ok, err)
	}
	defer ls.Release()

	prev := from
	state := ""
	for i := 0; i < 12 && state != "complete"; i++ {
		if _, err := ls.RunServiceRepairSlice(ctx, time.Now().Add(250*time.Millisecond)); err != nil {
			t.Logf("slice %d: %v", i, err)
		}
		var cursor time.Time
		var attempts int
		if err := st.pool.QueryRow(ctx,
			`SELECT COALESCE(cursor_at, range_start), state, attempts FROM service_repair_ranges WHERE id=$1`,
			rid).Scan(&cursor, &state, &attempts); err != nil {
			t.Fatalf("read range: %v", err)
		}
		if attempts != 0 {
			t.Fatalf("slice %d counted a failure (attempts=%d): running out of slice is a stop, not an error", i, attempts)
		}
		if state != "complete" && !cursor.After(prev) {
			t.Fatalf("slice %d made no progress: cursor still %s of [%s, %s)", i, cursor, from, to)
		}
		prev = cursor
	}
	if state != "complete" {
		t.Fatalf("range not complete after 12 production slices (cursor %s of [%s, %s))", prev, from, to)
	}
	if fact, ok := readFact(t, st, ctx, f.serviceID, base.Add(time.Minute)); !ok || fact.good != domain.CanonicalBucket.Microseconds() {
		t.Errorf("first held minute after repair: %+v exists=%v, want GOOD", fact, ok)
	}
}

// lateRepairRange plants a sealed UNKNOWN stretch and one late GOOD 90 minutes back with no later
// heartbeat, so the late_data range runs to the watermark — long enough for several slices.
func lateRepairRange(t *testing.T) (*Store, context.Context, string, time.Time) {
	t.Helper()
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	leaderSliceFor(t, st, ctx, 80)
	base := domain.FloorToBucket(time.Now().UTC().Add(-90 * time.Minute))
	if n, _, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{
		{MonitorID: f.http, Ts: base.Add(30 * time.Second), Up: true},
	}); err != nil || n != 1 {
		t.Fatalf("late backfill: n=%d err=%v", n, err)
	}
	var rid string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_repair_ranges WHERE service_id=$1 AND reason='late_data' AND state='pending'`,
		f.serviceID).Scan(&rid); err != nil {
		t.Fatalf("read range: %v", err)
	}
	return st, ctx, rid, base
}

// bucketWriteTrigger runs `body` (PL/pgSQL) before every bucket UPDATE, and is dropped with a
// fresh context so a failing test cannot leave it behind on the shared database.
func bucketWriteTrigger(t *testing.T, st *Store, ctx context.Context, body string) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		CREATE FUNCTION iter0198_bucket_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN `+body+`; RETURN NEW; END $$;
		CREATE TRIGGER iter0198_bucket_write BEFORE UPDATE ON service_reliability_buckets
		   FOR EACH ROW EXECUTE FUNCTION iter0198_bucket_write()`); err != nil {
		t.Fatalf("bucket-write trigger: %v", err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := st.pool.Exec(clean, `
			DROP TRIGGER IF EXISTS iter0198_bucket_write ON service_reliability_buckets;
			DROP FUNCTION IF EXISTS iter0198_bucket_write()`); err != nil {
			t.Errorf("drop bucket-write trigger: %v", err)
		}
	})
}

func repairRangeState(t *testing.T, st *Store, ctx context.Context, rid string) (cursor time.Time, attempts int) {
	t.Helper()
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE(cursor_at, range_start), attempts FROM service_repair_ranges WHERE id=$1`,
		rid).Scan(&cursor, &attempts); err != nil {
		t.Fatalf("read range: %v", err)
	}
	return cursor, attempts
}

// A healthy bucket that fits a FRESH slice is not a fault because it did not fit the slice's TAIL.
// At 75 ms per bucket write the first bucket commits, the batch stays at one, and the next bucket
// meets what is left of the slice. That is the slice running out (iter-0198 re-review #2,
// Important): counting it would back the range off 5 s, 10 s, 20 s … after every slice that made
// progress. Scenario from the reviewer's reproducer.
func TestRepairTailTimeoutOnOneBucketIsNotAFault(t *testing.T) {
	st, ctx, rid, base := lateRepairRange(t)
	bucketWriteTrigger(t, st, ctx, `PERFORM pg_sleep(0.075)`)
	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader session: ok=%v err=%v", ok, err)
	}
	defer ls.Release()

	prev := base
	for i := 0; i < 3; i++ {
		if _, err := ls.RunServiceRepairSlice(ctx, time.Now().Add(250*time.Millisecond)); err != nil {
			t.Logf("slice %d: %v", i, err)
		}
		cursor, attempts := repairRangeState(t, st, ctx, rid)
		if !cursor.After(prev) {
			t.Fatalf("test setup: slice %d committed nothing — one 75 ms bucket must fit a fresh slice", i)
		}
		if attempts != 0 {
			t.Fatalf("slice %d: running out at the slice's tail counted as a fault (attempts=%d)", i, attempts)
		}
		prev = cursor
	}
}

// Deterministic guard for the classification itself, with no timing lottery about whether the
// budget runs out between statements or inside one: every bucket after the first sleeps 1 s, far
// past any slice. Slice 0 commits bucket 0, grows to a multi-bucket batch and times out → release,
// no failure. Slice 1 opens fresh at one bucket on that same bucket and still times out → that is
// the §10.10 fault, counted once.
//
// The slow statement bumps a SEQUENCE before sleeping, and nextval is not rolled back with the
// batch: the test proves slice 0 reached it — i.e. that its release came from a 57014 being
// remapped — instead of trusting that it did. On a database slow enough that slice 0 runs out
// BETWEEN statements (errSliceBudget, which releases on its own), the remap would go unexercised and
// the old version of this guard passed with the remap disabled (iter-0198 re-review #3); that case
// now fails as a setup error, never as a green result.
func TestRepairTimeoutIsAReleaseAfterProgressAndAFaultOnAFreshBucket(t *testing.T) {
	st, ctx, rid, base := lateRepairRange(t)
	if _, err := st.pool.Exec(ctx, `CREATE SEQUENCE iter0198_slow_reached`); err != nil {
		t.Fatalf("reach counter: %v", err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := st.pool.Exec(clean, `DROP SEQUENCE IF EXISTS iter0198_slow_reached`); err != nil {
			t.Errorf("drop reach counter: %v", err)
		}
	})
	reached := func() int64 {
		t.Helper()
		var n int64
		if err := st.pool.QueryRow(ctx,
			`SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM iter0198_slow_reached`).Scan(&n); err != nil {
			t.Fatalf("read reach counter: %v", err)
		}
		return n
	}
	// Registered after the sequence, so cleanup drops the trigger before the sequence it calls.
	bucketWriteTrigger(t, st, ctx, fmt.Sprintf(
		`IF NEW.bucket_start >= '%s'::timestamptz THEN PERFORM nextval('iter0198_slow_reached'); PERFORM pg_sleep(1); END IF`,
		base.Add(time.Minute).Format(time.RFC3339)))
	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader session: ok=%v err=%v", ok, err)
	}
	defer ls.Release()

	first := base.Add(time.Minute)
	if _, err := ls.RunServiceRepairSlice(ctx, time.Now().Add(250*time.Millisecond)); err != nil {
		t.Errorf("slice 0: a timeout after progress must release, got %v", err)
	}
	if n := reached(); n < 1 {
		t.Fatalf("test setup: slice 0 never reached the slow statement (it ran out between statements), so the 57014 remap was not exercised")
	}
	if cursor, attempts := repairRangeState(t, st, ctx, rid); !cursor.Equal(first) || attempts != 0 {
		t.Fatalf("slice 0: cursor=%s attempts=%d, want %s and 0", cursor, attempts, first)
	}

	before := reached()
	_, err = ls.RunServiceRepairSlice(ctx, time.Now().Add(250*time.Millisecond))
	if reached() <= before {
		t.Fatalf("test setup: slice 1 never reached the slow statement")
	}
	if !isStatementTimeout(err) {
		t.Errorf("slice 1: a bucket that does not fit a fresh slice must fail with 57014, got %v", err)
	}
	if cursor, attempts := repairRangeState(t, st, ctx, rid); !cursor.Equal(first) || attempts != 1 {
		t.Fatalf("slice 1: cursor=%s attempts=%d, want %s and 1", cursor, attempts, first)
	}
}

// lock_timeout (55P03) is deliberately not folded into the slice budget: shrinking or releasing does
// not help a lock wait, and the retry backoff is its answer.
func TestRepairLockTimeoutKeepsTheFailurePath(t *testing.T) {
	st, ctx, rid, base := lateRepairRange(t)
	bucketWriteTrigger(t, st, ctx, fmt.Sprintf(
		`IF NEW.bucket_start >= '%s'::timestamptz THEN RAISE EXCEPTION 'iter0198 forced lock timeout' USING ERRCODE = '55P03'; END IF`,
		base.Add(time.Minute).Format(time.RFC3339)))
	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader session: ok=%v err=%v", ok, err)
	}
	defer ls.Release()

	_, err = ls.RunServiceRepairSlice(ctx, time.Now().Add(250*time.Millisecond))
	if pgErrCode(err) != "55P03" {
		t.Errorf("slice error = %v, want the 55P03", err)
	}
	if _, attempts := repairRangeState(t, st, ctx, rid); attempts != 1 {
		t.Fatalf("attempts=%d, want 1: a lock timeout is a range failure", attempts)
	}
}
