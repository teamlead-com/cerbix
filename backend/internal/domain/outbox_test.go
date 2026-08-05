package domain

import (
	"strings"
	"testing"
)

func TestRegionWorkerAlertMessage(t *testing.T) {
	miss := RegionWorkerAlert{Region: "geo1", ProjectID: "p1", MonitorCount: 3, Missing: true}.Message()
	if !strings.Contains(miss, "geo1") || !strings.Contains(miss, "no live worker") || !strings.Contains(miss, "--region geo1") {
		t.Fatalf("missing message = %q", miss)
	}
	ok := RegionWorkerAlert{Region: "geo1", MonitorCount: 3, Missing: false}.Message()
	if !strings.Contains(ok, "live worker again") {
		t.Fatalf("recovery message = %q", ok)
	}
}

func TestMonitorTransitionShouldNotify(t *testing.T) {
	cases := []struct {
		name string
		mt   MonitorTransition
		want bool
	}{
		{"up->down", MonitorTransition{Prev: StatusUp, Cur: StatusDown}, true},
		{"pending->down", MonitorTransition{Prev: StatusPending, Cur: StatusDown}, true},
		{"down->up recovery", MonitorTransition{Prev: StatusDown, Cur: StatusUp}, true},
		{"pending->up (first ok)", MonitorTransition{Prev: StatusPending, Cur: StatusUp}, false},
		{"down->down no change", MonitorTransition{Prev: StatusDown, Cur: StatusDown}, false},
		{"down->down reminder", MonitorTransition{Prev: StatusDown, Cur: StatusDown, Reminder: true}, true},
	}
	for _, c := range cases {
		if got := c.mt.ShouldNotify(); got != c.want {
			t.Errorf("%s: ShouldNotify() = %v, want %v", c.name, got, c.want)
		}
	}
}
