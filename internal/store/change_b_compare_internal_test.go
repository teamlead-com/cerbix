package store

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D8 at its boundaries (func-change-intelligence §7 *Comparison*, invariants 10–12;
// iter-0165 task 2, Agent B; D-0211 side-neutral clamp): parity with the canonical buckets AND
// with the series for every horizon and every alignment of T, the sealed_through clamp to the
// microsecond on either side, and the windows that touch the era.

// compareBFixture plants a varied, fully sealed [A − 25h, A + 25h) around an HOUR-aligned A in
// the past (era A − 30h, watermark A + 25h): 100 % before A and 90 % after, with overlays that
// give every horizon a different answer, and three changes whose T falls on the hour, mid-hour
// and on an odd minute.
func compareBFixture(t *testing.T, st *Store, ctx context.Context) (changeFixture, time.Time) {
	t.Helper()
	f := changeService(t, st, ctx)
	a := time.Now().UTC().Add(-30 * time.Hour).Truncate(time.Hour)
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a.Add(-25*time.Hour), a, minute, 0, 0, 0, "sealed")
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a, a.Add(25*time.Hour), 54*1_000_000, 6*1_000_000, 0, 0, "sealed")
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a.Add(-3*time.Hour), a.Add(-2*time.Hour), 57*1_000_000, 3*1_000_000, 0, 0, "sealed")
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a.Add(-50*time.Minute), a.Add(-40*time.Minute), 42*1_000_000, 18*1_000_000, 0, 0, "sealed")
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a.Add(2*time.Hour), a.Add(150*time.Minute), 48*1_000_000, 12*1_000_000, 0, 0, "sealed")
	plantRange(t, st, ctx, f.reportFixture, f.epochID, a.Add(7*time.Minute), a.Add(9*time.Minute), 30*1_000_000, 0, 30*1_000_000, 0, "sealed")
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-30*time.Hour), a.Add(25*time.Hour))
	// The changes are older than max_past allows through the write path: planted.
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "boundary", domain.ChangeKindDeploy, domain.ChangePhaseStarted, a.Add(-5*time.Minute))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "boundary", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, a.Add(30*time.Second))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "midhour", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, a.Add(30*time.Minute+30*time.Second))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "odd", domain.ChangeKindRollback, domain.ChangePhaseFailed, a.Add(7*time.Minute+59*time.Second))
	return f, a
}

// bucketSum is the canonical facts summed DIRECTLY — sealed buckets in [from, to) — the number
// every reader of the facts must agree with.
func bucketSum(t *testing.T, st *Store, ctx context.Context, serviceID string, from, to time.Time) (domain.ReliabilityDurations, int64) {
	t.Helper()
	var d domain.ReliabilityDurations
	var n int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(good_us), 0)::bigint, COALESCE(sum(bad_us), 0)::bigint,
		       COALESCE(sum(unknown_us), 0)::bigint, COALESCE(sum(excluded_us), 0)::bigint,
		       COALESCE(sum(healthy_us), 0)::bigint, COALESCE(sum(degraded_us), 0)::bigint,
		       COALESCE(sum(down_us), 0)::bigint, COALESCE(sum(health_unknown_us), 0)::bigint
		  FROM service_reliability_buckets
		 WHERE service_id = $1 AND state = 'sealed' AND bucket_start >= $2 AND bucket_start < $3`,
		serviceID, from, to).Scan(&n, &d.GoodUs, &d.BadUs, &d.UnknownUs, &d.ExcludedUs, &d.HealthyUs, &d.DegradedUs, &d.DownUs, &d.HealthUnknownUs); err != nil {
		t.Fatalf("bucket sum: %v", err)
	}
	return d, n
}

// For every change (T on the hour, mid-hour, on an odd minute) and every horizon (15m, 1h, 6h,
// 24h) both sides are figures whose Durations and Buckets equal the canonical buckets summed
// directly AND the hourly series summed over the same range (and the daily series for 24h);
// the hourly cells the comparison reads are NOT pre-aggregated hours — a 15-minute side sums
// exactly its 15 buckets, a mid-hour side its two partial hours. Availability is good/(good+bad).
func TestChangeCompareEqualsTheBucketsAndTheSeriesForEveryHorizonAndAlignment(t *testing.T) {
	st, ctx := declStore(t)
	f, a := compareBFixture(t, st, ctx)
	changes := []struct {
		ext string
		t   time.Time
	}{
		{"boundary", a},
		{"midhour", a.Add(30 * time.Minute)},
		{"odd", a.Add(7 * time.Minute)},
	}
	for _, c := range changes {
		for _, h := range domain.ChangeCompareHorizons {
			cmp, err := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "ci", c.ext, h)
			if err != nil {
				t.Fatalf("%s/%s: %v", c.ext, h, err)
			}
			if !cmp.T.Equal(c.t) {
				t.Fatalf("%s/%s: T = %s, want %s (the terminal floored to the minute)", c.ext, h, cmp.T, c.t)
			}
			for _, side := range []struct {
				name string
				s    domain.ChangeCompareSide
			}{{"before", cmp.Before}, {"after", cmp.After}} {
				if side.s.Figure == nil {
					t.Fatalf("%s/%s %s withheld: %+v", c.ext, h, side.name, side.s.Withheld)
				}
				want, n := bucketSum(t, st, ctx, f.serviceID, side.s.From, side.s.To)
				if side.s.Figure.Durations != want || side.s.Figure.Buckets != n || n != int64(h/time.Minute) {
					t.Fatalf("%s/%s %s = %+v (%d), buckets = %+v (%d)", c.ext, h, side.name, side.s.Figure.Durations, side.s.Figure.Buckets, want, n)
				}
				series, sn := seriesSum(t, st, ctx, f, side.s.From, side.s.To)
				if series != want || sn != n {
					t.Fatalf("%s/%s %s: the hourly series sums to %+v (%d), the buckets to %+v (%d)", c.ext, h, side.name, series, sn, want, n)
				}
				if h == 24*time.Hour {
					points, err := st.ServiceReliabilitySeries(ctx, f.projectID, f.serviceID, side.s.From, side.s.To, 24*time.Hour)
					if err != nil {
						t.Fatal(err)
					}
					var daily domain.ReliabilityDurations
					for _, p := range points {
						daily = addDurations(daily, p.Durations)
					}
					if daily != want {
						t.Fatalf("%s/24h %s: the daily series sums to %+v, the buckets to %+v", c.ext, side.name, daily, want)
					}
				}
				wantAvail := float64(want.GoodUs) / float64(want.GoodUs+want.BadUs) * 100
				if math.Abs(side.s.Figure.Availability-wantAvail) > 1e-9 {
					t.Fatalf("%s/%s %s availability = %v, want %v", c.ext, h, side.name, side.s.Figure.Availability, wantAvail)
				}
				if side.s.Figure.GoodSeconds != float64(want.GoodUs)/1e6 || side.s.Figure.UnknownSeconds != float64(want.UnknownUs)/1e6 {
					t.Fatalf("%s/%s %s: seconds are not µs/1e6", c.ext, h, side.name)
				}
			}
			if cmp.Delta == nil || math.Abs(*cmp.Delta-(cmp.After.Figure.Availability-cmp.Before.Figure.Availability)) > 1e-9 {
				t.Fatalf("%s/%s: delta = %v", c.ext, h, cmp.Delta)
			}
		}
	}
	// The overlays make the alignments distinguishable — the odd change's 15m `after` and the
	// boundary change's both hold the two unknown-only minutes, but at different offsets, so
	// their durations agree on unknown time and differ elsewhere.
	odd, _ := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "ci", "odd", 15*time.Minute)
	bnd, _ := st.ServiceReliabilityCompare(ctx, f.projectID, f.serviceID, "ci", "boundary", 15*time.Minute)
	if odd.After.Figure.UnknownSeconds != 60 || bnd.After.Figure.UnknownSeconds != 60 || odd.Before.Figure.Durations == bnd.Before.Figure.Durations {
		t.Fatalf("the fixture is not distinguishing alignments: odd=%+v boundary=%+v", odd.Before.Figure, bnd.Before.Figure)
	}
}

// The sealed_through clamp is exact to the microsecond and side-neutral (D-0211): a side that
// ends exactly AT sealed_through is a figure; one that ends one second past it is `pending` with
// sealed_through stated and no delta; a seal behind T makes `before` pending too; a seal exactly
// at T leaves a 15m `before` eligible while `after` is pending.
func TestChangeCompareASideEndingExactlyAtSealedThroughIsEligibleAndOneSecondPastIsPending(t *testing.T) {
	st, ctx := declStore(t)
	f, a := compareFixture(t, st, ctx) // T = A; era A − 8h; buckets [A − 7h, A + 3h)
	pending := func(side domain.ChangeCompareSide, sealed time.Time) bool {
		return side.Figure == nil && side.Withheld != nil && side.Withheld.Reason == domain.ChangeCompareWithheldPending &&
			side.Withheld.SealedThrough != nil && side.Withheld.SealedThrough.Equal(sealed)
	}
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-8*time.Hour), a.Add(time.Hour))
	if c := compareOK(t, st, ctx, f, time.Hour); c.After.Figure == nil || c.Delta == nil || !c.SealedThrough.Equal(a.Add(time.Hour)) {
		t.Fatalf("after ending exactly at the seal = %+v delta=%v", c.After, c.Delta)
	}
	seal := a.Add(time.Hour - time.Second)
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-8*time.Hour), seal)
	if c := compareOK(t, st, ctx, f, time.Hour); !pending(c.After, seal) || c.Delta != nil || c.Before.Figure == nil {
		t.Fatalf("after ending one second past the seal = %+v delta=%v before=%+v", c.After, c.Delta, c.Before)
	}
	// Still eligible for the horizon that fits under the seal.
	if c := compareOK(t, st, ctx, f, 15*time.Minute); c.After.Figure == nil || c.Delta == nil {
		t.Fatalf("15m after under the seal = %+v", c.After)
	}
	// The seal behind T: `before` ends at T, past the seal — pending on the before side too.
	seal = a.Add(-time.Second)
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-8*time.Hour), seal)
	if c := compareOK(t, st, ctx, f, 15*time.Minute); !pending(c.Before, seal) || !pending(c.After, seal) || c.Delta != nil {
		t.Fatalf("seal one second behind T: before=%+v after=%+v", c.Before, c.After)
	}
	// The seal exactly at T: `before` ends AT the seal — eligible; `after` pending.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-8*time.Hour), a)
	if c := compareOK(t, st, ctx, f, 15*time.Minute); c.Before.Figure == nil || !pending(c.After, a) || c.Delta != nil {
		t.Fatalf("seal exactly at T: before=%+v after=%+v", c.Before, c.After)
	}
	// One microsecond makes the difference.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-8*time.Hour), a.Add(-time.Microsecond))
	if c := compareOK(t, st, ctx, f, 15*time.Minute); !pending(c.Before, a.Add(-time.Microsecond)) {
		t.Fatalf("seal one microsecond behind T: before=%+v", c.Before)
	}
}

// A side that reaches before the era is withheld with the page's own reason,
// `window_precedes_materialization_era`, whether it CROSSES era_start or lies entirely before it
// (both sides when the era starts after T); a side with no sealed bucket at all is `no_facts`
// even when it also precedes the era — the no-facts answer is decided first.
func TestChangeCompareSidesTouchingTheEraCarryThePagesEraReasonOrNoFacts(t *testing.T) {
	st, ctx := declStore(t)
	f, a := compareFixture(t, st, ctx) // buckets [A − 7h, A + 3h); T = A
	undecidableEra := func(side domain.ChangeCompareSide) bool {
		return side.Figure == nil && side.Withheld != nil && side.Withheld.Reason == domain.ChangeCompareWithheldUndecidable &&
			side.Withheld.Detail == domain.ServiceReportReasonEraShort
	}
	// Crossing: the era starts 30 minutes before T.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-30*time.Minute), a.Add(3*time.Hour))
	c := compareOK(t, st, ctx, f, time.Hour)
	if !undecidableEra(c.Before) || c.After.Figure == nil || c.Delta != nil {
		t.Fatalf("1h before crossing the era = %+v; after = %+v", c.Before, c.After)
	}
	if c := compareOK(t, st, ctx, f, 15*time.Minute); c.Before.Figure == nil {
		t.Fatalf("15m before inside the era = %+v", c.Before)
	}
	// Exactly at the era: a side starting AT era_start is inside it.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-time.Hour), a.Add(3*time.Hour))
	if c := compareOK(t, st, ctx, f, time.Hour); c.Before.Figure == nil {
		t.Fatalf("1h before starting exactly at the era = %+v", c.Before)
	}
	setWatermark(t, st, ctx, f.reportFixture, a.Add(-time.Hour+time.Microsecond), a.Add(3*time.Hour))
	if c := compareOK(t, st, ctx, f, time.Hour); !undecidableEra(c.Before) {
		t.Fatalf("1h before starting one microsecond before the era = %+v", c.Before)
	}
	// Entirely before the era, with buckets: both sides carry the era reason, no delta.
	setWatermark(t, st, ctx, f.reportFixture, a.Add(2*time.Hour), a.Add(3*time.Hour))
	c = compareOK(t, st, ctx, f, time.Hour)
	if !undecidableEra(c.Before) || !undecidableEra(c.After) || c.Delta != nil {
		t.Fatalf("both sides before the era: before=%+v after=%+v", c.Before, c.After)
	}
	// Entirely before the era WITHOUT buckets: no_facts wins.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_reliability_buckets WHERE service_id = $1 AND bucket_start < $2`, f.serviceID, a); err != nil {
		t.Fatal(err)
	}
	c = compareOK(t, st, ctx, f, time.Hour)
	if c.Before.Withheld == nil || c.Before.Withheld.Reason != domain.ChangeCompareWithheldNoFacts || !undecidableEra(c.After) {
		t.Fatalf("no buckets before the era: before=%+v after=%+v", c.Before, c.After)
	}
}
