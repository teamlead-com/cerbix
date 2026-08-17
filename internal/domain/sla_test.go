package domain

import (
	"math"
	"testing"
)

// iter-0141 ([192] P1-1) + iter-0142 ([195] P0, D-0165): ONE objective rule — raw input in
// the OPEN (0,100), canonical 4-decimal rounding that must stay inside (0,100), so no
// accepted value can overflow, round behind the caller's back, violate the CHECK as a 500,
// or configure the zero error budget whose burn math answers a total outage with 0×.
func TestCanonicalObjective(t *testing.T) {
	for _, tc := range []struct {
		in      float64
		want    float64
		wantErr bool
	}{
		{100, 0, true},            // zero error budget is not a supported configuration
		{99.99995, 0, true},       // rounds to 100 — rejected by the post-round bound
		{99.9999, 99.9999, false}, // the maximum admissible objective
		{99.99994, 99.9999, false},
		{0.0001, 0.0001, false},
		{0.00001, 0, true},   // rounds to zero — a rejection, not a CHECK violation
		{100.00004, 0, true}, // raw >= 100 is rejected as said, never rounded into range
		{100.0001, 0, true},
		{0, 0, true},
		{-1, 0, true},
		{101, 0, true},
		{math.NaN(), 0, true}, // fail-closed even off the JSON path
		{math.Inf(1), 0, true},
		{math.Inf(-1), 0, true},
	} {
		got, err := CanonicalObjective(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("CanonicalObjective(%v) = %v, %v; want %v, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// §16.4b: the canonical key is a latch identity, so two rules cannot share one. A duplicate is
// rejected rather than merged — a merge would let whichever rule wrote last own the other's firing
// state, and the rule would change identity by firing if the key included the latch.
func TestValidateBurnRulesRejectsDuplicateCanonicalKeys(t *testing.T) {
	rule := BurnRule{LongWindowSeconds: 3600, ShortWindowSeconds: 300, Threshold: 14.4, Severity: BurnSeverityPage}
	// Same declared fields, different LATCH: still the same rule, so still a duplicate.
	latched := rule
	latched.Firing = true
	if err := ValidateBurnRules([]BurnRule{rule, latched}); err == nil {
		t.Fatal("two rules with the same canonical key were accepted: their latches would collide")
	}
	// Differing in any declared field makes them distinct.
	other := rule
	other.Threshold = 6
	if err := ValidateBurnRules([]BurnRule{rule, other}); err != nil {
		t.Fatalf("distinct rules rejected: %v", err)
	}
	if rule.Key() != latched.Key() {
		t.Fatal("the canonical key changed with the latch — a rule must not change identity by firing")
	}
}

// The key is about to become PERSISTED identity (`rule_key` for a service's normalized latch and its
// episodes), so it must be lossless: a formatted-to-4-places threshold collapsed distinct valid
// rules onto one key, which both false-rejected them here and would have made one latch answer for
// two rules. Non-finite thresholds are rejected outright — NaN fails every comparison, so the
// positivity check alone let it through and it would have become the literal key "NaN".
func TestBurnRuleKeyIsLosslessAndThresholdsAreFinite(t *testing.T) {
	mk := func(threshold float64) BurnRule {
		return BurnRule{LongWindowSeconds: 3600, ShortWindowSeconds: 300, Threshold: threshold, Severity: BurnSeverityPage}
	}
	a, b := mk(14.40001), mk(14.40002)
	if a.Key() == b.Key() {
		t.Fatalf("thresholds differing below 1e-4 collapsed to one key: %q", a.Key())
	}
	if err := ValidateBurnRules([]BurnRule{a, b}); err != nil {
		t.Fatalf("two DISTINCT rules were rejected as duplicates: %v", err)
	}
	// The same VALUE always spells the same way, whatever arithmetic produced it. The addition goes
	// through variables on purpose: as a constant expression Go folds 0.1+0.2 at arbitrary precision
	// into exactly 0.3, a DIFFERENT float64, and the assertion would be testing nothing.
	tenth, fifth := 0.1, 0.2
	if mk(tenth+fifth).Key() != mk(0.30000000000000004).Key() {
		t.Fatalf("the same float64 produced two different keys: %q vs %q",
			mk(tenth+fifth).Key(), mk(0.30000000000000004).Key())
	}
	for name, bad := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		if err := ValidateBurnRules([]BurnRule{mk(bad)}); err == nil {
			t.Fatalf("a %s threshold was accepted and would become a persisted latch identity", name)
		}
	}
}
