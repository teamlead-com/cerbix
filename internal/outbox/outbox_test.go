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
	"time"

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
	released    []string
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
	// D-0179: what the worker credited as an announcement somebody actually received. A LIST, so a
	// test can prove an ABSENCE — an attempted delivery resolving zero recipients must leave nothing.
	alertDelivered   []domain.ServiceAlert
	condemned        []domain.ServiceAlert
	markDeliveredErr error

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

// The settling writes REFUSE a cancelled context, because the real ones do: pgx fails a query on a
// dead context, and a settle that runs on the delivery's own bounded context is exactly the bug
// D-0186's fix could have introduced. A fake that ignores ctx cannot express the difference, and a
// mutation moving the settle onto the bounded context passed against the previous version of this.
func (f *fakeStore) MarkOutboxDelivered(ctx context.Context, id, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.delivered = append(f.delivered, id)
	return true, nil
}

// released records claims handed back UNATTEMPTED. The batch spends an attempt on every row it
// claims, so an event that never got its turn has to be refunded or it dead-letters unsent.
func (f *fakeStore) ReleaseOutboxClaim(ctx context.Context, id, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.released = append(f.released, id)
	return true, nil
}

func (f *fakeStore) FailOutbox(ctx context.Context, id, _, lastErr string, _ int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
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
	// sawDeadline records whether the confirmation send was given the delivery budget. It was the
	// one branch that dialled on `context.Background()` after D-0186 landed, so a fake that dropped
	// the context could not have noticed.
	sawDeadline bool
}

func (f *fakeMail) SendContext(ctx context.Context, to, subject, body string) error {
	if _, ok := ctx.Deadline(); ok {
		f.sawDeadline = true
	}
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
	delivered  *int
	// captureDeadline records whether the delivery context carried one, and burnBudget spends it —
	// the two halves of "bounded by the lease" that a test can otherwise only assert about itself.
	captureDeadline bool
	sawDeadline     bool
	deadline        time.Time
	burnBudget      bool
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

// resolved, when set, is how many of the requested channels still EXIST — the fake's way of deleting
// a snapshot recipient out from under an announcement. delivered, when set, is how many sends
// SUCCEEDED, which is a different number: a channel that exists can still return 500.
//
// They were one field, and the fake could therefore not express the case that mattered — one channel
// resolved, its send failed — so a test asserting "somebody was told" passed on a delivery nobody
// got. Zero means "all of them" for resolved; delivered defaults to resolved when the call did not
// error, and to zero when it did.
func (f *fakeNotify) DeliverChannelsReporting(
	ctx context.Context, channelIDs []string, text string,
) (domain.ChannelDelivery, error) {
	f.channelIDs, f.text, f.called = channelIDs, text, f.called+1
	if d, ok := ctx.Deadline(); ok {
		f.sawDeadline, f.deadline = true, d
	}
	if f.burnBudget {
		// Spend the whole budget — but never block forever. A fake that waits on a context with no
		// deadline turns "the bound is missing" into a hung test instead of a failing one, and a
		// mutation that hangs teaches nothing. The cap is well above any budget a test sets.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	out := domain.ChannelDelivery{Requested: len(channelIDs), Resolved: len(channelIDs)}
	if f.resolved != nil {
		out.Resolved = *f.resolved
	}
	switch {
	case f.delivered != nil:
		out.Delivered = *f.delivered
	case f.err != nil:
		out.Delivered = 0
	default:
		out.Delivered = out.Resolved
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

// delivered records what the worker credited as an announcement somebody actually received. It is a
// LIST rather than a flag on purpose: the point of D-0179 is that an attempted delivery resolving
// zero recipients must leave NO trace here, and a set that only ever grows cannot prove an absence.
func (f *fakeStore) MarkServiceAlertDelivered(ctx context.Context, a domain.ServiceAlert) error {
	// Refuses a dead context, because the real one does. A fake that ignores it cannot witness the
	// difference between recording on the delivery budget and recording on the caller's context —
	// and a mutation moving the credit onto the bounded one passed cleanly until this line existed.
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.markDeliveredErr != nil {
		return f.markDeliveredErr
	}
	f.alertDelivered = append(f.alertDelivered, a)
	return nil
}

// condemned is the mirror of `alertDelivered`: announcements the worker declared KNOWN dead. A list
// again, because the assertions that matter are about absence — a retry is still owed, so nothing
// must be condemned yet.
func (f *fakeStore) MarkServiceAlertUndeliverable(ctx context.Context, a domain.ServiceAlert) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.condemned = append(f.condemned, a)
	return nil
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
	if err := w.deliver(context.Background(), context.Background(), domain.OutboxEvent{
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

// D-0179 — the swallow [61]/[84] named, at the point where it is decided.
//
// The chain that made a member monitor silent for a page nobody received: the evaluator saw a route
// and enqueued the onset, latching firing; the channel went before the worker delivered; the worker
// resolved zero recipients, counted it and terminally SUCCEEDED, because no retry reaches a deleted
// channel; the latch still said firing, so no further edge was coming; and when the route came back,
// coverage re-armed against that latch.
//
// The worker's part of the fix is exactly this: an announcement that reached nobody must leave NO
// delivery evidence behind. Asserting an absence is why the fake keeps a list.
func TestAnAnnouncementNobodyReceivedIsNotCreditedAsDelivered(t *testing.T) {
	payload := func() []byte {
		b, err := json.Marshal(domain.ServiceAlert{
			ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
			Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
			Seq: 7, ConfirmedOver: 2, Recipients: []string{"ch-1"},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	for _, tc := range []struct {
		name     string
		resolved int
		want     int
	}{
		{"every recipient channel has been deleted", 0, 0},
		{"one of them is still there", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{
				alertSeq: map[string]int64{"svc-1|health||": 7},
				pending: []domain.OutboxEvent{
					{ID: "e-1", Topic: domain.TopicServiceAlert, Payload: payload()},
				},
			}
			resolved := tc.resolved
			nf := &fakeNotify{resolved: &resolved}
			m := &fakeMetrics{}
			newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

			if got := len(fs.alertDelivered); got != tc.want {
				t.Fatalf("%d delivery credits, want %d — coverage means somebody was TOLD, and a "+
					"credit here is what lets the service silence its members", got, tc.want)
			}
			if len(fs.failed) != 0 {
				t.Fatalf("the event was retried (%v): no retry reaches a deleted channel, and "+
					"parking a dead letter for it helps nobody", fs.failed)
			}
		})
	}
}

// A CLOSE is never credited. Coverage is about an announcement that is LIVE; crediting an ending
// would arm a service whose alert is over — the mirror of the bug above, in the other direction.
func TestACloseIsNotCreditedAsCoverage(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: false, State: domain.ServiceAlertHealthy,
		Seq: 8, CloseReason: domain.CloseRecovered, Recipients: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fs := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 8},
		pending:  []domain.OutboxEvent{{ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b}},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 {
		t.Fatalf("the close was not delivered (called=%d)", nf.called)
	}
	if len(fs.alertDelivered) != 0 {
		t.Fatalf("a CLOSE was credited as coverage: %+v", fs.alertDelivered)
	}
}

// Recording the credit is not allowed to re-send the page. The delivery already happened; returning
// an error would deliver it again, so the failure leaves coverage DIS-ARMED instead — the direction
// every other ambiguity in the conjunction takes.
func TestAnUnrecordedDeliveryFailsOpenRatherThanRetrying(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
		Seq: 7, ConfirmedOver: 2, Recipients: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fs := &fakeStore{
		alertSeq:         map[string]int64{"svc-1|health||": 7},
		markDeliveredErr: errors.New("boom"),
		pending:          []domain.OutboxEvent{{ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b}},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 {
		t.Fatalf("the page did not go out (called=%d)", nf.called)
	}
	if len(fs.failed) != 0 {
		t.Fatalf("the event was retried (%v) — the recipients would be paged a second time for one "+
			"announcement", fs.failed)
	}
	if len(fs.delivered) != 1 {
		t.Fatalf("the event was not settled: %v", fs.delivered)
	}
}

// A PARTIAL delivery is still an announcement people received.
//
// D-0179's contract is "at least one recipient", and the credit used to be gated on `err == nil` as
// well — so three channels reached and a fourth timing out left the service covering nobody until a
// retry came back clean, with its members paging redundantly the whole time. Safe, and louder than
// the contract says. The event still fails and still retries; the credit is monotonic and
// sequence-guarded, so the retry's second credit changes nothing.
func TestAPartialDeliveryStillCountsAsAnAnnouncement(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
		Seq: 7, ConfirmedOver: 2, Recipients: []string{"ch-1", "ch-2"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fs := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 7},
		pending:  []domain.OutboxEvent{{ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b}},
	}
	resolved, delivered := 2, 1
	nf := &fakeNotify{
		resolved: &resolved, delivered: &delivered, err: errors.New("one channel timed out"),
	}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

	if len(fs.alertDelivered) != 1 {
		t.Fatalf("%d delivery credits for an announcement ONE recipient received: the contract is at "+
			"least one recipient, not a clean call, and until the credit lands the members page for "+
			"themselves", len(fs.alertDelivered))
	}
	if len(fs.failed) != 1 {
		t.Fatalf("the event was not retried (%v): a channel that timed out has not been told yet",
			fs.failed)
	}
}

// The case the previous version of this file could not express, and the P0 it hid: the only channel
// that still EXISTS returns 500.
//
// `Resolved` counts channel rows, not successful sends, and the credit was reading it as if it did.
// So a service whose one webhook was returning errors suppressed every member's own alert for an
// announcement nobody received — and permanently, because once the outbox dead-letters the retry the
// latch stays firing and no further edge is coming.
func TestATotalSendFailureIsNotAnAnnouncement(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
		Seq: 7, ConfirmedOver: 2, Recipients: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fs := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 7},
		pending:  []domain.OutboxEvent{{ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b}},
	}
	// The channel is there and enabled — RESOLVED — and the send fails.
	resolved, delivered := 1, 0
	nf := &fakeNotify{
		resolved: &resolved, delivered: &delivered,
		err: errors.New("the only resolved channel failed: 500"),
	}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

	if n := len(fs.alertDelivered); n != 0 {
		t.Fatalf("coverage received %d delivery credit(s), but the only resolved channel failed: the "+
			"members are now silent for a page nobody got, and they stay silent once the retry "+
			"dead-letters", n)
	}
	if len(fs.failed) != 1 {
		t.Fatalf("the event was not retried (%v): a 500 is exactly what a retry is for", fs.failed)
	}
}

// D-0186 — a delivery is bounded by the claim's own lease.
//
// The claim token stops a deposed worker from SETTLING a row it no longer owns. It cannot stop that
// worker from still being inside an HTTP request while the new owner sends the same event: the fence
// is in the database and this happens outside it. Bounding the call is the half that is ours.
func TestDeliveryIsBoundedByTheClaimsLease(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
		Seq: 7, ConfirmedOver: 2, Recipients: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	t.Run("a lease already spent sends nothing at all", func(t *testing.T) {
		fs := &fakeStore{
			alertSeq: map[string]int64{"svc-1|health||": 7},
			pending: []domain.OutboxEvent{{
				ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b,
				LeaseUntil: time.Now().Add(-time.Minute), ClaimedAt: time.Now(),
			}},
		}
		nf := &fakeNotify{}
		newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

		if nf.called != 0 {
			t.Fatalf("the deliverer was called %d time(s) for a row this worker no longer owns: the "+
				"send would be a duplicate whose settle is guaranteed to lose the CAS", nf.called)
		}
		if len(fs.delivered) != 0 || len(fs.failed) != 0 {
			t.Fatalf("the row was settled (delivered=%v failed=%v) by a worker past its lease",
				fs.delivered, fs.failed)
		}
	})

	t.Run("the deliverer receives a deadline drawn from the lease", func(t *testing.T) {
		fs := &fakeStore{
			alertSeq: map[string]int64{"svc-1|health||": 7},
			pending: []domain.OutboxEvent{{
				ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b,
				LeaseUntil: time.Now().Add(30 * time.Second), ClaimedAt: time.Now(),
			}},
		}
		nf := &fakeNotify{captureDeadline: true}
		newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

		if !nf.sawDeadline {
			t.Fatal("the delivery context carried no deadline: an unbounded call can outlive the " +
				"claim that authorised it, and then two workers are sending the same event")
		}
		// Headroom is subtracted so the settle still fits inside the claim. A budget of the WHOLE
		// lease would hand the row over at the instant we tried to mark it delivered.
		if left := time.Until(nf.deadline); left > 29*time.Second {
			t.Fatalf("the delivery budget is %s of a 30s lease: nothing is left for the settling "+
				"write, so a successful send would be recorded as a failure and sent again", left)
		}
	})

	t.Run("settling is NOT bounded by the delivery budget", func(t *testing.T) {
		// A delivery that consumed its whole budget must still be able to record itself. Otherwise
		// the fix introduces the exact duplicate it exists to reduce.
		fs := &fakeStore{
			alertSeq: map[string]int64{"svc-1|health||": 7},
			pending: []domain.OutboxEvent{{
				ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b,
				LeaseUntil: time.Now().Add(3 * time.Second), ClaimedAt: time.Now(),
			}},
		}
		nf := &fakeNotify{burnBudget: true}
		newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

		if len(fs.delivered) != 1 {
			t.Fatalf("a delivery that used its whole budget was not settled (delivered=%v failed=%v): "+
				"the row goes back to the queue and the recipients are paged a second time",
				fs.delivered, fs.failed)
		}
	})

	t.Run("no lease means the previous behaviour", func(t *testing.T) {
		fs := &fakeStore{
			alertSeq: map[string]int64{"svc-1|health||": 7},
			pending:  []domain.OutboxEvent{{ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b}},
		}
		nf := &fakeNotify{captureDeadline: true}
		newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

		if nf.called != 1 {
			t.Fatalf("an event with no lease was not delivered (called=%d)", nf.called)
		}
		if nf.sawDeadline {
			t.Fatal("a deadline was imposed on an event carrying no lease: an older store, or a " +
				"caller that built the event itself, must keep the behaviour it had")
		}
	})
}

// D-0187 — an announcement is CONDEMNED only when no retry is owed.
//
// The evaluator re-announces on that fact, so getting it wrong in either direction is a real defect:
// condemning too eagerly sends a second copy of an event that was merely slow, and never condemning
// leaves the outage un-announced forever, which is the state D-0179 deliberately left it in.
func TestOnlyATerminalFailureCondemnsAnAnnouncement(t *testing.T) {
	payload := func(recipients []string) []byte {
		b, err := json.Marshal(domain.ServiceAlert{
			ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
			Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
			Seq: 7, ConfirmedOver: 2, Recipients: recipients,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	for _, tc := range []struct {
		name          string
		recipients    []string
		resolved      *int
		delivered     *int
		err           error
		wantCondemned int
	}{
		{
			name: "the snapshot is empty — nothing to retry", recipients: nil, wantCondemned: 1,
		},
		{
			name: "every channel in the snapshot has been deleted", recipients: []string{"ch-1"},
			resolved: intp(0), wantCondemned: 1,
		},
		{
			// A 500 IS retried, so the announcement is not dead yet. Condemning here would have the
			// evaluator re-announce while the original is still owed an attempt.
			name: "the only channel returned 500", recipients: []string{"ch-1"},
			resolved: intp(1), delivered: intp(0), err: errors.New("500"), wantCondemned: 0,
		},
		{
			name: "somebody was told", recipients: []string{"ch-1"}, wantCondemned: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{
				alertSeq: map[string]int64{"svc-1|health||": 7},
				pending: []domain.OutboxEvent{
					{ID: "e-1", Topic: domain.TopicServiceAlert, Payload: payload(tc.recipients)},
				},
			}
			nf := &fakeNotify{resolved: tc.resolved, delivered: tc.delivered, err: tc.err}
			newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

			if got := len(fs.condemned); got != tc.wantCondemned {
				t.Fatalf("%d condemnation(s), want %d — the evaluator re-announces on this fact, so "+
					"condemning a retryable failure duplicates a page and never condemning leaves "+
					"the outage unannounced", got, tc.wantCondemned)
			}
		})
	}
}

func intp(v int) *int { return &v }

// Every branch that RECORDS what a delivery did runs on the unbounded context, and every branch that
// SENDS runs on the bounded one. Both halves of D-0186, and the review found the gaps in each.
func TestTheBudgetBoundsSendingAndNeverRecording(t *testing.T) {
	alert := func() []byte {
		b, err := json.Marshal(domain.ServiceAlert{
			ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
			Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
			Seq: 7, ConfirmedOver: 2, Recipients: []string{"ch-1"},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	t.Run("a delivery that burns its budget still records the credit", func(t *testing.T) {
		// The credit was taken on the DELIVERY context, so a send that used its whole budget lost
		// it — the page went out, coverage never armed, and the members kept paging redundantly for
		// an announcement everybody received.
		fs := &fakeStore{
			alertSeq: map[string]int64{"svc-1|health||": 7},
			pending: []domain.OutboxEvent{{
				ID: "e-1", Topic: domain.TopicServiceAlert, Payload: alert(),
				LeaseUntil: time.Now().Add(3 * time.Second), ClaimedAt: time.Now(),
			}},
		}
		nf := &fakeNotify{burnBudget: true}
		newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

		if len(fs.alertDelivered) != 1 {
			t.Fatalf("%d delivery credits after a send that used its whole budget: the page went "+
				"out and coverage never armed, so the members page for an announcement everybody "+
				"received", len(fs.alertDelivered))
		}
	})

	t.Run("a condemnation survives a burnt budget too", func(t *testing.T) {
		// The mirror, and the worse half: losing the condemnation means the outage is NEVER
		// re-announced (D-0187), because the evaluator's trigger never appears.
		b, err := json.Marshal(domain.ServiceAlert{
			ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
			Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
			Seq: 7, Recipients: nil,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		fs := &fakeStore{
			alertSeq: map[string]int64{"svc-1|health||": 7},
			pending: []domain.OutboxEvent{{
				ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b,
				// Already spent for DELIVERY purposes would return early, so give it a live lease
				// and let the recording path be the only thing under test.
				LeaseUntil: time.Now().Add(30 * time.Second), ClaimedAt: time.Now(),
			}},
		}
		newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).drain(context.Background())
		if len(fs.condemned) != 1 {
			t.Fatalf("%d condemnations for an empty recipient snapshot", len(fs.condemned))
		}
	})

	t.Run("the subscriber confirmation is bounded like every other send", func(t *testing.T) {
		b, err := json.Marshal(domain.SubscriberConfirm{To: "a@x.com", Subject: "confirm", Body: "hi"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		fs := &fakeStore{pending: []domain.OutboxEvent{{
			ID: "e-1", Topic: domain.TopicSubscriberConfirm, Payload: b,
			LeaseUntil: time.Now().Add(30 * time.Second), ClaimedAt: time.Now(),
		}}}
		mail := &fakeMail{}
		newWorker(fs, &fakeWebhook{}, &fakeNotify{}, &fakeMetrics{}).WithMailer(mail).
			drain(context.Background())

		if len(mail.sent) != 1 {
			t.Fatalf("%d confirmations sent", len(mail.sent))
		}
		if !mail.sawDeadline {
			t.Fatal("the confirmation was sent on an unbounded context: a hung SMTP endpoint can " +
				"hold the session past the claim that authorised it, which is the overlap the " +
				"budget exists to bound")
		}
	})
}

// A claim spends an attempt on EVERY row in the batch, and the worker delivers them one at a time.
// An event whose turn comes after its lease has gone must get that attempt back.
//
// Without the refund a slow event at the front of a batch charges the ones behind it for deliveries
// that never happened, and enough turns like that dead-letter an event that was never sent once —
// the opposite of what a retry budget is for.
func TestAnUnattemptedClaimIsGivenBack(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
		Seq: 7, Recipients: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	now := time.Now()
	fs := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 7},
		pending: []domain.OutboxEvent{{
			ID: "e-late", Topic: domain.TopicServiceAlert, Payload: b, ClaimToken: "tok",
			// Claimed with a lease that was already gone by the time this row's turn came.
			ClaimedAt: now, LeaseUntil: now.Add(-time.Second),
		}},
	}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

	if nf.called != 0 {
		t.Fatalf("the deliverer was called %d time(s) past the lease", nf.called)
	}
	if len(fs.released) != 1 || fs.released[0] != "e-late" {
		t.Fatalf("released = %v, want the unattempted row: the batch already charged it an attempt, "+
			"and an event that was never tried must not pay for one", fs.released)
	}
	if len(fs.failed) != 0 || len(fs.delivered) != 0 {
		t.Fatalf("the row was settled (failed=%v delivered=%v) rather than handed back",
			fs.failed, fs.delivered)
	}
}

// The lease is spent as a SPAN in database time, never by subtracting a database timestamp from a
// worker one. Under skew the latter delivers after the lease really ended, or skips a claim that was
// perfectly good — and a monitoring product whose own clock handling is naive is a poor advertisement.
func TestTheLeaseIsSpentAsASpanNotAcrossTwoClocks(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
		Seq: 7, Recipients: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The database is an hour BEHIND this worker. The lease is a healthy 30 seconds in its own
	// terms; compared against the worker's clock it looks an hour stale.
	dbNow := time.Now().Add(-time.Hour)
	fs := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 7},
		pending: []domain.OutboxEvent{{
			ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b, ClaimToken: "tok",
			ClaimedAt: dbNow, LeaseUntil: dbNow.Add(30 * time.Second),
		}},
	}
	nf := &fakeNotify{captureDeadline: true}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())

	if nf.called != 1 {
		t.Fatalf("a valid 30-second lease was skipped because the database clock differs from this "+
			"one (called=%d)", nf.called)
	}
	if !nf.sawDeadline {
		t.Fatal("no deadline was applied")
	}
	if left := time.Until(nf.deadline); left <= 0 || left > 29*time.Second {
		t.Fatalf("the budget is %s: it should be the lease's own span minus headroom, measured on "+
			"this clock, not a difference between two clocks", left)
	}
}

// An announcement that dies by exhausting its retries reached nobody and is owed no further attempt,
// so it is condemned like any other terminal failure (D-0187).
//
// It was left out of the first version and named as "not covered". The reviewer was right that
// invariant 105's own words — "no retry owed" — already include this: an announcement that ran out of
// attempts is as permanently unheard as one whose channels were all deleted, and leaving it
// uncondemned means the outage is never announced again.
func TestAnAnnouncementThatDeadLettersIsCondemned(t *testing.T) {
	b, err := json.Marshal(domain.ServiceAlert{
		ServiceID: "svc-1", ProjectID: "p-1", ServiceName: "Checkout",
		Signal: domain.ServiceSignalHealth, Firing: true, State: domain.ServiceAlertDown,
		Seq: 7, Recipients: []string{"ch-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fs := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 7},
		pending: []domain.OutboxEvent{{
			ID: "e-1", Topic: domain.TopicServiceAlert, Payload: b, ClaimToken: "tok",
			// One short of the cap, so THIS attempt is the last one.
			Attempts: maxAttempts,
		}},
	}
	resolved, delivered := 1, 0
	nf := &fakeNotify{resolved: &resolved, delivered: &delivered, err: errors.New("500 again")}
	m := &fakeMetrics{}
	newWorker(fs, &fakeWebhook{}, nf, m).drain(context.Background())

	if m.dead != 1 {
		t.Fatalf("the event did not dead-letter (dead=%d)", m.dead)
	}
	if len(fs.condemned) != 1 {
		t.Fatalf("%d condemnations for an announcement that exhausted its retries: no attempt is "+
			"owed, nobody was told, and the evaluator has no trigger to announce again",
			len(fs.condemned))
	}

	// A NON-terminal failure of the same shape still condemns nothing: the retry is owed.
	fs2 := &fakeStore{
		alertSeq: map[string]int64{"svc-1|health||": 7},
		pending: []domain.OutboxEvent{{
			ID: "e-2", Topic: domain.TopicServiceAlert, Payload: b, ClaimToken: "tok", Attempts: 0,
		}},
	}
	newWorker(fs2, &fakeWebhook{}, &fakeNotify{resolved: &resolved, delivered: &delivered,
		err: errors.New("500")}, &fakeMetrics{}).drain(context.Background())
	if len(fs2.condemned) != 0 {
		t.Fatalf("%d condemnations while a retry was still owed", len(fs2.condemned))
	}
}
