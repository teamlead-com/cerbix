package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const statusPageColumns = "id, org_id, project_id, slug, title, visibility, unlisted_token, created_at, updated_at"

func scanStatusPage(row pgx.Row) (domain.StatusPage, error) {
	var (
		sp   domain.StatusPage
		proj *string
	)
	if err := row.Scan(&sp.ID, &sp.OrgID, &proj, &sp.Slug, &sp.Title, &sp.Visibility,
		&sp.UnlistedToken, &sp.CreatedAt, &sp.UpdatedAt); err != nil {
		return domain.StatusPage{}, err
	}
	if proj != nil {
		sp.ProjectID = *proj
	}
	return sp, nil
}

// CreateStatusPage inserts a status page (validated in domain).
func (s *Store) CreateStatusPage(ctx context.Context, sp domain.StatusPage) (domain.StatusPage, error) {
	if err := sp.Validate(); err != nil {
		return domain.StatusPage{}, fmt.Errorf("store: invalid status page: %w", err)
	}
	var projID *string
	if sp.ProjectID != "" {
		projID = &sp.ProjectID
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO status_pages (org_id, project_id, slug, title, visibility, unlisted_token)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+statusPageColumns,
		sp.OrgID, projID, sp.Slug, sp.Title, sp.Visibility, sp.UnlistedToken)
	created, err := scanStatusPage(row)
	if err != nil {
		return domain.StatusPage{}, fmt.Errorf("store: create status page: %w", err)
	}
	return created, nil
}

// UpdateStatusPage updates a page's title, visibility, and unlisted token
// (title/visibility set by the caller; slug and org are immutable). Returns the
// refreshed row or ErrNotFound.
func (s *Store) UpdateStatusPage(ctx context.Context, sp domain.StatusPage) (domain.StatusPage, error) {
	if err := sp.Validate(); err != nil {
		return domain.StatusPage{}, fmt.Errorf("store: invalid status page: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE status_pages SET title = $2, visibility = $3, unlisted_token = $4, updated_at = now()
		 WHERE id = $1 RETURNING `+statusPageColumns,
		sp.ID, sp.Title, sp.Visibility, sp.UnlistedToken)
	updated, err := scanStatusPage(row)
	if noRows(err) {
		return domain.StatusPage{}, ErrNotFound
	}
	if err != nil {
		return domain.StatusPage{}, fmt.Errorf("store: update status page: %w", err)
	}
	return updated, nil
}

// DeleteStatusPage removes a status page; its components cascade (FK ON DELETE
// CASCADE). Returns ErrNotFound if nothing was deleted.
func (s *Store) DeleteStatusPage(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM status_pages WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete status page: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetStatusPage returns a status page by id, or ErrNotFound.
func (s *Store) GetStatusPage(ctx context.Context, id string) (domain.StatusPage, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+statusPageColumns+` FROM status_pages WHERE id = $1`, id)
	sp, err := scanStatusPage(row)
	if noRows(err) {
		return domain.StatusPage{}, ErrNotFound
	}
	if err != nil {
		return domain.StatusPage{}, fmt.Errorf("store: get status page: %w", err)
	}
	return sp, nil
}

// GetStatusPageBySlug returns a status page by its unique slug, or ErrNotFound.
func (s *Store) GetStatusPageBySlug(ctx context.Context, slug string) (domain.StatusPage, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+statusPageColumns+` FROM status_pages WHERE slug = $1`, slug)
	sp, err := scanStatusPage(row)
	if noRows(err) {
		return domain.StatusPage{}, ErrNotFound
	}
	if err != nil {
		return domain.StatusPage{}, fmt.Errorf("store: get status page by slug: %w", err)
	}
	return sp, nil
}

// ListStatusPagesByOrg lists an organization's status pages, newest first.
func (s *Store) ListStatusPagesByOrg(ctx context.Context, orgID string) ([]domain.StatusPage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+statusPageColumns+` FROM status_pages WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list status pages: %w", err)
	}
	defer rows.Close()
	var out []domain.StatusPage
	for rows.Next() {
		sp, err := scanStatusPage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan status page: %w", err)
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate status pages: %w", err)
	}
	return out, nil
}

const componentColumns = `id, status_page_id, org_id, COALESCE(source_project::text,''), source,
	name, description, group_name, position, COALESCE(monitor_id::text,''),
	COALESCE(service_id::text,''), manual_status, revision, created_at, updated_at`

func scanComponent(row pgx.Row) (domain.Component, error) {
	var c domain.Component
	// Bindings and the source project come back as COALESCEd text, so a dormant binding reads
	// as "" rather than needing a nullable scan target per column.
	if err := row.Scan(&c.ID, &c.StatusPageID, &c.OrgID, &c.SourceProject, &c.Source,
		&c.Name, &c.Description, &c.GroupName, &c.Position, &c.MonitorID,
		&c.ServiceID, &c.ManualStatus, &c.Revision, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return domain.Component{}, err
	}
	return c, nil
}

// ErrPageComponentCeiling is returned when a page is at its persisted component ceiling
// (FR-021 §15.0). The ceiling only ever SHRINKS, so an oversized page cannot grow: the public
// render is unauthenticated and every component multiplies its cost.
var ErrPageComponentCeiling = errors.New("store: status page is at its component ceiling")

// CreateComponent inserts a component (validated in domain) inside one transaction that also
// enforces the page ceiling and bumps the page's component generation — the structural CAS the
// conversion preview compares against (§15.0). The org and the binding project are derived
// SERVER-side from the page and the binding, never taken from the caller, so a component cannot
// be planted in another tenant by a crafted request.
func (s *Store) CreateComponent(ctx context.Context, c domain.Component) (domain.Component, error) {
	if err := c.Validate(); err != nil {
		return domain.Component{}, fmt.Errorf("store: invalid component: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: begin create component: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The page row is locked first: the ceiling check is otherwise a check-then-act, and two
	// concurrent creates could both pass it.
	var orgID string
	var pageProject *string
	var ceiling int
	err = tx.QueryRow(ctx,
		`SELECT org_id, project_id, component_ceiling FROM status_pages WHERE id = $1 FOR UPDATE`,
		c.StatusPageID).Scan(&orgID, &pageProject, &ceiling)
	if noRows(err) {
		return domain.Component{}, ErrNotFound
	}
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: lock status page: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM components WHERE status_page_id = $1`, c.StatusPageID).Scan(&count); err != nil {
		return domain.Component{}, fmt.Errorf("store: count components: %w", err)
	}
	if count >= ceiling {
		return domain.Component{}, ErrPageComponentCeiling
	}

	source, bindingProject, err := componentSourceOf(ctx, tx, c)
	if err != nil {
		return domain.Component{}, err
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO components (status_page_id, org_id, source_project, source, name, description,
		                         group_name, position, monitor_id, service_id, manual_status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+componentColumns,
		c.StatusPageID, orgID, nullableID(bindingProject), string(source), c.Name, c.Description,
		c.GroupName, c.Position, nullableID(c.MonitorID), nullableID(c.ServiceID), c.ManualStatus)
	created, err := scanComponent(row)
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: create component: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Component{}, fmt.Errorf("store: commit create component: %w", err)
	}
	return created, nil
}

// componentSourceOf resolves the ACTIVE source and its project from the requested bindings.
// A service binding wins over a monitor one when both are given, because a caller asking for a
// service component with a leftover monitor id is describing a conversion, not an ambiguity.
func componentSourceOf(ctx context.Context, tx pgx.Tx, c domain.Component) (domain.ComponentSource, string, error) {
	switch {
	case c.ServiceID != "":
		var proj string
		err := tx.QueryRow(ctx, `SELECT project_id FROM services WHERE id = $1`, c.ServiceID).Scan(&proj)
		if noRows(err) {
			return "", "", ErrNotFound
		}
		if err != nil {
			return "", "", fmt.Errorf("store: resolve component service: %w", err)
		}
		return domain.ComponentSourceService, proj, nil
	case c.MonitorID != "":
		var proj string
		err := tx.QueryRow(ctx, `SELECT project_id FROM monitors WHERE id = $1`, c.MonitorID).Scan(&proj)
		if noRows(err) {
			return "", "", ErrNotFound
		}
		if err != nil {
			return "", "", fmt.Errorf("store: resolve component monitor: %w", err)
		}
		return domain.ComponentSourceMonitor, proj, nil
	default:
		return domain.ComponentSourceManual, "", nil
	}
}

// The page's structural CAS counter and each component's revision are advanced by DATABASE
// TRIGGERS (migration 00081), not here. "ANY component mutation bumps the generation" cannot be
// application discipline while FK actions are part of the contract: a project cascade deletes
// components with no application on the path, and a surviving org-level page's generation stayed
// put — so a preview for a DIFFERENT component on that page could still be confirmed after a
// neighbour had vanished ([314] P1-1).

// ListComponentsByPage lists a page's components in display order.
func (s *Store) ListComponentsByPage(ctx context.Context, pageID string) ([]domain.Component, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+componentColumns+` FROM components WHERE status_page_id = $1 ORDER BY position ASC, created_at ASC`, pageID)
	if err != nil {
		return nil, fmt.Errorf("store: list components: %w", err)
	}
	defer rows.Close()
	var out []domain.Component
	for rows.Next() {
		c, err := scanComponent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan component: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate components: %w", err)
	}
	return out, nil
}

// GetComponent returns a component by id, or ErrNotFound.
func (s *Store) GetComponent(ctx context.Context, id string) (domain.Component, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+componentColumns+` FROM components WHERE id = $1`, id)
	c, err := scanComponent(row)
	if noRows(err) {
		return domain.Component{}, ErrNotFound
	}
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: get component: %w", err)
	}
	return c, nil
}

// DeleteComponent removes a component and advances its page's structural generation, so a
// conversion preview taken while this line still existed cannot be confirmed against the page
// it no longer describes (§15.0, invariant 70).
func (s *Store) DeleteComponent(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin delete component: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// PAGE FIRST ([318] P0-2): the DELETE takes the component tuple, and the AFTER trigger then
	// waits for the page — component→page, which cycles against every page→component path. The
	// page is therefore locked before the component is touched at all.
	var pageID string
	err = tx.QueryRow(ctx, `SELECT status_page_id FROM components WHERE id = $1`, id).Scan(&pageID)
	if noRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: locate component page: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM status_pages WHERE id = $1 FOR UPDATE`, pageID); err != nil {
		return fmt.Errorf("store: lock component page: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM components WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete component: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit delete component: %w", err)
	}
	return nil
}

// ListOpenIncidentsByProject lists a project's unresolved incidents, newest first.
func (s *Store) ListOpenIncidentsByProject(ctx context.Context, projectID string) ([]domain.Incident, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+incidentColumns+` FROM incidents
		 WHERE project_id = $1 AND status <> 'resolved' ORDER BY started_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list open incidents: %w", err)
	}
	defer rows.Close()
	var out []domain.Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan open incident: %w", err)
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate open incidents: %w", err)
	}
	return out, nil
}
