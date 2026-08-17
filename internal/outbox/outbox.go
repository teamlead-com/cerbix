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
	MarkOutboxDelivered(ctx context.Context, id, claimToken string) (applied bool, err error)
	FailOutbox(ctx context.Context, id, claimToken, lastErr string, maxAttempts int) (applied bool, err error)
	GetMonitor(ctx context.Context, id string) (domain.Monitor, error)
	// Incident-context heuristics (attached to opened auto-incidents).
	IncidentContext(ctx context.Context, inc domain.Incident) (domain.IncidentContext, error)
	AppendIncidentContext(ctx context.Context, incidentID, body string) (bool, error)
	// Dependency-graph suppression: (transitive) parents currently down.
	DownAncestors(ctx context.Context, monitorID string) ([]store.DownAncestor, error)
	AppendSuppressionNote(ctx context.Context, monitorID, rootName string) (bool, error)
	// Maintenance suppression: is the monitor inside an active window right now.
	MonitorInMaintenance(ctx context.Context, monitorID string) (bool, error)
	// Service impact correlation (FR-021 §14.3): the one-transaction attempt.
	// Returns the NEWLY inserted links plus the witness-overflow count of the
	// [278] bound; a redelivery inserts none.
	CorrelateIncident(ctx context.Context, incidentID string) ([]domain.ServiceImpactLink, int, error)
	// Alerting ownership (FR-021 §16.1): whether a service ACTIVELY covers this signal for this
	// monitor — armed, quotable, routable, generation-matched and fresh. An error here must page.
	ActiveDelegation(ctx context.Context, monitorID, projectID string, signal store.DelegationSignal) (store.DelegationVerdict, error)
	ServiceAlertSequence(ctx context.Context, a domain.ServiceAlert) (int64, error)
	RecordSuppression(ctx context.Context, eventID, monitorID, projectID, topic string, owners []store.DelegationOwner) error
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
	// Impact correlation (FR-021 §14.6): links counted on INSERTED rows only;
	// failures count failed attempts (each also rides the outbox retry);
	// witness overflow counts incidents beyond the [278] per-service bound —
	// a durable-fact omission that must never be silent.
	RecordImpactLinks(role string, n int)
	RecordImpactFailure()
	// Alerting-ownership telemetry (FR-021 §16.6b): a suppressed delivery leaves no other trace,
	// and a fail-open is the state in which members page for themselves.
	RecordAlertSuppressed(topic, reason string)
	RecordDelegationFailOpen(reason string)
	RecordImpactWitnessOverflow(n int)
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
		applied, merr := w.store.MarkOutboxDelivered(ctx, e.ID, e.ClaimToken)
		if merr != nil {
			w.logger.Error("outbox_mark_delivered_failed", "id", e.ID, "error", merr.Error())
			return
		}
		if !applied {
			// Lost the claim_token CAS: another worker re-claimed this row (it will deliver
			// it). We already sent — a duplicate delivery, but not OUR counted delivery.
			w.logger.Warn("outbox_cas_lost", "id", e.ID, "phase", "delivered")
			return
		}
		if w.metrics != nil {
			w.metrics.RecordOutboxDelivered()
		}
		w.logger.Info("outbox_delivered", "id", e.ID, "topic", e.Topic, "attempt", e.Attempts+1)
		return
	}
	applied, ferr := w.store.FailOutbox(ctx, e.ID, e.ClaimToken, err.Error(), maxAttempts)
	if ferr != nil {
		w.logger.Error("outbox_fail_failed", "id", e.ID, "error", ferr.Error())
		return
	}
	if !applied {
		w.logger.Warn("outbox_cas_lost", "id", e.ID, "phase", "failed")
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

// serviceDelegationSuppressed reports whether a monitor's ONSET-like alert should stay quiet because
// a service actively covers that signal (FR-021 §16.1).
//
// Three properties, each of which is a finding from the design rounds:
//
//   - it is called ONLY for onset-like events. A recovery or a burn CLEAR is never suppressed, because
//     arming changes between an onset and its close: a DOWN can fail-open while the service is still
//     catching up, and muting the matching UP would leave whoever was paged holding an alert that can
//     never end.
//   - it FAILS OPEN. Any error, any ambiguity, delivers — and says why, with a counted reason.
//   - the suppression RECORD is written in the same scoped store operation that resolved the owners,
//     and a failure there also fails open: a suppression nobody can see is worse than a duplicate page.
func (w *Worker) serviceDelegationSuppressed(
	ctx context.Context, eventID string, monitor domain.Monitor, signal store.DelegationSignal, topic string,
) bool {
	v, err := w.store.ActiveDelegation(ctx, monitor.ID, monitor.ProjectID, signal)
	if err != nil {
		w.logger.Warn("delegation_lookup_failed", "monitor_id", monitor.ID,
			"signal", string(signal), "error", err.Error())
		w.metrics.RecordDelegationFailOpen("error")
		return false
	}
	if !v.Suppress() {
		w.metrics.RecordDelegationFailOpen(v.FailOpenReason)
		return false
	}
	if err := w.store.RecordSuppression(ctx, eventID, monitor.ID, monitor.ProjectID, topic, v.Owners); err != nil {
		// The visibility path is not optional: if the suppression cannot be recorded and explained,
		// it does not happen.
		w.logger.Warn("suppression_record_failed", "monitor_id", monitor.ID,
			"topic", topic, "error", err.Error())
		w.metrics.RecordDelegationFailOpen("record_failed")
		return false
	}
	w.logger.Info("alert_suppressed_by_service", "monitor_id", monitor.ID, "monitor", monitor.Name,
		"topic", topic, "owner", v.Owners[0].Name, "owners", len(v.Owners))
	w.metrics.RecordAlertSuppressed(topic, string(domain.SuppressionServiceDelegation))
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
	case domain.TopicIncidentCorrelation:
		// FR-021 §14.3: the impact-graph correlation attempt, on its OWN topic —
		// never a rider on incident_event, so webhook death never blocks it and
		// its failure never blocks incident delivery. The store method is one
		// transaction (locks, open recheck, links + 🕸 notes) and fully idempotent,
		// so the outbox's at-least-once redelivery is safe. NOTE: this topic is
		// FENCED (domain.FencedTopic) — see the claim fence in store.ClaimDueOutbox.
		var corr domain.IncidentCorrelation
		if err := json.Unmarshal(e.Payload, &corr); err != nil {
			return err
		}
		links, overflow, err := w.store.CorrelateIncident(ctx, corr.IncidentID)
		if err != nil {
			if w.metrics != nil {
				w.metrics.RecordImpactFailure()
			}
			return err
		}
		if overflow > 0 {
			// The [278] bound bit: incidents beyond the per-service witness cap were
			// deterministically not selected. Counted and logged — never silent.
			if w.metrics != nil {
				w.metrics.RecordImpactWitnessOverflow(overflow)
			}
			w.logger.Warn("incident_impact_witness_overflow", "incident", corr.IncidentID, "dropped", overflow)
		}
		if len(links) > 0 {
			perRole := map[string]int{}
			for _, l := range links {
				perRole[l.Role]++
			}
			if w.metrics != nil {
				for role, n := range perRole {
					w.metrics.RecordImpactLinks(role, n)
				}
			}
			w.logger.Info("incident_impact_linked", "incident", corr.IncidentID, "links", len(links))
		}
		return nil

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
		// Staleness gate (#2): a newer transition has superseded this event (the
		// monitor's state_sequence advanced past the one stamped at enqueue), so
		// delivering it now would fire a down alert (or reminder) for a state the
		// monitor already left — e.g. a retried/reordered DOWN after the recovery was
		// delivered. Drop it. Seq==0 is a legacy event pre-dating the counter: never
		// stale. Recovery events lose nothing — an up that's superseded means an even
		// newer transition will deliver the current state.
		if mt.Seq > 0 && monitor.StateSequence > mt.Seq {
			w.logger.Info("transition_superseded", "monitor_id", monitor.ID, "event_seq", mt.Seq, "current_seq", monitor.StateSequence, "reminder", mt.Reminder)
			return nil
		}
		// A monitor with an escalation policy has its DOWN alerts driven by the on-call
		// ladder (over its open auto-incident), so suppress the flat down-notify to
		// avoid double-alerting. Recovery (up) still goes to all channels.
		if monitor.EscalationPolicyID != "" && monitor.AutoIncident && mt.Cur == domain.StatusDown {
			return nil
		}
		// Service delegation (FR-021 §16.1): a service that ACTIVELY covers the live signal for
		// this monitor answers for it. DOWN and its reminders only — a recovery is never
		// suppressed, whatever the arming state.
		if mt.Cur == domain.StatusDown &&
			w.serviceDelegationSuppressed(ctx, e.ID, monitor, store.DelegationLive, domain.TopicMonitorTransition) {
			return nil
		}
		// Dependency-graph suppression: while a (transitive) parent is down, the
		// child's DOWN alerts (and reminders) stay quiet — the parent's alert names
		// the root cause. Recovery is never suppressed. Facts keep recording; only
		// delivery is muted, plus a one-time timeline note on the child's incident.
		if mt.Cur == domain.StatusDown && w.dependencySuppressed(ctx, monitor) {
			return nil
		}
		// Maintenance suppression: mute a DOWN notify while the monitor is inside an
		// active window. The transition is still recorded and re-enqueued by the
		// renotify job, so if the monitor is still down when the window closes the
		// next reminder delivers — the fix for "down during maintenance is never
		// alerted, even after the window ends". Recovery is never suppressed.
		if mt.Cur == domain.StatusDown {
			if inMaint, err := w.store.MonitorInMaintenance(ctx, monitor.ID); err != nil {
				w.logger.Warn("maintenance_check_failed", "monitor_id", monitor.ID, "error", err.Error())
			} else if inMaint {
				w.logger.Info("alert_suppressed_by_maintenance", "monitor_id", monitor.ID, "reminder", mt.Reminder)
				return nil
			}
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
		// Service delegation on the BURN signal, and only for a FIRING alert: a CLEAR is never
		// suppressed, because arming can change between the two and a recipient left holding a
		// firing burn alert has no way to learn it ended.
		if a.Firing &&
			w.serviceDelegationSuppressed(ctx, e.ID, monitor, store.DelegationBurn, domain.TopicSLOBurnAlert) {
			return nil
		}
		// A burn alert firing while a dependency ancestor is down is the cascade
		// speaking — suppress it like the down-notify. Recovery passes through.
		if a.Firing && w.dependencySuppressed(ctx, monitor) {
			return nil
		}
		return w.notifs.DeliverText(ctx, monitor, a.Message(monitor.Name))

	case domain.TopicServiceAlert:
		var a domain.ServiceAlert
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			return err
		}
		if w.alertSilenced() {
			return nil // instance-wide silence applies to a service alert exactly as to a monitor's
		}
		if w.notifs == nil {
			return errors.New("no notification deliverer configured")
		}
		// The ORDERING gate (§16.5). The outbox is at-least-once and it says so: a retried onset can
		// arrive after a newer close, and delivering it would re-announce a state the service has
		// already left. A superseded event is dropped, exactly as a superseded monitor transition is.
		// A CLOSE is never dropped by this gate — it carries the sequence of the onset it ends.
		current, err := w.store.ServiceAlertSequence(ctx, a)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// The service, its target or its rule is gone. A CLOSE must still deliver — its episode
			// outlived all three precisely so that whoever was paged learns it ended — while an
			// ONSET for a vanished latch has nothing to announce.
			if a.Firing {
				return nil
			}
		case err != nil:
			return err
		case a.Firing && a.Seq < current:
			w.logger.Info("service_alert_superseded", "service_id", a.ServiceID,
				"signal", string(a.Signal), "event_seq", a.Seq, "current_seq", current)
			return nil
		}
		return w.notifs.DeliverChannels(ctx, a.Recipients, a.Message())

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
		// The ladder is where a monitor with an escalation policy ACTUALLY pages — its flat DOWN
		// transition is already dropped above — so delegation has to cover it, or phase 5 would
		// keep its no-double-page promise for everyone except the installations that page properly.
		// The ladder's row, its progress and its incident are untouched; only this delivery is muted.
		if mon, err := w.store.GetMonitor(ctx, a.MonitorID); err != nil {
			w.logger.Warn("escalation_monitor_lookup_failed", "monitor_id", a.MonitorID, "error", err.Error())
			w.metrics.RecordDelegationFailOpen("error")
		} else if w.serviceDelegationSuppressed(ctx, e.ID, mon, store.DelegationLive, domain.TopicEscalationStep) {
			return nil
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
