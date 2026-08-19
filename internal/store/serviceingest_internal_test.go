package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// adoptedService declares a service RETROACTIVELY — the first-adoption case — so that
// heartbeats timestamped in the recent past fall inside its validity. An ordinary
// declaration is prospective and governs only from the next boundary, which means a
// just-created service cannot yet own any heartbeat that already exists.
func adoptedService(t *testing.T, st *Store, ctx context.Context, sli ...string) declFixture {
	t.Helper()
	f := seedDeclaration(t, st, ctx)
	if len(sli) == 0 {
		sli = []string{f.http}
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: sli,
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	return f
}

func ingestGeneration(t *testing.T, st *Store, ctx context.Context, serviceID string, bucket time.Time) int64 {
	t.Helper()
	var gen int64
	err := st.pool.QueryRow(ctx,
		`SELECT ingest_generation FROM service_bucket_ingest WHERE service_id=$1 AND bucket_start=$2`,
		serviceID, bucket).Scan(&gen)
	if noRows(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read ingest generation: %v", err)
	}
	return gen
}

// beat records a scheduled heartbeat carrying the monitor's CURRENT execution revision, as
// a real prober does — the ingest path rejects a result whose revision no longer matches.
func beat(t *testing.T, st *Store, ctx context.Context, monitorID string, ts time.Time, up bool) ResultOutcome {
	t.Helper()
	m, err := st.GetMonitor(ctx, monitorID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	out, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: monitorID, Ts: ts, Up: up, ExecutionRevision: m.ExecutionRevision,
	})
	if err != nil {
		t.Fatalf("record scheduled result: %v", err)
	}
	return out
}

func bucketOf(t *testing.T, st *Store, ctx context.Context, ts time.Time) time.Time {
	t.Helper()
	var b time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT date_bin('1 minute', $1::timestamptz, 'epoch'::timestamptz)`, ts).Scan(&b); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return b
}

// A heartbeat for a declared SLI member marks the bucket it belongs to.
func TestInsertedHeartbeatMarksItsBucket(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	ts := time.Now().UTC().Add(-30 * time.Second)
	beat(t, st, ctx, f.http, ts, true)
	bucket := bucketOf(t, st, ctx, ts)
	if got := ingestGeneration(t, st, ctx, f.serviceID, bucket); got != 1 {
		t.Fatalf("ingest generation = %d, want 1", got)
	}
}

// A redelivery of an already-counted heartbeat inserts nothing, so it must mark nothing.
// Marking on DELIVERY rather than on insertion would file a duplicate as arriving data — in
// the exact surface built to explain a disagreement between a sealed fact and raw history.
func TestDuplicateHeartbeatIsAFullNoOp(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	ts := time.Now().UTC().Add(-30 * time.Second)
	beat(t, st, ctx, f.http, ts, true)
	bucket := bucketOf(t, st, ctx, ts)
	first := ingestGeneration(t, st, ctx, f.serviceID, bucket)

	out := beat(t, st, ctx, f.http, ts, true)
	if out.Reason != ReasonDuplicate {
		t.Fatalf("the redelivery was not treated as a duplicate: %+v", out)
	}
	if got := ingestGeneration(t, st, ctx, f.serviceID, bucket); got != first {
		t.Errorf("a duplicate moved the ingest generation %d -> %d", first, got)
	}
}

// Membership is resolved AS OF the heartbeat's own bucket. A member removed from the SLI
// afterwards still routes an old heartbeat, because the fact for that bucket was produced by
// an epoch that contained it.
func TestHistoricalHeartbeatUsesMembershipAsOfItsBucket(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx) // revision 1, retroactive: sli = [http]

	// A heartbeat from before the removal, PINNED to the bucket's interior: this test sends a
	// second beat at old+1s and asserts on old's bucket, so old must sit far enough from the
	// minute boundary that both instants share it — a raw now()-10m flaked once a day when
	// the wall clock ran within a second of :59.
	old := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute).Add(10 * time.Second)
	beat(t, st, ctx, f.http, old, true)
	oldBucket := bucketOf(t, st, ctx, old)
	before := ingestGeneration(t, st, ctx, f.serviceID, oldBucket)
	if before == 0 {
		t.Fatal("the first heartbeat did not mark its bucket")
	}

	// Revision 2 drops it from the SLI, effective from the NEXT boundary.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.redis},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("revision 2: %v", err)
	}

	// A second heartbeat for the SAME old bucket still belongs to the service: revision 1
	// governs that instant, and it declared this monitor.
	beat(t, st, ctx, f.http, old.Add(time.Second), false)
	if got := ingestGeneration(t, st, ctx, f.serviceID, oldBucket); got != before+1 {
		t.Errorf("a heartbeat for a bucket the member DID belong to was ignored (%d -> %d)", before, got)
	}
}

// ...and the mirror: a member added afterwards must not dirty buckets in which it was not
// yet a member.
func TestHeartbeatBeforeMembershipDoesNotMark(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx) // sli = [http]; redis is context only

	// Pinned inside the bucket for the same reason as the membership test above: the second
	// beat at old+1s must land in old's own bucket for the assertion to mean anything.
	old := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute).Add(10 * time.Second)
	beat(t, st, ctx, f.redis, old, true)
	oldBucket := bucketOf(t, st, ctx, old)
	if got := ingestGeneration(t, st, ctx, f.serviceID, oldBucket); got != 0 {
		t.Fatalf("a diagnostic-only monitor marked a bucket (generation %d)", got)
	}

	// Promote redis into the SLI now.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http, f.redis},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("revision 2: %v", err)
	}
	// A heartbeat for that OLD bucket still must not mark it: redis was not a member then.
	beat(t, st, ctx, f.redis, old.Add(time.Second), true)
	if got := ingestGeneration(t, st, ctx, f.serviceID, oldBucket); got != 0 {
		t.Errorf("a heartbeat dirtied a bucket in which its monitor was not yet a member (generation %d)", got)
	}
}

// A monitor in no service's SLI writes nothing at all. This is what makes "zero services
// costs nothing" a property rather than a claim.
func TestHeartbeatForAnUnclaimedMonitorWritesNothing(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx) // no declaration written

	ts := time.Now().UTC().Add(-30 * time.Second)
	beat(t, st, ctx, f.http, ts, true)
	var rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_bucket_ingest`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d ingest rows for a monitor no service declares", rows)
	}
}

// A heartbeat landing in an already-SEALED bucket is recorded as a late arrival instead of
// dirtying it, so a sealed fact disagreeing with raw history always has a stored explanation
// — including months later, when the raw rows are gone.
func TestHeartbeatIntoASealedBucketBecomesALateArrival(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	// Pinned inside the bucket: the second late beat below lands at ts+1s and must aggregate
	// into THIS sealed bucket, not spill into an unsealed neighbor at a minute boundary.
	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute).Add(10 * time.Second)
	bucket := bucketOf(t, st, ctx, ts)

	// Materialize and seal that bucket by hand; the sealing pass itself is a later slice.
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id=$1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us,
		    unknown_us, health_unknown_us, state, sealed_at)
		 VALUES ($1,$2,$3,$4,60000000,60000000,60000000,'sealed', now())`,
		f.serviceID, f.projectID, epochID, bucket); err != nil {
		t.Fatalf("seal: %v", err)
	}

	beat(t, st, ctx, f.http, ts, true)

	var arrivals, overflow int64
	var examples string
	if err := st.pool.QueryRow(ctx,
		`SELECT arrivals, overflow, examples::text FROM service_late_arrivals
		  WHERE service_id=$1 AND bucket_start=$2 AND monitor_id=$3`,
		f.serviceID, bucket, f.http).Scan(&arrivals, &overflow, &examples); err != nil {
		t.Fatalf("late arrival not recorded: %v", err)
	}
	if arrivals != 1 {
		t.Errorf("arrivals = %d, want 1", arrivals)
	}

	// A SECOND late heartbeat aggregates into the same row rather than creating another:
	// one historical batch after a seal must not become millions of retained rows.
	beat(t, st, ctx, f.http, ts.Add(time.Second), false)
	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_late_arrivals WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, bucket).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d late-arrival rows for one (service, bucket, monitor)", rows)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT arrivals FROM service_late_arrivals WHERE service_id=$1 AND bucket_start=$2 AND monitor_id=$3`,
		f.serviceID, bucket, f.http).Scan(&arrivals); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if arrivals != 2 {
		t.Errorf("arrivals = %d, want 2 — the second late heartbeat did not aggregate", arrivals)
	}

	// ...and the sealed fact itself is untouched.
	var state string
	if err := st.pool.QueryRow(ctx,
		`SELECT state FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, bucket).Scan(&state); err != nil {
		t.Fatalf("read fact: %v", err)
	}
	if state != "sealed" {
		t.Errorf("the sealed fact changed to %q; late data is recorded, never counted", state)
	}
}

// The push path never checked whether its insert actually happened. A second ping at the
// same instant is one observation, and it must not mark anything twice.
func TestDuplicatePushPingDoesNotMarkTwice(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	push, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: "checkout-cron", Type: domain.MonitorPush,
		IntervalSeconds: 60, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("push monitor: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{push.ID}, SLI: []string{push.ID},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("declaration: %v", err)
	}

	at := time.Now().UTC().Add(-30 * time.Second)
	if _, err := st.RecordPushResult(ctx, push.ID, true, "ok", at, at); err != nil {
		t.Fatalf("first ping: %v", err)
	}
	bucket := bucketOf(t, st, ctx, at)
	first := ingestGeneration(t, st, ctx, f.serviceID, bucket)
	if first == 0 {
		t.Fatal("the push ping did not mark its bucket")
	}
	if _, err := st.RecordPushResult(ctx, push.ID, true, "ok", at, at); err != nil {
		t.Fatalf("duplicate ping: %v", err)
	}
	if got := ingestGeneration(t, st, ctx, f.serviceID, bucket); got != first {
		t.Errorf("a duplicate ping moved the ingest generation %d -> %d", first, got)
	}
}

// Invariant 37 (§10.4, §10.10): the late-arrival record is BOUNDED — at most `MaxLateExamples`
// example timestamps per aggregated row, with an overflow counter for everything beyond it.
//
// The aggregation itself is proven above; this is the bound. It matters because the pathological
// input is ordinary: one historical backfill of a month of heartbeats lands thousands of late
// arrivals in the same bucket, and a record that kept an example for each would turn "here is why
// the sealed fact disagrees with raw history" into a second, unbounded copy of that history. The
// honest alternative to keeping everything is to keep a few and SAY how many did not fit.
func TestLateArrivalExamplesAreBoundedAndCountTheOverflow(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute).Add(5 * time.Second)
	bucket := bucketOf(t, st, ctx, ts)

	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id=$1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us,
		    unknown_us, health_unknown_us, state, sealed_at)
		 VALUES ($1,$2,$3,$4,60000000,60000000,60000000,'sealed', now())`,
		f.serviceID, f.projectID, epochID, bucket); err != nil {
		t.Fatalf("seal: %v", err)
	}

	const beats = MaxLateExamples + 2
	for i := 0; i < beats; i++ {
		beat(t, st, ctx, f.http, ts.Add(time.Duration(i)*time.Second), i%2 == 0)
	}

	var arrivals, overflow, examples int64
	if err := st.pool.QueryRow(ctx,
		`SELECT arrivals, overflow, jsonb_array_length(examples) FROM service_late_arrivals
		  WHERE service_id=$1 AND bucket_start=$2 AND monitor_id=$3`,
		f.serviceID, bucket, f.http).Scan(&arrivals, &overflow, &examples); err != nil {
		t.Fatalf("late arrival not recorded: %v", err)
	}
	if arrivals != beats {
		t.Fatalf("arrivals = %d, want %d — the count is the evidence that nothing was dropped silently",
			arrivals, beats)
	}
	if examples != int64(MaxLateExamples) {
		t.Errorf("examples = %d, want the bound %d — an unbounded example list turns one late backfill "+
			"into a second copy of raw history (invariant 37)", examples, MaxLateExamples)
	}
	if want := int64(beats - MaxLateExamples); overflow != want {
		t.Errorf("overflow = %d, want %d — what did not fit must be COUNTED, so a reader can tell a "+
			"bounded record from a complete one (invariant 37)", overflow, want)
	}
}

// Invariant 27 (§10.6): heartbeat retention prunes RAW heartbeats only. Derived facts,
// late-arrival records and provenance are not pruned by it.
//
// This is what makes a sealed fact durable evidence: the raw rows behind it are expected to go, and
// the fact plus its provenance must outlive them — otherwise "availability last quarter" quietly
// becomes unanswerable at exactly the retention horizon, and the answer would depend on when it was
// asked. A purge that ever learned about the service tables would also make retention a silent
// rewriter of history rather than a cleaner of raw data.
func TestHeartbeatRetentionLeavesDerivedFactsAndTheirProvenance(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute).Add(5 * time.Second)
	bucket := bucketOf(t, st, ctx, ts)
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id=$1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us,
		    good_us, healthy_us, state, sealed_at, provenance)
		 VALUES ($1,$2,$3,$4,60000000,60000000,60000000,'sealed', now(),
		         '{"declared":1,"bad":[{"monitor_id":"m","reason":"probe_error"}]}'::jsonb)`,
		f.serviceID, f.projectID, epochID, bucket); err != nil {
		t.Fatalf("seal: %v", err)
	}
	// A late arrival for the same bucket: the second thing retention must not touch.
	beat(t, st, ctx, f.http, ts, false)

	// Purge everything: the cutoff is now, so no raw heartbeat is old enough to keep.
	if _, err := st.PurgeOldHeartbeats(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var state, provenance string
	if err := st.pool.QueryRow(ctx,
		`SELECT state, provenance::text FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start=$2`, f.serviceID, bucket).Scan(&state, &provenance); err != nil {
		t.Fatalf("the derived fact did not survive heartbeat retention: %v (invariant 27)", err)
	}
	if state != "sealed" || !strings.Contains(provenance, "probe_error") {
		t.Errorf("fact after purge: state=%q provenance=%s — retention must prune RAW heartbeats only, "+
			"so a sealed fact stays explainable after its raw rows are gone (invariant 27)", state, provenance)
	}
	var lateRows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_late_arrivals WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, bucket).Scan(&lateRows); err != nil {
		t.Fatalf("count late arrivals: %v", err)
	}
	if lateRows != 1 {
		t.Errorf("late-arrival rows after purge = %d, want 1 — the record that EXPLAINS a disagreement "+
			"must outlive the raw rows it is about (invariant 27)", lateRows)
	}
}
