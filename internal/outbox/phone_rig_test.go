package outbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// phoneRig runs the outbox worker over the REAL notify.Dispatcher, wired for phone with an in-memory
// delivery record and the fake provider, beside a Slack channel served by httptest: what a phone
// channel and a text channel each received is then counted at the edge, not inferred from a fake
// deliverer (FR-041a P2, P15, §11 "Outbox — coverage").
type phoneRig struct {
	t        *testing.T
	fs       *fakeStore
	ps       *memPhoneStore
	provider *telephony.FakeProvider
	slack    atomic.Int32
	slackURL string
	// slackDelay holds every Slack answer this long (or until the request is abandoned): a slow text
	// channel.
	slackDelay atomic.Int64
	metrics    *fakeMetrics
	w          *Worker
}

const (
	rigSlack = "slack1"
	rigSMS   = "sms1"
	rigVoice = "voice1"
)

func newPhoneRig(t *testing.T) *phoneRig {
	t.Helper()
	r := &phoneRig{t: t, ps: newMemPhoneStore(), provider: telephony.NewFakeProvider(), metrics: &fakeMetrics{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.slack.Add(1)
		if d := time.Duration(r.slackDelay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-req.Context().Done():
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	r.slackURL = srv.URL
	r.ps.targets[rigSMS] = domain.PhoneTarget{ChannelID: rigSMS, ProjectID: "p1", Kind: domain.ChannelSMS,
		Recipient: domain.PhoneRecipientNumber, Number: "+905551236869"}
	r.ps.targets[rigVoice] = domain.PhoneTarget{ChannelID: rigVoice, ProjectID: "p1", Kind: domain.ChannelVoice,
		Recipient: domain.PhoneRecipientUser, UserID: "u1", Number: "+447700900199"}
	r.fs = &fakeStore{
		monitors: map[string]domain.Monitor{
			"m1": {ID: "m1", ProjectID: "p1", Name: "payments-api", Status: domain.StatusDown, StateSequence: 7},
		},
		phoneFake: phoneFake{
			projects: map[string]domain.Project{"p1": {ID: "p1", Name: "prod"}},
			services: map[string]domain.Service{"s1": {ID: "s1", ProjectID: "p1", Slug: "checkout", Name: "Checkout"}},
			incidents: map[string]domain.Incident{
				"inc1": {ID: "inc1", ProjectID: "p1", MonitorID: "m1", Source: domain.SourceAuto},
				"inc2": {ID: "inc2", ProjectID: "p1", ServiceID: "s1", Source: domain.SourceAuto},
			},
			refs: map[string]string{"inc1": "X7K2P9QM", "inc2": "7MQ4H2ZD"},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	channels := rigChannels{channels: []domain.NotificationChannel{
		{ID: rigSlack, Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": r.slackURL}},
		{ID: rigSMS, Type: domain.ChannelSMS, Enabled: true},
		{ID: rigVoice, Type: domain.ChannelVoice, Enabled: true},
	}}
	tel := &rigTelephony{provider: r.provider}
	tel.ts = domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio}
	tel.ts.Normalize()
	d := notify.New(channels, nil).WithPhone(r.ps, tel, nil, logger)
	r.w = New(r.fs, nil, d, r.metrics, logger).
		WithPhoneLinks(func() string { return "https://cerbix.example" }, func() string { return "" })
	return r
}

// run delivers one event and reports what the edge received: Slack posts, SMS and calls.
func (r *phoneRig) run(e domain.OutboxEvent) (slack, sms, calls int) {
	r.t.Helper()
	beforeSlack, beforeCalls := r.slack.Load(), len(r.provider.Calls())
	if e.Attempts == 0 {
		e.Attempts = 1
	}
	r.fs.pending = append(r.fs.pending, e)
	r.w.drain(context.Background())
	for _, c := range r.provider.Calls()[beforeCalls:] {
		if c.Kind == "sms" {
			sms++
		} else {
			calls++
		}
	}
	return int(r.slack.Load() - beforeSlack), sms, calls
}

// rigChannels is notify.Store over the rig's channels: a monitor and a project hold all of them (the
// dispatcher's text paths drop the phone ones), and a snapshot resolves the ids that exist.
type rigChannels struct{ channels []domain.NotificationChannel }

func (c rigChannels) ListEnabledChannelsForMonitor(context.Context, string) ([]domain.NotificationChannel, error) {
	return c.channels, nil
}
func (c rigChannels) ListEnabledChannelsByProject(context.Context, string) ([]domain.NotificationChannel, error) {
	return c.channels, nil
}
func (c rigChannels) EnabledChannelsByIDs(_ context.Context, ids []string) ([]domain.NotificationChannel, error) {
	var out []domain.NotificationChannel
	for _, id := range ids {
		for _, ch := range c.channels {
			if ch.ID == id {
				out = append(out, ch)
			}
		}
	}
	return out, nil
}

// rigTelephony is the telephony in force for the rig.
type rigTelephony struct {
	ts       domain.TelephonySettings
	provider telephony.Provider
}

func (r *rigTelephony) Settings() domain.TelephonySettings    { return r.ts }
func (r *rigTelephony) Provider() (telephony.Provider, error) { return r.provider, nil }

// memPhoneStore is notify.PhoneStore in memory with D33's shape: one row per (event, channel); a
// retry meets an existing row — `sending` is in flight, `provider_unreachable` is re-armed, anything
// else is final — and scripted guards decide a fresh reservation (storm_limited, a stale row closed as
// outcome_unknown).
type memPhoneStore struct {
	mu        sync.Mutex
	targets   map[string]domain.PhoneTarget
	rows      map[string]*memRow // "event|channel"
	stormFor  map[string]bool    // channel → the storm guard refuses it
	staleFor  map[string]bool    // channel → a `sending` row of a crashed attempt, found stale
	reserves  int
	nextID    int
	byID      map[string]*memRow
	reasonsOf map[string][]string // channel → every reason a row of it ended with, in order
	// targetsErr fails PhoneTargets: the delivery record cannot be read, so nothing is counted.
	targetsErr error
}

type memRow struct {
	channel, reason string
	in              domain.PhoneReserveInput
}

func newMemPhoneStore() *memPhoneStore {
	return &memPhoneStore{targets: map[string]domain.PhoneTarget{}, rows: map[string]*memRow{},
		stormFor: map[string]bool{}, staleFor: map[string]bool{}, byID: map[string]*memRow{},
		reasonsOf: map[string][]string{}}
}

func (s *memPhoneStore) PhoneTargets(_ context.Context, ids []string) ([]domain.PhoneTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.targetsErr != nil {
		return nil, s.targetsErr
	}
	var out []domain.PhoneTarget
	for _, id := range ids {
		if t, ok := s.targets[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *memPhoneStore) ReservePhoneDelivery(_ context.Context, in domain.PhoneReserveInput) (domain.PhoneReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserves++
	key := in.OutboxEventID + "|" + in.ChannelID
	if row, ok := s.rows[key]; ok {
		switch row.reason {
		case domain.PhoneReasonSending:
			return domain.PhoneReservation{}, domain.ErrPhoneDeliveryInFlight
		case domain.PhoneReasonProviderUnreachable:
			row.reason = domain.PhoneReasonSending
			return domain.PhoneReservation{ID: s.idOf(row), Status: domain.PhoneReservationProceed, Reason: row.reason}, nil
		default:
			return domain.PhoneReservation{ID: s.idOf(row), Status: domain.PhoneReservationExisting, Reason: row.reason}, nil
		}
	}
	row := &memRow{channel: in.ChannelID, in: in, reason: domain.PhoneReasonSending}
	s.rows[key] = row
	s.nextID++
	s.byID[fmt.Sprintf("d%d", s.nextID)] = row
	switch {
	case s.stormFor[in.ChannelID]:
		s.end(row, domain.PhoneReasonStormLimited)
		return domain.PhoneReservation{ID: s.idOf(row), Status: domain.PhoneReservationStormLimited, Reason: row.reason}, nil
	case s.staleFor[in.ChannelID]:
		s.end(row, domain.PhoneReasonOutcomeUnknown)
		return domain.PhoneReservation{ID: s.idOf(row), Status: domain.PhoneReservationClosedStale, Reason: row.reason}, nil
	}
	return domain.PhoneReservation{ID: s.idOf(row), Status: domain.PhoneReservationProceed, Reason: row.reason}, nil
}

func (s *memPhoneStore) RecordUnsentPhoneDelivery(_ context.Context, in domain.PhoneReserveInput, reason string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := in.OutboxEventID + "|" + in.ChannelID
	if row, ok := s.rows[key]; ok {
		return false, row.reason, nil
	}
	row := &memRow{channel: in.ChannelID, in: in}
	s.rows[key] = row
	s.end(row, reason)
	return true, reason, nil
}

func (s *memPhoneStore) FinishPhoneDelivery(_ context.Context, id string, out domain.PhoneOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.byID[id]
	if !ok || row.reason != domain.PhoneReasonSending {
		return domain.ErrPhoneDeliveryClosed
	}
	s.end(row, out.Reason)
	return nil
}

// end settles a row; called with s.mu held.
func (s *memPhoneStore) end(row *memRow, reason string) {
	row.reason = reason
	s.reasonsOf[row.channel] = append(s.reasonsOf[row.channel], reason)
}

// idOf is the id a row was given; called with s.mu held.
func (s *memPhoneStore) idOf(row *memRow) string {
	for id, r := range s.byID {
		if r == row {
			return id
		}
	}
	return ""
}

// rowsFor returns the rows of a channel: their reasons and the inputs that wrote them.
func (s *memPhoneStore) rowsFor(channel string) []memRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []memRow
	for _, r := range s.rows {
		if r.channel == channel {
			out = append(out, *r)
		}
	}
	return out
}

// Final review M-5, end to end over the real renderer: a mail.public_base_url that cannot carry a
// link (it is not validated at save) used to make every SMS a render error, so the event retried until
// it dead-lettered and the page never went. The link is dropped instead, as when no base exists.
func TestUnrenderableMailBaseURLCostsTheLinkNotThePage(t *testing.T) {
	r := newPhoneRig(t)
	r.w.WithPhoneLinks(func() string { return "" }, func() string { return "https://status.example/a b" })
	r.fs.serviceLinks = map[string]store.PhoneLink{"s1": {IncidentID: "inc2", Ref: "7MQ4H2ZD"}}
	ev := domain.OutboxEvent{ID: "e1", Topic: domain.TopicServiceAlert, Payload: payload(t, domain.ServiceAlert{
		ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalHealth,
		Firing: true, State: domain.ServiceAlertDown, Seq: 1, Recipients: []string{rigSMS}})}
	_, sms, _ := r.run(ev)
	if sms != 1 || len(r.fs.failed) != 0 {
		t.Fatalf("sms %d, failed %v (%v); want the page sent and the event delivered", sms, r.fs.failed, r.fs.failReasons)
	}
	body := r.provider.Calls()[0].Body
	if !strings.Contains(body, "incident 7MQ4H2ZD") || strings.Contains(body, "status.example") {
		t.Fatalf("sms %q; want the reference without the link", body)
	}
	r.ps.mu.Lock()
	defer r.ps.mu.Unlock()
	for _, row := range r.ps.rows {
		if !slices.Contains(row.in.Notes, domain.PhoneNoteNoPublicURL) {
			t.Fatalf("row notes %v; want %s", row.in.Notes, domain.PhoneNoteNoPublicURL)
		}
	}
}

// FR-041a review P2-1: the phone half runs BESIDE the text half, inside the same lease budget, not
// after it. A Slack channel that holds the whole budget used to leave the phone half nothing: its
// calls were refused as provider_unreachable before a request, and the page waited for a retry — or
// several, behind several slow channels. Now the SMS and the call go out on the first attempt.
func TestASlowTextChannelDoesNotDelayThePage(t *testing.T) {
	r := newPhoneRig(t)
	r.slackDelay.Store(int64(2 * time.Second))
	now := time.Now()
	ev := domain.OutboxEvent{ID: "e-down", Topic: domain.TopicMonitorTransition, Attempts: 1, CreatedAt: now,
		ClaimedAt: now, LeaseUntil: now.Add(leaseHeadroom + time.Second), // a budget of one second
		Payload: payload(t, domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusUp, Cur: domain.StatusDown, Seq: 7})}
	slack, sms, calls := r.run(ev)
	if slack != 1 {
		t.Fatalf("slack posts %d; want the one slow post", slack)
	}
	if sms != 1 || calls != 1 {
		t.Fatalf("first attempt: %d sms, %d calls; want the SMS and the call placed beside the slow text", sms, calls)
	}
	for _, ch := range []string{rigSMS, rigVoice} {
		if rows := r.ps.rowsFor(ch); len(rows) != 1 || rows[0].reason != domain.PhoneReasonSent {
			t.Fatalf("%s rows %+v; want one sent", ch, rows)
		}
	}
	// The slow text failed the event (its retry is the text's, the residue §14 leaves to judge).
	if len(r.fs.failed) != 1 {
		t.Fatalf("failed %v; want the event retried for its text", r.fs.failed)
	}
}
