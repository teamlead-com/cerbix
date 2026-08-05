package store

import (
	"context"
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

const componentColumns = "id, status_page_id, name, description, group_name, position, monitor_id, manual_status, created_at, updated_at"

func scanComponent(row pgx.Row) (domain.Component, error) {
	var (
		c         domain.Component
		monitorID *string
	)
	if err := row.Scan(&c.ID, &c.StatusPageID, &c.Name, &c.Description, &c.GroupName,
		&c.Position, &monitorID, &c.ManualStatus, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return domain.Component{}, err
	}
	if monitorID != nil {
		c.MonitorID = *monitorID
	}
	return c, nil
}

// CreateComponent inserts a component (validated in domain).
func (s *Store) CreateComponent(ctx context.Context, c domain.Component) (domain.Component, error) {
	if err := c.Validate(); err != nil {
		return domain.Component{}, fmt.Errorf("store: invalid component: %w", err)
	}
	var monitorID *string
	if c.MonitorID != "" {
		monitorID = &c.MonitorID
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO components (status_page_id, name, description, group_name, position, monitor_id, manual_status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+componentColumns,
		c.StatusPageID, c.Name, c.Description, c.GroupName, c.Position, monitorID, c.ManualStatus)
	created, err := scanComponent(row)
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: create component: %w", err)
	}
	return created, nil
}

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

// DeleteComponent removes a component.
func (s *Store) DeleteComponent(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM components WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete component: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
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
