package reliability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Invariant 1 (§19, §7.2): the evaluator is a PURE function of its inputs and reads no clock.
//
// Every other reliability guarantee is derived from this one. A single `time.Now()` inside the
// reducer would make a recompute of a sealed bucket disagree with the fact stored for it, which
// is precisely what invariants 16 and 20 forbid — and the disagreement would depend on WHEN the
// recompute ran, so it would be unreproducible. The property is structural rather than
// behavioural, so no scenario test can hold it: a test that reduces a bucket twice and compares
// cannot distinguish a pure reducer from one whose clock read happens to fall inside the same
// bucket. Scanning the package's own source can, and that is what this does.
//
// `as_of` and every deadline arrive as explicit inputs, from the caller that owns the clock.
func TestTheReducerReadsNoClock(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for _, forbidden := range []string{"time.Now(", "time.Since(", "time.Until("} {
			if strings.Contains(string(src), forbidden) {
				t.Errorf("%s reads the clock via %s): the reducer must be a pure function of "+
					"(observations, epoch snapshot, maintenance spans, range) — invariant 1. A clock read "+
					"makes a recompute of a sealed bucket disagree with the fact stored for it, and the "+
					"disagreement depends on when the recompute ran", name, forbidden)
			}
		}
	}
	// Negative control: a scan that found nothing to read would pass vacuously forever.
	if scanned < 3 {
		t.Fatalf("scanned only %d production files — the guard is not looking at the package", scanned)
	}
}
