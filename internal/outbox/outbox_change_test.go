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
	fs = &fakeStore{pending: []domain.OutboxEvent{{ID: "e7", Topic: domain.TopicIncidentEvent, Payload: changeOpened(t, service), Attempts: 1}}}
	newWorker(fs, &fakeWebhook{err: errors.New("boom")}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs.changeLinked) != 0 || len(fs.failed) != 1 {
		t.Fatalf("a failed webhook must retry before any correlation: linked=%v failed=%v", fs.changeLinked, fs.failed)
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
