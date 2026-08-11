package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const projectColumns = "id, org_id, slug, name, created_at, updated_at"

func scanProject(row pgx.Row) (domain.Project, error) {
	var p domain.Project
	err := row.Scan(&p.ID, &p.OrgID, &p.Slug, &p.Name, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// CreateProject inserts a new project within an organization.
func (s *Store) CreateProject(ctx context.Context, orgID, slug, name string) (domain.Project, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $3) RETURNING `+projectColumns,
		orgID, slug, name)
	p, err := scanProject(row)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Project{}, ErrConflict
		}
		return domain.Project{}, fmt.Errorf("store: create project: %w", err)
	}
	return p, nil
}

// GetProject returns a project by id.
func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id)
	p, err := scanProject(row)
	if noRows(err) {
		return domain.Project{}, ErrNotFound
	}
	if err != nil {
		return domain.Project{}, fmt.Errorf("store: get project: %w", err)
	}
	return p, nil
}

// DeleteProject permanently removes a project and everything it owns. The database's
// ON DELETE CASCADE fanout (monitors + heartbeats/rollups, incidents, SLA, escalation,
// on-call, notification channels, project-scoped tokens/webhooks/status-pages, file
// bundles) does the child cleanup in one transaction (spec func-project-deletion §5).
//
// Scoped by org for tenant safety: a wrong (id, org_id) pair matches 0 rows and returns
// ErrNotFound — never a cross-tenant delete. Returns ErrManagedByFile when the project is
// owned by a file provider (a reconcile would recreate it — remove the provider's files
// instead, §7.3). The ownership check and the delete share one transaction so a concurrent
// file apply cannot claim ownership between the check and the delete.
func (s *Store) DeleteProject(ctx context.Context, orgID, projectID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: delete project: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var one int
	err = tx.QueryRow(ctx,
		`SELECT 1 WHERE EXISTS (SELECT 1 FROM file_provider_bundles WHERE project_id = $1)
		              OR EXISTS (SELECT 1 FROM managed_monitors WHERE project_id = $1)`,
		projectID).Scan(&one)
	if err == nil {
		return ErrManagedByFile
	}
	if !noRows(err) {
		return fmt.Errorf("store: delete project: ownership check: %w", err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1 AND org_id = $2`, projectID, orgID)
	if err != nil {
		return fmt.Errorf("store: delete project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: delete project: commit: %w", err)
	}
	return nil
}

// ListProjectsByOrg returns all projects in an organization.
func (s *Store) ListProjectsByOrg(ctx context.Context, orgID string) ([]domain.Project, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE org_id = $1 ORDER BY slug`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list projects by org: %w", err)
	}
	return collectProjects(rows)
}

// ListProjectsForUser returns only projects the user may access: those in an org
// where the user has an org-level grant (project_id IS NULL), plus those the user
// has a project-level grant on. Enforces tenant isolation at the query level.
func (s *Store) ListProjectsForUser(ctx context.Context, userID string) ([]domain.Project, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+projectColumns+` FROM projects p
		 WHERE EXISTS (
		     SELECT 1 FROM memberships m
		     WHERE m.user_id = $1
		       AND m.org_id = p.org_id
		       AND (m.project_id IS NULL OR m.project_id = p.id)
		 )
		 ORDER BY p.slug`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("store: list projects for user: %w", err)
	}
	return collectProjects(rows)
}

func collectProjects(rows pgx.Rows) ([]domain.Project, error) {
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan project: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate projects: %w", err)
	}
	return out, nil
}
