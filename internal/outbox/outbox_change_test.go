package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// fakeChangeMetrics records FR-025 D15's two correlation families.
type fakeChangeMetrics struct {
	correlations map[string]int
	errors       int
}

func (m *fakeChangeMetrics) RecordChangeCorrelations(role string, n int) {
	if m.correlations == nil {
		m.correlations = map[string]int{}
	}
	m.correlations[role] += n
}
func (m *fakeChangeMetrics) RecordChangeCorrelationError() { m.errors++ }

func changeOpened(t *testing.T, inc domain.Incident) []byte {
	t.Helper()
	b, err := json.Marshal(domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: inc})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FR-025 D7/D8 at the worker (invariants 7, 8): the correlation runs at a SERVICE auto-incident's
// `opened` delivery with the configured window and note cap, counts its links per role, and is
// FAIL-OPEN — a planted store error is counted and the delivery still completes. It does not run
// for a monitor incident, a manual incident, a resolved event, or an event whose webhook failed.
func TestChangeCorrelationRunsAtServiceIncidentOpenAndFailsOpen(t *testing.T) {
	service := domain.Incident{ID: "inc3", ProjectID: "p1", ServiceID: "svc1", Source: domain.SourceAuto}

	// Defaults when nothing is wired: the §5a numbers, so an unwired worker is not a silent one.
	fs := &fakeStore{pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}},
		changeLinkRes: store.ChangeCorrelation{Links: []store.ChangeCorrelationLink{
			{ChangeID: "c1", Role: domain.ChangeLinkRoleOwnService}, {ChangeID: "c2", Role: domain.ChangeLinkRoleOwnService},
			{ChangeID: "c3", Role: domain.ChangeLinkRoleUpstream}}, NoteAdded: true}}
	m := &fakeChangeMetrics{}
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).WithChangeCorrelation(0, 0, m).drain(context.Background())
	if len(fs.changeLinked) != 1 || fs.changeLinked[0] != "inc3|1h0m0s|5" {
		t.Fatalf("correlation calls = %v, want one with the default 60m window and note cap 5", fs.changeLinked)
	}
	if m.correlations[domain.ChangeLinkRoleOwnService] != 2 || m.correlations[domain.ChangeLinkRoleUpstream] != 1 || m.errors != 0 {
		t.Fatalf("metrics = %+v, want own_service=2 upstream=1 errors=0", m)
	}
	if len(fs.delivered) != 1 {
		t.Fatalf("delivered = %v, want the event delivered", fs.delivered)
	}

	// The operator's values reach the store call.
	fs = &fakeStore{pending: []domain.OutboxEvent{{ID: "e2", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}}}
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).WithChangeCorrelation(30*time.Minute, 3, nil).drain(context.Background())
	if len(fs.changeLinked) != 1 || fs.changeLinked[0] != "inc3|30m0s|3" {
		t.Fatalf("correlation calls = %v, want the configured 30m window and note cap 3", fs.changeLinked)
	}

	// Fail-open: a planted store error is counted and logged; the event is delivered, not failed.
	fs = &fakeStore{pending: []domain.OutboxEvent{{ID: "e3", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}},
		changeLinkErr: errors.New("planted")}
	m = &fakeChangeMetrics{}
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).WithChangeCorrelation(0, 0, m).drain(context.Background())
	if len(fs.delivered) != 1 || len(fs.failed) != 0 {
		t.Fatalf("a correlation error must not fail the delivery: delivered=%v failed=%v", fs.delivered, fs.failed)
	}
	if m.errors != 1 || len(m.correlations) != 0 {
		t.Fatalf("metrics after the planted error = %+v, want errors=1 and no links", m)
	}

	// Not for a MONITOR auto-incident (that path keeps its ⚡ context), a manual incident, or a
	// resolved event — and never before the webhook succeeded.
	monitor := domain.Incident{ID: "inc1", ProjectID: "p1", MonitorID: "m1", Source: domain.SourceAuto}
	manual := domain.Incident{ID: "inc2", ProjectID: "p1", ServiceID: "svc1", Source: domain.SourceManual}
	resolved, _ := json.Marshal(domain.IncidentEvent{Type: domain.EventIncidentResolved, Incident: service})
	fs = &fakeStore{pending: []domain.OutboxEvent{
		{ID: "e4", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, monitor), Attempts: 1},
		{ID: "e5", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, manual), Attempts: 1},
		{ID: "e6", Topic: domain.TopicIncidentEvent, Payload: resolved, Attempts: 1},
	}}
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs.changeLinked) != 0 {
		t.Fatalf("correlation ran for a monitor/manual/resolved event: %v", fs.changeLinked)
	}
	if len(fs.delivered) != 3 {
		t.Fatalf("delivered = %v, want all three", fs.delivered)
	}
	// A failed webhook fails the EVENT and nothing else: the correlation is attempted all the
	// same. This assertion used to demand the opposite — that a delivery failure skip the
	// correlation — which review [64] identified as the defect it was: a subscriber's endpoint
	// could keep a service incident from ever getting its links and its note. The fail-open of
	// D7/NFR-020 runs in both directions, and `TestChangeCorrelationRunsThoughTheWebhookDeliveryFails`
	// pins the whole of it.
	fs = &fakeStore{pending: []domain.OutboxEvent{{ID: "e7", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}}}
	newWorker(fs, &fakeWebhook{err: errors.New("boom")}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs.changeLinked) != 1 || len(fs.failed) != 1 {
		t.Fatalf("a failed webhook must still attempt the correlation and leave the event retryable: linked=%v failed=%v", fs.changeLinked, fs.failed)
	}
}

// A skipped attempt (the note already present on redelivery) and an attempt with no links count
// nothing and log nothing.
func TestChangeCorrelationSkippedAndEmptyResultsCountNothing(t *testing.T) {
	service := domain.Incident{ID: "inc3", ProjectID: "p1", ServiceID: "svc1", Source: domain.SourceAuto}
	for _, res := range []store.ChangeCorrelation{{Skipped: true}, {}} {
		fs := &fakeStore{pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}},
			changeLinkRes: res}
		m := &fakeChangeMetrics{}
		newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).WithChangeCorrelation(0, 0, m).drain(context.Background())
		if len(fs.changeLinked) != 1 || len(m.correlations) != 0 || m.errors != 0 || len(fs.delivered) != 1 {
			t.Fatalf("result %+v: linked=%v metrics=%+v delivered=%v", res, fs.changeLinked, m, fs.delivered)
		}
	}
}

// Review [64]: the correlation is not conditional on the notification. A webhook that fails —
// persistently, all the way to the dead letter — must still leave the service incident with its
// links and its note, must count its own failures as its own, and must not double-write when the
// delivery is retried.
func TestChangeCorrelationRunsThoughTheWebhookDeliveryFails(t *testing.T) {
	service := domain.Incident{ID: "inc9", ProjectID: "p1", ServiceID: "svc9", Source: domain.SourceAuto}
	linked := store.ChangeCorrelation{Links: []store.ChangeCorrelationLink{
		{ChangeID: "c1", Role: domain.ChangeLinkRoleOwnService}}, NoteAdded: true}

	// The delivery fails; the correlation still ran exactly once, and the event was NOT delivered.
	fs := &fakeStore{
		pending:       []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}},
		changeLinkRes: linked,
	}
	m := &fakeChangeMetrics{}
	newWorker(fs, &fakeWebhook{err: errors.New("subscriber endpoint down")}, &fakeNotify{}, &fakeMetrics{}).
		WithChangeCorrelation(0, 0, m).drain(context.Background())
	if len(fs.changeLinked) != 1 {
		t.Fatalf("correlation calls = %v, want exactly one although the delivery failed", fs.changeLinked)
	}
	if m.correlations[domain.ChangeLinkRoleOwnService] != 1 || m.errors != 0 {
		t.Fatalf("metrics = %+v, want the link counted once and no correlation error", m)
	}
	if len(fs.delivered) != 0 {
		t.Fatalf("delivered = %v, want the failed event left undelivered (retryable)", fs.delivered)
	}

	// The retry: the store is the idempotence (the note is already there, so it answers Skipped).
	// The attempt happens again — it must — but nothing is written and nothing is counted twice.
	fs.pending = []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 2}}
	fs.changeLinkRes = store.ChangeCorrelation{Skipped: true}
	newWorker(fs, &fakeWebhook{err: errors.New("still down")}, &fakeNotify{}, &fakeMetrics{}).
		WithChangeCorrelation(0, 0, m).drain(context.Background())
	if len(fs.changeLinked) != 2 {
		t.Fatalf("correlation calls = %v, want the retry to attempt again", fs.changeLinked)
	}
	if m.correlations[domain.ChangeLinkRoleOwnService] != 1 || m.errors != 0 {
		t.Fatalf("metrics after the retry = %+v, want the link still counted ONCE", m)
	}

	// A correlation failure is still the correlation's own: counted, and the delivery error is
	// what decides the event's fate.
	fs = &fakeStore{
		pending:       []domain.OutboxEvent{{ID: "e2", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}},
		changeLinkErr: errors.New("correlation exploded"),
	}
	m2 := &fakeChangeMetrics{}
	newWorker(fs, &fakeWebhook{err: errors.New("also down")}, &fakeNotify{}, &fakeMetrics{}).
		WithChangeCorrelation(0, 0, m2).drain(context.Background())
	if m2.errors != 1 {
		t.Fatalf("correlation errors = %d, want the planted failure counted", m2.errors)
	}
	if len(fs.delivered) != 0 {
		t.Fatalf("delivered = %v, want the webhook failure to decide the event", fs.delivered)
	}
}
