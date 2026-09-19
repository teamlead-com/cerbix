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

const projectGatePolicyColumns = `
	p.project_id, p.window_name, p.schema_version, p.clauses, p.budget_consumed_percent,
	p.max_seal_lag_seconds, p.unknown_behavior, p.revision, p.deleted_at, p.updated_at, p.updated_by`

func scanProjectGatePolicy(row scannable) (domain.GatePolicy, error) {
	var policy domain.GatePolicy
	var clauses []byte
	if err := row.Scan(&policy.ProjectID, &policy.Window, &policy.SchemaVersion, &clauses,
		&policy.BudgetConsumedPercent, &policy.MaxSealLagSeconds, &policy.UnknownBehavior,
		&policy.Revision, &policy.DeletedAt, &policy.UpdatedAt, &policy.UpdatedBy); err != nil {
		return domain.GatePolicy{}, err
	}
	if err := json.Unmarshal(clauses, &policy.Clauses); err != nil {
		return domain.GatePolicy{}, fmt.Errorf("store: decode project gate policy clauses: %w", err)
	}
	policy.UpdatedAt = policy.UpdatedAt.UTC()
	if policy.DeletedAt != nil {
		t := policy.DeletedAt.UTC()
		policy.DeletedAt = &t
	}
	return policy, nil
}

func readProjectGatePolicyRowOn(ctx context.Context, q dbConn, projectID string, forUpdate bool) (domain.GatePolicy, bool, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	policy, err := scanProjectGatePolicy(q.QueryRow(ctx,
		`SELECT`+projectGatePolicyColumns+` FROM project_gate_policies p WHERE p.project_id = $1`+lock, projectID))
	if noRows(err) {
		return domain.GatePolicy{}, false, nil
	}
	if err != nil {
		return domain.GatePolicy{}, false, fmt.Errorf("store: read project gate policy: %w", err)
	}
	return policy, true, nil
}

func lockProjectRowTx(ctx context.Context, tx pgx.Tx, projectID string) error {
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id = $1 FOR UPDATE`, projectID).Scan(&id); noRows(err) || isInvalidTextRepresentation(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: lock project: %w", err)
	}
	return nil
}

// GetProjectGatePolicy returns only a live project policy; tombstones stay internal generation
// history and are not exposed as a configured document.
func (s *Store) GetProjectGatePolicy(ctx context.Context, projectID string) (domain.GatePolicy, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)`, projectID).Scan(&exists); isInvalidTextRepresentation(err) {
		return domain.GatePolicy{}, ErrNotFound
	} else if err != nil {
		return domain.GatePolicy{}, fmt.Errorf("store: project gate policy scope: %w", err)
	}
	if !exists {
		return domain.GatePolicy{}, ErrNotFound
	}
	policy, found, err := readProjectGatePolicyRowOn(ctx, s.pool, projectID, false)
	if err != nil {
		return domain.GatePolicy{}, err
	}
	if !found || !policy.Live() {
		return domain.GatePolicy{}, ErrGatePolicyNotConfigured
	}
	return policy, nil
}

func validateProjectGatePolicyDocument(doc domain.GatePolicyDocument) (map[domain.GateClause]domain.ClauseAssignment, error) {
	clauses, err := domain.ValidateGatePolicyV1(doc)
	if err != nil {
		return nil, err
	}
	if _, ok := sla.WindowByName(doc.Window); !ok {
		return nil, &domain.GatePolicyError{Field: "window", Msg: fmt.Sprintf("%q is not an SLA window (the windows are %s)", doc.Window, windowNames())}
	}
	return clauses, nil
}

// PutProjectGatePolicy persists a versioned project source.  It intentionally validates no
// service target: targets remain service data and are evaluated under the effective policy.
func (s *Store) PutProjectGatePolicy(ctx context.Context, projectID string, expected *int64, doc domain.GatePolicyDocument, actor GateActor) (int64, bool, error) {
	if actor.Label == "" {
		return 0, false, errors.New("store: project gate policy write requires an actor label")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("store: begin project gate policy write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockProjectRowTx(ctx, tx, projectID); err != nil {
		return 0, false, err
	}
	clauses, err := validateProjectGatePolicyDocument(doc)
	if err != nil {
		return 0, false, err
	}
	current, found, err := readProjectGatePolicyRowOn(ctx, tx, projectID, true)
	if err != nil {
		return 0, false, err
	}
	live := found && current.Live()
	if expected == nil && live || expected != nil && (!live || current.Revision != *expected) {
		return 0, false, ErrGateRevisionConflict
	}
	if live && gatePolicyDocumentEqual(current, doc, clauses) {
		return current.Revision, false, nil
	}
	raw, err := canonicalJSONBytes(clauses)
	if err != nil {
		return 0, false, fmt.Errorf("store: encode project gate policy clauses: %w", err)
	}
	var revision int64
	if !found {
		err = tx.QueryRow(ctx, `INSERT INTO project_gate_policies (project_id, window_name, schema_version, clauses, budget_consumed_percent, max_seal_lag_seconds, unknown_behavior, revision, updated_at, updated_by) VALUES ($1,$2,$3,$4,$5,$6,$7,1,statement_timestamp(),$8) RETURNING revision`, projectID, doc.Window, doc.SchemaVersion, raw, doc.BudgetConsumedPercent, doc.MaxSealLagSeconds, string(doc.UnknownBehavior), actor.Label).Scan(&revision)
	} else {
		err = tx.QueryRow(ctx, `UPDATE project_gate_policies SET window_name=$2, schema_version=$3, clauses=$4, budget_consumed_percent=$5, max_seal_lag_seconds=$6, unknown_behavior=$7, revision=revision+1, deleted_at=NULL, updated_at=statement_timestamp(), updated_by=$8 WHERE project_id=$1 RETURNING revision`, projectID, doc.Window, doc.SchemaVersion, raw, doc.BudgetConsumedPercent, doc.MaxSealLagSeconds, string(doc.UnknownBehavior), actor.Label).Scan(&revision)
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: write project gate policy: %w", err)
	}
	if err := closeInheritedProjectGateOverridesTx(ctx, tx, projectID, domain.GateRevokedPolicyChanged); err != nil {
		return 0, false, err
	}
	action := "gate.project_policy.create"
	if live {
		action = "gate.project_policy.update"
	}
	if err := insertGateAudit(ctx, tx, projectID, actor, action, "project="+projectID+" revision="+strconv.FormatInt(revision, 10)+" actor="+actor.Label); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("store: commit project gate policy write: %w", err)
	}
	return revision, true, nil
}

func (s *Store) DeleteProjectGatePolicy(ctx context.Context, projectID string, expected int64, actor GateActor) error {
	if actor.Label == "" {
		return errors.New("store: project gate policy delete requires an actor label")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin project gate policy delete: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockProjectRowTx(ctx, tx, projectID); err != nil {
		return err
	}
	current, found, err := readProjectGatePolicyRowOn(ctx, tx, projectID, true)
	if err != nil {
		return err
	}
	if !found || !current.Live() {
		return ErrGatePolicyNotConfigured
	}
	if current.Revision != expected {
		return ErrGateRevisionConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE project_gate_policies SET deleted_at=statement_timestamp(), revision=revision+1, updated_at=statement_timestamp(), updated_by=$2 WHERE project_id=$1`, projectID, actor.Label); err != nil {
		return fmt.Errorf("store: delete project gate policy: %w", err)
	}
	if err := closeInheritedProjectGateOverridesTx(ctx, tx, projectID, domain.GateRevokedPolicyDeleted); err != nil {
		return err
	}
	if err := insertGateAudit(ctx, tx, projectID, actor, "gate.project_policy.delete", "project="+projectID+" revision="+strconv.FormatInt(current.Revision, 10)+" actor="+actor.Label); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit project gate policy delete: %w", err)
	}
	return nil
}

func closeInheritedProjectGateOverridesTx(ctx context.Context, tx pgx.Tx, projectID string, reason domain.GateRevokedReason) error {
	_, err := tx.Exec(ctx, `UPDATE service_gate_overrides o SET revoked_at=statement_timestamp(), revoked_reason=$2 FROM services s LEFT JOIN service_gate_policies sp ON sp.service_id=s.id AND sp.deleted_at IS NULL WHERE o.service_id=s.id AND o.project_id=$1 AND o.policy_source='project' AND o.revoked_at IS NULL AND sp.service_id IS NULL`, projectID, string(reason))
	if err != nil {
		return fmt.Errorf("store: close inherited project gate overrides: %w", err)
	}
	return nil
}

// EffectiveGatePolicy resolves a policy from one database snapshot without materializing a
// service copy.  Service wins; otherwise the project's live policy is inherited.
func (s *Store) EffectiveGatePolicy(ctx context.Context, projectID, serviceID string) (domain.EffectiveGatePolicy, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM services WHERE id=$1 AND project_id=$2)`, serviceID, projectID).Scan(&exists); isInvalidTextRepresentation(err) {
		return domain.EffectiveGatePolicy{}, ErrNotFound
	} else if err != nil {
		return domain.EffectiveGatePolicy{}, fmt.Errorf("store: effective gate policy service scope: %w", err)
	}
	if !exists {
		return domain.EffectiveGatePolicy{}, ErrNotFound
	}
	service, found, err := readGatePolicyRowOn(ctx, s.pool, serviceID, false)
	if err != nil {
		return domain.EffectiveGatePolicy{}, err
	}
	if found && service.Live() {
		rev := service.Revision
		return domain.EffectiveGatePolicy{Policy: service, Source: domain.GatePolicySourceService, OwnerID: serviceID, ServiceOverrideRevision: &rev}, nil
	}
	project, found, err := readProjectGatePolicyRowOn(ctx, s.pool, projectID, false)
	if err != nil {
		return domain.EffectiveGatePolicy{}, err
	}
	if !found || !project.Live() {
		return domain.EffectiveGatePolicy{}, ErrGatePolicyNotConfigured
	}
	return domain.EffectiveGatePolicy{Policy: project, Source: domain.GatePolicySourceProject, OwnerID: projectID}, nil
}

func effectiveGatePolicyTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string) (domain.EffectiveGatePolicy, error) {
	service, found, err := readGatePolicyRowOn(ctx, tx, serviceID, false)
	if err != nil {
		return domain.EffectiveGatePolicy{}, err
	}
	if found && service.Live() {
		revision := service.Revision
		return domain.EffectiveGatePolicy{Policy: service, Source: domain.GatePolicySourceService, OwnerID: serviceID, ServiceOverrideRevision: &revision}, nil
	}
	project, found, err := readProjectGatePolicyRowOn(ctx, tx, projectID, false)
	if err != nil {
		return domain.EffectiveGatePolicy{}, err
	}
	if !found || !project.Live() {
		return domain.EffectiveGatePolicy{}, ErrGatePolicyNotConfigured
	}
	return domain.EffectiveGatePolicy{Policy: project, Source: domain.GatePolicySourceProject, OwnerID: projectID}, nil
}
