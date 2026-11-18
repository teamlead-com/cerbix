package outbox

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// What the worker hands DeliverPhone (FR-041a D16, D32, R15, §4.4): the alert's facts, the incident
// link and the notes only the caller can know — with a fakeNotify, so the hand-off itself is the
// observation.

var incLink = store.PhoneLink{IncidentID: "inc1", Ref: "X7K2P9QM"}

// linkWorld is a worker over a monitor m1 (auto_incident on) with an sms and a voice channel linked,
// project "prod", and a 10 ms lookup interval so D32's wait is observable without a second of sleep.
func linkWorld(t *testing.T) (*fakeStore, *fakeNotify, *Worker) {
	t.Helper()
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", ProjectID: "p1", Name: "payments-api",
			Status: domain.StatusDown, StateSequence: 7, AutoIncident: true}},
		phoneFake: phoneFake{
			monitorLinks: map[string][]store.PhoneLink{},
			serviceLinks: map[string]store.PhoneLink{},
			projects:     map[string]domain.Project{"p1": {ID: "p1", Name: "prod"}},
			services:     map[string]domain.Service{"s1": {ID: "s1", ProjectID: "p1", Slug: "checkout", Name: "Checkout"}},
			incidents: map[string]domain.Incident{
				"inc1": {ID: "inc1", ProjectID: "p1", MonitorID: "m1", Source: domain.SourceAuto},
				"inc2": {ID: "inc2", ProjectID: "p1", ServiceID: "s1", Source: domain.SourceAuto},
			},
			refs: map[string]string{"inc1": "X7K2P9QM", "inc2": "7MQ4H2ZD"},
		},
	}
	nf := &fakeNotify{monitorPhones: map[string][]string{"m1": {"sms1", "voice1"}},
		phoneIDs: map[string]bool{"sms1": true, "voice1": true}}
	w := newWorker(fs, nil, nf, &fakeMetrics{}).
		WithPhoneLinks(func() string { return "https://cerbix.example" }, func() string { return "https://mail.example" })
	w.phone.refInterval = 10 * time.Millisecond
	return fs, nf, w
}

func monitorEvent(t *testing.T, id string, prev, cur domain.MonitorStatus, created time.Time) domain.OutboxEvent {
	t.Helper()
	return domain.OutboxEvent{ID: id, Topic: domain.TopicMonitorTransition, Attempts: 1, CreatedAt: created,
		Payload: payload(t, domain.MonitorTransition{MonitorID: "m1", Prev: prev, Cur: cur, Seq: 7})}
}

func deliverOne(t *testing.T, fs *fakeStore, w *Worker, e domain.OutboxEvent) {
	t.Helper()
	fs.pending = append(fs.pending, e)
	w.drain(context.Background())
}

// §11 "Outbox — monitor race", the worker's half: a DOWN delivered before the reconciler opens the
// incident and one delivered after make the same send to the same channels; only the second carries
// the link, and the first says no_incident_ref. Before giving up, the worker looks for the incident
// four more times, 250 ms apart in production (D32). The row's attachment to the incident that opens
// later is the store's (TestPhoneDeliveryAttachRace) and is exercised end to end in
// TestPhoneDeliveryOnPostgresMonitorRace.
func TestMonitorDownWaitsForItsIncidentRef(t *testing.T) {
	t.Run("never opens: sent without a link", func(t *testing.T) {
		fs, nf, w := linkWorld(t)
		e := monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now())
		before := append([]byte(nil), e.Payload...)
		start := time.Now()
		deliverOne(t, fs, w, e)
		if len(nf.phoneCalls) != 1 {
			t.Fatalf("phone calls %d; want 1", len(nf.phoneCalls))
		}
		c := nf.phoneCalls[0]
		if c.alert.Kind != telephony.KindMonitorDown || c.alert.IncidentRef != "" || c.alert.SubjectName != "payments-api" ||
			c.alert.ProjectName != "prod" {
			t.Fatalf("alert = %+v", c.alert)
		}
		wantEv := domain.PhoneEventRef{OutboxEventID: "e1", MonitorID: "m1", MonitorSeq: 7,
			Notes: []string{domain.PhoneNoteNoIncidentRef}}
		if !reflect.DeepEqual(c.ev, wantEv) {
			t.Fatalf("event ref = %+v; want %+v", c.ev, wantEv)
		}
		if !reflect.DeepEqual(c.channelIDs, []string{"sms1", "voice1"}) {
			t.Fatalf("channels %v", c.channelIDs)
		}
		if n := len(fs.linkCalls); n != 5 {
			t.Fatalf("%d lookups; want the first and four more", n)
		}
		if gap := fs.linkCalls[4].at.Sub(fs.linkCalls[0].at); gap < 40*time.Millisecond {
			t.Fatalf("the four retries took %v; want them 10 ms apart here (250 ms in production)", gap)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("took %v", took)
		}
		if !bytes.Equal(e.Payload, before) {
			t.Fatal("the stored event's payload bytes changed (P14)")
		}
		if len(fs.delivered) != 1 {
			t.Fatalf("delivered %v failed %q", fs.delivered, fs.failReasons)
		}
	})
	t.Run("opens on the third lookup: the same send, with the link", func(t *testing.T) {
		fs, nf, w := linkWorld(t)
		fs.monitorLinks["m1"] = []store.PhoneLink{{}, {}, incLink}
		deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now()))
		c := nf.phoneCalls[0]
		if c.alert.IncidentRef != incLink.Ref || c.alert.PublicBaseURL != "https://cerbix.example" || len(c.ev.Notes) != 0 {
			t.Fatalf("alert %+v notes %v; want the link and no note", c.alert, c.ev.Notes)
		}
		// The DOWN's row finds its incident under D32's lock in the store, not from the caller.
		if c.ev.IncidentID != "" || c.ev.MonitorSeq != 7 {
			t.Fatalf("event ref %+v", c.ev)
		}
		if !reflect.DeepEqual(c.channelIDs, []string{"sms1", "voice1"}) || len(fs.linkCalls) != 3 {
			t.Fatalf("channels %v, lookups %d", c.channelIDs, len(fs.linkCalls))
		}
	})
	t.Run("already open: no wait", func(t *testing.T) {
		fs, nf, w := linkWorld(t)
		fs.monitorLinks["m1"] = []store.PhoneLink{incLink}
		deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now()))
		if len(fs.linkCalls) != 1 || nf.phoneCalls[0].alert.IncidentRef != incLink.Ref {
			t.Fatalf("lookups %d alert %+v", len(fs.linkCalls), nf.phoneCalls[0].alert)
		}
	})
	t.Run("auto_incident off: one lookup, no wait, no note", func(t *testing.T) {
		fs, nf, w := linkWorld(t)
		m := fs.monitors["m1"]
		m.AutoIncident = false
		fs.monitors["m1"] = m
		deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now()))
		if len(fs.linkCalls) != 1 || len(nf.phoneCalls[0].ev.Notes) != 0 {
			t.Fatalf("lookups %d notes %v", len(fs.linkCalls), nf.phoneCalls[0].ev.Notes)
		}
	})
	t.Run("the wait stays inside the delivery budget", func(t *testing.T) {
		fs, nf, w := linkWorld(t)
		w.phone.refInterval = 250 * time.Millisecond
		claimed := time.Now()
		e := monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, claimed)
		// A lease of 2.3 s leaves 300 ms of budget after the headroom: room for two of the four waits.
		e.ClaimedAt, e.LeaseUntil = claimed, claimed.Add(leaseHeadroom+300*time.Millisecond)
		start := time.Now()
		deliverOne(t, fs, w, e)
		if took := time.Since(start); took > 700*time.Millisecond {
			t.Fatalf("the delivery took %v; the wait must end with the budget", took)
		}
		if len(nf.phoneCalls) != 1 {
			t.Fatal("the phone was not handed the alert once the wait ended")
		}
		// The lease budget reaches DeliverPhone as a deadline, so min(5 s, remaining) means what it says.
		if d := nf.phoneCalls[0].deadline; d.IsZero() || d.After(claimed.Add(300*time.Millisecond+50*time.Millisecond)) {
			t.Fatalf("DeliverPhone's deadline %v; want the lease budget (≈ %v)", d, claimed.Add(300*time.Millisecond))
		}
	})
}

// D32 for a recovery: the link is the monitor's open auto-incident, else its latest one resolved at or
// after the event was enqueued — read with the event's created_at, which the claim carries. Before and
// after the reconciler resolves it, the same incident is linked; with only an older outage resolved,
// there is no link, and no note unless the monitor has auto_incident on. Never a voice call (D18:
// DeliverPhone drops voice for this kind).
func TestMonitorRecoveryLink(t *testing.T) {
	enqueued := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	for _, when := range []string{"before the reconciler resolves", "after it resolves"} {
		t.Run(when, func(t *testing.T) {
			fs, nf, w := linkWorld(t)
			fs.monitorLinks["m1"] = []store.PhoneLink{incLink} // open, or resolved since: the store's choice
			deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusDown, domain.StatusUp, enqueued))
			if len(fs.linkCalls) != 1 || !fs.linkCalls[0].since.Equal(enqueued) {
				t.Fatalf("lookups %+v; want one, since the event's created_at %v", fs.linkCalls, enqueued)
			}
			c := nf.phoneCalls[0]
			if c.alert.Kind != telephony.KindMonitorRecovered || c.alert.IncidentRef != incLink.Ref ||
				c.ev.IncidentID != incLink.IncidentID || len(c.ev.Notes) != 0 {
				t.Fatalf("alert %+v ev %+v", c.alert, c.ev)
			}
		})
	}
	t.Run("only an older outage: no link, noted with auto_incident", func(t *testing.T) {
		fs, nf, w := linkWorld(t)
		deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusDown, domain.StatusUp, enqueued))
		c := nf.phoneCalls[0]
		if c.alert.IncidentRef != "" || c.ev.IncidentID != "" ||
			!reflect.DeepEqual(c.ev.Notes, []string{domain.PhoneNoteNoIncidentRef}) || len(fs.linkCalls) != 1 {
			t.Fatalf("alert %+v ev %+v lookups %d", c.alert, c.ev, len(fs.linkCalls))
		}
	})
	t.Run("only an older outage, auto_incident off: no link, no note", func(t *testing.T) {
		fs, nf, w := linkWorld(t)
		m := fs.monitors["m1"]
		m.AutoIncident = false
		fs.monitors["m1"] = m
		deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusDown, domain.StatusUp, enqueued))
		if c := nf.phoneCalls[0]; c.alert.IncidentRef != "" || len(c.ev.Notes) != 0 {
			t.Fatalf("alert %+v notes %v", c.alert, c.ev.Notes)
		}
	})
}

// The facts of each phoned topic as the worker hands them over (§4.4, R15, R17).
func TestPhoneAlertFacts(t *testing.T) {
	enqueued := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		event  domain.OutboxEvent
		setup  func(fs *fakeStore)
		ids    []string
		alert  telephony.PhoneAlert
		ev     domain.PhoneEventRef
		lookup string // the link lookup made, "" for none
	}{
		{name: "monitor burn: no incident, no link",
			event: domain.OutboxEvent{ID: "e1", Topic: domain.TopicSLOBurnAlert, Payload: payload(t, domain.SLOBurnAlert{
				MonitorID: "m1", Window: "30d", WindowSeconds: 3600, BurnRate: 14.4, Firing: true})},
			ids: []string{"sms1", "voice1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindMonitorBurn, BurnRate: 14.4, Window: "1h",
				SubjectName: "payments-api", ProjectName: "prod", PublicBaseURL: "https://cerbix.example"},
			ev: domain.PhoneEventRef{OutboxEventID: "e1", MonitorID: "m1"}},
		{name: "service health onset: the service's incident (R15)",
			event: domain.OutboxEvent{ID: "e2", Topic: domain.TopicServiceAlert, CreatedAt: enqueued, Payload: payload(t, domain.ServiceAlert{
				ServiceID: "s1", ProjectID: "p1", ServiceName: "Checkout", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
				Firing: true, State: domain.ServiceAlertDegraded, Seq: 1, Recipients: []string{"slack1", "sms1"}})},
			setup: func(fs *fakeStore) { fs.serviceLinks["s1"] = store.PhoneLink{IncidentID: "inc2", Ref: "7MQ4H2ZD"} },
			ids:   []string{"sms1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindServiceHealth, State: "degraded", SubjectName: "checkout",
				IsService: true, ProjectName: "prod", IncidentRef: "7MQ4H2ZD", PublicBaseURL: "https://cerbix.example"},
			ev: domain.PhoneEventRef{OutboxEventID: "e2", ServiceID: "s1", IncidentID: "inc2",
				ServiceState: string(domain.ServiceAlertDegraded)},
			lookup: "service:s1"},
		// A service incident opens with its alert (no D32 race), so a health or recovery alert that
		// finds none says so (FR-041a review P2-7). The recovery's state is not a paging state: its
		// row has no service_state (revision-7 review M-3).
		{name: "service recovered close without an incident: no link, noted, no service state",
			event: domain.OutboxEvent{ID: "e3", Topic: domain.TopicServiceAlert, CreatedAt: enqueued, Payload: payload(t, domain.ServiceAlert{
				ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
				CloseReason: domain.CloseRecovered, State: domain.ServiceAlertHealthy, Seq: 1, Recipients: []string{"sms1"}})},
			ids: []string{"sms1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindServiceRecovered, SubjectName: "checkout", IsService: true,
				ProjectName: "prod", PublicBaseURL: "https://cerbix.example"},
			ev:     domain.PhoneEventRef{OutboxEventID: "e3", ServiceID: "s1", Notes: []string{domain.PhoneNoteNoIncidentRef}},
			lookup: "service:s1"},
		{name: "service health onset without an incident: no link, noted",
			event: domain.OutboxEvent{ID: "e3b", Topic: domain.TopicServiceAlert, CreatedAt: enqueued, Payload: payload(t, domain.ServiceAlert{
				ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
				Firing: true, State: domain.ServiceAlertDown, Seq: 1, Recipients: []string{"sms1"}})},
			ids: []string{"sms1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindServiceHealth, State: "down", SubjectName: "checkout", IsService: true,
				ProjectName: "prod", PublicBaseURL: "https://cerbix.example"},
			ev: domain.PhoneEventRef{OutboxEventID: "e3b", ServiceID: "s1", ServiceState: string(domain.ServiceAlertDown),
				Notes: []string{domain.PhoneNoteNoIncidentRef}},
			lookup: "service:s1"},
		// A burn payload that carries a state (a buggy writer of service_state for every kind would
		// copy it): the row still has none (FR-041a review C m-3).
		{name: "service burn: no link, no service state",
			event: domain.OutboxEvent{ID: "e4", Topic: domain.TopicServiceAlert, CreatedAt: enqueued, Payload: payload(t, domain.ServiceAlert{
				ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalBurn, Firing: true, State: domain.ServiceAlertDown,
				Seq: 1, BurnRate: 6, WindowSeconds: 21600, Window: "30d", Recipients: []string{"sms1"}})},
			ids: []string{"sms1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindServiceBurn, BurnRate: 6, Window: "6h", SubjectName: "checkout",
				IsService: true, ProjectName: "prod", PublicBaseURL: "https://cerbix.example"},
			ev: domain.PhoneEventRef{OutboxEventID: "e4", ServiceID: "s1"}},
		{name: "monitor escalation step: its incident, step counted from 1",
			event: domain.OutboxEvent{ID: "e5", Topic: domain.TopicEscalationStep, Payload: payload(t, domain.EscalationStepAlert{
				IncidentID: "inc1", MonitorID: "m1", MonitorName: "payments-api", SubjectName: "payments-api",
				Step: 1, ChannelIDs: []string{"slack1", "voice1"}})},
			ids: []string{"voice1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindEscalationStep, Step: 2, SubjectName: "payments-api",
				ProjectName: "prod", IncidentRef: "X7K2P9QM", PublicBaseURL: "https://cerbix.example"},
			ev: domain.PhoneEventRef{OutboxEventID: "e5", IncidentID: "inc1", MonitorID: "m1", EscalationStep: 2}},
		{name: "service escalation step repeat: the service by its slug",
			event: domain.OutboxEvent{ID: "e6", Topic: domain.TopicEscalationStep, Payload: payload(t, domain.EscalationStepAlert{
				IncidentID: "inc2", ServiceID: "s1", MonitorName: "Checkout", SubjectName: "Checkout",
				Step: 2, Repeat: true, ChannelIDs: []string{"sms1"}})},
			ids: []string{"sms1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindEscalationStep, Step: 3, Repeat: true, SubjectName: "checkout",
				IsService: true, ProjectName: "prod", IncidentRef: "7MQ4H2ZD", PublicBaseURL: "https://cerbix.example"},
			ev: domain.PhoneEventRef{OutboxEventID: "e6", IncidentID: "inc2", ServiceID: "s1", EscalationStep: 3}},
		{name: "escalation step whose incident is gone: no link, noted",
			event: domain.OutboxEvent{ID: "e7", Topic: domain.TopicEscalationStep, Payload: payload(t, domain.EscalationStepAlert{
				IncidentID: "gone", MonitorID: "m1", SubjectName: "payments-api", Step: 0, ChannelIDs: []string{"sms1"}})},
			ids: []string{"sms1"},
			alert: telephony.PhoneAlert{Kind: telephony.KindEscalationStep, Step: 1, SubjectName: "payments-api",
				PublicBaseURL: "https://cerbix.example"},
			// m-2: the row names no incident that is not there (its foreign key).
			ev: domain.PhoneEventRef{OutboxEventID: "e7", MonitorID: "m1", EscalationStep: 1,
				Notes: []string{domain.PhoneNoteNoIncidentRef}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, nf, w := linkWorld(t)
			if c.setup != nil {
				c.setup(fs)
			}
			c.event.Attempts = 1
			deliverOne(t, fs, w, c.event)
			if len(nf.phoneCalls) != 1 {
				t.Fatalf("phone calls %d; failed %q", len(nf.phoneCalls), fs.failReasons)
			}
			got := nf.phoneCalls[0]
			if !reflect.DeepEqual(got.channelIDs, c.ids) {
				t.Fatalf("channels %v; want %v", got.channelIDs, c.ids)
			}
			if got.alert != c.alert {
				t.Fatalf("alert = %+v\nwant    %+v", got.alert, c.alert)
			}
			if !reflect.DeepEqual(got.ev, c.ev) {
				t.Fatalf("event ref = %+v; want %+v", got.ev, c.ev)
			}
			switch {
			case c.lookup == "" && len(fs.linkCalls) != 0:
				t.Fatalf("link lookups %+v; want none", fs.linkCalls)
			case c.lookup != "" && (len(fs.linkCalls) != 1 || fs.linkCalls[0].subject != c.lookup || !fs.linkCalls[0].since.Equal(enqueued)):
				t.Fatalf("link lookups %+v; want %s since %v", fs.linkCalls, c.lookup, enqueued)
			}
		})
	}
}

// §4.5 `no_public_url`: the telephony settings' public_base_url, else mail's; with neither the message
// keeps the reference and drops the link, and the row says so.
func TestPhonePublicBaseURL(t *testing.T) {
	cases := []struct {
		name            string
		telephony, mail string
		want            string
		notes           []string
	}{
		{"telephony's", "https://cerbix.example", "https://mail.example", "https://cerbix.example", nil},
		{"mail's when telephony has none", "", "https://mail.example", "https://mail.example", nil},
		{"neither", "", "", "", []string{domain.PhoneNoteNoPublicURL}},
		// Final review M-5: mail's base is not validated at save. A base the renderer cannot carry is
		// no base: the link goes, the page does not.
		{"mail's with a space inside", "", "https://mail.example/a b", "", []string{domain.PhoneNoteNoPublicURL}},
		{"mail's with a control character", "", "https://mail.example/\x07", "", []string{domain.PhoneNoteNoPublicURL}},
		{"mail's padded with spaces", "", "  https://mail.example\t", "https://mail.example", nil},
		{"telephony's unusable, then mail's", "https://cerbix.example/a b", "https://mail.example", "https://mail.example", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, nf, w := linkWorld(t)
			w.WithPhoneLinks(func() string { return c.telephony }, func() string { return c.mail })
			fs.monitorLinks["m1"] = []store.PhoneLink{incLink}
			deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now()))
			got := nf.phoneCalls[0]
			if got.alert.PublicBaseURL != c.want || got.alert.IncidentRef != incLink.Ref || !reflect.DeepEqual(got.ev.Notes, c.notes) {
				t.Fatalf("alert %+v notes %v; want base %q notes %v", got.alert, got.ev.Notes, c.want, c.notes)
			}
		})
	}
	// A worker nobody gave the URLs (Task 14 wires them) behaves as "neither".
	fs, nf, _ := linkWorld(t)
	w := newWorker(fs, nil, nf, &fakeMetrics{})
	fs.monitorLinks["m1"] = []store.PhoneLink{incLink}
	deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now()))
	if got := nf.phoneCalls[0]; got.alert.PublicBaseURL != "" || !reflect.DeepEqual(got.ev.Notes, []string{domain.PhoneNoteNoPublicURL}) {
		t.Fatalf("unwired: alert %+v notes %v", got.alert, got.ev.Notes)
	}
}

// The two halves fail the event together: the errors of both calls are joined.
func TestPhoneAndTextErrorsAreJoined(t *testing.T) {
	fs, nf, w := linkWorld(t)
	nf.err = errors.New("slack: status 500")
	nf.phone = func([]string, telephony.PhoneAlert, domain.PhoneEventRef) (domain.ChannelDelivery, error) {
		return domain.ChannelDelivery{}, errors.New("channel sms1: provider unreachable: status 503")
	}
	fs.monitorLinks["m1"] = []store.PhoneLink{incLink}
	deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now()))
	if len(fs.failReasons) != 1 || !strings.Contains(fs.failReasons[0], "slack: status 500") ||
		!strings.Contains(fs.failReasons[0], "provider unreachable") {
		t.Fatalf("fail reasons %q; want both errors", fs.failReasons)
	}

}

// A project without a phone channel: the monitor path never calls the phone and reads nothing for it;
// a reminder, which D18 does not phone, reads nothing for it even with a phone channel linked.
func TestNoPhoneChannelNoPhoneCall(t *testing.T) {
	fs, nf, w := linkWorld(t)
	nf.monitorPhones = nil
	deliverOne(t, fs, w, monitorEvent(t, "e1", domain.StatusUp, domain.StatusDown, time.Now()))
	if nf.called != 1 || len(nf.phoneCalls) != 0 || len(fs.reads) != 0 {
		t.Fatalf("text %d, phone %d, phone-half reads %v", nf.called, len(nf.phoneCalls), fs.reads)
	}
	nf.monitorPhones = map[string][]string{"m1": {"sms1"}}
	reminder := domain.OutboxEvent{ID: "e2", Topic: domain.TopicMonitorTransition, Attempts: 1,
		Payload: payload(t, domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusDown, Cur: domain.StatusDown,
			Seq: 7, Reminder: true})}
	deliverOne(t, fs, w, reminder)
	if nf.called != 2 || len(nf.phoneCalls) != 0 || len(fs.failed) != 0 || len(fs.reads) != 0 {
		t.Fatalf("reminder: text %d, phone %d, failed %q, reads %v", nf.called, len(nf.phoneCalls), fs.failReasons, fs.reads)
	}
}
