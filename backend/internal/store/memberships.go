package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const membershipColumns = "id, user_id, org_id, project_id, role, created_at"

func scanMembership(row pgx.Row) (domain.Membership, error) {
	var (
		m         domain.Membership
		projectID *string
		role      string
	)
	if err := row.Scan(&m.ID, &m.UserID, &m.OrgID, &projectID, &role, &m.CreatedAt); err != nil {
		return domain.Membership{}, err
	}
	if projectID != nil {
		m.ProjectID = *projectID
	}
	m.Role = domain.Role(role)
	return m, nil
}

// CreateMembership grants a user a role. The membership is validated in the
// domain layer before insertion; the schema re-enforces role/scope and that the
// project (if any) belongs to the org.
func (s *Store) CreateMembership(ctx context.Context, m domain.Membership) (domain.Membership, error) {
	if err := m.Validate(); err != nil {
		return domain.Membership{}, fmt.Errorf("store: invalid membership: %w", err)
	}
	var projectID *string
	if m.ProjectID != "" {
		projectID = &m.ProjectID
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO memberships (user_id, org_id, project_id, role)
		 VALUES ($1, $2, $3, $4) RETURNING `+membershipColumns,
		m.UserID, m.OrgID, projectID, string(m.Role))
	created, err := scanMembership(row)
	if err != nil {
		return domain.Membership{}, fmt.Errorf("store: create membership: %w", err)
	}
	return created, nil
}

// ListMembershipsByOrg returns every membership within an organization.
func (s *Store) ListMembershipsByOrg(ctx context.Context, orgID string) ([]domain.Membership, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+membershipColumns+` FROM memberships WHERE org_id = $1 ORDER BY project_id NULLS FIRST, user_id`,
		orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list memberships by org: %w", err)
	}
	defer rows.Close()
	var out []domain.Membership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan membership: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate memberships: %w", err)
	}
	return out, nil
}

// ListOrgMembers returns an org's memberships enriched with each user's
// identity (email, display name) and last-seen time (most recent session).
func (s *Store) ListOrgMembers(ctx context.Context, orgID string) ([]domain.Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.user_id, m.org_id, m.project_id, m.role, m.created_at,
		        u.email, u.display_name,
		        (SELECT max(created_at) FROM sessions se WHERE se.user_id = m.user_id) AS last_active
		   FROM memberships m JOIN users u ON u.id = m.user_id
		  WHERE m.org_id = $1
		  ORDER BY m.project_id NULLS FIRST, u.display_name, u.email`,
		orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list org members: %w", err)
	}
	defer rows.Close()
	var out []domain.Member
	for rows.Next() {
		var (
			mem       domain.Member
			projectID *string
			role      string
			lastSeen  *time.Time
		)
		if err := rows.Scan(&mem.ID, &mem.UserID, &mem.OrgID, &projectID, &role, &mem.CreatedAt,
			&mem.Email, &mem.DisplayName, &lastSeen); err != nil {
			return nil, fmt.Errorf("store: scan member: %w", err)
		}
		if projectID != nil {
			mem.ProjectID = *projectID
		}
		mem.Role = domain.Role(role)
		mem.LastActiveAt = lastSeen
		out = append(out, mem)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate members: %w", err)
	}
	return out, nil
}

// GetMembership returns a membership by id, or ErrNotFound.
func (s *Store) GetMembership(ctx context.Context, id string) (domain.Membership, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+membershipColumns+` FROM memberships WHERE id = $1`, id)
	m, err := scanMembership(row)
	if noRows(err) {
		return domain.Membership{}, ErrNotFound
	}
	if err != nil {
		return domain.Membership{}, fmt.Errorf("store: get membership: %w", err)
	}
	return m, nil
}

// UpdateMembershipRole changes a membership's role, returning the updated row.
func (s *Store) UpdateMembershipRole(ctx context.Context, id string, role domain.Role) (domain.Membership, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE memberships SET role = $2 WHERE id = $1 RETURNING `+membershipColumns, id, string(role))
	m, err := scanMembership(row)
	if noRows(err) {
		return domain.Membership{}, ErrNotFound
	}
	if err != nil {
		return domain.Membership{}, fmt.Errorf("store: update membership role: %w", err)
	}
	return m, nil
}

// DeleteMembership removes a membership, or returns ErrNotFound.
func (s *Store) DeleteMembership(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memberships WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete membership: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountOrgAdmins counts an org's org-level admins (project_id IS NULL,
// role = org_admin) — used to prevent removing/demoting the last one.
func (s *Store) CountOrgAdmins(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM memberships WHERE org_id = $1 AND project_id IS NULL AND role = 'org_admin'`,
		orgID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count org admins: %w", err)
	}
	return n, nil
}

// ListMembershipsForUser returns every membership held by a user.
func (s *Store) ListMembershipsForUser(ctx context.Context, userID string) ([]domain.Membership, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+membershipColumns+` FROM memberships WHERE user_id = $1 ORDER BY org_id, project_id NULLS FIRST`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("store: list memberships for user: %w", err)
	}
	defer rows.Close()
	var out []domain.Membership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan membership: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate memberships: %w", err)
	}
	return out, nil
}
