package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Project-scoped membership queries (iter-0206, FR-040, func-project-members.md). A project admin
// manages the members of their own project; the SCOPE is held here, in the SQL, not only in the
// handler: every statement names the project, so no id — an org-level membership, another
// project's, another org's — is reachable through a project.

// ErrUnknownUser is returned when a membership names a user that does not exist.
var ErrUnknownUser = errors.New("store: unknown user")

// ListProjectMembers returns the project-scoped memberships of one project, enriched with each
// user's identity and last-seen time (the shape of an org member row).
func (s *Store) ListProjectMembers(ctx context.Context, projectID string) ([]domain.Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.user_id, m.org_id, m.project_id, m.role, m.created_at,
		        u.email, u.display_name, u.last_seen_at
		   FROM memberships m JOIN users u ON u.id = m.user_id
		  WHERE m.project_id = $1
		  ORDER BY u.display_name, u.email`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list project members: %w", err)
	}
	defer rows.Close()
	out := []domain.Member{}
	for rows.Next() {
		var (
			mem       domain.Member
			projectID *string
			role      string
			lastSeen  *time.Time
		)
		if err := rows.Scan(&mem.ID, &mem.UserID, &mem.OrgID, &projectID, &role, &mem.CreatedAt,
			&mem.Email, &mem.DisplayName, &lastSeen); err != nil {
			return nil, fmt.Errorf("store: scan project member: %w", err)
		}
		if projectID != nil {
			mem.ProjectID = *projectID
		}
		mem.Role = domain.Role(role)
		mem.LastActiveAt = lastSeen
		out = append(out, mem)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate project members: %w", err)
	}
	return out, nil
}

// ListProjectInheritedMembers returns the ORG-level memberships of the project's organization —
// who reaches the project through the organization — as role, email and name only.
func (s *Store) ListProjectInheritedMembers(ctx context.Context, projectID string) ([]domain.InheritedMember, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.role, u.email, u.display_name
		   FROM projects p
		   JOIN memberships m ON m.org_id = p.org_id AND m.project_id IS NULL
		   JOIN users u ON u.id = m.user_id
		  WHERE p.id = $1
		  ORDER BY u.display_name, u.email`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list inherited members: %w", err)
	}
	defer rows.Close()
	out := []domain.InheritedMember{}
	for rows.Next() {
		var (
			im   domain.InheritedMember
			role string
		)
		if err := rows.Scan(&role, &im.Email, &im.DisplayName); err != nil {
			return nil, fmt.Errorf("store: scan inherited member: %w", err)
		}
		im.Role = domain.Role(role)
		out = append(out, im)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate inherited members: %w", err)
	}
	return out, nil
}

// CreateProjectMembership grants userID a project role on projectID; the organization is the
// project's own. Failures are told apart: the project is gone → ErrNotFound; the user already has
// a membership in this project → ErrConflict; the user does not exist → ErrUnknownUser. A role
// that is not a project role is refused by the domain check and, behind it, by the schema.
func (s *Store) CreateProjectMembership(ctx context.Context, projectID, userID string, role domain.Role) (domain.Membership, error) {
	if !role.ValidForScope(domain.ScopeProject) {
		return domain.Membership{}, fmt.Errorf("store: %q is not a project role", role)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO memberships (user_id, org_id, project_id, role)
		 SELECT $2, p.org_id, p.id, $3 FROM projects p WHERE p.id = $1
		 RETURNING `+membershipColumns,
		projectID, userID, string(role))
	m, err := scanMembership(row)
	switch {
	case noRows(err):
		return domain.Membership{}, ErrNotFound
	case isUniqueViolation(err):
		return domain.Membership{}, ErrConflict
	case isUnknownUserError(err):
		return domain.Membership{}, ErrUnknownUser
	case isForeignKeyViolation(err):
		// The project row was read and is gone by the INSERT's FK check.
		return domain.Membership{}, ErrNotFound
	case err != nil:
		return domain.Membership{}, fmt.Errorf("store: create project membership: %w", err)
	}
	return m, nil
}

// UpdateProjectMembershipRole changes the role of a project-scoped membership OF THIS PROJECT, or
// returns ErrNotFound — for an org-level row, another project's row, an unknown id, or a row that
// vanished since the caller looked.
func (s *Store) UpdateProjectMembershipRole(ctx context.Context, projectID, id string, role domain.Role) (domain.Membership, error) {
	if !role.ValidForScope(domain.ScopeProject) {
		return domain.Membership{}, fmt.Errorf("store: %q is not a project role", role)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE memberships SET role = $3 WHERE id = $1 AND project_id = $2 RETURNING `+membershipColumns,
		id, projectID, string(role))
	m, err := scanMembership(row)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.Membership{}, ErrNotFound
	}
	if err != nil {
		return domain.Membership{}, fmt.Errorf("store: update project membership role: %w", err)
	}
	return m, nil
}

// DeleteProjectMembership removes a project-scoped membership OF THIS PROJECT and returns it (for
// the audit row), or ErrNotFound.
func (s *Store) DeleteProjectMembership(ctx context.Context, projectID, id string) (domain.Membership, error) {
	row := s.pool.QueryRow(ctx,
		`DELETE FROM memberships WHERE id = $1 AND project_id = $2 RETURNING `+membershipColumns,
		id, projectID)
	m, err := scanMembership(row)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.Membership{}, ErrNotFound
	}
	if err != nil {
		return domain.Membership{}, fmt.Errorf("store: delete project membership: %w", err)
	}
	return m, nil
}

// isUnknownUserError: the user foreign key failed, or the user id is not even a uuid.
func isUnknownUserError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return (pgErr.Code == "23503" && pgErr.ConstraintName == "memberships_user_id_fkey") || pgErr.Code == "22P02"
}
