package store

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-041a §11 "Outbox — coverage", the evaluator's half (P18, D31 rules 2 and 3). The outbox decides
// what a phone delivery means for an announcement — credited, condemned, or neither — and its tests
// prove which of the three it chooses; this test proves what each choice then does to the evaluator
// when the service's ONLY route is a phone channel:
//
//   - neither (a route that was not phoned: storm_limited, provider_rejected, outcome_unknown): the
//     announcement stands, uncredited. Five more evaluations of the same outage enqueue nothing — no
//     re-announcement every fifteen seconds to the same channel with the same answer;
//   - condemned (no route at send): the next evaluation finds the service unroutable and withholds,
//     so there is no loop either, and the members page for themselves.
func TestAPhoneOnlyAnnouncementThatReachedNobodyNeitherLoopsNorCredits(t *testing.T) {
	w := newPhoneRouteWorld(t)
	exec(t, w.st, w.ctx, `DELETE FROM service_alert_state`)
	f := alertFixture{projectID: w.f.projectID, serviceID: w.f.serviceID, monitorID: w.f.monitorID}
	bob := w.member(t, "bob@example.com", domain.RoleEditor)
	w.verifiedPhone(t, bob, phoneA)
	phone := w.userChannel(t, domain.ChannelSMS, bob)

	setMemberHealth(t, w.st, w.ctx, f, true)
	evalOnce(t, w.st, w.ctx)
	setMemberHealth(t, w.st, w.ctx, f, false)
	evalOnce(t, w.st, w.ctx)
	if got := evalOnce(t, w.st, w.ctx); got.Onsets != 1 {
		t.Fatalf("the confirmed DOWN did not announce to the phone route: %+v", got)
	}
	events := alertEvents(t, w.st, w.ctx)
	if len(events) != 1 || len(events[0].Recipients) != 1 || events[0].Recipients[0] != phone.ID {
		t.Fatalf("announcement = %+v; want one to the phone channel", events)
	}

	// Neither: the worker marks nothing. Five more evaluations of the same outage enqueue nothing.
	for i := 0; i < 5; i++ {
		setMemberHealth(t, w.st, w.ctx, f, false)
		if got := evalOnce(t, w.st, w.ctx); got.Onsets != 0 {
			t.Fatalf("evaluation %d after an uncredited, uncondemned announcement re-announced: %+v", i+1, got)
		}
	}
	if n := len(alertEvents(t, w.st, w.ctx)); n != 1 {
		t.Fatalf("%d service alert events after five more evaluations; want still the one", n)
	}

	// Condemned: the phone stopped being a route before the send (its number unverified), and the
	// worker condemned the announcement. The next evaluation finds nobody to tell and withholds.
	exec(t, w.st, w.ctx, `UPDATE user_phones SET verified_at = NULL WHERE user_id = $1`, bob)
	if err := w.st.MarkServiceAlertUndeliverable(w.ctx, events[0]); err != nil {
		t.Fatalf("condemn: %v", err)
	}
	for i := 0; i < 5; i++ {
		setMemberHealth(t, w.st, w.ctx, f, false)
		if got := evalOnce(t, w.st, w.ctx); got.Onsets != 0 || got.Withheld[WithheldUnroutable] != 1 {
			t.Fatalf("evaluation %d after a condemned phone-only announcement: %+v; want withheld as unroutable", i+1, got)
		}
	}
	if n := len(alertEvents(t, w.st, w.ctx)); n != 1 {
		t.Fatalf("%d service alert events; want still the one: an unroutable service is not re-announced", n)
	}
}
