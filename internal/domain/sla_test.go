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
