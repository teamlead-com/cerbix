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
	// AlertReasonNoOwningService: no service in the project names this monitor as an SLI of its
	// effective definition, so there is nothing that could cover it. Reported by the delivery-time
	// lookup only — the badge is always asked about a service that exists.
	AlertReasonNoOwningService = "no_owning_service"
	// AlertReasonNotOwned: the service does not declare ownership at all.
	AlertReasonNotOwned = "not_owned"
	// AlertReasonPolicyPagesNothing: `page_on = {}` with `page_on_unknown` off — legal, and it means
	// the live signal replaces nothing whatever the service is doing.
	AlertReasonPolicyPagesNothing = "policy_pages_nothing"
	// AlertReasonStateNotPageable: the policy does not cover the state the service is IN. It may page
	// plenty — `page_on = {down}` while the service sits at DEGRADED is the ordinary case — and the
	// distinction matters because "pages nothing" and "does not page THIS" have different fixes.
	// Supersedes the older `policy_pages_nothing`, which asked whether the policy pages anything at
	// all and therefore armed coverage for states nobody had opted into (§16.1).
	AlertReasonStateNotPageable = "state_not_pageable"
	// AlertReasonOnsetPending: the state IS pageable and the announcement has not been committed yet
	// — the window D-0176 opens deliberately when an onset is withheld for want of a recipient.
	// Coverage begins when the onset does, not when the route comes back.
	AlertReasonOnsetPending = "onset_pending"
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
	// AlertReasonLatchInconsistent: an eligible burn latch is in a state the evaluator does not
	// write — today that means `clear` while still marked firing — so the row cannot be interpreted
	// as either a live announcement or a finished one. It is also the classifier's LAST case: if the
	// burn conjunction refused and no legitimate clause explains it, the latch is by construction
	// uninterpretable and saying so beats naming a cause that is not the cause. This is a defect or
	// legacy/corrupt state, never a configuration an operator chose (D-0178).
	AlertReasonLatchInconsistent = "latch_inconsistent"
	// AlertReasonOnsetUndelivered: the announcement WAS made and reached nobody. The worker resolved
	// zero recipients and stopped, because no retry reaches a channel that has been deleted, and the
	// latch stays firing so no further edge is coming. Coverage means somebody was told (D-0179).
	AlertReasonOnsetUndelivered = "onset_undelivered"
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
// serviceCoverageClausesSQL is every clause of both conjunctions as its own boolean, in the order the
// verdict functions below read them. It is shared TEXT rather than two copies because the badge and
// the delivery-time lookup must answer the same question the same way: a badge that says ARMED while
// delivery suppresses (or the reverse) is a bug an operator cannot even describe.
const serviceCoverageClausesSQL = `
	       s.owns_paging,
	       (cardinality(s.page_on) > 0 OR s.page_on_unknown),
	       COALESCE(` + livePageableStateSQL + `, false),
	       COALESCE(` + liveOnsetCommittedSQL + `, false),
	       COALESCE(` + liveOnsetDeliveredSQL + `, false),
	       ` + routablePredicate + `,
	       st.service_id IS NOT NULL,
	       COALESCE(st.config_generation = s.alert_config_generation, false),
	       COALESCE(st.revision_id IS NOT NULL AND st.revision_id = (` + effectiveRevisionSQL + `), false),
	       COALESCE(st.last_error, ''),
	       COALESCE(now() < st.lease_until, false),
	       EXISTS (SELECT 1 FROM sla_targets t
	                WHERE t.service_id = s.id AND t.burn_alert_enabled
	                  AND t.burn_rules <> '[]'::jsonb),
	       ` + burnReplacementExistsSQL + `,
	       ` + burnNothingBlindSQL + `,
	       ` + burnEveryRuleLatchedSQL + `,
	       NOT EXISTS (SELECT 1 FROM service_burn_alert_state b
	                     JOIN sla_targets t ON t.id = b.sla_target_id
	                    WHERE b.service_id = s.id AND b.project_id = s.project_id
	                      AND ` + burnEligibleTargetSQL + `
	                      AND now() >= b.lease_until),
	       EXISTS (SELECT 1 FROM service_burn_alert_state b
	                 JOIN sla_targets t ON t.id = b.sla_target_id
	                WHERE b.service_id = s.id AND b.project_id = s.project_id
	                  AND ` + burnEligibleTargetSQL + `
	                  AND b.last_verdict = 'fire' AND NOT (b.firing AND b.emitted_seq > 0)),
	       EXISTS (SELECT 1 FROM service_burn_alert_state b
	                 JOIN sla_targets t ON t.id = b.sla_target_id
	                WHERE b.service_id = s.id AND b.project_id = s.project_id
	                  AND ` + burnEligibleTargetSQL + `
	                  AND b.last_verdict = 'fire' AND b.firing AND b.emitted_seq > 0
	                  AND b.delivered_seq < b.emitted_seq),
	       EXISTS (SELECT 1 FROM service_burn_alert_state b
	                 JOIN sla_targets t ON t.id = b.sla_target_id
	                WHERE b.service_id = s.id AND b.project_id = s.project_id
	                  AND ` + burnEligibleTargetSQL + `
	                  AND b.last_verdict = 'hold'),
	       EXISTS (SELECT 1 FROM service_burn_alert_state b
	                 JOIN sla_targets t ON t.id = b.sla_target_id
	                WHERE b.service_id = s.id AND b.project_id = s.project_id
	                  AND ` + burnEligibleTargetSQL + `
	                  AND (b.config_generation <> s.alert_config_generation
	                       OR b.target_generation <> t.alert_generation)),
	       COALESCE((SELECT string_agg(DISTINCT b.last_error, '; ')
	                   FROM service_burn_alert_state b
	                   JOIN sla_targets t ON t.id = b.sla_target_id
	                  WHERE b.service_id = s.id AND b.project_id = s.project_id
	                    AND ` + burnEligibleTargetSQL + `
	                    AND b.last_error IS NOT NULL), '')`

// burnEligibleTargetSQL is the SAME eligibility the burn conjunction uses (`burnReplacementExistsSQL`
// and its two siblings): a target of THIS service with burn alerting on. The diagnostic aggregates
// above must be scoped by it, not merely by service.
//
// They were not, and it made the reason lie in exactly the state where a reason matters. A latch left
// behind on a DISABLED target — the supported write path prunes those, but nothing in the schema
// enforces it, so legacy rows, a repair, or a half-finished migration can leave one — reported `held`
// for a service whose ENABLED target was simply stale. The operator is then sent to look at a window
// that cannot fire, for a target that is not part of the answer.
const burnEligibleTargetSQL = `t.service_id = s.id AND t.burn_alert_enabled`

// serviceCoverageClauses is one service's answers, in the same order.
type serviceCoverageClauses struct {
	ownsPaging, policyPages, pageableState, onsetCommitted, routable bool
	onsetDelivered                                                   bool
	evaluated, genOK, revOK                                          bool
	liveErr                                                          string
	fresh                                                            bool
	hasTarget, replacement, nothingBlind, allLatched                 bool
	burnFresh, burnOnsetPending, burnOnsetUndelivered                bool
	anyHold, anyGenMismatch                                          bool
	burnErr                                                          string
}

// scanInto returns the destinations for `serviceCoverageClausesSQL`, in its column order.
func (c *serviceCoverageClauses) scanInto() []any {
	return []any{
		&c.ownsPaging, &c.policyPages, &c.pageableState, &c.onsetCommitted, &c.onsetDelivered,
		&c.routable,
		&c.evaluated, &c.genOK, &c.revOK, &c.liveErr, &c.fresh,
		&c.hasTarget, &c.replacement, &c.nothingBlind, &c.allLatched,
		&c.burnFresh, &c.burnOnsetPending, &c.burnOnsetUndelivered,
		&c.anyHold, &c.anyGenMismatch, &c.burnErr,
	}
}

// liveVerdict is §16.1's LIVE conjunction and its reason, in one place. The ORDER of the cases is the
// conjunction's own, so the reason names the nearest cause rather than the most alarming one.
func (c serviceCoverageClauses) liveVerdict() (bool, string) {
	switch {
	case !c.ownsPaging:
		return false, AlertReasonNotOwned
	case !c.evaluated:
		return false, AlertReasonNeverEvaluated
	case !c.policyPages:
		return false, AlertReasonPolicyPagesNothing
	case !c.pageableState:
		return false, AlertReasonStateNotPageable
	case !c.genOK:
		return false, AlertReasonGenerationChanged
	case !c.revOK:
		return false, AlertReasonRevisionChanged
	case c.liveErr != "":
		return false, AlertReasonEvaluationError
	case !c.fresh:
		return false, AlertReasonStaleLease
	case !c.routable:
		return false, AlertReasonUnroutable
	case !c.onsetCommitted:
		return false, AlertReasonOnsetPending
	case !c.onsetDelivered:
		return false, AlertReasonOnsetUndelivered
	default:
		return true, ""
	}
}

// burnVerdict is the same for the SEALED signal.
//
// The cases below are `burnRuleCoversSQL` taken apart. That predicate folds FIVE independent facts
// into one boolean — the verdict/firing/sequence shape, both generations, a recorded error and the
// lease — and for a while the reason was re-derived from three aggregates that did not map onto them,
// with `stale_lease` as the default for everything left over. So the ordinary D-0176 shape (a FIRE
// withheld for want of a route: `fire`, not firing, fresh lease) was reported as a stalled evaluator,
// which sends an operator to the scheduler when the thing to fix is a notification channel. Every
// fact the covers predicate uses now has its own clause and its own name.
//
// Routability is checked BEFORE the pending onset, exactly as `liveVerdict` does: while there is no
// route the actionable truth is the route, and `onset_pending` is what remains once it is back and
// the next evaluation has yet to announce.
func (c serviceCoverageClauses) burnVerdict() (bool, string) {
	switch {
	case !c.ownsPaging:
		return false, AlertReasonNotOwned
	case !c.hasTarget:
		return false, AlertReasonNoEnabledTarget
	case !c.allLatched:
		return false, AlertReasonRuleUnevaluated
	case !c.replacement || !c.nothingBlind:
		// ONE gate decides coverage — `burnRuleCoversSQL`, through these two clauses — and the
		// switch below only NAMES the refusal. They were briefly parallel: the diagnostics were
		// hoisted out to sit beside the gate, so each fact was decided twice and a mutation
		// weakening the gate left the answer unchanged because the diagnostic still fired. Two
		// implementations of one rule agree until they don't, which is the defect §34 fixed between
		// the badge and delivery and then reintroduced here.
		//
		// Every case is a fact the gate already uses, so each is reachable only when the gate has
		// refused, and the order is the gate's own.
		switch {
		case c.anyHold:
			return false, AlertReasonHeld
		case c.anyGenMismatch:
			return false, AlertReasonGenerationChanged
		case c.burnErr != "":
			return false, AlertReasonEvaluationError
		case !c.burnFresh:
			return false, AlertReasonStaleLease
		case c.burnOnsetPending && !c.routable:
			// Routability before the pending onset, exactly as `liveVerdict` orders them: while
			// there is no route the actionable truth is the route.
			return false, AlertReasonUnroutable
		case c.burnOnsetPending:
			return false, AlertReasonOnsetPending
		case c.burnOnsetUndelivered:
			return false, AlertReasonOnsetUndelivered
		default:
			// Everything the evaluator can legitimately write is named above, so a latch that still
			// fails the gate is in a shape it does not produce — `clear` while still marked firing.
			return false, AlertReasonLatchInconsistent
		}
	case !c.routable:
		return false, AlertReasonUnroutable
	default:
		return true, ""
	}
}

var serviceAlertingStateSQL = `
	SELECT` + serviceCoverageClausesSQL + `,
	       st.evaluated_at, st.lease_until,
	       (SELECT min(b.evaluated_at) FROM service_burn_alert_state b
	                 JOIN sla_targets t ON t.id = b.sla_target_id
	         WHERE b.service_id = s.id AND b.project_id = s.project_id
	           AND ` + burnEligibleTargetSQL + `),
	       (SELECT min(b.lease_until) FROM service_burn_alert_state b
	                 JOIN sla_targets t ON t.id = b.sla_target_id
	         WHERE b.service_id = s.id AND b.project_id = s.project_id
	           AND ` + burnEligibleTargetSQL + `)
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
	var clauses serviceCoverageClauses
	dest := append(clauses.scanInto(),
		&out.Live.EvaluatedAt, &out.Live.LeaseUntil, &out.Burn.EvaluatedAt, &out.Burn.LeaseUntil)
	err := s.pool.QueryRow(ctx, serviceAlertingStateSQL, serviceID, projectID).Scan(dest...)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return ServiceAlertingState{}, ErrNotFound
	}
	if err != nil {
		return ServiceAlertingState{}, fmt.Errorf("store: read alerting state: %w", err)
	}
	out.Live.LastError, out.Burn.LastError = clauses.liveErr, clauses.burnErr

	// ONE decision for both surfaces. The badge used to carry its own copy of these two switches and
	// the delivery lookup carried none at all, which is how `no_active_owner` became the only thing an
	// operator was ever told at delivery time while the badge knew exactly which clause had failed.
	out.Live.Armed, out.Live.Reason = clauses.liveVerdict()
	out.Burn.Armed, out.Burn.Reason = clauses.burnVerdict()

	return out, nil
}
