package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const orgColumns = "id, slug, name, created_at, updated_at"

func scanOrg(row pgx.Row) (domain.Organization, error) {
	var o domain.Organization
	err := row.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

// CreateOrganization inserts a new organization.
func (s *Store) CreateOrganization(ctx context.Context, slug, name string) (domain.Organization, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name) VALUES ($1, $2) RETURNING `+orgColumns,
		slug, name)
	o, err := scanOrg(row)
	if err != nil {
		return domain.Organization{}, fmt.Errorf("store: create organization: %w", err)
	}
	return o, nil
}

// GetOrganization returns an organization by id.
func (s *Store) GetOrganization(ctx context.Context, id string) (domain.Organization, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations WHERE id = $1`, id)
	o, err := scanOrg(row)
	if noRows(err) {
		return domain.Organization{}, ErrNotFound
	}
	if err != nil {
		return domain.Organization{}, fmt.Errorf("store: get organization: %w", err)
	}
	return o, nil
}

// ListOrganizations returns all organizations (for Global Admin use).
func (s *Store) ListOrganizations(ctx context.Context) ([]domain.Organization, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+orgColumns+` FROM organizations ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("store: list organizations: %w", err)
	}
	return collectOrgs(rows)
}

// ListOrganizationsForUser returns only organizations the user is a member of.
// This enforces tenant isolation at the query level.
func (s *Store) ListOrganizationsForUser(ctx context.Context, userID string) ([]domain.Organization, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+orgColumns+` FROM organizations o
		 WHERE EXISTS (
		     SELECT 1 FROM memberships m WHERE m.user_id = $1 AND m.org_id = o.id
		 )
		 ORDER BY o.slug`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("store: list organizations for user: %w", err)
	}
	return collectOrgs(rows)
}

func collectOrgs(rows pgx.Rows) ([]domain.Organization, error) {
	defer rows.Close()
	var out []domain.Organization
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan organization: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate organizations: %w", err)
	}
	return out, nil
}
