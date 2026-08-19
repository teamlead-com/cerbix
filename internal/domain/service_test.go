package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The file surface spells the floor `90s`; JSON must spell it the same way, or the two
// declaration surfaces disagree about what an operator declared.
func TestFreshnessFloorIsADurationStringOverJSON(t *testing.T) {
	b, err := json.Marshal(FreshnessPolicy{ActiveMultiplier: 3, ActiveFloor: 90 * time.Second})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"active_floor":"1m30s"`) {
		t.Fatalf("marshalled as %s, want a duration string", b)
	}

	var back FreshnessPolicy
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back.ActiveFloor != 90*time.Second || back.ActiveMultiplier != 3 {
		t.Fatalf("round-tripped to %+v", back)
	}
}

// Rows written before the codec existed hold nanoseconds, and they must still read back as
// the same policy — a stored declaration that stopped parsing would take the meaning of
// availability with it.
func TestFreshnessFloorStillReadsTheIntegerSpelling(t *testing.T) {
	var f FreshnessPolicy
	if err := json.Unmarshal([]byte(`{"active_multiplier":3,"active_floor":90000000000}`), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.ActiveFloor != 90*time.Second {
		t.Fatalf("active_floor = %v, want 90s", f.ActiveFloor)
	}
}

func TestFreshnessFloorRejectsNonsense(t *testing.T) {
	var f FreshnessPolicy
	if err := json.Unmarshal([]byte(`{"active_floor":"ninety seconds"}`), &f); err == nil {
		t.Fatal("a non-duration string was accepted")
	}
	// Omitted and empty both mean "not declared", which the defaults then fill.
	for _, body := range []string{`{}`, `{"active_floor":""}`, `{"active_floor":null}`} {
		var g FreshnessPolicy
		if err := json.Unmarshal([]byte(body), &g); err != nil {
			t.Errorf("%s: %v", body, err)
		}
		if g.ActiveFloor != 0 {
			t.Errorf("%s decoded to %v, want the zero that defaults fill", body, g.ActiveFloor)
		}
	}
}

// The whole policy block round-trips, so a declaration read back from the DB is the one that
// was written.
func TestServicePoliciesRoundTrip(t *testing.T) {
	p := ApplyServicePolicyDefaults(ServicePolicies{}, map[string]int{"core": 2}, 1)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ServicePolicies
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != p {
		t.Fatalf("round trip changed the policy:\n got %+v\nwant %+v\n(%s)", back, p, b)
	}
}

// Invariant 6 (§9.5): threshold validation runs against the DECLARED cardinality, and a momentary
// exclusion never triggers it.
//
// Both halves are load-bearing and they pull in opposite directions. Validating against the declared
// count is what makes `healthy_min: 3` on a two-member service a REFUSAL at write time instead of a
// service that can never be healthy. Not validating against the momentary count is what stops a
// maintenance window or a disabled member from retroactively invalidating a stored declaration —
// that case is the reducer's clamp (`TestQuorumClampsToEligibleAndRecordsIt`), not a rejection.
func TestThresholdValidationReadsTheDeclaredCardinality(t *testing.T) {
	policies := func(healthyMin int) ServicePolicies {
		return ApplyServicePolicyDefaults(ServicePolicies{
			Aggregation: AggregationPolicy{Mode: AggQuorum, DegradedMin: 1, HealthyMin: healthyMin},
			Maintenance: MaintenanceExclude,
		}, map[string]int{"core": 2}, 1) // one region, so the region clause cannot fire first
	}

	// Declared 2, asking for 3 healthy: refused BY NAME, at write time.
	err := ValidateServicePolicies(policies(3), map[string]int{"core": 2}, 1)
	if err == nil {
		t.Fatal("healthy_min 3 over 2 declared members was accepted — a service that can never be " +
			"healthy must be refused at write time, not discovered in a report (invariant 6)")
	}
	if !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "core") {
		t.Errorf("refusal = %q, want it to name the excess and the region", err)
	}

	// Declared 2, asking for 2: accepted — and it stays accepted no matter how many members are
	// momentarily excluded, because validation never sees the momentary count.
	if err := ValidateServicePolicies(policies(2), map[string]int{"core": 2}, 1); err != nil {
		t.Fatalf("healthy_min 2 over 2 declared members was refused: %v", err)
	}
	// The same policies with a SMALLER declared map is a different declaration, not a momentary
	// exclusion — and it is refused, which is what proves the check reads the map it is given.
	if err := ValidateServicePolicies(policies(2), map[string]int{"core": 1}, 1); err == nil {
		t.Error("healthy_min 2 over 1 declared member was accepted — validation must read the declared " +
			"cardinality it is handed (invariant 6)")
	}
}
