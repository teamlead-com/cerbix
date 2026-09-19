package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// The decision (func-reliability-gate D1, D4, D6a, D7, D8a, D9, D10; invariants 1–7, 9, 12,
// 15, 16, 19).
//
// ONE transaction, REPEATABLE READ and read-write, inside the store's deadline wrapper so the
// wrapper's `SET LOCAL`s — utility commands that establish no snapshot — run before the FIRST
// snapshot-bearing statement, `SELECT statement_timestamp()`, whose value is `evaluated_at`.
// Inside it, in order: the service row (tenant check), the live policy, the active override,
// the report path for the policy's window at evaluated_at, the window's target and its burn
// latches, the open-incident predicate, the coverage clauses, the governing revision, then
// the ledger INSERT and commit. Every fact is consumed from its owner through a
// transaction-taking variant; the gate computes NO reliability fact and reads no heartbeat.
// A serialization failure retries the whole transaction once; a second one is
// ErrGateSnapshotConflict, a transport error and never a decision.

// gateDecisionHook is a test seam, nil in production: called inside the decision transaction
// at two phases — after the policy read (gatePhasePolicyRead) and after every owner read,
// before the ledger INSERT (gatePhaseReadsDone) — with the attempt number. It lets a test
// mutate the world between reads to prove the snapshot, force a serialization failure on
// the first attempt, or hold the transaction open across a listing page.
var gateDecisionHook func(ctx context.Context, attempt int, phase string, tx pgx.Tx) error

const (
	gatePhasePolicyRead = "policy_read"
	gatePhaseReadsDone  = "reads_done"
)

func runGateDecisionHook(ctx context.Context, attempt int, phase string, tx pgx.Tx) error {
	if gateDecisionHook == nil {
		return nil
	}
	return gateDecisionHook(ctx, attempt, phase, tx)
}

// gateTarget is the policy's window's service-scoped `sla_targets` row.
type gateTarget struct {
	id                 string
	window             string
	objective          float64
	objectiveUpdatedAt time.Time
	rules              []domain.BurnRule
	burnAlertEnabled   bool
}

// gateClauseInputs is everything the D4 evaluation reads — all from one snapshot.
type gateClauseInputs struct {
	policy      domain.GatePolicy
	evaluatedAt time.Time
	report      domain.ServiceWindowReport
	// target is nil when the service has no target for the policy's window (D2).
	target  *gateTarget
	latches map[burnLatchKey]burnRuleLatch
	// incident is the open auto-incident, nil when none.
	incident *domain.Incident
}

// Owner names in reasons[].source.
const (
	gateSourceReportBudget = "service_reliability_report:budget.burned_percent"
	gateSourceBurnLatch    = "service_burn_alert_state"
	gateSourceIncidents    = "incidents"
	gateSourceRevisions    = "service_definition_revisions"
	gateSourceCoverage     = "service_alert_state"
)

// DecideGate is the reliability gate's one question for one service: the decision, recorded
// in the ledger, under the begin-through-commit `budget` (gate.evaluate_tx_budget_ms).
//
// Errors: ErrNotFound (foreign, unknown or malformed service), ErrGateSnapshotConflict (two
// serialization failures), ErrGateLedgerUnwritable (no partition for evaluated_at),
// ErrGateBudgetExceeded (the budget ran out), or a wrapped error. NOT_CONFIGURED is a decision,
// not an error.
func (s *Store) DecideGate(ctx context.Context, projectID, serviceID string, budget time.Duration) (domain.GateDecision, error) {
	deadline := time.Now().Add(budget)
	for attempt := 0; ; attempt++ {
		dec, err := s.decideGateOnce(ctx, projectID, serviceID, deadline, attempt)
		switch {
		case err == nil:
			return dec, nil
		case isSerializationFailure(err) && attempt == 0:
			continue
		case isSerializationFailure(err):
			return domain.GateDecision{}, fmt.Errorf("%w: %v", ErrGateSnapshotConflict, err)
		case errors.Is(err, errSliceBudget) || isStatementTimeout(err) || errors.Is(err, context.DeadlineExceeded):
			return domain.GateDecision{}, fmt.Errorf("%w: %v", ErrGateBudgetExceeded, err)
		default:
			return domain.GateDecision{}, err
		}
	}
}

func (s *Store) decideGateOnce(ctx context.Context, projectID, serviceID string, deadline time.Time, attempt int) (domain.GateDecision, error) {
	// The budget is begin-through-commit (§5a): the SAME absolute deadline bounds the pool
	// acquisition and BEGIN, not only the statements the deadline wrapper times after Begin
	// succeeds — under pool exhaustion the request would otherwise wait past the contract before
	// any per-statement bound existed (review [20]).
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	rawTx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: begin gate decision: %w", err)
	}
	defer rawTx.Rollback(ctx) //nolint:errcheck // no-op after commit
	tx := newDeadlineTx(rawTx, deadline, 0)

	// The linearization point (D6a): the first snapshot-bearing statement.
	var evaluatedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&evaluatedAt); err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: gate decision clock: %w", err)
	}
	evaluatedAt = evaluatedAt.UTC()

	// (1) The service row: tenant check. Foreign, unknown and malformed answer alike.
	var slug, name string
	err = tx.QueryRow(ctx, `SELECT slug, name FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).Scan(&slug, &name)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.GateDecision{}, ErrNotFound
	}
	if err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: gate decision service: %w", err)
	}

	id, err := newGateDecisionID(evaluatedAt)
	if err != nil {
		return domain.GateDecision{}, err
	}
	dec := domain.GateDecision{
		SchemaVersion: domain.GateDecisionSchemaV1,
		DecisionID:    id,
		EvaluatedAt:   evaluatedAt,
		ServiceID:     &serviceID,
		ServiceSlug:   slug,
		ServiceName:   name,
		Reasons:       []domain.GateReasonEntry{},
	}

	// (2) The policy. Absent or tombstoned → NOT_CONFIGURED: a recorded decision with no action
	// and no evidence beyond itself (D4, D7).
	effective, effectiveErr := effectiveGatePolicyTx(ctx, tx, projectID, serviceID)
	if err := runGateDecisionHook(ctx, attempt, gatePhasePolicyRead, tx); err != nil {
		return domain.GateDecision{}, err
	}
	if errors.Is(effectiveErr, ErrGatePolicyNotConfigured) {
		dec.State = domain.GateStateNotConfigured
		dec.Reasons = []domain.GateReasonEntry{{Code: string(domain.GateReasonNotConfigured), Docs: domain.GateDocsURL}}
		if err := runGateDecisionHook(ctx, attempt, gatePhaseReadsDone, tx); err != nil {
			return domain.GateDecision{}, err
		}
		if err := insertGateDecisionTx(ctx, tx, projectID, dec, nil); err != nil {
			return domain.GateDecision{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.GateDecision{}, fmt.Errorf("store: commit gate decision: %w", err)
		}
		return dec, nil
	}
	if effectiveErr != nil {
		return domain.GateDecision{}, effectiveErr
	}
	policy := effective.Policy
	dec.SchemaVersion = policy.SchemaVersion
	source, ownerID := effective.Source, effective.OwnerID
	dec.PolicySource, dec.PolicyOwnerID = &source, &ownerID
	if policy.SchemaVersion == domain.GatePolicySchemaV2 && policy.WindowMode == domain.GateWindowModeAll {
		return s.decideAllWindowsGateTx(ctx, tx, attempt, projectID, serviceID, evaluatedAt, dec, policy, effective)
	}
	window, ok := sla.WindowByName(policy.Window)
	if !ok {
		return domain.GateDecision{}, fmt.Errorf("store: gate policy window %q is not an SLA window", policy.Window)
	}

	// (3) The active override at evaluated_at, against the live revision.
	override, err := activeGateOverrideTx(ctx, tx, serviceID, evaluatedAt, effective)
	if err != nil {
		return domain.GateDecision{}, err
	}

	// (4) The report path for the policy's window — the budget owner and the withholding owner.
	report, err := s.serviceReliabilityReportTx(ctx, tx, projectID, serviceID, window, evaluatedAt)
	if err != nil {
		return domain.GateDecision{}, err
	}

	// (5) The window's target and its burn latches (D1: THAT window's target only).
	target, err := gateTargetTx(ctx, tx, serviceID, policy.Window)
	if err != nil {
		return domain.GateDecision{}, err
	}
	var latches map[burnLatchKey]burnRuleLatch
	if target != nil {
		if latches, err = burnLatchTx(ctx, tx, []string{target.id}); err != nil {
			return domain.GateDecision{}, err
		}
	}

	// (6) The open-incident predicate.
	var incident *domain.Incident
	switch inc, err := s.FindOpenAutoIncidentByService(ctx, tx, serviceID); {
	case err == nil:
		incident = &inc
	case errors.Is(err, ErrNotFound):
	default:
		return domain.GateDecision{}, err
	}

	// (7) Coverage, as evidence only (D11).
	coverage, err := serviceAlertingStateOn(ctx, tx, projectID, serviceID)
	if err != nil {
		return domain.GateDecision{}, err
	}

	// (8) The declaration in force at evaluated_at.
	governing, err := governingRevisionTx(ctx, tx, serviceID, evaluatedAt)
	if err != nil {
		return domain.GateDecision{}, err
	}

	if err := runGateDecisionHook(ctx, attempt, gatePhaseReadsDone, tx); err != nil {
		return domain.GateDecision{}, err
	}

	// The algebra and the evidence, pure from here on.
	in := gateClauseInputs{policy: policy, evaluatedAt: evaluatedAt, report: report, target: target, latches: latches, incident: incident}
	assembleGateDecision(&dec, in, override, coverage, governing)

	if err := insertGateDecisionTx(ctx, tx, projectID, dec, &policy); err != nil {
		return domain.GateDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: commit gate decision: %w", err)
	}
	return dec, nil
}

// gateTargetTx reads the policy's window's service-scoped target, nil when the service has
// none (D2: window_target_missing).
func gateTargetTx(ctx context.Context, tx pgx.Tx, serviceID, window string) (*gateTarget, error) {
	var t gateTarget
	var rules []byte
	err := tx.QueryRow(ctx, `
		SELECT id, window_name, objective::float8, updated_at, burn_rules, burn_alert_enabled
		  FROM sla_targets WHERE service_id = $1 AND window_name = $2`, serviceID, window).
		Scan(&t.id, &t.window, &t.objective, &t.objectiveUpdatedAt, &rules, &t.burnAlertEnabled)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: gate decision target: %w", err)
	}
	t.objectiveUpdatedAt = t.objectiveUpdatedAt.UTC()
	if err := json.Unmarshal(rules, &t.rules); err != nil {
		return nil, fmt.Errorf("store: gate decision burn rules: %w", err)
	}
	return &t, nil
}

func (s *Store) decideAllWindowsGateTx(
	ctx context.Context, tx pgx.Tx, attempt int, projectID, serviceID string, evaluatedAt time.Time,
	dec domain.GateDecision, policy domain.GatePolicy, effective domain.EffectiveGatePolicy,
) (domain.GateDecision, error) {
	override, err := activeGateOverrideTx(ctx, tx, serviceID, evaluatedAt, effective)
	if err != nil {
		return domain.GateDecision{}, err
	}
	targets, err := gateTargetsTx(ctx, tx, serviceID)
	if err != nil {
		return domain.GateDecision{}, err
	}
	targetIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		targetIDs = append(targetIDs, target.id)
	}
	latches, err := burnLatchTx(ctx, tx, targetIDs)
	if err != nil {
		return domain.GateDecision{}, err
	}
	var incident *domain.Incident
	switch inc, err := s.FindOpenAutoIncidentByService(ctx, tx, serviceID); {
	case err == nil:
		incident = &inc
	case errors.Is(err, ErrNotFound):
	default:
		return domain.GateDecision{}, err
	}
	coverage, err := serviceAlertingStateOn(ctx, tx, projectID, serviceID)
	if err != nil {
		return domain.GateDecision{}, err
	}
	governing, err := governingRevisionTx(ctx, tx, serviceID, evaluatedAt)
	if err != nil {
		return domain.GateDecision{}, err
	}

	reports, err := gateAllWindowReportsTx(ctx, tx, projectID, serviceID, targets, evaluatedAt)
	if err != nil {
		return domain.GateDecision{}, err
	}
	inputs := make([]gateClauseInputs, 0, len(targets))
	for index := range targets {
		target := &targets[index]
		inputs = append(inputs, gateClauseInputs{policy: policy, evaluatedAt: evaluatedAt, report: reports[index], target: target, latches: latches})
	}
	if err := runGateDecisionHook(ctx, attempt, gatePhaseReadsDone, tx); err != nil {
		return domain.GateDecision{}, err
	}

	verdicts := make([]domain.GateClauseVerdict, 0, len(targets)*4+1)
	evaluated := make([]domain.GateEvaluatedWindow, 0, len(inputs))
	for _, input := range inputs {
		windowVerdicts := windowScopedGateVerdicts(evaluateGateClauses(input), input.target)
		_, _, reasons := domain.DecideGateAlgebra(windowVerdicts, policy.UnknownBehavior)
		evaluated = append(evaluated, gateEvaluatedWindowOf(input, reasons))
		verdicts = append(verdicts, windowVerdicts...)
	}
	if len(inputs) == 0 {
		verdicts = append(verdicts, allWindowsMissingObjectiveVerdicts(policy)...)
	}
	incidentVerdict := domain.GateClauseVerdict{
		Clause: domain.ClauseServiceIncidentOpen, Assignment: policy.Clauses[domain.ClauseServiceIncidentOpen], Source: gateSourceIncidents,
	}
	if incident != nil {
		incidentVerdict.Matched, incidentVerdict.Value = true, incident.ID
	}
	verdicts = append(verdicts, incidentVerdict)
	assembleAllWindowsGateDecision(&dec, policy, verdicts, evaluated, inputs, override, coverage, governing)

	if err := insertGateDecisionTx(ctx, tx, projectID, dec, &policy); err != nil {
		return domain.GateDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: commit all-window gate decision: %w", err)
	}
	return dec, nil
}

func windowScopedGateVerdicts(verdicts []domain.GateClauseVerdict, target *gateTarget) []domain.GateClauseVerdict {
	out := make([]domain.GateClauseVerdict, 0, len(verdicts)-1)
	for _, verdict := range verdicts {
		if verdict.Clause == domain.ClauseServiceIncidentOpen {
			continue
		}
		targetID := target.id
		verdict.Window, verdict.TargetID = target.window, &targetID
		out = append(out, verdict)
	}
	return out
}

func gateEvaluatedWindowOf(in gateClauseInputs, reasons []domain.GateReasonEntry) domain.GateEvaluatedWindow {
	targetID, objective, objectiveAt := in.target.id, in.target.objective, in.target.objectiveUpdatedAt
	out := domain.GateEvaluatedWindow{
		Window: in.target.window, TargetID: &targetID, Objective: &objective, ObjectiveUpdatedAt: &objectiveAt,
		BurnLeases: gateBurnLeases(in), FactsFreshUntil: factsFreshUntil(in), Reasons: reasons,
	}
	if lag, sealed := in.report.SealLag(); sealed {
		sealedThrough, seconds := in.report.SealedThrough.UTC(), lag.Seconds()
		factRevisions := factRevisionsOf(in.report)
		out.SealedThrough, out.SealLagSeconds, out.FactRevisions = &sealedThrough, &seconds, &factRevisions
	}
	return out
}

func gateBurnLeases(in gateClauseInputs) []domain.GateBurnLease {
	if in.target == nil {
		return []domain.GateBurnLease{}
	}
	leases := make([]domain.GateBurnLease, 0, len(in.target.rules))
	for _, rule := range dedupedRules(in.target.rules) {
		lease := domain.GateBurnLease{RuleKey: rule.Key(), Severity: rule.Severity}
		if latch, ok := in.latches[burnLatchKey{targetID: in.target.id, ruleKey: rule.Key()}]; ok {
			firing, verdict := latch.firing, latch.lastVerdict
			evaluatedAt, leaseUntil := latch.evaluatedAt.UTC(), latch.leaseUntil.UTC()
			lease.Firing, lease.LastVerdict = &firing, &verdict
			lease.EvaluatedAt, lease.LeaseUntil = &evaluatedAt, &leaseUntil
			lease.Fresh = latchFresh(latch, in.evaluatedAt)
		}
		leases = append(leases, lease)
	}
	return leases
}

func allWindowsMissingObjectiveVerdicts(policy domain.GatePolicy) []domain.GateClauseVerdict {
	out := make([]domain.GateClauseVerdict, 0, 4)
	for _, clause := range domain.GateClausesFor(policy.SchemaVersion) {
		if clause == domain.ClauseServiceIncidentOpen {
			continue
		}
		out = append(out, domain.GateClauseVerdict{
			Clause: clause, Assignment: policy.Clauses[clause], Unavailable: domain.GateReasonNoObjective,
			Source: gateSourceReportBudget,
		})
	}
	return out
}

func assembleAllWindowsGateDecision(
	dec *domain.GateDecision, policy domain.GatePolicy, verdicts []domain.GateClauseVerdict,
	evaluated []domain.GateEvaluatedWindow, inputs []gateClauseInputs, override *domain.GateOverride,
	coverage ServiceAlertingState, governing *domain.GateGoverningRevision,
) {
	state, action, reasons := domain.DecideGateAlgebra(verdicts, policy.UnknownBehavior)
	dec.State, dec.Reasons, dec.EvaluatedWindows = state, reasons, evaluated
	dec.Action = &action
	revision, mode, unknown, lagMax := policy.Revision, domain.GateWindowModeAll, policy.UnknownBehavior, policy.MaxSealLagSeconds
	dec.PolicyRevision, dec.WindowMode = &revision, &mode
	dec.UnknownBehavior, dec.MaxSealLagSeconds = &unknown, &lagMax
	if governing != nil {
		g := *governing
		dec.GoverningRevision = &g
	} else {
		dec.Reasons = append(dec.Reasons, domain.GateReasonEntry{Code: string(domain.GateReasonNoGoverningRevision), Source: gateSourceRevisions})
	}
	if coverage.Live.EvaluatedAt != nil {
		if coverage.Live.LeaseUntil != nil {
			until := coverage.Live.LeaseUntil.UTC()
			dec.CoverageLeaseUntil = &until
		}
		dec.CoverageState = &domain.GateCoverageState{
			Live: domain.GateCoverageSignal{Armed: coverage.Live.Armed, Reason: coverage.Live.Reason},
			Burn: domain.GateCoverageSignal{Armed: coverage.Burn.Armed, Reason: coverage.Burn.Reason},
		}
	} else {
		dec.Reasons = append(dec.Reasons, domain.GateReasonEntry{Code: string(domain.GateReasonNeverEvaluated), Source: gateSourceCoverage})
	}
	for _, input := range inputs {
		fresh := factsFreshUntil(input)
		if fresh == nil || (dec.FactsFreshUntil != nil && !fresh.Before(*dec.FactsFreshUntil)) {
			continue
		}
		value := fresh.UTC()
		dec.FactsFreshUntil = &value
	}
	if override != nil && action == domain.GateActionBlock {
		was, allow := action, domain.GateActionAllow
		dec.Action, dec.UnoverriddenAction = &allow, &was
		id := override.ID
		dec.OverrideID = &id
		dec.Override = &domain.GateOverrideApplied{ID: override.ID, ActorLabel: override.ActorLabel, Reason: override.Reason, ExpiresAt: override.ExpiresAt.UTC()}
	}
}

// gateTargetsTx reads the service's complete target inventory once. The caller retains this
// ordered inventory in decision evidence rather than letting later target edits change history.
func gateTargetsTx(ctx context.Context, tx pgx.Tx, serviceID string) ([]gateTarget, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, window_name, objective::float8, updated_at, burn_rules, burn_alert_enabled
		  FROM sla_targets
		 WHERE service_id = $1`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("store: gate decision targets: %w", err)
	}
	defer rows.Close()
	var targets []gateTarget
	for rows.Next() {
		var target gateTarget
		var rules []byte
		if err := rows.Scan(&target.id, &target.window, &target.objective, &target.objectiveUpdatedAt, &rules, &target.burnAlertEnabled); err != nil {
			return nil, fmt.Errorf("store: gate decision target row: %w", err)
		}
		if _, ok := sla.WindowByName(target.window); !ok {
			return nil, fmt.Errorf("store: gate decision target window %q is not an SLA window", target.window)
		}
		if err := json.Unmarshal(rules, &target.rules); err != nil {
			return nil, fmt.Errorf("store: gate decision target burn rules: %w", err)
		}
		target.objectiveUpdatedAt = target.objectiveUpdatedAt.UTC()
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate gate decision targets: %w", err)
	}
	sort.Slice(targets, func(i, j int) bool { return gateWindowRank(targets[i].window) < gateWindowRank(targets[j].window) })
	return targets, nil
}

func gateWindowRank(name string) int {
	for index, window := range sla.StandardWindows {
		if window.Name == name {
			return index
		}
	}
	return len(sla.StandardWindows)
}

// governingRevisionTx is the declaration revision in force at `at` — the effective revision
// rule the delegation lookup applies (effectiveRevisionSQL), anchored at the snapshot instant
// rather than now(). nil when none governs.
func governingRevisionTx(ctx context.Context, tx pgx.Tx, serviceID string, at time.Time) (*domain.GateGoverningRevision, error) {
	var g domain.GateGoverningRevision
	err := tx.QueryRow(ctx, `
		SELECT r.id, r.revision
		  FROM service_definition_revisions r
		 WHERE r.service_id = $1 AND r.state = 'effective' AND r.effective_at <= $2
		 ORDER BY r.effective_at DESC, r.revision DESC
		 LIMIT 1`, serviceID, at).Scan(&g.ID, &g.Revision)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: gate decision governing revision: %w", err)
	}
	return &g, nil
}

// dedupedRules is the target's rules by canonical key, first occurrence wins — the same
// ambiguity guard the evaluator applies (§16.4b).
func dedupedRules(rules []domain.BurnRule) []domain.BurnRule {
	seen := make(map[string]bool, len(rules))
	out := make([]domain.BurnRule, 0, len(rules))
	for _, r := range rules {
		if key := r.Key(); !seen[key] {
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}

// latchFresh is the owner's freshness clause — `now() < lease_until` (alertdelegation.go) —
// at the snapshot instant.
func latchFresh(l burnRuleLatch, at time.Time) bool { return at.Before(l.leaseUntil) }

// evaluateGateClauses answers every clause of the policy's version from the owners' facts
// (D1, D11), in the version's order. Nothing here computes a number: the budget is the
// report's BurnedPercent, the burn level is the latch's `firing`, the incident is the row.
func evaluateGateClauses(in gateClauseInputs) []domain.GateClauseVerdict {
	out := make([]domain.GateClauseVerdict, 0, len(domain.GateClausesV1))
	for _, c := range domain.GateClausesFor(in.policy.SchemaVersion) {
		a := in.policy.Clauses[c]
		switch c {
		case domain.ClauseBudgetExhausted:
			out = append(out, budgetVerdict(in, c, a, 100))
		case domain.ClauseBudgetConsumed:
			out = append(out, budgetVerdict(in, c, a, float64(in.policy.BudgetConsumedPercent)))
		case domain.ClausePageBurnFiring:
			out = append(out, burnVerdict(in, c, a, domain.BurnSeverityPage))
		case domain.ClauseTicketBurnFiring:
			out = append(out, burnVerdict(in, c, a, domain.BurnSeverityTicket))
		case domain.ClauseServiceIncidentOpen:
			v := domain.GateClauseVerdict{Clause: c, Assignment: a, Source: gateSourceIncidents}
			if in.incident != nil {
				v.Matched, v.Value = true, in.incident.ID
			}
			out = append(out, v)
		}
	}
	return out
}

// budgetVerdict: BurnedPercent >= threshold, from the report path — UNAVAILABLE when the
// window has no target (window_target_missing), nothing is sealed (never_sealed), the seal
// lag exceeds the policy's bound (seal_stale, D8a), or the report withholds the number
// (budget_withheld). A withheld number is never a zero.
func budgetVerdict(in gateClauseInputs, c domain.GateClause, a domain.ClauseAssignment, threshold float64) domain.GateClauseVerdict {
	v := domain.GateClauseVerdict{Clause: c, Assignment: a, Source: gateSourceReportBudget}
	lag, sealed := in.report.SealLag()
	switch {
	case in.target == nil:
		v.Unavailable = domain.GateReasonWindowTargetMissing
	case !sealed:
		v.Unavailable = domain.GateReasonNeverSealed
	case lag > time.Duration(in.policy.MaxSealLagSeconds)*time.Second:
		v.Unavailable = domain.GateReasonSealStale
	case in.report.Budget == nil:
		v.Unavailable = domain.GateReasonBudgetWithheld
	default:
		pct := in.report.Budget.BurnedPercent
		v.Matched, v.Value = pct >= threshold, pct
	}
	return v
}

// burnVerdict: a rule of the given severity on the window's target is FIRING in the latch
// with a fresh lease. UNAVAILABLE when the window has no target, or when no firing rule is
// known and some rule of the severity has no evaluation or an expired lease (facts_stale). A
// target with no rule of the severity is a KNOWN non-match.
func burnVerdict(in gateClauseInputs, c domain.GateClause, a domain.ClauseAssignment, severity string) domain.GateClauseVerdict {
	v := domain.GateClauseVerdict{Clause: c, Assignment: a, Source: gateSourceBurnLatch}
	if in.target == nil {
		v.Unavailable = domain.GateReasonWindowTargetMissing
		return v
	}
	var firing []string
	stale := false
	for _, r := range dedupedRules(in.target.rules) {
		if r.Severity != severity {
			continue
		}
		l, ok := in.latches[burnLatchKey{targetID: in.target.id, ruleKey: r.Key()}]
		if !ok || !latchFresh(l, in.evaluatedAt) {
			stale = true
			continue
		}
		if l.firing {
			firing = append(firing, r.Key())
		}
	}
	switch {
	case len(firing) > 0:
		v.Matched, v.Value = true, strings.Join(firing, ",")
	case stale:
		v.Unavailable = domain.GateReasonFactsStale
	}
	return v
}

// assembleGateDecision applies the algebra, the override and the D7 presence table onto dec.
func assembleGateDecision(
	dec *domain.GateDecision, in gateClauseInputs, override *domain.GateOverride,
	coverage ServiceAlertingState, governing *domain.GateGoverningRevision,
) {
	policy := in.policy
	verdicts := evaluateGateClauses(in)
	state, action, reasons := domain.DecideGateAlgebra(verdicts, policy.UnknownBehavior)
	dec.State = state
	act := action
	dec.Action = &act

	// Policy fields: present when a policy exists.
	rev, mode, win, ub, lagMax := policy.Revision, policy.WindowMode, policy.Window, policy.UnknownBehavior, policy.MaxSealLagSeconds
	dec.PolicyRevision, dec.WindowMode, dec.Window = &rev, &mode, &win
	dec.UnknownBehavior, dec.MaxSealLagSeconds = &ub, &lagMax

	// Target fields: when the window's target exists.
	if in.target != nil {
		tid, obj, at := in.target.id, in.target.objective, in.target.objectiveUpdatedAt
		dec.TargetID, dec.Objective, dec.ObjectiveUpdatedAt = &tid, &obj, &at
		leases := gateBurnLeases(in)
		dec.BurnLeases = &leases
	}

	// Sealed fields: when any fact has been sealed.
	if lag, sealed := in.report.SealLag(); sealed {
		st := in.report.SealedThrough.UTC()
		dec.SealedThrough = &st
		secs := lag.Seconds()
		dec.SealLagSeconds = &secs
		fr := factRevisionsOf(in.report)
		dec.FactRevisions = &fr
	}

	// The governing declaration, or its absence, named.
	if governing != nil {
		g := *governing
		dec.GoverningRevision = &g
	} else {
		reasons = append(reasons, domain.GateReasonEntry{Code: string(domain.GateReasonNoGoverningRevision), Source: gateSourceRevisions})
	}

	// Coverage, or its absence, named.
	if coverage.Live.EvaluatedAt != nil {
		if coverage.Live.LeaseUntil != nil {
			until := coverage.Live.LeaseUntil.UTC()
			dec.CoverageLeaseUntil = &until
		}
		dec.CoverageState = &domain.GateCoverageState{
			Live: domain.GateCoverageSignal{Armed: coverage.Live.Armed, Reason: coverage.Live.Reason},
			Burn: domain.GateCoverageSignal{Armed: coverage.Burn.Armed, Reason: coverage.Burn.Reason},
		}
	} else {
		reasons = append(reasons, domain.GateReasonEntry{Code: string(domain.GateReasonNeverEvaluated), Source: gateSourceCoverage})
	}
	dec.Reasons = reasons

	// facts_fresh_until (D7, invariant 16): the ONE formula over decision-constraining horizons.
	dec.FactsFreshUntil = factsFreshUntil(in)

	// The override changes ONLY the action (D9): a BLOCK action becomes ALLOW with
	// unoverridden_action carrying what it would have been; state and reasons stay.
	if override != nil && action == domain.GateActionBlock {
		was := action
		allow := domain.GateActionAllow
		dec.Action, dec.UnoverriddenAction = &allow, &was
		oid := override.ID
		dec.OverrideID = &oid
		dec.Override = &domain.GateOverrideApplied{
			ID: override.ID, ActorLabel: override.ActorLabel, Reason: override.Reason, ExpiresAt: override.ExpiresAt.UTC(),
		}
	}
}

// factRevisionsOf is the COMPLETE surviving evidence about the revisions the sealed facts in
// the window were computed under (D7, §5a): the report's segments carry each one, and this is
// their count, both ends of the sorted id list and the SHA-256 over the sorted ids.
func factRevisionsOf(rep domain.ServiceWindowReport) domain.GateFactRevisions {
	seen := map[string]bool{}
	ids := make([]string, 0, len(rep.Segments))
	for _, seg := range rep.Segments {
		if !seen[seg.RevisionID] {
			seen[seg.RevisionID] = true
			ids = append(ids, seg.RevisionID)
		}
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	out := domain.GateFactRevisions{Count: len(ids), Digest: hex.EncodeToString(sum[:])}
	if len(ids) > 0 {
		first, last := ids[0], ids[len(ids)-1]
		out.FirstID, out.LastID = &first, &last
	}
	return out
}

// factsFreshUntil is the minimum of the seal horizon (sealed_through + max_seal_lag_seconds)
// when any budget clause is assigned block/warn, and of the leases of rules whose severity's
// clause is assigned block/warn. Coverage and ignored clauses are never in it. nil when no
// constraining horizon exists.
func factsFreshUntil(in gateClauseInputs) *time.Time {
	var min *time.Time
	consider := func(t time.Time) {
		t = t.UTC()
		if min == nil || t.Before(*min) {
			min = &t
		}
	}
	budgetConstrains := in.policy.Clauses[domain.ClauseBudgetExhausted].Constrains() ||
		in.policy.Clauses[domain.ClauseBudgetConsumed].Constrains()
	if budgetConstrains && in.report.SealedThrough != nil {
		consider(in.report.SealedThrough.Add(time.Duration(in.policy.MaxSealLagSeconds) * time.Second))
	}
	if in.target != nil {
		for _, r := range dedupedRules(in.target.rules) {
			var clause domain.GateClause
			switch r.Severity {
			case domain.BurnSeverityPage:
				clause = domain.ClausePageBurnFiring
			case domain.BurnSeverityTicket:
				clause = domain.ClauseTicketBurnFiring
			default:
				continue
			}
			if !in.policy.Clauses[clause].Constrains() {
				continue
			}
			if l, ok := in.latches[burnLatchKey{targetID: in.target.id, ruleKey: r.Key()}]; ok {
				consider(l.leaseUntil)
			}
		}
	}
	return min
}

// insertGateDecisionTx writes the ledger row (D10): top-level columns plus canonical
// `reasons`, `evidence` and `policy_snapshot`. The writer never truncates — a row exceeding a
// CHECK fails the decision; a row no partition holds is ErrGateLedgerUnwritable.
func insertGateDecisionTx(ctx context.Context, tx pgx.Tx, projectID string, dec domain.GateDecision, policy *domain.GatePolicy) error {
	reasons, err := canonicalJSONBytes(dec.Reasons)
	if err != nil {
		return fmt.Errorf("store: encode gate reasons: %w", err)
	}
	evidence, err := canonicalJSONBytes(dec.GateDecisionEvidence)
	if err != nil {
		return fmt.Errorf("store: encode gate evidence: %w", err)
	}
	var evaluatedWindows any
	if policy != nil {
		evaluated := dec.EvaluatedWindows
		if evaluated == nil {
			evaluated = []domain.GateEvaluatedWindow{}
		}
		raw, err := canonicalJSONBytes(evaluated)
		if err != nil {
			return fmt.Errorf("store: encode evaluated gate windows: %w", err)
		}
		evaluatedWindows = raw
	}
	var action *string
	if dec.Action != nil {
		a := string(*dec.Action)
		action = &a
	}
	// A nil interface is an explicit SQL NULL for the jsonb column (a nil []byte would not be).
	var snapshot any
	var window *string
	var windowMode *string
	if policy != nil {
		raw, err := canonicalJSONBytes(policy.Snapshot())
		if err != nil {
			return fmt.Errorf("store: encode gate policy snapshot: %w", err)
		}
		snapshot = raw
		if policy.Window != "" {
			w := policy.Window
			window = &w
		}
		mode := string(policy.WindowMode)
		windowMode = &mode
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO service_gate_decisions
		    (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
		     policy_revision, policy_source, policy_owner_id, window_name, window_mode, evaluated_windows, policy_snapshot, override_id, evaluated_at, sealed_through)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		dec.DecisionID, projectID, dec.ServiceID, dec.ServiceSlug, dec.ServiceName, string(dec.State), action,
		reasons, evidence, dec.PolicyRevision, dec.PolicySource, dec.PolicyOwnerID, window, windowMode, evaluatedWindows, snapshot, dec.OverrideID, dec.EvaluatedAt, dec.SealedThrough)
	switch {
	case err == nil:
		return nil
	case isNoPartitionForRow(err):
		return fmt.Errorf("%w: %v", ErrGateLedgerUnwritable, err)
	default:
		return fmt.Errorf("store: insert gate decision: %w", err)
	}
}
