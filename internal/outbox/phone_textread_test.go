package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// Task 12 re-review N-1: the phone half learns its channels from the text half's read. When that READ
// fails — one Slack or webhook channel whose config cannot be decrypted fails the whole list — the
// worker falls back to a phone-id-only read, so a broken text channel does not take the page with it.
// The event still fails for its text and is retried; the phone channels are phoned once (D19).
//
// A project without phone channels keeps what it had: the fallback read answers nothing, or fails,
// and the event fails with exactly the text's error. A text SEND that fails after a good read makes no
// fallback read at all.

// errUndecryptable is what the store answers for a channel list holding a config it cannot open.
var errUndecryptable = errors.New(`store: scan channel: store: decrypt channel config "url": secret: authentication failed`)

// brokenTextReads is notify.Store whose every channel read fails, as a list holding an undecryptable
// Slack config does.
type brokenTextReads struct{}

func (brokenTextReads) ListEnabledChannelsForMonitor(context.Context, string) ([]domain.NotificationChannel, error) {
	return nil, errUndecryptable
}
func (brokenTextReads) ListEnabledChannelsByProject(context.Context, string) ([]domain.NotificationChannel, error) {
	return nil, errUndecryptable
}
func (brokenTextReads) EnabledChannelsByIDs(context.Context, []string) ([]domain.NotificationChannel, error) {
	return nil, errUndecryptable
}

type brokenTextRig struct {
	fs       *fakeStore
	ps       *memPhoneStore
	provider *telephony.FakeProvider
	metrics  *fakeMetrics
	d        *notify.Dispatcher
	w        *Worker
}

// newBrokenTextRig is the phone rig (sms1 and voice1 are routes with numbers) behind a text read that
// always fails; the outbox store's phone-id-only reads know sms1 and voice1.
func newBrokenTextRig(t *testing.T, texts notify.Store) *brokenTextRig {
	t.Helper()
	base := newPhoneRig(t)
	r := &brokenTextRig{fs: base.fs, ps: base.ps, provider: base.provider, metrics: base.metrics}
	r.fs.monitorPhoneIDs = map[string][]string{"m1": {rigSMS, rigVoice}}
	r.fs.phoneChannels = map[string]bool{rigSMS: true, rigVoice: true}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tel := &rigTelephony{provider: r.provider}
	tel.ts = domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio}
	tel.ts.Normalize()
	r.d = notify.New(texts, nil).WithPhone(r.ps, tel, nil, logger)
	r.w = New(r.fs, nil, r.d, base.metrics, logger).
		WithPhoneLinks(func() string { return "https://cerbix.example" }, func() string { return "" })
	return r
}

// attempt delivers e once more and returns the provider calls it made and whether it failed.
func (r *brokenTextRig) attempt(e domain.OutboxEvent, n int) (calls int, failed bool) {
	before, failedBefore := len(r.provider.Calls()), len(r.fs.failed)
	e.Attempts = n
	r.fs.pending = append(r.fs.pending, e)
	r.w.drain(context.Background())
	return len(r.provider.Calls()) - before, len(r.fs.failed) > failedBefore
}

func textReadEvents(t *testing.T) []struct {
	name  string
	ev    domain.OutboxEvent
	calls int // provider calls the phone half makes: an SMS per phone channel, a call per voice one
} {
	mustJSON := func(v any) []byte { return payload(t, v) }
	onset := domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceName: "Checkout", ServiceSlug: "checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown, Seq: 1,
		Recipients: []string{rigSlack, rigSMS}}
	return []struct {
		name  string
		ev    domain.OutboxEvent
		calls int
	}{
		{"monitor DOWN", domain.OutboxEvent{ID: "e-down", Topic: domain.TopicMonitorTransition,
			Payload: mustJSON(domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusUp, Cur: domain.StatusDown, Seq: 7})}, 2},
		{"monitor burn firing", domain.OutboxEvent{ID: "e-burn", Topic: domain.TopicSLOBurnAlert,
			Payload: mustJSON(domain.SLOBurnAlert{MonitorID: "m1", Window: "30d", WindowSeconds: 3600, BurnRate: 14.4, Firing: true})}, 2},
		{"service health onset", domain.OutboxEvent{ID: "e-onset", Topic: domain.TopicServiceAlert, Payload: mustJSON(onset)}, 1},
		{"escalation step", domain.OutboxEvent{ID: "e-step", Topic: domain.TopicEscalationStep,
			Payload: mustJSON(domain.EscalationStepAlert{IncidentID: "inc1", MonitorID: "m1", SubjectName: "payments-api",
				Step: 1, ChannelIDs: []string{rigSlack, rigSMS, rigVoice}})}, 2},
	}
}

func TestAFailedTextReadStillPhones(t *testing.T) {
	for _, c := range textReadEvents(t) {
		t.Run(c.name, func(t *testing.T) {
			r := newBrokenTextRig(t, brokenTextReads{})
			c.ev.CreatedAt = time.Now()
			calls, failed := r.attempt(c.ev, 1)
			if calls != c.calls {
				t.Fatalf("first attempt: %d provider calls; want %d (the page goes out although the text read failed)", calls, c.calls)
			}
			if !failed || !strings.Contains(r.fs.failReasons[0], errUndecryptable.Error()) {
				t.Fatalf("first attempt failed=%v %q; want the event failed for its text", failed, r.fs.failReasons)
			}
			// The retry the failure asks for: the text still fails, nobody is phoned twice (D19).
			calls, failed = r.attempt(c.ev, 2)
			if calls != 0 || !failed {
				t.Fatalf("retry: %d provider calls, failed=%v; want none and the event failed again", calls, failed)
			}
			for _, ch := range []string{rigSMS, rigVoice} {
				for _, row := range r.ps.rowsFor(ch) {
					if row.reason != domain.PhoneReasonSent {
						t.Errorf("%s row %s; want sent", ch, row.reason)
					}
				}
			}
			if c.ev.Topic == domain.TopicServiceAlert && len(r.fs.alertDelivered) == 0 {
				t.Error("an onset the phone delivered was not credited: somebody was told")
			}
		})
	}
}

// slackOnlyFailingSends is a Slack-only project whose Slack answers 500: the read succeeds, the send
// fails.
type slackOnlyFailingSends struct{ p14SlackOnly }

func TestNoPhoneProjectWhoseTextFailsMakesNoPhoneCallAndFailsAsBefore(t *testing.T) {
	mustJSON := func(v any) []byte { return payload(t, v) }
	events := []domain.OutboxEvent{
		{ID: "e-down", Topic: domain.TopicMonitorTransition,
			Payload: mustJSON(domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusUp, Cur: domain.StatusDown, Seq: 7})},
		{ID: "e-burn", Topic: domain.TopicSLOBurnAlert,
			Payload: mustJSON(domain.SLOBurnAlert{MonitorID: "m1", Window: "30d", WindowSeconds: 3600, BurnRate: 14.4, Firing: true})},
		{ID: "e-onset", Topic: domain.TopicServiceAlert, Payload: mustJSON(domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1",
			ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown, Seq: 1,
			Recipients: []string{"slack1"}})},
		{ID: "e-step", Topic: domain.TopicEscalationStep, Payload: mustJSON(domain.EscalationStepAlert{IncidentID: "inc1",
			MonitorID: "m1", SubjectName: "payments-api", Step: 1, ChannelIDs: []string{"slack1"}})},
	}
	// textError is the event's failure before phone existed: the text method's own error.
	textError := func(t *testing.T, d *notify.Dispatcher, e domain.OutboxEvent) string {
		m := domain.Monitor{ID: "m1", ProjectID: "p1", Name: "payments-api"}
		var err error
		switch e.Topic {
		case domain.TopicMonitorTransition:
			err = d.Deliver(context.Background(), m, false, nil)
		case domain.TopicSLOBurnAlert:
			err = d.DeliverText(context.Background(), m, "x")
		case domain.TopicServiceAlert:
			_, err = d.DeliverChannelsReporting(context.Background(), []string{"slack1"}, "x")
		default:
			err = d.DeliverChannels(context.Background(), []string{"slack1"}, "x")
		}
		if err == nil {
			t.Fatal("the text method did not fail")
		}
		return err.Error()
	}
	for _, mode := range []string{"read fails, fallback answers nothing", "read fails, fallback fails too", "read succeeds, send fails"} {
		for _, e := range events {
			t.Run(mode+": "+e.Topic, func(t *testing.T) {
				var posts atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					posts.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer srv.Close()
				var texts notify.Store = brokenTextReads{}
				if mode == "read succeeds, send fails" {
					texts = slackOnlyFailingSends{p14SlackOnly{ch: domain.NotificationChannel{ID: "slack1",
						Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": srv.URL}}}}
				}
				reads := &p14FailingReads{calls: map[string]int{}, fakeStore: &fakeStore{
					monitors: map[string]domain.Monitor{"m1": {ID: "m1", ProjectID: "p1", Name: "payments-api",
						Status: domain.StatusDown, StateSequence: 7, AutoIncident: true}},
				}}
				if mode == "read fails, fallback fails too" {
					reads.phoneIDsErr = errors.New("injected: the fallback read fails")
				}
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				d := notify.New(texts, nil).WithPhone(p14FailingRecord{reads: reads}, p14Telephony{reads: reads}, nil, logger)
				want := textError(t, notify.New(texts, nil), e)
				posts.Store(0)
				w := New(reads, nil, d, &fakeMetrics{}, logger)
				e.Attempts, e.CreatedAt = 1, time.Now()
				reads.pending = []domain.OutboxEvent{e}
				w.drain(context.Background())

				if len(reads.failReasons) != 1 || reads.failReasons[0] != want {
					t.Errorf("failed %q; want exactly the text's error %q", reads.failReasons, want)
				}
				if len(reads.calls) != 0 {
					t.Errorf("phone-half calls for a project without phone channels: %v; want none", reads.calls)
				}
				fallback := reads.reads["MonitorPhoneChannelIDs"] + reads.reads["PhoneChannelIDs"]
				switch {
				case mode == "read succeeds, send fails" && fallback != 0:
					t.Errorf("a failed SEND made %d phone-id reads; the text's read already answered", fallback)
				case mode != "read succeeds, send fails" && fallback != 1:
					t.Errorf("a failed text read made %d phone-id reads; want the one fallback", fallback)
				}
				for _, m := range []string{"GetProject", "GetService", "GetIncident", "IncidentRef", "MonitorPhoneLink", "ServicePhoneLink"} {
					if n := reads.reads[m]; n != 0 {
						t.Errorf("%s read %d times for a project without phone channels", m, n)
					}
				}
			})
		}
	}
}

// FR-041a owner answer 3 (review C m-4): a service alert whose text read FAILED has not learnt which
// of its recipients are there — the read that would have told it did not answer — so none of them is
// counted missing, and nothing is recorded as undeliverable, also when the phone-id fallback read
// answered. The phone project is phoned and the onset credited as before; the event still fails for
// its text. A project without phone channels fails exactly as before, and its failed read is no
// longer reported as a missing recipient either.
func TestAFailedTextReadIsNotAMissingRecipient(t *testing.T) {
	onset := func(recipients ...string) domain.OutboxEvent {
		return domain.OutboxEvent{ID: "e-onset", Topic: domain.TopicServiceAlert, CreatedAt: time.Now(),
			Payload: payload(t, domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceName: "Checkout",
				ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
				Seq: 1, Recipients: recipients})}
	}
	check := func(t *testing.T, m *fakeMetrics) {
		t.Helper()
		if m.recipientMissing != 0 || len(m.undeliverable) != 0 {
			t.Fatalf("recipient_missing %d, undeliverable %v; want neither recorded for a failed text read",
				m.recipientMissing, m.undeliverable)
		}
	}
	t.Run("phone project: the fallback read finds the SMS channel", func(t *testing.T) {
		r := newBrokenTextRig(t, brokenTextReads{})
		calls, failed := r.attempt(onset(rigSlack, rigSMS), 1)
		if calls != 1 || !failed {
			t.Fatalf("%d provider calls, failed=%v; want the SMS sent and the event failed for its text", calls, failed)
		}
		if len(r.fs.alertDelivered) == 0 {
			t.Fatal("an onset the phone delivered was not credited")
		}
		check(t, r.metrics)
	})
	t.Run("no phone channels: the fallback read answers nothing", func(t *testing.T) {
		reads := &p14FailingReads{calls: map[string]int{}, fakeStore: &fakeStore{}}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		d := notify.New(brokenTextReads{}, nil).WithPhone(p14FailingRecord{reads: reads}, p14Telephony{reads: reads}, nil, logger)
		m := &fakeMetrics{}
		w := New(reads, nil, d, m, logger)
		e := onset("slack1")
		e.Attempts = 1
		reads.pending = []domain.OutboxEvent{e}
		w.drain(context.Background())
		if len(reads.failReasons) != 1 || !strings.Contains(reads.failReasons[0], errUndecryptable.Error()) {
			t.Fatalf("failed %q; want the text's error", reads.failReasons)
		}
		if n := reads.reads["PhoneChannelIDs"]; n != 1 {
			t.Fatalf("%d phone-id reads; want the one fallback, answering nothing", n)
		}
		if len(reads.calls) != 0 {
			t.Fatalf("phone-half calls for a project without phone channels: %v", reads.calls)
		}
		check(t, m)
	})
}

// The fallback is only for a read that did not complete: a phone project whose Slack SEND fails was
// told its phone channels by the read, and is phoned from them without a second read.
func TestAFailedTextSendNeedsNoFallbackRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := newBrokenTextRig(t, rigChannels{channels: []domain.NotificationChannel{
		{ID: rigSlack, Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": srv.URL}},
		{ID: rigSMS, Type: domain.ChannelSMS, Enabled: true},
		{ID: rigVoice, Type: domain.ChannelVoice, Enabled: true},
	}})
	ev := domain.OutboxEvent{ID: "e-down", Topic: domain.TopicMonitorTransition, CreatedAt: time.Now(),
		Payload: payload(t, domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusUp, Cur: domain.StatusDown, Seq: 7})}
	if calls, failed := r.attempt(ev, 1); calls != 2 || !failed {
		t.Fatalf("%d provider calls, failed=%v; want the SMS and the call, and the event failed for its text", calls, failed)
	}
	if n := r.fs.reads["MonitorPhoneChannelIDs"] + r.fs.reads["PhoneChannelIDs"]; n != 0 {
		t.Fatalf("%d phone-id reads; the text half's read answered", n)
	}
}
