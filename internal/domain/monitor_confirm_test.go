package domain

import "testing"

func TestConfirmIntervalNormalize(t *testing.T) {
	mk := func(typ MonitorType, confirm, interval, threshold int) Monitor {
		m := Monitor{Name: "x", ProjectID: "p", Type: typ, Target: "t:1",
			IntervalSeconds: interval, TimeoutSeconds: 5, FailureThreshold: threshold,
			ConfirmIntervalSeconds: confirm}
		m.Normalize()
		return m
	}
	if got := mk(MonitorTCP, 2, 60, 3).ConfirmIntervalSeconds; got != 5 {
		t.Fatalf("below-min clamp = %d, want 5", got)
	}
	if got := mk(MonitorTCP, 300, 60, 3).ConfirmIntervalSeconds; got != 60 {
		t.Fatalf("above-interval clamp = %d, want 60", got)
	}
	if got := mk(MonitorTCP, 0, 60, 3).ConfirmIntervalSeconds; got != 0 {
		t.Fatalf("explicit off = %d, want 0", got)
	}
	if got := mk(MonitorPush, 10, 60, 3).ConfirmIntervalSeconds; got != 0 {
		t.Fatalf("push must zero confirm interval, got %d", got)
	}
	if got := mk(MonitorComposite, 10, 60, 3).ConfirmIntervalSeconds; got != 0 {
		t.Fatalf("composite must zero confirm interval, got %d", got)
	}
	// Phase predicates.
	m := mk(MonitorTCP, 10, 60, 3)
	if !m.ConfirmConfigured() {
		t.Fatal("configured monitor must report ConfirmConfigured")
	}
	m.Status = StatusUp
	m.ConsecutiveFailures = 1
	if !m.InConfirmPhase() {
		t.Fatal("fails=1/3 up must be in confirm phase")
	}
	m.ConsecutiveFailures = 3
	if m.InConfirmPhase() {
		t.Fatal("at-threshold must not be in confirm phase")
	}
	if mk(MonitorTCP, 10, 60, 1).ConfirmConfigured() {
		t.Fatal("threshold=1 disables the phase")
	}
}
