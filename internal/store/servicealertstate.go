package store

import (
	"context"
	"fmt"
	"time"
)

// FR-021 §16.1/§16.6b — what a service's coverage is doing RIGHT NOW, per signal, for the UI.
//
// The dangerous way to build this is to ask the same question in different words. `ActiveDelegation`
// already owns "armed", and a badge that decided it independently would eventually say ARMED while
// delivery said otherwise — which is worse than no badge, because it would be believed. So this read
// is composed from the SAME predicate constants the delegation lookup is composed from
// (`routablePredicate`, `burnReplacementExistsSQL`, `burnNothingBlindSQL`,
// `burnEveryRuleLatchedSQL`, `effectiveRevisionSQL`), and `Armed` is their conjunction rather than a
// separate opinion. The regression that matters asserts exactly that: for every dis-arming cause,
// this read and `ActiveDelegation` must agree.
//
// What this read adds is the REASON. Delegation only needs a yes or no; an operator looking at a
// service needs to know WHICH clause is unsatisfied, because "not armed" has eleven causes and they
// call for different actions — turn ownership on, wait for the next evaluation, fix a route, look at
// an evaluation error.

// Dis-arming reasons. FIXED and low-cardinality: §16.6b forbids unbounded label values on this
// surface, and the UI renders these as text it must be able to translate.
const (
	// AlertReasonNotOwned: the service does not declare ownership at all.
	AlertReasonNotOwned = "not_owned"
	// AlertReasonPolicyPagesNothing: `page_on = {}` with `page_on_unknown` off — legal, and it
	// means the live signal replaces nothing.
	AlertReasonPolicyPagesNothing = "policy_pages_nothing"
	// AlertReasonNeverEvaluated: no verdict exists yet. Absence of evidence is not coverage.
	AlertReasonNeverEvaluated = "never_evaluated"
	// AlertReasonGenerationChanged: the verdict is for a configuration that has been replaced.
	AlertReasonGenerationChanged = "generation_changed"
	// AlertReasonRevisionChanged: the verdict was computed from a declaration that no longer
	// governs — a service still measuring the previous definition.
	AlertReasonRevisionChanged = "revision_changed"
	// AlertReasonEvaluationError: the last evaluation failed and said why.
	AlertReasonEvaluationError = "evaluation_error"
	// AlertReasonStaleLease: the last successful evaluation is too old to speak for now.
	AlertReasonStaleLease = "stale_lease"
	// AlertReasonNoEnabledTarget: no enabled burn target with rules exists.
	AlertReasonNoEnabledTarget = "no_enabled_target"
	// AlertReasonHeld: a rule's window could not be quoted, so the rule cannot fire (§16.4).
	AlertReasonHeld = "held"
	// AlertReasonRuleUnevaluated: a declared rule has no verdict of its own yet.
	AlertReasonRuleUnevaluated = "rule_unevaluated"
	// AlertReasonUnroutable: nothing this service could notify resolves right now.
	AlertReasonUnroutable = "unroutable"
)

// ServiceSignalState is one signal's coverage.
type ServiceSignalState struct {
	Armed bool
	// Reason names the FIRST unsatisfied clause, in the conjunction's own order, so the answer is
	// stable and points at the nearest cause. Empty exactly when Armed.
	Reason string
	// EvaluatedAt is the last SUCCESSFUL evaluation, nil when there has never been one.
	EvaluatedAt *time.Time
	LeaseUntil  *time.Time
	// LastError is the evaluator's own message, surfaced rather than summarized.
	LastError string
}

// ServiceAlertingState is both signals. They are independent: one may be armed while the other is
// not, which is the whole point of per-signal delegation.
type ServiceAlertingState struct {
	Live ServiceSignalState
	Burn ServiceSignalState
}

// serviceAlertingStateSQL asks each clause of both conjunctions as its own boolean, over one row.
//
// Selecting the clauses instead of their AND is what makes a REASON possible without writing the
// conjunction twice: `Armed` below is the conjunction of exactly these columns, and the columns are
// the delegation lookup's own predicate text.
var serviceAlertingStateSQL = `
	SELECT s.owns_paging,
	       (cardinality(s.page_on) > 0 OR s.page_on_unknown),
	       ` + routablePredicate + `,
	       st.service_id IS NOT NULL,
	       COALESCE(st.config_generation = s.alert_config_generation, false),
	       COALESCE(st.revision_id IS NOT NULL AND st.revision_id = (` + effectiveRevisionSQL + `), false),
	       COALESCE(st.last_error, ''),
	       COALESCE(now() < st.lease_until, false),
	       st.evaluated_at, st.lease_until,
	       EXISTS (SELECT 1 FROM sla_targets t
	                WHERE t.service_id = s.id AND t.burn_alert_enabled
	                  AND t.burn_rules <> '[]'::jsonb),
	       ` + burnReplacementExistsSQL + `,
	       ` + burnNothingBlindSQL + `,
	       ` + burnEveryRuleLatchedSQL + `,
	       (SELECT count(*) FROM service_burn_alert_state b
	         WHERE b.service_id = s.id AND b.project_id = s.project_id
	           AND b.last_verdict = 'hold') > 0,
	       EXISTS (SELECT 1 FROM service_burn_alert_state b
	                 JOIN sla_targets t ON t.id = b.sla_target_id
	                WHERE b.service_id = s.id AND b.project_id = s.project_id
	                  AND (b.config_generation <> s.alert_config_generation
	                       OR b.target_generation <> t.alert_generation)),
	       (SELECT min(b.evaluated_at) FROM service_burn_alert_state b
	         WHERE b.service_id = s.id AND b.project_id = s.project_id),
	       (SELECT min(b.lease_until) FROM service_burn_alert_state b
	         WHERE b.service_id = s.id AND b.project_id = s.project_id),
	       COALESCE((SELECT string_agg(DISTINCT b.last_error, '; ')
	                   FROM service_burn_alert_state b
	                  WHERE b.service_id = s.id AND b.project_id = s.project_id
	                    AND b.last_error IS NOT NULL), '')
	  FROM services s
	  LEFT JOIN service_alert_state st ON st.service_id = s.id AND st.project_id = s.project_id
	 WHERE s.id = $1 AND s.project_id = $2`

// ServiceAlertingState reads both signals' coverage for one service.
//
// Tenant-scoped, and a foreign, unknown or malformed id answers ErrNotFound exactly as the policy
// read and the two writers do — one answer for all three, so existence never leaks.
func (s *Store) ServiceAlertingState(
	ctx context.Context, projectID, serviceID string,
) (ServiceAlertingState, error) {
	var out ServiceAlertingState
	var (
		ownsPaging, policyPages, routable            bool
		evaluated, genOK, revOK, fresh               bool
		liveErr                                      string
		hasTarget, replacement, nothingBlind, allLat bool
		anyHold, anyGenMismatch                      bool
		burnErr                                      string
	)
	err := s.pool.QueryRow(ctx, serviceAlertingStateSQL, serviceID, projectID).Scan(
		&ownsPaging, &policyPages, &routable,
		&evaluated, &genOK, &revOK, &liveErr, &fresh,
		&out.Live.EvaluatedAt, &out.Live.LeaseUntil,
		&hasTarget, &replacement, &nothingBlind, &allLat, &anyHold, &anyGenMismatch,
		&out.Burn.EvaluatedAt, &out.Burn.LeaseUntil, &burnErr)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return ServiceAlertingState{}, ErrNotFound
	}
	if err != nil {
		return ServiceAlertingState{}, fmt.Errorf("store: read alerting state: %w", err)
	}
	out.Live.LastError, out.Burn.LastError = liveErr, burnErr

	// The order is the conjunction's own, so the reason names the nearest cause rather than the
	// most alarming one.
	switch {
	case !ownsPaging:
		out.Live.Reason = AlertReasonNotOwned
	case !policyPages:
		out.Live.Reason = AlertReasonPolicyPagesNothing
	case !evaluated:
		out.Live.Reason = AlertReasonNeverEvaluated
	case !genOK:
		out.Live.Reason = AlertReasonGenerationChanged
	case !revOK:
		out.Live.Reason = AlertReasonRevisionChanged
	case liveErr != "":
		out.Live.Reason = AlertReasonEvaluationError
	case !fresh:
		out.Live.Reason = AlertReasonStaleLease
	case !routable:
		out.Live.Reason = AlertReasonUnroutable
	default:
		out.Live.Armed = true
	}

	switch {
	case !ownsPaging:
		out.Burn.Reason = AlertReasonNotOwned
	case !hasTarget:
		out.Burn.Reason = AlertReasonNoEnabledTarget
	case !allLat:
		// A declared rule with no verdict of its own. Checked before the blindness clause because
		// "it has never run" explains more than "something is unquotable".
		out.Burn.Reason = AlertReasonRuleUnevaluated
	case !replacement || !nothingBlind:
		out.Burn.Reason = burnBlindReason(anyHold, anyGenMismatch, burnErr)
	case !routable:
		out.Burn.Reason = AlertReasonUnroutable
	default:
		out.Burn.Armed = true
	}
	return out, nil
}

// burnBlindReason distinguishes the ways a latch can fail to be a replacement, in the order
// `burnRuleCoversSQL` itself asks them: quotable, then the two generations, then the error, then the
// lease. Naming them apart matters because they call for different actions — a HOLD means the window
// cannot be quoted and will clear itself, a generation mismatch means the next evaluation of the NEW
// configuration will re-arm, an error means somebody has to look, and a stale lease means the
// evaluator is not running.
func burnBlindReason(anyHold, anyGenMismatch bool, burnErr string) string {
	switch {
	case anyHold:
		return AlertReasonHeld
	case anyGenMismatch:
		return AlertReasonGenerationChanged
	case burnErr != "":
		return AlertReasonEvaluationError
	default:
		return AlertReasonStaleLease
	}
}
