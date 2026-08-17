package domain

import (
	"fmt"
	"time"
)

// FR-021 §16 — alerting ownership.
//
// §13 held this as intent because monitor burn alerting already pages: turning on service alerts
// without an ownership rule pages twice for one failure. The owner's decisions (D-0168) are that
// ownership is DECLARED at the service and defaults to off, that a service alerts on a LIVE health
// transition AND a SEALED burn breach, that the alert notifies without opening an incident, and
// that service burn rules are the same `BurnRule` monitors already use.
//
// This file owns the vocabulary and the pure decisions. Nothing here reads a clock or a database:
// what pages is a function of (declared policy, evaluated state, previous announcement), which is
// the only shape in which "why was I woken" is answerable after the fact.

// ServiceAlertState is one service's alerting state. It is deliberately the SAME five values the
// evaluator produces plus nothing: an alerting-specific state machine would be a second opinion
// about what "down" means.
type ServiceAlertState string

const (
	ServiceAlertHealthy  ServiceAlertState = "healthy"
	ServiceAlertDegraded ServiceAlertState = "degraded"
	ServiceAlertDown     ServiceAlertState = "down"
	// ServiceAlertUnknown is "cerbix cannot see this service right now". It is NOT a synonym for
	// down and never pages as one — see ServiceAlertPolicy.PageOnUnknown.
	ServiceAlertUnknown ServiceAlertState = "unknown"
	// ServiceAlertExcluded is a declared maintenance window: a declared silence.
	ServiceAlertExcluded ServiceAlertState = "excluded"
)

// ValidServiceAlertState reports whether s is one of the five.
func ValidServiceAlertState(s ServiceAlertState) bool {
	switch s {
	case ServiceAlertHealthy, ServiceAlertDegraded, ServiceAlertDown,
		ServiceAlertUnknown, ServiceAlertExcluded:
		return true
	}
	return false
}

// ServiceAlertPolicy is what a service DECLARED about being paged for. Every field defaults to the
// quiet, non-surprising value: a service that says nothing pages for `down` only, does not page for
// unknown, and confirms over two evaluations.
type ServiceAlertPolicy struct {
	// OwnsPaging is the ownership declaration of §16.1. While true, the service's SLI members stop
	// DELIVERING their own alerts — they keep probing, flipping, recording and opening incidents.
	OwnsPaging bool `json:"owns_paging"`
	// PageOn is the subset of {down, degraded} that notifies. `unknown` is deliberately not
	// expressible here: it has its own switch, because folding it in would let it be enabled by a
	// UI that offers "all states" without anybody deciding that blindness is an outage.
	PageOn []ServiceAlertState `json:"page_on"`
	// PageOnUnknown is off by default. On, it says "tell me when you cannot see this service",
	// which is a legitimate thing to want and a different statement from "it is down".
	PageOnUnknown bool `json:"page_on_unknown"`
	// ConfirmEvaluations is how many consecutive evaluations a new state needs before it notifies.
	// Counted in evaluations over a FIXED cadence so the delay is computable and printable.
	ConfirmEvaluations int `json:"confirm_evaluations"`
}

// DefaultServiceAlertPolicy is what an undeclared service has.
func DefaultServiceAlertPolicy() ServiceAlertPolicy {
	return ServiceAlertPolicy{
		PageOn:             []ServiceAlertState{ServiceAlertDown},
		ConfirmEvaluations: 2,
	}
}

// Validate enforces the policy's bounds. `page_on` may not name `unknown` or `excluded`: the first
// has its own switch and the second is a declared silence, so accepting either here would create
// two ways to say one thing and a third that contradicts the spec.
func (p ServiceAlertPolicy) Validate() error {
	if p.ConfirmEvaluations < 1 || p.ConfirmEvaluations > 10 {
		return fmt.Errorf("service alert policy: confirm_evaluations must be 1..10, got %d",
			p.ConfirmEvaluations)
	}
	for _, s := range p.PageOn {
		switch s {
		case ServiceAlertDown, ServiceAlertDegraded:
		case ServiceAlertUnknown:
			return fmt.Errorf("service alert policy: unknown has its own switch (page_on_unknown) " +
				"and cannot be listed in page_on")
		default:
			return fmt.Errorf("service alert policy: %q cannot be paged on", s)
		}
	}
	return nil
}

// Pages reports whether the policy asks to be notified about being IN this state.
//
// This is the whole "what wakes someone" decision, in one total function with no clock and no I/O:
//
//   - EXCLUDED never pages. A declared maintenance window is a declared silence.
//   - UNKNOWN pages only under its own explicit switch, and never because `down` was requested.
//   - HEALTHY never pages as an alarm; a return to healthy is a RECOVERY, which
//     ServiceAlertTransition decides separately, because "notify me about down" implies "tell me
//     when it is over" and does not imply "page me about health".
func (p ServiceAlertPolicy) Pages(s ServiceAlertState) bool {
	switch s {
	case ServiceAlertExcluded, ServiceAlertHealthy:
		return false
	case ServiceAlertUnknown:
		return p.PageOnUnknown
	}
	for _, want := range p.PageOn {
		if want == s {
			return true
		}
	}
	return false
}

// ServiceAlertCloseReason names why an announcement ENDED. Every close carries one, because
// "recovered" is a claim about the service and three of these are claims about US — we stopped
// looking, the operator declared a window, the operator changed the policy. Announcing any of them as
// a recovery would assert evidence that does not exist.
type ServiceAlertCloseReason string

const (
	CloseRecovered          ServiceAlertCloseReason = "recovered"
	CloseVisibilityLost     ServiceAlertCloseReason = "visibility_lost"
	CloseEnteredMaintenance ServiceAlertCloseReason = "entered_maintenance"
	CloseOwnershipDisabled  ServiceAlertCloseReason = "ownership_disabled"
	ClosePolicyChanged      ServiceAlertCloseReason = "policy_changed"
	CloseBurnDisabled       ServiceAlertCloseReason = "burn_disabled"
	CloseRuleRemoved        ServiceAlertCloseReason = "rule_removed"
	CloseServiceDeleted     ServiceAlertCloseReason = "service_deleted"
)

// ServiceAlertDecision is what one evaluation concluded.
type ServiceAlertDecision struct {
	// Notify is true when an event must be enqueued.
	Notify bool
	// Close marks a notification that ENDS an announcement rather than starting one.
	Close bool
	// CloseReason is set when Close is true, and it is never inferred: a close that lost sight of the
	// service says so instead of claiming a recovery.
	CloseReason ServiceAlertCloseReason
	// NextEmitted is the state to store in `emitted_state` when Notify is true. Named for when it
	// changes — at ENQUEUE, not at delivery, because the outbox is at-least-once.
	NextEmitted ServiceAlertState
	// NextFiring is the LEVEL to store in `live_firing`. It is a separate fact from the state: with
	// `page_on = {down}`, healthy→degraded changes the state and not the level.
	NextFiring bool
	// Reason is why nothing was sent. An empty Reason with Notify=false is impossible: silence
	// always has a name, because "why was I not paged" is the question this feature must survive.
	Reason string
}

// DecideServiceAlert is the edge rule, and it is pure.
//
//	current  — what this evaluation observed
//	streak   — consecutive evaluations of `current`, including this one
//	firing   — whether an announcement is currently OPEN (the level, from `live_firing`)
//	emitted  — what the last ENQUEUED notification announced, or "" if nothing ever has
//
// The level is what makes this correct where a state comparison is not. With `page_on = {down}`:
// healthy→degraded changes the state but pages nothing and must not open an announcement;
// degraded→healthy must not emit a recovery for something never announced; down→degraded must CLOSE,
// because the operator was told the service was down and it no longer is.
//
// The three closes that are not recoveries are the owner's decision 7: leaving a paged state for one
// that cannot page — maintenance, or lost visibility — ends the announcement and NAMES why. Calling
// either "recovered" would assert evidence nobody has.
func DecideServiceAlert(
	p ServiceAlertPolicy, current ServiceAlertState, streak int, firing bool, emitted ServiceAlertState,
) ServiceAlertDecision {
	if !p.OwnsPaging {
		// A close for an open announcement is the caller's job through the lifecycle path
		// (CloseOwnershipDisabled), not this function's: by the time ownership is off, the policy no
		// longer describes what should have been announced.
		return ServiceAlertDecision{Reason: "service does not own paging"}
	}
	if streak < p.ConfirmEvaluations {
		return ServiceAlertDecision{
			NextFiring: firing,
			Reason: fmt.Sprintf("state %s not confirmed yet (%d of %d evaluations)",
				current, streak, p.ConfirmEvaluations),
		}
	}
	pageable := p.Pages(current)
	switch {
	case pageable && firing && emitted == current:
		return ServiceAlertDecision{NextFiring: true, Reason: fmt.Sprintf("%s already announced", current)}
	case pageable:
		// A new pageable state: from nothing, from healthy, or from another pageable state. The last
		// case is ONE new onset, not a close followed by an onset — the operator is told what it is
		// now, and a manufactured "recovered" in between would be false.
		return ServiceAlertDecision{Notify: true, NextEmitted: current, NextFiring: true}
	case !firing:
		// Nothing is open, and this state does not page. There is nothing to end.
		return ServiceAlertDecision{Reason: fmt.Sprintf("%s does not page", current)}
	default:
		// An announcement is open and the service has left the state that opened it.
		return ServiceAlertDecision{
			Notify: true, Close: true, CloseReason: closeReasonFor(current),
			NextEmitted: current, NextFiring: false,
		}
	}
}

// closeReasonFor names the end honestly. Only a return to HEALTHY is a recovery; the other two are
// statements about what WE can say, not about the service.
func closeReasonFor(current ServiceAlertState) ServiceAlertCloseReason {
	switch current {
	case ServiceAlertHealthy:
		return CloseRecovered
	case ServiceAlertExcluded:
		return CloseEnteredMaintenance
	case ServiceAlertUnknown:
		return CloseVisibilityLost
	default:
		// Reached only if a pageable state stops being pageable mid-flight (a policy edit between
		// evaluations), which the lifecycle path names explicitly. Falling back to `policy_changed`
		// keeps the close truthful rather than inventing a recovery.
		return ClosePolicyChanged
	}
}

// ServiceAlert is the payload of the `service_alert` outbox topic.
//
// It carries the SIGNAL that produced it, because the two have different latencies and an operator
// reading a page must know which one they are holding: a live transition is an outage page, and a
// sealed burn breach is a budget signal that trails the seal watermark by construction.
type ServiceAlert struct {
	ServiceID   string `json:"service_id"`
	ProjectID   string `json:"project_id"`
	ServiceName string `json:"service_name"`
	ServiceSlug string `json:"service_slug"`
	// Signal is "health" or "burn".
	Signal ServiceAlertSignal `json:"signal"`
	// Firing distinguishes the onset from the close for BOTH signals.
	Firing bool `json:"firing"`
	// CloseReason is present on a close and absent on an onset. A close that is not a recovery says
	// which of the three it is, so a channel can render "we stopped seeing it" as itself.
	CloseReason ServiceAlertCloseReason `json:"close_reason,omitempty"`
	// EpisodeID ties a close to the onset it ends, and carries the immutable recipient snapshot the
	// close must reach — the people who heard the onset, not whoever is on call now.
	EpisodeID string `json:"episode_id"`
	// Seq is the monotonic per-service (health) or per-rule (burn) sequence. Delivery drops an ONSET
	// whose Seq is behind the current one, so a retried onset cannot re-announce a state already
	// left. The outbox is at-least-once; this is what keeps a duplicate from being a LIE.
	Seq int64 `json:"seq"`
	// RuleKey identifies which burn rule this is about; empty for the health signal. It is the
	// canonical key over the rule's DECLARED fields, so it cannot change because the rule fired.
	RuleKey string `json:"rule_key,omitempty"`
	// Recipients is the IMMUTABLE snapshot resolved when the episode opened. A close goes to the
	// people who heard the onset, not to whoever is on call at close time — a schedule that rotated
	// mid-incident would otherwise page a stranger and leave the original recipient hanging.
	Recipients []string `json:"recipients"`

	// Health-signal fields.
	State ServiceAlertState `json:"state,omitempty"`
	// ConfirmedOver states how many evaluations agreed, so the delay in the page is explainable.
	ConfirmedOver int `json:"confirmed_over,omitempty"`
	// FailingInputs / TotalInputs describe the aggregation that produced the state. Diagnostics,
	// not the decision: the decision is the evaluator's.
	FailingInputs int `json:"failing_inputs,omitempty"`
	TotalInputs   int `json:"total_inputs,omitempty"`

	// Burn-signal fields, the same shape the monitor path already publishes.
	Window             string  `json:"window,omitempty"`
	WindowSeconds      int     `json:"window_seconds,omitempty"`
	ShortWindowSeconds int     `json:"short_window_seconds,omitempty"`
	Severity           string  `json:"severity,omitempty"`
	Objective          float64 `json:"objective,omitempty"`
	BurnRate           float64 `json:"burn_rate,omitempty"`
	Threshold          float64 `json:"threshold,omitempty"`
	// SealedThrough is the watermark the burn number was computed from. A number without its basis
	// is what §11.2 spent a phase removing, so the alert states it.
	SealedThrough *time.Time `json:"sealed_through,omitempty"`
}

// ServiceAlertSignal names which of the two signals fired.
type ServiceAlertSignal string

const (
	// ServiceSignalHealth is the LIVE transition: the outage page.
	ServiceSignalHealth ServiceAlertSignal = "health"
	// ServiceSignalBurn is the SEALED budget signal, late by construction.
	ServiceSignalBurn ServiceAlertSignal = "burn"
)

// AlertSuppressionReason names why a monitor's alert was not delivered.
type AlertSuppressionReason string

// SuppressionServiceDelegation is §16.1's reason: a service that owns paging answers for this
// monitor. It is recorded per suppressed delivery, because a suppressed alert leaves no trace in
// the outbox or the channels, and "did anyone get told?" is the first question after an incident.
const SuppressionServiceDelegation AlertSuppressionReason = "service_delegation"

// Message renders the human-readable notification. It states which signal it is, because the two
// have different latencies and an operator holding a page must know whether it describes now or a
// budget over sealed time.
func (a ServiceAlert) Message() string {
	if a.Signal == ServiceSignalBurn {
		if !a.Firing {
			return fmt.Sprintf("✅ %s: error-budget burn back under %.0f× (%s window, %s).",
				a.ServiceName, a.Threshold, a.Window, a.closeSuffix())
		}
		basis := "sealed facts"
		if a.SealedThrough != nil {
			basis = "facts sealed through " + a.SealedThrough.UTC().Format("15:04 MST")
		}
		return fmt.Sprintf("🔥 %s is burning its %s budget %.1f× too fast (objective %.3f%%, %s — this signal trails the seal watermark).",
			a.ServiceName, a.Window, a.BurnRate, a.Objective, basis)
	}
	if !a.Firing {
		return fmt.Sprintf("✅ %s: alert closed (%s).", a.ServiceName, a.closeSuffix())
	}
	inputs := ""
	if a.TotalInputs > 0 {
		inputs = fmt.Sprintf(" — %d of %d reliability inputs failing", a.FailingInputs, a.TotalInputs)
	}
	return fmt.Sprintf("🚨 %s is %s%s (confirmed over %d evaluations).",
		a.ServiceName, a.State, inputs, a.ConfirmedOver)
}

// closeSuffix renders WHY an announcement ended. Only a return to healthy is a recovery; the others
// are statements about what cerbix can say, and a channel must be able to tell them apart.
func (a ServiceAlert) closeSuffix() string {
	switch a.CloseReason {
	case CloseRecovered:
		return "recovered"
	case CloseVisibilityLost:
		return "no longer measurable — this is not a recovery"
	case CloseEnteredMaintenance:
		return "entered a declared maintenance window — this is not a recovery"
	case CloseOwnershipDisabled:
		return "paging ownership was turned off for this service"
	case ClosePolicyChanged:
		return "the paging policy no longer covers this state"
	case CloseBurnDisabled:
		return "burn alerting was disabled for this service"
	case CloseRuleRemoved:
		return "the burn rule was removed"
	case CloseServiceDeleted:
		return "the service was deleted"
	default:
		return string(a.CloseReason)
	}
}
