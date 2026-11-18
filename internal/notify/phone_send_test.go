package notify

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a Task 12: what the outbox worker needs of DeliverPhone beyond Task 11 — D18's voice column,
// the count of phone routes for an alert that is not phoned — and the five findings of the Task 11
// review that live in the code the worker calls.

// D18: a recovery is an SMS only. A voice channel in the recipient set is not handed it — no row, no
// call — but it IS a route by rule 1, so Resolved counts it (D31: Resolved follows rule 1 and nothing
// else); otherwise a service's recovered close with a voice recipient would report that recipient as
// missing.
func TestDeliverPhoneSkipsVoiceForAnSMSOnlyKind(t *testing.T) {
	for _, kind := range []telephony.AlertKind{telephony.KindMonitorRecovered, telephony.KindServiceRecovered} {
		t.Run(string(kind), func(t *testing.T) {
			sms := smsTarget("c1", numA)
			voice := voiceTarget("c2", "u2", numB)
			voiceOff := domain.PhoneTarget{ChannelID: "c3", ProjectID: "p1", Kind: domain.ChannelVoice,
				Recipient: domain.PhoneRecipientUser, UserID: "u3", NotRouteReason: domain.DeliverableNoVerifiedNumber}
			st := newPhoneStore(sms, voice, voiceOff)
			p := telephony.NewFakeProvider()
			d := New(st, nil).WithPhone(st, enabledTelephony(p), nil, discardLogger())
			alert := telephony.PhoneAlert{Kind: kind, SubjectName: "checkout", ProjectName: "prod"}

			res, err := d.DeliverPhone(context.Background(), []string{"c1", "c2", "c3"}, alert,
				domain.PhoneEventRef{OutboxEventID: "ev1"})
			if err != nil {
				t.Fatal(err)
			}
			if calls := p.Calls(); len(calls) != 1 || calls[0].Kind != "sms" {
				t.Fatalf("provider calls = %+v; want the one SMS", calls)
			}
			if len(st.reserved) != 1 || st.reserved[0].ChannelID != "c1" || len(st.unsent) != 0 {
				t.Fatalf("reserved %+v unsent %+v; want only the sms channel handed the alert", st.reserved, st.unsent)
			}
			if want := (domain.ChannelDelivery{Requested: 3, Resolved: 2, Delivered: 1}); res != want {
				t.Fatalf("delivery = %+v; want %+v", res, want)
			}
		})
	}
}

// PhoneRoutes counts the phone routes among a set without handing anybody anything: what a service
// alert that D18 does not phone (a burn CLEAR, a close that is not a recovery) adds to Resolved.
func TestPhoneRoutes(t *testing.T) {
	unreadable := smsTarget("c4", "")
	unreadable.NumberErr = errors.New("store: open the number of phone channel c4: authentication failed")
	st := newPhoneStore(
		smsTarget("c1", numA),
		voiceTarget("c2", "u2", numB),
		domain.PhoneTarget{ChannelID: "c3", ProjectID: "p1", Kind: domain.ChannelSMS, Recipient: domain.PhoneRecipientUser,
			UserID: "u3", NotRouteReason: domain.DeliverableRecipientNotEligible},
		unreadable,
	)
	p := telephony.NewFakeProvider()
	src := enabledTelephony(p)
	d := New(st, nil).WithPhone(st, src, nil, discardLogger())
	n, err := d.PhoneRoutes(context.Background(), []string{"c1", "c2", "c3", "c4", "slack1"})
	if err != nil || n != 3 {
		t.Fatalf("PhoneRoutes = %d, %v; want 3 (c3 is no route; the unreadable number is a route)", n, err)
	}
	if len(p.Calls()) != 0 || len(st.reserved) != 0 || len(st.unsent) != 0 || src.built.Load() != 0 {
		t.Fatal("counting the routes delivered something")
	}

	// Unwired: nothing to count without a phone channel; an error, never a silent zero, with one.
	none := &fakeStore{channels: []domain.NotificationChannel{{ID: "s1", Type: domain.ChannelSlack, Enabled: true}}}
	if n, err := New(none, nil).PhoneRoutes(context.Background(), []string{"s1"}); n != 0 || err != nil {
		t.Fatalf("unwired, no phone channel: %d, %v", n, err)
	}
	withPhone := &fakeStore{channels: []domain.NotificationChannel{{ID: "p1", Type: domain.ChannelSMS, Enabled: true}}}
	if _, err := New(withPhone, nil).PhoneRoutes(context.Background(), []string{"p1"}); err == nil {
		t.Fatal("unwired with a phone channel: no error")
	}
}

// Task 11 review, finding 1: a target whose number cannot be opened fails its own channel — an error,
// so the event is retried; no reservation and no row — while the other channel is phoned once.
func TestDeliverPhoneUnreadableNumberFailsOnlyItsChannel(t *testing.T) {
	broken := voiceTarget("c2", "u2", "")
	broken.NumberErr = errors.New("store: open the number of phone channel c2: authentication failed")
	st := newPhoneStore(smsTarget("c1", numA), broken)
	p := telephony.NewFakeProvider()
	m := &phoneMetrics{}
	d := New(st, nil).WithPhone(st, enabledTelephony(p), m, discardLogger())

	res, err := d.DeliverPhone(context.Background(), []string{"c1", "c2"}, stepAlert, stepEvent)
	if err == nil || !strings.Contains(err.Error(), "channel c2") || strings.Contains(err.Error(), "channel c1") {
		t.Fatalf("err = %v; want an error for c2 alone", err)
	}
	if calls := p.Calls(); len(calls) != 1 || calls[0].To != numA {
		t.Fatalf("provider calls = %+v; want c1 phoned", calls)
	}
	if len(st.reserved) != 1 || st.reserved[0].ChannelID != "c1" || len(st.unsent) != 0 {
		t.Fatalf("reserved %+v unsent %+v; want c1 reserved and no row for c2", st.reserved, st.unsent)
	}
	// c2 is a route by configuration: counted in Resolved (rule 1), not delivered.
	if want := (domain.ChannelDelivery{Requested: 2, Resolved: 2, Delivered: 1}); res != want {
		t.Fatalf("delivery = %+v; want %+v", res, want)
	}
	if len(m.deliveries) != 1 || m.deliveries["sms|fake|sent"] != 1 {
		t.Fatalf("metrics %v; want only c1's sent", m.deliveries)
	}
}

// Task 11 review, finding 2: the first attempt phoned the channel, a webhook failed the event, and
// before the retry the recipient stopped being a route. The retry writes nothing (the row exists) and
// must still report the person as reached.
func TestDeliverPhoneUnsentRetryCountsAnEarlierSentRow(t *testing.T) {
	gone := domain.PhoneTarget{ChannelID: "c1", ProjectID: "p1", Kind: domain.ChannelSMS,
		Recipient: domain.PhoneRecipientUser, UserID: "u1", NotRouteReason: domain.DeliverableRecipientNotEligible}
	st := newPhoneStore(gone)
	st.unsentExisting["c1"] = domain.PhoneReasonSent
	m := &phoneMetrics{}
	d := New(st, nil).WithPhone(st, enabledTelephony(telephony.NewFakeProvider()), m, discardLogger())
	res, err := d.DeliverPhone(context.Background(), []string{"c1"}, stepAlert, stepEvent)
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.ChannelDelivery{Requested: 1, Resolved: 0, Delivered: 1}); res != want {
		t.Fatalf("delivery = %+v; want %+v (no route now, reached on the earlier attempt)", res, want)
	}
	if len(m.deliveries) != 0 {
		t.Fatalf("metrics %v; want none: nothing was written", m.deliveries)
	}

	// An earlier row that is not `sent` is not delivered.
	st = newPhoneStore(gone)
	st.unsentExisting["c1"] = domain.PhoneReasonProviderRejected
	d = New(st, nil).WithPhone(st, enabledTelephony(telephony.NewFakeProvider()), nil, discardLogger())
	if res, err := d.DeliverPhone(context.Background(), []string{"c1"}, stepAlert, stepEvent); err != nil || res.Delivered != 0 {
		t.Fatalf("over a rejected row: %+v, %v", res, err)
	}
}

// Task 11 review, finding 3: the database says a voice channel is a route (the provider was switched to
// Twilio), but this replica's settings snapshot still builds a provider without voice. That is not a
// refusal to record as final: it is provider_unreachable, and the event is retried after the snapshot
// refreshes. No request is made.
func TestDeliverPhoneVoiceUnderASnapshotWithoutVoiceIsRetried(t *testing.T) {
	st := newPhoneStore(voiceTarget("c1", "u1", numA))
	p := telephony.NewFakeProvider()
	p.SetSupportsVoice(false)
	m := &phoneMetrics{}
	d := New(st, nil).WithPhone(st, enabledTelephony(p), m, discardLogger())
	res, err := d.DeliverPhone(context.Background(), []string{"c1"}, stepAlert, stepEvent)
	if err == nil {
		t.Fatal("err = nil; want provider_unreachable so the outbox retries")
	}
	if got := st.finished["c1"].Reason; got != domain.PhoneReasonProviderUnreachable {
		t.Fatalf("finished %q; want %q", got, domain.PhoneReasonProviderUnreachable)
	}
	if len(p.Calls()) != 0 {
		t.Fatal("a request was made")
	}
	if want := (domain.ChannelDelivery{Requested: 1, Resolved: 1}); res != want {
		t.Fatalf("delivery = %+v; want %+v", res, want)
	}
}

// Task 11 review, finding 4: a reservation status this code does not know (a later part's) is an error,
// never an existing final row counted silently.
func TestDeliverPhoneUnknownReservationStatusFails(t *testing.T) {
	st := newPhoneStore(smsTarget("c1", numA))
	st.reserveStatus["c1"] = domain.PhoneReservationStatus("quota_exceeded")
	p := telephony.NewFakeProvider()
	d := New(st, nil).WithPhone(st, enabledTelephony(p), nil, discardLogger())
	res, err := d.DeliverPhone(context.Background(), []string{"c1"}, stepAlert, stepEvent)
	if err == nil || !strings.Contains(err.Error(), "quota_exceeded") {
		t.Fatalf("err = %v; want an error naming the status", err)
	}
	if len(p.Calls()) != 0 || res.Delivered != 0 {
		t.Fatalf("calls %d, delivery %+v", len(p.Calls()), res)
	}
}

// leakyStore answers every channel read of notify.Store with a phone channel among Slack ones. When a
// method is added to notify.Store this stops compiling; the method added here then makes
// TestDispatchStoreFiltersEveryChannelRead fail until dispatchStore filters it too.
type leakyStore struct{}

func leakyChannels() []domain.NotificationChannel {
	return []domain.NotificationChannel{
		{ID: "slack1", Type: domain.ChannelSlack, Enabled: true},
		{ID: "sms1", Type: domain.ChannelSMS, Enabled: true},
		{ID: "voice1", Type: domain.ChannelVoice, Enabled: true},
	}
}

func (leakyStore) ListEnabledChannelsForMonitor(context.Context, string) ([]domain.NotificationChannel, error) {
	return leakyChannels(), nil
}
func (leakyStore) ListEnabledChannelsByProject(context.Context, string) ([]domain.NotificationChannel, error) {
	return leakyChannels(), nil
}
func (leakyStore) EnabledChannelsByIDs(context.Context, []string) ([]domain.NotificationChannel, error) {
	return leakyChannels(), nil
}

// Task 11 review, finding 7: every read of notify.Store that returns channels is filtered by
// dispatchStore, found by reflection over the interface rather than listed by hand, so a fourth read
// cannot hand a text path a phone channel unnoticed.
func TestDispatchStoreFiltersEveryChannelRead(t *testing.T) {
	channelType := reflect.TypeOf(domain.NotificationChannel{})
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	iface := reflect.TypeOf((*Store)(nil)).Elem()
	ds := reflect.ValueOf(&dispatchStore{Store: leakyStore{}})
	checked := 0
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		returnsChannels := false
		for o := 0; o < m.Type.NumOut(); o++ {
			out := m.Type.Out(o)
			if out == channelType || (out.Kind() == reflect.Slice && out.Elem() == channelType) {
				returnsChannels = true
			}
		}
		if !returnsChannels {
			continue
		}
		args := make([]reflect.Value, m.Type.NumIn())
		for a := range args {
			if in := m.Type.In(a); in == ctxType {
				args[a] = reflect.ValueOf(context.Background())
			} else {
				args[a] = reflect.Zero(in)
			}
		}
		for _, out := range ds.MethodByName(m.Name).Call(args) {
			var chs []domain.NotificationChannel
			switch v := out.Interface().(type) {
			case []domain.NotificationChannel:
				chs = v
			case domain.NotificationChannel:
				chs = []domain.NotificationChannel{v}
			default:
				continue
			}
			for _, ch := range chs {
				if domain.IsPhoneChannelType(ch.Type) {
					t.Errorf("dispatchStore.%s hands a text path the %s channel %s: override it in dispatchStore",
						m.Name, ch.Type, ch.ID)
				}
			}
			if len(chs) == 0 {
				t.Errorf("dispatchStore.%s returned no channel: the leaky store's Slack channel must pass", m.Name)
			}
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("checked %d channel reads; notify.Store has at least three", checked)
	}
}
