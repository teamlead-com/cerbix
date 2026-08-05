package domain

import "testing"

func TestVisibilityValid(t *testing.T) {
	for _, v := range []Visibility{VisibilityPublic, VisibilityInternal, VisibilityUnlisted} {
		if !v.Valid() {
			t.Errorf("visibility %q should be valid", v)
		}
	}
	if Visibility("secret").Valid() {
		t.Error("secret visibility should be invalid")
	}
}

func TestComponentStatusFromMonitor(t *testing.T) {
	cases := map[MonitorStatus]ComponentStatus{
		StatusUp:      CompOperational,
		StatusPending: CompOperational,
		StatusDown:    CompMajorOutage,
	}
	for in, want := range cases {
		if got := ComponentStatusFromMonitor(in); got != want {
			t.Errorf("from %q = %q, want %q", in, got, want)
		}
	}
}

func TestSummaryStatusWorstOf(t *testing.T) {
	if got := SummaryStatus(nil); got != CompOperational {
		t.Errorf("empty summary = %q, want operational", got)
	}
	got := SummaryStatus([]ComponentStatus{CompOperational, CompDegraded, CompMajorOutage, CompMaintenance})
	if got != CompMajorOutage {
		t.Errorf("summary = %q, want major_outage", got)
	}
	// Maintenance outranks operational but never an outage.
	if got := SummaryStatus([]ComponentStatus{CompOperational, CompMaintenance}); got != CompMaintenance {
		t.Errorf("summary = %q, want maintenance", got)
	}
}

func TestStatusPageValidate(t *testing.T) {
	base := StatusPage{OrgID: "o1", Slug: "s", Title: "S", Visibility: VisibilityPublic}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid page rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*StatusPage)
	}{
		{"no org", func(p *StatusPage) { p.OrgID = "" }},
		{"no slug", func(p *StatusPage) { p.Slug = " " }},
		{"no title", func(p *StatusPage) { p.Title = "" }},
		{"bad visibility", func(p *StatusPage) { p.Visibility = "nope" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mut(&p)
			if err := p.Validate(); err == nil {
				t.Errorf("expected %s to fail", tc.name)
			}
		})
	}
}

func TestComponentValidate(t *testing.T) {
	if err := (Component{StatusPageID: "sp1", Name: "API"}).Validate(); err != nil {
		t.Fatalf("valid component rejected: %v", err)
	}
	if err := (Component{Name: "API"}).Validate(); err == nil {
		t.Error("component without page should fail")
	}
	if err := (Component{StatusPageID: "sp1"}).Validate(); err == nil {
		t.Error("component without name should fail")
	}
	if err := (Component{StatusPageID: "sp1", Name: "API", ManualStatus: "bad"}).Validate(); err == nil {
		t.Error("component with bad manual status should fail")
	}
}
