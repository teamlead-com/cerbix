package dispatch

import "testing"

func TestJobsQueueForRegion(t *testing.T) {
	if got, _ := jobsQueueForGeneration("geo1", ProtocolV1); got != "checks.jobs.geo1" {
		t.Fatalf("region queue = %q", got)
	}
	if got, _ := jobsQueueForGeneration("", ProtocolV1); got != "checks.jobs.core" {
		t.Fatalf("empty region → core queue, got %q", got)
	}
}

// A5 — EVERY generation of both families applies the empty-region default.
//
// The default used to be restated in one helper per generation, and the generation-4 jobs helper —
// the newest — omitted it. A publisher with an empty region would then have declared and published
// to `checks.jobs.v4.`, a name no consumer binds, and those jobs would have sat unconsumed until
// their TTL while every other generation routed to `core`. Unreachable today, and a helper that is
// wrong in a way nobody currently exercises is a trap laid for the next caller.
//
// The assertion iterates the MAPPED generations rather than naming them, so a generation added
// tomorrow is covered without anyone remembering to extend this test — which is the same property
// the fix itself has.
//
// The mutation that must kill this: give any one generation its own helper without the default.
func TestEveryQueueGenerationDefaultsAnEmptyRegionToCore(t *testing.T) {
	for _, family := range []struct {
		name     string
		prefixes map[int]string
		resolve  func(string, int) (string, bool)
	}{
		{"jobs", jobsQueuePrefixes, jobsQueueForGeneration},
		{"tests", testsQueuePrefixes, testsQueueForGeneration},
	} {
		for generation, prefix := range family.prefixes {
			empty, ok := family.resolve("", generation)
			if !ok {
				t.Fatalf("%s generation %d maps to no queue", family.name, generation)
			}
			if want := prefix + "core"; empty != want {
				t.Errorf("%s generation %d with an empty region names %q, want %q — a name no "+
					"consumer binds, so every job published there sits until its TTL",
					family.name, generation, empty, want)
			}
			named, _ := family.resolve("geo1", generation)
			if want := prefix + "geo1"; named != want {
				t.Errorf("%s generation %d names %q for geo1, want %q", family.name, generation, named, want)
			}
		}
	}
}
