package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	if svc.ProjectID == "" || svc.Name == "" {
		return domain.Service{}, fmt.Errorf("store: service requires project_id and name")
	}
	// The slug is the URL segment AND the bundle's reference key, so a malformed one is a
	// broken reference in two places. Both callers already check; the store checks too,
	// because it is the one place every future caller has to come through.
	if !domain.ValidServiceSlug(svc.Slug) {
		return domain.Service{}, fmt.Errorf("store: service slug must match %s", domain.MonitorSlugPattern())
	}
	// The membership advisory lock is OUTERMOST (§15.4), and creating a service is a change
	// to the set a preview's staleness check compares against. Without it the cap is a
	// check-then-act, and a concurrent create can slip inside a confirm's predicate window —
	// row locks cannot help, because a service that does not exist yet has no row to lock.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Service{}, fmt.Errorf("store: begin create service: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := lockServiceMembership(ctx, tx, svc.ProjectID); err != nil {
		return domain.Service{}, err
	}

	// The owner is a REFERENCE to this project's routing primitives. The composite FK added
	// in 00069 makes a cross-tenant owner impossible in the schema; this turns the resulting
	// constraint violation into an answer the caller can act on, and takes the rows in the
	// §15.4 order (routing before services) with KEY SHARE so they cannot vanish under the
	// insert.
	if err := assertOwnerInProjectTx(ctx, tx, svc.ProjectID, svc.EscalationPolicyID, svc.OncallScheduleID); err != nil {
		return domain.Service{}, err
	}

	if err := s.assertProjectServiceCapTx(ctx, tx, svc.ProjectID, 1); err != nil {
		return domain.Service{}, err
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO services (project_id, slug, name, description, escalation_policy_id, oncall_schedule_id)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING id, project_id, slug, name, description,
		           COALESCE(escalation_policy_id::text,''), COALESCE(oncall_schedule_id::text,''),
		           created_at, updated_at`,
		svc.ProjectID, svc.Slug, svc.Name, svc.Description,
		nullableID(svc.EscalationPolicyID), nullableID(svc.OncallScheduleID))
	out, err := scanService(row)
	if isUniqueViolation(err) {
		// The slug is project-unique and IMMUTABLE, so a collision is not something the
		// caller can be helped past by retrying with the same input — it is a conflict,
		// and the 500 it would otherwise become would hide that.
		return domain.Service{}, ErrConflict
	}
	if err != nil {
		return domain.Service{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Service{}, fmt.Errorf("store: commit create service: %w", err)
	}
	return out, nil
}

// GetService reads one service by id, scoped to its project.
func (s *Store) GetService(ctx context.Context, projectID, id string) (domain.Service, error) {
	return getServiceOn(ctx, s.pool, projectID, id)
}

func getServiceOn(ctx context.Context, q queryRower, projectID, id string) (domain.Service, error) {
	row := q.QueryRow(ctx,
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin delete service: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// §15.4 lock order: the membership advisory lock is OUTERMOST and has to be taken before
	// the service row, not after it. Deleting a service changes the very set a maintenance
	// preview's staleness check compares against.
	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return err
	}
	// membership → graph, never the other way (§14.1): the delete's cascade removes
	// edges, so it is an edge-mutating path and serializes with every other one.
	if err := lockServiceGraph(ctx, tx, projectID); err != nil {
		return err
	}

	// Then the row, so a file apply claiming ownership serializes behind this check rather
	// than racing it. Absence of the row is ErrNotFound before ownership is consulted.
	var one int
	err = tx.QueryRow(ctx,
		`SELECT 1 FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`, id, projectID).Scan(&one)
	if noRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock service: %w", err)
	}
	if err := assertServiceNotFileManagedTx(ctx, tx, id); err != nil {
		return err
	}
	// A desired file edge pins its target (§14.2): while an applied file-owned service
	// names this one in depends_on, deletion is a 409 naming the provider — the guard
	// that keeps the provider's last-known-good literally true.
	if err := assertServiceNotPinnedByFileEdgesTx(ctx, tx, id); err != nil {
		return err
	}
	// A preview whose affected set included this service goes stale on its own: the stored
	// set is a snapshot the deletion cannot edit (00068 dropped that cascade), so the set
	// comparison in confirmPreviewTx sees a member the current set no longer has.
	if _, err := tx.Exec(ctx, `DELETE FROM services WHERE id = $1 AND project_id = $2`, id, projectID); err != nil {
		return fmt.Errorf("store: delete service: %w", err)
	}
	// Audited in the SAME transaction: a deleted service takes its facts with it (cascade),
	// and the audit row is then the only durable statement that the deletion was an act.
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 SELECT p.org_id, NULL, false, 'service.deleted', $2
		   FROM projects p WHERE p.id = $1`,
		projectID, "service="+id); err != nil {
		return fmt.Errorf("store: audit service delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit delete service: %w", err)
	}
	return nil
}

// ErrServiceManagedByFile is returned by the UI/API write paths when the target service is
// owned by a file provider. A declaration written here would be silently restated by the very
// next reconcile — the file, not the UI, is where that service's meaning is edited. Ownership
// blocks regardless of orphan state: there is no automatic release.
//
// It is distinct from ErrManagedByFile (monitors) only so the message stays true; both map to
// the same 409.
var ErrServiceManagedByFile = errors.New("store: service is managed by a file provider")

// assertServiceNotFileManagedTx rejects a UI write to a file-owned service. The caller holds
// the service row (or the membership lock), so the check is atomic against a concurrent apply.
func assertServiceNotFileManagedTx(ctx context.Context, tx pgx.Tx, serviceID string) error {
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM managed_services WHERE service_id = $1`, serviceID).Scan(&one)
	if noRows(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: service ownership check: %w", err)
	}
	return ErrServiceManagedByFile
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

// DeclarationOptions carries the parts of a declaration write that are not the declaration.
type DeclarationOptions struct {
	CreatedBy string
	// BackfillFrom makes the FIRST revision retroactive, so that a service adopted over
	// existing history has a producing definition for the facts that history will yield —
	// §6.6, the one retroactive case. It is floored to a bucket boundary.
	//
	// What it produces is a DECLARED RECONSTRUCTION, not a measurement claim: the range is
	// evaluated with today's members as declared today, because no history of what each
	// monitor's interval, region or target used to be exists to read instead. Callers must
	// label it as such wherever they show it.
	//
	// It is rejected on any revision but the first. A later revision reaching backwards would
	// rewrite facts another declaration already produced, which is an audited administrative
	// repair and not an ordinary edit.
	BackfillFrom time.Time
}

// ErrRetroactiveNotFirstRevision is returned when a write after the first asks to reach
// backwards.
var ErrRetroactiveNotFirstRevision = errors.New("store: only the first revision may be retroactive")

// PutServiceDeclaration commits a new definition revision AND its matching evaluation
// epoch, atomically.
//
// expectedRevision is the revision the caller observed; 0 means "no declaration yet". A
// mismatch is ErrRevisionConflict rather than a merge, because two operators editing an SLI
// have made two different statements about what availability means and picking one silently
// is the worst of the three options.
func (s *Store) PutServiceDeclaration(
	ctx context.Context, projectID, serviceID string,
	decl domain.ServiceDeclaration, expectedRevision int64, opts DeclarationOptions,
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
	// Only the UI path guards ownership: the file apply reaches putServiceDeclarationTx
	// directly, which is exactly the write this refuses to let a human make behind its back.
	if err := assertServiceNotFileManagedTx(ctx, tx, serviceID); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}
	rev, epoch, err := s.putServiceDeclarationTx(ctx, tx, projectID, serviceID, monitors, sli, decl.Policies, expectedRevision, opts)
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}
	// The declaration change is audited IN the transaction that makes it, on the UI path
	// only — the file apply audits at bundle level, and the system retirement path writes
	// its own event. The revision row already records who; the audit log is where an org
	// admin looks first, and a declaration that redefines what availability MEANS belongs
	// there next to every other privileged act.
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 SELECT p.org_id, NULL, false, 'service.declaration_put', $2
		   FROM projects p WHERE p.id = $1`,
		projectID, fmt.Sprintf("service=%s revision=%d by=%s", serviceID, rev.Revision, opts.CreatedBy)); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: audit declaration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: commit declaration: %w", err)
	}
	return rev, epoch, nil
}

// putServiceDeclarationTx is the body, separated so the file-provider apply can write a
// declaration inside the transaction that is already applying the bundle's monitors — the
// service and the monitors it names have to become visible together.
//
// The caller is responsible for having taken the §15.4 locks, membership first.
func (s *Store) putServiceDeclarationTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string,
	monitors, sli []string, rawPolicies domain.ServicePolicies, expectedRevision int64, opts DeclarationOptions,
) (domain.DefinitionRevision, domain.EvaluationEpoch, error) {
	createdBy := opts.CreatedBy
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
	err := tx.QueryRow(ctx, `SELECT id FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`,
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

	if err := s.enforceDeclarationBoundsTx(ctx, tx, projectID, serviceID, monitors, sli); err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}

	members, err := s.loadEpochMembers(ctx, tx, projectID, sli, rawPolicies)
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}

	// Policies are defaulted and validated against the DECLARED cardinality, by the one
	// validator the API and the file provider share.
	declaredPerRegion := map[string]int{}
	for _, m := range members {
		declaredPerRegion[m.Semantics.Region]++
	}
	policies := rawPolicies
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
	if !opts.BackfillFrom.IsZero() {
		if currentRevision != 0 {
			return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, ErrRetroactiveNotFirstRevision
		}
		// The one retroactive case, and it FLOORS rather than ceils: the first revision must
		// already be in force over the range it is about to backfill, or the facts it produces
		// would predate the definition that produced them.
		if err := tx.QueryRow(ctx,
			`SELECT date_bin('1 minute', $1::timestamptz, 'epoch'::timestamptz)`, opts.BackfillFrom).Scan(&effectiveAt); err != nil {
			return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: resolve backfill boundary: %w", err)
		}
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
	if err == nil {
		// Same transaction as the epoch: fan-out counts only what commits.
		err = bumpMetricEventTx(ctx, tx, metricEventEpochFanout, 1)
	}
	if err != nil {
		return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
	}

	// The declaration is what puts the service on the materialization path. Without this the
	// whole subsystem was inert in production: rows existed, the evaluator was correct, and
	// nothing ever asked it to run — every seal in the test suite came from a test calling
	// the store directly.
	//
	// Only a declaration that actually has reliability inputs starts materialization. A
	// service with an empty SLI produces no facts by design, and giving it a start would
	// queue a driver that can only ever walk over nothing.
	if len(sli) > 0 {
		if err := ensureMaterializationRowTx(ctx, tx, projectID, serviceID, effectiveAt); err != nil {
			return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, err
		}
		// Coming back from a declared silence starts a NEW contiguity era. Without this the
		// watermark stops in front of the empty period forever and a re-enabled service can
		// never report again: the gap is not a stall the system owes buckets for, it is a
		// period a human declared it was measuring nothing.
		if currentRevision > 0 {
			var prevHadInputs bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(
				   SELECT 1 FROM service_definition_members m
				     JOIN service_definition_revisions r ON r.id = m.revision_id
				    WHERE r.service_id = $1 AND r.revision = $2 AND m.role = 'sli')`,
				serviceID, currentRevision).Scan(&prevHadInputs); err != nil {
				return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: read previous inputs: %w", err)
			}
			if !prevHadInputs {
				// The cursor moves WITH the era, in the same statement. The silent period
				// produces no facts by declaration, so there is nothing there for the
				// driver to walk — but if the scheduler was down or backlogged through the
				// silence, `materialized_through` still points at its beginning, and a
				// revived service would then burn one empty bucket per slice for the whole
				// gap (90 days of silence = 129 600 buckets) before reaching the new era.
				// GREATEST keeps a cursor that is somehow already past the boundary.
				if _, err := tx.Exec(ctx,
					`UPDATE service_materialization
					    SET era_start = $2, sealed_through = NULL,
					        materialized_through = GREATEST(COALESCE(materialized_through, $2), $2)
					  WHERE service_id = $1`, serviceID, effectiveAt); err != nil {
					return domain.DefinitionRevision{}, domain.EvaluationEpoch{}, fmt.Errorf("store: start materialization era: %w", err)
				}
			}
		}
	}
	return rev, epoch, nil
}

// The cap numbers are the domain's (§10.10); config validates operator input against them
// fail-fast, so by the time a value reaches this store it is legal. The store still refuses
// to run past the hard maxima — defense in depth, not a second interpretation of the config.
const (
	DefaultMaxServicesPerProject = domain.DefaultMaxServicesPerProject
	HardMaxServicesPerProject    = domain.HardMaxServicesPerProject
	DefaultMaxMembersPerRevision = domain.DefaultMaxMembersPerRevision
	HardMaxMembersPerRevision    = domain.HardMaxMembersPerRevision
	DefaultMaxServicesPerMonitor = domain.DefaultMaxServicesPerMonitor
	HardMaxServicesPerMonitor    = domain.HardMaxServicesPerMonitor
)

// ServiceLimits is the resolved, clamped policy this store enforces.
type ServiceLimits struct {
	ServicesPerProject int
	MembersPerRevision int
	ServicesPerMonitor int
}

// capServiceLimits fills zeros with the defaults (for programmatic callers that set nothing)
// and refuses to run past the hard maxima. It is DEFENSE, not policy: operator configuration
// is validated fail-fast in internal/config, which REJECTS a negative or over-maximum value
// at startup rather than silently reinterpreting it — a config the operator wrote and the
// config the system runs must be the same config (FR-003).
func capServiceLimits(in ServiceLimits) ServiceLimits {
	pick := func(v, def, hard int) int {
		if v <= 0 {
			v = def
		}
		if v > hard {
			v = hard
		}
		return v
	}
	return ServiceLimits{
		ServicesPerProject: pick(in.ServicesPerProject, DefaultMaxServicesPerProject, HardMaxServicesPerProject),
		MembersPerRevision: pick(in.MembersPerRevision, DefaultMaxMembersPerRevision, HardMaxMembersPerRevision),
		ServicesPerMonitor: pick(in.ServicesPerMonitor, DefaultMaxServicesPerMonitor, HardMaxServicesPerMonitor),
	}
}

// serviceLimits resolves what this store enforces right now.
func (s *Store) serviceLimits() ServiceLimits { return capServiceLimits(s.svcLimits) }

// assertProjectServiceCapTx is the ONE owner of the per-project cap, shared by the UI create
// and the file-provider apply. Two writers with one counting them is how the cap became a
// claim rather than a rule.
//
// The caller holds the project's service-membership advisory lock, which is what makes this a
// serialized check rather than a check-then-act (§10.10).
func (s *Store) assertProjectServiceCapTx(ctx context.Context, tx pgx.Tx, projectID string, adding int) error {
	limit := s.serviceLimits().ServicesPerProject
	var existing int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM services WHERE project_id = $1`, projectID).Scan(&existing); err != nil {
		return fmt.Errorf("store: count services: %w", err)
	}
	if existing+adding > limit {
		return fmt.Errorf("%w: %d + %d exceeds the cap of %d", ErrTooManyServices, existing, adding, limit)
	}
	return nil
}

var (
	// ErrTooManyMembers is returned when a declaration's context exceeds the cap.
	ErrTooManyMembers = errors.New("store: too many members in one declaration")
	// ErrMonitorInTooManyServices is returned when declaring this SLI would push a monitor
	// past the per-monitor service cap.
	ErrMonitorInTooManyServices = errors.New("store: monitor is a reliability input for too many services")
	// ErrTooManyServices is returned when a project is at its service cap.
	ErrTooManyServices = errors.New("store: too many services in this project")
)

// enforceDeclarationBoundsTx applies the fan-out caps ON THE WRITE PATH.
//
// They existed before only as a read-time check inside noteHeartbeatForServices — which runs
// in the HEARTBEAT's transaction. So the 26th declaration of a monitor was accepted happily
// and then broke ingest for that monitor: a service-configuration change took down core
// monitoring, and the error surfaced nowhere near the write that caused it. A cap has to be
// refused where it is exceeded, not enforced later by a kill switch.
func (s *Store) enforceDeclarationBoundsTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, monitors, sli []string,
) error {
	limits := s.serviceLimits()
	if len(monitors) > limits.MembersPerRevision {
		return fmt.Errorf("%w: %d declared, cap %d", ErrTooManyMembers, len(monitors), limits.MembersPerRevision)
	}
	if len(sli) == 0 {
		return nil
	}
	// How many OTHER services already declare each of these monitors as a reliability input.
	// Serialized by the per-service lock this transaction already holds plus the membership
	// advisory lock its callers take, so two concurrent declarations cannot both squeeze in.
	rows, err := tx.Query(ctx,
		`SELECT monitor_id, count(*) FROM service_member_refs
		  WHERE project_id = $1 AND role = 'sli' AND monitor_id = ANY($2) AND service_id <> $3
		  GROUP BY monitor_id`,
		projectID, sli, serviceID)
	if err != nil {
		return fmt.Errorf("store: count service fan-out: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var monitorID string
		var n int
		if err := rows.Scan(&monitorID, &n); err != nil {
			return fmt.Errorf("store: scan fan-out: %w", err)
		}
		if n+1 > limits.ServicesPerMonitor {
			return fmt.Errorf("%w: %s would be an input for %d services, cap %d",
				ErrMonitorInTooManyServices, monitorID, n+1, limits.ServicesPerMonitor)
		}
	}
	return rows.Err()
}

// ErrOwnerNotInProject is returned when a service names routing that belongs to some other
// project. Routing decides who gets paged, so pointing it across a tenant boundary is the one
// mistake in this feature that wakes the wrong humans.
var ErrOwnerNotInProject = errors.New("store: owner does not belong to this project")

func assertOwnerInProjectTx(ctx context.Context, tx pgx.Tx, projectID, escalationID, oncallID string) error {
	if escalationID != "" {
		var one int
		err := tx.QueryRow(ctx,
			`SELECT 1 FROM escalation_policies WHERE id = $1 AND project_id = $2 FOR KEY SHARE`,
			escalationID, projectID).Scan(&one)
		if noRows(err) {
			return fmt.Errorf("%w: escalation policy", ErrOwnerNotInProject)
		}
		if err != nil {
			return fmt.Errorf("store: check escalation policy tenancy: %w", err)
		}
	}
	if oncallID != "" {
		var one int
		err := tx.QueryRow(ctx,
			`SELECT 1 FROM oncall_schedules WHERE id = $1 AND project_id = $2 FOR KEY SHARE`,
			oncallID, projectID).Scan(&one)
		if noRows(err) {
			return fmt.Errorf("%w: on-call schedule", ErrOwnerNotInProject)
		}
		if err != nil {
			return fmt.Errorf("store: check on-call schedule tenancy: %w", err)
		}
	}
	return nil
}

// ensureMaterializationRowTx puts the service on the driver's list, once.
//
// `materialization_start` is the earliest instant this service will ever have facts for, and
// it NEVER moves: a later revision cannot make history start earlier or later than it did,
// and moving it would silently redefine what a complete window means. ON CONFLICT DO NOTHING
// is therefore the whole update policy.
func ensureMaterializationRowTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, start time.Time) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO service_materialization
		   (service_id, project_id, materialization_start, materialized_through, era_start)
		 VALUES ($1,$2,$3,$3,$3)
		 ON CONFLICT (service_id) DO NOTHING`,
		serviceID, projectID, start); err != nil {
		return fmt.Errorf("store: start materialization: %w", err)
	}
	return nil
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
	rows, err := tx.Query(ctx,
		`UPDATE service_evaluation_epochs SET state = 'superseded_before_effect'
		  WHERE service_id = $1 AND effective_at = $2 AND state = 'effective'
		  RETURNING id`,
		serviceID, effectiveAt)
	if err != nil {
		return fmt.Errorf("store: supersede epoch at boundary: %w", err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return err
	}
	return cancelRangesOfOrigin(ctx, tx, serviceID, ids)
}

func collectIDs(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan superseded id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// cancelRangesOfOrigin cancels durable work that belonged to rows which never took effect.
//
// Scoping this to the ORIGIN is the whole point. The first implementation cancelled every
// pending or running range starting at or after the boundary, so writing a declaration
// silently discarded an operator's admin recompute, a confirmed maintenance repair and an
// adoption backfill that happened to start there — the exact opposite of the union
// preservation coalescing exists to guarantee, and invisible, because a superseded range
// leaves no complaint behind.
func cancelRangesOfOrigin(ctx context.Context, tx pgx.Tx, serviceID string, originIDs []string) error {
	if len(originIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE service_repair_ranges SET state = 'superseded', updated_at = now()
		  WHERE service_id = $1 AND origin_id = ANY($2) AND state IN ('pending','running')`,
		serviceID, originIDs); err != nil {
		return fmt.Errorf("store: cancel superseded ranges: %w", err)
	}
	return nil
}

// supersedeAtBoundary is the DECLARATION path: a new revision claims the boundary, so any
// revision and any epoch already claiming it yield, and the durable ranges scoped to them
// are cancelled.
func supersedeAtBoundary(ctx context.Context, tx pgx.Tx, serviceID string, effectiveAt time.Time) error {
	rows, err := tx.Query(ctx,
		`UPDATE service_definition_revisions SET state = 'superseded_before_effect'
		  WHERE service_id = $1 AND effective_at = $2 AND state = 'effective'
		  RETURNING id`,
		serviceID, effectiveAt)
	if err != nil {
		return fmt.Errorf("store: supersede revision at boundary: %w", err)
	}
	revisionIDs, err := collectIDs(rows)
	if err != nil {
		return err
	}
	if err := cancelRangesOfOrigin(ctx, tx, serviceID, revisionIDs); err != nil {
		return err
	}
	if err := supersedeEpochsAtBoundary(ctx, tx, serviceID, effectiveAt); err != nil {
		return err
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

// resolveMonitorSlugTx picks the slug a new monitor gets.
//
// An explicit slug is honoured and validated; an absent one is derived from the display
// name by the SAME normalization the backfill migration performs, so a monitor created today
// and one adopted from an old row land on the same shape. Uniqueness can only be answered by
// the database, so the collision loop lives here — and its suffix comes from a counter
// rather than a clock, because two runs of the same input must produce the same slug.
func resolveMonitorSlugTx(ctx context.Context, tx pgx.Tx, m domain.Monitor) (string, error) {
	if m.Slug != "" {
		if !domain.ValidMonitorSlug(m.Slug) {
			return "", fmt.Errorf("store: monitor slug %q must match %s", m.Slug, domain.MonitorSlugPattern())
		}
		return m.Slug, nil
	}
	base := domain.NormalizeMonitorSlug(m.Name)
	candidate := base
	for n := 1; ; n++ {
		var taken bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM monitors WHERE project_id = $1 AND slug = $2)`,
			m.ProjectID, candidate).Scan(&taken); err != nil {
			return "", fmt.Errorf("store: check monitor slug: %w", err)
		}
		if !taken {
			return candidate, nil
		}
		if n > 50 {
			return "", fmt.Errorf("store: could not derive a free slug from %q", m.Name)
		}
		// The suffix must FIT: a base already at the 63-char shape limit (the digit-only
		// name path yields exactly "monitor-" + 55) plus "-1" is 65 characters — an invalid
		// slug this loop never re-validated, so the INSERT died on the shape constraint as
		// a 500. Trim the base, not the suffix: the counter is what keeps candidates unique.
		suffix := fmt.Sprintf("-%d", n)
		trimmed := base
		if maxLen := 63 - len(suffix); len(trimmed) > maxLen {
			trimmed = strings.TrimRight(trimmed[:maxLen], "-")
		}
		candidate = trimmed + suffix
	}
}

// ServiceSummary is one row of the services list.
//
// It carries `SealedThrough` and the two member counts because the LIST is where an operator
// decides which service to look at, and a list that omitted the watermark would let a stalled
// service look identical to a healthy one. Both counts are here for the same reason they are
// declared separately: a service whose context and SLI counts are equal is legitimate, and
// the reader has to be able to see which is which.
type ServiceSummary struct {
	Service   domain.Service
	ManagedBy string

	// Revision is 0 when nothing has been declared — a valid state, not a missing row.
	Revision       int64
	EffectiveAt    *time.Time
	ContextMembers int
	SLIMembers     int
	EpochSeq       int64

	SealedThrough  *time.Time
	RepairingCount int
}

// ListServiceSummaries reads the list rows in ONE query. The obvious alternative — list the
// services and fetch each detail — is an N+1 on the first screen of the feature.
func (s *Store) ListServiceSummaries(ctx context.Context, projectID string) ([]ServiceSummary, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.id, s.project_id, s.slug, s.name, s.description,
		        COALESCE(s.escalation_policy_id::text,''), COALESCE(s.oncall_schedule_id::text,''),
		        s.created_at, s.updated_at,
		        COALESCE(ms.provider_id, ''),
		        COALESCE(r.revision, 0), r.effective_at,
		        COALESCE(cnt.ctx, 0), COALESCE(cnt.sli, 0),
		        COALESCE(e.epoch_seq, 0),
		        m.sealed_through,
		        COALESCE(rp.n, 0)
		   FROM services s
		   LEFT JOIN managed_services ms ON ms.service_id = s.id
		   LEFT JOIN LATERAL (
		       SELECT id, revision, effective_at
		         FROM service_definition_revisions
		        WHERE service_id = s.id AND state = 'effective'
		        ORDER BY effective_at DESC, revision DESC LIMIT 1) r ON true
		   LEFT JOIN LATERAL (
		       SELECT count(*) FILTER (WHERE role = 'context') AS ctx,
		              count(*) FILTER (WHERE role = 'sli')     AS sli
		         FROM service_definition_members WHERE revision_id = r.id) cnt ON true
		   LEFT JOIN LATERAL (
		       SELECT epoch_seq FROM service_evaluation_epochs
		        WHERE service_id = s.id AND state = 'effective'
		        ORDER BY effective_at DESC, epoch_seq DESC LIMIT 1) e ON true
		   LEFT JOIN service_materialization m ON m.service_id = s.id
		   LEFT JOIN LATERAL (
		       SELECT count(*) AS n FROM service_repair_ranges
		        WHERE service_id = s.id AND state IN ('pending','running','error')) rp ON true
		  WHERE s.project_id = $1
		  ORDER BY s.slug`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list service summaries: %w", err)
	}
	defer rows.Close()

	out := []ServiceSummary{}
	for rows.Next() {
		var v ServiceSummary
		if err := rows.Scan(
			&v.Service.ID, &v.Service.ProjectID, &v.Service.Slug, &v.Service.Name, &v.Service.Description,
			&v.Service.EscalationPolicyID, &v.Service.OncallScheduleID,
			&v.Service.CreatedAt, &v.Service.UpdatedAt,
			&v.ManagedBy,
			&v.Revision, &v.EffectiveAt,
			&v.ContextMembers, &v.SLIMembers,
			&v.EpochSeq,
			&v.SealedThrough,
			&v.RepairingCount,
		); err != nil {
			return nil, fmt.Errorf("store: scan service summary: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ServiceDetail is everything the read API needs about one service: what was declared, what
// is being measured, and how far materialization has actually got.
type ServiceDetail struct {
	Service     domain.Service
	Declaration *domain.DefinitionRevision
	Epoch       *domain.EvaluationEpoch
	ManagedBy   string

	MaterializationStart *time.Time
	SealedThrough        *time.Time
	RetractedAt          *time.Time
	RetractedTo          *time.Time
	// Repairing are the ranges whose numbers are not currently trustworthy. They are part of
	// the read surface rather than an internal detail: a range still being computed must
	// read as work in progress, not as missing data.
	Repairing []RepairRange
}

// ServiceDetail reads one service with its declaration, the epoch in force, and the state of
// materialization.
func (s *Store) ServiceDetail(ctx context.Context, projectID, serviceID string) (ServiceDetail, error) {
	var out ServiceDetail
	// ONE snapshot. This screen is assembled from six reads — service, ownership, revision,
	// epoch, materialization, repair queue — and six pool round trips each saw their own
	// instant: a declaration landing between two of them produced a detail whose epoch did
	// not belong to its revision. REPEATABLE READ pins every read to one MVCC snapshot;
	// READ ONLY says what this transaction is for.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ServiceDetail{}, fmt.Errorf("store: begin detail snapshot: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	svc, err := getServiceOn(ctx, tx, projectID, serviceID)
	if err != nil {
		return ServiceDetail{}, err
	}
	out.Service = svc

	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT provider_id FROM managed_services WHERE service_id = $1), '')`,
		serviceID).Scan(&out.ManagedBy); err != nil {
		return ServiceDetail{}, fmt.Errorf("store: read service ownership: %w", err)
	}

	rev := domain.DefinitionRevision{ServiceID: serviceID, ProjectID: projectID}
	var policyJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT id, revision, created_at, effective_at, policies, created_by
		   FROM service_definition_revisions
		  WHERE service_id = $1 AND state = 'effective'
		  ORDER BY effective_at DESC, revision DESC LIMIT 1`, serviceID).
		Scan(&rev.ID, &rev.Revision, &rev.CreatedAt, &rev.EffectiveAt, &policyJSON, &rev.CreatedBy)
	switch {
	case noRows(err):
		// A service with no declaration is a valid state, not an error.
	case err != nil:
		return ServiceDetail{}, fmt.Errorf("store: read declaration: %w", err)
	default:
		if err := json.Unmarshal(policyJSON, &rev.Policies); err != nil {
			return ServiceDetail{}, fmt.Errorf("store: decode policies: %w", err)
		}
		rev.State = domain.RevisionEffective
		if rev.Monitors, rev.SLI, err = revisionMembers(ctx, tx, rev.ID); err != nil {
			return ServiceDetail{}, err
		}
		out.Declaration = &rev
	}

	epoch := domain.EvaluationEpoch{ServiceID: serviceID, ProjectID: projectID}
	var snapshot []byte
	err = tx.QueryRow(ctx,
		`SELECT id, epoch_seq, revision_id, created_at, effective_at, snapshot, snapshot_hash
		   FROM service_evaluation_epochs
		  WHERE service_id = $1 AND state = 'effective'
		  ORDER BY effective_at DESC, epoch_seq DESC LIMIT 1`, serviceID).
		Scan(&epoch.ID, &epoch.Seq, &epoch.RevisionID, &epoch.CreatedAt, &epoch.EffectiveAt, &snapshot, &epoch.SnapshotHash)
	switch {
	case noRows(err):
	case err != nil:
		return ServiceDetail{}, fmt.Errorf("store: read epoch: %w", err)
	default:
		if err := json.Unmarshal(snapshot, &epoch.Members); err != nil {
			return ServiceDetail{}, fmt.Errorf("store: decode epoch snapshot: %w", err)
		}
		epoch.State = domain.RevisionEffective
		out.Epoch = &epoch
	}

	err = tx.QueryRow(ctx,
		`SELECT materialization_start, sealed_through, retracted_at, retracted_to
		   FROM service_materialization WHERE service_id = $1`, serviceID).
		Scan(&out.MaterializationStart, &out.SealedThrough, &out.RetractedAt, &out.RetractedTo)
	if err != nil && !noRows(err) {
		return ServiceDetail{}, fmt.Errorf("store: read materialization: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT range_start, range_end, reason, state, cursor_at, attempts, last_error
		   FROM service_repair_ranges
		  WHERE service_id = $1 AND state IN ('pending','running','error')
		  ORDER BY range_start`, serviceID)
	if err != nil {
		return ServiceDetail{}, fmt.Errorf("store: read repair ranges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rr RepairRange
		var reason string
		var cursor *time.Time
		if err := rows.Scan(&rr.From, &rr.To, &reason, &rr.State, &cursor, &rr.Attempts, &rr.LastError); err != nil {
			return ServiceDetail{}, fmt.Errorf("store: scan repair range: %w", err)
		}
		rr.Reason = RepairReason(reason)
		if cursor != nil {
			rr.Cursor = *cursor
		}
		out.Repairing = append(out.Repairing, rr)
	}
	return out, rows.Err()
}

// revisionMembers reads a revision's two member lists, which are stored separately because
// they are separately declared.
func revisionMembers(ctx context.Context, q queryRower2, revisionID string) (monitors, sli []string, err error) {
	rows, err := q.Query(ctx,
		`SELECT monitor_id, role FROM service_definition_members WHERE revision_id = $1 ORDER BY monitor_id`,
		revisionID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read revision members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, nil, fmt.Errorf("store: scan revision member: %w", err)
		}
		switch domain.MemberRole(role) {
		case domain.RoleContext:
			monitors = append(monitors, id)
		case domain.RoleSLI:
			sli = append(sli, id)
		}
	}
	return monitors, sli, rows.Err()
}

type queryRower2 interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
