package domain

import "testing"

// FR-021 §16 — the edge rule is pure, so every "who gets woken" question is answerable here
// without a database, a clock or a leader. These are the cases the spec argues about.

func TestServiceAlertPolicyPages(t *testing.T) {
	down := ServiceAlertPolicy{OwnsPaging: true, PageOn: []ServiceAlertState{ServiceAlertDown},
		ConfirmEvaluations: 2}
	both := ServiceAlertPolicy{OwnsPaging: true,
		PageOn: []ServiceAlertState{ServiceAlertDown, ServiceAlertDegraded}, ConfirmEvaluations: 1}
	blind := ServiceAlertPolicy{OwnsPaging: true, PageOn: []ServiceAlertState{ServiceAlertDown},
		PageOnUnknown: true, ConfirmEvaluations: 1}

	cases := []struct {
		name   string
		policy ServiceAlertPolicy
		state  ServiceAlertState
		want   bool
	}{
		{"down pages by default", down, ServiceAlertDown, true},
		{"degraded is opt-in and off here", down, ServiceAlertDegraded, false},
		{"degraded pages when declared", both, ServiceAlertDegraded, true},
		// The load-bearing one: asking for `down` must never enable `unknown`. "We cannot see it"
		// and "it is broken" are different statements.
		{"unknown does NOT page because down was requested", down, ServiceAlertUnknown, false},
		{"unknown pages only under its own switch", blind, ServiceAlertUnknown, true},
		// A declared maintenance window is a declared silence, whatever else is declared.
		{"excluded never pages", both, ServiceAlertExcluded, false},
		{"excluded never pages even with the unknown switch", blind, ServiceAlertExcluded, false},
		// Healthy is not an alarm; the recovery path handles the return.
		{"healthy is not an alarm", both, ServiceAlertHealthy, false},
	}
	for _, c := range cases {
		if got := c.policy.Pages(c.state); got != c.want {
			t.Errorf("%s: Pages(%q) = %v, want %v", c.name, c.state, got, c.want)
		}
	}
}

func TestServiceAlertPolicyValidate(t *testing.T) {
	ok := DefaultServiceAlertPolicy()
	if err := ok.Validate(); err != nil {
		t.Fatalf("the default policy is invalid: %v", err)
	}
	if len(ok.PageOn) != 1 || ok.PageOn[0] != ServiceAlertDown || ok.PageOnUnknown {
		t.Fatalf("default policy = %+v, want down-only and unknown off", ok)
	}

	// `unknown` in page_on is refused with its reason: it has its own switch, and accepting it here
	// would give a UI two ways to enable it and let one of them do so without a decision.
	bad := ServiceAlertPolicy{PageOn: []ServiceAlertState{ServiceAlertUnknown}, ConfirmEvaluations: 2}
	err := bad.Validate()
	if err == nil {
		t.Fatal("page_on accepted `unknown`")
	}
	if got := err.Error(); got == "" || !contains(got, "page_on_unknown") {
		t.Fatalf("refusal = %q, want it to name the switch that exists instead", got)
	}
	for _, n := range []int{0, -1, 11} {
		p := ServiceAlertPolicy{PageOn: []ServiceAlertState{ServiceAlertDown}, ConfirmEvaluations: n}
		if err := p.Validate(); err == nil {
			t.Errorf("confirm_evaluations %d was accepted", n)
		}
	}
}

// The edge rule. Every row here is a sentence from §16.2 that an implementation could get wrong in
// a way no integration test would notice until someone was not paged.
func TestDecideServiceAlert(t *testing.T) {
	own := func(mut func(*ServiceAlertPolicy)) ServiceAlertPolicy {
		p := DefaultServiceAlertPolicy()
		p.OwnsPaging = true
		if mut != nil {
			mut(&p)
		}
		return p
	}

	cases := []struct {
		name         string
		policy       ServiceAlertPolicy
		current      ServiceAlertState
		streak       int
		notified     ServiceAlertState
		wantNotify   bool
		wantRecovery bool
		wantNext     ServiceAlertState
	}{
		{
			name: "a service that does not own paging never notifies",
			policy: func() ServiceAlertPolicy {
				p := own(nil)
				p.OwnsPaging = false
				return p
			}(),
			current: ServiceAlertDown, streak: 9, notified: "",
		},
		{
			name: "an unconfirmed state waits", policy: own(nil),
			current: ServiceAlertDown, streak: 1, notified: "",
		},
		{
			name: "a confirmed down announces", policy: own(nil),
			current: ServiceAlertDown, streak: 2, notified: "",
			wantNotify: true, wantNext: ServiceAlertDown,
		},
		{
			name: "the same state is not re-announced", policy: own(nil),
			current: ServiceAlertDown, streak: 40, notified: ServiceAlertDown,
		},
		{
			name: "recovery: down → healthy announces the END", policy: own(nil),
			current: ServiceAlertHealthy, streak: 2, notified: ServiceAlertDown,
			wantNotify: true, wantRecovery: true, wantNext: ServiceAlertHealthy,
		},
		{
			// The first observed state being healthy must NOT emit a recovery for something that was
			// never announced — a service that starts life healthy pages nobody.
			name: "healthy with nothing ever announced is silent", policy: own(nil),
			current: ServiceAlertHealthy, streak: 5, notified: "",
		},
		{
			// degraded → down is a NEW announcement, not a recovery followed by a page.
			name: "pageable → pageable is one new announcement",
			policy: own(func(p *ServiceAlertPolicy) {
				p.PageOn = []ServiceAlertState{ServiceAlertDown, ServiceAlertDegraded}
				p.ConfirmEvaluations = 1
			}),
			current: ServiceAlertDown, streak: 1, notified: ServiceAlertDegraded,
			wantNotify: true, wantNext: ServiceAlertDown,
		},
		{
			// down → excluded: the new state would never page, but the operator was told something
			// was wrong and is owed the end of it.
			name: "entering maintenance while paged is a recovery", policy: own(nil),
			current: ServiceAlertExcluded, streak: 2, notified: ServiceAlertDown,
			wantNotify: true, wantRecovery: true, wantNext: ServiceAlertExcluded,
		},
		{
			// down → unknown with the switch OFF: same reasoning. Losing sight of a service that was
			// down is not "still down" and not "fixed"; the honest move is to end the announcement.
			name: "losing sight while paged ends the announcement", policy: own(nil),
			current: ServiceAlertUnknown, streak: 2, notified: ServiceAlertDown,
			wantNotify: true, wantRecovery: true, wantNext: ServiceAlertUnknown,
		},
		{
			// ...and with the switch ON it is a page in its own right, not a recovery.
			name: "unknown pages when declared, and is not a recovery",
			policy: own(func(p *ServiceAlertPolicy) {
				p.PageOnUnknown = true
				p.ConfirmEvaluations = 1
			}),
			current: ServiceAlertUnknown, streak: 1, notified: ServiceAlertDown,
			wantNotify: true, wantNext: ServiceAlertUnknown,
		},
		{
			name: "excluded with nothing announced is silent", policy: own(nil),
			current: ServiceAlertExcluded, streak: 3, notified: "",
		},
	}

	for _, c := range cases {
		got := DecideServiceAlert(c.policy, c.current, c.streak, c.notified)
		if got.Notify != c.wantNotify || got.Recovery != c.wantRecovery {
			t.Errorf("%s: notify=%v recovery=%v, want notify=%v recovery=%v (reason %q)",
				c.name, got.Notify, got.Recovery, c.wantNotify, c.wantRecovery, got.Reason)
			continue
		}
		if c.wantNotify && got.NextNotified != c.wantNext {
			t.Errorf("%s: next notified = %q, want %q", c.name, got.NextNotified, c.wantNext)
		}
		// Silence always has a name: an empty reason with no notification would leave "why was I
		// not paged" unanswerable, which is the question this whole feature has to survive.
		if !got.Notify && got.Reason == "" {
			t.Errorf("%s: silent with no stated reason", c.name)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
