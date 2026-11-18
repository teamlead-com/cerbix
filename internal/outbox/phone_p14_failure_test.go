package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// P14 under failure (Task 12 review, I-1): a project WITHOUT phone channels, delivered by a worker
// whose dispatcher IS wired for phone, while every read the phone half could make fails — the delivery
// record's PhoneTargets (and so PhoneRoutes), the names (GetProject, GetService, GetIncident) and the
// links (MonitorPhoneLink, ServicePhoneLink, IncidentRef: D32's wait included). Every phoned path and
// the unphoned close must post its Slack text exactly once, be delivered, and make none of those
// reads: the same calls and the same ways to fail as before phone existed.

// p14FailingReads is the outbox store with every phone-half read counted. With onlyRecord set the
// outbox's own reads answer (a project, a service, an incident and its reference exist) and only the
// delivery record fails, so a read the phone half makes AFTER the text has been sent is what fails.
type p14FailingReads struct {
	*fakeStore
	mu         sync.Mutex
	calls      map[string]int
	onlyRecord bool
}

var errP14Read = errors.New("injected: the phone half must not read this for a project without phone channels")

func (s *p14FailingReads) fail(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[method]++
	return errP14Read
}

// read counts an outbox-side read; it fails unless only the record is to fail.
func (s *p14FailingReads) read(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[method]++
	if s.onlyRecord {
		return nil
	}
	return errP14Read
}

func (s *p14FailingReads) MonitorPhoneLink(context.Context, string, time.Time) (store.PhoneLink, error) {
	return store.PhoneLink{}, s.read("MonitorPhoneLink")
}
func (s *p14FailingReads) ServicePhoneLink(context.Context, string, time.Time) (store.PhoneLink, error) {
	return store.PhoneLink{}, s.read("ServicePhoneLink")
}

// ListMonitorPhoneChannelIDs is the read e594636's monitor path made before its text half; the store
// no longer has it. It stays so this test runs, and fails, against that commit.
func (s *p14FailingReads) ListMonitorPhoneChannelIDs(context.Context, string) ([]string, error) {
	if err := s.read("ListMonitorPhoneChannelIDs"); err != nil {
		return nil, err
	}
	return []string{"slack1"}, nil // a read that answers hands the phone half the only channel there is
}
func (s *p14FailingReads) GetProject(context.Context, string) (domain.Project, error) {
	return domain.Project{ID: "p1", Name: "prod"}, s.read("GetProject")
}
func (s *p14FailingReads) GetService(context.Context, string, string) (domain.Service, error) {
	return domain.Service{ID: "s1", ProjectID: "p1", Slug: "checkout"}, s.read("GetService")
}
func (s *p14FailingReads) GetIncident(_ context.Context, id string) (domain.Incident, error) {
	return domain.Incident{ID: id, ProjectID: "p1"}, s.read("GetIncident")
}
func (s *p14FailingReads) IncidentRef(context.Context, string) (string, error) {
	return "X7K2P9QM", s.read("IncidentRef")
}

// p14FailingRecord is notify's phone delivery record with every method failing and counted.
type p14FailingRecord struct{ reads *p14FailingReads }

func (r p14FailingRecord) PhoneTargets(context.Context, []string) ([]domain.PhoneTarget, error) {
	return nil, r.reads.fail("PhoneTargets")
}
func (r p14FailingRecord) ReservePhoneDelivery(context.Context, domain.PhoneReserveInput) (domain.PhoneReservation, error) {
	return domain.PhoneReservation{}, r.reads.fail("ReservePhoneDelivery")
}
func (r p14FailingRecord) RecordUnsentPhoneDelivery(context.Context, domain.PhoneReserveInput, string) (bool, string, error) {
	return false, "", r.reads.fail("RecordUnsentPhoneDelivery")
}
func (r p14FailingRecord) FinishPhoneDelivery(context.Context, string, domain.PhoneOutcome) error {
	return r.reads.fail("FinishPhoneDelivery")
}

// p14Telephony is telephony switched on, with a provider that must never be built.
type p14Telephony struct{ reads *p14FailingReads }

func (t p14Telephony) Settings() domain.TelephonySettings {
	return domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio}
}
func (t p14Telephony) Provider() (telephony.Provider, error) {
	return nil, t.reads.fail("Provider")
}

// p14SlackOnly is the project's channels: one Slack channel, nothing else.
type p14SlackOnly struct{ ch domain.NotificationChannel }

func (s p14SlackOnly) ListEnabledChannelsForMonitor(context.Context, string) ([]domain.NotificationChannel, error) {
	return []domain.NotificationChannel{s.ch}, nil
}
func (s p14SlackOnly) ListEnabledChannelsByProject(context.Context, string) ([]domain.NotificationChannel, error) {
	return []domain.NotificationChannel{s.ch}, nil
}
func (s p14SlackOnly) EnabledChannelsByIDs(_ context.Context, ids []string) ([]domain.NotificationChannel, error) {
	for _, id := range ids {
		if id == s.ch.ID {
			return []domain.NotificationChannel{s.ch}, nil
		}
	}
	return nil, nil
}

func TestNoPhoneProjectGainsNoPhoneReadsOrFailures(t *testing.T) {
	mustJSON := func(v any) []byte { return payload(t, v) }
	onset := domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceName: "Checkout", ServiceSlug: "checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown, Seq: 1, Recipients: []string{"slack1"}}
	recovered := onset
	recovered.Firing, recovered.State, recovered.CloseReason = false, "", domain.CloseRecovered
	burnClear := domain.ServiceAlert{ServiceID: "s1", ProjectID: "p1", ServiceSlug: "checkout", Signal: domain.ServiceSignalBurn,
		CloseReason: domain.CloseRecovered, Seq: 1, Recipients: []string{"slack1"}}
	cases := []struct {
		name, topic string
		payload     []byte
	}{
		{"monitor DOWN (auto_incident on)", domain.TopicMonitorTransition,
			mustJSON(domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusUp, Cur: domain.StatusDown, Seq: 7})},
		{"monitor recovery", domain.TopicMonitorTransition,
			mustJSON(domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusDown, Cur: domain.StatusUp, Seq: 7})},
		{"monitor burn firing", domain.TopicSLOBurnAlert,
			mustJSON(domain.SLOBurnAlert{MonitorID: "m1", Window: "30d", WindowSeconds: 3600, BurnRate: 14.4, Firing: true})},
		{"service health onset", domain.TopicServiceAlert, mustJSON(onset)},
		{"service recovered close", domain.TopicServiceAlert, mustJSON(recovered)},
		{"service burn CLEAR (not phoned: PhoneRoutes)", domain.TopicServiceAlert, mustJSON(burnClear)},
		{"monitor escalation step", domain.TopicEscalationStep, mustJSON(domain.EscalationStepAlert{
			IncidentID: "inc1", MonitorID: "m1", SubjectName: "payments-api", Step: 1, ChannelIDs: []string{"slack1"}})},
		{"service escalation step", domain.TopicEscalationStep, mustJSON(domain.EscalationStepAlert{
			IncidentID: "inc2", ServiceID: "s1", SubjectName: "Checkout", Step: 1, Repeat: true, ChannelIDs: []string{"slack1"}})},
	}
	for _, onlyRecord := range []bool{false, true} {
		for i, c := range cases {
			name := c.name + ": every phone read fails"
			if onlyRecord {
				name = c.name + ": only the delivery record fails, after the text"
			}
			t.Run(name, func(t *testing.T) { p14Case(t, i, c.topic, c.payload, onlyRecord) })
		}
	}
}

// p14Case delivers one event of a Slack-only project and asserts it went exactly as it did before
// phone existed: one Slack post, delivered on its first attempt, no phone-half call at all. A failure
// is retried once, so a text re-sent by that retry is counted.
func p14Case(t *testing.T, i int, topic string, raw []byte, onlyRecord bool) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	reads := &p14FailingReads{calls: map[string]int{}, onlyRecord: onlyRecord, fakeStore: &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", ProjectID: "p1", Name: "payments-api",
			Status: domain.StatusDown, StateSequence: 7, AutoIncident: true}},
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := notify.New(p14SlackOnly{ch: domain.NotificationChannel{ID: "slack1", Type: domain.ChannelSlack,
		Enabled: true, Config: map[string]string{"url": srv.URL}}}, nil).
		WithPhone(p14FailingRecord{reads: reads}, p14Telephony{reads: reads}, nil, logger)
	m := &fakeMetrics{}
	w := New(reads, nil, d, m, logger)

	ev := domain.OutboxEvent{ID: fmt.Sprintf("e%d", i), Topic: topic, Payload: raw,
		Attempts: 1, CreatedAt: time.Now()}
	reads.pending = []domain.OutboxEvent{ev}
	w.drain(context.Background())
	if len(reads.failed) != 0 {
		// The retry the failure asks for — and what it sends a second time.
		ev.Attempts = 2
		reads.pending = []domain.OutboxEvent{ev}
		w.drain(context.Background())
	}

	if n := posts.Load(); n != 1 {
		t.Errorf("Slack posts %d; want exactly one", n)
	}
	if len(reads.failed) != 0 {
		t.Errorf("failed %q; want the event delivered on its first attempt", reads.failReasons)
	}
	if len(reads.calls) != 0 {
		t.Errorf("phone-half calls for a project without phone channels: %v; want none", reads.calls)
	}
	if m.recipientMissing != 0 || len(m.undeliverable) != 0 {
		t.Errorf("recipient_missing %d, undeliverable %v; want neither", m.recipientMissing, m.undeliverable)
	}
}
