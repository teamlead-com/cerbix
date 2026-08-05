package domain

import (
	"strings"
	"testing"
	"time"
)

func TestEscalationPolicyValidate(t *testing.T) {
	ch := EscalationTarget{Type: EscalationTargetChannel, ID: "c1"}
	cases := map[string]struct {
		p  EscalationPolicy
		ok bool
	}{
		"ok": {EscalationPolicy{Name: "p", ProjectID: "pr", Steps: []EscalationStep{
			{AfterSeconds: 0, Targets: []EscalationTarget{ch}},
			{AfterSeconds: 600, Targets: []EscalationTarget{ch}},
		}}, true},
		"no name":         {EscalationPolicy{ProjectID: "pr", Steps: []EscalationStep{{Targets: []EscalationTarget{ch}}}}, false},
		"no project":      {EscalationPolicy{Name: "p", Steps: []EscalationStep{{Targets: []EscalationTarget{ch}}}}, false},
		"no steps":        {EscalationPolicy{Name: "p", ProjectID: "pr"}, false},
		"no targets":      {EscalationPolicy{Name: "p", ProjectID: "pr", Steps: []EscalationStep{{AfterSeconds: 0}}}, false},
		"decreasing":      {EscalationPolicy{Name: "p", ProjectID: "pr", Steps: []EscalationStep{{AfterSeconds: 600, Targets: []EscalationTarget{ch}}, {AfterSeconds: 0, Targets: []EscalationTarget{ch}}}}, false},
		"bad type":        {EscalationPolicy{Name: "p", ProjectID: "pr", Steps: []EscalationStep{{Targets: []EscalationTarget{{Type: "pigeon", ID: "x"}}}}}, false},
		"empty target id": {EscalationPolicy{Name: "p", ProjectID: "pr", Steps: []EscalationStep{{Targets: []EscalationTarget{{Type: EscalationTargetChannel, ID: " "}}}}}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := c.p.Validate(); (err == nil) != c.ok {
				t.Fatalf("Validate() err=%v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestOnCallScheduleResolve(t *testing.T) {
	anchor := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // a Monday
	s := OnCallSchedule{
		Name: "primary", ProjectID: "pr", ShiftSeconds: 7 * 24 * 3600, AnchorAt: anchor,
		Participants: []string{"alice", "bob", "carol"},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("valid schedule rejected: %v", err)
	}
	// Week 0 → alice, week 1 → bob, week 3 wraps → alice.
	tests := []struct {
		at   time.Time
		want string
	}{
		{anchor, "alice"},
		{anchor.Add(3 * 24 * time.Hour), "alice"},  // mid week 0
		{anchor.Add(7 * 24 * time.Hour), "bob"},    // week 1
		{anchor.Add(14 * 24 * time.Hour), "carol"}, // week 2
		{anchor.Add(21 * 24 * time.Hour), "alice"}, // week 3 wraps
		{anchor.Add(-1 * 24 * time.Hour), "carol"}, // before anchor → previous slot
	}
	for _, tc := range tests {
		if got := s.OnCall(tc.at); got != tc.want {
			t.Fatalf("OnCall(%s) = %q, want %q", tc.at, got, tc.want)
		}
	}
}

func TestOnCallOverrideWins(t *testing.T) {
	anchor := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	s := OnCallSchedule{
		ShiftSeconds: 7 * 24 * 3600, AnchorAt: anchor, Participants: []string{"alice", "bob"},
		Overrides: []OnCallOverride{{
			ChannelID: "carol",
			StartsAt:  anchor.Add(24 * time.Hour),
			EndsAt:    anchor.Add(48 * time.Hour),
		}},
	}
	// Outside the override window → rotation (week 0 = alice).
	if got := s.OnCall(anchor); got != "alice" {
		t.Fatalf("outside override = %q, want alice", got)
	}
	// Inside the override window → carol, regardless of rotation.
	if got := s.OnCall(anchor.Add(30 * time.Hour)); got != "carol" {
		t.Fatalf("inside override = %q, want carol", got)
	}
	// At ends_at (exclusive) → back to rotation.
	if got := s.OnCall(anchor.Add(48 * time.Hour)); got != "alice" {
		t.Fatalf("at end = %q, want alice", got)
	}

	if err := (OnCallOverride{ChannelID: "c", StartsAt: anchor, EndsAt: anchor.Add(time.Hour)}).Validate(); err != nil {
		t.Fatalf("valid override rejected: %v", err)
	}
	if err := (OnCallOverride{ChannelID: "c", StartsAt: anchor, EndsAt: anchor}).Validate(); err == nil {
		t.Fatal("override with ends<=starts should be invalid")
	}
	if err := (OnCallOverride{StartsAt: anchor, EndsAt: anchor.Add(time.Hour)}).Validate(); err == nil {
		t.Fatal("override without channel should be invalid")
	}
}

func TestEscalationStepAlertMessage(t *testing.T) {
	m := EscalationStepAlert{MonitorName: "payments", Step: 1}.Message()
	if !strings.Contains(m, "payments") || !strings.Contains(m, "step 2") || !strings.Contains(m, "Acknowledge") {
		t.Fatalf("message = %q", m)
	}
	r := EscalationStepAlert{MonitorName: "payments", Step: 2, Repeat: true}.Message()
	if !strings.Contains(r, "Still unacknowledged") {
		t.Fatalf("repeat message = %q", r)
	}
}
