package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Invariant 25 (§19, §10.10): `min_decidable_coverage` is FIXED at 0.95 and is not
// operator-settable in phase 1.
//
// The number decides when a window stops stating availability and says `partial` instead, so a knob
// for it is a knob for how much missing evidence an installation is willing to call a number. That
// is exactly the decision §11 removes from the operator, and the spec fixes the value so a report
// means the same thing on every installation. Two halves, and the second is the one that rots: the
// value itself, and the absence of any path that could move it.
func TestTheDecidableCoverageFloorIsFixedAndNotAKnob(t *testing.T) {
	if minDecidableCoverage != 0.95 {
		t.Fatalf("min decidable coverage = %v, want the fixed 0.95 of §10.10 — the threshold decides "+
			"when a window stops claiming a number, and it must mean the same on every installation",
			minDecidableCoverage)
	}

	// No configuration, settings or API surface may name it. A scan is the only way to hold this:
	// a knob that exists but is unused today reads as harmless and ships as a promise.
	scanned := 0
	for _, dir := range []string{"../config", "../settings", "../api"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s/%s: %v", dir, name, err)
			}
			scanned++
			for _, knob := range []string{"min_decidable_coverage", "MinDecidableCoverage", "DecidableCoverageFloor"} {
				if strings.Contains(string(src), knob) {
					t.Errorf("%s/%s exposes %q: the decidable-coverage floor is fixed in phase 1 (invariant 25), "+
						"and a settable floor is a settable definition of what a reliability number means",
						dir, name, knob)
				}
			}
		}
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d files across config/settings/api — the guard is not looking", scanned)
	}
}
