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
	ReleaseOutboxClaim(ctx context.Context, id, claimToken string) (bool, error)
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
	// Change correlation (FR-025 D7): at a SERVICE auto-incident's `opened` delivery, link the
	// changes that preceded it and append the one `🚀 Changes:` note, in one transaction.
	// Fail-open at the caller: an error is logged and counted, the delivery proceeds.
	LinkPrecedingChanges(ctx context.Context, incidentID string, window time.Duration, noteMax int) (store.ChangeCorrelation, error)
	// Alerting ownership (FR-021 §16.1): whether a service ACTIVELY covers this signal for this
	// monitor — armed, quotable, routable, generation-matched and fresh. An error here must page.
	ActiveDelegation(ctx context.Context, monitorID, projectID string, signal store.DelegationSignal) (store.DelegationVerdict, error)
	ServiceAlertSequence(ctx context.Context, a domain.ServiceAlert) (int64, error)
	MarkServiceAlertDelivered(ctx context.Context, a domain.ServiceAlert) error
	MarkServiceAlertUndeliverable(ctx context.Context, a domain.ServiceAlert) error
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
	// DeliverChannelsReporting is the same delivery, plus what it actually REACHED. A service
	// alert carries the recipient snapshot from when the announcement opened (§16.4a), so a
	// channel in it may since have been deleted or disabled: that is not an error and not
	// success, and §16.6b requires it to be counted rather than silently absorbed.
	DeliverChannelsReporting(ctx context.Context, channelIDs []string, text string) (domain.ChannelDelivery, error)
}

// MailSender delivers a plain-text email (status-page subscription confirmations).
// Optional; when nil, subscriber-confirm events fail and eventually park as dead.
//
// `SendContext` and not `Send`, because this runs under the claim's delivery budget (D-0186): a
// sender that ignores the context can hold an SMTP session past the lease that authorised it, which
// is the one branch that still did after the budget landed.
type MailSender interface {
	SendContext(ctx context.Context, to, subject, body string) error
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
	// Per-signal delegation outcomes, and the two ways a service alert can fail to reach anybody
	// without failing (§16.6b): a snapshot recipient that no longer exists, and an announcement
	// with nobody left to tell.
	RecordServiceDelegation(signal, state string)
	RecordServiceAlertUndeliverable(signal string)
	RecordServiceAlertRecipientMissing(n int)
	RecordImpactWitnessOverflow(n int)
}

// ChangeCorrelationMetrics counts FR-025 D7's correlation outcomes (D15:
// cerbix_change_correlations_total{role}, cerbix_change_correlation_errors_total). Optional and
// nil-safe; the metrics registry binds it. No per-service or per-identity label, ever.
type ChangeCorrelationMetrics interface {
	RecordChangeCorrelations(role string, n int)
	RecordChangeCorrelationError()
}

type noopChangeMetrics struct{}

func (noopChangeMetrics) RecordChangeCorrelations(string, int) {}
func (noopChangeMetrics) RecordChangeCorrelationError()        {}

// The §5a defaults the worker runs with until WithChangeCorrelation hands it the operator's
// values — the same numbers config.Default carries, so an unwired worker is not a silent one.
const (
	defaultChangeCorrelationWindow  = 60 * time.Minute
	defaultChangeCorrelationNoteMax = 5
)

// Worker polls the outbox and delivers due events.
type Worker struct {
	store    Store
	webhooks WebhookDeliverer
	notifs   NotifyDeliverer
	mail     MailSender
	metrics  Metrics
	logger   *slog.Logger
	silenced func() bool // optional: when true, alert notifications are suppressed
	// FR-025 D7: the preceding-change window and the note's entry cap, and the counters.
	changeWindow  time.Duration
	changeNoteMax int
	changeMetrics ChangeCorrelationMetrics
}

// WithChangeCorrelation sets FR-025 D7's bounds (`change.correlation_window`,
// `change.correlation_note_max`) and the counter sink; a nil sink is a no-op. Non-positive
// values keep the defaults.
func (w *Worker) WithChangeCorrelation(window time.Duration, noteMax int, m ChangeCorrelationMetrics) *Worker {
	if window > 0 {
		w.changeWindow = window
	}
	if noteMax > 0 {
		w.changeNoteMax = noteMax
	}
	if m != nil {
		w.changeMetrics = m
	}
	return w
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
	// A nil registry becomes a no-op HERE rather than a guard at each call site. The delegation path
	// grew several unconditional `w.metrics.Record*` calls, so "metrics may be nil" was true of the
	// constructor and false of the code — and the panic waited for the first SUPPRESSED event, which
	// is to say for production. One substitution makes the documented contract true everywhere,
	// including at call sites nobody has written yet.
	if metrics == nil {
		metrics = noopMetrics{}
	}
	return &Worker{
		store: st, webhooks: webhooks, notifs: notifs, metrics: metrics, logger: logger,
		changeWindow: defaultChangeCorrelationWindow, changeNoteMax: defaultChangeCorrelationNoteMax,
		changeMetrics: noopChangeMetrics{},
	}
}

// noopMetrics absorbs every counter for a worker built without a registry.
type noopMetrics struct{}

func (noopMetrics) RecordOutboxDelivered()                 {}
func (noopMetrics) RecordOutboxDead()                      {}
func (noopMetrics) RecordImpactLinks(string, int)          {}
func (noopMetrics) RecordImpactFailure()                   {}
func (noopMetrics) RecordAlertSuppressed(string, string)   {}
func (noopMetrics) RecordDelegationFailOpen(string)        {}
func (noopMetrics) RecordServiceDelegation(string, string) {}
func (noopMetrics) RecordServiceAlertUndeliverable(string) {}
func (noopMetrics) RecordServiceAlertRecipientMissing(int) {}
func (noopMetrics) RecordImpactWitnessOverflow(int)        {}

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
		// BEFORE the call, not after it. The lease starts ticking at the database's `now()` inside
		// `ClaimDueOutbox`, so the statement's own time — planning, the scan, the network round trip
		// — is lease already spent. Marking the batch start after the call returns hands that time
		// back as budget the worker does not have, and a slow claim then delivers past the lease it
		// believes it is inside.
		claimedAt := time.Now()
		events, err := w.store.ClaimDueOutbox(ctx, batchLimit)
		if err != nil {
			w.logger.Error("outbox_claim_failed", "error", err.Error())
			return
		}
		for i := range events {
			w.process(ctx, events[i], claimedAt)
		}
		if len(events) < batchLimit {
			return
		}
	}
}

// leaseHeadroom is subtracted from the lease so the settling writes below still fit inside the claim
// this worker holds. A delivery that used the whole lease would hand the row to the next worker at
// the instant it tried to mark it delivered, turning a success into a lost CAS and a duplicate send.
const leaseHeadroom = 2 * time.Second

func (w *Worker) process(ctx context.Context, e domain.OutboxEvent, claimedAt time.Time) {
	// DELIVERY is bounded by the claim's own lease (D-0186).
	//
	// The claim token already stops a deposed worker from SETTLING a row it no longer owns. What it
	// cannot do is stop that worker from still being inside an HTTP request or an SMTP session while
	// the new owner delivers the same event — the fence lives in the database and this happens
	// outside it. Cancelling the call does not un-send bytes a receiver already accepted, which is
	// the part D-0177 admitted nobody can fix; bounding the call is the part that IS ours, and it
	// makes the overlap window a decision rather than an accident.
	//
	// Zero means the claim did not carry a lease — an older store, or a caller that built the event
	// itself — and then delivery keeps whatever timeout it had, which is the previous behaviour.
	//
	// The bound is on DELIVERY ONLY. The settling writes below run on the caller's context, because
	// a delivery that used its whole budget would otherwise reach `MarkOutboxDelivered` with a
	// cancelled context and turn a successful send into an unrecorded one — the row would go back to
	// the queue and the recipients would be paged twice. That is the failure this whole change
	// exists to reduce, and it would have been introduced by the fix.
	deliverCtx := ctx
	if !e.LeaseUntil.IsZero() && !e.ClaimedAt.IsZero() {
		// The lease as a SPAN in database time, spent against elapsed time on this clock. Never
		// `time.Until(e.LeaseUntil)`: that subtracts a database timestamp from a worker timestamp,
		// and under skew it either delivers after the lease really ended or skips a claim that was
		// perfectly good.
		budget := e.LeaseUntil.Sub(e.ClaimedAt) - time.Since(claimedAt) - leaseHeadroom
		if budget <= 0 {
			// Past it before the first byte — a batch takes up to `batchLimit` rows at once and this
			// one waited its turn behind the others. Do not send: the row belongs to somebody else
			// now, and the duplicate's settle would lose the CAS anyway.
			//
			// But GIVE THE CLAIM BACK. `ClaimDueOutbox` already spent an attempt on every row in the
			// batch, and an event that was never tried must not pay for that: enough turns like this
			// and it dead-letters having never been sent, which is the opposite of a retry budget.
			w.logger.Warn("outbox_lease_expired_before_delivery", "id", e.ID, "topic", e.Topic,
				"lease_until", e.LeaseUntil)
			if released, rerr := w.store.ReleaseOutboxClaim(ctx, e.ID, e.ClaimToken); rerr != nil {
				w.logger.Error("outbox_release_failed", "id", e.ID, "error", rerr.Error())
			} else if !released {
				// Somebody else already owns it, and their attempt is not ours to refund.
				w.logger.Info("outbox_release_skipped", "id", e.ID, "reason", "reclaimed")
			}
			return
		}
		var cancel context.CancelFunc
		deliverCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	err := w.deliver(deliverCtx, ctx, e)
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
		// Dead is terminal, so a service alert that dies here reached nobody and no retry is owed —
		// which is exactly the condition D-0187 re-announces on. It was left out at first and named
		// as not covered, and the reviewer was right that "no retry owed" already includes this: an
		// announcement that exhausted its attempts is as permanently unheard as one whose channels
		// were all deleted, and leaving it uncondemned means the outage is never announced again.
		w.condemnDead(ctx, e)
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

// condemnDead is `condemn` for an event that died by exhausting its retries. It has to decode the
// payload itself, because the delivery path that would have parsed it is the one that just failed.
//
// Only `service_alert` matters here: it is the topic whose delivery arms coverage and whose absence
// the evaluator can act on. Anything else that dead-letters is an operator's problem and says so
// through the dead-letter surface.
func (w *Worker) condemnDead(ctx context.Context, e domain.OutboxEvent) {
	if e.Topic != domain.TopicServiceAlert {
		return
	}
	var a domain.ServiceAlert
	if err := json.Unmarshal(e.Payload, &a); err != nil {
		w.logger.Warn("service_alert_condemn_undecodable", "id", e.ID, "error", err.Error())
		return
	}
	w.condemn(ctx, a)
}

// condemn records that an announcement is KNOWN dead, so the evaluator can announce the outage again
// once somebody can be told (D-0187).
//
// Called ONLY from the terminal paths — an empty recipient snapshot, or a delivery that resolved
// nobody — never where a retry is still owed. The distinction is the whole point of the column: a
// re-announcement triggered by "not delivered yet" would duplicate an event that was merely slow.
//
// A failure to record leaves the outage un-re-announced, which is the state it was already in, so it
// is logged rather than retried: retrying would re-send the page.
func (w *Worker) condemn(ctx context.Context, a domain.ServiceAlert) {
	if !a.Firing {
		return
	}
	if err := w.store.MarkServiceAlertUndeliverable(ctx, a); err != nil {
		w.logger.Warn("service_alert_condemn_failed", "service_id", a.ServiceID,
			"signal", string(a.Signal), "seq", a.Seq, "error", err.Error())
	}
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
		w.metrics.RecordServiceDelegation(delegationSignalName(signal), "degraded")
		return false
	}
	if !v.Suppress() {
		w.metrics.RecordDelegationFailOpen(v.FailOpenReason)
		w.metrics.RecordServiceDelegation(delegationSignalName(signal), "disarmed")
		return false
	}
	if err := w.store.RecordSuppression(ctx, eventID, monitor.ID, monitor.ProjectID, topic, v.Owners); err != nil {
		// The visibility path is not optional: if the suppression cannot be recorded and explained,
		// it does not happen.
		w.logger.Warn("suppression_record_failed", "monitor_id", monitor.ID,
			"topic", topic, "error", err.Error())
		w.metrics.RecordDelegationFailOpen("record_failed")
		w.metrics.RecordServiceDelegation(delegationSignalName(signal), "degraded")
		return false
	}
	w.logger.Info("alert_suppressed_by_service", "monitor_id", monitor.ID, "monitor", monitor.Name,
		"topic", topic, "owner", v.Owners[0].Name, "owners", len(v.Owners))
	w.metrics.RecordAlertSuppressed(topic, string(domain.SuppressionServiceDelegation))
	w.metrics.RecordServiceDelegation(delegationSignalName(signal), "armed")
	return true
}

// delegationSignalName maps the store's delegation signal to §16.6b's `signal` label, which is the
// same vocabulary the evaluator families use — two names for one signal would split every dashboard.
func delegationSignalName(signal store.DelegationSignal) string {
	if signal == store.DelegationBurn {
		return string(domain.ServiceSignalBurn)
	}
	return string(domain.ServiceSignalHealth)
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

// linkPrecedingChanges runs FR-025 D7's correlation for an opened service auto-incident. Errors
// are logged and counted, never propagated — the incident event itself was already delivered
// (invariant 8: the incident opens and resolves exactly as it does today).
func (w *Worker) linkPrecedingChanges(ctx context.Context, inc domain.Incident) {
	res, err := w.store.LinkPrecedingChanges(ctx, inc.ID, w.changeWindow, w.changeNoteMax)
	if err != nil {
		w.logger.Warn("change_correlation_failed", "incident", inc.ID, "service_id", inc.ServiceID, "error", err.Error())
		w.changeMetrics.RecordChangeCorrelationError()
		return
	}
	if res.Skipped || len(res.Links) == 0 {
		return
	}
	perRole := map[string]int{}
	for _, l := range res.Links {
		perRole[l.Role]++
	}
	for role, n := range perRole {
		w.changeMetrics.RecordChangeCorrelations(role, n)
	}
	w.logger.Info("incident_changes_linked", "incident", inc.ID, "links", len(res.Links), "note", res.NoteAdded)
}

// deliver dispatches one event by topic. A nil error means the event is done.
// `settleCtx` is the caller's UNBOUNDED context, separate from `ctx`, which carries the delivery
// budget (D-0186). Every write that RECORDS what a delivery did — the coverage credit, the
// condemnation — takes it, for the same reason `MarkOutboxDelivered` does: a send that used its whole
// budget must still be able to say so. Recording on the bounded context loses the credit for a page
// that went out, which re-arms nothing and pages the members redundantly, and loses the condemnation
// for a page that reached nobody, which means the outage is never re-announced (D-0187).
func (w *Worker) deliver(ctx, settleCtx context.Context, e domain.OutboxEvent) error {
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
		// There is NO ordering gate here, and its absence is the design rather than an omission.
		//
		// The first version compared this event's sequence with the incident's CURRENT one and
		// dropped the older. Two things were wrong with it. It dropped an opening that had never been
		// delivered merely because a later fact existed, so a subscriber received an update or a
		// resolution for an outage they were never told had begun — the exact silence the fence was
		// built to prevent, arriving by a different route. And the read happened BEFORE the delivery
		// call, so two workers could still interleave around it: a check whose answer can change
		// while the thing it authorises is in flight is not a fence.
		//
		// Ordering is enforced where it can be durable: `ClaimDueOutbox` will not release an event
		// while an earlier event of the same incident is undelivered, and a CLAIMED row is not a
		// delivered one. Nothing here needs to guess.
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
		// Change correlation (FR-025 D7): for a SERVICE auto-incident, the changes that preceded
		// it are linked and named once — the same place, the same best-effort contract: a
		// failure is logged and counted and never fails the (already delivered) event.
		if ev.Type == domain.EventIncidentOpened && ev.Incident.Source == domain.SourceAuto && ev.Incident.ServiceID != "" {
			w.linkPrecedingChanges(ctx, ev.Incident)
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
		// Nobody to tell is a FACT about this announcement, not a delivery to retry: the snapshot
		// was taken when it opened and every channel in it has since gone, or it opened with an
		// empty route. Counted, and then done — retrying it forever would park a dead letter for
		// something no retry can fix.
		if len(a.Recipients) == 0 {
			w.metrics.RecordServiceAlertUndeliverable(string(a.Signal))
			w.logger.Warn("service_alert_undeliverable", "service_id", a.ServiceID,
				"signal", string(a.Signal), "reason", "empty_recipient_snapshot")
			w.condemn(settleCtx, a)
			return nil
		}
		res, err := w.notifs.DeliverChannelsReporting(ctx, a.Recipients, a.Message())
		if missing := res.Requested - res.Resolved; missing > 0 {
			// §16.4a: a snapshot recipient whose channel has been deleted is COUNTED, never
			// silently replaced by whoever is on call now — replacing it would page a stranger
			// and leave the person who heard the onset holding it.
			w.metrics.RecordServiceAlertRecipientMissing(missing)
			w.logger.Warn("service_alert_recipient_missing", "service_id", a.ServiceID,
				"signal", string(a.Signal), "missing", missing, "requested", res.Requested)
		}
		if err == nil && res.Resolved == 0 {
			// Enqueued, attempted, and heard by nobody. Terminal on purpose — no retry reaches a
			// channel that is gone — but it must NOT look like an announcement from here on, or the
			// service would go on covering members for an onset that reached no one (D-0179).
			w.metrics.RecordServiceAlertUndeliverable(string(a.Signal))
			w.condemn(settleCtx, a)
			return nil
		}
		// Somebody was told. THIS is what arms coverage: §16.1's committed onset is now a DELIVERED
		// one, and until this lands the members keep paging for themselves.
		//
		// Credited on `Delivered > 0` — sends that SUCCEEDED — and not on `Resolved`, which counts
		// channel rows that exist and are enabled. That distinction is the whole of this branch: a
		// service whose only channel returns 500 resolves ONE and delivers NONE, and crediting it
		// suppressed the members' own alerts for an announcement nobody got. Permanently, once the
		// outbox dead-lettered the retry, because the latch stays firing and no further edge comes.
		//
		// Independent of `err` on purpose. A partial delivery — three channels reached, a fourth
		// timing out — IS an announcement people received, and D-0179's contract is "at least one
		// recipient", not "a clean call"; the error below still fails the event so the fourth is
		// retried. The credit is monotonic and sequence-guarded, so the retry's re-delivery to
		// everyone changes nothing the second time.
		//
		// A CLOSE never counts. Coverage is about an announcement that is LIVE, and crediting an
		// ending would arm a service whose alert is over. The store refuses one too — a safety
		// property with one guard is a safety property one refactor from being gone.
		//
		// A failure to RECORD does not fail the delivery: the page has gone out, and returning that
		// error would send it again. It leaves coverage dis-armed, the direction every other
		// ambiguity in the conjunction takes.
		if a.Firing && res.Delivered > 0 {
			if derr := w.store.MarkServiceAlertDelivered(settleCtx, a); derr != nil {
				w.logger.Warn("service_alert_delivery_unrecorded", "service_id", a.ServiceID,
					"signal", string(a.Signal), "seq", a.Seq, "error", derr.Error())
			}
		}
		return err

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
		//
		// A SERVICE's step (FR-023) skips this entirely, and not as an optimisation: delegation
		// exists so a service can page INSTEAD of its members, and this step IS that page. Muting
		// it would leave the outage with nobody told at all. Skipping the lookup with it also
		// matters — a service step carries no monitor id, so the lookup could only fail, and the
		// fail-open warning it logged would be an alarm about a question nobody asked.
		if a.ServiceID == "" {
			if mon, err := w.store.GetMonitor(ctx, a.MonitorID); err != nil {
				w.logger.Warn("escalation_monitor_lookup_failed", "monitor_id", a.MonitorID, "error", err.Error())
				w.metrics.RecordDelegationFailOpen("error")
			} else if w.serviceDelegationSuppressed(ctx, e.ID, mon, store.DelegationLive, domain.TopicEscalationStep) {
				return nil
			}
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
		return w.mail.SendContext(ctx, sc.To, sc.Subject, sc.Body)

	default:
		return errors.New("unknown outbox topic: " + e.Topic)
	}
}
