package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
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
	pending     []domain.OutboxEvent
	delivered   []string
	failed      []string
	failReasons []string
	monitors    map[string]domain.Monitor
	claimErr    error

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

	// alerting-ownership fakes (FR-021 §16)
	delegation      map[string]store.DelegationVerdict // "monitor|signal" → verdict
	delegationErr   error
	suppressRecords []string // "event|monitor|topic|owners"
	suppressErr     error
	alertSeq        map[string]int64 // "service|signal|target|rule" → current sequence
	incidentSeq     map[string]int64 // incident id → current lifecycle sequence
	alertSeqMissing bool

	// impact-correlation fakes (FR-021 §14.3)
	correlated        []string // incident ids passed to CorrelateIncident
	correlateLinks    []domain.ServiceImpactLink
	correlateOverflow int
	correlateErr      error
	// monitorLookups records every GetMonitor the worker performs, so a test can assert a
	// lookup did NOT happen — the absence of a call is the claim in FR-023 D7.
	monitorLookups []string
}

func (f *fakeStore) CorrelateIncident(_ context.Context, incidentID string) ([]domain.ServiceImpactLink, int, error) {
	f.correlated = append(f.correlated, incidentID)
	if f.correlateErr != nil {
		return nil, 0, f.correlateErr
	}
	return f.correlateLinks, f.correlateOverflow, nil
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
func (f *fakeStore) FailOutbox(_ context.Context, id, _, lastErr string, _ int) (bool, error) {
	f.failed = append(f.failed, id)
	f.failReasons = append(f.failReasons, lastErr)
	return true, nil
}
func (f *fakeStore) GetMonitor(_ context.Context, id string) (domain.Monitor, error) {
	f.monitorLookups = append(f.monitorLookups, id)
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
	resolved   *int
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

// resolved, when set, is how many of the requested channels still exist — the fake's way of
// deleting a snapshot recipient out from under an announcement. Zero means "all of them".
func (f *fakeNotify) DeliverChannelsReporting(
	_ context.Context, channelIDs []string, text string,
) (domain.ChannelDelivery, error) {
	f.channelIDs, f.text, f.called = channelIDs, text, f.called+1
	out := domain.ChannelDelivery{Requested: len(channelIDs), Resolved: len(channelIDs)}
	if f.resolved != nil {
		out.Resolved = *f.resolved
	}
	return out, f.err
}

type fakeMetrics struct {
	suppressed                                      map[string]int
	failOpen                                        map[string]int
	delegation                                      map[string]int // "signal/state" → count
	undeliverable                                   map[string]int
	recipientMissing                                int
	delivered, dead, impactFailures, impactOverflow int
	impactLinks                                     map[string]int
}

func (m *fakeMetrics) RecordOutboxDelivered() { m.delivered++ }
func (m *fakeMetrics) RecordOutboxDead()      { m.dead++ }
func (m *fakeMetrics) RecordImpactLinks(role string, n int) {
	if m.impactLinks == nil {
		m.impactLinks = map[string]int{}
	}
	m.impactLinks[role] += n
}
func (m *fakeMetrics) RecordImpactFailure() { m.impactFailures++ }
func (m *fakeMetrics) RecordAlertSuppressed(topic, reason string) {
	if m.suppressed == nil {
		m.suppressed = map[string]int{}
	}
	m.suppressed[topic+"|"+reason]++
}
func (m *fakeMetrics) RecordServiceDelegation(signal, state string) {
	if m.delegation == nil {
		m.delegation = map[string]int{}
	}
	m.delegation[signal+"/"+state]++
}

func (m *fakeMetrics) RecordServiceAlertUndeliverable(signal string) {
	if m.undeliverable == nil {
		m.undeliverable = map[string]int{}
	}
	m.undeliverable[signal]++
}

func (m *fakeMetrics) RecordServiceAlertRecipientMissing(n int) { m.recipientMissing += n }

func (m *fakeMetrics) RecordDelegationFailOpen(reason string) {
	if m.failOpen == nil {
		m.failOpen = map[string]int{}
	}
	m.failOpen[reason]++
}
func (m *fakeMetrics) RecordImpactWitnessOverflow(n int) { m.impactOverflow += n }

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

func transitionSeq(t *testing.T, monitorID string, prev, cur domain.MonitorStatus, seq int64, reminder bool) []byte {
	t.Helper()
	b, err := json.Marshal(domain.MonitorTransition{MonitorID: monitorID, Prev: prev, Cur: cur, Seq: seq, Reminder: reminder})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newWorker(fs *fakeStore, wh *fakeWebhook, nf *fakeNotify, m *fakeMetrics) *Worker {
	return New(fs, wh, nf, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestStaleTransitionSuppressed covers the #2 delivery-time staleness gate: a
// transition (or reminder) whose stamped Seq is older than the monitor's current
// state_sequence is dropped as superseded, while an up-to-date Seq and a legacy
// Seq==0 event both deliver.
func TestStaleTransitionSuppressed(t *testing.T) {
	// Monitor has since advanced to state_sequence 3 (e.g. down→up→down again).
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{"m1": {ID: "m1", Name: "API", Status: domain.StatusDown, StateSequence: 3}},
		pending: []domain.OutboxEvent{
			// Stale DOWN from an earlier sequence — must be dropped (delivered, not notified).
			{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 1, false), Attempts: 1},
			// Stale reminder — must be dropped too.
			{ID: "e2", Topic: domain.TopicMonitorTransition, Payload: transitionSeq(t, "m1", domain.StatusDown, domain.StatusDown, 2, true), Attempts: 1},
			// Current DOWN (seq == current) — must deliver.
			{ID: "e3", Topic: domain.TopicMonitorTransition, Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 3, false), Attempts: 1},
			// Legacy event with no sequence — never treated as stale, must deliver.
			{ID: "e4", Topic: domain.TopicMonitorTransition, Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 0, false), Attempts: 1},
		},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

	// All four events are terminally handled (delivered), none failed/retried.
	if len(fs.delivered) != 4 || len(fs.failed) != 0 {
		t.Fatalf("delivered=%v failed=%v, want all 4 delivered", fs.delivered, fs.failed)
	}
	// Only the current (e3) and legacy (e4) events actually notified.
	if nf.called != 2 {
		t.Fatalf("notifier called %d times, want 2 (current + legacy; stale down & reminder dropped)", nf.called)
	}
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

	// A SERVICE-anchored auto-incident gets NO ⚡ context (FR-022 invariant 12). The note's whole
	// content is about a MONITOR — co-failing monitors, a dominant error class, a single region —
	// and the ⚡/⏸ family keeps exactly one home each so a single outage is never explained twice.
	// The source is `auto` here, which is what makes this a real gate rather than the manual case
	// two blocks up passing again under a new name.
	openedService, _ := json.Marshal(domain.IncidentEvent{
		Type: domain.EventIncidentOpened,
		Incident: domain.Incident{
			ID: "inc3", ProjectID: "p1", ServiceID: "svc1", Source: domain.SourceAuto,
		},
	})
	fs5 := &fakeStore{pending: []domain.OutboxEvent{{ID: "e6", Topic: domain.TopicIncidentEvent, Payload: openedService, Attempts: 1}}, ictx: ictx}
	newWorker(fs5, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	if len(fs5.delivered) != 1 {
		t.Fatalf("a service incident's event was not delivered: %v", fs5.delivered)
	}
	if len(fs5.appended) != 0 {
		t.Fatalf("a SERVICE incident got a ⚡ context note (%+v) — its content describes a monitor, and "+
			"the outage would then be explained twice (FR-022 invariant 12)", fs5.appended)
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

// The correlation topic delivers through its own case: the store attempt runs,
// newly inserted links are counted per role, and the event is marked delivered.
func TestDeliversIncidentCorrelation(t *testing.T) {
	fs := &fakeStore{
		pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicIncidentCorrelation, Payload: []byte(`{"incident_id":"i1"}`), Attempts: 1},
		},
		correlateOverflow: 2,
		correlateLinks: []domain.ServiceImpactLink{
			{ServiceID: "s1", Slug: "payments", Role: domain.ImpactProbableRoot, Path: []string{"payments", "checkout"}},
			{ServiceID: "s2", Slug: "storefront", Role: domain.ImpactAffected, Path: []string{"checkout", "storefront"}},
			{ServiceID: "s3", Slug: "billing", Role: domain.ImpactProbableRoot, Path: []string{"billing", "checkout"}},
		},
	}
	m := &fakeMetrics{}
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, m).drain(context.Background())

	if len(fs.correlated) != 1 || fs.correlated[0] != "i1" {
		t.Fatalf("correlated = %v, want [i1]", fs.correlated)
	}
	if len(fs.delivered) != 1 || len(fs.failed) != 0 {
		t.Fatalf("delivered=%v failed=%v", fs.delivered, fs.failed)
	}
	if m.impactLinks[domain.ImpactProbableRoot] != 2 || m.impactLinks[domain.ImpactAffected] != 1 {
		t.Fatalf("impact link metrics = %v", m.impactLinks)
	}
	if m.impactOverflow != 2 {
		t.Fatalf("witness overflow metric = %d, want the returned 2 ([283]: the count must reach RecordImpactWitnessOverflow)", m.impactOverflow)
	}
}

// The two failure envelopes are independent (invariant 52): a dead webhook
// never blocks correlation, and a failing correlation never blocks the
// incident webhook — each event fails or delivers on its own.
func TestCorrelationAndWebhookFailIndependently(t *testing.T) {
	// (a) webhook dead, correlation delivers.
	fs := &fakeStore{
		pending: []domain.OutboxEvent{
			{ID: "wh", Topic: domain.TopicIncidentEvent, Payload: incidentEvent(t, domain.EventIncidentOpened, "p1"), Attempts: 1},
			{ID: "corr", Topic: domain.TopicIncidentCorrelation, Payload: []byte(`{"incident_id":"i1"}`), Attempts: 1},
		},
	}
	m := &fakeMetrics{}
	newWorker(fs, &fakeWebhook{err: errors.New("endpoint dead")}, &fakeNotify{}, m).drain(context.Background())
	if len(fs.delivered) != 1 || fs.delivered[0] != "corr" {
		t.Fatalf("want correlation delivered despite the dead webhook, got delivered=%v failed=%v", fs.delivered, fs.failed)
	}
	if len(fs.correlated) != 1 {
		t.Fatalf("correlate not attempted: %v", fs.correlated)
	}

	// (b) correlation failing, webhook delivers; the failure is counted.
	fs2 := &fakeStore{
		pending: []domain.OutboxEvent{
			{ID: "wh", Topic: domain.TopicIncidentEvent, Payload: incidentEvent(t, domain.EventIncidentOpened, "p1"), Attempts: 1},
			{ID: "corr", Topic: domain.TopicIncidentCorrelation, Payload: []byte(`{"incident_id":"i1"}`), Attempts: 1},
		},
		correlateErr: errors.New("correlation bug"),
	}
	m2 := &fakeMetrics{}
	newWorker(fs2, &fakeWebhook{}, &fakeNotify{}, m2).drain(context.Background())
	if len(fs2.delivered) != 1 || fs2.delivered[0] != "wh" {
		t.Fatalf("want the webhook delivered despite the correlation failure, got delivered=%v failed=%v", fs2.delivered, fs2.failed)
	}
	if len(fs2.failed) != 1 || fs2.failed[0] != "corr" {
		t.Fatalf("want the correlation event on the retry path, got failed=%v", fs2.failed)
	}
	if m2.impactFailures != 1 {
		t.Fatalf("impact failures = %d, want 1", m2.impactFailures)
	}
}

// The fence pin (invariant 61): every topic domain.FencedTopics() names must be
// dispatchable by THIS binary's worker — a fenced topic the switch does not
// know would wait forever, because no other owner may claim it either.
func TestFencedTopicsAreDispatchable(t *testing.T) {
	payloads := map[string][]byte{
		domain.TopicIncidentCorrelation: []byte(`{"incident_id":"i1"}`),
		// A CLOSE, deliberately: an onset for a service the fake knows nothing about would be dropped
		// by the ordering gate as "the service is gone", and this test is about the switch knowing
		// the topic, not about the gate.
		domain.TopicServiceAlert: []byte(
			`{"service_id":"s1","service_name":"Checkout","signal":"health","firing":false,` +
				`"close_reason":"recovered","seq":1,"recipients":["ch-1"]}`),
		// A RESOLUTION, for the same reason the service alert above is a close: the ordering gate
		// never drops one, so this exercises the switch rather than the gate.
		domain.TopicIncidentEvent: []byte(
			`{"event":"incident.resolved","incident":{"id":"inc-1","project_id":"p1"},"seq":2}`),
	}
	for _, topic := range domain.FencedTopics() {
		payload, ok := payloads[topic]
		if !ok {
			t.Fatalf("fenced topic %q has no test payload — add it here AND make sure the worker dispatches it", topic)
		}
		fs := &fakeStore{pending: []domain.OutboxEvent{{ID: "e", Topic: topic, Payload: payload, Attempts: 1}}}
		newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
		if len(fs.delivered) != 1 {
			t.Fatalf("fenced topic %q not dispatched: delivered=%v failed=%v", topic, fs.delivered, fs.failed)
		}
	}
}

// The THIRD parity leg (the other two live in internal/store): every topic this binary can
// ENQUEUE must be one its own worker can DISPATCH. The schema-side guard proves such a row can be
// written; without this one, a topic could be written, accepted, and then sit failing forever
// because the switch has no case for it — the mirror image of the whitelist narrowing that stopped
// three live topics in phase 5.
//
// It asserts the SWITCH, not the payloads: a topic the worker knows fails on its fixture ("cannot
// unmarshal", a nil deliverer), while a topic it does not know fails with "unknown outbox topic".
// Only the second is drift, so only the second fails this test.
func TestEveryEnqueueableTopicIsDispatchable(t *testing.T) {
	for _, topic := range domain.AllTopics() {
		fs := &fakeStore{pending: []domain.OutboxEvent{
			{ID: "e", Topic: topic, Payload: []byte(`{}`), Attempts: 1},
		}}
		newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
		for _, reason := range fs.failReasons {
			if strings.Contains(reason, "unknown outbox topic") {
				t.Fatalf("topic %q is enqueueable but the worker has no dispatch case: %s", topic, reason)
			}
		}
	}
	// The guard has teeth: a topic nobody dispatches IS reported this way.
	fs := &fakeStore{pending: []domain.OutboxEvent{
		{ID: "e", Topic: "not_a_topic", Payload: []byte(`{}`), Attempts: 1},
	}}
	newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
	var sawUnknown bool
	for _, reason := range fs.failReasons {
		if strings.Contains(reason, "unknown outbox topic") {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Fatal("the drift signal this guard reads no longer exists — it would pass vacuously")
	}
}

// ── FR-021 §16 store surface ─────────────────────────────────────────────────────────────

func (f *fakeStore) ActiveDelegation(
	_ context.Context, monitorID, projectID string, signal store.DelegationSignal,
) (store.DelegationVerdict, error) {
	if f.delegationErr != nil {
		return store.DelegationVerdict{}, f.delegationErr
	}
	v, ok := f.delegation[monitorID+"|"+string(signal)]
	if !ok {
		return store.DelegationVerdict{FailOpenReason: "no_active_owner"}, nil
	}
	return v, nil
}

func (f *fakeStore) RecordSuppression(
	_ context.Context, eventID, monitorID, projectID, topic string, owners []store.DelegationOwner,
) error {
	if f.suppressErr != nil {
		return f.suppressErr
	}
	names := make([]string, 0, len(owners))
	for _, o := range owners {
		names = append(names, o.Name)
	}
	f.suppressRecords = append(f.suppressRecords,
		eventID+"|"+monitorID+"|"+topic+"|"+strings.Join(names, ","))
	return nil
}

// incidentSeq is the incident's CURRENT lifecycle sequence, keyed by incident id. Absent means the
// incident is gone, which the gate must treat as "an opening has nothing to announce".
func (f *fakeStore) IncidentEventSequence(_ context.Context, incidentID string) (int64, error) {
	seq, ok := f.incidentSeq[incidentID]
	if !ok {
		return 0, store.ErrNotFound
	}
	return seq, nil
}

func (f *fakeStore) ServiceAlertSequence(
	_ context.Context, a domain.ServiceAlert,
) (int64, error) {
	if f.alertSeqMissing {
		return 0, store.ErrNotFound
	}
	// Keyed by the same tuple the real latch is keyed by: two targets of one service may carry the
	// same canonical rule key, so a fake keyed by (service, rule) would answer for the wrong one and
	// make the ordering gate look correct in a test while it is broken in the store.
	return f.alertSeq[a.ServiceID+"|"+string(a.Signal)+"|"+a.SLATargetID+"|"+a.RuleKey], nil
}

// FR-021 §16.1 at the delivery boundary. These are the cases the two design rounds produced, and
// each of them is a way to lose a page or its ending.

func armedFor(monitorID string, signals ...store.DelegationSignal) map[string]store.DelegationVerdict {
	m := map[string]store.DelegationVerdict{}
	for _, sig := range signals {
		m[monitorID+"|"+string(sig)] = store.DelegationVerdict{
			Owners: []store.DelegationOwner{{ServiceID: "svc-1", Slug: "checkout", Name: "Checkout"}},
		}
	}
	return m
}

// A DOWN is suppressed while live coverage is armed; the RECOVERY of the same monitor is not,
// whatever the arming says. Arming changes between an onset and its close, and a muted recovery
// leaves whoever was paged holding an alert that can never end.
func TestDelegationSuppressesOnsetButNeverRecovery(t *testing.T) {
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{
			"m1": {ID: "m1", ProjectID: "p1", Name: "checkout-http", Status: domain.StatusDown},
		},
		delegation: armedFor("m1", store.DelegationLive),
		pending: []domain.OutboxEvent{
			{ID: "e-down", Topic: domain.TopicMonitorTransition,
				Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 0, false)},
		},
	}
	nf, m := &fakeNotify{}, &fakeMetrics{}
	newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

	if nf.called != 0 {
		t.Fatalf("a DOWN was delivered while the service actively covers it (called=%d)", nf.called)
	}
	if len(fs.suppressRecords) != 1 || !strings.Contains(fs.suppressRecords[0], "Checkout") {
		t.Fatalf("suppression records = %v, want one naming the covering service", fs.suppressRecords)
	}
	if m.suppressed["monitor_transition|service_delegation"] != 1 {
		t.Fatalf("suppression counter = %v", m.suppressed)
	}

	// The recovery, with delegation still armed.
	fs.monitors["m1"] = domain.Monitor{ID: "m1", ProjectID: "p1", Name: "checkout-http", Status: domain.StatusUp}
	fs.pending = []domain.OutboxEvent{
		{ID: "e-up", Topic: domain.TopicMonitorTransition,
			Payload: transitionSeq(t, "m1", domain.StatusDown, domain.StatusUp, 0, false)},
	}
	nf.called = 0
	newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())
	if nf.called != 1 {
		t.Fatalf("the RECOVERY was suppressed (called=%d): a recipient would hold a DOWN forever", nf.called)
	}
}

// Everything ambiguous PAGES, and says why.
func TestDelegationFailsOpen(t *testing.T) {
	cases := []struct {
		name       string
		arrange    func(*fakeStore)
		wantReason string
	}{
		{"lookup error", func(f *fakeStore) { f.delegationErr = errors.New("boom") }, "error"},
		{"no active owner", func(f *fakeStore) {
			f.delegation = map[string]store.DelegationVerdict{
				"m1|live": {FailOpenReason: "no_active_owner"},
			}
		}, "no_active_owner"},
		{"the suppression cannot be recorded", func(f *fakeStore) {
			f.delegation = armedFor("m1", store.DelegationLive)
			f.suppressErr = errors.New("write failed")
		}, "record_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeStore{
				monitors: map[string]domain.Monitor{
					"m1": {ID: "m1", ProjectID: "p1", Name: "checkout-http", Status: domain.StatusDown},
				},
				pending: []domain.OutboxEvent{
					{ID: "e", Topic: domain.TopicMonitorTransition,
						Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 0, false)},
				},
			}
			c.arrange(fs)
			nf, m := &fakeNotify{}, &fakeMetrics{}
			newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

			if nf.called != 1 {
				t.Fatalf("%s: the alert was MUTED (called=%d) — an ambiguous delegation must page",
					c.name, nf.called)
			}
			if m.failOpen[c.wantReason] != 1 {
				t.Fatalf("%s: fail-open counters = %v, want reason %q", c.name, m.failOpen, c.wantReason)
			}
		})
	}
}

// The ladder is where a monitor with an escalation policy ACTUALLY pages, so delegation has to cover
// it — otherwise phase 5 keeps its promise for everyone except the installations that page properly.
func TestDelegationSuppressesTheEscalationLadder(t *testing.T) {
	payload, err := json.Marshal(domain.EscalationStepAlert{
		IncidentID: "inc-1", MonitorID: "m1", MonitorName: "checkout-http",
		Step: 0, ChannelIDs: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fs := &fakeStore{
		monitors:   map[string]domain.Monitor{"m1": {ID: "m1", ProjectID: "p1", Name: "checkout-http"}},
		delegation: armedFor("m1", store.DelegationLive),
		pending:    []domain.OutboxEvent{{ID: "e-step", Topic: domain.TopicEscalationStep, Payload: payload}},
	}
	nf, m := &fakeNotify{}, &fakeMetrics{}
	newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

	if nf.called != 0 {
		t.Fatalf("the escalation step paged anyway (called=%d)", nf.called)
	}
	if m.suppressed["escalation_step|service_delegation"] != 1 {
		t.Fatalf("suppression counter = %v, want the ladder counted", m.suppressed)
	}
}

// The burn signal is delegated SEPARATELY: a service can cover the live signal while its burn window
// is held, and then a member's burn alert must still be delivered.
func TestBurnDelegationIsIndependentOfLive(t *testing.T) {
	firing, err := json.Marshal(domain.SLOBurnAlert{MonitorID: "m1", Window: "30d", Firing: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	clear, err := json.Marshal(domain.SLOBurnAlert{MonitorID: "m1", Window: "30d", Firing: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	base := func(d map[string]store.DelegationVerdict, ev domain.OutboxEvent) *fakeStore {
		return &fakeStore{
			monitors:   map[string]domain.Monitor{"m1": {ID: "m1", ProjectID: "p1", Name: "checkout-http"}},
			delegation: d,
			pending:    []domain.OutboxEvent{ev},
		}
	}

	// LIVE coverage alone must not touch a burn alert.
	fs := base(armedFor("m1", store.DelegationLive),
		domain.OutboxEvent{ID: "e1", Topic: domain.TopicSLOBurnAlert, Payload: firing})
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 {
		t.Fatalf("a burn alert was suppressed by LIVE coverage (called=%d)", nf.called)
	}

	// With burn armed, the FIRING alert is suppressed...
	fs = base(armedFor("m1", store.DelegationLive, store.DelegationBurn),
		domain.OutboxEvent{ID: "e2", Topic: domain.TopicSLOBurnAlert, Payload: firing})
	nf = &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 0 {
		t.Fatalf("a FIRING burn alert was delivered while burn coverage is armed (called=%d)", nf.called)
	}

	// ...and a CLEAR still is not.
	fs = base(armedFor("m1", store.DelegationLive, store.DelegationBurn),
		domain.OutboxEvent{ID: "e3", Topic: domain.TopicSLOBurnAlert, Payload: clear})
	nf = &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 {
		t.Fatalf("the burn CLEAR was suppressed: a recipient would hold a firing alert forever")
	}
}

// The service's own alert: the ordering gate drops a superseded ONSET, and never a close.
func TestServiceAlertOrderingGate(t *testing.T) {
	alert := func(seq int64, firing bool, reason domain.ServiceAlertCloseReason) []byte {
		b, err := json.Marshal(domain.ServiceAlert{
			ServiceID: "svc-1", ServiceName: "Checkout", Signal: domain.ServiceSignalHealth,
			Firing: firing, State: domain.ServiceAlertDown, Seq: seq, ConfirmedOver: 2,
			CloseReason: reason, Recipients: []string{"ch-1"},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	fs := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 7},
		pending: []domain.OutboxEvent{
			{ID: "e-stale", Topic: domain.TopicServiceAlert, Payload: alert(3, true, "")},
		},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 0 {
		t.Fatalf("a superseded ONSET was announced (called=%d)", nf.called)
	}

	fs.pending = []domain.OutboxEvent{
		{ID: "e-now", Topic: domain.TopicServiceAlert, Payload: alert(7, true, "")},
	}
	nf = &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 {
		t.Fatalf("the current onset was not delivered (called=%d)", nf.called)
	}

	// A CLOSE for a service that no longer exists still delivers: its episode outlived it precisely
	// so the people who were paged learn that it ended, and it says WHY.
	fs.alertSeqMissing = true
	fs.pending = []domain.OutboxEvent{
		{ID: "e-close", Topic: domain.TopicServiceAlert,
			Payload: alert(8, false, domain.CloseServiceDeleted)},
	}
	nf = &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 {
		t.Fatalf("a close for a deleted service was dropped (called=%d)", nf.called)
	}
	if !strings.Contains(nf.text, "deleted") {
		t.Fatalf("the close does not say why it ended: %q", nf.text)
	}
}

// FR-021 §16.4a/§16.6b — the two ways an announcement reaches nobody WITHOUT failing.
//
// Both are invisible in every other signal: the outbox marks the row delivered, no error is
// returned, no retry happens, and the operator sees a page that was never sent. They are the reason
// the spec asks for counters of their own.
func TestServiceAlertThatReachesNobodyIsCounted(t *testing.T) {
	alert := func(recipients []string) []byte {
		b, err := json.Marshal(domain.ServiceAlert{
			ServiceID: "svc-1", ServiceName: "Checkout", Signal: domain.ServiceSignalHealth,
			Firing: false, State: domain.ServiceAlertDown, Seq: 9,
			CloseReason: domain.CloseRecovered, Recipients: recipients,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	t.Run("a snapshot recipient whose channel is gone", func(t *testing.T) {
		fs := &fakeStore{alertSeqMissing: true, pending: []domain.OutboxEvent{
			{ID: "e1", Topic: domain.TopicServiceAlert, Payload: alert([]string{"ch-1", "ch-2"})},
		}}
		one := 1
		nf := &fakeNotify{resolved: &one} // ch-2 has been deleted since the onset
		m := &fakeMetrics{}
		newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

		if nf.called != 1 {
			t.Fatalf("the close was not attempted (called=%d)", nf.called)
		}
		if m.recipientMissing != 1 {
			t.Fatalf("recipient_missing = %d, want 1: a snapshot recipient that no longer exists "+
				"is COUNTED, never silently replaced by whoever is on call now", m.recipientMissing)
		}
		if m.undeliverable["health"] != 0 {
			t.Fatalf("an alert that reached one of two recipients was called undeliverable")
		}
	})

	t.Run("nobody left at all", func(t *testing.T) {
		fs := &fakeStore{alertSeqMissing: true, pending: []domain.OutboxEvent{
			{ID: "e2", Topic: domain.TopicServiceAlert, Payload: alert([]string{"ch-1"})},
		}}
		zero := 0
		nf := &fakeNotify{resolved: &zero}
		m := &fakeMetrics{}
		newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

		if m.recipientMissing != 1 || m.undeliverable["health"] != 1 {
			t.Fatalf("missing=%d undeliverable=%d, want 1 and 1: an announcement nobody heard is "+
				"not a successful delivery", m.recipientMissing, m.undeliverable["health"])
		}
	})

	t.Run("an empty recipient snapshot", func(t *testing.T) {
		fs := &fakeStore{alertSeqMissing: true, pending: []domain.OutboxEvent{
			{ID: "e3", Topic: domain.TopicServiceAlert, Payload: alert(nil)},
		}}
		nf := &fakeNotify{}
		m := &fakeMetrics{}
		newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

		if nf.called != 0 {
			t.Fatalf("an empty route was still sent somewhere (called=%d)", nf.called)
		}
		if m.undeliverable["health"] != 1 {
			t.Fatalf("undeliverable = %d, want 1", m.undeliverable["health"])
		}
	})
}

// §16.6b — delegation reports its outcome per signal, in three states, because "why is this monitor
// still paging" is the question the runbook starts from. `armed` means a member's alert was
// suppressed by an active replacement; `disarmed` means there was none, so it paged for itself;
// `degraded` means the lookup could not conclude, which also pages.
func TestDelegationOutcomesAreCountedPerSignal(t *testing.T) {
	down := func() []domain.OutboxEvent {
		return []domain.OutboxEvent{{ID: "e", Topic: domain.TopicMonitorTransition,
			Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 0, false)}}
	}
	monitors := map[string]domain.Monitor{
		"m1": {ID: "m1", ProjectID: "p1", Name: "checkout-http", Status: domain.StatusDown},
	}

	t.Run("armed", func(t *testing.T) {
		fs := &fakeStore{monitors: monitors, delegation: armedFor("m1", store.DelegationLive), pending: down()}
		m := &fakeMetrics{}
		newWorker(fs, &fakeWebhook{}, &fakeNotify{}, m).drain(context.Background())
		if m.delegation["health/armed"] != 1 {
			t.Fatalf("delegation counters = %v, want one health/armed", m.delegation)
		}
	})

	t.Run("disarmed", func(t *testing.T) {
		fs := &fakeStore{monitors: monitors, pending: down()} // no active owner
		m := &fakeMetrics{}
		newWorker(fs, &fakeWebhook{}, &fakeNotify{}, m).drain(context.Background())
		if m.delegation["health/disarmed"] != 1 {
			t.Fatalf("delegation counters = %v, want one health/disarmed", m.delegation)
		}
	})

	t.Run("degraded", func(t *testing.T) {
		fs := &fakeStore{monitors: monitors, delegationErr: errors.New("lookup exploded"), pending: down()}
		m := &fakeMetrics{}
		nf := &fakeNotify{}
		newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())
		if m.delegation["health/degraded"] != 1 {
			t.Fatalf("delegation counters = %v, want one health/degraded", m.delegation)
		}
		if nf.called != 1 {
			t.Fatal("an ambiguous delegation lookup did not FAIL OPEN: the page was withheld")
		}
	})
}

// Invariant 78 (§16, §19): suppression touches DELIVERY ONLY. Facts, status flips,
// auto-incidents, escalation rows and progress, and the SLO history are what they would have been
// with no delegation at all — the only differences are the page that was not sent and the
// suppression record that says so.
//
// This is the invariant that makes delegation safe to turn on: an operator who disowns a service
// tomorrow must find the same history they would have had, not a gap. Run one identical event
// through an ARMED worker and a DISARMED one and compare everything the worker did to the store.
func TestDelegationChangesDeliveryAndNothingElse(t *testing.T) {
	build := func(armed bool) (*fakeStore, *fakeNotify) {
		fs := &fakeStore{
			monitors: map[string]domain.Monitor{
				"m1": {ID: "m1", ProjectID: "p1", Name: "checkout-http", Status: domain.StatusDown},
			},
			ictx: domain.IncidentContext{CoFailures: []string{"api-http"}, CoFailureTotal: 1, DominantClass: "timeout"},
			pending: []domain.OutboxEvent{
				{ID: "e-down", Topic: domain.TopicMonitorTransition,
					Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 0, false)},
			},
		}
		if armed {
			fs.delegation = armedFor("m1", store.DelegationLive)
		}
		nf := &fakeNotify{}
		newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
		return fs, nf
	}

	armed, armedNotify := build(true)
	open, openNotify := build(false)

	// The page is the ONLY thing that differs.
	if armedNotify.called != 0 || openNotify.called == 0 {
		t.Fatalf("delivery: armed called=%d (want 0), disarmed called=%d (want >0)",
			armedNotify.called, openNotify.called)
	}
	if len(armed.suppressRecords) != 1 || len(open.suppressRecords) != 0 {
		t.Fatalf("suppression records: armed=%v, disarmed=%v", armed.suppressRecords, open.suppressRecords)
	}

	// Everything else the worker did to the store is identical. The event is CONSUMED either way:
	// a suppressed row that stayed pending would be redelivered forever, and a suppressed row that
	// FAILED would poison the queue with a decision that was deliberate.
	if !reflect.DeepEqual(armed.delivered, open.delivered) {
		t.Fatalf("delivered rows differ: armed=%v, disarmed=%v — suppression must consume the event, "+
			"not change the queue's history (invariant 78)", armed.delivered, open.delivered)
	}
	if !reflect.DeepEqual(armed.failed, open.failed) || len(armed.failed) != 0 {
		t.Fatalf("failure rows differ: armed=%v, disarmed=%v — a deliberate suppression is not a "+
			"delivery failure (invariant 78)", armed.failed, open.failed)
	}
	if !reflect.DeepEqual(armed.appended, open.appended) {
		t.Fatalf("incident context differs: armed=%v, disarmed=%v — the ⚡ context note is a FACT about "+
			"the outage and does not depend on who pages (invariant 78)", armed.appended, open.appended)
	}
	if !reflect.DeepEqual(armed.correlated, open.correlated) {
		t.Fatalf("correlation differs: armed=%v, disarmed=%v (invariant 78)", armed.correlated, open.correlated)
	}
}

// FR-023 D7 / invariant 10: a SERVICE's escalation step is the owner's own page, so delivery-time
// delegation must not touch it — suppressing it would leave the outage with nobody told at all.
// The monitor case is asserted beside it, because the whole point is that ONE of the two changed.
//
// The second assertion is about a call that must NOT happen: a service step carries no monitor id,
// so a lookup could only fail, and the fail-open warning it logged would be an alarm about a
// question nobody asked.
func TestAServiceEscalationStepSkipsDelegationEntirely(t *testing.T) {
	step := func(a domain.EscalationStepAlert) []byte {
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	// A world where delegation WOULD suppress: the monitor is covered by an armed service.
	armed := func() *fakeStore {
		return &fakeStore{
			monitors: map[string]domain.Monitor{"m1": {ID: "m1", ProjectID: "p1", Name: "api-http"}},
			// keyed "monitor|signal", the shape the fake's ActiveDelegation reads
			delegation: map[string]store.DelegationVerdict{
				"m1|" + string(store.DelegationLive): {
					Owners: []store.DelegationOwner{{ServiceID: "svc1", Slug: "checkout", Name: "Checkout"}},
				},
			},
		}
	}

	// The MONITOR's step is suppressed — unchanged behaviour (NFR-018).
	mon := armed()
	mon.pending = []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicEscalationStep, Attempts: 1,
		Payload: step(domain.EscalationStepAlert{IncidentID: "inc1", MonitorID: "m1", MonitorName: "api-http",
			SubjectName: "api-http", Step: 0, ChannelIDs: []string{"c1"}})}}
	nfMon := &fakeNotify{}
	newWorker(mon, &fakeWebhook{}, nfMon, &fakeMetrics{}).drain(context.Background())
	if nfMon.called != 0 {
		t.Fatalf("a monitor's step was delivered while a service owns its paging: %d calls — that is "+
			"the no-double-page promise of §16.6b broken", nfMon.called)
	}

	// The SERVICE's own step is DELIVERED, and no monitor lookup is attempted for it.
	svc := armed()
	svc.pending = []domain.OutboxEvent{{ID: "e2", Topic: domain.TopicEscalationStep, Attempts: 1,
		Payload: step(domain.EscalationStepAlert{IncidentID: "inc2", ServiceID: "svc1",
			MonitorName: "Checkout", SubjectName: "Checkout", Step: 0, ChannelIDs: []string{"c1"}})}}
	nfSvc := &fakeNotify{}
	newWorker(svc, &fakeWebhook{}, nfSvc, &fakeMetrics{}).drain(context.Background())
	if nfSvc.called != 1 {
		t.Fatalf("a SERVICE's escalation step was not delivered (%d calls) — delegation exists so a "+
			"service can page INSTEAD of its members, and this step IS that page", nfSvc.called)
	}
	if len(svc.monitorLookups) != 0 {
		t.Fatalf("a service step looked up monitor(s) %v — it carries no monitor id, so the lookup "+
			"could only fail and log a fail-open nobody asked for (FR-023 D7)", svc.monitorLookups)
	}
	if len(svc.suppressRecords) != 0 {
		t.Fatalf("a service step recorded a suppression: %v", svc.suppressRecords)
	}

	// Invariant 11: instance-wide SILENCE mutes a service step exactly as it mutes a monitor's, and
	// the event is CONSUMED — a muted row left pending would be redelivered forever. Stated by
	// assertion rather than inherited by reading, because "the check happens earlier in the same
	// function" is an argument about code, not about behaviour.
	silenced := armed()
	silenced.pending = []domain.OutboxEvent{{ID: "e3", Topic: domain.TopicEscalationStep, Attempts: 1,
		Payload: step(domain.EscalationStepAlert{IncidentID: "inc3", ServiceID: "svc1",
			MonitorName: "Checkout", SubjectName: "Checkout", Step: 0, ChannelIDs: []string{"c1"}})}}
	nfSilenced := &fakeNotify{}
	newWorker(silenced, &fakeWebhook{}, nfSilenced, &fakeMetrics{}).
		WithSilence(func() bool { return true }).drain(context.Background())
	if nfSilenced.called != 0 {
		t.Fatalf("instance-wide silence did not mute a SERVICE step: %d calls", nfSilenced.called)
	}
	if len(silenced.delivered) != 1 {
		t.Fatalf("a muted service step was not consumed (%v) — it would be redelivered forever", silenced.delivered)
	}
}

// D-0177, after its first version was found to be neither causal nor a fence.
//
// The gate compared the event's sequence with the incident's CURRENT one and dropped the older. It
// dropped an opening that had never been delivered merely because a later fact existed — so a
// subscriber received the update, or the resolution, for an outage nobody had told them about — and
// it read that sequence BEFORE the delivery call, so two workers could still interleave around it.
// Ordering lives in the claim now, and the worker delivers what it is handed.
func TestAnUndeliveredOpeningIsNeverDroppedForBeingOld(t *testing.T) {
	fs := &fakeStore{incidentSeq: map[string]int64{"inc-1": 7}}
	wh := &fakeWebhook{}
	w := newWorker(fs, wh, &fakeNotify{}, &fakeMetrics{})

	opening, err := json.Marshal(domain.IncidentEvent{
		Type: domain.EventIncidentOpened, Seq: 1,
		Incident: domain.Incident{ID: "inc-1", ProjectID: "p1", Title: "down"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := w.deliver(context.Background(), domain.OutboxEvent{
		ID: "e1", Topic: domain.TopicIncidentEvent, Payload: opening,
	}); err != nil {
		t.Fatalf("deliver opening: %v", err)
	}
	if len(wh.got) != 1 || wh.got[0].Type != domain.EventIncidentOpened {
		t.Fatalf("the opening was dropped because later events exist: %+v — the subscriber would "+
			"receive an update for an outage nobody told them had begun", wh.got)
	}
}

// `New` documents metrics as optional, and the delegation path grew several unconditional
// `w.metrics.Record*` calls — so the contract was true of the constructor and false of the code, and
// the panic waited for the first SUPPRESSED event. That is to say, for production.
func TestAWorkerBuiltWithoutMetricsDeliversAndSuppresses(t *testing.T) {
	fs := &fakeStore{
		monitors: map[string]domain.Monitor{
			"m1": {ID: "m1", ProjectID: "p1", Name: "API", Status: domain.StatusDown, StateSequence: 1},
		},
		// A service actively covers this monitor, which is what drives the metrics calls.
		delegation: map[string]store.DelegationVerdict{
			"m1|live": {Owners: []store.DelegationOwner{{ServiceID: "svc1", Slug: "checkout", Name: "Checkout"}}},
		},
		pending: []domain.OutboxEvent{{
			ID: "e1", Topic: domain.TopicMonitorTransition,
			Payload: transitionSeq(t, "m1", domain.StatusUp, domain.StatusDown, 1, false), Attempts: 1,
		}},
	}
	// nil metrics — the documented case.
	w := New(fs, &fakeWebhook{}, &fakeNotify{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.drain(context.Background()) // must not panic
}
