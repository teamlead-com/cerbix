package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// iter-0201 — the forward pass must keep advancing a service with a LONG era.
//
// Observed on a deployed instance: every service's watermark stopped on 2026-09-24 and every slice
// since then failed with "store: compute sealed_through: canceling statement due to statement
// timeout". The forward pass materializes buckets until only the commit reserve (60 ms) is left,
// then recomputes the watermark — and the recompute walked EVERY bucket of the era with a window
// function (74 ms for a 52 000-bucket era there). Past that size the recompute never fits, the whole
// transaction rolls back, nothing is ever committed, and the era never shrinks: a permanent stall.
// The forward driver always claims the most-behind service, so one stalled service starved the rest.
//
// The fixture plants a 200-day era of sealed facts (288 000 buckets) behind the watermark — far past
// anything the old recompute could do inside the reserve on any machine — and drives the scheduler's
// own slice at the production 250 ms. The watermark must move.
func TestForwardAdvanceKeepsUpWithALongEra(t *testing.T) {
	st, ctx := declStore(t)
	// Adopted three days back so every bucket the forward pass walks has an epoch, and the
	// cursor below sits two days behind: like the deployed instance, the work loop always runs
	// until only the commit reserve is left, which is the budget the recompute then gets.
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-72 * time.Hour)}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	rf := reportFixture{declFixture: f, epochID: epochID}

	through := domain.FloorToBucket(time.Now().UTC().Add(-48 * time.Hour))
	era := through.Add(-200 * 24 * time.Hour)
	plantRange(t, st, ctx, rf, epochID, era, through, minute, 0, 0, 0, "sealed")
	setWatermark(t, st, ctx, rf, era, through)

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader session: ok=%v err=%v", ok, err)
	}
	defer ls.Release()

	for i := 0; i < 5; i++ {
		if _, err := ls.RunServiceSlice(ctx, time.Now().Add(250*time.Millisecond)); err != nil {
			t.Fatalf("slice %d failed: %v — the watermark recompute must not depend on the era's length", i, err)
		}
	}
	if got := sealedThrough(t, st, ctx, f.serviceID); got == nil || !got.After(through) {
		t.Fatalf("sealed_through = %v after five production slices, want past %s", got, through)
	}
}

// The incremental recompute starts at the current watermark, so it must still STOP at the first
// hole or unsealed bucket it meets ahead of it — the contiguity definition (§10.5) is unchanged.
func TestIncrementalWatermarkStillStopsAtAHoleAhead(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	rf := reportFixture{declFixture: f, epochID: epochID}

	base := domain.FloorToBucket(time.Now().UTC().Add(-3 * time.Hour))
	plantRange(t, st, ctx, rf, epochID, base, base.Add(60*time.Minute), minute, 0, 0, 0, "sealed")
	setWatermark(t, st, ctx, rf, base, base.Add(30*time.Minute))
	// A hole at +40m.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, base.Add(40*time.Minute)); err != nil {
		t.Fatalf("make hole: %v", err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if err := advanceSealedThrough(ctx, tx, f.serviceID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	var got time.Time
	if err := tx.QueryRow(ctx, `SELECT sealed_through FROM service_materialization WHERE service_id=$1`,
		f.serviceID).Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := base.Add(40 * time.Minute); !got.Equal(want) {
		t.Fatalf("sealed_through = %s, want %s — the start of the hole", got, want)
	}
}

// The unsealed stop rule, with no hole in front of it: a provisional bucket ahead of the watermark
// stops the walk at its own start (iter-0201 review, Minor — the hole test above cannot reach this
// rule because its hole stops the walk first).
func TestIncrementalWatermarkStopsAtAnUnsealedBucket(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	rf := reportFixture{declFixture: f, epochID: epochID}

	base := domain.FloorToBucket(time.Now().UTC().Add(-3 * time.Hour))
	plantRange(t, st, ctx, rf, epochID, base, base.Add(60*time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, rf, epochID, base.Add(45*time.Minute), base.Add(46*time.Minute), minute, 0, 0, 0, "provisional")
	setWatermark(t, st, ctx, rf, base, base.Add(30*time.Minute))

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if err := advanceSealedThrough(ctx, tx, f.serviceID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	var got time.Time
	if err := tx.QueryRow(ctx, `SELECT sealed_through FROM service_materialization WHERE service_id=$1`,
		f.serviceID).Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := base.Add(45 * time.Minute); !got.Equal(want) {
		t.Fatalf("sealed_through = %s, want %s — the start of the unsealed bucket", got, want)
	}
}

// iter-0201 review, Important (the reviewer's reproducer, made permanent). The bounded walk lets the
// watermark trail SEALED facts while it catches up after a retraction. A late heartbeat for such a
// fact — sealed, but ahead of the lagging watermark — must still queue its late_data repair: the fact
// is the authority on "sealed" (§10.4), not the watermark. Otherwise the watermark later catches up
// over a fact the late DOWN contradicts, and nothing ever corrects it.
func TestLateArrivalDuringWatermarkCatchUpIsRepaired(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	era := domain.FloorToBucket(time.Now().UTC().Add(-4 * 24 * time.Hour))
	_, epoch, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: era})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rf := reportFixture{declFixture: f, epochID: epoch.ID}
	through := domain.FloorToBucket(time.Now().UTC().Add(-10 * time.Minute))
	bucket := through.Add(-10 * time.Minute)
	// The target minute really is GOOD throughout before the late DOWN arrives.
	beat(t, st, ctx, f.http, era.Add(-30*time.Second), true)
	beat(t, st, ctx, f.http, bucket.Add(-30*time.Second), true)
	beat(t, st, ctx, f.http, bucket.Add(time.Minute), true)
	plantRange(t, st, ctx, rf, epoch.ID, era, through, minute, 0, 0, 0, "sealed")
	setWatermark(t, st, ctx, rf, era, through)

	// A one-minute retroactive exclusion, four days back, through the real preview/confirm path.
	p, err := st.PreviewMaintenanceMutation(ctx, f.projectID, f.http, era, era.Add(time.Minute), 30*24*time.Hour, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http, StartsAt: era, EndsAt: era.Add(time.Minute), Reason: "retroactive exclusion",
	}, p.ID, 30*24*time.Hour); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got := sealedThrough(t, st, ctx, f.serviceID); got == nil || !got.Equal(era) {
		t.Fatalf("retraction not reached: got %v want %v", got, era)
	}
	drainRepair(t, st, ctx)
	var unfinished int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_repair_ranges WHERE service_id=$1 AND state <> 'complete'`, f.serviceID).Scan(&unfinished); err != nil {
		t.Fatal(err)
	}
	if unfinished != 0 {
		t.Fatalf("maintenance did not complete: %d unfinished", unfinished)
	}
	t.Logf("completed maintenance: watermark=%v; late bucket=%v; old watermark=%v", sealedThrough(t, st, ctx, f.serviceID), bucket, through)

	inserted, _, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{{MonitorID: f.http, Ts: bucket.Add(30 * time.Second), Up: false}})
	if err != nil || inserted != 1 {
		t.Fatalf("historical insert: inserted=%d err=%v", inserted, err)
	}
	var repairs, arrivals int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_repair_ranges WHERE service_id=$1 AND reason='late_data' AND state='pending'`, f.serviceID).Scan(&repairs); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT arrivals FROM service_late_arrivals WHERE service_id=$1 AND bucket_start=$2 AND monitor_id=$3`, f.serviceID, bucket, f.http).Scan(&arrivals); err != nil {
		t.Fatal(err)
	}
	t.Logf("late arrivals=%d pending late_data repairs=%d", arrivals, repairs)
	drainRepair(t, st, ctx)
	// Simulate the subsequent sealing transactions catching the watermark back up. They
	// must not be mistaken for a recompute of the already-sealed, now contradicted fact.
	for i := 0; i < 5; i++ {
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := advanceSealedThrough(ctx, tx, f.serviceID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := sealedThrough(t, st, ctx, f.serviceID); got == nil || !got.Equal(through) {
		t.Fatalf("catch-up: got %v want %v", got, through)
	}
	after, ok := readFact(t, st, ctx, f.serviceID, bucket)
	if !ok || after.bad != 30_000_000 {
		t.Fatalf("late DOWN was not repaired after watermark caught up: fact=%+v exists=%v; want bad_us=30000000", after, ok)
	}
}
