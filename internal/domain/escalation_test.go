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
	// Deliberately NOT calling Validate here: these participants are readable placeholders for the
	// index arithmetic this test is about, and validity of the ids is the validator's own subject
	// (see the participant cases below). Rewriting them as uuids would make every assertion
	// unreadable to prove something already proved elsewhere.
	//
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

// IsChannelID tells CORRUPTION from a channel that was merely deleted. The two look identical at read
// time and have opposite correct answers — §16.6 falls back for a deletion, while a typo must be fixed
// by a person rather than papered over by falling back forever — so the evaluator needs a cheap way to
// separate them. The authoritative write-path check is the store's (a participant must be a channel OF
// THIS PROJECT); this is the crude half, and it is the half read time can afford.
func TestIsChannelIDSeparatesCorruptionFromADeletedChannel(t *testing.T) {
	const good = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	if !IsChannelID(good) || !IsChannelID(strings.ToUpper(good)) {
		t.Fatal("a channel id was rejected")
	}
	for _, bad := range []string{
		"",                                     // nothing at all
		"alice",                                // a human name
		"ops-team",                             // a team handle
		"1b4e28ba-2fa1-11d2-883f",              // truncated
		"1b4e28ba2fa111d2883f0016d3cca427",     // no dashes
		"1b4e28ba-2fa1-11d2-883f-0016d3cca42g", // not hex
	} {
		if IsChannelID(bad) {
			t.Fatalf("%q was accepted as a channel id", bad)
		}
	}
}
