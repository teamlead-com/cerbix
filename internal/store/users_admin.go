package store

import (
	"context"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// ListAllUsers returns every user of the instance (global-admin surface),
// user-keyed with the memberships aggregated per user — users outside any
// organization come back with an empty memberships list. q filters by a
// case-insensitive substring of email or display name.
func (s *Store) ListAllUsers(ctx context.Context, q string) ([]domain.AdminUser, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT u.id, COALESCE(u.oidc_sub, ''), u.email, u.display_name, u.is_global_admin,
		        u.password_hash IS NOT NULL, u.created_at, u.updated_at,
		        (SELECT max(created_at) FROM sessions se WHERE se.user_id = u.id) AS last_active
		   FROM users u
		  WHERE $1 = '' OR u.email ILIKE '%' || $1 || '%' OR u.display_name ILIKE '%' || $1 || '%'
		  ORDER BY u.created_at, u.email`, q)
	if err != nil {
		return nil, fmt.Errorf("store: list all users: %w", err)
	}
	defer rows.Close()
	var out []domain.AdminUser
	index := map[string]int{}
	for rows.Next() {
		var (
			u        domain.AdminUser
			hasLocal bool
			lastSeen *time.Time
		)
		if err := rows.Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.IsGlobalAdmin,
			&hasLocal, &u.CreatedAt, &u.UpdatedAt, &lastSeen); err != nil {
			return nil, fmt.Errorf("store: scan user: %w", err)
		}
		switch {
		case hasLocal && u.OIDCSub != "":
			u.AuthType = "both"
		case u.OIDCSub != "":
			u.AuthType = "oidc"
		case hasLocal:
			u.AuthType = "local"
		default:
			u.AuthType = "none"
		}
		u.LastActiveAt = lastSeen
		u.Memberships = []domain.AdminUserMembership{}
		index[u.ID] = len(out)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate users: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	mrows, err := s.pool.Query(ctx,
		`SELECT m.user_id, m.org_id, o.name, COALESCE(m.project_id::text, ''), COALESCE(p.name, ''), m.role
		   FROM memberships m
		   JOIN organizations o ON o.id = m.org_id
		   LEFT JOIN projects p ON p.id = m.project_id
		  ORDER BY o.name, m.project_id NULLS FIRST`)
	if err != nil {
		return nil, fmt.Errorf("store: list user memberships: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var (
			userID string
			m      domain.AdminUserMembership
			role   string
		)
		if err := mrows.Scan(&userID, &m.OrgID, &m.OrgName, &m.ProjectID, &m.ProjectName, &role); err != nil {
			return nil, fmt.Errorf("store: scan user membership: %w", err)
		}
		m.Role = domain.Role(role)
		if i, ok := index[userID]; ok {
			out[i].Memberships = append(out[i].Memberships, m)
		}
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate user memberships: %w", err)
	}
	return out, nil
}

// DeleteUser removes a user; memberships, sessions, TOTP state and reset
// tokens go with it via ON DELETE CASCADE, audit entries keep a NULL actor.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountGlobalAdmins reports how many users hold the global-admin flag —
// the guard against demoting or deleting the last one.
func (s *Store) CountGlobalAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_global_admin`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count global admins: %w", err)
	}
	return n, nil
}
