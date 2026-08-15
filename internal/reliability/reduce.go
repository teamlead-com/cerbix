package reliability

import (
	"fmt"
	"time"
)

// Input is everything Reduce reads. There is no clock and no store here: `now` enters only
// as the caller's bucket bound, which is what makes the determinism property of §10.8
// testable rather than aspirational.
type Input struct {
	// Start and End are the canonical bucket, half-open [Start, End). An observation at
	// exactly End belongs to the next bucket.
	Start, End time.Time
	// Members is the evaluation epoch's snapshot, not the live monitor rows.
	Members []Member
	// Observations may include instants BEFORE Start: sample-and-hold needs the one in
	// force when the bucket opened.
	Observations []Observation
	// Maintenance holds effective spans, already resolved per §10.9.
	Maintenance []MaintenanceSpan
	Policies    Policies
}

// Reduce evaluates one canonical bucket.
//
// The bucket is split at every breakpoint where some member's effective state can change,
// the aggregation runs once per sub-interval, and each sub-interval's exact length
// accumulates into one bin of each axis. Both axes are then checked to conserve: a
// reducer that loses time produces a budget nobody can reconcile, and the check costs two
// additions.
func Reduce(in Input) (Bucket, error) {
	if !in.End.After(in.Start) {
		return Bucket{}, fmt.Errorf("reliability: bucket end %s is not after start %s", in.End, in.Start)
	}
	length := in.End.Sub(in.Start)

	b := Bucket{Start: in.Start, End: in.End}
	b.Provenance.Declared = len(in.Members)

	if len(in.Members) == 0 {
		// A service with no declared reliability inputs has no facts at all (§9.5); if a
		// caller reduces one anyway, the whole bucket is UNKNOWN and never GOOD.
		b.Durations.Unknown = length
		b.Durations.HealthUnknown = length
		return b, nil
	}

	series := buildSeries(in.Members, in.Observations)
	points := breakpoints(series, in.Maintenance, in.Start, in.End)

	causes := newCauseSet()
	var weakened []Weakened

	for i, t := range points {
		next := in.End
		if i+1 < len(points) {
			next = points[i+1]
		}
		span := next.Sub(t)
		if span <= 0 {
			continue
		}
		tallies := censusAt(series, in.Maintenance, in.Policies, t)
		out := aggregate(tallies, in.Policies)
		b.Durations.add(out, span)

		// Provenance collects the members that CAUSED a duration, so the causes recorded
		// are the ones a reader would ask about rather than every member of every instant.
		switch out.Availability {
		case AvailBad:
			for _, tl := range tallies {
				causes.addBad(tl.badCauses)
			}
		case AvailUnknown:
			for _, tl := range tallies {
				causes.addUnknown(tl.unknownCauses)
			}
		case AvailExcluded:
			for _, tl := range tallies {
				causes.addExcluded(tl.excludedCauses)
			}
		}
		weakened = mergeWeakened(weakened, out.Weakened)
	}

	if got := b.Durations.Total(); got != length {
		return Bucket{}, fmt.Errorf("reliability: availability axis does not conserve: %s of %s", got, length)
	}
	if got := b.Durations.HealthTotal(); got != length {
		return Bucket{}, fmt.Errorf("reliability: health axis does not conserve: %s of %s", got, length)
	}

	b.Provenance.Bad, b.Provenance.Unknown, b.Provenance.Excluded, b.Provenance.Overflow = causes.result()
	b.Provenance.Weakened = weakened
	return b, nil
}

// causeSet deduplicates provenance references and bounds each class at MaxCauses, counting
// what did not fit rather than truncating silently.
type causeSet struct {
	bad, unknown, excluded []MemberCause
	seenBad                map[string]struct{}
	seenUnknown            map[string]struct{}
	seenExcluded           map[string]struct{}
	overflow               int
}

func newCauseSet() *causeSet {
	return &causeSet{
		seenBad:      map[string]struct{}{},
		seenUnknown:  map[string]struct{}{},
		seenExcluded: map[string]struct{}{},
	}
}

func (c *causeSet) add(dst *[]MemberCause, seen map[string]struct{}, in []MemberCause) {
	for _, m := range in {
		key := m.MonitorID + "|" + m.Region + "|" + string(m.Reason)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if len(*dst) >= MaxCauses {
			c.overflow++
			continue
		}
		*dst = append(*dst, m)
	}
}

func (c *causeSet) addBad(in []MemberCause)      { c.add(&c.bad, c.seenBad, in) }
func (c *causeSet) addUnknown(in []MemberCause)  { c.add(&c.unknown, c.seenUnknown, in) }
func (c *causeSet) addExcluded(in []MemberCause) { c.add(&c.excluded, c.seenExcluded, in) }

func (c *causeSet) result() ([]MemberCause, []MemberCause, []MemberCause, int) {
	return c.bad, c.unknown, c.excluded, c.overflow
}

// mergeWeakened keeps one record per region: a clamp that held for part of a bucket is
// reported once, with the widest gap between the declared and the effective threshold.
func mergeWeakened(acc []Weakened, in []Weakened) []Weakened {
	for _, w := range in {
		found := false
		for i := range acc {
			if acc[i].Region != w.Region {
				continue
			}
			found = true
			if w.EffectiveDegraded < acc[i].EffectiveDegraded || w.EffectiveHealthy < acc[i].EffectiveHealthy {
				acc[i] = w
			}
			break
		}
		if !found {
			acc = append(acc, w)
		}
	}
	return acc
}
