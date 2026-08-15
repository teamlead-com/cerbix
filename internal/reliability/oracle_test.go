package reliability

import (
	"math/rand"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// grain is the resolution every generated instant is aligned to. It is sub-second on
// purpose — that is where a reducer that samples at an endpoint instead of integrating
// falls apart — while still letting the oracle below be EXACT rather than approximate.
const grain = 250 * time.Millisecond

const slots = int(time.Minute / grain)

// oracleReduce is a deliberately naive, independent implementation: walk the bucket in
// fixed `grain` slices and add each slice whole. Because every generated breakpoint is
// grain-aligned, no member's state can change inside a slice, so this is exact — and it
// shares no code with the piecewise reducer beyond the aggregation the test is not trying
// to re-derive.
func oracleReduce(in Input) Durations {
	series := buildSeries(in.Members, in.Observations)
	var d Durations
	for i := 0; i < slots; i++ {
		t := in.Start.Add(time.Duration(i) * grain)
		out := aggregate(censusAt(series, in.Maintenance, in.Policies, t), in.Policies)
		d.add(out, grain)
	}
	return d
}

// randomInput builds a bucket whose every instant is grain-aligned.
func randomInput(rng *rand.Rand) Input {
	regions := []string{"core", "geo1", "geo2"}
	nRegions := 1 + rng.Intn(3)
	declaredPerRegion := map[string]int{}

	var members []Member
	var obs []Observation
	id := 0
	for r := 0; r < nRegions; r++ {
		region := regions[r]
		n := 1 + rng.Intn(3)
		declaredPerRegion[region] = n
		for k := 0; k < n; k++ {
			id++
			name := string(rune('a'+id-1)) + region
			isPush := rng.Intn(4) == 0
			typ := domain.MonitorHTTP
			if isPush {
				typ = domain.MonitorPush
			}
			// Stale windows deliberately span from "expires inside the bucket" to
			// "outlives it", so both branches are exercised.
			stale := time.Duration(1+rng.Intn(8)) * 10 * time.Second
			m := Member{
				MonitorID: name, Type: typ, Region: region,
				Enabled:    rng.Intn(8) != 0,
				StaleAfter: stale,
			}
			if isPush {
				m.ArmedAt = bucketStart.Add(-time.Duration(rng.Intn(240)) * grain)
			}
			members = append(members, m)

			for o := 0; o < rng.Intn(4); o++ {
				// Observations may precede the bucket: sample-and-hold needs the one in
				// force when it opened.
				off := time.Duration(rng.Intn(2*slots)-slots) * grain
				obs = append(obs, Observation{MonitorID: name, Ts: bucketStart.Add(off), Up: rng.Intn(3) != 0})
			}
		}
	}

	var spans []MaintenanceSpan
	for s := 0; s < rng.Intn(3); s++ {
		from := time.Duration(rng.Intn(slots)) * grain
		to := from + time.Duration(1+rng.Intn(slots))*grain
		mid := ""
		if rng.Intn(2) == 0 && len(members) > 0 {
			mid = members[rng.Intn(len(members))].MonitorID
		}
		spans = append(spans, MaintenanceSpan{
			ID: "mw", MonitorID: mid,
			From: bucketStart.Add(from), To: bucketStart.Add(to),
		})
	}

	modes := []domain.AggMode{domain.AggAll, domain.AggAny, domain.AggQuorum}
	missing := []domain.MissingDataPolicy{domain.MissingUnknown, domain.MissingBad, domain.MissingIgnore}
	maxDeclared := 0
	for _, n := range declaredPerRegion {
		if n > maxDeclared {
			maxDeclared = n
		}
	}
	p := Policies{
		Aggregation:         domain.AggregationPolicy{Mode: modes[rng.Intn(len(modes))], DegradedMin: 1 + rng.Intn(maxDeclared), HealthyMin: maxDeclared},
		MissingData:         missing[rng.Intn(len(missing))],
		MaintenanceExcludes: true,
	}
	if p.Aggregation.HealthyMin < p.Aggregation.DegradedMin {
		p.Aggregation.HealthyMin = p.Aggregation.DegradedMin
	}

	return Input{
		Start: bucketStart, End: bucketEnd,
		Members: members, Observations: obs, Maintenance: spans,
		Policies: domain.ApplyServicePolicyDefaults(p, declaredPerRegion, nRegions),
	}
}

// TestReducerMatchesOracle is the property the whole duration model rests on: the
// piecewise reducer, which splits the bucket only where a state can change, must agree
// exactly with a naive slice-by-slice walk. Disagreement means the breakpoint set is
// incomplete — a state change the reducer never noticed — and that is invisible in any
// hand-written example.
func TestReducerMatchesOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(20260816))
	for i := 0; i < 3000; i++ {
		in := randomInput(rng)
		got, err := Reduce(in)
		if err != nil {
			t.Fatalf("case %d: Reduce: %v\ninput: %+v", i, err, in)
		}
		want := oracleReduce(in)
		if got.Durations != want {
			t.Fatalf("case %d: reducer and oracle disagree\n got: %+v\nwant: %+v\ninput: %+v", i, got.Durations, want, in)
		}
	}
}

// Conservation is checked inside Reduce, but assert it independently over the same corpus
// so a future change that relaxes the internal check cannot pass silently.
func TestBothAxesAlwaysConserve(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))
	for i := 0; i < 2000; i++ {
		in := randomInput(rng)
		b, err := Reduce(in)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if b.Durations.Total() != time.Minute {
			t.Fatalf("case %d: availability axis sums to %s", i, b.Durations.Total())
		}
		if b.Durations.HealthTotal() != time.Minute {
			t.Fatalf("case %d: health axis sums to %s", i, b.Durations.HealthTotal())
		}
	}
}

// Excluded time is shared by both axes, and health-excluded never appears on its own.
func TestExcludedIsSharedByBothAxes(t *testing.T) {
	rng := rand.New(rand.NewSource(777))
	for i := 0; i < 1000; i++ {
		in := randomInput(rng)
		b, err := Reduce(in)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		health := b.Durations.Healthy + b.Durations.Degraded + b.Durations.Down + b.Durations.HealthUnknown
		avail := b.Durations.Good + b.Durations.Bad + b.Durations.Unknown
		if health != avail {
			t.Fatalf("case %d: non-excluded time differs between axes: health %s vs availability %s", i, health, avail)
		}
	}
}

// A GOOD duration can never be produced with no eligible member: that is the vacuous
// truth this whole feature exists to prevent, and it is worth asserting over the corpus
// rather than only in the hand-written case.
func TestNoGoodTimeWithoutAnEligibleMember(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 2000; i++ {
		in := randomInput(rng)
		everEligible := false
		series := buildSeries(in.Members, in.Observations)
		for s := 0; s < slots; s++ {
			at := in.Start.Add(time.Duration(s) * grain)
			for _, tl := range censusAt(series, in.Maintenance, in.Policies, at) {
				if tl.eligible() > 0 {
					everEligible = true
				}
			}
		}
		b, err := Reduce(in)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !everEligible && b.Durations.Good > 0 {
			t.Fatalf("case %d: %s of GOOD with no eligible member at any instant", i, b.Durations.Good)
		}
	}
}
