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

// ServiceAlertDecision is what one evaluation concluded.
type ServiceAlertDecision struct {
	// Notify is true when an event must be enqueued.
	Notify bool
	// Recovery marks a notification that announces the END of a paged state rather than its start.
	Recovery bool
	// NextNotified is the value to store in `notified_state` when Notify is true. It is the state
	// that was ANNOUNCED, which is the only thing that makes the next edge computable.
	NextNotified ServiceAlertState
	// Reason is why nothing was sent, for the operator-facing view and the tests. An empty Reason
	// with Notify=false is impossible: silence always has a name.
	Reason string
}

// DecideServiceAlert is the edge rule, and it is pure.
//
//	current   — what this evaluation observed
//	streak    — how many consecutive evaluations have observed `current`, including this one
//	notified  — what the last DELIVERED notification announced, or "" if nothing ever has
//
// The three cases that make this subtler than "did the state change":
//
//   - a service whose FIRST observed state is not pageable must not announce anything, and must not
//     record a recovery it never paged for. `notified == ""` therefore stays empty until something
//     pageable is actually announced;
//   - a move from one pageable state to another (degraded → down) is a new announcement, not a
//     recovery followed by a page;
//   - a move from a pageable state to a NON-pageable one (down → excluded, down → unknown with the
//     switch off) is a RECOVERY of the paged state, because the operator was told something was
//     wrong and is owed the end of it — even though the new state itself would never have paged.
func DecideServiceAlert(
	p ServiceAlertPolicy, current ServiceAlertState, streak int, notified ServiceAlertState,
) ServiceAlertDecision {
	if !p.OwnsPaging {
		return ServiceAlertDecision{Reason: "service does not own paging"}
	}
	if streak < p.ConfirmEvaluations {
		return ServiceAlertDecision{
			Reason: fmt.Sprintf("state %s not confirmed yet (%d of %d evaluations)",
				current, streak, p.ConfirmEvaluations),
		}
	}
	pageable := p.Pages(current)
	switch {
	case notified == current:
		return ServiceAlertDecision{Reason: fmt.Sprintf("%s already announced", current)}
	case pageable:
		// A new pageable state — whether from healthy, from nothing, or from another pageable one.
		return ServiceAlertDecision{Notify: true, NextNotified: current}
	case notified == "":
		// Nothing was ever announced and this state does not page: there is nothing to recover from.
		return ServiceAlertDecision{Reason: fmt.Sprintf("%s does not page", current)}
	default:
		// Something WAS announced and the service has left it. The operator is owed the end.
		return ServiceAlertDecision{Notify: true, Recovery: true, NextNotified: current}
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
	// Firing distinguishes the onset from the recovery for BOTH signals.
	Firing bool `json:"firing"`

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
