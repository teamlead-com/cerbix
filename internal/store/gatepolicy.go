package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// The per-service gate policy (func-reliability-gate D11, D13a, D14; invariants 11, 13, 21).
//
// `revision` is a GENERATION the database owns and never reuses: the row is never deleted —
// DELETE tombstones it and bumps the revision in the same statement; a re-create is an UPDATE
// that clears the tombstone with revision + 1 — so a stale screen holding an old revision can
// never CAS over, or bind an override to, a policy it has not seen. Every write runs under
// the service row's FOR UPDATE, validates first, then runs the CAS BEFORE the no-op
// comparison (an identical body with a stale expected_revision is a conflict, never a silent
// success), and closes the active override in the SAME transaction (D9). Audit rows are
// written inside the transaction with the actor and the before/after documents.

// gatePolicyColumns is the one SELECT list every policy read shares.
const gatePolicyColumns = `
	p.service_id, p.project_id, p.window_name, p.schema_version, p.clauses,
	p.budget_consumed_percent, p.max_seal_lag_seconds, p.unknown_behavior,
	p.revision, p.deleted_at, p.updated_at, p.updated_by`

func scanGatePolicy(row scannable) (domain.GatePolicy, error) {
	var p domain.GatePolicy
	var clauses []byte
	err := row.Scan(&p.ServiceID, &p.ProjectID, &p.Window, &p.SchemaVersion, &clauses,
		&p.BudgetConsumedPercent, &p.MaxSealLagSeconds, &p.UnknownBehavior,
		&p.Revision, &p.DeletedAt, &p.UpdatedAt, &p.UpdatedBy)
	if err != nil {
		return domain.GatePolicy{}, err
	}
	if err := json.Unmarshal(clauses, &p.Clauses); err != nil {
		return domain.GatePolicy{}, fmt.Errorf("store: decode gate policy clauses: %w", err)
	}
	p.UpdatedAt = p.UpdatedAt.UTC()
	if p.DeletedAt != nil {
		t := p.DeletedAt.UTC()
		p.DeletedAt = &t
	}
	return p, nil
}

// readGatePolicyRowOn reads the policy row — live OR tombstoned — for a service whose
// existence and tenancy the caller has already established. found is false when no row has
// ever been written.
func readGatePolicyRowOn(ctx context.Context, q dbConn, serviceID string, forUpdate bool) (p domain.GatePolicy, found bool, err error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	p, err = scanGatePolicy(q.QueryRow(ctx,
		`SELECT`+gatePolicyColumns+` FROM service_gate_policies p WHERE p.service_id = $1`+lock, serviceID))
	if noRows(err) {
		return domain.GatePolicy{}, false, nil
	}
	if err != nil {
		return domain.GatePolicy{}, false, fmt.Errorf("store: read gate policy: %w", err)
	}
	return p, true, nil
}

// liveGatePolicyRevisionOn is the service's current LIVE revision, nil when the policy is
// absent or tombstoned — the argument domain.GateOverrideStatusAt takes.
func liveGatePolicyRevisionOn(ctx context.Context, q dbConn, serviceID string) (*int64, error) {
	var rev *int64
	err := q.QueryRow(ctx,
		`SELECT revision FROM service_gate_policies WHERE service_id = $1 AND deleted_at IS NULL`,
		serviceID).Scan(&rev)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read live gate policy revision: %w", err)
	}
	return rev, nil
}

// GetGatePolicy reads a service's LIVE policy. A foreign, unknown or malformed service id is
// ErrNotFound (the tenant contract of D15); a service with no policy — never written, or
// tombstoned — is ErrGatePolicyNotConfigured.
func (s *Store) GetGatePolicy(ctx context.Context, projectID, serviceID string) (domain.GatePolicy, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM services WHERE id = $1 AND project_id = $2)`, serviceID, projectID).Scan(&exists)
	if isInvalidTextRepresentation(err) {
		return domain.GatePolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.GatePolicy{}, fmt.Errorf("store: gate policy service scope: %w", err)
	}
	if !exists {
		return domain.GatePolicy{}, ErrNotFound
	}
	p, found, err := readGatePolicyRowOn(ctx, s.pool, serviceID, false)
	if err != nil {
		return domain.GatePolicy{}, err
	}
	if !found || !p.Live() {
		return domain.GatePolicy{}, ErrGatePolicyNotConfigured
	}
	return p, nil
}

// validateGatePolicyDocumentTx is the store's half of the write validation: the domain
// validates the document's shape and bounds (ValidateGatePolicyV1); the store adds the one
// rule only it can check — the window must be a known SLA window the service has a
// service-scoped target for (D2) — naming `window` like every other refusal.
func validateGatePolicyDocumentTx(
	ctx context.Context, tx pgx.Tx, serviceID string, doc domain.GatePolicyDocument,
) (map[domain.GateClause]domain.ClauseAssignment, error) {
	clauses, err := domain.ValidateGatePolicyV1(doc)
	if err != nil {
		return nil, err
	}
	if _, ok := sla.WindowByName(doc.Window); !ok {
		return nil, &domain.GatePolicyError{Field: "window",
			Msg: fmt.Sprintf("%q is not an SLA window (the windows are %s)", doc.Window, windowNames())}
	}
	var hasTarget bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sla_targets WHERE service_id = $1 AND window_name = $2)`,
		serviceID, doc.Window).Scan(&hasTarget); err != nil {
		return nil, fmt.Errorf("store: gate policy window target: %w", err)
	}
	if !hasTarget {
		return nil, &domain.GatePolicyError{Field: "window",
			Msg: fmt.Sprintf("the service has no SLO target for window %q; set one before a policy can govern it", doc.Window)}
	}
	return clauses, nil
}

func windowNames() string {
	out := ""
	for i, w := range sla.StandardWindows {
		if i > 0 {
			out += ", "
		}
		out += w.Name
	}
	return out
}

// gatePolicyDocumentEqual is the D14 no-op comparison over the CANONICAL form: the version,
// the window, every clause's assignment, both thresholds and the unknown behaviour.
func gatePolicyDocumentEqual(stored domain.GatePolicy, doc domain.GatePolicyDocument, clauses map[domain.GateClause]domain.ClauseAssignment) bool {
	if stored.SchemaVersion != doc.SchemaVersion || stored.Window != doc.Window ||
		stored.BudgetConsumedPercent != doc.BudgetConsumedPercent ||
		stored.MaxSealLagSeconds != doc.MaxSealLagSeconds ||
		stored.UnknownBehavior != doc.UnknownBehavior ||
		len(stored.Clauses) != len(clauses) {
		return false
	}
	for c, a := range clauses {
		if stored.Clauses[c] != a {
			return false
		}
	}
	return true
}

// gatePolicyAuditText renders a document for the audit target, canonically.
func gatePolicyAuditText(doc *domain.GatePolicyDocument, clauses map[domain.GateClause]domain.ClauseAssignment) string {
	if doc == nil {
		return "none"
	}
	raw, err := canonicalJSONBytes(struct {
		SchemaVersion         int                                           `json:"schema_version"`
		Window                string                                        `json:"window"`
		Clauses               map[domain.GateClause]domain.ClauseAssignment `json:"clauses"`
		BudgetConsumedPercent int                                           `json:"budget_consumed_percent"`
		MaxSealLagSeconds     int                                           `json:"max_seal_lag_seconds"`
		UnknownBehavior       domain.GateUnknownBehavior                    `json:"unknown_behavior"`
	}{doc.SchemaVersion, doc.Window, clauses, doc.BudgetConsumedPercent, doc.MaxSealLagSeconds, doc.UnknownBehavior})
	if err != nil {
		return "unrenderable"
	}
	return string(raw)
}

// closeGateOverridesTx closes EVERY unrevoked override of the service with a system reason
// (policy_changed, policy_deleted): revoked_at and revoked_reason set, attribution NULL (D9,
// D13a). There is at most one such row by construction; the statement does not rely on it.
func closeGateOverridesTx(ctx context.Context, tx pgx.Tx, serviceID string, reason domain.GateRevokedReason) error {
	if _, err := tx.Exec(ctx, `
		UPDATE service_gate_overrides
		   SET revoked_at = now(), revoked_reason = $2
		 WHERE service_id = $1 AND revoked_at IS NULL`, serviceID, string(reason)); err != nil {
		return fmt.Errorf("store: close gate overrides (%s): %w", reason, err)
	}
	return nil
}

// PutGatePolicy creates or replaces a service's policy under the D13a/D14 contract, in ONE
// transaction: the service row FOR UPDATE (tenant check and serialization point); the
// document validated exhaustively (refusals name the field); THEN the CAS — expected == nil
// matches only "no row" or "tombstoned row", expected != nil must equal the LIVE row's
// revision (a tombstoned row never matches a non-nil expected), any mismatch is
// ErrGateRevisionConflict and nothing changes; THEN the no-op comparison — an identical live
// document returns the current revision with changed == false and writes nothing (no bump,
// no audit, no override closure). Otherwise: INSERT revision 1, or UPDATE with revision + 1
// clearing the tombstone; the active override is closed as `policy_changed`; the audit row
// `gate.policy.write` carries before → after.
func (s *Store) PutGatePolicy(
	ctx context.Context, projectID, serviceID string, expected *int64, doc domain.GatePolicyDocument, actor GateActor,
) (revision int64, changed bool, err error) {
	if actor.Label == "" {
		return 0, false, errors.New("store: gate policy write requires an actor label")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("store: begin gate policy write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceRowTx(ctx, tx, projectID, serviceID); err != nil {
		return 0, false, err
	}
	clauses, err := validateGatePolicyDocumentTx(ctx, tx, serviceID, doc)
	if err != nil {
		return 0, false, err
	}
	current, found, err := readGatePolicyRowOn(ctx, tx, serviceID, true)
	if err != nil {
		return 0, false, err
	}

	// The CAS, before anything compares documents (D13a).
	live := found && current.Live()
	switch {
	case expected == nil && live:
		return 0, false, ErrGateRevisionConflict
	case expected != nil && (!live || current.Revision != *expected):
		return 0, false, ErrGateRevisionConflict
	}
	// The no-op (D14): identical live document → nothing moves.
	if live && gatePolicyDocumentEqual(current, doc, clauses) {
		return current.Revision, false, nil
	}

	clausesJSON, err := canonicalJSONBytes(clauses)
	if err != nil {
		return 0, false, fmt.Errorf("store: encode gate policy clauses: %w", err)
	}
	var before *domain.GatePolicyDocument
	var beforeClauses map[domain.GateClause]domain.ClauseAssignment
	if live {
		d := current.Document()
		before, beforeClauses = &d, current.Clauses
	}
	if !found {
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_gate_policies
			    (service_id, project_id, window_name, schema_version, clauses, budget_consumed_percent,
			     max_seal_lag_seconds, unknown_behavior, revision, deleted_at, updated_at, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, NULL, now(), $9)
			RETURNING revision`,
			serviceID, projectID, doc.Window, doc.SchemaVersion, clausesJSON, doc.BudgetConsumedPercent,
			doc.MaxSealLagSeconds, string(doc.UnknownBehavior), actor.Label).Scan(&revision); err != nil {
			return 0, false, fmt.Errorf("store: insert gate policy: %w", err)
		}
	} else {
		if err := tx.QueryRow(ctx, `
			UPDATE service_gate_policies
			   SET window_name = $2, schema_version = $3, clauses = $4, budget_consumed_percent = $5,
			       max_seal_lag_seconds = $6, unknown_behavior = $7,
			       revision = revision + 1, deleted_at = NULL, updated_at = now(), updated_by = $8
			 WHERE service_id = $1
			RETURNING revision`,
			serviceID, doc.Window, doc.SchemaVersion, clausesJSON, doc.BudgetConsumedPercent,
			doc.MaxSealLagSeconds, string(doc.UnknownBehavior), actor.Label).Scan(&revision); err != nil {
			return 0, false, fmt.Errorf("store: update gate policy: %w", err)
		}
	}
	if err := closeGateOverridesTx(ctx, tx, serviceID, domain.GateRevokedPolicyChanged); err != nil {
		return 0, false, err
	}
	target := "service=" + serviceID + " revision " + gateRevisionText(before, current) + "→" + strconv.FormatInt(revision, 10) +
		" before=" + gatePolicyAuditText(before, beforeClauses) + " after=" + gatePolicyAuditText(&doc, clauses)
	if err := insertGateAudit(ctx, tx, projectID, actor, "gate.policy.write", target); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("store: commit gate policy write: %w", err)
	}
	return revision, true, nil
}

func gateRevisionText(before *domain.GatePolicyDocument, current domain.GatePolicy) string {
	if before == nil {
		if current.Revision > 0 {
			return "none(" + strconv.FormatInt(current.Revision, 10) + ")"
		}
		return "none"
	}
	return strconv.FormatInt(current.Revision, 10)
}

// DeleteGatePolicy tombstones the policy (D13a): the row is never physically deleted —
// `deleted_at` is set and `revision` bumped in the same statement, so the generation is never
// reused. No live row → ErrGatePolicyNotConfigured; a revision mismatch → conflict, nothing
// changed. The active override is closed as `policy_deleted`; the audit row is
// `gate.policy.delete`.
func (s *Store) DeleteGatePolicy(ctx context.Context, projectID, serviceID string, expected int64, actor GateActor) error {
	if actor.Label == "" {
		return errors.New("store: gate policy delete requires an actor label")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin gate policy delete: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceRowTx(ctx, tx, projectID, serviceID); err != nil {
		return err
	}
	current, found, err := readGatePolicyRowOn(ctx, tx, serviceID, true)
	if err != nil {
		return err
	}
	if !found || !current.Live() {
		return ErrGatePolicyNotConfigured
	}
	if current.Revision != expected {
		return ErrGateRevisionConflict
	}
	var revision int64
	if err := tx.QueryRow(ctx, `
		UPDATE service_gate_policies
		   SET deleted_at = now(), revision = revision + 1, updated_at = now(), updated_by = $2
		 WHERE service_id = $1
		RETURNING revision`, serviceID, actor.Label).Scan(&revision); err != nil {
		return fmt.Errorf("store: tombstone gate policy: %w", err)
	}
	if err := closeGateOverridesTx(ctx, tx, serviceID, domain.GateRevokedPolicyDeleted); err != nil {
		return err
	}
	before := current.Document()
	target := "service=" + serviceID + " revision " + strconv.FormatInt(current.Revision, 10) + "→" + strconv.FormatInt(revision, 10) +
		" before=" + gatePolicyAuditText(&before, current.Clauses) + " after=none"
	if err := insertGateAudit(ctx, tx, projectID, actor, "gate.policy.delete", target); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit gate policy delete: %w", err)
	}
	return nil
}
