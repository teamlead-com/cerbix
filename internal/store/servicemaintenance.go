package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/reliability"
)

// Maintenance as a retroactive declaration (func-service-reliability §10.9).
//
// A maintenance window is a statement about time, and the product lets an operator make it
// about PAST time. That is why reproducibility here is over DECLARED inputs rather than
// absolute: a change to a declared input is a recorded change carrying its own repair, and
// never a silent rewrite — nor a silent no-op, which of the three outcomes is the worst
// because it looks like a completed command.
//
// Removal has two distinct intents and they are different acts:
//
//   - ARCHIVE says "no longer in active inventory", and for an active window "this
//     maintenance ends now". It never rewrites sealed past: the evaluator keeps reading the
//     retained row's effective span regardless of `archived_at`. It needs no raw data, so an
//     old window can always be cleaned up.
//   - ANNUL says "this exclusion was a mistake and the past must be recomputed without it".
//     That is a retroactive repair, and it carries the preview, the audit and the fence.
//
// Creation has only ONE intent, so it is decided purely by range.

var (
	// ErrRetroactiveNeedsPreview is returned when a mutation would touch sealed time without
	// a confirmed preview.
	ErrRetroactiveNeedsPreview = errors.New("store: mutation intersects sealed time and needs a preview")
	// ErrPreviewStale is returned when the world moved between preview and confirm.
	ErrPreviewStale = errors.New("store: preview is stale")
	// ErrPreviewApproximate is returned when a partial preview is confirmed. Applying the
	// part nobody previewed would make "preview" a word rather than a control.
	ErrPreviewApproximate = errors.New("store: an approximate preview cannot be confirmed")
	// ErrUnrecomputableRange is returned when the sealed part of a range can no longer be
	// recomputed because the raw heartbeats behind it are gone.
	ErrUnrecomputableRange = errors.New("store: range is no longer recomputable from raw data")
)

// PreviewExpiry bounds how long a preview token stays confirmable.
const PreviewExpiry = 10 * time.Minute

// previewBinding is the mutation a confirm claims the token authorizes.
type previewBinding struct {
	monitorID string
	// targetID is the window an annul names. Empty for a create.
	targetID string
	mutation MaintenanceMutation
	from, to time.Time
}

// PreviewService is one affected service and the generation the preview saw it at.
type PreviewService struct {
	ServiceID            string
	DefinitionGeneration int64
	// Before and After are the four-way availability split over the requested range, as it
	// stands and as the mutation would leave it. Both axes are carried because a mutation can
	// move health without moving good/bad at all, and a projection showing only the first
	// would report "nothing changes" for a change.
	Before, After ServiceAggregate
	// Projected is false when the range exceeded the projection bound, which makes the whole
	// preview `approximate` and unconfirmable.
	Projected bool
}

// MaintenancePreview is the token a retroactive mutation must carry.
type MaintenancePreview struct {
	ID        string
	ProjectID string
	// MonitorID, TargetID and Mutation are what the token AUTHORIZES. A confirm that does not
	// check them is a token for "some change somewhere", which is not a gate.
	MonitorID string
	// TargetID is the window an annul would remove. Empty for a create — annul is identified
	// by its window, because two windows over the same monitor and range are different
	// mutations with different consequences.
	TargetID              string
	Mutation              MaintenanceMutation
	From, To              time.Time
	MaintenanceGeneration int64
	RawFloor              time.Time
	Coverage              string
	ExpiresAt             time.Time
	Services              []PreviewService
	// EarliestRepairable is echoed back on a fence rejection, so the operator is told when
	// rather than only that.
	EarliestRepairable time.Time
}

// PreviewMaintenanceMutation computes what a retroactive create or annul would change.
//
// rawFloor is the earliest instant still recomputable — the caller resolves it from
// heartbeat retention, because that is a settings question and this package does no
// settings I/O.
// MaintenanceMutation names WHICH change a preview authorizes. A token has to carry it: a
// preview of "annul this window" must not confirm "create a window here".
type MaintenanceMutation string

const (
	MutationCreate MaintenanceMutation = "create"
	MutationAnnul  MaintenanceMutation = "annul"
)

func (s *Store) PreviewMaintenanceMutation(
	ctx context.Context, projectID, monitorID string, from, to, rawFloor time.Time, createdBy string,
) (MaintenancePreview, error) {
	return s.PreviewMutation(ctx, projectID, monitorID, MutationCreate, from, to, rawFloor, createdBy)
}

// PreviewMutation computes what a retroactive mutation would change and issues a token BOUND
// to it: the monitor, the exact range and the kind of change.
func (s *Store) PreviewMutation(
	ctx context.Context, projectID, monitorID string, mutation MaintenanceMutation,
	from, to, rawFloor time.Time, createdBy string,
) (MaintenancePreview, error) {
	return s.PreviewMutationOf(ctx, projectID, monitorID, "", mutation, from, to, rawFloor, createdBy)
}

// PreviewMutationOf is the full form: `targetID` names the window an annul would remove.
//
// Annul is identified by its WINDOW, not by monitor and range. Two windows over the same
// monitor and the same range are different mutations — with both in place, annulling one may
// change nothing while annulling the other changes the number — so a token issued for one
// must not confirm the other.
func (s *Store) PreviewMutationOf(
	ctx context.Context, projectID, monitorID, targetID string, mutation MaintenanceMutation,
	from, to, rawFloor time.Time, createdBy string,
) (MaintenancePreview, error) {
	if !to.After(from) {
		return MaintenancePreview{}, fmt.Errorf("store: preview range end %s is not after start %s", to, from)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MaintenancePreview{}, fmt.Errorf("store: begin preview: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return MaintenancePreview{}, err
	}
	services, err := servicesAffectedByWindow(ctx, tx, projectID, monitorID, from, to)
	if err != nil {
		return MaintenancePreview{}, err
	}

	var generation int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		projectID).Scan(&generation); err != nil {
		return MaintenancePreview{}, fmt.Errorf("store: read maintenance generation: %w", err)
	}

	// What the mutation WOULD do, per affected service, through the same reducer the
	// materializer uses.
	addSpan := (*reliability.MaintenanceSpan)(nil)
	dropID := ""
	switch mutation {
	case MutationCreate:
		addSpan = &reliability.MaintenanceSpan{ID: "preview", MonitorID: monitorID, From: from, To: to}
	case MutationAnnul:
		dropID = targetID
	}
	coverage := "complete"
	for i := range services {
		before, aerr := currentAggregate(ctx, tx, services[i].ServiceID, from, to)
		if aerr != nil {
			return MaintenancePreview{}, aerr
		}
		after, projected, perr := s.projectMutation(ctx, tx, projectID, services[i].ServiceID, from, to, addSpan, dropID)
		if perr != nil {
			return MaintenancePreview{}, perr
		}
		services[i].Before = before
		services[i].After = after
		services[i].Projected = projected
		if !projected {
			// One unprojectable service makes the WHOLE token approximate. A confirm that
			// accepted it would be authorizing a change to a service nobody was shown.
			coverage = "approximate"
		}
	}

	p := MaintenancePreview{
		ProjectID: projectID, MonitorID: monitorID, TargetID: targetID,
		Mutation: mutation, From: from, To: to,
		MaintenanceGeneration: generation, RawFloor: rawFloor,
		Coverage: coverage, Services: services,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO maintenance_previews
		   (project_id, monitor_id, target_id, mutation, requested_start, requested_end,
		    maintenance_generation, raw_floor, coverage, expires_at, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now() + $10::interval, $11)
		 RETURNING id, expires_at`,
		projectID, nullableID(monitorID), nullableID(targetID), string(mutation), from, to,
		generation, rawFloor, p.Coverage, PreviewExpiry.String(), createdBy).
		Scan(&p.ID, &p.ExpiresAt); err != nil {
		return MaintenancePreview{}, fmt.Errorf("store: insert preview: %w", err)
	}

	// The affected set lives in a COMPLETE relation, never a bounded array. Re-reading the
	// generations of services already known proves those rows did not move and proves
	// nothing about the SET: a truncated array would let a confirm pass while a service it
	// never checked is mutated.
	for _, svc := range services {
		if _, err := tx.Exec(ctx,
			`INSERT INTO maintenance_preview_services
			   (preview_id, project_id, service_id, definition_generation,
			    before_good_us, before_bad_us, before_unknown_us, before_excluded_us,
			    after_good_us, after_bad_us, after_unknown_us, after_excluded_us, projected)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			p.ID, projectID, svc.ServiceID, svc.DefinitionGeneration,
			svc.Before.Good, svc.Before.Bad, svc.Before.Unknown, svc.Before.Excluded,
			svc.After.Good, svc.After.Bad, svc.After.Unknown, svc.After.Excluded,
			svc.Projected); err != nil {
			return MaintenancePreview{}, fmt.Errorf("store: insert preview service: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MaintenancePreview{}, fmt.Errorf("store: commit preview: %w", err)
	}
	return p, nil
}

// servicesAffectedByWindow returns every service whose SLI declared the window's monitor at
// any point inside the range, with its current definition generation and the availability it
// currently reports over that range.
//
// A window with an empty monitor id is project-wide and affects every service that has a
// declaration.
func servicesAffectedByWindow(ctx context.Context, tx pgx.Tx, projectID, monitorID string, from, to time.Time) ([]PreviewService, error) {
	var rows pgx.Rows
	var err error
	if monitorID == "" {
		rows, err = tx.Query(ctx,
			`SELECT s.id,
			        COALESCE((SELECT MAX(r.revision) FROM service_definition_revisions r
			                   WHERE r.service_id = s.id AND r.state='effective'), 0),
			        COALESCE((SELECT SUM(b.good_us) FROM service_reliability_buckets b
			                   WHERE b.service_id = s.id AND b.bucket_start >= $2 AND b.bucket_start < $3), 0),
			        COALESCE((SELECT SUM(b.bad_us) FROM service_reliability_buckets b
			                   WHERE b.service_id = s.id AND b.bucket_start >= $2 AND b.bucket_start < $3), 0)
			   FROM services s
			  WHERE s.project_id = $1
			    AND EXISTS (SELECT 1 FROM service_definition_revisions r
			                 WHERE r.service_id = s.id AND r.state='effective')
			  ORDER BY s.id`, projectID, from, to)
	} else {
		rows, err = tx.Query(ctx,
			`SELECT DISTINCT s.id,
			        COALESCE((SELECT MAX(r2.revision) FROM service_definition_revisions r2
			                   WHERE r2.service_id = s.id AND r2.state='effective'), 0),
			        COALESCE((SELECT SUM(b.good_us) FROM service_reliability_buckets b
			                   WHERE b.service_id = s.id AND b.bucket_start >= $3 AND b.bucket_start < $4), 0),
			        COALESCE((SELECT SUM(b.bad_us) FROM service_reliability_buckets b
			                   WHERE b.service_id = s.id AND b.bucket_start >= $3 AND b.bucket_start < $4), 0)
			   FROM services s
			   JOIN service_definition_revisions r ON r.service_id = s.id AND r.state='effective'
			   JOIN service_definition_members m
			     ON m.revision_id = r.id AND m.role='sli' AND m.monitor_id = $2
			  WHERE s.project_id = $1
			  ORDER BY s.id`, projectID, monitorID, from, to)
	}
	if err != nil {
		return nil, fmt.Errorf("store: resolve affected services: %w", err)
	}
	defer rows.Close()
	var out []PreviewService
	for rows.Next() {
		var svc PreviewService
		if err := rows.Scan(&svc.ServiceID, &svc.DefinitionGeneration, &svc.Before.Good, &svc.Before.Bad); err != nil {
			return nil, fmt.Errorf("store: scan affected service: %w", err)
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// rangeIntersectsSealed reports whether any affected service has a SEALED fact in the range.
// Nothing sealed means nothing to repair, and the mutation is an ordinary prospective one.
func rangeIntersectsSealed(ctx context.Context, tx pgx.Tx, services []PreviewService, from, to time.Time) (bool, error) {
	if len(services) == 0 {
		return false, nil
	}
	ids := make([]string, 0, len(services))
	for _, svc := range services {
		ids = append(ids, svc.ServiceID)
	}
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets
		  WHERE service_id = ANY($1) AND state='sealed' AND bucket_start >= $2 AND bucket_start < $3`,
		ids, from, to).Scan(&n); err != nil {
		return false, fmt.Errorf("store: check sealed intersection: %w", err)
	}
	return n > 0, nil
}

// CreateMaintenanceWindowChecked creates a window, deciding by RANGE whether it is ordinary.
//
// previewID may be empty for a window that touches no sealed time. When the range does
// intersect sealed facts the mutation is a retroactive repair by definition — whether it is
// a create or an annul — and it requires a confirmed preview.
func (s *Store) CreateMaintenanceWindowChecked(
	ctx context.Context, w domain.MaintenanceWindow, previewID string, rawFloor time.Time,
) (domain.MaintenanceWindow, error) {
	if err := w.Validate(); err != nil {
		return domain.MaintenanceWindow{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.MaintenanceWindow{}, fmt.Errorf("store: begin create window: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceMembership(ctx, tx, w.ProjectID); err != nil {
		return domain.MaintenanceWindow{}, err
	}
	services, err := servicesAffectedByWindow(ctx, tx, w.ProjectID, w.MonitorID, w.StartsAt, w.EndsAt)
	if err != nil {
		return domain.MaintenanceWindow{}, err
	}
	sealed, err := rangeIntersectsSealed(ctx, tx, services, w.StartsAt, w.EndsAt)
	if err != nil {
		return domain.MaintenanceWindow{}, err
	}
	if sealed {
		if previewID == "" {
			return domain.MaintenanceWindow{}, ErrRetroactiveNeedsPreview
		}
		binding := previewBinding{monitorID: w.MonitorID, mutation: MutationCreate, from: w.StartsAt, to: w.EndsAt}
		actor, cerr := confirmPreviewTx(ctx, tx, w.ProjectID, previewID, services, binding, w.StartsAt, rawFloor)
		if cerr != nil {
			return domain.MaintenanceWindow{}, cerr
		}
		// The MUTATION itself is audited, separately from the bucket restatements it causes.
		// A reader looking for "who authorized rewriting these numbers, and under which
		// token" cannot answer it from per-bucket rows: those say what changed, not who
		// decided it should.
		if err := recordMutationAuditTx(ctx, tx, w.ProjectID, MutationCreate, w.MonitorID, "",
			previewID, actor, w.StartsAt, w.EndsAt, services); err != nil {
			return domain.MaintenanceWindow{}, err
		}
	}

	var out domain.MaintenanceWindow
	if err := tx.QueryRow(ctx,
		`INSERT INTO maintenance_windows (project_id, monitor_id, starts_at, ends_at, reason)
		 VALUES ($1,$2,$3,$4,$5)
		 RETURNING id, project_id, COALESCE(monitor_id::text,''), starts_at, ends_at, reason, created_at`,
		w.ProjectID, nullableID(w.MonitorID), w.StartsAt, w.EndsAt, w.Reason).
		Scan(&out.ID, &out.ProjectID, &out.MonitorID, &out.StartsAt, &out.EndsAt, &out.Reason, &out.CreatedAt); err != nil {
		return domain.MaintenanceWindow{}, fmt.Errorf("store: insert maintenance window: %w", err)
	}

	if err := invalidateForMaintenance(ctx, tx, w.ProjectID, services, w.StartsAt, w.EndsAt); err != nil {
		return domain.MaintenanceWindow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.MaintenanceWindow{}, fmt.Errorf("store: commit create window: %w", err)
	}
	return out, nil
}

// ArchiveMaintenanceWindow removes a window from active inventory WITHOUT touching sealed
// past, and cuts an active one short at the exact statement time.
//
// It never needs raw data, so an old window can always be cleaned up — which is precisely
// the operation the alternative design made impossible forever.
func (s *Store) ArchiveMaintenanceWindow(ctx context.Context, projectID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE maintenance_windows
		    SET archived_at = statement_timestamp(),
		        -- Only an ACTIVE window is cut short, and at the exact instant: the reducer
		        -- handles arbitrary maintenance edges, so rounding to a bucket boundary would
		        -- silently extend or shorten a real exclusion by up to a whole bucket.
		        cancel_effective_at = CASE
		            WHEN starts_at <= statement_timestamp() AND ends_at > statement_timestamp()
		            THEN statement_timestamp() ELSE cancel_effective_at END
		  WHERE id = $1 AND project_id = $2 AND archived_at IS NULL`, id, projectID)
	if err != nil {
		return fmt.Errorf("store: archive maintenance window: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AnnulMaintenanceWindow removes a window's effect from the evaluator's input and repairs
// the range it covered.
//
// This is the privileged act that says the exclusion was a mistake, so it carries the
// preview, the audit and the raw fence — and it is the ONLY thing that takes a span out of
// the evaluator's input.
func (s *Store) AnnulMaintenanceWindow(ctx context.Context, projectID, id, previewID string, rawFloor time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin annul: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return err
	}
	var monitorID string
	var from, to time.Time
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(monitor_id::text,''), starts_at, LEAST(ends_at, COALESCE(cancel_effective_at, ends_at))
		   FROM maintenance_windows WHERE id=$1 AND project_id=$2 FOR UPDATE`,
		id, projectID).Scan(&monitorID, &from, &to); err != nil {
		if noRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: read window for annul: %w", err)
	}

	services, err := servicesAffectedByWindow(ctx, tx, projectID, monitorID, from, to)
	if err != nil {
		return err
	}
	sealed, err := rangeIntersectsSealed(ctx, tx, services, from, to)
	if err != nil {
		return err
	}
	if sealed {
		if previewID == "" {
			return ErrRetroactiveNeedsPreview
		}
		binding := previewBinding{monitorID: monitorID, targetID: id, mutation: MutationAnnul, from: from, to: to}
		actor, cerr := confirmPreviewTx(ctx, tx, projectID, previewID, services, binding, from, rawFloor)
		if cerr != nil {
			return cerr
		}
		if err := recordMutationAuditTx(ctx, tx, projectID, MutationAnnul, monitorID, id,
			previewID, actor, from, to, services); err != nil {
			return err
		}
	}

	// Annul is a HARD removal from the evaluator's input: the row goes. Archive keeps it,
	// which is what makes archive safe for sealed history and annul the operation that needs
	// a fence.
	if _, err := tx.Exec(ctx, `DELETE FROM maintenance_windows WHERE id=$1 AND project_id=$2`, id, projectID); err != nil {
		return fmt.Errorf("store: annul maintenance window: %w", err)
	}
	if err := invalidateForMaintenance(ctx, tx, projectID, services, from, to); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// confirmPreviewTx is the compare-and-swap.
//
// It runs INSIDE the mutating transaction, while the project's `service_membership` advisory
// lock is held — the same lock every set-changing path takes. Re-resolving the set before
// the lock would be a check-then-act one level down, and row locks cannot help: they protect
// rows that exist, and a service created concurrently is exactly what has no row yet.
func confirmPreviewTx(
	ctx context.Context, tx pgx.Tx, projectID, previewID string, current []PreviewService,
	want previewBinding, sealedStart, rawFloor time.Time,
) (actor string, err error) {
	var storedGeneration int64
	var storedRawFloor, expiresAt time.Time
	var coverage string
	var storedMonitor, storedTarget *string
	var storedMutation string
	var storedFrom, storedTo time.Time
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(created_by,''), maintenance_generation, raw_floor, coverage, expires_at,
		        monitor_id::text, target_id::text, mutation, requested_start, requested_end
		   FROM maintenance_previews WHERE id=$1 AND project_id=$2 FOR UPDATE`,
		previewID, projectID).Scan(&actor, &storedGeneration, &storedRawFloor, &coverage, &expiresAt,
		&storedMonitor, &storedTarget, &storedMutation, &storedFrom, &storedTo); err != nil {
		if noRows(err) {
			return actor, ErrPreviewStale
		}
		return actor, fmt.Errorf("store: read preview: %w", err)
	}
	if coverage != "complete" {
		return actor, ErrPreviewApproximate
	}
	// Expiry is decided by the DATABASE clock, like every other instant in this protocol.
	// Comparing a process clock against a stored timestamp lets replica skew accept a token
	// that has expired or reject one that has not.
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp() > $1`, expiresAt).Scan(&expired); err != nil {
		return actor, fmt.Errorf("store: check preview expiry: %w", err)
	}
	if expired {
		return actor, ErrPreviewStale
	}

	// The token authorizes ONE mutation. Without this the confirm checked that the world had
	// not moved and never checked what it was being asked to do, so a token issued for a
	// two-minute window on one monitor authorized a twelve-hour window on another as long as
	// both touched the same services.
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if storedMutation != string(want.mutation) ||
		deref(storedMonitor) != want.monitorID ||
		deref(storedTarget) != want.targetID ||
		!storedFrom.Equal(want.from) || !storedTo.Equal(want.to) {
		return actor, ErrPreviewStale
	}
	// The floor recorded WHEN THE PREVIEW RAN also has to still hold: retention only moves
	// forward, so a token issued against an older floor cannot authorize a range that has
	// since fallen out of raw.
	if sealedStart.Before(storedRawFloor) {
		return actor, fmt.Errorf("%w: earliest repairable is %s", ErrUnrecomputableRange, storedRawFloor.UTC().Format(time.RFC3339))
	}

	var generation int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		projectID).Scan(&generation); err != nil {
		return actor, fmt.Errorf("store: read maintenance generation: %w", err)
	}
	if generation != storedGeneration {
		return actor, ErrPreviewStale
	}

	// raw_floor is a MONOTONIC predicate, not an equality: the floor advances continuously
	// with retention, so byte equality would make every token stale by construction. The
	// question is whether the range is still repairable, not whether the clock stood still.
	if sealedStart.Before(rawFloor) {
		return actor, fmt.Errorf("%w: earliest repairable is %s", ErrUnrecomputableRange, rawFloor.UTC().Format(time.RFC3339))
	}

	// Exact SET equality, over the complete relation.
	stored := map[string]int64{}
	rows, err := tx.Query(ctx,
		`SELECT service_id, definition_generation FROM maintenance_preview_services WHERE preview_id=$1`, previewID)
	if err != nil {
		return actor, fmt.Errorf("store: read preview services: %w", err)
	}
	for rows.Next() {
		var id string
		var gen int64
		if err := rows.Scan(&id, &gen); err != nil {
			rows.Close()
			return actor, fmt.Errorf("store: scan preview service: %w", err)
		}
		stored[id] = gen
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return actor, fmt.Errorf("store: read preview services: %w", err)
	}
	if len(stored) != len(current) {
		return actor, fmt.Errorf("%w: the affected set changed size (%d previewed, %d now)", ErrPreviewStale, len(stored), len(current))
	}
	for _, svc := range current {
		gen, ok := stored[svc.ServiceID]
		if !ok {
			return actor, fmt.Errorf("%w: service %s was not in the preview", ErrPreviewStale, svc.ServiceID)
		}
		if gen != svc.DefinitionGeneration {
			return actor, fmt.Errorf("%w: service %s moved from revision %d to %d", ErrPreviewStale, svc.ServiceID, gen, svc.DefinitionGeneration)
		}
	}

	// A confirmed token is spent.
	if _, err := tx.Exec(ctx, `DELETE FROM maintenance_previews WHERE id=$1`, previewID); err != nil {
		return actor, fmt.Errorf("store: consume preview: %w", err)
	}
	return actor, nil
}

// invalidateForMaintenance does, in the SAME transaction as the mutation, everything that
// stops the API from confidently serving a number computed under a declaration that no
// longer exists.
//
// Enqueueing a repair is not enough on its own: a pending job leaves minutes or hours in
// which the old number is still served. The watermark rewind is what makes the affected
// range read as incomplete immediately.
func invalidateForMaintenance(ctx context.Context, tx pgx.Tx, projectID string, services []PreviewService, from, to time.Time) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO project_maintenance_generation (project_id, generation, updated_at)
		 VALUES ($1, 1, now())
		 ON CONFLICT (project_id) DO UPDATE
		    SET generation = project_maintenance_generation.generation + 1, updated_at = now()`,
		projectID); err != nil {
		return fmt.Errorf("store: bump maintenance generation: %w", err)
	}

	rangeStart := domain.FloorToBucket(from)
	rangeEnd := domain.CeilToBucket(to)
	for _, svc := range services {
		if _, err := tx.Exec(ctx,
			`INSERT INTO service_repair_ranges
			   (service_id, project_id, range_start, range_end, reason, maintenance_generation, cursor_at)
			 VALUES ($1,$2,$3,$4,'maintenance',
			         COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$2), 0), $3)`,
			svc.ServiceID, projectID, rangeStart, rangeEnd); err != nil {
			return fmt.Errorf("store: enqueue maintenance repair: %w", err)
		}
		// Rewind the watermark to the earliest affected bucket and RECORD the retraction: a
		// window that silently shortened is indistinguishable from a bug.
		if _, err := tx.Exec(ctx,
			`UPDATE service_materialization
			    SET sealed_through = LEAST(COALESCE(sealed_through, $2), $2),
			        retracted_at = now(), retracted_to = $2
			  WHERE service_id = $1 AND (sealed_through IS NULL OR sealed_through > $2)`,
			svc.ServiceID, rangeStart); err != nil {
			return fmt.Errorf("store: rewind watermark: %w", err)
		}
	}
	return nil
}

// recordMutationAuditTx records the retroactive mutation an operator authorized: which kind,
// which monitor and window, under which preview token, over which range, and how many services
// it restates.
//
// The per-bucket recompute rows say WHAT changed. This says who decided it should, and on the
// strength of which token — the two questions an audit of a history rewrite has to answer, and
// neither can be reconstructed from the other.
func recordMutationAuditTx(
	ctx context.Context, tx pgx.Tx, projectID string, mutation MaintenanceMutation,
	monitorID, windowID, previewID, createdBy string, from, to time.Time, services []PreviewService,
) error {
	target := fmt.Sprintf("mutation=%s monitor=%s window=%s preview=%s by=%s range=[%s,%s) services=%d",
		mutation, orDash(monitorID), orDash(windowID), previewID, orDash(createdBy),
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), len(services))
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 SELECT p.org_id, NULL, false, 'service.maintenance_mutated', $2
		   FROM projects p WHERE p.id = $1`,
		projectID, target); err != nil {
		return fmt.Errorf("store: audit maintenance mutation: %w", err)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ── The projection ──────────────────────────────────────────────────────────────────────
//
// A preview that shows only the CURRENT numbers is not a preview: the operator is being asked
// to authorize a change to sealed figures and is shown what they already are. This computes
// what they WOULD become, by running the same reducer the materializer runs, over the same
// range, with the mutation applied — and writing nothing.

// previewProjectionBudget bounds how many buckets one preview will re-reduce. Past it the
// preview is honestly `approximate` and a confirm refuses it: a change nobody could be shown
// is not one an operator can be said to have approved.
const previewProjectionBudget = 4320 // three days of canonical buckets

// projectMutation returns the aggregate a service would report over [from, to) once the
// mutation is applied. `addSpan` is the window a create would introduce; `dropWindowID` is the
// window an annul would remove. Exactly one is set.
func (s *Store) projectMutation(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to time.Time,
	addSpan *reliability.MaintenanceSpan, dropWindowID string,
) (agg ServiceAggregate, projected bool, err error) {
	if to.Sub(from)/domain.CanonicalBucket > previewProjectionBudget {
		return ServiceAggregate{}, false, nil
	}
	for at := domain.FloorToBucket(from); at.Before(to); at = at.Add(domain.CanonicalBucket) {
		end := at.Add(domain.CanonicalBucket)
		epochID, members, policies, eerr := epochAt(ctx, tx, serviceID, at)
		if eerr != nil {
			return ServiceAggregate{}, false, eerr
		}
		if epochID == "" || len(members) == 0 {
			continue
		}
		observations, oerr := observationsFor(ctx, tx, members, at, end)
		if oerr != nil {
			return ServiceAggregate{}, false, oerr
		}
		spans, serr := maintenanceSpansFor(ctx, tx, projectID, members, at, end)
		if serr != nil {
			return ServiceAggregate{}, false, serr
		}
		// Apply the hypothetical. A create ADDS its span; an annul DROPS the one it names.
		// Both go through the same reducer as the real thing, so the projection cannot drift
		// from what the confirm would actually produce.
		if dropWindowID != "" {
			kept := spans[:0]
			for _, sp := range spans {
				if sp.ID != dropWindowID {
					kept = append(kept, sp)
				}
			}
			spans = kept
		}
		if addSpan != nil && addSpan.To.After(at) && addSpan.From.Before(end) {
			spans = append(spans, *addSpan)
		}
		b, rerr := reliability.Reduce(reliability.Input{
			Start: at, End: end, Members: members,
			Observations: observations, Maintenance: spans, Policies: policies,
		})
		if rerr != nil {
			return ServiceAggregate{}, false, fmt.Errorf("store: project bucket %s: %w", at, rerr)
		}
		agg.Good += b.Durations.Good.Microseconds()
		agg.Bad += b.Durations.Bad.Microseconds()
		agg.Unknown += b.Durations.Unknown.Microseconds()
		agg.Excluded += b.Durations.Excluded.Microseconds()
	}
	return agg, true, nil
}

// ServiceAggregate is the four-way availability split over a range, in microseconds. It is
// what a preview shows on both sides of a mutation.
type ServiceAggregate struct{ Good, Bad, Unknown, Excluded int64 }

// currentAggregate sums what the service ALREADY reports over the range, straight from the
// facts — the "before" half of the projection.
func currentAggregate(ctx context.Context, tx pgx.Tx, serviceID string, from, to time.Time) (ServiceAggregate, error) {
	var a ServiceAggregate
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(good_us),0), COALESCE(SUM(bad_us),0),
		        COALESCE(SUM(unknown_us),0), COALESCE(SUM(excluded_us),0)
		   FROM service_reliability_buckets
		  WHERE service_id = $1 AND bucket_start >= $2 AND bucket_start < $3`,
		serviceID, from, to).Scan(&a.Good, &a.Bad, &a.Unknown, &a.Excluded); err != nil {
		return ServiceAggregate{}, fmt.Errorf("store: read current aggregate: %w", err)
	}
	return a, nil
}
