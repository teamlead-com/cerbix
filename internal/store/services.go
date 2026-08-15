package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Service declarations and their evaluation epochs (func-service-reliability §6, §15.4).
//
// Everything in this file writes BOTH axes in ONE transaction. A definition revision with
// no matching epoch is not a latency problem, it is an unsatisfiable reference: a fact
// points at the epoch alone, so a revision that never got one can never be referenced by
// anything the evaluator produces.

// ErrRevisionConflict is returned when the caller's observed revision is no longer the
// current one. Two operators editing an SLI must not silently interleave, so the write is
// optimistic and the loser is told rather than merged.
var ErrRevisionConflict = errors.New("store: service declaration changed since it was read")

// ErrSLINotInContext is returned when a declared reliability input is not also in the
// operational context. The two lists are declared independently, but an SLI member outside
// the context would be a number with no visible source.
var ErrSLINotInContext = errors.New("store: every sli member must also be in monitors")

// lockServiceMembership takes the project-scoped advisory lock that serializes every path
// able to change which services a mutation affects.
//
// It is the OUTERMOST lock of §15.4, ahead of secrets, monitors and service rows, because
// it is coarse and project-scoped: a path that discovered it needed this lock while already
// holding a finer one would invert the order. Row locks cannot substitute for it — they
// protect rows that exist, and the thing being protected here is a predicate, so a service
// created concurrently is exactly what slips through.
func lockServiceMembership(ctx context.Context, tx pgx.Tx, projectID string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('service_membership'), hashtext($1))`, projectID); err != nil {
		return fmt.Errorf("store: lock service membership: %w", err)
	}
	return nil
}

// CreateService adds a Service with no declaration. A service with no reliability inputs is
// a valid state — operational context and no SLO — and it reports availability as
// unavailable rather than as 100%.
func (s *Store) CreateService(ctx context.Context, svc domain.Service) (domain.Service, error) {
	if svc.ProjectID == "" || svc.Slug == "" || svc.Name == "" {
		return domain.Service{}, fmt.Errorf("store: service requires project_id, slug and name")
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO services (project_id, slug, name, description, escalation_policy_id, oncall_schedule_id)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING id, project_id, slug, name, description,
		           COALESCE(escalation_policy_id::text,''), COALESCE(oncall_schedule_id::text,''),
		           created_at, updated_at`,
		svc.ProjectID, svc.Slug, svc.Name, svc.Description,
		nullableID(svc.EscalationPolicyID), nullableID(svc.OncallScheduleID))
	return scanService(row)
}

// GetService reads one service by id, scoped to its project.
func (s *Store) GetService(ctx context.Context, projectID, id string) (domain.Service, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, project_id, slug, name, description,
		        COALESCE(escalation_policy_id::text,''), COALESCE(oncall_schedule_id::text,''),
		        created_at, updated_at
		   FROM services WHERE id = $1 AND project_id = $2`, id, projectID)
	return scanService(row)
}

// ListServices returns a project's services, slug-ordered.
func (s *Store) ListServices(ctx context.Context, projectID string) ([]domain.Service, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, project_id, slug, name, description,
		        COALESCE(escalation_policy_id::text,''), COALESCE(oncall_schedule_id::text,''),
		        created_at, updated_at
		   FROM services WHERE project_id = $1 ORDER BY slug`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list services: %w", err)
	}
	defer rows.Close()
	var out []domain.Service
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// DeleteService removes a service and, by cascade, its declarations, epochs, facts, ingest
// rows, late arrivals and repair ranges — in one transaction, so nothing is left pointing
// at a service that no longer exists.
func (s *Store) DeleteService(ctx context.Context, projectID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM services WHERE id = $1 AND project_id = $2`, id, projectID)
	if err != nil {
		return fmt.Errorf("store: delete service: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanService(row scannable) (domain.Service, error) {
	var svc domain.Service
	err := row.Scan(&svc.ID, &svc.ProjectID, &svc.Slug, &svc.Name, &svc.Description,
		&svc.EscalationPolicyID, &svc.OncallScheduleID, &svc.CreatedAt, &svc.UpdatedAt)
	if noRows(err) {
		return domain.Service{}, ErrNotFound
	}
	if err != nil {
		return domain.Service{}, fmt.Errorf("store: scan service: %w", err)
	}
	return svc, nil
}

// PutServiceDeclaration commits a new definition revision AND its matching evaluation
// epoch, atomically.
//
// expectedRevision is the revision the caller observed; 0 means "no declaration yet". A
// mismatch is ErrRevisionConflict rather than a merge, because two operators editing an SLI
// have made two different statements about what availability means and picking one silently
// is the worst of the three options.
func (s *Store) PutServiceDeclaration(
	ctx context.Context, projectID, serviceID string,
	decl domain.ServiceDeclaration, expectedRevision int64, createdBy string,
) (domain.DefinitionRevision, domain.EvaluationEpoch, error) {
	monitors := dedupeIDs(decl.Monitors)
	sli := dedupeIDs(decl.SLI)
	inContext := map[string]bool{}
	for _, id := range monitors {
		inContext[id] = true
	}
	for _, id := range sli {
		if !inContext[id] {
			return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, ErrSLINotInContext
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: begin declaration: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// §15.4 lock order, outermost first. Nothing below may be taken before these.
	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}
	// Referenced monitors, id ascending and FOR KEY SHARE: the declaration names them, so
	// they must not vanish under the write, and ascending order is what keeps this path out
	// of a cycle with every other path that touches monitors.
	if len(monitors) > 0 {
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM monitors WHERE id = ANY($1) AND project_id = $2 ORDER BY id FOR KEY SHARE`,
			monitors, projectID); err != nil {
			return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: lock declared monitors: %w", err)
		}
	}
	var lockedService string
	err = tx.QueryRow(ctx, `SELECT id FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		serviceID, projectID).Scan(&lockedService)
	if noRows(err) {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, ErrNotFound
	}
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: lock service: %w", err)
	}

	// Optimistic concurrency against the CURRENT effective revision.
	var currentRevision int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM service_definition_revisions
		  WHERE service_id = $1 AND state = 'effective'`, serviceID).Scan(&currentRevision); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: current revision: %w", err)
	}
	if currentRevision != expectedRevision {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, ErrRevisionConflict
	}

	members, err := s.loadEpochMembers(ctx, tx, projectID, sli, decl.Policies)
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}

	// Policies are defaulted and validated against the DECLARED cardinality, by the one
	// validator the API and the file provider share.
	declaredPerRegion := map[string]int{}
	for _, m := range members {
		declaredPerRegion[m.Semantics.Region]++
	}
	policies := decl.Policies
	if len(members) > 0 {
		policies = domain.ApplyServicePolicyDefaults(policies, declaredPerRegion, len(declaredPerRegion))
		if err := domain.ValidateServicePolicies(policies, declaredPerRegion, len(declaredPerRegion)); err != nil {
			return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
		}
	}

	// The database clock decides when this write happened, and the boundary is ceiled from
	// it in the SAME statement. An application clock here would let two nodes disagree about
	// which bucket a revision first governs, and two statements would let them disagree with
	// each other.
	var createdAt, effectiveAt time.Time
	if err := tx.QueryRow(ctx,
		`WITH t AS (SELECT statement_timestamp() AS at)
		 SELECT at,
		        CASE WHEN date_bin('1 minute', at, 'epoch'::timestamptz) = at
		             THEN at
		             ELSE date_bin('1 minute', at, 'epoch'::timestamptz) + interval '1 minute' END
		   FROM t`).Scan(&createdAt, &effectiveAt); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: resolve effective_at: %w", err)
	}

	// Same-boundary resolution. Two writes seconds apart target the same next boundary; the
	// later one wins and the earlier is retained for audit as a row that never governed. Its
	// durable ranges go with it, so no job survives pointing at a row without effect.
	if err := supersedeAtBoundary(ctx, tx, serviceID, effectiveAt); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}

	policyJSON, err := json.Marshal(policies)
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: encode policies: %w", err)
	}
	rev := domain.DefinitionRevision{
		ServiceID: serviceID, ProjectID: projectID, Revision: currentRevision + 1,
		CreatedAt: createdAt, EffectiveAt: effectiveAt, State: domain.RevisionEffective,
		Monitors: monitors, SLI: sli, Policies: policies, CreatedBy: createdBy,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO service_definition_revisions
		   (service_id, project_id, revision, created_at, effective_at, policies, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		serviceID, projectID, rev.Revision, createdAt, effectiveAt, policyJSON, createdBy).Scan(&rev.ID); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: insert revision: %w", err)
	}

	if err := writeRevisionMembers(ctx, tx, rev.ID, projectID, monitors, sli); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}
	if err := replaceMemberRefs(ctx, tx, serviceID, projectID, monitors, sli); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}

	epoch, err := insertEpoch(ctx, tx, rev, members)
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: commit declaration: %w", err)
	}
	return rev, epoch, nil
}

// supersedeEpochsAtBoundary yields the EPOCH axis at one boundary, and touches nothing else.
//
// This is the whole operation an execution-driven epoch is allowed to perform. An earlier
// draft reused the declaration-path helper here, which also superseded the REVISION at that
// boundary — so an ordinary monitor edit silently marked the service's declaration as never
// having taken effect, and the next epoch then found no declaration to resolve and created
// nothing. An execution change must never mutate a declaration; that is the point of
// splitting the two axes.
func supersedeEpochsAtBoundary(ctx context.Context, tx pgx.Tx, serviceID string, effectiveAt time.Time) error {
	if _, err := tx.Exec(ctx,
		`UPDATE service_evaluation_epochs SET state = 'superseded_before_effect'
		  WHERE service_id = $1 AND effective_at = $2 AND state = 'effective'`,
		serviceID, effectiveAt); err != nil {
		return fmt.Errorf("store: supersede epoch at boundary: %w", err)
	}
	return nil
}

// supersedeAtBoundary is the DECLARATION path: a new revision claims the boundary, so any
// revision and any epoch already claiming it yield, and the durable ranges scoped to them
// are cancelled.
func supersedeAtBoundary(ctx context.Context, tx pgx.Tx, serviceID string, effectiveAt time.Time) error {
	if _, err := tx.Exec(ctx,
		`UPDATE service_definition_revisions SET state = 'superseded_before_effect'
		  WHERE service_id = $1 AND effective_at = $2 AND state = 'effective'`,
		serviceID, effectiveAt); err != nil {
		return fmt.Errorf("store: supersede revision at boundary: %w", err)
	}
	if err := supersedeEpochsAtBoundary(ctx, tx, serviceID, effectiveAt); err != nil {
		return err
	}
	// A row that never took effect governs no bucket, so work scoped to it is work with no
	// target. Cancelling it here — in the same transaction — is what stops a job from
	// outliving the row that asked for it.
	if _, err := tx.Exec(ctx,
		`UPDATE service_repair_ranges SET state = 'superseded', updated_at = now()
		  WHERE service_id = $1 AND range_start >= $2 AND state IN ('pending','running')`,
		serviceID, effectiveAt); err != nil {
		return fmt.Errorf("store: cancel superseded ranges: %w", err)
	}
	return nil
}

func writeRevisionMembers(ctx context.Context, tx pgx.Tx, revisionID, projectID string, monitors, sli []string) error {
	sliSet := map[string]bool{}
	for _, id := range sli {
		sliSet[id] = true
	}
	for _, id := range monitors {
		roles := []domain.MemberRole{domain.RoleContext}
		if sliSet[id] {
			roles = append(roles, domain.RoleSLI)
		}
		for _, role := range roles {
			// The name snapshot is what keeps a historical row legible after the monitor it
			// names is deleted — this table deliberately has no FK to monitors.
			if _, err := tx.Exec(ctx,
				`INSERT INTO service_definition_members (revision_id, project_id, monitor_id, monitor_name, role)
				 VALUES ($1,$2,$3, COALESCE((SELECT name FROM monitors WHERE id = $3), ''), $4)`,
				revisionID, projectID, id, string(role)); err != nil {
				return fmt.Errorf("store: insert revision member: %w", err)
			}
		}
	}
	return nil
}

// replaceMemberRefs rewrites the CURRENT reference set. These rows carry the deferred FK
// that makes deleting an in-use monitor fail at COMMIT, and they are what the ingest
// handshake reads to answer "which services declare this monitor".
func replaceMemberRefs(ctx context.Context, tx pgx.Tx, serviceID, projectID string, monitors, sli []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM service_member_refs WHERE service_id = $1`, serviceID); err != nil {
		return fmt.Errorf("store: clear member refs: %w", err)
	}
	sliSet := map[string]bool{}
	for _, id := range sli {
		sliSet[id] = true
	}
	for _, id := range monitors {
		roles := []domain.MemberRole{domain.RoleContext}
		if sliSet[id] {
			roles = append(roles, domain.RoleSLI)
		}
		for _, role := range roles {
			if _, err := tx.Exec(ctx,
				`INSERT INTO service_member_refs (service_id, project_id, monitor_id, role) VALUES ($1,$2,$3,$4)`,
				serviceID, projectID, id, string(role)); err != nil {
				return fmt.Errorf("store: insert member ref: %w", err)
			}
		}
	}
	return nil
}

// loadEpochMembers builds the snapshot: the evaluation-semantics projection of every
// declared SLI member, plus its resolved staleness deadline and dead-man baseline.
//
// It reads through scanMonitorNoSecrets — the decrypt-free boundary — so credential
// plaintext is never even materialized on the way to the snapshot. That is a schema
// guarantee rather than a discipline one, which is the difference between "we filter it
// out" and "it was never there".
func (s *Store) loadEpochMembers(ctx context.Context, tx pgx.Tx, projectID string, sli []string, policies domain.ServicePolicies) ([]domain.EpochMember, error) {
	if len(sli) == 0 {
		return nil, nil
	}
	freshness := policies.Freshness
	if freshness.ActiveMultiplier == 0 && freshness.ActiveFloor == 0 {
		freshness = domain.DefaultFreshnessPolicy()
	}

	// Credential generations come from the referenced secrets. Only identity and rotation
	// reach the snapshot; the value never does.
	gens := map[string]map[string]string{}
	grows, err := tx.Query(ctx,
		`SELECT r.monitor_id, r.setting_key, COALESCE(s.rotated_at, s.created_at)
		   FROM monitor_secret_refs r
		   JOIN project_secrets s ON s.id = r.secret_id AND s.project_id = r.project_id
		  WHERE r.monitor_id = ANY($1) AND r.project_id = $2`, sli, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: read credential generations: %w", err)
	}
	for grows.Next() {
		var monitorID, key string
		var gen time.Time
		if err := grows.Scan(&monitorID, &key, &gen); err != nil {
			grows.Close()
			return nil, fmt.Errorf("store: scan credential generation: %w", err)
		}
		if gens[monitorID] == nil {
			gens[monitorID] = map[string]string{}
		}
		gens[monitorID][key] = gen.UTC().Format(time.RFC3339Nano)
	}
	grows.Close()
	if err := grows.Err(); err != nil {
		return nil, fmt.Errorf("store: read credential generations: %w", err)
	}

	byID := map[string]domain.EpochMember{}
	rows, err := tx.Query(ctx,
		`SELECT `+monitorColumns+`, COALESCE(push_armed_at, created_at)
		   FROM monitors WHERE id = ANY($1) AND project_id = $2`, sli, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: read declared members: %w", err)
	}
	for rows.Next() {
		var armedAt time.Time
		m, err := s.scanMonitorNoSecrets(rows, &armedAt)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan declared member: %w", err)
		}
		sem, err := domain.MonitorEvaluationSemantics(m, gens[m.ID])
		if err != nil {
			rows.Close()
			return nil, err
		}
		byID[m.ID] = domain.EpochMember{
			MonitorID: m.ID,
			Semantics: sem,
			StaleAfter: domain.ResolveStaleAfter(freshness, m.Type,
				time.Duration(m.IntervalSeconds)*time.Second, time.Duration(m.GraceSeconds)*time.Second),
			ArmedAt: armedAt,
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read declared members: %w", err)
	}

	// Ordered by the declaration, so the snapshot hash does not depend on how Postgres
	// happened to return the rows.
	out := make([]domain.EpochMember, 0, len(sli))
	for _, id := range sli {
		m, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("store: declared sli member %s is not a monitor in this project", id)
		}
		out = append(out, m)
	}
	return out, nil
}

// insertEpoch writes the epoch that matches a revision. It is unconditional: the
// snapshot-hash no-op rule applies only to epochs driven by a monitor execution write, and
// applying it here would leave a revision no fact could reference.
func insertEpoch(ctx context.Context, tx pgx.Tx, rev domain.DefinitionRevision, members []domain.EpochMember) (domain.EvaluationEpoch, error) {
	snapshot, err := json.Marshal(members)
	if err != nil {
		return domain.EvaluationEpoch{}, fmt.Errorf("store: encode epoch snapshot: %w", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(epoch_seq), 0) + 1 FROM service_evaluation_epochs WHERE service_id = $1`,
		rev.ServiceID).Scan(&seq); err != nil {
		return domain.EvaluationEpoch{}, fmt.Errorf("store: next epoch seq: %w", err)
	}
	epoch := domain.EvaluationEpoch{
		ServiceID: rev.ServiceID, ProjectID: rev.ProjectID, Seq: seq, RevisionID: rev.ID,
		CreatedAt: rev.CreatedAt, EffectiveAt: rev.EffectiveAt, State: domain.RevisionEffective,
		Members: members, SnapshotHash: domain.EpochSnapshotHash(members),
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO service_evaluation_epochs
		   (service_id, project_id, epoch_seq, revision_id, created_at, effective_at, snapshot, snapshot_hash)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		epoch.ServiceID, epoch.ProjectID, seq, rev.ID, rev.CreatedAt, rev.EffectiveAt,
		snapshot, epoch.SnapshotHash).Scan(&epoch.ID); err != nil {
		return domain.EvaluationEpoch{}, fmt.Errorf("store: insert epoch: %w", err)
	}
	return epoch, nil
}

func dedupeIDs(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, id := range in {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
