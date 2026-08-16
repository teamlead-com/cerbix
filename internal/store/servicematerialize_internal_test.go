package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type factRow struct {
	epochID                                string
	good, bad, unknown, excluded           int64
	healthy, degraded, down, healthUnknown int64
	state                                  string
	sealedGeneration                       *int64
}

func readFact(t *testing.T, st *Store, ctx context.Context, serviceID string, bucket time.Time) (factRow, bool) {
	t.Helper()
	var f factRow
	err := st.pool.QueryRow(ctx,
		`SELECT epoch_id, good_us, bad_us, unknown_us, excluded_us,
		        healthy_us, degraded_us, down_us, health_unknown_us, state, sealed_ingest_generation
		   FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		serviceID, bucket).Scan(&f.epochID, &f.good, &f.bad, &f.unknown, &f.excluded,
		&f.healthy, &f.degraded, &f.down, &f.healthUnknown, &f.state, &f.sealedGeneration)
	if noRows(err) {
		return factRow{}, false
	}
	if err != nil {
		t.Fatalf("read fact: %v", err)
	}
	return f, true
}

// materializeFrom PINS where the driver starts for a fixture that needs a specific instant.
//
// It no longer creates the row: the DECLARATION does that now, in production. This helper
// used to be the only thing that ever created it, which is exactly how a subsystem that
// produced nothing outside tests passed its whole suite.
func materializeFrom(t *testing.T, st *Store, ctx context.Context, f declFixture, from time.Time) {
	t.Helper()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_materialization
		   (service_id, project_id, materialization_start, materialized_through, era_start)
		 VALUES ($1,$2,$3,$3,$3)
		 ON CONFLICT (service_id) DO UPDATE
		    SET materialization_start = EXCLUDED.materialization_start,
		        materialized_through  = EXCLUDED.materialized_through,
		        era_start             = EXCLUDED.era_start`,
		f.serviceID, f.projectID, from); err != nil {
		t.Fatalf("materialization row: %v", err)
	}
}

func sealedThrough(t *testing.T, st *Store, ctx context.Context, serviceID string) *time.Time {
	t.Helper()
	var through *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT sealed_through FROM service_materialization WHERE service_id=$1`, serviceID).Scan(&through); err != nil {
		t.Fatalf("sealed_through: %v", err)
	}
	return through
}

// A bucket with an UP observation in force throughout is GOOD for its whole length, on both
// axes, and the conservation CHECK in the schema agrees.
func TestMaterializeWritesAConservingFact(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	beat(t, st, ctx, f.http, base.Add(-5*time.Second), true)

	n, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if n != 1 {
		t.Fatalf("materialized %d buckets, want 1", n)
	}
	fact, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("no fact was written")
	}
	if fact.good != 60_000_000 {
		t.Errorf("good = %dµs, want a whole minute", fact.good)
	}
	if fact.healthy != 60_000_000 {
		t.Errorf("healthy = %dµs, want a whole minute — the health axis is stored too", fact.healthy)
	}
	if fact.good+fact.bad+fact.unknown+fact.excluded != 60_000_000 {
		t.Error("the availability axis does not account for the whole bucket")
	}
}

// A member GOOD for part of a bucket and UNKNOWN for the rest contributes both, exactly —
// end to end, through the store rather than only in the pure reducer.
func TestMaterializeSplitsAPartiallyStaleBucket(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	// The default freshness floor is 90s, so an observation 70s before the bucket opens
	// decays 20s into it.
	beat(t, st, ctx, f.http, base.Add(-70*time.Second), true)

	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fact, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("no fact")
	}
	if fact.good != 20_000_000 || fact.unknown != 40_000_000 {
		t.Errorf("good=%dµs unknown=%dµs, want 20s and 40s", fact.good, fact.unknown)
	}
}

// A bucket whose grace has elapsed is SEALED and carries the ingest generation observed
// under the row lock — that generation is what makes the seal a compare-and-swap rather
// than a hopeful read.
func TestMaturedBucketIsSealedWithItsGeneration(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	beat(t, st, ctx, f.http, base.Add(10*time.Second), true)

	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fact, _ := readFact(t, st, ctx, f.serviceID, base)
	if fact.state != "sealed" {
		t.Fatalf("state = %q, want sealed: the grace elapsed 28 minutes ago", fact.state)
	}
	if fact.sealedGeneration == nil {
		t.Fatal("a sealed fact carries no ingest generation; the seal was not a compare-and-swap")
	}
	if *fact.sealedGeneration != 1 {
		t.Errorf("sealed generation = %d, want 1 (one inserted heartbeat)", *fact.sealedGeneration)
	}
}

// A bucket still inside its grace stays PROVISIONAL: late data may still change it, and
// sealing early is how a budget becomes a number two viewings disagree about.
func TestRecentBucketStaysProvisional(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	base := now.Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	beat(t, st, ctx, f.http, base.Add(time.Second), true)

	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fact, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("no fact")
	}
	if fact.state != "provisional" {
		t.Errorf("state = %q, want provisional: this bucket has not even ended yet", fact.state)
	}
	if fact.sealedGeneration != nil {
		t.Error("a provisional fact carries a sealed generation")
	}
}

// Ordinary materialization never rewrites a sealed fact. Doing so is an audited recompute
// or repair, and neither comes through this path.
func TestSealedFactIsNotRewrittenByOrdinaryMaterialization(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	beat(t, st, ctx, f.http, base.Add(10*time.Second), true)
	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("first: %v", err)
	}
	before, _ := readFact(t, st, ctx, f.serviceID, base)

	// A second, contradicting observation lands late and a second pass runs.
	beat(t, st, ctx, f.http, base.Add(20*time.Second), false)
	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("second: %v", err)
	}
	after, _ := readFact(t, st, ctx, f.serviceID, base)
	if after.good != before.good || after.bad != before.bad {
		t.Errorf("a sealed fact changed under ordinary materialization: %+v -> %+v", before, after)
	}
}

// The watermark is defined by CONTIGUITY. A hole holds it rather than being jumped over,
// which is what makes a stalled service visible instead of merely plausible.
func TestSealedThroughStopsAtAHole(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	for i := 0; i < 5; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}
	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(5*time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	through := sealedThrough(t, st, ctx, f.serviceID)
	if through == nil || !through.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("sealed_through = %v, want %s after five contiguous sealed buckets", through, base.Add(5*time.Minute))
	}

	// Punch a hole in the middle and recompute the watermark.
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("punch hole: %v", err)
	}
	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base.Add(4*time.Minute), base.Add(5*time.Minute)); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	through = sealedThrough(t, st, ctx, f.serviceID)
	if through == nil || !through.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("sealed_through = %v, want the hole at %s — the watermark jumped a gap",
			through, base.Add(2*time.Minute))
	}
}

// An archived maintenance window keeps its effect on time it already covered. Letting a
// later pass drop it would turn every archive into an annul: a sealed bucket changing with
// no preview, no raw fence and no audited intent.
func TestArchivedMaintenanceKeepsItsEffectiveSpan(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	beat(t, st, ctx, f.http, base.Add(-5*time.Second), false) // would be BAD without the window

	mw, err := st.createMaintenanceWindowUnchecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(time.Minute), Reason: "db failover",
	})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	// Archive it — "no longer in active inventory", not "it never happened".
	if _, err := st.pool.Exec(ctx, `UPDATE maintenance_windows SET archived_at = now() WHERE id=$1`, mw.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fact, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("no fact")
	}
	if fact.excluded != 60_000_000 {
		t.Fatalf("excluded = %dµs, want the whole bucket: an archived window lost its effective span", fact.excluded)
	}
	if fact.bad != 0 {
		t.Errorf("bad = %dµs — archiving silently became an annul", fact.bad)
	}
}

// Cancelling an active window truncates it at the EXACT statement time, not at a bucket
// boundary. The reducer handles arbitrary edges, so rounding would silently extend or
// shorten a real exclusion by up to a whole bucket.
func TestCancelledWindowTruncatesAtItsExactInstant(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	beat(t, st, ctx, f.http, base.Add(-5*time.Second), true)

	mw, err := st.createMaintenanceWindowUnchecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(time.Minute), Reason: "cut short",
	})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	cancelAt := base.Add(15 * time.Second)
	if _, err := st.pool.Exec(ctx,
		`UPDATE maintenance_windows SET cancel_effective_at = $2 WHERE id=$1`, mw.ID, cancelAt); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fact, _ := readFact(t, st, ctx, f.serviceID, base)
	if fact.excluded != 15_000_000 {
		t.Errorf("excluded = %dµs, want exactly 15s — the cancel instant was rounded", fact.excluded)
	}
	if fact.good != 45_000_000 {
		t.Errorf("good = %dµs, want 45s", fact.good)
	}
}

// A service with no declared reliability inputs produces no facts at all. It reports
// availability as unavailable — never as anything, and certainly never as 100%.
func TestServiceWithoutSLIProducesNoFacts(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: nil,
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatalf("declaration: %v", err)
	}
	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)

	n, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if n != 0 {
		t.Errorf("materialized %d buckets for a service with no reliability inputs", n)
	}
}

// Each bucket resolves its OWN epoch, so a range spanning a boundary evaluates each part
// under the semantics in force there.
func TestEachBucketResolvesItsOwnEpoch(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	for i := 0; i < 3; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+time.Second), true)
	}
	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(3*time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var epochs int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(DISTINCT epoch_id) FROM service_reliability_buckets WHERE service_id=$1`,
		f.serviceID).Scan(&epochs); err != nil {
		t.Fatalf("count epochs: %v", err)
	}
	if epochs != 1 {
		t.Fatalf("%d distinct epochs across one unchanged range, want 1", epochs)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id=$1 AND state='effective'
		  ORDER BY effective_at DESC, epoch_seq DESC LIMIT 1`, f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("epoch: %v", err)
	}
	fact, _ := readFact(t, st, ctx, f.serviceID, base)
	if fact.epochID != epochID {
		t.Errorf("the fact resolves to %s, want the epoch governing its bucket (%s)", fact.epochID, epochID)
	}
}

// The seal MATERIALIZES the ingest row before locking it, rather than locking whatever
// happens to exist.
//
// The difference only shows for a bucket that received no heartbeat at all: there is no row
// to lock, so a `SELECT … FOR UPDATE` locks nothing and a concurrent ingest can insert one
// and commit inside the window between the seal's read and its write. Proving the race
// itself needs two connections and a barrier; proving the MECHANISM needs only this — after
// sealing an empty bucket the row must exist, because that row is the mutual-exclusion point
// every later arrival for this bucket is decided under.
func TestSealMaterializesTheIngestRowForAnEmptyBucket(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	// Deliberately no heartbeat anywhere near this bucket.

	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fact, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("an empty bucket produced no fact; UNKNOWN time is still measured time")
	}
	if fact.unknown != 60_000_000 {
		t.Errorf("unknown = %dµs, want the whole bucket", fact.unknown)
	}
	if fact.state != "sealed" {
		t.Fatalf("state = %q, want sealed", fact.state)
	}

	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_bucket_ingest WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, base).Scan(&rows); err != nil {
		t.Fatalf("count ingest rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the seal left no ingest row for an empty bucket: a later arrival would be decided against nothing")
	}
}

// iter-0139 — the carry-in state is the LATEST prior observation, by definition, not an
// arbitrary one. The un-parenthesized DISTINCT ON branch let ANY prior row become the hold
// state once a member had two of them; a bucket with no in-bucket observations then
// materialized from whichever row the plan happened to visit first. Two prior observations,
// old DOWN then recent UP, no in-bucket rows: the bucket must be GOOD end to end.
func TestCarryInIsTheLatestPriorObservation(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	// Both observations sit INSIDE the member's freshness window at the bucket start, so
	// staleness cannot mask which row carried in: the wrong (older DOWN) carry-in yields
	// bad time, the right (newer UP) carry-in yields a fully GOOD minute.
	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	beat(t, st, ctx, f.http, base.Add(-40*time.Second), false)
	beat(t, st, ctx, f.http, base.Add(-10*time.Second), true)

	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fact, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("no fact")
	}
	if fact.good != 60_000_000 {
		t.Errorf("good = %dµs with a recent UP carry-in, want the whole minute — an arbitrary prior observation held the state", fact.good)
	}
	if fact.bad != 0 {
		t.Errorf("bad = %dµs from an older DOWN that a newer UP superseded", fact.bad)
	}
}
