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
		StatusUp:   CompOperational,
		StatusDown: CompMajorOutage,
		// FR-021 §17 / D-0167, the ONE intentional change to existing public output: a monitor
		// that has never been confirmed either way used to publish `operational`. This assertion
		// replaces the one that pinned that behaviour, and its previous expectation is recorded
		// here so a future reader sees a decision rather than a drifted test.
		StatusPending: CompNoData,
	}
	for in, want := range cases {
		if got := ComponentStatusFromMonitor(in); got != want {
			t.Errorf("from %q = %q, want %q", in, got, want)
		}
	}
	// The old mapping is REMOVED, not merely unasserted.
	if ComponentStatusFromMonitor(StatusPending) == CompOperational {
		t.Error("a pending monitor still reports operational — the §17 change was reverted")
	}
	// An unrecognized status is unmeasured, never health.
	if got := ComponentStatusFromMonitor(MonitorStatus("wat")); got != CompNoData {
		t.Errorf("unknown monitor status = %q, want no_data", got)
	}
}

func TestSummaryStatusWorstOf(t *testing.T) {
	// An EMPTY page is the second inherited lie (§17): it used to summarize `operational`, i.e.
	// "all systems operational" with no systems configured.
	if got := SummaryStatus(nil); got != CompNoData {
		t.Errorf("empty summary = %q, want no_data", got)
	}
	if s := Summarize(nil); s.State != SummaryEmpty {
		t.Errorf("empty page state = %q, want empty", s.State)
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

// The summary is worst-of-MEASURED plus an unmeasured COUNT: `no_data` never joins the severity
// ladder, so "operational, but two components were not measured" cannot read as all-clear
// (invariant 67).
func TestSummarizeKeepsUnmeasuredApart(t *testing.T) {
	cases := []struct {
		name           string
		in             []ComponentStatus
		wantStatus     ComponentStatus
		wantState      PageSummaryState
		wantUnmeasured int
	}{
		{"all measured and fine", []ComponentStatus{CompOperational, CompOperational},
			CompOperational, SummaryOperational, 0},
		{"operational plus unmeasured", []ComponentStatus{CompOperational, CompNoData, CompNoData},
			CompOperational, SummaryOperational, 2},
		{"all unmeasured is not operational", []ComponentStatus{CompNoData, CompNoData},
			CompNoData, SummaryNoData, 2},
		{"an outage still wins", []ComponentStatus{CompNoData, CompMajorOutage},
			CompMajorOutage, SummaryImpaired, 1},
		{"maintenance is measured", []ComponentStatus{CompMaintenance, CompNoData},
			CompMaintenance, SummaryImpaired, 1},
		{"an unknown future value counts as unmeasured, never as health",
			[]ComponentStatus{ComponentStatus("teleported"), CompOperational},
			CompOperational, SummaryOperational, 1},
	}
	for _, c := range cases {
		got := Summarize(c.in)
		if got.Status != c.wantStatus || got.State != c.wantState || got.UnmeasuredCount != c.wantUnmeasured {
			t.Errorf("%s: got %+v, want status=%q state=%q unmeasured=%d",
				c.name, got, c.wantStatus, c.wantState, c.wantUnmeasured)
		}
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
