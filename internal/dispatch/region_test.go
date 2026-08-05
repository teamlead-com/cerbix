package dispatch

import "testing"

func TestJobsQueueForRegion(t *testing.T) {
	if got := jobsQueueForRegion("geo1"); got != "checks.jobs.geo1" {
		t.Fatalf("region queue = %q", got)
	}
	if got := jobsQueueForRegion(""); got != "checks.jobs.core" {
		t.Fatalf("empty region → core queue, got %q", got)
	}
}
