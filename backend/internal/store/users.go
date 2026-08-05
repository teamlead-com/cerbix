package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const userColumns = "id, oidc_sub, email, display_name, is_global_admin, created_at, updated_at"

func scanUser(row pgx.Row) (domain.User, error) {
	var (
		u   domain.User
		sub *string // oidc_sub is nullable for local users
	)
	err := row.Scan(&u.ID, &sub, &u.Email, &u.DisplayName, &u.IsGlobalAdmin, &u.CreatedAt, &u.UpdatedAt)
	if sub != nil {
		u.OIDCSub = *sub
	}
	return u, err
}

// UpsertUserByOIDCSub creates or updates a user keyed by OIDC subject.
// This backs just-in-time provisioning on OIDC login.
func (s *Store) UpsertUserByOIDCSub(ctx context.Context, sub, email, displayName string) (domain.User, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO users (oidc_sub, email, display_name)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (oidc_sub)
		 DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, updated_at = now()
		 RETURNING `+userColumns,
		sub, email, displayName)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, fmt.Errorf("store: upsert user: %w", err)
	}
	return u, nil
}

// GetUser returns a user by id.
func (s *Store) GetUser(ctx context.Context, id string) (domain.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if noRows(err) {
		return domain.User{}, ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("store: get user: %w", err)
	}
	return u, nil
}

// GetUserByEmail returns a user by email, or ErrNotFound. Used to add a member
// by email (an already-provisioned colleague) instead of a raw user id.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (domain.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1 ORDER BY created_at LIMIT 1`, email)
	u, err := scanUser(row)
	if noRows(err) {
		return domain.User{}, ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("store: get user by email: %w", err)
	}
	return u, nil
}

// GetUserByOIDCSub returns a user by OIDC subject.
func (s *Store) GetUserByOIDCSub(ctx context.Context, sub string) (domain.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE oidc_sub = $1`, sub)
	u, err := scanUser(row)
	if noRows(err) {
		return domain.User{}, ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("store: get user by sub: %w", err)
	}
	return u, nil
}

// SetGlobalAdmin toggles a user's global-admin flag (used for bootstrap).
func (s *Store) SetGlobalAdmin(ctx context.Context, id string, admin bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET is_global_admin = $2, updated_at = now() WHERE id = $1`, id, admin)
	if err != nil {
		return fmt.Errorf("store: set global admin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
