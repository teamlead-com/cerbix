package domain

import "testing"

func TestIncidentStatusValidAndTerminal(t *testing.T) {
	for _, s := range []IncidentStatus{IncidentInvestigating, IncidentIdentified, IncidentMonitoring, IncidentResolved} {
		if !s.Valid() {
			t.Errorf("status %q should be valid", s)
		}
	}
	if IncidentStatus("bogus").Valid() {
		t.Error("bogus status should be invalid")
	}
	if !IncidentResolved.Terminal() {
		t.Error("resolved should be terminal")
	}
	if IncidentInvestigating.Terminal() {
		t.Error("investigating should not be terminal")
	}
}

func TestIncidentImpactAndSourceValid(t *testing.T) {
	for _, i := range []IncidentImpact{ImpactNone, ImpactMinor, ImpactMajor, ImpactCritical} {
		if !i.Valid() {
			t.Errorf("impact %q should be valid", i)
		}
	}
	if IncidentImpact("meltdown").Valid() {
		t.Error("meltdown impact should be invalid")
	}
	for _, s := range []IncidentSource{SourceManual, SourceAPI, SourceAuto} {
		if !s.Valid() {
			t.Errorf("source %q should be valid", s)
		}
	}
	if IncidentSource("cron").Valid() {
		t.Error("cron source should be invalid")
	}
}

func TestIncidentValidate(t *testing.T) {
	base := Incident{ProjectID: "p1", Title: "x", Status: IncidentInvestigating, Impact: ImpactMinor, Source: SourceManual}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid incident rejected: %v", err)
	}
	tests := []struct {
		name string
		mut  func(*Incident)
	}{
		{"no title", func(i *Incident) { i.Title = "  " }},
		{"no project", func(i *Incident) { i.ProjectID = "" }},
		{"bad status", func(i *Incident) { i.Status = "nope" }},
		{"bad impact", func(i *Incident) { i.Impact = "nope" }},
		{"bad source", func(i *Incident) { i.Source = "nope" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inc := base
			tc.mut(&inc)
			if err := inc.Validate(); err == nil {
				t.Errorf("expected %s to fail validation", tc.name)
			}
		})
	}
}

func TestIncidentUpdateValidate(t *testing.T) {
	if err := (IncidentUpdate{IncidentID: "i1", Status: IncidentIdentified}).Validate(); err != nil {
		t.Fatalf("valid update rejected: %v", err)
	}
	if err := (IncidentUpdate{Status: IncidentIdentified}).Validate(); err == nil {
		t.Error("update without incident_id should fail")
	}
	if err := (IncidentUpdate{IncidentID: "i1", Status: "bad"}).Validate(); err == nil {
		t.Error("update with bad status should fail")
	}
}
