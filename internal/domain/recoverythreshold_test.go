package domain

import (
	"strings"
	"testing"
)

// FR-043 §4.5 / I10: one rule on every write surface, and it lives in Monitor.Validate only —
// an integer in 1..10, exactly 1 for push and composite. Nothing clamps: Normalize leaves the
// value as it was given, so an explicit 0 is refused rather than defaulted.

func recoveryMonitor(typ MonitorType, threshold int) Monitor {
	m := Monitor{
		Name: "r", ProjectID: "p", Type: typ, Target: "https://example.com",
		IntervalSeconds: 60, TimeoutSeconds: 10, RecoveryThreshold: threshold,
	}
	switch typ {
	case MonitorPush:
		m.Target = ""
	case MonitorComposite:
		m.Target = ""
		m.Config = map[string]string{"children": "00000000-0000-0000-0000-000000000001"}
	}
	return m
}

func TestRecoveryThresholdRange(t *testing.T) {
	for _, tc := range []struct {
		typ       MonitorType
		threshold int
		ok        bool
	}{
		{MonitorHTTP, 0, false},
		{MonitorHTTP, -1, false},
		{MonitorHTTP, 1, true},
		{MonitorHTTP, 3, true},
		{MonitorHTTP, 10, true},
		{MonitorHTTP, 11, false},
		{MonitorTCP, 10, true},
		{MonitorPush, 1, true},
		{MonitorPush, 2, false},
		{MonitorPush, 0, false},
		{MonitorComposite, 1, true},
		{MonitorComposite, 3, false},
		{MonitorComposite, 0, false},
	} {
		m := recoveryMonitor(tc.typ, tc.threshold)
		m.Normalize()
		if m.RecoveryThreshold != tc.threshold {
			t.Fatalf("%s/%d: Normalize changed recovery_threshold to %d — nothing may clamp it", tc.typ, tc.threshold, m.RecoveryThreshold)
		}
		err := m.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s/%d: refused: %v", tc.typ, tc.threshold, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s/%d: accepted, want refused", tc.typ, tc.threshold)
			} else if !strings.Contains(err.Error(), "recovery_threshold") {
				t.Errorf("%s/%d: message %q does not name recovery_threshold", tc.typ, tc.threshold, err)
			}
		}
	}
}

func TestRecoveryThresholdMessagesNameTheRule(t *testing.T) {
	m := recoveryMonitor(MonitorHTTP, 11)
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "between 1 and 10") {
		t.Fatalf("range: %v", err)
	}
	m = recoveryMonitor(MonitorPush, 2)
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "must be 1 for push") {
		t.Fatalf("push: %v", err)
	}
	m = recoveryMonitor(MonitorComposite, 2)
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "must be 1 for composite") {
		t.Fatalf("composite: %v", err)
	}
}

func TestRecoveryFieldsClassification(t *testing.T) {
	for field, want := range map[string]SemanticClass{
		"RecoveryThreshold":    SemanticDelivery,
		"ConsecutiveSuccesses": SemanticRuntime,
		"RecoveryStartedAt":    SemanticRuntime,
	} {
		if got := ClassifyMonitorField(field); got != want {
			t.Errorf("%s: class %v, want %v", field, got, want)
		}
	}
}
