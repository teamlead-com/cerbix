package store

import (
	"context"
	"testing"
	"time"
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
	startMaterialization(t, st, ctx, f, base)
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
	startMaterialization(t, st, ctx, f, base)

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
	startMaterialization(t, st, ctx, f, base)

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
