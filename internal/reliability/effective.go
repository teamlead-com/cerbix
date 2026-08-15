package reliability

import (
	"sort"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// memberSeries is one member's snapshot plus its observations, sorted by timestamp.
type memberSeries struct {
	Member
	obs []Observation
}

// buildSeries groups observations by member and sorts them. Observations BEFORE the
// bucket start are kept: sample-and-hold needs the one in force when the bucket opened,
// and dropping it would make every bucket start UNKNOWN.
//
// `heartbeats` already carries a unique (monitor_id, ts), so two observations cannot share
// an instant and no tie-break is required. The spec deliberately does not order by ctid:
// a physical heap location changes under VACUUM FULL and a reproducible output cannot
// depend on it (§7.2).
func buildSeries(members []Member, obs []Observation) []memberSeries {
	byID := make(map[string][]Observation, len(members))
	for _, o := range obs {
		byID[o.MonitorID] = append(byID[o.MonitorID], o)
	}
	out := make([]memberSeries, 0, len(members))
	for _, m := range members {
		s := memberSeries{Member: m, obs: byID[m.MonitorID]}
		sort.Slice(s.obs, func(i, j int) bool { return s.obs[i].Ts.Before(s.obs[j].Ts) })
		out = append(out, s)
	}
	return out
}

// latestAt returns the last observation at or before t, and whether one exists.
func (s memberSeries) latestAt(t time.Time) (Observation, bool) {
	idx := sort.Search(len(s.obs), func(i int) bool { return s.obs[i].Ts.After(t) })
	if idx == 0 {
		return Observation{}, false
	}
	return s.obs[idx-1], true
}

// stateAt returns the member's effective state at t, and the reason when it is UNKNOWN.
//
// The precedence is §7.1's, in order: disabled excludes; then the TYPE normalizes,
// including staleness; then maintenance excludes and wins over the normalized state — a
// stale push member inside a maintenance window is excluded, not BAD.
func (s memberSeries) stateAt(t time.Time, spans []MaintenanceSpan, maintenanceExcludes bool) (State, UnknownReason) {
	if !s.Enabled {
		return StateExcluded, ""
	}
	if maintenanceExcludes {
		for _, sp := range spans {
			if sp.covers(s.MonitorID, t) {
				return StateExcluded, ""
			}
		}
	}
	o, ok := s.latestAt(t)
	if !ok {
		// No observation yet. For an active probe the absence of a result is uncertainty.
		// For push the absence IS the failure — but only once the dead-man has actually
		// run out, measured from the instant the switch was armed, which is what the
		// product's own stale-push sweep does.
		if s.Type == domain.MonitorPush && !s.ArmedAt.IsZero() && !t.Before(s.ArmedAt.Add(s.StaleAfter)) {
			return StateBad, ""
		}
		return StateUnknown, ReasonNoObservation
	}
	if deadline := o.Ts.Add(s.StaleAfter); !t.Before(deadline) {
		// Past the deadline the observed state has decayed. What replaces it is the type's
		// answer, not a global rule: uncertainty for an active probe, failure for push.
		if s.Type == domain.MonitorPush {
			return StateBad, ""
		}
		return StateUnknown, ReasonStale
	}
	if o.Up {
		return StateGood, ""
	}
	return StateBad, ""
}

// breakpoints returns the instants inside [start, end) at which some member's effective
// state can change, always including start and end. Between two consecutive breakpoints
// every member's state is constant by construction, which is what lets the aggregation
// run once per sub-interval instead of being sampled at an arbitrary instant.
func breakpoints(series []memberSeries, spans []MaintenanceSpan, start, end time.Time) []time.Time {
	seen := map[int64]struct{}{}
	out := make([]time.Time, 0, 8)
	add := func(t time.Time) {
		if t.Before(start) || !t.Before(end) {
			return
		}
		k := t.UnixNano()
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, t)
	}
	add(start)
	for _, s := range series {
		for _, o := range s.obs {
			add(o.Ts)
			add(o.Ts.Add(s.StaleAfter))
		}
		if s.Type == domain.MonitorPush && !s.ArmedAt.IsZero() {
			// The dead-man expiring with no observation at all is a state change too.
			add(s.ArmedAt.Add(s.StaleAfter))
		}
	}
	for _, sp := range spans {
		add(sp.From)
		add(sp.To)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
