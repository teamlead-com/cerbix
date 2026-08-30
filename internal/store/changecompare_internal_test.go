package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D8 — the before/after comparison (func-change-intelligence §7 *Comparison*; invariants
// 10, 11, 12). The contract is PARITY with ServiceReliabilitySeries over the same range and
// snapshot — the figures are sums of the series' own cells — and every withholding is the
// reliability page's.

// compareFixture plants a fully sealed [A − 7h, A + 3h): 100% before A, 90% after A, the era at
// A − 8h and the watermark at A + 3h, with a deploy started at A − 5m and succeeded at A + 30s,
// so T = A. Every horizon's `before` is inside the plant; a 6h `after` reaches past the seal.
func compareFixture(t *testing.T, st *Store, ctx context.Context) (changeFixture, time.Time) {
	t.Helper()
	f := changeService(t, st, ctx)
	a := anchor()
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a.Add(-7*time.Hour), a, minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a, a.Add(3*time.Hour), 54*1_000_000, 6*1_000_000, 0, 0, "sealed")
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-8*time.Hour), a.Add(3*time.Hour))
	mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseStarted, a.Add(-5*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, a.Add(30*time.Second)))
	return f, a
}

// seriesSum sums the series' SEALED cells over [from, to) at the 1h step — the number the
// comparison must equal to the microsecond.
func seriesSum(t *testing.T, st *Store, ctx context.Context, f changeFixture, from, to time.Time) (domain.ReliabilityDurations, int64) {
	t.Helper()
	points, err := st.ServiceReliabilitySeries(ctx, f.projectID, f.serviceID, from, to, time.Hour)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	var d domain.ReliabilityDurations
	var buckets int64
	for _, p := range points {
		if p.Provisional {
			continue
		}
		d = addDurations(d, p.Durations)
		buckets += p.Buckets
	}
	return d, buckets
}

func compareOK(t *testing.T, st *Store, ctx context.Context, f changeFixture, h time.Duration) domain.ChangeComparison {
	t.Helper()
	cmp, err := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "github-actions", "run-1", h)
	if err != nil {
		t.Fatalf("compare %s: %v", h, err)
	}
	return cmp
}

// Parity: before and after equal the sums of ServiceReliabilitySeries over the same bucket
// ranges, to the microsecond; T is the terminal's instant floored to the bucket; delta is
// after − before in availability points.
func TestChangeCompareHasParityWithTheSeriesToTheMicrosecond(t *testing.T) {
	st, ctx := declStore(t)
	f, a := compareFixture(t, st, ctx)
	for _, h := range []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour} {
		cmp := compareOK(t, st, ctx, f, h)
		if !cmp.T.Equal(a) || cmp.Change.Phase != domain.ChangePhaseSucceeded || cmp.Horizon != h.String() {
			t.Fatalf("%s: T=%s change=%+v horizon=%s", h, cmp.T, cmp.Change, cmp.Horizon)
		}
		if !cmp.Before.From.Equal(a.Add(-h)) || !cmp.Before.To.Equal(a) || !cmp.After.From.Equal(a) || !cmp.After.To.Equal(a.Add(h)) {
			t.Fatalf("%s: ranges %+v %+v", h, cmp.Before, cmp.After)
		}
		if cmp.Before.Figure == nil || cmp.Before.Withheld != nil {
			t.Fatalf("%s: before = %+v, want a figure", h, cmp.Before)
		}
		wantBefore, nb := seriesSum(t, st, ctx, f, a.Add(-h), a)
		if cmp.Before.Figure.Durations != wantBefore || cmp.Before.Figure.Buckets != nb || nb != int64(h/time.Minute) {
			t.Fatalf("%s before = %+v (%d buckets), series = %+v (%d)", h, cmp.Before.Figure.Durations, cmp.Before.Figure.Buckets, wantBefore, nb)
		}
		// 6h after reaches A + 6h, beyond sealed_through (A + 3h): pending, no figure, no delta —
		// the pending test states the full contract; here only the parity of what IS stated.
		if h == 6*time.Hour {
			if cmp.After.Figure != nil || cmp.After.Withheld == nil || cmp.After.Withheld.Reason != domain.ChangeCompareWithheldPending || cmp.Delta != nil {
				t.Fatalf("6h after = %+v delta=%v, want pending", cmp.After, cmp.Delta)
			}
			continue
		}
		if cmp.After.Figure == nil || cmp.After.Withheld != nil {
			t.Fatalf("%s: after = %+v, want a figure", h, cmp.After)
		}
		wantAfter, na := seriesSum(t, st, ctx, f, a, a.Add(h))
		if cmp.After.Figure.Durations != wantAfter || cmp.After.Figure.Buckets != na || na != int64(h/time.Minute) {
			t.Fatalf("%s after = %+v (%d buckets), series = %+v (%d)", h, cmp.After.Figure.Durations, cmp.After.Figure.Buckets, wantAfter, na)
		}
		if cmp.Before.Figure.Availability != 100 || cmp.After.Figure.Availability != 90 || cmp.Delta == nil || *cmp.Delta != -10 {
			t.Fatalf("%s: before=%v after=%v delta=%v", h, cmp.Before.Figure.Availability, cmp.After.Figure.Availability, cmp.Delta)
		}
		if cmp.Before.Figure.GoodSeconds != float64(wantBefore.GoodUs)/1e6 || cmp.After.Figure.BadSeconds != float64(wantAfter.BadUs)/1e6 {
			t.Fatalf("%s: seconds are not the µs / 1e6", h)
		}
		if cmp.SealedThrough == nil || !cmp.SealedThrough.Equal(a.Add(3*time.Hour)) {
			t.Fatalf("%s: sealed_through = %v", h, cmp.SealedThrough)
		}
	}
	// Two reads in one snapshot state are equal byte for byte (AsOf aside).
	x, y := compareOK(t, st, ctx, f, time.Hour), compareOK(t, st, ctx, f, time.Hour)
	x.AsOf, y.AsOf = time.Time{}, time.Time{}
	if !reflect.DeepEqual(x, y) {
		t.Fatalf("two reads differ:\n%+v\n%+v", x, y)
	}
	// A repaired bucket between two calls: the second equals the series' NEW sum (parity, not
	// persistence).
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a.Add(10*time.Minute), a.Add(11*time.Minute), 0, minute, 0, 0, "sealed")
	z := compareOK(t, st, ctx, f, time.Hour)
	wantAfter, _ := seriesSum(t, st, ctx, f, a, a.Add(time.Hour))
	if z.After.Figure.Durations != wantAfter || z.After.Figure.Durations == y.After.Figure.Durations {
		t.Fatalf("after a repair: %+v, series %+v, previous %+v", z.After.Figure.Durations, wantAfter, y.After.Figure.Durations)
	}
}

// `after` is pending with sealed_through stated and NO figure whenever T + h > sealed_through;
// delta is absent; `before` is unaffected. A `before` that reaches past the watermark is
// undecidable with the page's own stale-watermark reason.
func TestChangeCompareEitherSideIsPendingPastSealedThrough(t *testing.T) {
	st, ctx := declStore(t)
	f, a := compareFixture(t, st, ctx)
	// 6h from the fixture: T + 6h = A + 6h > A + 3h.
	cmp := compareOK(t, st, ctx, f, 6*time.Hour)
	if cmp.After.Figure != nil || cmp.After.Withheld == nil || cmp.After.Withheld.Reason != domain.ChangeCompareWithheldPending ||
		cmp.After.Withheld.SealedThrough == nil || !cmp.After.Withheld.SealedThrough.Equal(a.Add(3*time.Hour)) || cmp.Delta != nil {
		t.Fatalf("after = %+v delta=%v, want pending with sealed_through %s and no delta", cmp.After, cmp.Delta, a.Add(3*time.Hour))
	}
	if cmp.Before.Figure == nil {
		t.Fatalf("before = %+v, want a figure", cmp.Before)
	}
	// Move the watermark to A + 30m: 1h after is pending; 15m after is a figure.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-4*time.Hour), a.Add(30*time.Minute))
	if c := compareOK(t, st, ctx, f, time.Hour); c.After.Withheld == nil || c.After.Withheld.Reason != domain.ChangeCompareWithheldPending || c.Delta != nil {
		t.Fatalf("1h after with sealed_through A+30m = %+v", c.After)
	}
	if c := compareOK(t, st, ctx, f, 15*time.Minute); c.After.Figure == nil || c.Delta == nil {
		t.Fatalf("15m after with sealed_through A+30m = %+v", c.After)
	}
	// Sealing T + h makes `after` ELIGIBLE, it does not freeze it.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-4*time.Hour), a.Add(time.Hour))
	if c := compareOK(t, st, ctx, f, time.Hour); c.After.Figure == nil {
		t.Fatalf("after exactly sealed to T + h = %+v", c.After)
	}
	// The watermark behind T: `before` reaches past it too, so BOTH sides are pending with the
	// watermark stated (owner decision D-0211) — not `undecidable`: the facts are not yet sealed,
	// they are not undecidable.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-4*time.Hour), a.Add(-10*time.Minute))
	c := compareOK(t, st, ctx, f, time.Hour)
	if c.Before.Withheld == nil || c.Before.Withheld.Reason != domain.ChangeCompareWithheldPending || c.Before.Withheld.Detail != "" ||
		c.Before.Withheld.SealedThrough == nil || !c.Before.Withheld.SealedThrough.Equal(a.Add(-10*time.Minute)) ||
		c.After.Withheld == nil || c.After.Withheld.Reason != domain.ChangeCompareWithheldPending || c.Delta != nil {
		t.Fatalf("watermark behind T: before=%+v after=%+v delta=%v, want both pending with sealed_through and no delta", c.Before, c.After, c.Delta)
	}
	// Nothing sealed at all: both sides no_facts.
	if _, err := st.pool.Exec(ctx, `UPDATE service_materialization SET sealed_through = NULL WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	c = compareOK(t, st, ctx, f, time.Hour)
	if c.Before.Withheld == nil || c.Before.Withheld.Reason != domain.ChangeCompareWithheldNoFacts || c.After.Withheld == nil || c.After.Withheld.Reason != domain.ChangeCompareWithheldNoFacts || c.SealedThrough != nil {
		t.Fatalf("nothing sealed: before=%+v after=%+v", c.Before, c.After)
	}
}

// A revision or epoch boundary inside a side is definition_changed; a range the report path
// would withhold is undecidable with the SAME reason string; no buckets is no_facts.
func TestChangeCompareWithholdsWithThePagesOwnReasons(t *testing.T) {
	st, ctx := declStore(t)
	f, a := compareFixture(t, st, ctx)

	// A second revision (and epoch) planted over the last 30 minutes before T.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("revision 2: %v", err)
	}
	var epoch2 string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 ORDER BY epoch_seq DESC LIMIT 1`, f.serviceID).Scan(&epoch2); err != nil {
		t.Fatal(err)
	}
	if epoch2 == f.epochID {
		t.Fatal("fixture broke: no new epoch")
	}
	plantRange(t, st, ctx, f.reportFixture, epoch2, a.Add(-30*time.Minute), a, minute, 0, 0, 0, "sealed")
	c := compareOK(t, st, ctx, f, time.Hour)
	if c.Before.Withheld == nil || c.Before.Withheld.Reason != domain.ChangeCompareWithheldDefinitionChanged || c.Before.Figure != nil {
		t.Fatalf("revision boundary inside before = %+v", c.Before)
	}
	if c.After.Figure == nil || c.Delta != nil {
		t.Fatalf("after must still be a figure and delta absent: %+v %v", c.After, c.Delta)
	}
	// 15m before lies wholly inside epoch2: a figure again — the boundary is what withholds.
	if c := compareOK(t, st, ctx, f, 15*time.Minute); c.Before.Figure == nil {
		t.Fatalf("15m before inside one epoch = %+v", c.Before)
	}
	// Same-revision epoch boundary: also definition_changed (D8 names epoch boundaries too).
	var epoch3 string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO service_evaluation_epochs (service_id, project_id, epoch_seq, revision_id, effective_at, snapshot_hash)
		SELECT service_id, project_id, epoch_seq + 1, revision_id, effective_at + interval '1 minute', 'epoch3'
		  FROM service_evaluation_epochs WHERE id = $1 RETURNING id`, epoch2).Scan(&epoch3); err != nil {
		t.Fatal(err)
	}
	plantRange(t, st, ctx, f.reportFixture, epoch3, a.Add(-5*time.Minute), a, minute, 0, 0, 0, "sealed")
	if c := compareOK(t, st, ctx, f, 15*time.Minute); c.Before.Withheld == nil || c.Before.Withheld.Reason != domain.ChangeCompareWithheldDefinitionChanged {
		t.Fatalf("epoch boundary inside before = %+v", c.Before)
	}

}

// A range the report path would withhold is undecidable with the SAME reason string; no buckets
// is no_facts.
func TestChangeCompareUndecidableAndNoFactsUseThePagesReasons(t *testing.T) {
	st, ctx := declStore(t)
	var c domain.ChangeComparison
	// The page's verdicts, same reason strings: a hole → storage_gap; unknown only →
	// zero_decidable_time; a side reaching before the era → window_precedes_materialization_era.
	g, ga := compareFixture(t, st, ctx)
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_reliability_buckets WHERE service_id = $1 AND bucket_start >= $2 AND bucket_start < $3`,
		g.serviceID, ga.Add(-30*time.Minute), ga.Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	c = compareOK(t, st, ctx, g, time.Hour)
	if c.Before.Withheld == nil || c.Before.Withheld.Reason != domain.ChangeCompareWithheldUndecidable || c.Before.Withheld.Detail != domain.ServiceReportReasonStorageGap {
		t.Fatalf("hole inside before = %+v", c.Before)
	}
	plantRange(t, st, ctx, g.reportFixture, g.epochID, ga.Add(-time.Hour), ga, 0, 0, minute, 0, "sealed")
	c = compareOK(t, st, ctx, g, time.Hour)
	if c.Before.Withheld == nil || c.Before.Withheld.Detail != domain.ServiceReportReasonZeroDecidable {
		t.Fatalf("unknown-only before = %+v", c.Before)
	}
	setWatermark(t, st, ctx, g.reportFixture, ga.Add(-30*time.Minute), ga.Add(3*time.Hour))
	c = compareOK(t, st, ctx, g, time.Hour)
	if c.Before.Withheld == nil || c.Before.Withheld.Detail != domain.ServiceReportReasonEraShort {
		t.Fatalf("before reaching past the era = %+v", c.Before)
	}
	// No buckets at all in the range → no_facts (the after side of a fresh fixture, emptied).
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_reliability_buckets WHERE service_id = $1 AND bucket_start >= $2`, g.serviceID, ga); err != nil {
		t.Fatal(err)
	}
	if c := compareOK(t, st, ctx, g, time.Hour); c.After.Withheld == nil || c.After.Withheld.Reason != domain.ChangeCompareWithheldNoFacts {
		t.Fatalf("no buckets after = %+v", c.After)
	}

}

// Provisional cells never enter a stated number (the page would not quote them either).
func TestChangeCompareProvisionalCellsNeverEnterAFigure(t *testing.T) {
	st, ctx := declStore(t)
	// Provisional cells never enter a stated number: a provisional bucket inside `after` reads
	// as a hole (the page would not quote it either).
	h, b := compareFixture(t, st, ctx)
	plantRange(t, st, ctx, h.reportFixture, h.epochID, b.Add(10*time.Minute), b.Add(11*time.Minute), minute, 0, 0, 0, "provisional")
	if c := compareOK(t, st, ctx, h, time.Hour); c.After.Withheld == nil || c.After.Withheld.Detail != domain.ServiceReportReasonStorageGap {
		t.Fatalf("provisional cell inside after = %+v", c.After)
	}
}

// horizon=2h → horizon_invalid; a group with only started → no_terminal_phase; an unknown
// identity or a foreign service → ErrNotFound.
func TestChangeCompareRefusesHorizonIdentityAndStartedOnly(t *testing.T) {
	st, ctx := declStore(t)
	f, a := compareFixture(t, st, ctx)
	code := func(err error) string {
		var ce *domain.ChangeError
		if errors.As(err, &ce) {
			return ce.Code
		}
		return ""
	}
	if _, err := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "github-actions", "run-1", 2*time.Hour); code(err) != domain.ChangeErrHorizonInvalid {
		t.Fatalf("2h: %v", err)
	}
	mustRecord(t, st, ctx, changeInput(f, "run-2", domain.ChangePhaseStarted, a))
	if _, err := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "github-actions", "run-2", time.Hour); code(err) != domain.ChangeErrNoTerminalPhase {
		t.Fatalf("started only: %v", err)
	}
	if _, err := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "github-actions", "nope", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown identity: %v", err)
	}
	if _, err := st.ServiceReliabilityCompare(ctx, f.otherProjectID, f.serviceID, "github-actions", "run-1", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign project: %v", err)
	}
	if _, err := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "Bad_Source", "run-1", time.Hour); code(err) != domain.ChangeErrSourceInvalid {
		t.Fatalf("bad source: %v", err)
	}
	// Terminal alone: T from the only phase.
	mustRecord(t, st, ctx, changeInput(f, "run-3", domain.ChangePhaseFailed, a.Add(90*time.Second)))
	if c, err := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "github-actions", "run-3", 15*time.Minute); err != nil || !c.T.Equal(a.Add(time.Minute)) {
		t.Fatalf("terminal alone: T=%v err=%v", c.T, err)
	}
}
