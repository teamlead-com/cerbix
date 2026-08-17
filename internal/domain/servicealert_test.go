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

// The edge rule. Every row is a sentence from §16.3 that an implementation could get wrong in a way
// no integration test would notice until somebody was not paged — or was paged for a state the
// service had already left.
func TestDecideServiceAlert(t *testing.T) {
	own := func(mut func(*ServiceAlertPolicy)) ServiceAlertPolicy {
		p := DefaultServiceAlertPolicy()
		p.OwnsPaging = true
		if mut != nil {
			mut(&p)
		}
		return p
	}
	bothStates := func(p *ServiceAlertPolicy) {
		p.PageOn = []ServiceAlertState{ServiceAlertDown, ServiceAlertDegraded}
		p.ConfirmEvaluations = 1
	}

	cases := []struct {
		name        string
		policy      ServiceAlertPolicy
		current     ServiceAlertState
		streak      int
		firing      bool
		emitted     ServiceAlertState
		wantNotify  bool
		wantClose   bool
		wantReason  ServiceAlertCloseReason
		wantEmitted ServiceAlertState
		wantFiring  bool
	}{
		{
			name: "a service that does not own paging never notifies",
			policy: func() ServiceAlertPolicy {
				p := own(nil)
				p.OwnsPaging = false
				return p
			}(),
			current: ServiceAlertDown, streak: 9,
		},
		{
			name: "an unconfirmed state waits and keeps the level", policy: own(nil),
			current: ServiceAlertDown, streak: 1, firing: true, emitted: ServiceAlertDown,
			wantFiring: true,
		},
		{
			name: "a confirmed down opens an announcement", policy: own(nil),
			current: ServiceAlertDown, streak: 2,
			wantNotify: true, wantEmitted: ServiceAlertDown, wantFiring: true,
		},
		{
			name: "the same open state is not re-announced", policy: own(nil),
			current: ServiceAlertDown, streak: 40, firing: true, emitted: ServiceAlertDown,
			wantFiring: true,
		},
		{
			// The LEVEL is what makes this right: with page_on={down}, degraded is not pageable, so
			// this changes the state and must not open anything.
			name: "healthy → degraded with down-only pages nothing", policy: own(nil),
			current: ServiceAlertDegraded, streak: 5,
		},
		{
			// ...and must not emit a recovery either, because nothing was ever announced.
			name: "degraded → healthy with nothing open is silent", policy: own(nil),
			current: ServiceAlertHealthy, streak: 5,
		},
		{
			name: "down → healthy closes as RECOVERED", policy: own(nil),
			current: ServiceAlertHealthy, streak: 2, firing: true, emitted: ServiceAlertDown,
			wantNotify: true, wantClose: true, wantReason: CloseRecovered,
			wantEmitted: ServiceAlertHealthy,
		},
		{
			// down → degraded while only `down` pages: the operator was told DOWN and it is not down
			// any more, so the announcement ENDS. Leaving it open would keep a page alive for a state
			// the service left.
			name: "down → degraded with down-only closes", policy: own(nil),
			current: ServiceAlertDegraded, streak: 2, firing: true, emitted: ServiceAlertDown,
			wantNotify: true, wantClose: true, wantReason: ClosePolicyChanged,
			wantEmitted: ServiceAlertDegraded,
		},
		{
			// ...but when BOTH page, it is one new onset, not a close plus an onset: a manufactured
			// "recovered" in between would be false.
			name: "degraded → down when both page is ONE new onset", policy: own(bothStates),
			current: ServiceAlertDown, streak: 1, firing: true, emitted: ServiceAlertDegraded,
			wantNotify: true, wantEmitted: ServiceAlertDown, wantFiring: true,
		},
		{
			name: "entering maintenance while paged closes as ENTERED_MAINTENANCE", policy: own(nil),
			current: ServiceAlertExcluded, streak: 2, firing: true, emitted: ServiceAlertDown,
			wantNotify: true, wantClose: true, wantReason: CloseEnteredMaintenance,
			wantEmitted: ServiceAlertExcluded,
		},
		{
			// The owner's decision 7: losing sight ENDS the announcement, and says so. Calling it
			// "recovered" would assert evidence nobody has.
			name: "losing sight while paged closes as VISIBILITY_LOST", policy: own(nil),
			current: ServiceAlertUnknown, streak: 2, firing: true, emitted: ServiceAlertDown,
			wantNotify: true, wantClose: true, wantReason: CloseVisibilityLost,
			wantEmitted: ServiceAlertUnknown,
		},
		{
			// ...and with the switch ON it is a page in its own right, not a close.
			name: "unknown pages when declared, and is not a close",
			policy: own(func(p *ServiceAlertPolicy) {
				p.PageOnUnknown = true
				p.ConfirmEvaluations = 1
			}),
			current: ServiceAlertUnknown, streak: 1, firing: true, emitted: ServiceAlertDown,
			wantNotify: true, wantEmitted: ServiceAlertUnknown, wantFiring: true,
		},
		{
			name: "excluded with nothing open is silent", policy: own(nil),
			current: ServiceAlertExcluded, streak: 3,
		},
	}

	for _, c := range cases {
		got := DecideServiceAlert(c.policy, c.current, c.streak, c.firing, c.emitted)
		if got.Notify != c.wantNotify || got.Close != c.wantClose {
			t.Errorf("%s: notify=%v close=%v, want notify=%v close=%v (reason %q)",
				c.name, got.Notify, got.Close, c.wantNotify, c.wantClose, got.Reason)
			continue
		}
		if c.wantClose && got.CloseReason != c.wantReason {
			t.Errorf("%s: close reason = %q, want %q", c.name, got.CloseReason, c.wantReason)
		}
		if !c.wantClose && got.CloseReason != "" {
			t.Errorf("%s: a non-close carries close reason %q", c.name, got.CloseReason)
		}
		if c.wantNotify && got.NextEmitted != c.wantEmitted {
			t.Errorf("%s: next emitted = %q, want %q", c.name, got.NextEmitted, c.wantEmitted)
		}
		if got.NextFiring != c.wantFiring {
			t.Errorf("%s: next firing = %v, want %v", c.name, got.NextFiring, c.wantFiring)
		}
		// Silence always has a name: an empty reason with no notification leaves "why was I not
		// paged" unanswerable, which is the question this feature has to survive.
		if !got.Notify && got.Reason == "" {
			t.Errorf("%s: silent with no stated reason", c.name)
		}
	}
}

// A close ALWAYS names its reason, and only a return to healthy is a recovery. This is the honesty
// rule of owner decision 7 stated as a table, because a future refactor that made "close" implicitly
// mean "recovered" would pass every behavioural test above.
func TestCloseReasonNeverInventsARecovery(t *testing.T) {
	for state, want := range map[ServiceAlertState]ServiceAlertCloseReason{
		ServiceAlertHealthy:  CloseRecovered,
		ServiceAlertExcluded: CloseEnteredMaintenance,
		ServiceAlertUnknown:  CloseVisibilityLost,
		ServiceAlertDegraded: ClosePolicyChanged,
	} {
		if got := closeReasonFor(state); got != want {
			t.Errorf("closeReasonFor(%q) = %q, want %q", state, got, want)
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
