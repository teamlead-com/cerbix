package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// isStatementBudgetError reports whether an error is the server refusing to spend more time:
// statement_timeout (57014) or lock_timeout (55P03). These are the budget speaking, not the
// data — the projection degrades instead of failing.
func isStatementBudgetError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "57014" || pgErr.Code == "55P03"
}

// resolveRawFloorTx computes the earliest recomputable instant on the DATABASE clock. A
// non-positive retention means "no raw purge is configured", which resolves to the epoch —
// everything retained is repairable.
func resolveRawFloorTx(ctx context.Context, tx pgx.Tx, retention time.Duration) (time.Time, error) {
	if retention <= 0 {
		return time.Unix(0, 0).UTC(), nil
	}
	var floor time.Time
	if err := tx.QueryRow(ctx,
		`SELECT statement_timestamp() - ($1 * interval '1 microsecond')`,
		retention.Microseconds()).Scan(&floor); err != nil {
		return time.Time{}, fmt.Errorf("store: resolve raw floor: %w", err)
	}
	return floor, nil
}

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
	// Reason says WHY a service was not projected: range_too_long, wall_budget or
	// evidence_gone. Three different remediations — narrow the range, try again, and "this
	// can never run" — and a bare boolean made the operator guess between them.
	Reason string
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
// rawRetention is how long raw heartbeats are kept; the earliest recomputable instant (the
// "raw floor") is resolved from it ON THE DATABASE CLOCK, inside the same transaction as the
// check it gates. Passing a process-computed floor here let replica clock skew accept a range
// whose raw evidence was already purged, or reject one that was still whole — every other
// instant in this protocol is the database's, and this one is no exception.
// MaintenanceMutation names WHICH change a preview authorizes. A token has to carry it: a
// preview of "annul this window" must not confirm "create a window here".
type MaintenanceMutation string

const (
	MutationCreate MaintenanceMutation = "create"
	MutationAnnul  MaintenanceMutation = "annul"
)

func (s *Store) PreviewMaintenanceMutation(
	ctx context.Context, projectID, monitorID string, from, to time.Time, rawRetention time.Duration, createdBy string,
) (MaintenancePreview, error) {
	return s.PreviewMutation(ctx, projectID, monitorID, MutationCreate, from, to, rawRetention, createdBy)
}

// PreviewMutation computes what a retroactive mutation would change and issues a token BOUND
// to it: the monitor, the exact range and the kind of change.
func (s *Store) PreviewMutation(
	ctx context.Context, projectID, monitorID string, mutation MaintenanceMutation,
	from, to time.Time, rawRetention time.Duration, createdBy string,
) (MaintenancePreview, error) {
	return s.PreviewMutationOf(ctx, projectID, monitorID, "", mutation, from, to, rawRetention, createdBy)
}

// PreviewMutationOf is the full form: `targetID` names the window an annul would remove.
//
// Annul is identified by its WINDOW, not by monitor and range. Two windows over the same
// monitor and the same range are different mutations — with both in place, annulling one may
// change nothing while annulling the other changes the number — so a token issued for one
// must not confirm the other.
func (s *Store) PreviewMutationOf(
	ctx context.Context, projectID, monitorID, targetID string, mutation MaintenanceMutation,
	from, to time.Time, rawRetention time.Duration, createdBy string,
) (MaintenancePreview, error) {
	if !to.After(from) {
		return MaintenancePreview{}, fmt.Errorf("store: preview range end %s is not after start %s", to, from)
	}
	// The projection runs UNDER the project's membership advisory lock — it has to, or the
	// affected set could move between projection and token. That makes its runtime the
	// project's runtime, so ONE caller-side deadline covers the whole operation: lock,
	// resolve, gate, project, persist, commit. deadlineTx checks the remainder before every
	// statement and re-derives the server bounds as it shrinks; the persistence of the token
	// itself runs inside a RESERVE the work phases may not touch, so an exhausted budget
	// still writes down the approximate answer instead of dying while trying to say it.
	deadline := time.Now().Add(s.previewWallBudget())
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(schedulingTolerance))
	defer cancel()

	rawTx, err := s.pool.Begin(ctx)
	if err != nil {
		return MaintenancePreview{}, fmt.Errorf("store: begin preview: %w", err)
	}
	defer rawTx.Rollback(ctx) //nolint:errcheck // no-op after commit
	tx := newDeadlineTx(rawTx, deadline, previewPersistReserve)
	workDeadline := deadline.Add(-previewPersistReserve)

	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return MaintenancePreview{}, err
	}
	rawFloor, err := resolveRawFloorTx(ctx, tx, rawRetention)
	if err != nil {
		return MaintenancePreview{}, err
	}
	services, err := servicesAffectedByWindow(ctx, tx, projectID, monitorID, from, to)
	if err != nil {
		return MaintenancePreview{}, err
	}

	// Retention fails closed AT THE PREVIEW, not only at the confirm. A range whose raw
	// evidence is already purged cannot be recomputed; projecting it anyway produced a
	// confident "complete after" for a change the confirm was always going to 422 — the
	// diagnostic surface promising what the system knew it would refuse.
	if len(services) > 0 && from.Before(rawFloor) {
		if sealed, serr := rangeIntersectsSealed(ctx, tx, services, from, to); serr != nil {
			return MaintenancePreview{}, serr
		} else if sealed {
			return MaintenancePreview{}, fmt.Errorf("%w: earliest repairable is %s",
				ErrUnrecomputableRange, rawFloor.UTC().Format(time.RFC3339))
		}
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
		// The wall check runs before every service, against the WORK deadline — the whole
		// budget minus the persistence reserve. Past it, remaining services are skipped
		// without a single statement: per-statement bounds alone cannot stop accumulation,
		// only refusing to start can.
		if !time.Now().Before(workDeadline) {
			services[i].Reason = ReasonWallBudget
			coverage = "approximate"
			continue
		}
		// Each projection runs in a SAVEPOINT: a statement bound fired mid-projection
		// aborts the savepoint, not the transaction, so exhaustion still delivers the
		// promised `approximate` token instead of a rolled-back 500. The adapter re-derives
		// the bounds inside the savepoint and the parent invalidates its own view when the
		// child finishes — SET LOCAL issued inside a savepoint SURVIVES its RELEASE.
		sp, serr := tx.Begin(ctx)
		if serr != nil {
			return MaintenancePreview{}, fmt.Errorf("store: begin projection savepoint: %w", serr)
		}
		before, after, reason, perr := s.projectBothSides(ctx, sp, projectID, services[i].ServiceID, from, to, addSpan, dropID, workDeadline)
		switch {
		case perr != nil && (isStatementBudgetError(perr) || errors.Is(perr, errSliceBudget)):
			if rerr := sp.Rollback(ctx); rerr != nil {
				return MaintenancePreview{}, fmt.Errorf("store: rollback projection savepoint: %w", rerr)
			}
			services[i].Reason = ReasonWallBudget
			coverage = "approximate"
			continue
		case perr != nil:
			return MaintenancePreview{}, perr
		}
		if cerr := sp.Commit(ctx); cerr != nil {
			return MaintenancePreview{}, fmt.Errorf("store: release projection savepoint: %w", cerr)
		}
		services[i].Before = before
		services[i].After = after
		services[i].Projected = reason == ""
		services[i].Reason = reason
		if reason != "" {
			// One unprojectable service makes the WHOLE token approximate. A confirm that
			// accepted it would be authorizing a change to a service nobody was shown.
			coverage = "approximate"
		}
	}

	// PERSISTENCE runs through the persist envelope: the reserve drops to zero — this is the
	// time the work phases were forbidden to touch — but every statement AND the commit stay
	// behind per-statement, remainder-derived bounds. Two review rounds in a row re-created
	// accumulation by switching back to the raw transaction here with one fixed SET:
	// statement_timeout restarts per statement, so a fixed bound across N statements bounds
	// none of their sum. The N itself is also gone — the affected services land in ONE
	// unnest insert, so the whole phase is two statements and a commit.
	persist := tx.persistPhase()

	p := MaintenancePreview{
		ProjectID: projectID, MonitorID: monitorID, TargetID: targetID,
		Mutation: mutation, From: from, To: to,
		MaintenanceGeneration: generation, RawFloor: rawFloor,
		EarliestRepairable: rawFloor,
		Coverage:           coverage, Services: services,
	}
	if err := persist.QueryRow(ctx,
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
	// never checked is mutated. One statement for all rows, on purpose — the persistence
	// phase's cost must not scale with the affected count.
	n := len(services)
	ids := make([]string, n)
	gens := make([]int64, n)
	bg, bb, bu, bx := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	ag, ab, au, ax := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	bh, bd, bdn, bhu := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	ah, ad, adn, ahu := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	projected := make([]bool, n)
	for i, svc := range services {
		ids[i], gens[i], projected[i] = svc.ServiceID, svc.DefinitionGeneration, svc.Projected
		bg[i], bb[i], bu[i], bx[i] = svc.Before.Good, svc.Before.Bad, svc.Before.Unknown, svc.Before.Excluded
		ag[i], ab[i], au[i], ax[i] = svc.After.Good, svc.After.Bad, svc.After.Unknown, svc.After.Excluded
		bh[i], bd[i], bdn[i], bhu[i] = svc.Before.Healthy, svc.Before.Degraded, svc.Before.Down, svc.Before.HealthUnknown
		ah[i], ad[i], adn[i], ahu[i] = svc.After.Healthy, svc.After.Degraded, svc.After.Down, svc.After.HealthUnknown
	}
	if n > 0 {
		if _, err := persist.Exec(ctx,
			`INSERT INTO maintenance_preview_services
			   (preview_id, project_id, service_id, definition_generation,
			    before_good_us, before_bad_us, before_unknown_us, before_excluded_us,
			    after_good_us, after_bad_us, after_unknown_us, after_excluded_us,
			    before_healthy_us, before_degraded_us, before_down_us, before_health_unknown_us,
			    after_healthy_us, after_degraded_us, after_down_us, after_health_unknown_us,
			    projected)
			 SELECT $1, $2, u.*
			   FROM unnest($3::uuid[], $4::bigint[],
			               $5::bigint[], $6::bigint[], $7::bigint[], $8::bigint[],
			               $9::bigint[], $10::bigint[], $11::bigint[], $12::bigint[],
			               $13::bigint[], $14::bigint[], $15::bigint[], $16::bigint[],
			               $17::bigint[], $18::bigint[], $19::bigint[], $20::bigint[],
			               $21::boolean[]) AS u`,
			p.ID, projectID, ids, gens,
			bg, bb, bu, bx, ag, ab, au, ax,
			bh, bd, bdn, bhu, ah, ad, adn, ahu,
			projected); err != nil {
			return MaintenancePreview{}, fmt.Errorf("store: insert preview services: %w", err)
		}
	}
	if err := persist.Commit(ctx); err != nil {
		return MaintenancePreview{}, fmt.Errorf("store: commit preview: %w", err)
	}
	return p, nil
}

// servicesAffectedByWindow returns every service whose declaration DECLARED the window's
// monitor as a reliability input at some instant inside [from, to), with its current
// definition generation.
//
// "At some instant inside the range" is a validity-interval question: a revision governs
// [effective_at, next revision's effective_at), and only revisions whose validity OVERLAPS
// the requested range count. The first implementation joined any effective revision the
// service ever had, so a service that used the monitor solely OUTSIDE the range entered the
// affected set — inflating previews, going approximate or evidence_gone for time the
// mutation does not touch, and queueing repair work with nothing to repair.
//
// A window with an empty monitor id is project-wide and affects every service that had any
// declaration in force inside the range.
func servicesAffectedByWindow(ctx context.Context, tx pgx.Tx, projectID, monitorID string, from, to time.Time) ([]PreviewService, error) {
	var rows pgx.Rows
	var err error
	const revisionValidity = `
		revs AS (
		    SELECT r.id, r.service_id, r.effective_at,
		           LEAD(r.effective_at) OVER (PARTITION BY r.service_id ORDER BY r.effective_at, r.revision) AS next_at
		      FROM service_definition_revisions r
		     WHERE r.project_id = $1 AND r.state = 'effective'
		)`
	if monitorID == "" {
		rows, err = tx.Query(ctx,
			`WITH `+revisionValidity+`
			 SELECT DISTINCT s.id,
			        COALESCE((SELECT MAX(r2.revision) FROM service_definition_revisions r2
			                   WHERE r2.service_id = s.id AND r2.state='effective'), 0)
			   FROM services s
			   JOIN revs rv ON rv.service_id = s.id
			        AND rv.effective_at < $3 AND COALESCE(rv.next_at, 'infinity'::timestamptz) > $2
			  WHERE s.project_id = $1
			  ORDER BY s.id`, projectID, from, to)
	} else {
		rows, err = tx.Query(ctx,
			`WITH `+revisionValidity+`
			 SELECT DISTINCT s.id,
			        COALESCE((SELECT MAX(r2.revision) FROM service_definition_revisions r2
			                   WHERE r2.service_id = s.id AND r2.state='effective'), 0)
			   FROM services s
			   JOIN revs rv ON rv.service_id = s.id
			        AND rv.effective_at < $4 AND COALESCE(rv.next_at, 'infinity'::timestamptz) > $3
			   JOIN service_definition_members m
			     ON m.revision_id = rv.id AND m.role='sli' AND m.monitor_id = $2
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
		if err := rows.Scan(&svc.ServiceID, &svc.DefinitionGeneration); err != nil {
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
	// INTERVAL overlap, not boundary membership. A window entirely inside one bucket —
	// [10:00:30, 10:00:45) — contains no bucket_start at all, so `bucket_start >= from AND
	// bucket_start < to` counted zero, the mutation needed no token, and the invalidation
	// then FLOORED to 10:00 and rewrote exactly the sealed bucket this gate exists to guard.
	// A half-open range overlaps a bucket iff it starts before the bucket ends and ends
	// after the bucket starts.
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets
		  WHERE service_id = ANY($1) AND state='sealed'
		    AND bucket_start < $3 AND bucket_start + ($4 * interval '1 microsecond') > $2`,
		ids, from, to, domain.CanonicalBucket.Microseconds()).Scan(&n); err != nil {
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
	ctx context.Context, w domain.MaintenanceWindow, previewID string, rawRetention time.Duration,
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
		rawFloor, ferr := resolveRawFloorTx(ctx, tx, rawRetention)
		if ferr != nil {
			return domain.MaintenanceWindow{}, ferr
		}
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
func (s *Store) AnnulMaintenanceWindow(ctx context.Context, projectID, id, previewID string, rawRetention time.Duration) error {
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
		rawFloor, ferr := resolveRawFloorTx(ctx, tx, rawRetention)
		if ferr != nil {
			return ferr
		}
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

// previewPersistReserve is the slice of the preview budget held back for writing the token
// itself: the work phases may not touch it, so an exhausted budget can still persist the
// approximate answer instead of dying while trying to say it.
const previewPersistReserve = 300 * time.Millisecond

// previewWallBudget is how long one preview may hold the project's membership lock.
func (s *Store) previewWallBudget() time.Duration {
	if s.previewBudget > 0 {
		return s.previewBudget
	}
	return 2 * time.Second
}

// previewProjectionBudget bounds how many buckets one preview will re-reduce. Past it the
// preview is honestly `approximate` and a confirm refuses it: a change nobody could be shown
// is not one an operator can be said to have approved.
const previewProjectionBudget = 4320 // three days of canonical buckets

// Unprojected reasons. A bare false forces the operator to guess between "narrow the range",
// "try again" and "this can never run", which are three different remediations.
const (
	ReasonRangeTooLong = "range_too_long"
	ReasonWallBudget   = "wall_budget"
	ReasonEvidenceGone = "evidence_gone"
)

// projectBothSides computes the BEFORE and the AFTER over the EXACT requested range, through
// the same clipped reducer, in one pass.
//
// Clipping is the point. The first implementation reduced whole buckets, so an operator
// previewing [10:00:30, 10:00:45) confirmed numbers computed over a full minute — sixty
// seconds of splits for a fifteen-second question, violating the payload's own contract that
// each axis sums to the range's length. Both sides now reduce
// [max(bucket_start, from), min(bucket_end, to)) per bucket, from the same observations and
// spans, so each conserves to (to − from) exactly and their difference is exactly the
// mutation's effect. Time no epoch governs is counted UNKNOWN on both sides — undecided is
// the honest name for it, and dropping it would break the conservation the client checks.
//
// `addSpan` is the window a create would introduce; `dropWindowID` the window an annul would
// remove. Exactly one is set.
func (s *Store) projectBothSides(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to time.Time,
	addSpan *reliability.MaintenanceSpan, dropWindowID string, deadline time.Time,
) (before, after ServiceAggregate, reason string, err error) {
	// TOUCHED buckets, not duration divided by bucket: an unaligned range touches one more
	// bucket than its length suggests — [10:00:30, 13:00:30) walks floor(from) through
	// ceil(to) — and integer division admitted budget+1 buckets at exactly the limit.
	if domain.CeilToBucket(to).Sub(domain.FloorToBucket(from))/domain.CanonicalBucket > previewProjectionBudget {
		return ServiceAggregate{}, ServiceAggregate{}, ReasonRangeTooLong, nil
	}
	for at := domain.FloorToBucket(from); at.Before(to); at = at.Add(domain.CanonicalBucket) {
		// The bucket count above bounds WORK; this bounds TIME. Slow buckets under a small
		// count still hold the project's membership lock, and a preview that cannot finish
		// inside its wall budget is honestly approximate rather than expensively complete.
		if !time.Now().Before(deadline) {
			return ServiceAggregate{}, ServiceAggregate{}, ReasonWallBudget, nil
		}
		cs, ce := at, at.Add(domain.CanonicalBucket)
		if cs.Before(from) {
			cs = from
		}
		if ce.After(to) {
			ce = to
		}
		dur := ce.Sub(cs).Microseconds()

		epochID, members, policies, eerr := epochAt(ctx, tx, serviceID, at)
		if eerr != nil {
			return ServiceAggregate{}, ServiceAggregate{}, "", eerr
		}
		if epochID == "" || len(members) == 0 {
			before.Unknown += dur
			before.HealthUnknown += dur
			after.Unknown += dur
			after.HealthUnknown += dur
			continue
		}
		// Evidence destroyed since sealing makes the range unrecomputable: the confirm's
		// repair would park with ErrEvidenceGone, so the token must be unconfirmable NOW —
		// an approved projection of a change that cannot run is a promise the system knows
		// it will break.
		if gone, gerr := anyMemberMonitorGone(ctx, tx, members); gerr != nil {
			return ServiceAggregate{}, ServiceAggregate{}, "", gerr
		} else if gone {
			return ServiceAggregate{}, ServiceAggregate{}, ReasonEvidenceGone, nil
		}
		observations, oerr := observationsFor(ctx, tx, members, cs, ce)
		if oerr != nil {
			return ServiceAggregate{}, ServiceAggregate{}, "", oerr
		}
		spans, serr := maintenanceSpansFor(ctx, tx, projectID, members, cs, ce)
		if serr != nil {
			return ServiceAggregate{}, ServiceAggregate{}, "", serr
		}

		// The BEFORE side reduces the world as it stands; the AFTER side reduces the same
		// inputs with the hypothetical applied — a create ADDS its span, an annul DROPS the
		// one it names. One shared read, two reductions, so the two sides cannot drift.
		spansAfter := make([]reliability.MaintenanceSpan, 0, len(spans)+1)
		for _, sp := range spans {
			if dropWindowID != "" && sp.ID == dropWindowID {
				continue
			}
			spansAfter = append(spansAfter, sp)
		}
		if addSpan != nil && addSpan.To.After(cs) && addSpan.From.Before(ce) {
			spansAfter = append(spansAfter, *addSpan)
		}

		for _, side := range []struct {
			agg *ServiceAggregate
			sp  []reliability.MaintenanceSpan
		}{{&before, spans}, {&after, spansAfter}} {
			b, rerr := reliability.Reduce(reliability.Input{
				Start: cs, End: ce, Members: members,
				Observations: observations, Maintenance: side.sp, Policies: policies,
			})
			if rerr != nil {
				return ServiceAggregate{}, ServiceAggregate{}, "", fmt.Errorf("store: project bucket %s: %w", cs, rerr)
			}
			side.agg.Good += b.Durations.Good.Microseconds()
			side.agg.Bad += b.Durations.Bad.Microseconds()
			side.agg.Unknown += b.Durations.Unknown.Microseconds()
			side.agg.Excluded += b.Durations.Excluded.Microseconds()
			side.agg.Healthy += b.Durations.Healthy.Microseconds()
			side.agg.Degraded += b.Durations.Degraded.Microseconds()
			side.agg.Down += b.Durations.Down.Microseconds()
			side.agg.HealthUnknown += b.Durations.HealthUnknown.Microseconds()
		}
	}
	return before, after, "", nil
}

// ServiceAggregate is one side of a projection: BOTH conserved axes over a range, in
// microseconds. Health is carried alongside availability because a mutation can move one
// without the other — an exclusion landing entirely inside already-degraded time changes
// health history and leaves good/bad untouched, and a preview showing only availability
// reports "no change" for a change.
type ServiceAggregate struct {
	Good, Bad, Unknown, Excluded           int64
	Healthy, Degraded, Down, HealthUnknown int64
}
