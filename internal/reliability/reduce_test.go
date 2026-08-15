package reliability

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

var (
	bucketStart = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	bucketEnd   = bucketStart.Add(time.Minute)
)

func httpMember(id, region string) Member {
	return Member{
		MonitorID: id, Type: domain.MonitorHTTP, Region: region, Enabled: true,
		StaleAfter: domain.ResolveStaleAfter(domain.DefaultFreshnessPolicy(), domain.MonitorHTTP, 30*time.Second, 0),
	}
}

func pushMember(id, region string, armed time.Time) Member {
	return Member{
		MonitorID: id, Type: domain.MonitorPush, Region: region, Enabled: true,
		StaleAfter: domain.ResolveStaleAfter(domain.DefaultFreshnessPolicy(), domain.MonitorPush, 60*time.Second, 0),
		ArmedAt:    armed,
	}
}

func allPolicies() Policies {
	return domain.ApplyServicePolicyDefaults(Policies{MaintenanceExcludes: true}, map[string]int{"core": 1}, 1)
}

func reduce(t *testing.T, in Input) Bucket {
	t.Helper()
	b, err := Reduce(in)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if got := b.Durations.Total(); got != bucketEnd.Sub(bucketStart) {
		t.Fatalf("availability axis does not conserve: %s", got)
	}
	if got := b.Durations.HealthTotal(); got != bucketEnd.Sub(bucketStart) {
		t.Fatalf("health axis does not conserve: %s", got)
	}
	return b
}

// The case r2 had no answer for: a member GOOD for part of a bucket and UNKNOWN for the
// rest, with no BAD anywhere. "BAD dominates GOOD" says nothing here, and the two
// defensible readings — GOOD bucket or UNKNOWN bucket — differ by a whole minute of budget.
// The reducer splits it instead of choosing.
func TestGoodThenUnknownInsideOneBucket(t *testing.T) {
	m := httpMember("a", "core")
	// StaleAfter is 90s (the floor), so place the observation so its deadline lands 20s in.
	obs := Observation{MonitorID: "a", Ts: bucketStart.Add(-70 * time.Second), Up: true}

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{m}, Observations: []Observation{obs},
		Policies: allPolicies(),
	})

	if b.Durations.Good != 20*time.Second {
		t.Errorf("good = %s, want 20s", b.Durations.Good)
	}
	if b.Durations.Unknown != 40*time.Second {
		t.Errorf("unknown = %s, want 40s", b.Durations.Unknown)
	}
	if b.Durations.Bad != 0 || b.Durations.Excluded != 0 {
		t.Errorf("bad/excluded should be zero, got %s/%s", b.Durations.Bad, b.Durations.Excluded)
	}
	if len(b.Provenance.Unknown) != 1 || b.Provenance.Unknown[0].Reason != ReasonStale {
		t.Errorf("provenance should name the stale member, got %+v", b.Provenance.Unknown)
	}
}

// Partial maintenance is ordinary, not an edge case. Crediting a whole GOOD bucket from
// 20s of evidence, or discarding the bucket entirely, are both wrong by up to 40s.
func TestPartialMaintenanceSplitsTheBucket(t *testing.T) {
	m := httpMember("a", "core")
	obs := Observation{MonitorID: "a", Ts: bucketStart.Add(-1 * time.Second), Up: true}
	span := MaintenanceSpan{ID: "mw", MonitorID: "a", From: bucketStart, To: bucketStart.Add(40 * time.Second)}

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{m}, Observations: []Observation{obs},
		Maintenance: []MaintenanceSpan{span}, Policies: allPolicies(),
	})

	if b.Durations.Excluded != 40*time.Second {
		t.Errorf("excluded = %s, want 40s", b.Durations.Excluded)
	}
	if b.Durations.Good != 20*time.Second {
		t.Errorf("good = %s, want 20s", b.Durations.Good)
	}
	// The excluded duration is shared by both axes: declared exclusion is the same time
	// under either reading.
	if b.Durations.Healthy != 20*time.Second {
		t.Errorf("healthy = %s, want 20s", b.Durations.Healthy)
	}
}

// Maintenance excludes and WINS over the normalized state: a stale push member inside a
// window is excluded, not BAD.
func TestMaintenanceBeatsAStalePushMember(t *testing.T) {
	m := pushMember("p", "core", bucketStart.Add(-10*time.Minute))
	span := MaintenanceSpan{ID: "mw", MonitorID: "p", From: bucketStart, To: bucketEnd}

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{m}, Maintenance: []MaintenanceSpan{span}, Policies: allPolicies(),
	})
	if b.Durations.Excluded != time.Minute {
		t.Errorf("excluded = %s, want the whole bucket", b.Durations.Excluded)
	}
	if b.Durations.Bad != 0 {
		t.Errorf("bad = %s, want 0 — exclusion wins over the type's normalization", b.Durations.Bad)
	}
}

// For push the absence of a ping IS the failure, measured from the instant the dead-man
// was armed — mirroring the product's own stale-push sweep, which falls back to
// push_armed_at/created_at when there is no result yet.
func TestPushDeadManExpiresFromArmedAt(t *testing.T) {
	armed := bucketStart.Add(-30 * time.Second) // StaleAfter is 60s → expires 30s into the bucket
	m := pushMember("p", "core", armed)

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{m}, Policies: allPolicies(),
	})
	if b.Durations.Unknown != 30*time.Second {
		t.Errorf("unknown = %s, want 30s before the dead-man expires", b.Durations.Unknown)
	}
	if b.Durations.Bad != 30*time.Second {
		t.Errorf("bad = %s, want 30s after it expires", b.Durations.Bad)
	}
}

// An active probe with no observation is UNCERTAIN, never failing. Confusing the two is
// how a monitoring system invents outages on a freshly created service.
func TestActiveProbeWithNoObservationIsUnknownNotBad(t *testing.T) {
	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{httpMember("a", "core")}, Policies: allPolicies(),
	})
	if b.Durations.Unknown != time.Minute {
		t.Errorf("unknown = %s, want the whole bucket", b.Durations.Unknown)
	}
	if b.Durations.Bad != 0 {
		t.Errorf("bad = %s, want 0", b.Durations.Bad)
	}
	if len(b.Provenance.Unknown) != 1 || b.Provenance.Unknown[0].Reason != ReasonNoObservation {
		t.Errorf("provenance should say why, got %+v", b.Provenance.Unknown)
	}
}

// A deliberately disabled member is EXCLUDED, not BAD — it must not tank a service forever.
func TestDisabledMemberIsExcluded(t *testing.T) {
	m := httpMember("a", "core")
	m.Enabled = false
	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{m}, Policies: allPolicies(),
	})
	if b.Durations.Excluded != time.Minute {
		t.Errorf("excluded = %s, want the whole bucket", b.Durations.Excluded)
	}
}

// §8.1: `ignore` removes an UNKNOWN member only while other KNOWN members keep the
// interval decidable. r3's table had no variable for the ignored member, so this case
// returned UNKNOWN and contradicted §8.1 in the same document.
func TestIgnoreDropsAnUnknownMemberWhileOthersDecide(t *testing.T) {
	good := httpMember("good", "core")
	dark := httpMember("dark", "core")
	p := domain.ApplyServicePolicyDefaults(Policies{MissingData: domain.MissingIgnore, MaintenanceExcludes: true}, map[string]int{"core": 2}, 1)

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members:      []Member{good, dark},
		Observations: []Observation{{MonitorID: "good", Ts: bucketStart.Add(-1 * time.Second), Up: true}},
		Policies:     p,
	})
	if b.Durations.Good != time.Minute {
		t.Errorf("good = %s, want the whole bucket: the known member decides it", b.Durations.Good)
	}
}

// ...but when `ignore` removes the LAST source of information the interval is UNKNOWN,
// never EXCLUDED. Otherwise one settings change buys 100% coverage on a service that
// measured nothing.
func TestIgnoreCannotLaunderTheLastUnknownIntoExcluded(t *testing.T) {
	p := domain.ApplyServicePolicyDefaults(Policies{MissingData: domain.MissingIgnore, MaintenanceExcludes: true}, map[string]int{"core": 1}, 1)
	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{httpMember("dark", "core")}, Policies: p,
	})
	if b.Durations.Unknown != time.Minute {
		t.Errorf("unknown = %s, want the whole bucket", b.Durations.Unknown)
	}
	if b.Durations.Excluded != 0 {
		t.Fatalf("excluded = %s — `ignore` laundered unmeasured time into declared-out-of-scope time", b.Durations.Excluded)
	}
}

// Every declared member excluded by DECLARATION is out of scope; the bucket is EXCLUDED
// and never GOOD.
func TestAllMembersExcludedIsExcludedNeverGood(t *testing.T) {
	a, bm := httpMember("a", "core"), httpMember("b", "core")
	bm.Enabled = false
	span := MaintenanceSpan{ID: "mw", MonitorID: "a", From: bucketStart, To: bucketEnd}

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{a, bm}, Maintenance: []MaintenanceSpan{span},
		Policies: domain.ApplyServicePolicyDefaults(Policies{MaintenanceExcludes: true}, map[string]int{"core": 2}, 1),
	})
	if b.Durations.Excluded != time.Minute {
		t.Errorf("excluded = %s, want the whole bucket", b.Durations.Excluded)
	}
	if b.Durations.Good != 0 {
		t.Fatalf("good = %s — a vacuous truth reports perfect availability precisely when nothing was measured", b.Durations.Good)
	}
}

// A quorum of 3 with one member in maintenance must not report BAD: the threshold clamps
// to the eligible cardinality, and the clamp is recorded rather than silent.
func TestQuorumClampsToEligibleAndRecordsIt(t *testing.T) {
	members := []Member{httpMember("a", "core"), httpMember("b", "core"), httpMember("c", "core")}
	obs := []Observation{
		{MonitorID: "a", Ts: bucketStart.Add(-1 * time.Second), Up: true},
		{MonitorID: "b", Ts: bucketStart.Add(-1 * time.Second), Up: true},
		{MonitorID: "c", Ts: bucketStart.Add(-1 * time.Second), Up: true},
	}
	span := MaintenanceSpan{ID: "mw", MonitorID: "c", From: bucketStart, To: bucketEnd}
	p := domain.ApplyServicePolicyDefaults(Policies{
		Aggregation:         domain.AggregationPolicy{Mode: domain.AggQuorum, DegradedMin: 3, HealthyMin: 3},
		MaintenanceExcludes: true,
	}, map[string]int{"core": 3}, 1)

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: members, Observations: obs, Maintenance: []MaintenanceSpan{span}, Policies: p,
	})
	if b.Durations.Good != time.Minute {
		t.Fatalf("good = %s, want the whole bucket — a planned exclusion manufactured a failure verdict", b.Durations.Good)
	}
	if len(b.Provenance.Weakened) != 1 {
		t.Fatalf("the clamp must be recorded, got %+v", b.Provenance.Weakened)
	}
	w := b.Provenance.Weakened[0]
	if w.DeclaredDegradedMin != 3 || w.EffectiveDegraded != 2 || w.ExcludedMaintenance != 1 {
		t.Errorf("weakened record is wrong: %+v", w)
	}
	if w.Ignored != 0 {
		t.Errorf("a maintenance clamp must not be reported as an ignore clamp: %+v", w)
	}
}

// One dark vantage point makes a multi-region service DEGRADED, not DOWN — the outcome
// §9's opening paragraph demands and the reason the region policy is an object rather
// than a boolean.
func TestOneDarkRegionIsDegradedNotDown(t *testing.T) {
	members := []Member{httpMember("a", "core"), httpMember("a1", "geo1"), httpMember("a2", "geo2")}
	obs := []Observation{
		{MonitorID: "a", Ts: bucketStart.Add(-1 * time.Second), Up: true},
		{MonitorID: "a1", Ts: bucketStart.Add(-1 * time.Second), Up: true},
		// geo2 never reported: UNKNOWN, not absent.
	}
	p := domain.ApplyServicePolicyDefaults(Policies{MaintenanceExcludes: true},
		map[string]int{"core": 1, "geo1": 1, "geo2": 1}, 3)

	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: members, Observations: obs, Policies: p,
	})
	if b.Durations.Good != time.Minute {
		t.Errorf("good = %s, want the whole bucket: two of three regions are serving", b.Durations.Good)
	}
	if b.Durations.Degraded != time.Minute {
		t.Errorf("degraded = %s, want the whole bucket: the third region is dark", b.Durations.Degraded)
	}
	if b.Durations.Healthy != 0 {
		t.Errorf("healthy = %s, want 0", b.Durations.Healthy)
	}
}

// An observation at exactly End belongs to the NEXT bucket; a stale deadline landing
// exactly on an instant has already expired.
func TestHalfOpenEqualityCases(t *testing.T) {
	m := httpMember("a", "core")
	b := reduce(t, Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{m},
		Observations: []Observation{
			{MonitorID: "a", Ts: bucketStart.Add(-90 * time.Second), Up: true}, // deadline == bucketStart
			{MonitorID: "a", Ts: bucketEnd, Up: true},                          // belongs to the next bucket
		},
		Policies: allPolicies(),
	})
	if b.Durations.Unknown != time.Minute {
		t.Errorf("unknown = %s, want the whole bucket: the deadline had already expired at Start", b.Durations.Unknown)
	}
	if b.Durations.Good != 0 {
		t.Errorf("good = %s — the observation at End leaked into this bucket", b.Durations.Good)
	}
}

func TestReduceIsDeterministic(t *testing.T) {
	in := Input{
		Start: bucketStart, End: bucketEnd,
		Members: []Member{httpMember("a", "core"), pushMember("p", "geo1", bucketStart.Add(-30*time.Second))},
		Observations: []Observation{
			{MonitorID: "a", Ts: bucketStart.Add(-40 * time.Second), Up: true},
			{MonitorID: "a", Ts: bucketStart.Add(25 * time.Second), Up: false},
		},
		Maintenance: []MaintenanceSpan{{ID: "mw", From: bucketStart.Add(10 * time.Second), To: bucketStart.Add(20 * time.Second)}},
		Policies:    domain.ApplyServicePolicyDefaults(Policies{MaintenanceExcludes: true}, map[string]int{"core": 1, "geo1": 1}, 2),
	}
	first, err := Reduce(in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Reduce(in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Durations != second.Durations {
		t.Fatalf("two runs disagree:\n%+v\n%+v", first.Durations, second.Durations)
	}
}
