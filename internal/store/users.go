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

// UpsertUserByOIDCIdentity creates or updates a user keyed by the OIDC identity
// (issuer, subject) — not the subject alone, so two providers minting the same
// `sub` map to distinct accounts (see migration 00056). This backs just-in-time
// provisioning on OIDC login.
//
// A legacy row provisioned before the issuer column existed (issuer NULL, this
// subject) adopts the current issuer on first login here — a one-time claim,
// guarded so it can't hijack a subject already bound to this issuer. The whole
// thing runs in one transaction so the claim and the upsert are atomic.
func (s *Store) UpsertUserByOIDCIdentity(ctx context.Context, issuer, sub, email, displayName string) (domain.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.User{}, fmt.Errorf("store: begin upsert user: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// One-time legacy claim: a pre-issuer row with this subject adopts the current
	// issuer, unless (issuer, sub) already exists (never steal a bound identity).
	if _, err := tx.Exec(ctx,
		`UPDATE users SET oidc_issuer = $1
		   WHERE oidc_issuer IS NULL AND oidc_sub = $2
		     AND NOT EXISTS (SELECT 1 FROM users u2 WHERE u2.oidc_issuer = $1 AND u2.oidc_sub = $2)`,
		issuer, sub); err != nil {
		return domain.User{}, fmt.Errorf("store: claim legacy oidc user: %w", err)
	}

	row := tx.QueryRow(ctx,
		`INSERT INTO users (oidc_issuer, oidc_sub, email, display_name)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (oidc_issuer, oidc_sub) WHERE oidc_sub IS NOT NULL
		 DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, updated_at = now()
		 RETURNING `+userColumns,
		issuer, sub, email, displayName)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, fmt.Errorf("store: upsert user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.User{}, fmt.Errorf("store: commit upsert user: %w", err)
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
// Test support; the login path uses UpsertUserByOIDCIdentity.
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
