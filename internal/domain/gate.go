package domain

import (
	"fmt"
	"strings"
	"time"
)

// FR-024 — the reliability gate (func-reliability-gate.md).
//
// This file owns the gate's VOCABULARY and its pure decisions: the states and actions of D4,
// the clause set of D11 with its version, the reason codes under which a clause is
// unavailable, the policy document and its exhaustive validation (D11, D14), and the
// override record with the read-time status function of D13a. Nothing here reads a clock or a
// database; the store consumes these types and the API decodes into them.

// GateState is what was OBSERVED (D4). ALLOW, WARN, BLOCK and UNKNOWN are policy outcomes;
// NOT_CONFIGURED is the fifth state, carried when the service has no policy, and it has NO
// action — what to do with it is the integration's visible choice, never a rendered ALLOW.
type GateState string

const (
	GateStateAllow         GateState = "ALLOW"
	GateStateWarn          GateState = "WARN"
	GateStateBlock         GateState = "BLOCK"
	GateStateUnknown       GateState = "UNKNOWN"
	GateStateNotConfigured GateState = "NOT_CONFIGURED"
)

// ValidGateState reports whether s is one of the five states.
func ValidGateState(s GateState) bool {
	switch s {
	case GateStateAllow, GateStateWarn, GateStateBlock, GateStateUnknown, GateStateNotConfigured:
		return true
	}
	return false
}

// GateAction is what the pipeline should DO (D4). UNKNOWN resolves to the policy's declared
// unknown_behavior; NOT_CONFIGURED has no action at all.
type GateAction string

const (
	GateActionAllow GateAction = "ALLOW"
	GateActionWarn  GateAction = "WARN"
	GateActionBlock GateAction = "BLOCK"
)

// ValidGateAction reports whether a is one of the three actions.
func ValidGateAction(a GateAction) bool {
	switch a {
	case GateActionAllow, GateActionWarn, GateActionBlock:
		return true
	}
	return false
}

// ClauseAssignment is what a policy declares about one clause (D11): block, warn or ignore.
// An ignored clause is still evaluated for evidence but contributes nothing — its
// unavailability never produces UNKNOWN (D4 step 1).
type ClauseAssignment string

const (
	ClauseAssignBlock  ClauseAssignment = "block"
	ClauseAssignWarn   ClauseAssignment = "warn"
	ClauseAssignIgnore ClauseAssignment = "ignore"
)

// ValidClauseAssignment reports whether a is one of block|warn|ignore.
func ValidClauseAssignment(a ClauseAssignment) bool {
	switch a {
	case ClauseAssignBlock, ClauseAssignWarn, ClauseAssignIgnore:
		return true
	}
	return false
}

// Constrains reports whether the assignment can decide the outcome — block or warn. Only
// constraining clauses can make a decision UNKNOWN when unavailable, and only their horizons
// enter facts_fresh_until (D7).
func (a ClauseAssignment) Constrains() bool {
	return a == ClauseAssignBlock || a == ClauseAssignWarn
}

// GateClause names one reliability fact a policy may assign (D11). The vocabulary is
// CLOSED and VERSIONED: a policy of schema_version 1 assigns exactly GateClausesV1.
type GateClause string

const (
	// ClauseBudgetExhausted — BurnedPercent >= 100 for the policy's window.
	ClauseBudgetExhausted GateClause = "budget_exhausted"
	// ClauseBudgetConsumed — BurnedPercent >= budget_consumed_percent for the policy's window.
	ClauseBudgetConsumed GateClause = "budget_consumed"
	// ClausePageBurnFiring — a page-severity burn rule of the window's target is FIRING.
	ClausePageBurnFiring GateClause = "page_burn_firing"
	// ClauseTicketBurnFiring — a ticket-severity burn rule of the window's target is FIRING.
	ClauseTicketBurnFiring GateClause = "ticket_burn_firing"
	// ClauseServiceIncidentOpen — an unresolved auto-incident anchored to this service.
	ClauseServiceIncidentOpen GateClause = "service_incident_open"
)

// GatePolicySchemaV1 is the only policy schema version the server knows (D14).
const GatePolicySchemaV1 = 1

// GateClausesV1 is the exhaustive, ordered clause set of schema_version 1 (D11). A write must
// assign every one of them exactly once; the order is the order reasons are reported in.
var GateClausesV1 = []GateClause{
	ClauseBudgetExhausted,
	ClauseBudgetConsumed,
	ClausePageBurnFiring,
	ClauseTicketBurnFiring,
	ClauseServiceIncidentOpen,
}

// GateClausesFor returns the clause set of a schema version, or nil for an unknown one.
func GateClausesFor(schemaVersion int) []GateClause {
	if schemaVersion == GatePolicySchemaV1 {
		out := make([]GateClause, len(GateClausesV1))
		copy(out, GateClausesV1)
		return out
	}
	return nil
}

// IsBudgetClause reports whether c rests on the sealed budget — the clauses `seal_stale`
// makes unavailable (D8a) and whose seal horizon enters facts_fresh_until (D7).
func (c GateClause) IsBudgetClause() bool {
	return c == ClauseBudgetExhausted || c == ClauseBudgetConsumed
}

// IsBurnClause reports whether c rests on a burn latch — the clauses whose rule leases enter
// facts_fresh_until (D7) and that `facts_stale` makes unavailable (D11).
func (c GateClause) IsBurnClause() bool {
	return c == ClausePageBurnFiring || c == ClauseTicketBurnFiring
}

// GateReason is a reason code in `reasons[]` for a clause that is UNAVAILABLE rather than
// false (D11), or the whole-decision reason `not_configured` (D4). These are not
// assignments; they are the conditions under which a block/warn clause cannot be answered
// and D4 step 3 applies.
type GateReason string

const (
	// GateReasonBudgetWithheld — the report withholds the number (decideServiceWindow).
	GateReasonBudgetWithheld GateReason = "budget_withheld"
	// GateReasonSealStale — evaluated_at − sealed_through > max_seal_lag_seconds (D8a).
	GateReasonSealStale GateReason = "seal_stale"
	// GateReasonFactsStale — a burn lease expired or no evaluation exists.
	GateReasonFactsStale GateReason = "facts_stale"
	// GateReasonNoObjective — the window's target has no objective.
	GateReasonNoObjective GateReason = "no_objective"
	// GateReasonWindowTargetMissing — the service has no target for the policy's window (D2).
	GateReasonWindowTargetMissing GateReason = "window_target_missing"
	// GateReasonNeverSealed — no fact has been sealed for the service (D7).
	GateReasonNeverSealed GateReason = "never_sealed"
	// GateReasonNeverEvaluated — the service has no coverage evaluation yet (D7).
	GateReasonNeverEvaluated GateReason = "never_evaluated"
	// GateReasonNoGoverningRevision — no declaration governs at evaluated_at (D7).
	GateReasonNoGoverningRevision GateReason = "no_governing_revision"
	// GateReasonNotConfigured — the service has no policy (D4, state NOT_CONFIGURED).
	GateReasonNotConfigured GateReason = "not_configured"
)

// GateUnknownBehavior is the policy's mandatory, default-less answer to "what does the
// pipeline do when a clause it depends on cannot be answered" (D5): warn or block.
type GateUnknownBehavior string

const (
	GateUnknownWarn  GateUnknownBehavior = "warn"
	GateUnknownBlock GateUnknownBehavior = "block"
)

// ValidGateUnknownBehavior reports whether b is warn|block.
func ValidGateUnknownBehavior(b GateUnknownBehavior) bool {
	return b == GateUnknownWarn || b == GateUnknownBlock
}

// Action is the GateAction an UNKNOWN state resolves to under this behaviour (D4 step 3).
func (b GateUnknownBehavior) Action() GateAction {
	if b == GateUnknownBlock {
		return GateActionBlock
	}
	return GateActionWarn
}

// Bounds of the policy document (D3, D8a, D14).
const (
	// GateBudgetConsumedPercentMin / Max bound `budget_consumed_percent` (D3: 1..100).
	GateBudgetConsumedPercentMin = 1
	GateBudgetConsumedPercentMax = 100
	// GateSealLagStep is the granularity of `max_seal_lag_seconds`: a whole number of minutes
	// (D8a, D14).
	GateSealLagStep = time.Minute
)

// GateOverride bounds (D9).
const (
	// GateOverrideMaxDuration is how far ahead `expires_at` may be — a hard maximum, no default.
	GateOverrideMaxDuration = 7 * 24 * time.Hour
	// GateOverrideReasonMinLen / MaxLen bound the mandatory reason, in characters.
	GateOverrideReasonMinLen = 1
	GateOverrideReasonMaxLen = 500
)

// GateClauseEntry is one (clause, assignment) pair of a policy document AS WRITTEN — an
// ordered list rather than a map, because D14 refuses a DUPLICATE clause by name and a map
// has already lost the duplicate by the time anybody looks. The transport decodes the JSON
// object into this list in document order; the domain decides.
type GateClauseEntry struct {
	Clause     GateClause
	Assignment ClauseAssignment
}

// GatePolicyDocument is the full D11/D14 policy body: schema_version, window, every clause
// of the version, the threshold, the seal-lag bound and the unknown behaviour. The server
// fills nothing in — an omitted field is a refused request, not a default — so every field
// here is what the caller wrote.
type GatePolicyDocument struct {
	SchemaVersion         int
	Window                string
	Clauses               []GateClauseEntry
	BudgetConsumedPercent int
	MaxSealLagSeconds     int
	UnknownBehavior       GateUnknownBehavior
}

// GatePolicyError is a refused policy document. Field names the offending field in the
// wire's spelling (`clauses.budget_consumed` for a clause), and Error() states the range or
// the rule, so a 400 can carry both without the transport re-deriving them.
type GatePolicyError struct {
	Field string
	Msg   string
}

func (e *GatePolicyError) Error() string { return e.Field + ": " + e.Msg }

func gateErr(field, format string, args ...any) error {
	return &GatePolicyError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// ValidateGatePolicyV1 validates a schema_version 1 policy document exhaustively (D11, D14)
// and returns its clause map on success. It refuses, naming the field and the range:
//
//   - a schema_version other than 1;
//   - an empty window (whether the service HAS a target for it is the store's question, D2);
//   - a clause outside the version's vocabulary, a clause of the vocabulary that is missing,
//     and a clause assigned twice — each by NAME, so adding a clause in a later version cannot
//     silently reinterpret a stored policy;
//   - an assignment other than block|warn|ignore;
//   - budget_consumed_percent outside 1..100;
//   - max_seal_lag_seconds outside [MinSealLag, MaxSealLag] or not a whole number of minutes;
//   - an unknown_behavior other than warn|block (D5: mandatory, no default).
//
// The result map is canonical — exactly the version's clauses, each once — which is what a
// stored `clauses` column holds.
func ValidateGatePolicyV1(doc GatePolicyDocument) (map[GateClause]ClauseAssignment, error) {
	if doc.SchemaVersion != GatePolicySchemaV1 {
		return nil, gateErr("schema_version", "must be %d (the only known version), got %d",
			GatePolicySchemaV1, doc.SchemaVersion)
	}
	if strings.TrimSpace(doc.Window) == "" || doc.Window != strings.TrimSpace(doc.Window) {
		return nil, gateErr("window", "must name the SLO window the policy governs; got %q", doc.Window)
	}

	known := make(map[GateClause]bool, len(GateClausesV1))
	for _, c := range GateClausesV1 {
		known[c] = true
	}
	clauses := make(map[GateClause]ClauseAssignment, len(GateClausesV1))
	for _, e := range doc.Clauses {
		field := "clauses." + string(e.Clause)
		if !known[e.Clause] {
			return nil, gateErr(field, "unknown clause %q for schema_version %d; the clauses are %s",
				e.Clause, GatePolicySchemaV1, joinClauses(GateClausesV1))
		}
		if _, dup := clauses[e.Clause]; dup {
			return nil, gateErr(field, "clause %q is assigned more than once; every clause is assigned exactly once", e.Clause)
		}
		if !ValidClauseAssignment(e.Assignment) {
			return nil, gateErr(field, "assignment must be one of %s|%s|%s, got %q",
				ClauseAssignBlock, ClauseAssignWarn, ClauseAssignIgnore, e.Assignment)
		}
		clauses[e.Clause] = e.Assignment
	}
	for _, c := range GateClausesV1 {
		if _, ok := clauses[c]; !ok {
			return nil, gateErr("clauses."+string(c), "clause %q is missing; schema_version %d assigns every one of %s",
				c, GatePolicySchemaV1, joinClauses(GateClausesV1))
		}
	}

	if doc.BudgetConsumedPercent < GateBudgetConsumedPercentMin || doc.BudgetConsumedPercent > GateBudgetConsumedPercentMax {
		return nil, gateErr("budget_consumed_percent", "must be an integer between %d and %d, got %d",
			GateBudgetConsumedPercentMin, GateBudgetConsumedPercentMax, doc.BudgetConsumedPercent)
	}

	minLag, maxLag, step := int(MinSealLag/time.Second), int(MaxSealLag/time.Second), int(GateSealLagStep/time.Second)
	if doc.MaxSealLagSeconds < minLag || doc.MaxSealLagSeconds > maxLag {
		return nil, gateErr("max_seal_lag_seconds", "must be between %d and %d seconds, got %d (the floor %d is "+
			"LateArrivalGrace + CanonicalBucket + 2 × CanonicalBucket, the lag a healthy materializer can satisfy)",
			minLag, maxLag, doc.MaxSealLagSeconds, minLag)
	}
	if doc.MaxSealLagSeconds%step != 0 {
		return nil, gateErr("max_seal_lag_seconds", "must be a whole number of minutes (a multiple of %d seconds), got %d",
			step, doc.MaxSealLagSeconds)
	}

	if !ValidGateUnknownBehavior(doc.UnknownBehavior) {
		return nil, gateErr("unknown_behavior", "must be %s|%s and has no default, got %q",
			GateUnknownWarn, GateUnknownBlock, doc.UnknownBehavior)
	}
	return clauses, nil
}

func joinClauses(cs []GateClause) string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}

// GatePolicy is one service's stored policy (§5 `service_gate_policies`). Revision is
// DB-owned and MONOTONIC per service, never reused: the row is never deleted — a DELETE sets
// DeletedAt and bumps Revision, a re-create is an UPDATE that clears DeletedAt with
// Revision + 1 (D13a) — so a stale screen holding an old revision can never CAS over a
// different policy.
type GatePolicy struct {
	ServiceID             string                          `json:"service_id"`
	ProjectID             string                          `json:"project_id"`
	Window                string                          `json:"window"`
	SchemaVersion         int                             `json:"schema_version"`
	Clauses               map[GateClause]ClauseAssignment `json:"clauses"`
	BudgetConsumedPercent int                             `json:"budget_consumed_percent"`
	MaxSealLagSeconds     int                             `json:"max_seal_lag_seconds"`
	UnknownBehavior       GateUnknownBehavior             `json:"unknown_behavior"`
	Revision              int64                           `json:"revision"`
	// DeletedAt is the tombstone: non-nil means the service currently has NO policy
	// (decisions are NOT_CONFIGURED) while the generation counter keeps its history.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
	// UpdatedBy is the writer's immutable server-derived label (the principal's AuditLabel),
	// the spelling every other service-scoped record keeps (created_by on revisions).
	UpdatedBy string `json:"updated_by"`
}

// Live reports whether the policy governs decisions right now — present and not tombstoned.
func (p GatePolicy) Live() bool { return p.DeletedAt == nil }

// Document is the policy as a D11/D14 document, for the no-op comparison of D14 (a write
// identical to the stored policy bumps nothing).
func (p GatePolicy) Document() GatePolicyDocument {
	entries := make([]GateClauseEntry, 0, len(p.Clauses))
	for _, c := range GateClausesFor(p.SchemaVersion) {
		if a, ok := p.Clauses[c]; ok {
			entries = append(entries, GateClauseEntry{Clause: c, Assignment: a})
		}
	}
	return GatePolicyDocument{
		SchemaVersion:         p.SchemaVersion,
		Window:                p.Window,
		Clauses:               entries,
		BudgetConsumedPercent: p.BudgetConsumedPercent,
		MaxSealLagSeconds:     p.MaxSealLagSeconds,
		UnknownBehavior:       p.UnknownBehavior,
	}
}

// GateRevokedReason is why an override was closed (D9, D13a). Every closure — human or
// system — sets revoked_at and one of these; only `manual` carries attribution.
type GateRevokedReason string

const (
	GateRevokedManual        GateRevokedReason = "manual"
	GateRevokedExpired       GateRevokedReason = "expired"
	GateRevokedPolicyChanged GateRevokedReason = "policy_changed"
	GateRevokedPolicyDeleted GateRevokedReason = "policy_deleted"
)

// ValidGateRevokedReason reports whether r is one of the four closure reasons.
func ValidGateRevokedReason(r GateRevokedReason) bool {
	switch r {
	case GateRevokedManual, GateRevokedExpired, GateRevokedPolicyChanged, GateRevokedPolicyDeleted:
		return true
	}
	return false
}

// GateOverrideStatus is the read-time status of an override (D13a): a FUNCTION over semantic
// facts, never a stored column — see GateOverrideStatusAt.
type GateOverrideStatus string

const (
	GateOverrideActive  GateOverrideStatus = "active"
	GateOverrideExpired GateOverrideStatus = "expired"
	GateOverrideRevoked GateOverrideStatus = "revoked"
	GateOverrideInert   GateOverrideStatus = "inert"
)

// GateOverride is one row of `service_gate_overrides` (§5, D9). The actor and the revoker are
// each stored as the COMPLETE typed triple — a nullable user id, a via-token flag and an
// immutable server-derived label — because for a machine principal the typed half is
// `NULL + true`, which after commit reads as "some token", and the evidence has to name which
// one for as long as the row exists (invariant 17).
type GateOverride struct {
	ID             string
	ServiceID      string
	ProjectID      string
	PolicyRevision int64

	// The actor triple (D9): who created it.
	ActorUserID *string
	ViaToken    bool
	ActorLabel  string

	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time

	// The lifecycle pair: null while open, set by ANY closure (D13a).
	RevokedAt     *time.Time
	RevokedReason GateRevokedReason // "" while open

	// The revoker triple: present only for a `manual` closure; all null otherwise.
	RevokedByUserID *string
	RevokedViaToken *bool
	RevokedByLabel  *string
}

// Open reports whether the row has not been closed by any closure (revoked_at IS NULL). It is
// the slot predicate D9's creation path serializes on — NOT the read-time status, which also
// weighs the clock and the live revision.
func (o GateOverride) Open() bool { return o.RevokedAt == nil && o.RevokedReason == "" }

// GateOverrideStatusAt is the status function of D13a, computed at read time over SEMANTIC
// FACTS rather than over which housekeeping has run. currentLiveRevision is the service's
// current live policy revision, or nil when the policy is tombstoned or absent. The
// precedence, exhaustive and in this order:
//
//  1. revoked_reason = manual                                            → revoked
//  2. revoked_reason IN (policy_changed, policy_deleted)
//     OR policy_revision ≠ the current live revision OR no live policy   → inert
//  3. revoked_reason = expired OR expires_at <= now                     → expired
//  4. otherwise                                                          → active
//
// Rows 2 and 3 each join a recorded reason with the live fact it records, so the in-lock
// closure that later writes `expired` or `policy_changed` onto a row cannot move it: a
// mismatched, expired, unrevoked row is inert before the next POST closes it as `expired`
// and inert after. A status moves only when a semantic fact moves — a manual revoke, a
// policy edit, the clock passing expires_at.
func GateOverrideStatusAt(o GateOverride, now time.Time, currentLiveRevision *int64) GateOverrideStatus {
	if o.RevokedReason == GateRevokedManual {
		return GateOverrideRevoked
	}
	if o.RevokedReason == GateRevokedPolicyChanged || o.RevokedReason == GateRevokedPolicyDeleted ||
		currentLiveRevision == nil || *currentLiveRevision != o.PolicyRevision {
		return GateOverrideInert
	}
	if o.RevokedReason == GateRevokedExpired || !o.ExpiresAt.After(now) {
		return GateOverrideExpired
	}
	return GateOverrideActive
}

// ── The decision response (D7) and its ledger evidence (D10) ─────────────────────────────

// GateDecisionSchemaV1 is the `schema_version` every decision response carries (D7).
const GateDecisionSchemaV1 = 1

// GateDocsURL is the documentation link a NOT_CONFIGURED decision carries in its one reason
// (D4): the service has no policy, the response says so, and what to do with that is the
// integration's visible choice.
const GateDocsURL = "https://github.com/teamlead-com/cerbix/blob/main/docs/specs/func-reliability-gate.md"

// GateReasonEntry is one `reasons[]` element (D4, D7). For a clause it carries the clause,
// its assignment, the value that matched (or nothing when the clause was UNAVAILABLE — Code
// then names the unavailability) and the owner the fact came from. The whole-decision
// `not_configured` entry carries Docs. Every field but Code is optional on the wire.
type GateReasonEntry struct {
	Code       string           `json:"code"`
	Clause     GateClause       `json:"clause,omitempty"`
	Assignment ClauseAssignment `json:"assignment,omitempty"`
	Value      any              `json:"value,omitempty"`
	Source     string           `json:"source,omitempty"`
	Docs       string           `json:"docs,omitempty"`
}

// GateFactRevisions is the COMPLETE surviving evidence about the definition revisions the
// sealed facts in the window were computed under (D7, §5a): a count, both ends of the sorted
// id list and the SHA-256 over the sorted ids — because the revisions themselves are deleted
// with their service and a decision outlives it.
type GateFactRevisions struct {
	Count   int     `json:"count"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
	Digest  string  `json:"digest"`
}

// GateGoverningRevision names the declaration in force at evaluated_at (D7).
type GateGoverningRevision struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// GateBurnLease is one rule of the policy's target as the burn latch holds it (D1, D7): the
// rule's canonical key and severity from `sla_targets.burn_rules`, and — when the rule has
// been evaluated — the latch's level, verdict, evaluation instant and lease. A rule with no
// latch row carries nulls, which is what `facts_stale` means for it.
type GateBurnLease struct {
	RuleKey     string     `json:"rule_key"`
	Severity    string     `json:"severity"`
	Firing      *bool      `json:"firing"`
	LastVerdict *string    `json:"last_verdict"`
	EvaluatedAt *time.Time `json:"evaluated_at"`
	LeaseUntil  *time.Time `json:"lease_until"`
	// Fresh is the owner's freshness clause (`now() < lease_until`) at evaluated_at.
	Fresh bool `json:"fresh"`
}

// GateCoverageSignal is one signal's coverage verdict as evidence (D11: never a clause).
type GateCoverageSignal struct {
	Armed  bool   `json:"armed"`
	Reason string `json:"reason,omitempty"`
}

// GateCoverageState is both signals — the same strings the service page's badge shows, from
// the same snapshot.
type GateCoverageState struct {
	Live GateCoverageSignal `json:"live"`
	Burn GateCoverageSignal `json:"burn"`
}

// GateOverrideApplied is the override a decision applied (D9): the id, the immutable actor
// label, the reason and the expiry.
type GateOverrideApplied struct {
	ID         string    `json:"id"`
	ActorLabel string    `json:"actor_label"`
	Reason     string    `json:"reason"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// GateDecisionEvidence is the part of a decision that is not a top-level ledger column — the
// `evidence` jsonb of `service_gate_decisions`, stored canonically (sorted keys, no
// whitespace). It is embedded in GateDecision so the wire response inlines it, and decoded
// back on a ledger read so the by-id response is the decision as it was. Every field follows
// the D7 presence table: a nil pointer is ABSENT on the wire, never null.
type GateDecisionEvidence struct {
	UnoverriddenAction *GateAction          `json:"unoverridden_action,omitempty"`
	UnknownBehavior    *GateUnknownBehavior `json:"unknown_behavior,omitempty"`
	MaxSealLagSeconds  *int                 `json:"max_seal_lag_seconds,omitempty"`

	TargetID           *string    `json:"target_id,omitempty"`
	Objective          *float64   `json:"objective,omitempty"`
	ObjectiveUpdatedAt *time.Time `json:"objective_updated_at,omitempty"`

	// SealLagSeconds is evaluated_at − sealed_through in seconds (D8a), present with
	// sealed_through.
	SealLagSeconds     *float64               `json:"seal_lag,omitempty"`
	FactRevisions      *GateFactRevisions     `json:"fact_revisions,omitempty"`
	GoverningRevision  *GateGoverningRevision `json:"governing_revision,omitempty"`
	BurnLeases         *[]GateBurnLease       `json:"burn_leases,omitempty"`
	CoverageLeaseUntil *time.Time             `json:"coverage_lease_until,omitempty"`
	CoverageState      *GateCoverageState     `json:"coverage_state,omitempty"`
	Override           *GateOverrideApplied   `json:"override,omitempty"`
	FactsFreshUntil    *time.Time             `json:"facts_fresh_until,omitempty"`
}

// GateDecision is the D7 response and, column for column, one ledger row (D10). The
// always-present fields are plain values; everything conditional is a pointer with omitempty
// so absence is absence. ServiceID is the one deliberate present-and-null: it is `null` once
// the service has been deleted (ON DELETE SET NULL), and that is a different statement from
// "never applied".
type GateDecision struct {
	SchemaVersion int       `json:"schema_version"`
	DecisionID    string    `json:"decision_id"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	ServiceID     *string   `json:"service_id"`
	ServiceSlug   string    `json:"service_slug"`
	ServiceName   string    `json:"service_name"`
	State         GateState `json:"state"`
	// Action is absent for NOT_CONFIGURED only (D4).
	Action  *GateAction       `json:"action,omitempty"`
	Reasons []GateReasonEntry `json:"reasons"`
	// PolicyRevision and Window are present when a policy exists.
	PolicyRevision *int64  `json:"policy_revision,omitempty"`
	Window         *string `json:"window,omitempty"`
	// OverrideID is present exactly when an override was applied — the id the listing carries;
	// Override (in the evidence) is the same override with its attribution.
	OverrideID    *string    `json:"override_id,omitempty"`
	SealedThrough *time.Time `json:"sealed_through,omitempty"`

	GateDecisionEvidence
}

// Overridden reports whether an active override changed this decision's action (D9): the
// metric's `overridden="true"`.
func (d GateDecision) Overridden() bool { return d.Override != nil }

// GateDecisionSummary is one listing item (§5, the listing contract): the by-id response's
// presence rules over the fields the history screen shows. It is a strict projection of
// GateDecision — every field here is spelled and conditioned exactly as there.
type GateDecisionSummary struct {
	SchemaVersion  int               `json:"schema_version"`
	DecisionID     string            `json:"decision_id"`
	EvaluatedAt    time.Time         `json:"evaluated_at"`
	ServiceID      *string           `json:"service_id"`
	ServiceSlug    string            `json:"service_slug"`
	ServiceName    string            `json:"service_name"`
	State          GateState         `json:"state"`
	Action         *GateAction       `json:"action,omitempty"`
	Reasons        []GateReasonEntry `json:"reasons"`
	PolicyRevision *int64            `json:"policy_revision,omitempty"`
	OverrideID     *string           `json:"override_id,omitempty"`
}

// Summary projects a decision onto its listing item.
func (d GateDecision) Summary() GateDecisionSummary {
	return GateDecisionSummary{
		SchemaVersion:  d.SchemaVersion,
		DecisionID:     d.DecisionID,
		EvaluatedAt:    d.EvaluatedAt,
		ServiceID:      d.ServiceID,
		ServiceSlug:    d.ServiceSlug,
		ServiceName:    d.ServiceName,
		State:          d.State,
		Action:         d.Action,
		Reasons:        d.Reasons,
		PolicyRevision: d.PolicyRevision,
		OverrideID:     d.OverrideID,
	}
}

// GatePolicySnapshot is the `policy_snapshot` jsonb of a ledger row (D10): the document the
// decision was evaluated under and its revision, so the row reads the same after the policy
// moves on.
type GatePolicySnapshot struct {
	Revision              int64                           `json:"revision"`
	SchemaVersion         int                             `json:"schema_version"`
	Window                string                          `json:"window"`
	Clauses               map[GateClause]ClauseAssignment `json:"clauses"`
	BudgetConsumedPercent int                             `json:"budget_consumed_percent"`
	MaxSealLagSeconds     int                             `json:"max_seal_lag_seconds"`
	UnknownBehavior       GateUnknownBehavior             `json:"unknown_behavior"`
}

// Snapshot is the policy as a ledger snapshot.
func (p GatePolicy) Snapshot() GatePolicySnapshot {
	return GatePolicySnapshot{
		Revision:              p.Revision,
		SchemaVersion:         p.SchemaVersion,
		Window:                p.Window,
		Clauses:               p.Clauses,
		BudgetConsumedPercent: p.BudgetConsumedPercent,
		MaxSealLagSeconds:     p.MaxSealLagSeconds,
		UnknownBehavior:       p.UnknownBehavior,
	}
}

// ── The D4 algebra, pure ─────────────────────────────────────────────────────────────────

// GateClauseVerdict is one clause after evaluation: matched, not matched, or UNAVAILABLE
// with the reason code that says why (D4, D11).
type GateClauseVerdict struct {
	Clause     GateClause
	Assignment ClauseAssignment
	// Matched is meaningful only when Unavailable is empty.
	Matched bool
	// Unavailable is the GateReason under which the clause could not be answered, "" when it
	// could.
	Unavailable GateReason
	// Value is what the clause saw (the burned percent, the firing rule key, the incident id)
	// — reported in reasons[] for a matched clause; nil when unavailable.
	Value any
	// Source names the owner the fact came from.
	Source string
}

// DecideGateAlgebra is D4's total, deterministic algebra over ALL clauses of a policy:
//
//  1. an `ignore` clause contributes nothing (its unavailability never produces UNKNOWN);
//  2. any KNOWN, matching `block` clause → BLOCK/BLOCK, whatever else is unknown;
//  3. else any UNAVAILABLE `block`/`warn` clause → UNKNOWN, action = unknown_behavior;
//  4. else any matching `warn` clause → WARN/WARN;
//  5. else ALLOW/ALLOW.
//
// reasons[] is TOTAL: every matched or unavailable clause, in the order given — ignored
// clauses included, because they are evaluated for evidence even though they decide nothing.
func DecideGateAlgebra(verdicts []GateClauseVerdict, unknown GateUnknownBehavior) (GateState, GateAction, []GateReasonEntry) {
	reasons := make([]GateReasonEntry, 0, len(verdicts))
	var knownBlock, anyUnavailable, warnMatch bool
	for _, v := range verdicts {
		switch {
		case v.Unavailable != "":
			reasons = append(reasons, GateReasonEntry{
				Code: string(v.Unavailable), Clause: v.Clause, Assignment: v.Assignment, Source: v.Source,
			})
			if v.Assignment.Constrains() {
				anyUnavailable = true
			}
		case v.Matched:
			reasons = append(reasons, GateReasonEntry{
				Code: string(v.Clause), Clause: v.Clause, Assignment: v.Assignment, Value: v.Value, Source: v.Source,
			})
			switch v.Assignment {
			case ClauseAssignBlock:
				knownBlock = true
			case ClauseAssignWarn:
				warnMatch = true
			}
		}
	}
	switch {
	case knownBlock:
		return GateStateBlock, GateActionBlock, reasons
	case anyUnavailable:
		return GateStateUnknown, unknown.Action(), reasons
	case warnMatch:
		return GateStateWarn, GateActionWarn, reasons
	default:
		return GateStateAllow, GateActionAllow, reasons
	}
}
