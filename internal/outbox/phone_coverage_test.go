package outbox

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// §11 "Outbox — coverage" (P18, D16, D20, D31 rules 2 and 3): what a service alert's phone delivery
// means for the announcement. Delivered credits coverage; a route that was not phoned leaves it
// announced and uncredited — neither credited nor condemned; a channel that is no route at send
// counts in nothing and, alone, condemns it. The evaluator's half — that "neither" re-announces
// nothing over five more evaluations — is TestAPhoneOnlyAnnouncementThatReachedNobodyNeitherLoopsNorCredits
// in internal/store.
func TestServiceAlertPhoneCoverage(t *testing.T) {
	onset := func(recipients ...string) domain.OutboxEvent {
		return domain.OutboxEvent{ID: "e1", Topic: domain.TopicServiceAlert, Payload: payload(t, domain.ServiceAlert{
			ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
			Firing: true, State: domain.ServiceAlertDown, Seq: 1, Recipients: recipients})}
	}
	type verdict string
	const (
		credited  verdict = "credited"
		neither   verdict = "neither"
		condemned verdict = "condemned"
	)
	cases := []struct {
		name    string
		setup   func(r *phoneRig)
		reason  string
		verdict verdict
	}{
		{"delivered", nil, domain.PhoneReasonSent, credited},
		{"storm_limited", func(r *phoneRig) { r.ps.stormFor[rigSMS] = true }, domain.PhoneReasonStormLimited, neither},
		{"provider_rejected", func(r *phoneRig) {
			r.provider.ScriptSMS(telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"})
		}, domain.PhoneReasonProviderRejected, neither},
		{"outcome_unknown", func(r *phoneRig) { r.ps.staleFor[rigSMS] = true }, domain.PhoneReasonOutcomeUnknown, neither},
		{"not a route at send", func(r *phoneRig) {
			tg := r.ps.targets[rigSMS]
			tg.NotRouteReason, tg.Number = domain.DeliverableNoVerifiedNumber, ""
			r.ps.targets[rigSMS] = tg
		}, domain.PhoneReasonNotDeliverable, condemned},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newPhoneRig(t)
			if c.setup != nil {
				c.setup(r)
			}
			r.run(onset(rigSMS))
			if len(r.fs.failed) != 0 {
				t.Fatalf("the event failed: %q; every case here is final", r.fs.failReasons)
			}
			rows := r.ps.rowsFor(rigSMS)
			if len(rows) != 1 || rows[0].reason != c.reason {
				t.Fatalf("rows %+v; want one %s", rows, c.reason)
			}
			got := neither
			switch {
			case len(r.fs.alertDelivered) == 1 && len(r.fs.condemned) == 0:
				got = credited
			case len(r.fs.alertDelivered) == 0 && len(r.fs.condemned) == 1:
				got = condemned
			case len(r.fs.alertDelivered) != 0 || len(r.fs.condemned) != 0:
				t.Fatalf("credited %d and condemned %d", len(r.fs.alertDelivered), len(r.fs.condemned))
			}
			if got != c.verdict {
				t.Fatalf("the announcement was %s; want %s", got, c.verdict)
			}
			if wantUndeliverable := map[bool]int{true: 1, false: 0}[c.verdict == condemned]; r.metrics.undeliverable["health"] != wantUndeliverable {
				t.Fatalf("undeliverable counted %d; want %d", r.metrics.undeliverable["health"], wantUndeliverable)
			}
			// A channel that is a route is never reported missing; one that is no route at send is.
			wantMissing := map[bool]int{true: 1, false: 0}[c.verdict == condemned]
			if r.metrics.recipientMissing != wantMissing {
				t.Fatalf("recipient_missing %d; want %d", r.metrics.recipientMissing, wantMissing)
			}
		})
	}
}

// D16: both calls are handed the same snapshot and Requested is counted ONCE. A snapshot of one text
// and one phone recipient, both there, raises no service_alert_recipient_missing; a recipient that
// really is gone still does.
func TestServiceAlertWithTextAndPhoneRecipientsCountsRequestedOnce(t *testing.T) {
	r := newPhoneRig(t)
	ev := domain.OutboxEvent{ID: "e1", Topic: domain.TopicServiceAlert, Payload: payload(t, domain.ServiceAlert{
		ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
		Firing: true, State: domain.ServiceAlertDown, Seq: 1, Recipients: []string{rigSlack, rigSMS}})}
	slack, sms, _ := r.run(ev)
	if slack != 1 || sms != 1 {
		t.Fatalf("slack %d sms %d; want one each", slack, sms)
	}
	if r.metrics.recipientMissing != 0 {
		t.Fatalf("recipient_missing %d for recipients that are all there", r.metrics.recipientMissing)
	}
	if len(r.fs.alertDelivered) != 1 {
		t.Fatal("the announcement was not credited")
	}

	// The same snapshot with a recipient whose channel was deleted: exactly that one is missing.
	r = newPhoneRig(t)
	ev.ID, ev.Payload = "e2", payload(t, domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown, Seq: 1,
		Recipients: []string{rigSlack, rigSMS, "deleted-channel"}})
	r.run(ev)
	if r.metrics.recipientMissing != 1 {
		t.Fatalf("recipient_missing %d; want the deleted one", r.metrics.recipientMissing)
	}
}

// A service alert D18 does not phone still has its phone recipients counted as there (D31: Resolved
// follows rule 1 and nothing else): a burn CLEAR, a close that is not a recovery, and a recovered
// close to a voice channel are not reported as missing recipients, and reach no phone.
func TestUnphonedServiceAlertsDoNotReportPhoneRecipientsMissing(t *testing.T) {
	cases := []struct {
		name string
		a    domain.ServiceAlert
	}{
		{"burn CLEAR", domain.ServiceAlert{Signal: domain.ServiceSignalBurn, CloseReason: domain.CloseRecovered}},
		{"health close: visibility_lost", domain.ServiceAlert{Signal: domain.ServiceSignalHealth, CloseReason: domain.CloseVisibilityLost}},
		{"health close: recovered, to a voice channel", domain.ServiceAlert{Signal: domain.ServiceSignalHealth, CloseReason: domain.CloseRecovered}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newPhoneRig(t)
			a := c.a
			a.ServiceID, a.ProjectID, a.ServiceSlug, a.Seq = "s1", "p1", "checkout", 1
			a.Recipients = []string{rigSlack, rigVoice}
			if a.CloseReason != domain.CloseRecovered || a.Signal == domain.ServiceSignalBurn {
				a.Recipients = []string{rigSlack, rigSMS, rigVoice}
			}
			slack, sms, calls := r.run(domain.OutboxEvent{ID: "e1", Topic: domain.TopicServiceAlert, Payload: payload(t, a)})
			if slack != 1 || sms != 0 || calls != 0 {
				t.Fatalf("slack %d sms %d calls %d; want the Slack close only", slack, sms, calls)
			}
			if r.metrics.recipientMissing != 0 || r.metrics.undeliverable["health"]+r.metrics.undeliverable["burn"] != 0 {
				t.Fatalf("recipient_missing %d, undeliverable %v: phone recipients that are there were reported",
					r.metrics.recipientMissing, r.metrics.undeliverable)
			}
			if len(r.ps.rowsFor(rigSMS))+len(r.ps.rowsFor(rigVoice)) != 0 {
				t.Fatal("a phone channel was handed an alert D18 does not phone it")
			}
		})
	}
}

// A provider that cannot be reached is not an answer: the event is retried, and the announcement is
// neither credited nor condemned while a retry is owed.
func TestServiceAlertPhoneUnreachableIsRetried(t *testing.T) {
	r := newPhoneRig(t)
	r.provider.ScriptSMS(telephony.Result{Outcome: telephony.Unreachable, HTTPStatus: 503})
	r.run(domain.OutboxEvent{ID: "e1", Topic: domain.TopicServiceAlert, Payload: payload(t, domain.ServiceAlert{
		ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
		Firing: true, State: domain.ServiceAlertDown, Seq: 1, Recipients: []string{rigSMS}})})
	if len(r.fs.failed) != 1 {
		t.Fatalf("failed %v; want the event retried", r.fs.failed)
	}
	if len(r.fs.alertDelivered) != 0 || len(r.fs.condemned) != 0 {
		t.Fatalf("credited %d condemned %d; want neither while a retry is owed", len(r.fs.alertDelivered), len(r.fs.condemned))
	}
}

// Task 12 review, m-3: when the phone routes of an unphoned close cannot be counted, the two counts
// that would read them — recipient_missing and undeliverable — are not recorded at all rather than
// recorded false. The close itself is delivered as before.
func TestUnphonedCloseWhosePhoneRoutesCannotBeCountedRecordsNoFalseDiagnostics(t *testing.T) {
	for _, recipients := range [][]string{{"slack1", "sms1"}, {"sms1"}} {
		t.Run(fmt.Sprint(recipients), func(t *testing.T) {
			fs := &fakeStore{}
			nf := &fakeNotify{phoneIDs: map[string]bool{"sms1": true}, phoneRoutesErr: errors.New("db down")}
			resolved := len(recipients) - 1 // the text half resolves the Slack channel, if any
			nf.resolved = &resolved
			m := &fakeMetrics{}
			fs.pending = []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicServiceAlert, Attempts: 1,
				Payload: payload(t, domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout",
					Signal: domain.ServiceSignalBurn, CloseReason: domain.CloseRecovered, Seq: 1, Recipients: recipients})}}
			newWorker(fs, nil, nf, m).drain(context.Background())
			if len(nf.routeCalls) != 1 || len(nf.phoneCalls) != 0 {
				t.Fatalf("PhoneRoutes calls %v, phone calls %d", nf.routeCalls, len(nf.phoneCalls))
			}
			if m.recipientMissing != 0 || len(m.undeliverable) != 0 {
				t.Fatalf("recipient_missing %d, undeliverable %v; want neither recorded", m.recipientMissing, m.undeliverable)
			}
			if len(fs.delivered) != 1 || len(fs.condemned) != 0 {
				t.Fatalf("delivered %v condemned %d", fs.delivered, len(fs.condemned))
			}
		})
	}
}

// FR-041a review P2-7: a phone half that fails BEFORE it has counted the event's phone routes — the
// project name it prepares the message with cannot be read, the delivery record cannot be read, or
// the text read failed and so did the phone-id-only read behind it — does not know whether the phone
// recipients are there. The event is retried, and nothing is recorded as a missing recipient or an
// undeliverable announcement.
func TestServiceAlertPhoneHalfFailingBeforeCountingIsNotAMissingRecipient(t *testing.T) {
	onset := domain.OutboxEvent{ID: "e1", Topic: domain.TopicServiceAlert, Payload: payload(t, domain.ServiceAlert{
		ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
		Firing: true, State: domain.ServiceAlertDown, Seq: 1, Recipients: []string{rigSlack, rigSMS}})}
	injected := errors.New("injected: db down")
	check := func(t *testing.T, fs *fakeStore, m *fakeMetrics) {
		t.Helper()
		if len(fs.failed) != 1 {
			t.Fatalf("failed %v; want the event retried", fs.failed)
		}
		if m.recipientMissing != 0 || len(m.undeliverable) != 0 {
			t.Fatalf("recipient_missing %d, undeliverable %v; want neither recorded", m.recipientMissing, m.undeliverable)
		}
	}
	t.Run("the message cannot be prepared", func(t *testing.T) {
		r := newPhoneRig(t)
		r.fs.projectErr = injected
		r.run(onset)
		check(t, r.fs, r.metrics)
	})
	t.Run("the delivery record cannot be read", func(t *testing.T) {
		r := newPhoneRig(t)
		r.ps.targetsErr = injected
		r.run(onset)
		check(t, r.fs, r.metrics)
	})
	t.Run("the text read and the phone-id read both fail", func(t *testing.T) {
		r := newBrokenTextRig(t, brokenTextReads{})
		r.fs.phoneIDsErr = injected
		m := &fakeMetrics{}
		r.w.metrics = m
		r.attempt(onset, 1)
		check(t, r.fs, m)
	})
}
