package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
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

// PreviewService is one affected service and the generation the preview saw it at.
type PreviewService struct {
	ServiceID            string
	DefinitionGeneration int64
	BeforeGood           int64
	BeforeBad            int64
}

// MaintenancePreview is the token a retroactive mutation must carry.
type MaintenancePreview struct {
	ID                    string
	ProjectID             string
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
func (s *Store) PreviewMaintenanceMutation(
	ctx context.Context, projectID, monitorID string, from, to, rawFloor time.Time, createdBy string,
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

	p := MaintenancePreview{
		ProjectID: projectID, From: from, To: to,
		MaintenanceGeneration: generation, RawFloor: rawFloor,
		Coverage: "complete", Services: services,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO maintenance_previews
		   (project_id, requested_start, requested_end, maintenance_generation, raw_floor, coverage, expires_at, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6, now() + $7::interval, $8)
		 RETURNING id, expires_at`,
		projectID, from, to, generation, rawFloor, p.Coverage, PreviewExpiry.String(), createdBy).
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
			   (preview_id, project_id, service_id, definition_generation, before_good_us, before_bad_us)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			p.ID, projectID, svc.ServiceID, svc.DefinitionGeneration, svc.BeforeGood, svc.BeforeBad); err != nil {
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
		if err := rows.Scan(&svc.ServiceID, &svc.DefinitionGeneration, &svc.BeforeGood, &svc.BeforeBad); err != nil {
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
		if err := confirmPreviewTx(ctx, tx, w.ProjectID, previewID, services, w.StartsAt, rawFloor); err != nil {
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
		if err := confirmPreviewTx(ctx, tx, projectID, previewID, services, from, rawFloor); err != nil {
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
func confirmPreviewTx(ctx context.Context, tx pgx.Tx, projectID, previewID string, current []PreviewService, sealedStart, rawFloor time.Time) error {
	var storedGeneration int64
	var storedRawFloor, expiresAt time.Time
	var coverage string
	if err := tx.QueryRow(ctx,
		`SELECT maintenance_generation, raw_floor, coverage, expires_at
		   FROM maintenance_previews WHERE id=$1 AND project_id=$2 FOR UPDATE`,
		previewID, projectID).Scan(&storedGeneration, &storedRawFloor, &coverage, &expiresAt); err != nil {
		if noRows(err) {
			return ErrPreviewStale
		}
		return fmt.Errorf("store: read preview: %w", err)
	}
	if coverage != "complete" {
		return ErrPreviewApproximate
	}
	if time.Now().After(expiresAt) {
		return ErrPreviewStale
	}

	var generation int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		projectID).Scan(&generation); err != nil {
		return fmt.Errorf("store: read maintenance generation: %w", err)
	}
	if generation != storedGeneration {
		return ErrPreviewStale
	}

	// raw_floor is a MONOTONIC predicate, not an equality: the floor advances continuously
	// with retention, so byte equality would make every token stale by construction. The
	// question is whether the range is still repairable, not whether the clock stood still.
	if sealedStart.Before(rawFloor) {
		return fmt.Errorf("%w: earliest repairable is %s", ErrUnrecomputableRange, rawFloor.UTC().Format(time.RFC3339))
	}

	// Exact SET equality, over the complete relation.
	stored := map[string]int64{}
	rows, err := tx.Query(ctx,
		`SELECT service_id, definition_generation FROM maintenance_preview_services WHERE preview_id=$1`, previewID)
	if err != nil {
		return fmt.Errorf("store: read preview services: %w", err)
	}
	for rows.Next() {
		var id string
		var gen int64
		if err := rows.Scan(&id, &gen); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan preview service: %w", err)
		}
		stored[id] = gen
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read preview services: %w", err)
	}
	if len(stored) != len(current) {
		return fmt.Errorf("%w: the affected set changed size (%d previewed, %d now)", ErrPreviewStale, len(stored), len(current))
	}
	for _, svc := range current {
		gen, ok := stored[svc.ServiceID]
		if !ok {
			return fmt.Errorf("%w: service %s was not in the preview", ErrPreviewStale, svc.ServiceID)
		}
		if gen != svc.DefinitionGeneration {
			return fmt.Errorf("%w: service %s moved from revision %d to %d", ErrPreviewStale, svc.ServiceID, gen, svc.DefinitionGeneration)
		}
	}

	// A confirmed token is spent.
	if _, err := tx.Exec(ctx, `DELETE FROM maintenance_previews WHERE id=$1`, previewID); err != nil {
		return fmt.Errorf("store: consume preview: %w", err)
	}
	return nil
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
