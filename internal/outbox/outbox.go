// Package outbox delivers transactionally-queued events (incident webhooks,
// monitor-transition notifications) with retry and backoff. Events are enqueued
// in the same DB transaction as the state change that produced them (see store),
// so nothing is lost on a restart. A single worker — or several across replicas —
// claims due events with FOR UPDATE SKIP LOCKED, so delivery is at-least-once and
// safe to run everywhere the results pipeline runs.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

const (
	pollInterval = 2 * time.Second
	batchLimit   = 50
	// maxAttempts before an event is parked as dead for operator inspection.
	maxAttempts = 10
)

// Store is the persistence surface the worker needs.
type Store interface {
	ClaimDueOutbox(ctx context.Context, limit int) ([]domain.OutboxEvent, error)
	MarkOutboxDelivered(ctx context.Context, id string) error
	FailOutbox(ctx context.Context, id, lastErr string, maxAttempts int) error
	GetMonitor(ctx context.Context, id string) (domain.Monitor, error)
	// Incident-context heuristics (attached to opened auto-incidents).
	IncidentContext(ctx context.Context, inc domain.Incident) (domain.IncidentContext, error)
	AppendIncidentContext(ctx context.Context, incidentID, body string) (bool, error)
	// Dependency-graph suppression: (transitive) parents currently down.
	DownAncestors(ctx context.Context, monitorID string) ([]store.DownAncestor, error)
	AppendSuppressionNote(ctx context.Context, monitorID, rootName string) (bool, error)
}

// WebhookDeliverer delivers an incident event to a project's webhooks.
type WebhookDeliverer interface {
	Deliver(ctx context.Context, ev domain.IncidentEvent) error
}

// NotifyDeliverer delivers a monitor transition to the monitor's channels.
type NotifyDeliverer interface {
	Deliver(ctx context.Context, monitor domain.Monitor, up bool) error
	DeliverText(ctx context.Context, monitor domain.Monitor, text string) error
	DeliverProjectText(ctx context.Context, projectID, text string) error
	DeliverChannels(ctx context.Context, channelIDs []string, text string) error
}

// MailSender delivers a plain-text email (status-page subscription confirmations).
// Optional; when nil, subscriber-confirm events fail and eventually park as dead.
type MailSender interface {
	Send(to, subject, body string) error
}

// Metrics counts delivery outcomes. Optional and nil-safe.
type Metrics interface {
	RecordOutboxDelivered()
	RecordOutboxDead()
}

// Worker polls the outbox and delivers due events.
type Worker struct {
	store    Store
	webhooks WebhookDeliverer
	notifs   NotifyDeliverer
	mail     MailSender
	metrics  Metrics
	logger   *slog.Logger
	silenced func() bool // optional: when true, alert notifications are suppressed
}

// WithMailer sets the sender used to deliver subscriber-confirmation emails.
// Optional and nil-safe: without it, subscriber-confirm events park as dead.
func (w *Worker) WithMailer(m MailSender) *Worker {
	w.mail = m
	return w
}

// WithSilence gates alert notifications (monitor transitions + burn alerts) behind
// an instance-wide silence check. Incident webhooks and SLA reports are unaffected.
func (w *Worker) WithSilence(f func() bool) *Worker {
	w.silenced = f
	return w
}

// alertSilenced reports whether alert notifications are currently suppressed.
func (w *Worker) alertSilenced() bool { return w.silenced != nil && w.silenced() }

// New builds a worker. webhooks and notifs may be nil (their events then fail and
// eventually park as dead); metrics may be nil.
func New(st Store, webhooks WebhookDeliverer, notifs NotifyDeliverer, metrics Metrics, logger *slog.Logger) *Worker {
	return &Worker{store: st, webhooks: webhooks, notifs: notifs, metrics: metrics, logger: logger}
}

// Run polls until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

// drain claims and delivers due events until a claim comes back empty (so a
// backlog isn't rate-limited to one batch per tick) or the context ends.
func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := w.store.ClaimDueOutbox(ctx, batchLimit)
		if err != nil {
			w.logger.Error("outbox_claim_failed", "error", err.Error())
			return
		}
		for i := range events {
			w.process(ctx, events[i])
		}
		if len(events) < batchLimit {
			return
		}
	}
}

func (w *Worker) process(ctx context.Context, e domain.OutboxEvent) {
	err := w.deliver(ctx, e)
	if err == nil {
		if merr := w.store.MarkOutboxDelivered(ctx, e.ID); merr != nil {
			w.logger.Error("outbox_mark_delivered_failed", "id", e.ID, "error", merr.Error())
			return
		}
		if w.metrics != nil {
			w.metrics.RecordOutboxDelivered()
		}
		return
	}
	if ferr := w.store.FailOutbox(ctx, e.ID, err.Error(), maxAttempts); ferr != nil {
		w.logger.Error("outbox_fail_failed", "id", e.ID, "error", ferr.Error())
		return
	}
	if e.Attempts >= maxAttempts {
		w.logger.Error("outbox_dead", "id", e.ID, "topic", e.Topic, "attempts", e.Attempts, "error", err.Error())
		if w.metrics != nil {
			w.metrics.RecordOutboxDead()
		}
	} else {
		w.logger.Warn("outbox_delivery_retry", "id", e.ID, "topic", e.Topic, "attempt", e.Attempts, "error", err.Error())
	}
}

// dependencySuppressed reports whether a monitor's alert should stay quiet
// because a (transitive) dependency parent is down. Fail-open: a lookup error
// must never mute a real alert, so it logs and reports false. When suppressing,
// it best-effort annotates the child's open auto-incident once.
func (w *Worker) dependencySuppressed(ctx context.Context, monitor domain.Monitor) bool {
	if len(monitor.DependsOn) == 0 {
		return false
	}
	ancestors, err := w.store.DownAncestors(ctx, monitor.ID)
	if err != nil {
		w.logger.Warn("dependency_lookup_failed", "monitor_id", monitor.ID, "error", err.Error())
		return false
	}
	if len(ancestors) == 0 {
		return false
	}
	w.logger.Info("alert_suppressed_by_dependency",
		"monitor_id", monitor.ID, "monitor", monitor.Name, "root", ancestors[0].Name)
	if _, err := w.store.AppendSuppressionNote(ctx, monitor.ID, ancestors[0].Name); err != nil {
		w.logger.Warn("suppression_note_failed", "monitor_id", monitor.ID, "error", err.Error())
	}
	return true
}

// attachIncidentContext computes and posts the heuristic context summary for an
// opened auto-incident. Errors are logged, never propagated — the incident event
// itself was already delivered.
func (w *Worker) attachIncidentContext(ctx context.Context, inc domain.Incident) {
	ictx, err := w.store.IncidentContext(ctx, inc)
	if err != nil {
		w.logger.Warn("incident_context_failed", "incident", inc.ID, "error", err.Error())
		return
	}
	if ictx.Empty() {
		return
	}
	added, err := w.store.AppendIncidentContext(ctx, inc.ID, ictx.Render())
	if err != nil {
		w.logger.Warn("incident_context_append_failed", "incident", inc.ID, "error", err.Error())
		return
	}
	if added {
		w.logger.Info("incident_context_attached", "incident", inc.ID,
			"co_failures", ictx.CoFailureTotal, "class", ictx.DominantClass, "region", ictx.Region)
	}
}

// deliver dispatches one event by topic. A nil error means the event is done.
func (w *Worker) deliver(ctx context.Context, e domain.OutboxEvent) error {
	switch e.Topic {
	case domain.TopicIncidentEvent:
		if w.webhooks == nil {
			return errors.New("no webhook deliverer configured")
		}
		var ev domain.IncidentEvent
		if err := json.Unmarshal(e.Payload, &ev); err != nil {
			return err
		}
		if err := w.webhooks.Deliver(ctx, ev); err != nil {
			return err
		}
		// Heuristic RCA context for freshly-opened auto-incidents: correlated
		// co-failures, dominant error class, single-region hint — posted once as
		// a system timeline update. Best-effort: a context failure never fails
		// the (already delivered) event; the append is idempotent, so a retry
		// after a partial failure can't double-post.
		if ev.Type == domain.EventIncidentOpened && ev.Incident.Source == domain.SourceAuto && ev.Incident.MonitorID != "" {
			w.attachIncidentContext(ctx, ev.Incident)
		}
		return nil

	case domain.TopicMonitorTransition:
		var mt domain.MonitorTransition
		if err := json.Unmarshal(e.Payload, &mt); err != nil {
			return err
		}
		if !mt.ShouldNotify() {
			return nil // policy: this transition isn't notified — done, not an error
		}
		if w.alertSilenced() {
			return nil // instance-wide silence: suppress the notification (no-op, delivered)
		}
		if w.notifs == nil {
			return errors.New("no notification deliverer configured")
		}
		monitor, err := w.store.GetMonitor(ctx, mt.MonitorID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // monitor deleted since enqueue — nothing to notify
		}
		if err != nil {
			return err
		}
		// A monitor with an escalation policy has its DOWN alerts driven by the on-call
		// ladder (over its open auto-incident), so suppress the flat down-notify to
		// avoid double-alerting. Recovery (up) still goes to all channels.
		if monitor.EscalationPolicyID != "" && monitor.AutoIncident && mt.Cur == domain.StatusDown {
			return nil
		}
		// Dependency-graph suppression: while a (transitive) parent is down, the
		// child's DOWN alerts (and reminders) stay quiet — the parent's alert names
		// the root cause. Recovery is never suppressed. Facts keep recording; only
		// delivery is muted, plus a one-time timeline note on the child's incident.
		if mt.Cur == domain.StatusDown && w.dependencySuppressed(ctx, monitor) {
			return nil
		}
		return w.notifs.Deliver(ctx, monitor, mt.Cur == domain.StatusUp)

	case domain.TopicSLOBurnAlert:
		var a domain.SLOBurnAlert
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			return err
		}
		if w.alertSilenced() {
			return nil // instance-wide silence: suppress the burn alert (no-op, delivered)
		}
		if w.notifs == nil {
			return errors.New("no notification deliverer configured")
		}
		monitor, err := w.store.GetMonitor(ctx, a.MonitorID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // monitor deleted since enqueue — nothing to notify
		}
		if err != nil {
			return err
		}
		// A burn alert firing while a dependency ancestor is down is the cascade
		// speaking — suppress it like the down-notify. Recovery passes through.
		if a.Firing && w.dependencySuppressed(ctx, monitor) {
			return nil
		}
		return w.notifs.DeliverText(ctx, monitor, a.Message(monitor.Name))

	case domain.TopicSLAReport:
		var rep domain.SLAReport
		if err := json.Unmarshal(e.Payload, &rep); err != nil {
			return err
		}
		if w.notifs == nil {
			return errors.New("no notification deliverer configured")
		}
		return w.notifs.DeliverProjectText(ctx, rep.ProjectID, rep.Message())

	case domain.TopicRegionWorkerAlert:
		var a domain.RegionWorkerAlert
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			return err
		}
		if w.alertSilenced() {
			return nil // instance-wide silence: suppress the region alert (no-op, delivered)
		}
		if w.notifs == nil {
			return errors.New("no notification deliverer configured")
		}
		return w.notifs.DeliverProjectText(ctx, a.ProjectID, a.Message())

	case domain.TopicEscalationStep:
		var a domain.EscalationStepAlert
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			return err
		}
		if w.alertSilenced() {
			return nil // instance-wide silence: suppress the escalation step (no-op, delivered)
		}
		if w.notifs == nil {
			return errors.New("no notification deliverer configured")
		}
		return w.notifs.DeliverChannels(ctx, a.ChannelIDs, a.Message())

	case domain.TopicSubscriberConfirm:
		var sc domain.SubscriberConfirm
		if err := json.Unmarshal(e.Payload, &sc); err != nil {
			return err
		}
		if w.mail == nil {
			return errors.New("no mailer configured")
		}
		// A confirmation email is transactional, not an alert — instance-wide
		// alert silence does not suppress it.
		return w.mail.Send(sc.To, sc.Subject, sc.Body)

	default:
		return errors.New("unknown outbox topic: " + e.Topic)
	}
}
