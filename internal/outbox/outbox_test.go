package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

func burnAlert(t *testing.T, monitorID string, firing bool) []byte {
	t.Helper()
	b, err := json.Marshal(domain.SLOBurnAlert{
		MonitorID: monitorID, Window: "30d", WindowSeconds: 3600,
		Objective: 99, BurnRate: 30, Threshold: 14.4, Firing: firing,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fakeStore struct {
	pending   []domain.OutboxEvent
	delivered []string
	failed    []string
	monitors  map[string]domain.Monitor
	claimErr  error

	// incident-context fakes
	ictx       domain.IncidentContext
	ictxErr    error
	appended   []string // rendered bodies passed to AppendIncidentContext
	appendSeen map[string]bool

	// dependency-suppression fakes
	downAncestors    []store.DownAncestor
	downAncErr       error
	suppressionNotes []string

	// maintenance-suppression fakes
	inMaintenance    map[string]bool
	inMaintenanceErr error
}

func (f *fakeStore) IncidentContext(_ context.Context, _ domain.Incident) (domain.IncidentContext, error) {
	return f.ictx, f.ictxErr
}

func (f *fakeStore) DownAncestors(_ context.Context, _ string) ([]store.DownAncestor, error) {
	return f.downAncestors, f.downAncErr
}

func (f *fakeStore) AppendSuppressionNote(_ context.Context, monitorID, root string) (bool, error) {
	f.suppressionNotes = append(f.suppressionNotes, monitorID+"←"+root)
	return true, nil
}

func (f *fakeStore) MonitorInMaintenance(_ context.Context, monitorID string) (bool, error) {
	if f.inMaintenanceErr != nil {
		return false, f.inMaintenanceErr
	}
	return f.inMaintenance[monitorID], nil
}

func (f *fakeStore) AppendIncidentContext(_ context.Context, incidentID, body string) (bool, error) {
	if f.appendSeen == nil {
		f.appendSeen = map[string]bool{}
	}
	if f.appendSeen[incidentID] {
		return false, nil // idempotent: already attached
	}
	f.appendSeen[incidentID] = true
	f.appended = append(f.appended, body)
	return true, nil
}

func (f *fakeStore) ClaimDueOutbox(_ context.Context, limit int) ([]domain.OutboxEvent, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if len(f.pending) == 0 {
		return nil, nil
	}
	n := len(f.pending)
	if n > limit {
		n = limit
	}
	batch := f.pending[:n]
	f.pending = f.pending[n:]
	return batch, nil
}
func (f *fakeStore) MarkOutboxDelivered(_ context.Context, id, _ string) (bool, error) {
	f.delivered = append(f.delivered, id)
	return true, nil
}
func (f *fakeStore) FailOutbox(_ context.Context, id, _, _ string, _ int) (bool, error) {
	f.failed = append(f.failed, id)
	return true, nil
}
func (f *fakeStore) GetMonitor(_ context.Context, id string) (domain.Monitor, error) {
	m, ok := f.monitors[id]
	if !ok {
		return domain.Monitor{}, store.ErrNotFound
	}
	return m, nil
}

type fakeMail struct {
	sent []domain.SubscriberConfirm
	err  error
}

func (f *fakeMail) Send(to, subject, body string) error {
	f.sent = append(f.sent, domain.SubscriberConfirm{To: to, Subject: subject, Body: body})
	return f.err
}

type fakeWebhook struct {
	got []domain.IncidentEvent
	err error
}

func (f *fakeWebhook) Deliver(_ context.Context, ev domain.IncidentEvent) error {
	f.got = append(f.got, ev)
	return f.err
}

type fakeNotify struct {
	monitor    domain.Monitor
	up         bool
	text       string
	projectID  string
	channelIDs []string
	called     int
	err        error
}

func (f *fakeNotify) Deliver(_ context.Context, m domain.Monitor, up bool) error {
	f.monitor, f.up, f.called = m, up, f.called+1
	return f.err
}

func (f *fakeNotify) DeliverText(_ context.Context, m domain.Monitor, text string) error {
	f.monitor, f.text, f.called = m, text, f.called+1
	return f.err
}

func (f *fakeNotify) DeliverProjectText(_ context.Context, projectID, text string) error {
	f.projectID, f.text, f.called = projectID, text, f.called+1
	return f.err
}

func (f *fakeNotify) DeliverChannels(_ context.Context, channelIDs []string, text string) error {
	f.channelIDs, f.text, f.called = channelIDs, text, f.called+1
	return f.err
}

type fakeMetrics struct{ delivered, dead int }

func (m *fakeMetrics) RecordOutboxDelivered() { m.delivered++ }
func (m *fakeMetrics) RecordOutboxDead()      { m.dead++ }

func incidentEvent(t *testing.T, typ, project string) []byte {
	t.Helper()
	b, err := json.Marshal(domain.IncidentEvent{Type: typ, Incident: domain.Incident{ID: "inc1", ProjectID: project}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func transition(t *testing.T, monitorID string, prev, cur domain.MonitorStatus) []byte {
	t.Helper()
	b, err := json.Marshal(domain.MonitorTransition{MonitorID: monitorID, Prev: prev, Cur: cur})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newWorker(fs *fakeStore, wh *fakeWebhook, nf *fakeNotify, m *fakeMetrics) *Worker {
	return New(fs, wh, nf, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestDeliversIncidentAndTransition(t *testing.T) {
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API", Status: domain.StatusDown}},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicIncidentEvent, Payload: incidentEvent(t, domain.EventIncidentOpened, "p1"), Attempts: 1},
			{ID: "e2", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: 1},
		},
	}
	wh, nf, m := &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}
	newWorker(fs, wh, nf, m).drain(context.Background())

	if len(fs.delivered) != 2 || len(fs.failed) != 0 {
		t.Fatalf("delivered=%v failed=%v, want both delivered", fs.delivered, fs.failed)
	}
	if len(wh.got) != 1 || wh.got[0].Type != domain.EventIncidentOpened {
		t.Fatalf("webhook deliveries = %+v", wh.got)
	}
	if nf.called != 1 || nf.up != false || nf.monitor.ID != "m1" {
		t.Fatalf("notify call = %+v up=%v", nf.monitor, nf.up)
	}
	if m.delivered != 2 {
		t.Fatalf("metrics delivered = %d, want 2", m.delivered)
	}
}

func TestSilenceSuppressesAlerts(t *testing.T) {
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API", Status: domain.StatusDown}},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: 1},
			{ID: "e2", Topic: domain.TopicSLOBurnAlert, Payload: burnAlert(t, "m1", true), Attempts: 1},
			{ID: "e3", Topic: domain.TopicIncidentEvent, Payload: incidentEvent(t, domain.EventIncidentOpened, "p1"), Attempts: 1},
		},
	}
	wh, nf := &fakeWebhook{}, &fakeNotify{}
	newWorker(fs, wh, nf, &fakeMetrics{}).WithSilence(func() bool { return true }).drain(context.Background())

	// Alert notifications are suppressed (no-op delivered); the incident webhook is not.
	if nf.called != 0 {
		t.Fatalf("notifier called %d times under silence, want 0", nf.called)
	}
	if len(wh.got) != 1 {
		t.Fatalf("incident webhook should still fire under silence, got %d", len(wh.got))
	}
	if len(fs.delivered) != 3 {
		t.Fatalf("all events should be marked delivered, got %v", fs.delivered)
	}
}

func TestDeliversBurnAlert(t *testing.T) {
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API"}},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicSLOBurnAlert, Payload: burnAlert(t, "m1", true), Attempts: 1},
		},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 || nf.monitor.ID != "m1" || !strings.Contains(nf.text, "burning") {
		t.Fatalf("burn alert delivery: called=%d monitor=%s text=%q", nf.called, nf.monitor.ID, nf.text)
	}
	if len(fs.delivered) != 1 {
		t.Fatalf("burn alert should be delivered, got %v", fs.delivered)
	}
}

func TestDeliversSLAReport(t *testing.T) {
	rep, err := json.Marshal(domain.SLAReport{
		ProjectID: "p1", ProjectName: "API",
		Windows: []domain.SLAReportWindow{{Window: "7d", UptimePercent: 99.9, Up: 999, Total: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicSLAReport, Payload: rep, Attempts: 1}}}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 || nf.projectID != "p1" || !strings.Contains(nf.text, "Weekly SLA report") {
		t.Fatalf("sla report delivery: called=%d project=%s text=%q", nf.called, nf.projectID, nf.text)
	}
	if len(fs.delivered) != 1 {
		t.Fatalf("sla report should be delivered, got %v", fs.delivered)
	}
}

// TestDependencySuppression covers the cascade-muting rules: a child's DOWN
// transition and firing burn alert stay quiet while an ancestor is down (with a
// one-time timeline note), recovery always delivers, a lookup error fails open,
// and no down ancestor means normal delivery.
func TestDependencySuppression(t *testing.T) {
	child := domain.Monitor{ID: "child", Name: "api", DependsOn: []string{"parent"}}
	downMT, _ := json.Marshal(domain.MonitorTransition{MonitorID: "child", Prev: domain.StatusUp, Cur: domain.StatusDown})
	upMT, _ := json.Marshal(domain.MonitorTransition{MonitorID: "child", Prev: domain.StatusDown, Cur: domain.StatusUp})
	burnOn, _ := json.Marshal(domain.SLOBurnAlert{MonitorID: "child", Firing: true, WindowSeconds: 3600})
	burnOff, _ := json.Marshal(domain.SLOBurnAlert{MonitorID: "child", Firing: false, WindowSeconds: 3600})
	anc := []store.DownAncestor{{ID: "parent", Name: "postgres-main"}}

	// Ancestor down → down-transition suppressed (delivered no-op) + note; the
	// recovery in the same drain still goes out.
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"child": child},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: downMT, Attempts: 1},
			{ID: "e2", Topic: domain.TopicMonitorTransition, Payload: upMT, Attempts: 1},
		},
		downAncestors: anc,
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 || !nf.up {
		t.Fatalf("only the recovery must deliver: called=%d up=%v", nf.called, nf.up)
	}
	if len(fs.delivered) != 2 {
		t.Fatalf("both events must be delivered (one as suppression no-op): %v", fs.delivered)
	}
	if len(fs.suppressionNotes) != 1 || fs.suppressionNotes[0] != "child←postgres-main" {
		t.Fatalf("suppression note = %v", fs.suppressionNotes)
	}

	// Burn alert: firing suppressed, recovery delivered.
	fs2 := &fakeStore{
		monitors: map[string]domain.Monitor{"child": child},
		pending: []domain.OutboxEvent{
			{ID: "e3", Topic: domain.TopicSLOBurnAlert, Payload: burnOn, Attempts: 1},
			{ID: "e4", Topic: domain.TopicSLOBurnAlert, Payload: burnOff, Attempts: 1},
		},
		downAncestors: anc,
	}
	nf2 := &fakeNotify{}
	newWorker(fs2, &fakeWebhook{}, nf2, &fakeMetrics{}).drain(context.Background())
	if nf2.called != 1 || !strings.Contains(nf2.text, "back to normal") {
		t.Fatalf("only the burn recovery must deliver: called=%d text=%q", nf2.called, nf2.text)
	}

	// No down ancestors → normal delivery.
	fs3 := &fakeStore{
		monitors: map[string]domain.Monitor{"child": child},
		pending:  []domain.OutboxEvent{{ID: "e5", Topic: domain.TopicMonitorTransition, Payload: downMT, Attempts: 1}},
	}
	nf3 := &fakeNotify{}
	newWorker(fs3, &fakeWebhook{}, nf3, &fakeMetrics{}).drain(context.Background())
	if nf3.called != 1 || nf3.up {
		t.Fatalf("healthy graph must alert normally: called=%d", nf3.called)
	}

	// Lookup error → fail open (alert delivered, not muted).
	fs4 := &fakeStore{
		monitors:   map[string]domain.Monitor{"child": child},
		pending:    []domain.OutboxEvent{{ID: "e6", Topic: domain.TopicMonitorTransition, Payload: downMT, Attempts: 1}},
		downAncErr: errors.New("db hiccup"),
	}
	nf4 := &fakeNotify{}
	newWorker(fs4, &fakeWebhook{}, nf4, &fakeMetrics{}).drain(context.Background())
	if nf4.called != 1 {
		t.Fatalf("lookup error must fail open: called=%d", nf4.called)
	}
}

func TestMaintenanceSuppression(t *testing.T) {
	mon := domain.Monitor{ID: "m1", Name: "api"}
	downMT, _ := json.Marshal(domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusUp, Cur: domain.StatusDown})
	reminderMT, _ := json.Marshal(domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusDown, Cur: domain.StatusDown, Reminder: true})
	upMT, _ := json.Marshal(domain.MonitorTransition{MonitorID: "m1", Prev: domain.StatusDown, Cur: domain.StatusUp})

	// In maintenance: the initial down AND the renotify reminder are muted (delivered
	// as no-ops), but recovery still goes out — this is exactly how a still-down
	// monitor gets alerted once the window closes (the next reminder is no longer muted).
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": mon},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: downMT, Attempts: 1},
			{ID: "e2", Topic: domain.TopicMonitorTransition, Payload: reminderMT, Attempts: 1},
			{ID: "e3", Topic: domain.TopicMonitorTransition, Payload: upMT, Attempts: 1},
		},
		inMaintenance: map[string]bool{"m1": true},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 || !nf.up {
		t.Fatalf("during maintenance only recovery must deliver: called=%d up=%v", nf.called, nf.up)
	}
	if len(fs.delivered) != 3 {
		t.Fatalf("all events must be marked delivered (down/reminder as no-ops): %v", fs.delivered)
	}

	// Window closed (not in maintenance) → the down alert delivers normally.
	fs2 := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": mon},
		pending:  []domain.OutboxEvent{{ID: "e4", Topic: domain.TopicMonitorTransition, Payload: reminderMT, Attempts: 1}},
	}
	nf2 := &fakeNotify{}
	newWorker(fs2, &fakeWebhook{}, nf2, &fakeMetrics{}).drain(context.Background())
	if nf2.called != 1 || nf2.up {
		t.Fatalf("after the window the reminder must deliver: called=%d up=%v", nf2.called, nf2.up)
	}

	// Lookup error → fail open (a down alert is never silently muted by a DB hiccup).
	fs3 := &fakeStore{
		monitors:         map[string]domain.Monitor{"m1": mon},
		pending:          []domain.OutboxEvent{{ID: "e5", Topic: domain.TopicMonitorTransition, Payload: downMT, Attempts: 1}},
		inMaintenanceErr: errors.New("db hiccup"),
	}
	nf3 := &fakeNotify{}
	newWorker(fs3, &fakeWebhook{}, nf3, &fakeMetrics{}).drain(context.Background())
	if nf3.called != 1 {
		t.Fatalf("maintenance lookup error must fail open: called=%d", nf3.called)
	}
}

func TestIncidentContextAttached(t *testing.T) {
	openedAuto, _ := json.Marshal(domain.IncidentEvent{
		Type: domain.EventIncidentOpened,
		Incident: domain.Incident{
			ID: "inc1", ProjectID: "p1", MonitorID: "m1", Source: domain.SourceAuto,
		},
	})
	ictx := domain.IncidentContext{CoFailureTotal: 2, CoFailures: []string{"a", "b"}, DominantClass: domain.ErrClassRefused, WindowMinutes: 5}

	// Opened auto-incident → exactly one context update, even across a re-delivery.
	fs := &fakeStore{
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicIncidentEvent, Payload: openedAuto, Attempts: 1},
			{ID: "e2", Topic: domain.TopicIncidentEvent, Payload: openedAuto, Attempts: 1}, // re-delivery
		},
		ictx: ictx,
	}
	wh := &fakeWebhook{}
	newWorker(fs, wh, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(wh.got) != 2 || len(fs.delivered) != 2 {
		t.Fatalf("webhook deliveries = %d, delivered = %v", len(wh.got), fs.delivered)
	}
	if len(fs.appended) != 1 || !strings.Contains(fs.appended[0], domain.ErrClassRefused) {
		t.Fatalf("context updates = %+v, want exactly one containing the class", fs.appended)
	}

	// Manual incident → no context.
	openedManual, _ := json.Marshal(domain.IncidentEvent{
		Type:     domain.EventIncidentOpened,
		Incident: domain.Incident{ID: "inc2", ProjectID: "p1", Source: domain.SourceManual},
	})
	fs2 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e3", Topic: domain.TopicIncidentEvent, Payload: openedManual, Attempts: 1}}, ictx: ictx}
	newWorker(fs2, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs2.appended) != 0 {
		t.Fatalf("manual incident must not get context: %+v", fs2.appended)
	}

	// Webhook failure → event retries, context NOT attached yet.
	fs3 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e4", Topic: domain.TopicIncidentEvent, Payload: openedAuto, Attempts: 1}}, ictx: ictx}
	newWorker(fs3, &fakeWebhook{err: errors.New("boom")}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs3.appended) != 0 || len(fs3.failed) != 1 {
		t.Fatalf("failed delivery must not attach context: appended=%v failed=%v", fs3.appended, fs3.failed)
	}

	// Context error → event still delivered (best-effort).
	fs4 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e5", Topic: domain.TopicIncidentEvent, Payload: openedAuto, Attempts: 1}}, ictxErr: errors.New("db down")}
	newWorker(fs4, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs4.delivered) != 1 {
		t.Fatalf("context failure must not fail the event: delivered=%v", fs4.delivered)
	}
}

func TestDeliversSubscriberConfirm(t *testing.T) {
	payload, err := json.Marshal(domain.SubscriberConfirm{
		To: "a@x.com", Subject: "Confirm your subscription to API", Body: "click confirm=abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicSubscriberConfirm, Payload: payload, Attempts: 1}}}
	mail := &fakeMail{}
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).WithMailer(mail).drain(context.Background())
	if len(mail.sent) != 1 || mail.sent[0].To != "a@x.com" || !strings.Contains(mail.sent[0].Body, "confirm=") {
		t.Fatalf("confirm email delivery: %+v", mail.sent)
	}
	if len(fs.delivered) != 1 {
		t.Fatalf("confirm should be delivered, got %v", fs.delivered)
	}

	// A confirmation is transactional, not an alert: instance-wide silence does NOT
	// suppress it.
	fs2 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e2", Topic: domain.TopicSubscriberConfirm, Payload: payload, Attempts: 1}}}
	mail2 := &fakeMail{}
	newWorker(fs2, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).WithMailer(mail2).WithSilence(func() bool { return true }).drain(context.Background())
	if len(mail2.sent) != 1 || len(fs2.delivered) != 1 {
		t.Fatalf("silence must not suppress a confirmation: sent=%d delivered=%v", len(mail2.sent), fs2.delivered)
	}

	// No mailer configured → the event fails (and will retry / eventually dead-letter).
	fs3 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e3", Topic: domain.TopicSubscriberConfirm, Payload: payload, Attempts: 1}}}
	newWorker(fs3, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs3.failed) != 1 || len(fs3.delivered) != 0 {
		t.Fatalf("confirm without mailer should fail: failed=%v delivered=%v", fs3.failed, fs3.delivered)
	}
}

func TestDeliversRegionWorkerAlert(t *testing.T) {
	payload, err := json.Marshal(domain.RegionWorkerAlert{
		Region: "geo1", ProjectID: "p1", MonitorCount: 3, Missing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicRegionWorkerAlert, Payload: payload, Attempts: 1}}}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 || nf.projectID != "p1" || !strings.Contains(nf.text, "no live worker") {
		t.Fatalf("region alert delivery: called=%d project=%s text=%q", nf.called, nf.projectID, nf.text)
	}
	if len(fs.delivered) != 1 {
		t.Fatalf("region alert should be delivered, got %v", fs.delivered)
	}

	// Instance-wide silence suppresses it (delivered as a no-op, not retried).
	fs2 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e2", Topic: domain.TopicRegionWorkerAlert, Payload: payload, Attempts: 1}}}
	nf2 := &fakeNotify{}
	newWorker(fs2, &fakeWebhook{}, nf2, &fakeMetrics{}).WithSilence(func() bool { return true }).drain(context.Background())
	if nf2.called != 0 || len(fs2.delivered) != 1 {
		t.Fatalf("silence should suppress region alert: called=%d delivered=%v", nf2.called, fs2.delivered)
	}
}

func TestDeliversEscalationStep(t *testing.T) {
	payload, err := json.Marshal(domain.EscalationStepAlert{
		IncidentID: "i1", MonitorID: "m1", MonitorName: "payments", Step: 1, ChannelIDs: []string{"c1", "c2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicEscalationStep, Payload: payload, Attempts: 1}}}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 || len(nf.channelIDs) != 2 || !strings.Contains(nf.text, "payments") {
		t.Fatalf("escalation delivery: called=%d channels=%v text=%q", nf.called, nf.channelIDs, nf.text)
	}
	// Silence suppresses it (delivered as a no-op).
	fs2 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e2", Topic: domain.TopicEscalationStep, Payload: payload, Attempts: 1}}}
	nf2 := &fakeNotify{}
	newWorker(fs2, &fakeWebhook{}, nf2, &fakeMetrics{}).WithSilence(func() bool { return true }).drain(context.Background())
	if nf2.called != 0 || len(fs2.delivered) != 1 {
		t.Fatalf("silence should suppress escalation: called=%d delivered=%v", nf2.called, fs2.delivered)
	}
}

func TestEscalationPolicySuppressesFlatDown(t *testing.T) {
	// A monitor with an escalation policy: the flat DOWN transition is suppressed (the
	// ladder drives it), but a recovery (UP) still notifies all channels.
	mon := domain.Monitor{ID: "m1", Name: "API", Status: domain.StatusDown, AutoIncident: true, EscalationPolicyID: "pol1"}
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": mon},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: 1},
		},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 0 || len(fs.delivered) != 1 {
		t.Fatalf("policy monitor down should be suppressed: called=%d delivered=%v", nf.called, fs.delivered)
	}
	// Recovery still delivers.
	fs2 := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API", Status: domain.StatusUp, AutoIncident: true, EscalationPolicyID: "pol1"}},
		pending:  []domain.OutboxEvent{{ID: "e2", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusDown, domain.StatusUp), Attempts: 1}},
	}
	nf2 := &fakeNotify{}
	newWorker(fs2, &fakeWebhook{}, nf2, &fakeMetrics{}).drain(context.Background())
	if nf2.called != 1 || nf2.up != true {
		t.Fatalf("recovery should notify: called=%d up=%v", nf2.called, nf2.up)
	}
}

func TestTransitionPolicySkipsNonNotified(t *testing.T) {
	// pending→up is not a notifiable transition; it should be marked delivered
	// without calling the notifier.
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Status: domain.StatusUp}},
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusPending, domain.StatusUp), Attempts: 1},
		},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 0 {
		t.Fatalf("notifier called %d times for a non-notified transition", nf.called)
	}
	if len(fs.delivered) != 1 {
		t.Fatalf("event should be marked delivered (no-op), got delivered=%v", fs.delivered)
	}
}

func TestDeletedMonitorTransitionIsDropped(t *testing.T) {
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{}, // m1 no longer exists
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: transition(t, "m1", domain.StatusUp, domain.StatusDown), Attempts: 1},
		},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 0 || len(fs.delivered) != 1 || len(fs.failed) != 0 {
		t.Fatalf("deleted monitor should drop cleanly: called=%d delivered=%v failed=%v", nf.called, fs.delivered, fs.failed)
	}
}

func TestFailureRetriesThenDead(t *testing.T) {
	// A webhook that errors: an early attempt retries; the max-th attempt is dead.
	retrying := &fakeStore{pending: []domain.OutboxEvent{
		{ID: "e1", Topic: domain.TopicIncidentEvent, Payload: incidentEvent(t, domain.EventIncidentOpened, "p1"), Attempts: 3},
	}}
	m1 := &fakeMetrics{}
	newWorker(retrying, &fakeWebhook{err: errors.New("boom")}, &fakeNotify{}, m1).drain(context.Background())
	if len(retrying.failed) != 1 || len(retrying.delivered) != 0 {
		t.Fatalf("early failure should be recorded as failed, not delivered: %+v", retrying)
	}
	if m1.dead != 0 {
		t.Fatalf("attempt 3 < max should not be dead yet")
	}

	dead := &fakeStore{pending: []domain.OutboxEvent{
		{ID: "e2", Topic: domain.TopicIncidentEvent, Payload: incidentEvent(t, domain.EventIncidentOpened, "p1"), Attempts: maxAttempts},
	}}
	m2 := &fakeMetrics{}
	newWorker(dead, &fakeWebhook{err: errors.New("boom")}, &fakeNotify{}, m2).drain(context.Background())
	if len(dead.failed) != 1 || m2.dead != 1 {
		t.Fatalf("max-attempt failure should count dead: failed=%v dead=%d", dead.failed, m2.dead)
	}
}

func TestUnknownTopicAndNilDeliverersFail(t *testing.T) {
	cases := []struct {
		name    string
		event   domain.OutboxEvent
		webhook *fakeWebhook
		notify  *fakeNotify
	}{
		{"unknown topic", domain.OutboxEvent{ID: "e1", Topic: "bogus", Payload: []byte("{}"), Attempts: 1}, &fakeWebhook{}, &fakeNotify{}},
		{"nil webhook deliverer", domain.OutboxEvent{ID: "e2", Topic: domain.TopicIncidentEvent, Payload: incidentEvent(t, domain.EventIncidentOpened, "p1"), Attempts: 1}, nil, &fakeNotify{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeStore{pending: []domain.OutboxEvent{c.event}}
			var wh WebhookDeliverer
			if c.webhook != nil {
				wh = c.webhook
			}
			New(fs, wh, c.notify, &fakeMetrics{}, slog.New(slog.NewTextHandler(io.Discard, nil))).drain(context.Background())
			if len(fs.failed) != 1 || len(fs.delivered) != 0 {
				t.Fatalf("expected failure: failed=%v delivered=%v", fs.failed, fs.delivered)
			}
		})
	}
}

func TestDrainToleratesClaimError(t *testing.T) {
	fs := &fakeStore{claimErr: errors.New("db down")}
	// Must return (log + stop), not spin or panic.
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
}

func TestRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: Run must return promptly
	newWorker(&fakeStore{}, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).Run(ctx)
}
