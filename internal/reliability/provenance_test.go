package reliability

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Invariant 35 (§19, §10.3): provenance carries a bounded, overflow-counted cause set for
// `unknown_duration` as well as for `bad_duration`, so a `partial` window is still explainable
// after raw retention has removed the heartbeats.
//
// The bound is the whole point: provenance is stored beside every fact, so an unbounded cause set
// would make one bucket's explanation grow with the fleet — and the honest alternative to
// truncating silently is to say how much did not fit. UNKNOWN is called out separately because it
// is the class a reader reaches for when the report says `partial`, and it is the one that is easy
// to leave unbounded while bounding BAD.
func TestProvenanceCauseSetsAreBoundedAndCountTheirOverflow(t *testing.T) {
	// One DOWN observation per member, timed so that freshness expires exactly halfway through the
	// bucket: every member is BAD for the first half and UNKNOWN for the second. That gives the same
	// bucket a BAD sub-interval and an UNKNOWN one, so both cause classes are exercised at once —
	// which is the only way to see whether UNKNOWN is bounded too.
	build := func(n int) Bucket {
		members := make([]Member, 0, n)
		obs := make([]Observation, 0, n)
		half := bucketStart.Add(30 * time.Second)
		for i := 0; i < n; i++ {
			id := string(rune('a'+i%26)) + string(rune('0'+i/26))
			m := httpMember(id, "core")
			members = append(members, m)
			obs = append(obs, Observation{MonitorID: id, Ts: half.Add(-m.StaleAfter), Up: false})
		}
		p := domain.ApplyServicePolicyDefaults(Policies{Maintenance: domain.MaintenanceExclude},
			map[string]int{"core": n}, n)
		return reduce(t, Input{
			Start: bucketStart, End: bucketEnd, Members: members, Observations: obs, Policies: p,
		})
	}

	over := build(MaxCauses + 3)
	if got := len(over.Provenance.Bad); got != MaxCauses {
		t.Fatalf("bad causes = %d, want the bound %d — an unbounded cause set makes one bucket's "+
			"explanation grow with the fleet (invariant 35)", got, MaxCauses)
	}
	if got := len(over.Provenance.Unknown); got != MaxCauses {
		t.Fatalf("unknown causes = %d, want the bound %d — UNKNOWN is the class a reader needs when "+
			"the report says `partial`, and bounding only BAD leaves it unbounded (invariant 35)",
			got, MaxCauses)
	}
	// Three did not fit in each class, and the same member recurring in later sub-intervals of the
	// same class is deduplicated rather than counted again.
	if want := 6; over.Provenance.Overflow != want {
		t.Fatalf("overflow = %d, want %d — what did not fit must be COUNTED, not truncated silently, "+
			"and a recurring cause must not inflate the count (invariant 35)", over.Provenance.Overflow, want)
	}
	if over.Provenance.Declared != MaxCauses+3 {
		t.Fatalf("declared = %d, want %d", over.Provenance.Declared, MaxCauses+3)
	}

	// Negative control: under the bound nothing is truncated and nothing overflows, so the
	// assertions above cannot pass by always reporting the bound.
	under := build(3)
	if len(under.Provenance.Bad) != 3 || len(under.Provenance.Unknown) != 3 {
		t.Fatalf("under the bound: bad = %d, unknown = %d, want 3 and 3",
			len(under.Provenance.Bad), len(under.Provenance.Unknown))
	}
	if under.Provenance.Overflow != 0 {
		t.Fatalf("under the bound: overflow = %d, want 0", under.Provenance.Overflow)
	}
}

// Invariant 4's tail (§8.1, §9.4): provenance distinguishes an IGNORE-weakened quorum from a
// DECLARATION-weakened one.
//
// The clamp record already existed and was tested for the maintenance case only, which hid the
// distinction the invariant is actually about. It matters when someone asks why a service reported
// GOOD on fewer members than it declares: "an operator excluded one" and "policy dropped an
// unknown one" are different answers, and only one of them is a reason to change the declaration.
// `Weakened` carries `Ignored`, `ExcludedMaintenance` and `ExcludedDisabled` as separate counts —
// this pins that they are populated apart rather than summed into one "missing" number.
func TestProvenanceTellsAnIgnoredMemberFromAnExcludedOne(t *testing.T) {
	members := []Member{httpMember("a", "core"), httpMember("b", "core"), httpMember("c", "core")}
	// `a` is UP, `b` has no observation at all (UNKNOWN → ignored by policy), `c` is disabled.
	members[2].Enabled = false
	obs := []Observation{{MonitorID: "a", Ts: bucketStart.Add(-time.Second), Up: true}}
	p := domain.ApplyServicePolicyDefaults(Policies{
		Aggregation: domain.AggregationPolicy{Mode: domain.AggQuorum, DegradedMin: 3, HealthyMin: 3},
		MissingData: domain.MissingIgnore,
		Maintenance: domain.MaintenanceExclude,
	}, map[string]int{"core": 3}, 3)

	b := reduce(t, Input{Start: bucketStart, End: bucketEnd, Members: members, Observations: obs, Policies: p})
	if len(b.Provenance.Weakened) != 1 {
		t.Fatalf("weakened records = %d, want 1 — the clamp bit and must be recorded", len(b.Provenance.Weakened))
	}
	w := b.Provenance.Weakened[0]
	if w.Ignored != 1 {
		t.Errorf("Ignored = %d, want 1 — a member dropped by `missing_data: ignore` must be counted as "+
			"IGNORED, not folded into an exclusion (invariant 4)", w.Ignored)
	}
	if w.ExcludedDisabled != 1 {
		t.Errorf("ExcludedDisabled = %d, want 1 — a disabled member is a DECLARATION-side exclusion and "+
			"must stay distinguishable from an ignored one (invariant 4)", w.ExcludedDisabled)
	}
	if w.ExcludedMaintenance != 0 {
		t.Errorf("ExcludedMaintenance = %d, want 0 — nothing here was in maintenance, and a summed "+
			"\"missing\" count would show one", w.ExcludedMaintenance)
	}
	if w.DeclaredDegradedMin != 3 || w.EffectiveDegraded != 1 {
		t.Errorf("declared/effective degraded = %d/%d, want 3/1 — the clamp must state both numbers so a "+
			"reader can see how far the threshold fell", w.DeclaredDegradedMin, w.EffectiveDegraded)
	}
}
