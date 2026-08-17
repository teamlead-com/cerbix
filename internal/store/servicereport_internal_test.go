package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// FR-021 phase 2 — the reporting compute core (iter-0138). One test per §19 phase-2
// invariant where the store owns it; §11's confident-lie case has its own regression.

type reportFixture struct {
	declFixture
	epochID    string
	revisionID string
}

func reportService(t *testing.T, st *Store, ctx context.Context) reportFixture {
	t.Helper()
	f := adoptedService(t, st, ctx)
	rf := reportFixture{declFixture: f}
	if err := st.pool.QueryRow(ctx,
		`SELECT id, revision_id FROM service_evaluation_epochs
		  WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&rf.epochID, &rf.revisionID); err != nil {
		t.Fatalf("fixture epoch: %v", err)
	}
	return rf
}

// plantRange bulk-writes facts at the canonical cadence over [from, to), each bucket with
// the given µs split. The health axis mirrors availability (healthy=good, down=bad) so both
// conservation CHECKs hold.
func plantRange(t *testing.T, st *Store, ctx context.Context, f reportFixture, epochID string, from, to time.Time, good, bad, unknown, excluded int64, state string) {
	t.Helper()
	var sealedAt any
	if state == "sealed" {
		sealedAt = time.Now().UTC()
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_reliability_buckets
		  (service_id, project_id, epoch_id, bucket_start, bucket_size_us,
		   good_us, bad_us, unknown_us, excluded_us,
		   healthy_us, degraded_us, down_us, health_unknown_us, state, sealed_at)
		SELECT $1, $2, $3, gs, 60000000, $6, $7, $8, $9, $6, 0, $7, $8, $10, $11
		  FROM generate_series($4::timestamptz, $5::timestamptz - interval '1 minute', interval '1 minute') gs
		ON CONFLICT (service_id, bucket_start) DO UPDATE SET
		  epoch_id = EXCLUDED.epoch_id, good_us = EXCLUDED.good_us, bad_us = EXCLUDED.bad_us,
		  unknown_us = EXCLUDED.unknown_us, excluded_us = EXCLUDED.excluded_us,
		  healthy_us = EXCLUDED.healthy_us, degraded_us = EXCLUDED.degraded_us,
		  down_us = EXCLUDED.down_us, health_unknown_us = EXCLUDED.health_unknown_us,
		  state = EXCLUDED.state, sealed_at = EXCLUDED.sealed_at`,
		f.serviceID, f.projectID, epochID, from, to, good, bad, unknown, excluded, state, sealedAt); err != nil {
		t.Fatalf("plant range: %v", err)
	}
}

func setWatermark(t *testing.T, st *Store, ctx context.Context, f reportFixture, era, through time.Time) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_materialization
		   SET era_start = $2, materialization_start = LEAST(materialization_start, $2),
		       sealed_through = $3, materialized_through = $3
		 WHERE service_id = $1`, f.serviceID, era, through); err != nil {
		t.Fatalf("watermark: %v", err)
	}
}

func testWindow(minutes int) sla.Window {
	return sla.Window{Name: "test", Duration: time.Duration(minutes) * time.Minute}
}

// anchor is a minute-aligned instant comfortably in the past, far from minute-boundary luck.
func anchor() time.Time {
	return time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Minute)
}

const minute = 60_000_000 // µs

// Invariant 38 + §11.3: the window is [sealed_through − W, sealed_through) — never ending at
// now — the response carries as_of, sealed_through, both duration axes, the coverage
// fraction, and any repairing interval; a provisional tail right of the watermark is
// excluded from every number.
func TestReportWindowEndsAtSealedThroughNotNow(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	sealed := anchor()
	from := sealed.Add(-5 * time.Minute)
	plantRange(t, st, ctx, f, f.epochID, from, sealed, minute, 0, 0, 0, "sealed")
	// The provisional tail: right of the watermark, must not leak into any number.
	plantRange(t, st, ctx, f, f.epochID, sealed, sealed.Add(time.Minute), 0, minute, 0, 0, "provisional")
	setWatermark(t, st, ctx, f, from.Add(-time.Hour), sealed)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, from, from.Add(2*time.Minute), ReasonAdmin); err != nil {
		t.Fatalf("enqueue repair: %v", err)
	}

	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !rep.From.Equal(from) || !rep.To.Equal(sealed) {
		t.Fatalf("window [%s, %s), want [%s, %s) — the window must end at sealed_through", rep.From, rep.To, from, sealed)
	}
	if rep.SealedThrough == nil || !rep.SealedThrough.Equal(sealed) || rep.AsOf.IsZero() {
		t.Fatal("as_of/sealed_through missing from the response")
	}
	if rep.Durations.BadUs != 0 || rep.Durations.GoodUs != 5*minute {
		t.Fatalf("durations %+v include the provisional tail", rep.Durations)
	}
	if rep.Durations.HealthyUs != 5*minute {
		t.Fatalf("the health axis is not carried (healthy=%d)", rep.Durations.HealthyUs)
	}
	if rep.Status != domain.ServiceReportOK || !rep.StorageContinuity || rep.Coverage != 1 {
		t.Fatalf("status=%s continuity=%v coverage=%v for a fully sealed fully measured window", rep.Status, rep.StorageContinuity, rep.Coverage)
	}
	if rep.Availability == nil || *rep.Availability != 100 {
		t.Fatalf("availability = %v, want 100 over five good buckets", rep.Availability)
	}
	if len(rep.Repairing) != 1 || !rep.Repairing[0].From.Equal(from) || !rep.Repairing[0].To.Equal(from.Add(2*time.Minute)) {
		t.Fatalf("repairing = %+v, want the pending range clipped to the window", rep.Repairing)
	}
}

// Invariant 40 + §11.1's confident lie: excluded time is in NEITHER denominator, a zero
// denominator is `unavailable` (never 100%, never 0×), and one measured bucket in a sea of
// unknown yields a PARTIAL number, not a confident one.
func TestReportNeverManufacturesNumbers(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	sealed := anchor()
	setWatermark(t, st, ctx, f, sealed.Add(-time.Hour), sealed)

	// (a) the confident lie: 1 good bucket, 4 entirely unknown.
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-5*time.Minute), sealed.Add(-4*time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-4*time.Minute), sealed, 0, 0, minute, 0, "sealed")
	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportPartial || rep.Reason != domain.ServiceReportReasonLowCoverage {
		t.Fatalf("status=%s/%s for 60s of measurement in 5m, want partial/low-coverage", rep.Status, rep.Reason)
	}
	if rep.Coverage != 0.2 {
		t.Fatalf("coverage = %v, want 0.2", rep.Coverage)
	}

	// (b) zero decidable time: unavailable, and no availability is quoted at all.
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-5*time.Minute), sealed, 0, 0, minute, 0, "sealed")
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportUnavailable || rep.Reason != domain.ServiceReportReasonZeroDecidable {
		t.Fatalf("status=%s/%s for an all-unknown window, want unavailable/zero-decidable", rep.Status, rep.Reason)
	}
	if rep.Availability != nil {
		t.Fatalf("availability %v fabricated from zero measured time", *rep.Availability)
	}

	// (c) excluded-only window: excluded is in NEITHER denominator, so this is a zero
	// denominator too — never 100%.
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-5*time.Minute), sealed, 0, 0, 0, minute, "sealed")
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportUnavailable || rep.Availability != nil {
		t.Fatalf("status=%s availability=%v for an excluded-only window, want unavailable/absent", rep.Status, rep.Availability)
	}
	if rep.Coverage != 0 {
		t.Fatalf("coverage = %v with zero decidable and zero measured time", rep.Coverage)
	}

	// (d) the MIXED case that actually discriminates the denominators: half of every bucket
	// is declared out of scope, the other half measured good. Excluded time in EITHER
	// denominator would read 50% here; the honest answers are availability 100 of the
	// measured time and coverage 1 of the decidable time.
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-5*time.Minute), sealed, minute/2, 0, 0, minute/2, "sealed")
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportOK || rep.Coverage != 1 {
		t.Fatalf("status=%s coverage=%v — excluded time leaked into the coverage denominator", rep.Status, rep.Coverage)
	}
	if rep.Availability == nil || *rep.Availability != 100 {
		t.Fatalf("availability = %v — excluded time leaked into the availability denominator", *rep.Availability)
	}
}

// Invariant 39: storage continuity and decidable coverage are judged INDEPENDENTLY — a fully
// stored window can be under-measured, a hole is a storage answer, and a window reaching
// before the era is insufficient history with the numbers still absent.
func TestReportChecksContinuityAndCoverageIndependently(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	sealed := anchor()

	// (a) full continuity, under-measured: partial with the coverage reason.
	setWatermark(t, st, ctx, f, sealed.Add(-time.Hour), sealed)
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-10*time.Minute), sealed.Add(-1*time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-1*time.Minute), sealed, 0, 0, minute, 0, "sealed")
	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(10))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !rep.StorageContinuity || rep.Status != domain.ServiceReportPartial || rep.Reason != domain.ServiceReportReasonLowCoverage {
		t.Fatalf("continuity=%v status=%s/%s, want stored-but-under-measured", rep.StorageContinuity, rep.Status, rep.Reason)
	}
	if rep.Availability == nil {
		t.Fatal("a partial window still quotes its number with the fraction and reason (§11.2)")
	}

	// (b) a hole inside the era: storage gap, independent of coverage — and §11.2's "both
	// must pass" governs the NUMBERS: the rows that survived the hole cannot vouch for the
	// window, so no availability and no budget are quoted, objective or not.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO sla_targets (service_id, window_name, objective) VALUES ($1, 'test', 99.9)`,
		f.serviceID); err != nil {
		t.Fatalf("target: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, sealed.Add(-5*time.Minute)); err != nil {
		t.Fatalf("dig hole: %v", err)
	}
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(10))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.StorageContinuity || rep.Status != domain.ServiceReportPartial || rep.Reason != domain.ServiceReportReasonStorageGap {
		t.Fatalf("continuity=%v status=%s/%s, want a storage gap", rep.StorageContinuity, rep.Status, rep.Reason)
	}
	if rep.SealedBuckets != 9 || rep.ExpectedBuckets != 10 {
		t.Fatalf("sealed=%d expected=%d, want 9/10", rep.SealedBuckets, rep.ExpectedBuckets)
	}
	if rep.Availability != nil {
		t.Fatalf("availability %v quoted over a storage hole — the surviving rows cannot vouch for the window", *rep.Availability)
	}
	if rep.Budget != nil {
		t.Fatal("a budget was computed over a storage hole")
	}
	if rep.Objective == nil {
		t.Fatal("the objective itself is stateable even when its numbers are not")
	}
	if rep.AggregateWithheld != "" {
		t.Fatalf("aggregate_withheld = %q over a one-revision gap — the label is a claim, not a default", rep.AggregateWithheld)
	}

	// (b2) a ZERO-fact gap makes no claims about revisions at all ([170] P2-1): the known
	// reason is the storage gap, and "spans_definition_revisions" over nothing is invented.
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM service_reliability_buckets WHERE service_id=$1`, f.serviceID); err != nil {
		t.Fatalf("empty the window: %v", err)
	}
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(10))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportPartial || rep.Reason != domain.ServiceReportReasonStorageGap {
		t.Fatalf("status=%s/%s for a zero-fact window inside the era", rep.Status, rep.Reason)
	}
	if rep.AggregateWithheld != "" {
		t.Fatalf("aggregate_withheld = %q with zero facts — an unsupported revision claim", rep.AggregateWithheld)
	}
	if rep.SealedBuckets != 0 || rep.Availability != nil || rep.Budget != nil {
		t.Fatalf("a zero-fact window quoted something: %+v", rep)
	}

	// (c) the window reaches before the era: insufficient history, and no window aggregate.
	setWatermark(t, st, ctx, f, sealed.Add(-8*time.Minute), sealed)
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(10))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportInsufficientHistory || rep.Reason != domain.ServiceReportReasonEraShort {
		t.Fatalf("status=%s/%s for a window older than the era", rep.Status, rep.Reason)
	}
	if rep.Availability != nil || rep.Budget != nil {
		t.Fatal("a 10m number was quoted for a service whose era covers 8m")
	}
}

// Invariant 43: a window spanning definition revisions is ONLY segments — the aggregate is
// withheld, not even labelled — while epochs within ONE revision are segmented in the
// payload with the aggregate present.
func TestReportSegmentsRevisionsAndEpochsDifferently(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	sealed := anchor()
	setWatermark(t, st, ctx, f, sealed.Add(-time.Hour), sealed)

	// A second REVISION (drop redis from monitors — sli unchanged) and its epoch.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("revision 2: %v", err)
	}
	var epoch2, revision2 string
	if err := st.pool.QueryRow(ctx,
		`SELECT id, revision_id FROM service_evaluation_epochs
		  WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`,
		f.serviceID).Scan(&epoch2, &revision2); err != nil {
		t.Fatalf("epoch 2: %v", err)
	}
	if revision2 == f.revisionID {
		t.Fatal("fixture broke: the second declaration did not create a new revision")
	}

	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-6*time.Minute), sealed.Add(-3*time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f, epoch2, sealed.Add(-3*time.Minute), sealed, 0, minute, 0, 0, "sealed")

	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(6))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Segments) != 2 {
		t.Fatalf("%d segments across a revision boundary, want 2", len(rep.Segments))
	}
	if rep.Availability != nil || rep.AggregateWithheld != domain.ServiceReportReasonSpansRevisions {
		t.Fatalf("availability=%v withheld=%q — an aggregate crossed a definition boundary", rep.Availability, rep.AggregateWithheld)
	}
	if rep.Segments[0].Availability == nil || *rep.Segments[0].Availability != 100 ||
		rep.Segments[1].Availability == nil || *rep.Segments[1].Availability != 0 {
		t.Fatalf("segments must carry their own numbers: %+v", rep.Segments)
	}

	// Same-revision epochs: plant BOTH halves under revision 2's epochs. A second epoch of
	// revision 2 is authored directly (an execution-semantics change without redeclaration).
	var epoch3 string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO service_evaluation_epochs
		  (service_id, project_id, epoch_seq, revision_id, effective_at, snapshot_hash)
		SELECT service_id, project_id, epoch_seq + 1, revision_id, effective_at + interval '1 minute', 'epoch3'
		  FROM service_evaluation_epochs WHERE id = $1
		RETURNING id`, epoch2).Scan(&epoch3); err != nil {
		t.Fatalf("epoch 3: %v", err)
	}
	plantRange(t, st, ctx, f, epoch2, sealed.Add(-6*time.Minute), sealed.Add(-3*time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f, epoch3, sealed.Add(-3*time.Minute), sealed, minute, 0, 0, 0, "sealed")

	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(6))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Segments) != 2 {
		t.Fatalf("%d segments across an epoch boundary, want 2 (epochs are segmented too)", len(rep.Segments))
	}
	if rep.AggregateWithheld != "" || rep.Availability == nil || *rep.Availability != 100 {
		t.Fatalf("availability=%v withheld=%q — one revision's epochs must aggregate", rep.Availability, rep.AggregateWithheld)
	}
}

// Invariant 44 (§6.6): only the RETROACTIVE part of a first-adoption backfill is a declared
// reconstruction; measurement after the declaration write is not.
func TestReportMarksOnlyTheBackfilledReconstruction(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	var createdAt time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT created_at FROM service_definition_revisions WHERE service_id=$1 AND revision=1`,
		f.serviceID).Scan(&createdAt); err != nil {
		t.Fatalf("revision 1: %v", err)
	}
	boundary := domain.CeilToBucket(createdAt)
	from, to := boundary.Add(-3*time.Minute), boundary.Add(3*time.Minute)
	plantRange(t, st, ctx, f, f.epochID, from, to, minute, 0, 0, 0, "sealed")
	setWatermark(t, st, ctx, f, from, to)

	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(6))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Segments) != 2 {
		t.Fatalf("%d segments, want the reconstruction split at CeilToBucket(created_at)", len(rep.Segments))
	}
	if !rep.Segments[0].DeclaredReconstruction || rep.Segments[1].DeclaredReconstruction {
		t.Fatalf("reconstruction flags %v/%v, want true/false around the declaration write",
			rep.Segments[0].DeclaredReconstruction, rep.Segments[1].DeclaredReconstruction)
	}
	if !rep.Segments[0].To.Equal(boundary) || !rep.Segments[1].From.Equal(boundary) {
		t.Fatalf("split at [%s|%s], want %s", rep.Segments[0].To, rep.Segments[1].From, boundary)
	}
	if rep.Segments[0].EpochID != rep.Segments[1].EpochID {
		t.Fatal("the reconstruction split invented a second epoch")
	}
}

// Invariant 45 + §11.3: the budget states the objective that produced it and the objective's
// change is annotated (updated_at moves); without a service-scoped target there is no
// budget and no burn — nothing is borrowed from another scope.
func TestReportBudgetStatesItsObjective(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	sealed := anchor()
	setWatermark(t, st, ctx, f, sealed.Add(-8*time.Hour), sealed)
	// 30 buckets: 29 good, 1 bad → availability ≈ 96.67%.
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-30*time.Minute), sealed.Add(-time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-time.Minute), sealed, 0, minute, 0, 0, "sealed")

	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(30))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Objective != nil || rep.Budget != nil || rep.Burn != nil {
		t.Fatalf("objective/budget/burn present without a service-scoped target: %+v", rep)
	}

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO sla_targets (service_id, window_name, objective) VALUES ($1, 'test', 99.9)`,
		f.serviceID); err != nil {
		t.Fatalf("target: %v", err)
	}
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(30))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Objective == nil || *rep.Objective != 99.9 || rep.Budget == nil || rep.Budget.Objective != 99.9 {
		t.Fatalf("the budget does not state its objective: %+v", rep.Budget)
	}
	if rep.ObjectiveUpdatedAt == nil {
		t.Fatal("the objective annotation timestamp is missing")
	}
	if rep.Budget.Met {
		t.Fatal("96.67%% availability met a 99.9 objective")
	}
	firstAt := *rep.ObjectiveUpdatedAt

	// The objective is a CURRENT-VIEW parameter: a change is reflected instantly over
	// identical facts, and the annotation timestamp moves.
	if _, err := st.pool.Exec(ctx,
		`UPDATE sla_targets SET objective = 50, updated_at = now() WHERE service_id = $1`,
		f.serviceID); err != nil {
		t.Fatalf("retarget: %v", err)
	}
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(30))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Budget == nil || rep.Budget.Objective != 50 || !rep.Budget.Met {
		t.Fatalf("objective change not reflected as a current view: %+v", rep.Budget)
	}
	if !rep.ObjectiveUpdatedAt.After(firstAt) {
		t.Fatal("the objective-change annotation did not move")
	}
}

// §11.3's burn rules: rates come from [sealed_through − w, sealed_through); a real-time
// window with no sealed time at all is insufficient_sealed_coverage, never a stale rate and
// never 0×.
func TestReportBurnIsBoundedBySealedCoverage(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	// Fresh watermark: both burn windows computable.
	sealed := time.Now().UTC().Truncate(time.Minute)
	era := sealed.Add(-8 * time.Hour)
	setWatermark(t, st, ctx, f, era, sealed)
	// 6h of good with 6 bad minutes in the last hour.
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-6*time.Hour), sealed.Add(-6*time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-6*time.Minute), sealed, 0, minute, 0, 0, "sealed")
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO sla_targets (service_id, window_name, objective) VALUES ($1, 'test', 99)`,
		f.serviceID); err != nil {
		t.Fatalf("target: %v", err)
	}

	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(360))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Burn) != 2 {
		t.Fatalf("%d burn windows, want the fixed 1h/6h pair", len(rep.Burn))
	}
	oneH, sixH := rep.Burn[0], rep.Burn[1]
	if oneH.Status != domain.ServiceReportOK || oneH.Rate == nil {
		t.Fatalf("1h burn = %+v, want a rate over fresh sealed data", oneH)
	}
	// 6 bad minutes of 60 = 10%% bad; objective 99 allows 1%% → burn 10×.
	if *oneH.Rate < 9.9 || *oneH.Rate > 10.1 {
		t.Fatalf("1h burn rate = %v, want ≈10", *oneH.Rate)
	}
	if sixH.Status != domain.ServiceReportOK || sixH.Rate == nil {
		t.Fatalf("6h burn = %+v", sixH)
	}
	if !oneH.StorageContinuity || oneH.SealedBuckets != 60 || oneH.ExpectedBuckets != 60 || oneH.Coverage != 1 {
		t.Fatalf("1h burn window verdicts missing from the payload: %+v", oneH)
	}

	// A HOLE inside the 1h window: the burn window measures its own storage continuity and
	// withholds the rate — a rate from the surviving rows is fabricated confidence, and 59
	// buckets cannot prove the missing one belonged to the same revision.
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, sealed.Add(-30*time.Minute)); err != nil {
		t.Fatalf("dig burn hole: %v", err)
	}
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(360))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	oneH, sixH = rep.Burn[0], rep.Burn[1]
	if oneH.Status != domain.ServiceReportPartial || oneH.Reason != domain.ServiceReportReasonStorageGap || oneH.Rate != nil {
		t.Fatalf("1h burn over a hole = %+v, want partial/storage_gap with NO rate", oneH)
	}
	if oneH.SealedBuckets != 59 || oneH.ExpectedBuckets != 60 || oneH.StorageContinuity {
		t.Fatalf("1h burn continuity verdicts = %+v, want 59/60, no continuity", oneH)
	}
	if sixH.Status != domain.ServiceReportPartial || sixH.Reason != domain.ServiceReportReasonStorageGap || sixH.Rate != nil {
		t.Fatalf("6h burn shares the hole = %+v, want partial/storage_gap with NO rate", sixH)
	}
	// The main window shares the verdict: no availability, no budget over a gap.
	if rep.Availability != nil || rep.Budget != nil {
		t.Fatal("the main window quoted numbers over the same hole")
	}
	// Refill the hole for the stale-watermark scenario below.
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-30*time.Minute), sealed.Add(-29*time.Minute), minute, 0, 0, 0, "sealed")

	// Stale watermark: the real-time 1h window holds no sealed data → no rate, named
	// status. The 6h window still overlaps sealed time and stays computable.
	stale := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	setWatermark(t, st, ctx, f, era, stale)
	plantRange(t, st, ctx, f, f.epochID, stale.Add(-6*time.Hour), stale, minute, 0, 0, 0, "sealed")
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(360))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	oneH, sixH = rep.Burn[0], rep.Burn[1]
	if oneH.Status != domain.ServiceReportInsufficientSealed || oneH.Rate != nil {
		t.Fatalf("1h burn over a 2h-stale watermark = %+v, want insufficient_sealed_coverage with no rate", oneH)
	}
	if oneH.Reason != domain.ServiceReportReasonStaleWatermark {
		t.Fatalf("1h burn reason = %q", oneH.Reason)
	}
	// [170] P1-2: the staleness status QUALIFIES the answer — it does not rewrite the
	// anchored window's storage facts. [sealed_through − 1h, sealed_through) is fully
	// stored and fully measured, and its verdict fields must say so.
	if oneH.SealedBuckets != 60 || oneH.ExpectedBuckets != 60 || !oneH.StorageContinuity || oneH.Coverage != 1 {
		t.Fatalf("stale 1h burn relabeled a full window as empty: %+v", oneH)
	}
	if sixH.Status != domain.ServiceReportOK || sixH.Rate == nil {
		t.Fatalf("6h burn = %+v, want still computable (sealed data within 6h of now)", sixH)
	}

	// Era-short carries the same honesty: the stored part of the anchored window keeps its
	// true verdict fields under the insufficient_history status.
	setWatermark(t, st, ctx, f, stale.Add(-3*time.Hour), stale)
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(360))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	sixH = rep.Burn[1]
	if sixH.Status != domain.ServiceReportInsufficientHistory || sixH.Reason != domain.ServiceReportReasonEraShort || sixH.Rate != nil {
		t.Fatalf("6h burn with a 3h era = %+v, want insufficient_history with no rate", sixH)
	}
	if sixH.SealedBuckets != 360 || sixH.ExpectedBuckets != 360 || !sixH.StorageContinuity || sixH.Coverage != 1 {
		t.Fatalf("era-short 6h burn relabeled a full window as empty: %+v", sixH)
	}
}

// Invariant 47 + §13: a service-scoped burn alert is impossible at the schema, the
// application layer offers no knob, and the three-way scope exclusivity holds.
// SUPERSEDED BY PHASE 5 (§16.4): a service-scoped burn target used to be rejected outright
// (`sla_targets_service_no_burn_chk`), because a service had no burn signal to own. Migration 00082
// drops that check deliberately — the sealed burn signal is now a service's, with its LATCH
// normalized into `service_burn_alert_state` (§16.4b) rather than kept in the target's JSON. What
// remains guarded here is the part that did NOT change: three-way scope exclusivity and the
// objective-upsert contract.
func TestServiceScopedBurnTargetIsSupported(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		 VALUES ($1, '90d', 99.9, true, '[{"threshold":1}]')`, f.serviceID); err != nil {
		t.Fatalf("a service burn target must now be accepted (§16.4): %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO sla_targets (service_id, project_id, window_name, objective)
		 VALUES ($1, $2, '30d', 99.9)`, f.serviceID, f.projectID); err == nil ||
		!strings.Contains(err.Error(), "sla_targets_scope_chk") {
		t.Fatalf("a two-scope target passed the three-way exclusivity: %v", err)
	}

	// The supported path: objective only, per standard window, idempotent upsert.
	if err := st.UpsertServiceSLATarget(ctx, f.projectID, f.serviceID, "30d", 99.9); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertServiceSLATarget(ctx, f.projectID, f.serviceID, "30d", 99.95); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	var objective float64
	if err := st.pool.QueryRow(ctx,
		`SELECT objective::float8 FROM sla_targets WHERE service_id = $1 AND window_name = '30d'`,
		f.serviceID).Scan(&objective); err != nil || objective != 99.95 {
		t.Fatalf("objective = %v (%v), want 99.95", objective, err)
	}
	if err := st.UpsertServiceSLATarget(ctx, f.projectID, f.serviceID, "5m", 99.9); err == nil {
		t.Fatal("an unknown window name was accepted")
	}
	if err := st.UpsertServiceSLATarget(ctx, f.projectID, "00000000-0000-0000-0000-000000000000", "30d", 99.9); err != ErrNotFound {
		t.Fatalf("upsert for a missing service = %v, want ErrNotFound", err)
	}
}

// iter-0144: the objective is a CURRENT-VIEW parameter, visible before the first bucket
// seals — an operator who just set a target must see it on the nothing-sealed report (no
// budget, no burn: there is nothing to compute them from).
func TestNothingSealedReportStillStatesItsObjective(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx) // declared SLI, watermark row exists, nothing sealed yet
	if err := st.UpsertServiceSLATarget(ctx, f.projectID, f.serviceID, "30d", 99.9); err != nil {
		t.Fatalf("target: %v", err)
	}
	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, sla.Window{Name: "30d", Duration: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportInsufficientSealed || rep.Reason != domain.ServiceReportReasonNothingSealed {
		t.Fatalf("status=%s/%s, want the nothing-sealed reason", rep.Status, rep.Reason)
	}
	if rep.Objective == nil || *rep.Objective != 99.9 || rep.ObjectiveUpdatedAt == nil {
		t.Fatalf("objective %v/%v — a stored target is invisible before sealing", rep.Objective, rep.ObjectiveUpdatedAt)
	}
	if rep.Budget != nil || rep.Burn != nil || rep.Availability != nil {
		t.Fatal("numbers were computed from nothing")
	}
}

// Invariant 41: a service with an empty sli[] has NO SLO — the report says so instead of
// inventing 100%.
func TestReportWithoutSLIHasNoSLO(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: nil,
	}, 0, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("empty-sli declaration: %v", err)
	}
	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(30))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Status != domain.ServiceReportUnavailable || rep.Reason != domain.ServiceReportReasonNoSLI {
		t.Fatalf("status=%s/%s for an empty sli[], want unavailable/no_sli", rep.Status, rep.Reason)
	}
	if rep.Availability != nil || rep.Budget != nil {
		t.Fatal("an SLO was invented for a service that declares no reliability inputs")
	}
}

// Invariant 42: adding a monitor to monitors[] (diagnostics) leaves every reliability NUMBER
// identical — same per-segment durations, availability and coverage — and the new epoch's
// snapshot hash proves the evaluation semantics did not move. The window aggregate across
// the resulting declaration boundary is withheld by invariant 43's presentation rule; the
// numbers themselves must not change.
func TestReportDiagnosticMonitorChangesNoNumber(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	sealed := anchor()
	setWatermark(t, st, ctx, f, sealed.Add(-time.Hour), sealed)
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-5*time.Minute), sealed, minute, 0, 0, 0, "sealed")

	before, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	var hashBefore string
	if err := st.pool.QueryRow(ctx,
		`SELECT snapshot_hash FROM service_evaluation_epochs WHERE id = $1`, f.epochID).Scan(&hashBefore); err != nil {
		t.Fatalf("hash: %v", err)
	}
	// redis joins monitors[] only; the SLI is untouched.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("diagnostic declaration: %v", err)
	}
	var hashAfter string
	if err := st.pool.QueryRow(ctx, `
		SELECT snapshot_hash FROM service_evaluation_epochs
		 WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`, f.serviceID).Scan(&hashAfter); err != nil {
		t.Fatalf("hash after: %v", err)
	}
	if hashAfter != hashBefore {
		t.Fatal("a diagnostics-only change moved the evaluation snapshot hash — it reached the SLI")
	}

	after, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("report after: %v", err)
	}
	if len(after.Segments) != len(before.Segments) {
		t.Fatalf("segments %d → %d from a diagnostics-only change over untouched facts", len(before.Segments), len(after.Segments))
	}
	for i := range after.Segments {
		b, a := before.Segments[i], after.Segments[i]
		if a.Durations != b.Durations || a.Coverage != b.Coverage ||
			(a.Availability == nil) != (b.Availability == nil) ||
			(a.Availability != nil && *a.Availability != *b.Availability) {
			t.Fatalf("segment %d numbers moved: %+v → %+v", i, b, a)
		}
	}
	if *after.Availability != *before.Availability {
		t.Fatal("the aggregate over untouched facts moved")
	}
}

// §10.2/§12.1: on-read rollups are exact sums keyed by epoch, never merged across an epoch
// boundary — one hour that spans two epochs yields two points — and provisional time rolls
// up separately from sealed time.
func TestReliabilitySeriesNeverMergesAcrossEpochs(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	hour := time.Now().UTC().Add(-7 * time.Hour).Truncate(time.Hour)
	var epoch2 string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO service_evaluation_epochs
		  (service_id, project_id, epoch_seq, revision_id, effective_at, snapshot_hash)
		SELECT service_id, project_id, epoch_seq + 1, revision_id, effective_at + interval '1 minute', 'series2'
		  FROM service_evaluation_epochs WHERE id = $1
		RETURNING id`, f.epochID).Scan(&epoch2); err != nil {
		t.Fatalf("epoch 2: %v", err)
	}
	plantRange(t, st, ctx, f, f.epochID, hour, hour.Add(30*time.Minute), minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f, epoch2, hour.Add(30*time.Minute), hour.Add(50*time.Minute), 0, minute, 0, 0, "sealed")
	plantRange(t, st, ctx, f, epoch2, hour.Add(50*time.Minute), hour.Add(time.Hour), 0, minute, 0, 0, "provisional")

	points, err := st.ServiceReliabilitySeries(ctx, f.projectID, f.serviceID, hour, hour.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("%d points for one hour spanning two epochs + a provisional tail, want 3", len(points))
	}
	for _, p := range points {
		if !p.Start.Equal(hour) {
			t.Fatalf("point start %s, want %s", p.Start, hour)
		}
	}
	if points[0].EpochID != f.epochID || points[0].Durations.GoodUs != 30*minute || points[0].Provisional {
		t.Fatalf("epoch-1 point wrong: %+v", points[0])
	}
	if points[1].EpochID != epoch2 || points[1].Durations.BadUs != 20*minute || points[1].Provisional {
		t.Fatalf("sealed epoch-2 point wrong: %+v", points[1])
	}
	if !points[2].Provisional || points[2].Durations.BadUs != 10*minute {
		t.Fatalf("provisional point wrong: %+v", points[2])
	}
	if _, err := st.ServiceReliabilitySeries(ctx, f.projectID, f.serviceID, hour, hour.Add(time.Hour), 7*time.Minute); err == nil {
		t.Fatal("an arbitrary series step was accepted")
	}
}

// §11.3: current_health is computed LIVE from provisional inputs through the PRODUCT path —
// declaration + ordinary heartbeat ingest only, no direct fact insert, no direct
// MaterializeServiceRange — and stored facts are never consulted: a sealed HEALTHY history
// left by a stalled materializer must not impersonate "now".
func TestServiceHealthNowIsLiveAndRefusesSealedHistory(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	// Nothing observed yet: the SLI layer is unknown, honestly.
	h, err := st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !h.Unstable || h.SLI != "unknown" {
		t.Fatalf("no observations yet: %+v, want unstable unknown", h)
	}
	if h.AsOf.IsZero() {
		t.Fatal("as_of missing")
	}

	// The ADVERSARIAL DISTRACTOR: a stalled materializer's sealed HEALTHY history. If the
	// live signal read stored facts, this is what it would (wrongly) report.
	stale := anchor()
	plantRange(t, st, ctx, f, f.epochID, stale, stale.Add(5*time.Minute), minute, 0, 0, 0, "sealed")
	setWatermark(t, st, ctx, f, stale.Add(-time.Hour), stale.Add(5*time.Minute))

	// The live truth arrives through ORDINARY ingest: the SLI member reports DOWN.
	beat(t, st, ctx, f.http, time.Now().UTC().Add(-20*time.Second), false)
	h, err = st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.SLI != "down" {
		t.Fatalf("SLI = %q with a live DOWN observation — sealed history impersonated the present", h.SLI)
	}
	if !h.Unstable {
		t.Fatal("the live signal must declare itself unstable")
	}
}

// [170] P1-1: current_health is the state AT as_of — not the worst state anywhere in the
// current minute, and not a state established by an accepted-but-future observation. All
// inputs arrive through ORDINARY ingest with strictly increasing timestamps.
func TestServiceHealthNowIsTheStateAtAsOf(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	now := time.Now().UTC()

	// DOWN then UP inside one minute: the recovered service reads UP now. A worst-in-bucket
	// reduction would keep reporting down until the minute boundary.
	beat(t, st, ctx, f.http, now.Add(-40*time.Second), false)
	beat(t, st, ctx, f.http, now.Add(-10*time.Second), true)
	h, err := st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.SLI != "healthy" {
		t.Fatalf("SLI = %q after a same-minute DOWN→UP recovery, want healthy — worst-in-bucket leaked", h.SLI)
	}

	// The mirror: UP then DOWN inside the same minute reads DOWN now.
	beat(t, st, ctx, f.http, now.Add(-5*time.Second), false)
	h, err = st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.SLI != "down" {
		t.Fatalf("SLI = %q after a same-minute UP→DOWN, want down", h.SLI)
	}

	// An observation with an accepted FUTURE timestamp (result skew allows up to 5m) must
	// not answer early: the state at as_of is still the DOWN in force now.
	out := beat(t, st, ctx, f.http, now.Add(90*time.Second), true)
	if out.Reason == ReasonFutureTimestamp {
		t.Fatalf("fixture broke: a +90s timestamp was quarantined instead of accepted (%+v)", out)
	}
	h, err = st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.SLI != "down" {
		t.Fatalf("SLI = %q — an accepted future observation answered before its time", h.SLI)
	}
}

// healthAt drives the seam with an exact instant: boundary cases (an input effective at
// PRECISELY as_of) are not reproducible through the wrapper that mints its own clock.
func healthAt(t *testing.T, st *Store, ctx context.Context, f reportFixture, at time.Time) domain.ServiceHealthNow {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	h, err := serviceHealthNowTx(ctx, tx, f.projectID, f.serviceID, at)
	if err != nil {
		t.Fatalf("health at %s: %v", at, err)
	}
	return h
}

// [175] P1: the live state is RIGHT-continuous at as_of — an observation, a stale deadline
// or a maintenance start effective EXACTLY at as_of belongs to the answer; a left-limit
// evaluation returns the pre-boundary state on all three.
func TestServiceHealthNowIncludesBreakpointsAtAsOf(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	// (a) an observation AT as_of is effective at its own timestamp.
	at := time.Now().UTC().Truncate(time.Minute).Add(-10*time.Minute + 10*time.Second)
	beat(t, st, ctx, f.http, at.Add(-30*time.Second), false)
	beat(t, st, ctx, f.http, at, true)
	if h := healthAt(t, st, ctx, f, at); h.SLI != "healthy" {
		t.Fatalf("SLI = %q with an UP observation exactly at as_of, want healthy — the left limit answered", h.SLI)
	}

	// (b) the stale deadline is stale AT the deadline (effective.go), so the answer at
	// exactly obs.ts + StaleAfter is unknown, not the still-fresh prior state.
	var staleAfter time.Duration
	{
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, members, _, err := epochAt(ctx, tx, f.serviceID, at)
		if err != nil || len(members) == 0 {
			t.Fatalf("members: %v (%d)", err, len(members))
		}
		staleAfter = members[0].StaleAfter
		_ = tx.Rollback(ctx)
	}
	if staleAfter <= 0 {
		t.Fatalf("fixture broke: StaleAfter = %s", staleAfter)
	}
	deadline := at.Add(staleAfter)
	if h := healthAt(t, st, ctx, f, deadline); h.SLI != "unknown" {
		t.Fatalf("SLI = %q exactly at the stale deadline, want unknown — the left limit answered", h.SLI)
	}
	if h := healthAt(t, st, ctx, f, deadline.Add(-time.Microsecond)); h.SLI != "healthy" {
		t.Fatalf("SLI = %q one µs before the stale deadline, want the still-fresh healthy", h.SLI)
	}

	// (c) a maintenance window starting EXACTLY at as_of excludes the member at as_of. The
	// boundary sits 15s ahead — inside the member's freshness window — so the −1µs probe
	// proves the flip comes from the window edge, not from staleness.
	mStart := time.Now().UTC().Add(15 * time.Second).Truncate(time.Microsecond)
	if staleAfter <= 20*time.Second {
		t.Fatalf("fixture broke: StaleAfter %s too short for the boundary probe", staleAfter)
	}
	if _, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: mStart, EndsAt: mStart.Add(30 * time.Minute), Reason: "boundary",
	}, "", 90*24*time.Hour); err != nil {
		t.Fatalf("window: %v", err)
	}
	beat(t, st, ctx, f.http, time.Now().UTC().Truncate(time.Microsecond), true) // fresh going into the boundary
	if h := healthAt(t, st, ctx, f, mStart.Add(-time.Microsecond)); h.SLI != "healthy" {
		t.Fatalf("SLI = %q one µs before the maintenance start, want healthy", h.SLI)
	}
	if h := healthAt(t, st, ctx, f, mStart); h.SLI != "unknown" {
		t.Fatalf("SLI = %q exactly at the maintenance start, want unknown (excluded)", h.SLI)
	}
}

// [178] P1: freshness durations are NANOSECOND-granular (time.ParseDuration), so a derived
// stale deadline can fall strictly inside any fixed-width evaluation window — the reviewer's
// exact repro: active_floor=90.0000005s puts the deadline 500ns AFTER a µs-aligned as_of,
// where a windowed reduction saw both the healthy prefix and the post-deadline state and the
// priority switch answered with the worst. A point evaluation must answer healthy at
// T+90s (the deadline is 500ns later) and unknown-or-worse at the NEXT representable µs.
func TestServiceHealthNowSurvivesSubMicrosecondDeadlines(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
		Policies: domain.ServicePolicies{
			MissingData: domain.MissingBad,
			Freshness:   domain.FreshnessPolicy{ActiveMultiplier: 3, ActiveFloor: 90*time.Second + 500*time.Nanosecond},
		},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	rf := reportFixture{declFixture: f}

	// A µs-aligned observation; the resolved StaleAfter is max(3×30s, 90.0000005s) =
	// 90.0000005s, so the stale deadline sits 500ns past every representable µs instant.
	obsAt := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	beat(t, st, ctx, f.http, obsAt, true)

	// AT T+90s the member is still fresh by 500ns: the state at as_of is healthy. A
	// windowed evaluation of any fixed width crosses the deadline and answers down
	// (missing_data=bad converts stale to bad).
	if h := healthAt(t, st, ctx, rf, obsAt.Add(90*time.Second)); h.SLI != "healthy" {
		t.Fatalf("SLI = %q at T+90s with the deadline 500ns later, want healthy — a window crossed a sub-µs deadline", h.SLI)
	}
	// One representable µs later the deadline has passed: stale, and missing_data=bad
	// makes that read down.
	if h := healthAt(t, st, ctx, rf, obsAt.Add(90*time.Second+time.Microsecond)); h.SLI != "down" {
		t.Fatalf("SLI = %q one µs past the sub-µs deadline, want down under missing_data=bad", h.SLI)
	}
}

// §12.2: the two live layers are separate — a failing or undecided DIAGNOSTIC monitor never
// touches the customer-facing SLI layer, a pending member makes diagnostics UNKNOWN rather
// than silently healthy, and the failing list is deterministic.
func TestServiceHealthNowIsTwoSeparateLayers(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	// The SLI member reports UP through ordinary ingest → the SLI layer is healthy.
	beat(t, st, ctx, f.http, time.Now().UTC().Add(-20*time.Second), true)
	h, err := st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.SLI != "healthy" {
		t.Fatalf("SLI = %q with a live UP observation", h.SLI)
	}
	// Both monitors still carry their initial 'pending' status: a member that has never
	// been confirmed either way cannot make diagnostics "ok".
	if h.Diagnostics != "unknown" {
		t.Fatalf("diagnostics = %q over pending members, want unknown", h.Diagnostics)
	}

	// redis (diagnostics-only) goes down; http confirms up.
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET status = CASE WHEN id = $1 THEN 'down' ELSE 'up' END WHERE id IN ($1, $2)`,
		f.redis, f.http); err != nil {
		t.Fatalf("set statuses: %v", err)
	}
	h, err = st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.SLI != "healthy" {
		t.Fatalf("SLI = %q — a diagnostic monitor degraded the customer-facing layer", h.SLI)
	}
	if h.Diagnostics != "failing" || len(h.FailingMonitors) != 1 {
		t.Fatalf("diagnostics = %q %v, want failing with one monitor", h.Diagnostics, h.FailingMonitors)
	}

	// Everything up → ok.
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET status = 'up' WHERE id IN ($1, $2)`, f.redis, f.http); err != nil {
		t.Fatalf("recover: %v", err)
	}
	h, err = st.ServiceHealthNow(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Diagnostics != "ok" {
		t.Fatalf("diagnostics = %q with every member up", h.Diagnostics)
	}
}

// P1-3 ([166]): one report is ONE snapshot and ONE clock. The report body is assembled
// through a repeatable-read transaction pinned BEFORE a concurrent invalidation commits:
// every part of the answer — watermark, facts, repair queue — must come from the
// pre-mutation state; a fresh report then sees the post-mutation state. Any helper reading
// the pool directly would mix the two into a state that never existed.
func TestReportIsAssembledFromOneSnapshot(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	sealed := anchor()
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-5*time.Minute), sealed, minute, 0, 0, 0, "sealed")
	setWatermark(t, st, ctx, f, sealed.Add(-time.Hour), sealed)

	tx, asOf, err := st.beginReportSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	// The concurrent invalidation, committed AFTER the snapshot was pinned: the watermark
	// advances, new BAD facts appear, and a repair range lands in the queue.
	plantRange(t, st, ctx, f, f.epochID, sealed, sealed.Add(2*time.Minute), 0, minute, 0, 0, "sealed")
	setWatermark(t, st, ctx, f, sealed.Add(-time.Hour), sealed.Add(2*time.Minute))
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, sealed.Add(-3*time.Minute), sealed, ReasonAdmin); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	rep, err := st.serviceReliabilityReportTx(ctx, tx, f.projectID, f.serviceID, testWindow(5), asOf)
	if err != nil {
		t.Fatalf("report in snapshot: %v", err)
	}
	if rep.SealedThrough == nil || !rep.SealedThrough.Equal(sealed) {
		t.Fatalf("sealed_through = %v inside the snapshot, want the pre-mutation %s", rep.SealedThrough, sealed)
	}
	if rep.Durations.BadUs != 0 || rep.Durations.GoodUs != 5*minute {
		t.Fatalf("durations %+v mixed post-mutation facts into a pre-mutation window", rep.Durations)
	}
	if len(rep.Repairing) != 0 {
		t.Fatalf("a post-snapshot repair range leaked into the snapshot report: %+v", rep.Repairing)
	}
	if !rep.AsOf.Equal(asOf) {
		t.Fatalf("as_of = %s, want the snapshot's own clock %s", rep.AsOf, asOf)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// A fresh report sees the post-mutation world, consistently.
	rep, err = st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, testWindow(5))
	if err != nil {
		t.Fatalf("fresh report: %v", err)
	}
	if rep.SealedThrough == nil || !rep.SealedThrough.Equal(sealed.Add(2*time.Minute)) {
		t.Fatalf("fresh sealed_through = %v, want the post-mutation watermark", rep.SealedThrough)
	}
	if rep.Durations.BadUs != 2*minute {
		t.Fatalf("fresh durations %+v, want the two BAD buckets visible", rep.Durations)
	}
	if len(rep.Repairing) != 1 {
		t.Fatalf("fresh repairing = %+v, want the enqueued range", rep.Repairing)
	}
}

// Invariant 32 / AGENTS: every exported phase-2 read and write is tenant-scoped — a caller
// naming the right service under the WRONG project gets nothing, learns nothing, and
// mutates nothing.
func TestServiceReportSurfacesAreTenantScoped(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	sealed := anchor()
	plantRange(t, st, ctx, f, f.epochID, sealed.Add(-5*time.Minute), sealed, minute, 0, 0, 0, "sealed")
	setWatermark(t, st, ctx, f, sealed.Add(-time.Hour), sealed)
	if err := st.UpsertServiceSLATarget(ctx, f.projectID, f.serviceID, "30d", 99.9); err != nil {
		t.Fatalf("target: %v", err)
	}
	// A real second tenant: its own organization and project.
	org2, err := st.CreateOrganization(ctx, "acme-foreign", "Acme Foreign")
	if err != nil {
		t.Fatalf("foreign org: %v", err)
	}
	proj2, err := st.CreateProject(ctx, org2.ID, "payments-foreign", "Payments Foreign")
	if err != nil {
		t.Fatalf("foreign project: %v", err)
	}
	foreign := declFixture{projectID: proj2.ID}

	// Since iter-0141 the reads answer a wrong-project caller with ErrNotFound — never a
	// no_sli/unknown-shaped 200 that doubles as an existence oracle — and still leak nothing.
	rep, err := st.ServiceReliabilityReport(ctx, foreign.projectID, f.serviceID, testWindow(5))
	if err != ErrNotFound {
		t.Fatalf("foreign report = %v, want ErrNotFound", err)
	}
	if rep.SealedThrough != nil || len(rep.Segments) != 0 || rep.Availability != nil || rep.Objective != nil {
		t.Fatalf("a wrong-project report leaked tenant data: %+v", rep)
	}
	h, err := st.ServiceHealthNow(ctx, foreign.projectID, f.serviceID)
	if err != ErrNotFound {
		t.Fatalf("foreign health = %v, want ErrNotFound", err)
	}
	if h.SLI == "healthy" || h.Diagnostics == "failing" || len(h.FailingMonitors) != 0 {
		t.Fatalf("a wrong-project health read leaked tenant data: %+v", h)
	}
	points, err := st.ServiceReliabilitySeries(ctx, foreign.projectID, f.serviceID, sealed.Add(-time.Hour), sealed, time.Hour)
	if err != ErrNotFound {
		t.Fatalf("foreign series = %v, want ErrNotFound", err)
	}
	if len(points) != 0 {
		t.Fatalf("a wrong-project series returned %d points", len(points))
	}
	if err := st.UpsertServiceSLATarget(ctx, foreign.projectID, f.serviceID, "30d", 10); err != ErrNotFound {
		t.Fatalf("foreign upsert = %v, want ErrNotFound", err)
	}
	var objective float64
	if err := st.pool.QueryRow(ctx,
		`SELECT objective::float8 FROM sla_targets WHERE service_id = $1 AND window_name = '30d'`,
		f.serviceID).Scan(&objective); err != nil || objective != 99.9 {
		t.Fatalf("objective after the foreign upsert = %v (%v), want the untouched 99.9", objective, err)
	}
}

// [192] P1-4: the series' existence check and its data read share ONE snapshot — a service
// deleted between them must not yield a 200/empty assembled from two worlds. The pinned
// snapshot still answers from the pre-delete world; a fresh call is ErrNotFound.
func TestSeriesExistenceAndDataShareOneSnapshot(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)
	hour := time.Now().UTC().Add(-7 * time.Hour).Truncate(time.Hour)
	plantRange(t, st, ctx, f, f.epochID, hour, hour.Add(10*time.Minute), minute, 0, 0, 0, "sealed")

	tx, _, err := st.beginReportSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	points, err := serviceReliabilitySeriesTx(ctx, tx, f.projectID, f.serviceID, hour, hour.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("series in snapshot: %v", err)
	}
	if len(points) != 1 || points[0].Durations.GoodUs != 10*minute {
		t.Fatalf("the pinned snapshot lost its own world: %+v", points)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := st.ServiceReliabilitySeries(ctx, f.projectID, f.serviceID, hour, hour.Add(time.Hour), time.Hour); err != ErrNotFound {
		t.Fatalf("fresh series after delete = %v, want ErrNotFound", err)
	}
}

// [192] P1-1 + [195] P0 (D-0165): the admissible objective contract (0,100) is
// REPRESENTABLE at four decimals, the canonical value is what the row holds, and the schema
// CHECK (00079) is the final fence for EVERY writer — the API rule and the column agree that
// a zero error budget is not a configuration.
func TestObjectiveIsCanonicalAndRepresentable(t *testing.T) {
	st, ctx := declStore(t)
	f := reportService(t, st, ctx)

	// The maximum admissible objective survives byte-exact: the echo and the row are the
	// same number.
	if err := st.UpsertServiceSLATarget(ctx, f.projectID, f.serviceID, "30d", 99.9999); err != nil {
		t.Fatalf("objective 99.9999: %v", err)
	}
	var stored float64
	if err := st.pool.QueryRow(ctx,
		`SELECT objective::float8 FROM sla_targets WHERE service_id = $1 AND window_name = '30d'`,
		f.serviceID).Scan(&stored); err != nil || stored != 99.9999 {
		t.Fatalf("stored = %v (%v), want exactly 99.9999", stored, err)
	}
	// The monitor scope shares the column and the same bound.
	if _, err := st.UpsertMonitorSLATarget(ctx, f.http, "30d", 99.9999, false, nil); err != nil {
		t.Fatalf("monitor objective 99.9999: %v", err)
	}
	// The SCHEMA fence: a writer that bypasses the application rule still cannot store the
	// zero-error-budget objective.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO sla_targets (service_id, window_name, objective) VALUES ($1, '7d', 100)`,
		f.serviceID); err == nil || !strings.Contains(err.Error(), "sla_targets_objective_chk") {
		t.Fatalf("objective=100 passed the schema fence: %v", err)
	}
}
