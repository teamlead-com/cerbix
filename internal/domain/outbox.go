package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Outbox topics. Each maps to a delivery handler in the outbox worker.
const (
	// TopicIncidentEvent carries a JSON-encoded IncidentEvent for webhook delivery.
	TopicIncidentEvent = "incident_event"
	// TopicMonitorTransition carries a MonitorTransition for notification delivery.
	TopicMonitorTransition = "monitor_transition"
	// TopicSLOBurnAlert carries an SLOBurnAlert for notification delivery.
	TopicSLOBurnAlert = "slo_burn_alert"
	// TopicSLAReport carries an SLAReport for delivery to a project's channels.
	TopicSLAReport = "sla_report"
	// TopicRegionWorkerAlert carries a RegionWorkerAlert for delivery to an affected
	// project's channels (a worker-pool region lost/regained its worker).
	TopicRegionWorkerAlert = "region_worker_alert"
	// TopicEscalationStep carries an EscalationStepAlert for delivery to the channels
	// resolved for one step of an on-call escalation ladder.
	TopicEscalationStep = "escalation_step"
	// TopicSubscriberConfirm carries a SubscriberConfirm: a rendered status-page
	// subscription double-opt-in email, sent off the request path so a slow or
	// failing SMTP never blocks (or errors) the subscribe call.
	TopicSubscriberConfirm = "subscriber_confirm"
	// TopicIncidentCorrelation carries an IncidentCorrelation: the service-graph
	// impact computation for a freshly-opened monitor-anchored incident. Its own
	// topic — never a rider on incident_event — so webhook death never blocks
	// correlation and a correlation failure never blocks incident delivery
	// (FR-021 §14.3). FENCED: see FencedTopic.
	TopicIncidentCorrelation = "incident_correlation"
	// TopicServiceAlert carries a ServiceAlert: a service's own page, on either of its two
	// signals (FR-021 §16). Its own topic rather than a rider on `slo_burn_alert`, because the
	// delegation rule has to be able to tell WHOSE alert it is holding, and a monitor-shaped
	// payload has no room for a service that has no monitor id. FENCED: see FencedTopic.
	TopicServiceAlert = "service_alert"
)

// AllTopics enumerates every topic this binary can ENQUEUE. It exists because the database keeps
// its own copy of that list in `outbox_events_topic_check`, and a whitelist rewritten by hand is
// how three live topics (`region_worker_alert`, `escalation_step`, `subscriber_confirm`) were
// silently dropped while a fourth was being added — every escalation, region alert and subscriber
// confirmation would have failed its insert in production. A store test pins this list against the
// constraint, so the two lists can no longer drift in either direction.
func AllTopics() []string {
	return []string{
		TopicIncidentEvent, TopicMonitorTransition, TopicSLOBurnAlert, TopicSLAReport,
		TopicRegionWorkerAlert, TopicEscalationStep, TopicSubscriberConfirm,
		TopicIncidentCorrelation, TopicServiceAlert,
	}
}

// FencedTopic reports whether a topic's rows use the fenced claimable class
// ('pending_fenced'): a status the legacy claim shape (status = 'pending', no
// topic predicate) cannot select, so an old delivery owner in a mixed-version
// fleet can neither claim, attempt-burn nor dead-letter them. EVERY topic
// introduced after phase 2 is fenced; the pre-fence topics stay legacy forever
// because every deployed owner already dispatches them. This map is the ONE
// source of truth for enqueue class, capable claim, retry and replay
// (FR-021 §14.3, invariant 61) — a test pins it against the worker's dispatch
// switch so the two cannot drift.
func FencedTopic(topic string) bool {
	for _, t := range FencedTopics() {
		if t == topic {
			return true
		}
	}
	return false
}

// FencedTopics enumerates every fenced topic THIS BINARY knows how to
// dispatch. The capable claim selects 'pending_fenced' rows for exactly this
// set — so a binary never claims a fenced row of a topic a newer release
// introduced (the same protection the fence gives against pre-fence binaries,
// carried forward automatically).
func FencedTopics() []string {
	// `incident_event` joined this set with D-0177's causal fence, and the reason is the rolling
	// upgrade rather than the topic's age. The ordering guarantee lives in the CLAIM — a successor is
	// not handed out while a predecessor of the same incident is undelivered — so a pre-fence worker
	// running the old claim would take the rows and bypass it. Fencing them means such a worker never
	// sees them: it does not list this topic, so its capable claim cannot match. The cost is the
	// fenced class's usual one — during a rolling upgrade, incident events wait for a worker that can
	// order them, which is a delay rather than a wrong order.
	//
	// The class is settled by the DATABASE (migration 00088), not by this list alone: an old producer
	// still running through the upgrade reads ITS OWN copy of this function and would otherwise keep
	// writing legacy rows for exactly the window the fence exists to cover. Rows written before the
	// barrier are promoted once, dead ones included, so a replay years later cannot put them back in
	// a class the old claim can reach.
	//
	// What the barrier does NOT cover: work an old worker had ALREADY CLAIMED when it ran. Its token
	// is replaced so it cannot settle the row, but an external call it has already made cannot be
	// recalled by anything on this side (D-0177). Draining old workers before migrating is the
	// operational answer; the guarantee here is about rows, not about calls in flight.
	return []string{TopicIncidentCorrelation, TopicServiceAlert, TopicIncidentEvent}
}

// IncidentCorrelation is the payload for a TopicIncidentCorrelation outbox
// event: the incident whose impact links are to be computed. The attempt
// re-reads everything else authoritatively under locks (§14.3), so the payload
// carries identity only.
type IncidentCorrelation struct {
	IncidentID string `json:"incident_id"`
}

// SubscriberConfirm is the payload for a TopicSubscriberConfirm outbox event: a
// rendered confirmation email for a new status-page subscriber. The subscribe
// handler renders subject/body (it needs the confirm token and the public base
// URL) and queues it; the outbox worker delivers it via the mailer with the
// usual retry/backoff/dead-letter semantics.
type SubscriberConfirm struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// OutboxEvent is one durably-queued outbound event awaiting delivery.
type OutboxEvent struct {
	ID       string
	Topic    string
	Payload  []byte
	Attempts int
	// ClaimToken identifies this delivery claim; the worker passes it back on
	// mark-delivered/fail so a stale claim (lease expired, row re-claimed) can't
	// overwrite the current owner's terminal state.
	ClaimToken string
}

// OutboxEventView is an outbox row exposed to operators (the dead-letter admin
// endpoints). Payload is raw JSON passed through as-is.
type OutboxEventView struct {
	ID        string          `json:"id"`
	Topic     string          `json:"topic"`
	Status    string          `json:"status"`
	Attempts  int             `json:"attempts"`
	LastError string          `json:"last_error"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// MonitorTransition is the payload for a TopicMonitorTransition outbox event: a
// monitor's status changed from Prev to Cur. The delivery worker applies the
// notification policy (notify on down; on up only when recovering from down), so
// the store records the raw fact and the application layer decides what to send.
type MonitorTransition struct {
	MonitorID string        `json:"monitor_id"`
	Prev      MonitorStatus `json:"prev"`
	Cur       MonitorStatus `json:"cur"`
	At        time.Time     `json:"at"`
	Reminder  bool          `json:"reminder,omitempty"` // re-notification while still down (not a real transition)
	// Seq is the monitor's state_sequence at enqueue time. At delivery, an event
	// whose Seq is older than the monitor's current state_sequence is dropped as
	// superseded — a stale DOWN or reminder can't fire after a newer transition.
	// Zero means "unset" (legacy events pre-dating the counter): never treated as
	// stale, always delivered.
	Seq int64 `json:"seq,omitempty"`
}

// SLOBurnAlert is the payload for a TopicSLOBurnAlert outbox event: a monitor's
// SLO error budget is (or is no longer) burning faster than its threshold over a
// short window. Firing distinguishes the alert from its recovery. The delivery
// worker fans it out to the monitor's notification channels as free text.
type SLOBurnAlert struct {
	MonitorID     string    `json:"monitor_id"`
	Window        string    `json:"window"`
	WindowSeconds int       `json:"window_seconds"` // the rule's LONG window
	Objective     float64   `json:"objective"`
	BurnRate      float64   `json:"burn_rate"`
	Threshold     float64   `json:"threshold"`
	Firing        bool      `json:"firing"`
	At            time.Time `json:"at"`
	// Multi-window rule attribution (D-0098): the short confirmation window and
	// the rule's severity. Empty on legacy single-window events.
	ShortWindowSeconds int    `json:"short_window_seconds,omitempty"`
	Severity           string `json:"severity,omitempty"`
}

// windows renders the rule's window pair ("1h ∧ 5m") or the single legacy window.
func (a SLOBurnAlert) windows() string {
	if a.ShortWindowSeconds > 0 && a.ShortWindowSeconds != a.WindowSeconds {
		return humanWindow(a.WindowSeconds) + " ∧ " + humanWindow(a.ShortWindowSeconds)
	}
	return humanWindow(a.WindowSeconds)
}

// sevTag renders the severity prefix ("[page] " / "[ticket] "), empty for legacy.
func (a SLOBurnAlert) sevTag() string {
	if a.Severity == "" {
		return ""
	}
	return "[" + a.Severity + "] "
}

// Message renders the human-readable burn alert for the given monitor name.
func (a SLOBurnAlert) Message(monitorName string) string {
	icon := "🔥"
	if a.Severity == BurnSeverityTicket {
		icon = "⚠️"
	}
	if a.Firing {
		return fmt.Sprintf("%s %s%s is burning its SLO error budget fast — %.1f× over %s (objective %.4g%%).",
			icon, a.sevTag(), monitorName, a.BurnRate, a.windows(), a.Objective)
	}
	return fmt.Sprintf("✅ %s%s error-budget burn is back to normal (%.1f× over %s).",
		a.sevTag(), monitorName, a.BurnRate, a.windows())
}

// humanWindow renders a burn window in the coarsest whole unit (h/m/s).
func humanWindow(seconds int) string {
	switch {
	case seconds%3600 == 0 && seconds >= 3600:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds%60 == 0 && seconds >= 60:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// RegionWorkerAlert is the payload for a TopicRegionWorkerAlert outbox event: a
// worker-pool region lost its last live worker while it still had enabled monitors
// (Missing=true), or regained one (Missing=false). Because notification channels are
// per-project, the scheduler fans one event out to each affected project, carrying that
// project's monitor count in the region. The delivery worker sends it as free text.
type RegionWorkerAlert struct {
	Region       string `json:"region"`
	ProjectID    string `json:"project_id"`
	MonitorCount int    `json:"monitor_count"`
	Missing      bool   `json:"missing"`
}

// Message renders the human-readable region-worker alert for a project's channels.
func (a RegionWorkerAlert) Message() string {
	if a.Missing {
		return fmt.Sprintf("Region %q has no live worker — %d monitor(s) in this project are not being checked. Start a worker with --region %s.",
			a.Region, a.MonitorCount, a.Region)
	}
	return fmt.Sprintf("Region %q has a live worker again — %d monitor(s) in this project resumed checking.",
		a.Region, a.MonitorCount)
}

// EscalationStepAlert is the payload for a TopicEscalationStep outbox event: one
// rung of an on-call escalation ladder fired for an open (unacknowledged)
// auto-incident. The scheduler resolves the step's targets (channels + whoever is
// on call) into concrete channel ids at fire time; the delivery worker sends the
// text to exactly those channels. Repeat marks a re-send of the final step.
//
// The subject may be a MONITOR or, since FR-023, a SERVICE — the two are exclusive,
// as the incident's anchor is. Three fields carry that, and the reason each exists
// is worth stating because this topic is PRE-FENCE (domain.FencedTopics): a
// currently-deployed worker claims these rows and must keep rendering them.
//
//   - ServiceID is set for a service step, and is what the delivery worker branches
//     on to skip delegation — a service's ladder IS the owner's page, and
//     suppressing it would mute the only alert anyone gets (FR-023 D7);
//   - SubjectName is the authoritative display name for both kinds;
//   - MonitorName is a LEGACY RENDER FIELD, kept populated with the SUBJECT's name
//     (the service's, for a service step) so a pre-FR-023 worker renders a correct
//     sentence instead of " is DOWN". Fencing the topic was the alternative and it
//     is worse: pre-fence topics stay legacy forever precisely because every
//     deployed owner already dispatches them, so fencing this one would stop those
//     workers claiming escalation steps at all during a rolling upgrade.
type EscalationStepAlert struct {
	IncidentID  string   `json:"incident_id"`
	MonitorID   string   `json:"monitor_id"`
	MonitorName string   `json:"monitor_name"`
	ServiceID   string   `json:"service_id,omitempty"`
	SubjectName string   `json:"subject_name,omitempty"`
	Step        int      `json:"step"`
	Repeat      bool     `json:"repeat,omitempty"`
	ChannelIDs  []string `json:"channel_ids"`
}

// Subject is the name to render. SubjectName wins; MonitorName is the fallback for
// payloads enqueued before FR-023, which never carried one.
func (a EscalationStepAlert) Subject() string {
	if a.SubjectName != "" {
		return a.SubjectName
	}
	return a.MonitorName
}

// Message renders the human-readable escalation notification.
func (a EscalationStepAlert) Message() string {
	if a.Repeat {
		return fmt.Sprintf("🚨 Still unacknowledged: %s is DOWN (escalation step %d). Acknowledge the incident to stop escalation.",
			a.Subject(), a.Step+1)
	}
	return fmt.Sprintf("🚨 %s is DOWN — on-call escalation step %d. Acknowledge the incident to stop escalation.",
		a.Subject(), a.Step+1)
}

// SLAReportWindow is one rolling window's availability line in a weekly report.
type SLAReportWindow struct {
	Window        string  `json:"window"`
	UptimePercent float64 `json:"uptime_percent"`
	Up            int64   `json:"up"`
	Total         int64   `json:"total"`
}

// SLAReport is the payload for a TopicSLAReport outbox event: a project's periodic
// availability summary, delivered to the project's notification channels.
type SLAReport struct {
	ProjectID   string            `json:"project_id"`
	ProjectName string            `json:"project_name"`
	Windows     []SLAReportWindow `json:"windows"`
	GeneratedAt time.Time         `json:"generated_at"`
}

// Message renders the report as a multi-line text summary.
func (r SLAReport) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 Weekly SLA report — %s", r.ProjectName)
	for _, w := range r.Windows {
		if w.Total == 0 {
			fmt.Fprintf(&b, "\n  %-3s: no data", w.Window)
			continue
		}
		fmt.Fprintf(&b, "\n  %-3s: %.3f%% (%d/%d checks)", w.Window, w.UptimePercent, w.Up, w.Total)
	}
	return b.String()
}

// ShouldNotify reports whether this transition warrants a channel notification:
// any move to down, a recovery to up from down (not from pending/unknown), or a
// reminder re-notification while still down.
func (t MonitorTransition) ShouldNotify() bool {
	switch {
	case t.Reminder:
		return true
	case t.Cur == StatusDown && t.Prev != StatusDown:
		return true
	case t.Cur == StatusUp && t.Prev == StatusDown:
		return true
	default:
		return false
	}
}
