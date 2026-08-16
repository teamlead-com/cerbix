package reliability

import (
	"sort"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// tally is one region's member census for a single sub-interval, in the variables §9.3
// names: d declared, x excluded, i ignored, n = d − x − i eligible, partitioned after the
// policy into g good, b bad, u unknown.
//
// `i` exists because `missing_data_policy: ignore` has to go somewhere. Without it the
// ignored member either stays in u — and the policy does nothing — or vanishes and the
// equality d = x + i + g + b + u is false.
type tally struct {
	region                   string
	declared                 int
	excludedDisabled         int
	excludedMaintenance      int
	ignored                  int
	good, bad, unknown       int
	badCauses, unknownCauses []MemberCause
	excludedCauses           []MemberCause
}

func (t tally) eligible() int {
	return t.declared - t.excludedDisabled - t.excludedMaintenance - t.ignored
}

func (t tally) excluded() int { return t.excludedDisabled + t.excludedMaintenance }

// aggregateRegion applies §9.3 within one region.
//
// Thresholds are declared against d and clamped to the post-policy n. Clamping is what
// keeps a TEMPORARY exclusion from invalidating a STORED declaration: without it a quorum
// of 3 with one member in maintenance would report BAD — a failure verdict manufactured by
// a planned exclusion — and an execution change such as disabling a monitor could
// effectively invalidate a file-owned declaration, which §6.2 forbids.
func aggregateRegion(t tally, p Policies) (Availability, Health, *Weakened) {
	n := t.eligible()
	if n == 0 {
		// §9.4: resolved by CAUSE, not by a single constant.
		if t.ignored > 0 {
			// `ignore` removed the last source of information. UNKNOWN, never EXCLUDED —
			// otherwise the policy buys coverage it did not measure.
			return AvailUnknown, HealthUnknown, nil
		}
		if t.excluded() == t.declared && t.declared > 0 {
			return AvailExcluded, HealthExcluded, nil
		}
		return AvailUnknown, HealthUnknown, nil
	}

	g, b, u := t.good, t.bad, t.unknown
	var weak *Weakened

	switch p.Aggregation.Mode {
	case domain.AggAll:
		switch {
		case b > 0:
			return AvailBad, HealthDown, nil
		case u > 0:
			return AvailUnknown, HealthUnknown, nil
		default:
			return AvailGood, HealthHealthy, nil
		}
	case domain.AggAny:
		switch {
		case g > 0:
			return AvailGood, HealthHealthy, nil
		case b == n:
			return AvailBad, HealthDown, nil
		default:
			return AvailUnknown, HealthUnknown, nil
		}
	default: // AggQuorum
		degraded, healthy := p.Aggregation.DegradedMin, p.Aggregation.HealthyMin
		effDegraded, effHealthy := min(degraded, n), min(healthy, n)
		if effDegraded < degraded || effHealthy < healthy {
			weak = &Weakened{
				Region: t.region, Declared: t.declared, Eligible: n,
				ExcludedDisabled: t.excludedDisabled, ExcludedMaintenance: t.excludedMaintenance,
				Ignored:             t.ignored,
				DeclaredDegradedMin: degraded, DeclaredHealthyMin: healthy,
				EffectiveDegraded: effDegraded, EffectiveHealthy: effHealthy,
			}
		}
		// Availability has exactly ONE good branch: since degraded_min <= healthy_min,
		// writing it as two invites an implementer to make them differ.
		switch {
		case g >= effDegraded:
			if g >= effHealthy {
				return AvailGood, HealthHealthy, weak
			}
			return AvailGood, HealthDegraded, weak
		case g+u >= effDegraded:
			return AvailUnknown, HealthUnknown, weak
		default:
			return AvailBad, HealthDown, weak
		}
	}
}

// regionResult is one region's verdict inside a sub-interval.
type regionResult struct {
	availability Availability
	health       Health
}

// aggregate runs the fixed six-step order of §9.2 over one sub-interval and returns the
// service outcome.
//
// The expected region set is the set of distinct regions of the DECLARED members in the
// epoch snapshot — not a free-text list — so a region present in the snapshot with no
// observations is UNKNOWN rather than absent. Silently dropping it would let two dark
// regions look like unanimous health.
func aggregate(tallies []tally, p Policies) Outcome {
	if len(tallies) == 0 {
		// No declared members at all. A service with an empty sli[] never reaches the
		// evaluator (§9.5); reaching here with nothing declared is UNKNOWN, never GOOD.
		return Outcome{Availability: AvailUnknown, Health: HealthUnknown}
	}

	var weakened []Weakened
	results := make([]regionResult, 0, len(tallies))
	allExcluded := true
	for _, t := range tallies {
		a, h, w := aggregateRegion(t, p)
		if w != nil {
			weakened = append(weakened, *w)
		}
		if a != AvailExcluded {
			allExcluded = false
		}
		results = append(results, regionResult{availability: a, health: h})
	}
	if allExcluded {
		return Outcome{Availability: AvailExcluded, Health: HealthExcluded, Weakened: weakened}
	}

	// R is the expected set minus regions that are EXCLUDED: a region whose members were
	// all excluded by declaration is out of scope, and must not drag the combination down.
	var gr, ur, hr, inR int
	for _, r := range results {
		if r.availability == AvailExcluded {
			continue
		}
		inR++
		switch r.availability {
		case AvailGood:
			gr++
		case AvailUnknown:
			ur++
		}
		if r.health == HealthHealthy {
			hr++
		}
	}
	if inR == 0 {
		return Outcome{Availability: AvailUnknown, Health: HealthUnknown, Weakened: weakened}
	}

	effDegraded := min(p.Region.DegradedMinRegions, inR)
	effHealthy := min(p.Region.HealthyMinRegions, inR)

	switch {
	case gr >= effDegraded:
		if hr >= effHealthy {
			return Outcome{Availability: AvailGood, Health: HealthHealthy, Weakened: weakened}
		}
		return Outcome{Availability: AvailGood, Health: HealthDegraded, Weakened: weakened}
	case gr+ur >= effDegraded:
		return Outcome{Availability: AvailUnknown, Health: HealthUnknown, Weakened: weakened}
	default:
		return Outcome{Availability: AvailBad, Health: HealthDown, Weakened: weakened}
	}
}

// censusAt builds the per-region tallies for one instant, applying the fixed order of
// §9.2: normalize by type, exclude, then apply the missing-data policy.
func censusAt(series []memberSeries, spans []MaintenanceSpan, p Policies, t time.Time) []tally {
	byRegion := map[string]*tally{}
	order := make([]string, 0, 4)
	get := func(region string) *tally {
		if tl, ok := byRegion[region]; ok {
			return tl
		}
		tl := &tally{region: region}
		byRegion[region] = tl
		order = append(order, region)
		return tl
	}

	type pending struct {
		member memberSeries
		region *tally
	}
	var unknowns []pending

	for _, s := range series {
		tl := get(s.Region)
		tl.declared++
		st, reason := s.stateAt(t, spans, p.Maintenance == domain.MaintenanceExclude)
		switch st {
		case StateExcluded:
			if !s.Enabled {
				tl.excludedDisabled++
			} else {
				tl.excludedMaintenance++
			}
			tl.excludedCauses = append(tl.excludedCauses, MemberCause{MonitorID: s.MonitorID, Region: s.Region})
		case StateGood:
			tl.good++
		case StateBad:
			tl.bad++
			tl.badCauses = append(tl.badCauses, MemberCause{MonitorID: s.MonitorID, Region: s.Region})
		default:
			tl.unknown++
			tl.unknownCauses = append(tl.unknownCauses, MemberCause{MonitorID: s.MonitorID, Region: s.Region, Reason: reason})
			unknowns = append(unknowns, pending{member: s, region: tl})
		}
	}

	switch p.MissingData {
	case domain.MissingBad:
		for _, pd := range unknowns {
			pd.region.unknown--
			pd.region.bad++
			pd.region.badCauses = append(pd.region.badCauses, MemberCause{MonitorID: pd.member.MonitorID, Region: pd.member.Region})
		}
	case domain.MissingIgnore:
		for _, pd := range unknowns {
			pd.region.unknown--
			pd.region.ignored++
		}
	}

	sort.Strings(order)
	out := make([]tally, 0, len(order))
	for _, region := range order {
		out = append(out, *byRegion[region])
	}
	return out
}

// StateAt is the POINT evaluator (iter-0140): the aggregated outcome in force AT the
// instant t, right-continuous — an observation, a stale deadline, a maintenance edge or a
// policy input effective exactly at t is included, regardless of the sub-microsecond
// precision of any derived deadline. It is deliberately COMPOSED from the reducer's own
// pieces — buildSeries → censusAt(t) → aggregate — the exact computation Reduce performs
// for the sub-interval beginning at t, so the live signal and the facts can never disagree
// about what a member's state at an instant means. No interval is involved: the iter-0139
// fixed-width window was splittable by nanosecond-granular freshness deadlines
// (time.ParseDuration admits them), and any fixed width would be.
func StateAt(members []Member, observations []Observation, maintenance []MaintenanceSpan, p Policies, t time.Time) Outcome {
	if len(members) == 0 {
		// Mirrors aggregate's empty-tally semantics: nothing declared is UNKNOWN, never GOOD.
		return Outcome{Availability: AvailUnknown, Health: HealthUnknown}
	}
	series := buildSeries(members, observations)
	return aggregate(censusAt(series, maintenance, p, t), p)
}
