package store

import (
	"context"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// LocalCredential is the minimal record needed to authenticate a local login.
type LocalCredential struct {
	UserID        string
	PasswordHash  string
	IsGlobalAdmin bool
	TOTPEnabled   bool
	TOTPSecret    string // decrypted; empty unless TOTP is enrolled
}

// CreateLocalUser creates a password-backed (non-OIDC) user.
func (s *Store) CreateLocalUser(ctx context.Context, email, displayName, passwordHash string, globalAdmin bool) (domain.User, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name, password_hash, is_global_admin)
		 VALUES ($1,$2,$3,$4) RETURNING `+userColumns,
		email, displayName, passwordHash, globalAdmin)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, fmt.Errorf("store: create local user: %w", err)
	}
	return u, nil
}

// LocalCredentialByEmail returns the credential for a local account, or ErrNotFound.
func (s *Store) LocalCredentialByEmail(ctx context.Context, email string) (LocalCredential, error) {
	var (
		c      LocalCredential
		secret string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, password_hash, is_global_admin, totp_enabled, totp_secret FROM users
		 WHERE email = $1 AND password_hash IS NOT NULL`, email).
		Scan(&c.UserID, &c.PasswordHash, &c.IsGlobalAdmin, &c.TOTPEnabled, &secret)
	if noRows(err) {
		return LocalCredential{}, ErrNotFound
	}
	if err != nil {
		return LocalCredential{}, fmt.Errorf("store: local credential: %w", err)
	}
	plain, err := s.cipher.Decrypt(secret)
	if err != nil {
		return LocalCredential{}, fmt.Errorf("store: decrypt totp secret: %w", err)
	}
	c.TOTPSecret = plain
	return c, nil
}

// PasswordHashByID returns a user's password hash, or ErrNotFound if the user
// does not exist or is not a local account.
func (s *Store) PasswordHashByID(ctx context.Context, id string) (string, error) {
	var hash *string
	err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, id).Scan(&hash)
	if noRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: password hash: %w", err)
	}
	if hash == nil {
		return "", ErrNotFound // not a local account
	}
	return *hash, nil
}

// ChangePassword sets a local account's new password hash AND revokes every session of the user
// except keepSessionToken (the caller's; "" keeps none), in one transaction (sec-hardening-2 H-5):
// a stolen or stale session cannot outlive the credential change, and a failed revocation leaves
// the old password in place rather than a new password with live old sessions. Returns the number
// of sessions removed; ErrNotFound when the user is gone or is not a local account. API tokens
// are not touched — they are org-scoped credentials with their own lifecycle.
func (s *Store) ChangePassword(ctx context.Context, userID, passwordHash, keepSessionToken string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin change password: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1 AND password_hash IS NOT NULL`,
		userID, passwordHash)
	if err != nil {
		return 0, fmt.Errorf("store: change password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	// A blank keepSessionToken hashes to a value no real token matches: every session goes.
	tag, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2`,
		userID, HashToken(keepSessionToken))
	if err != nil {
		return 0, fmt.Errorf("store: change password revoke sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit change password: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountUsers returns the total number of users (used for bootstrap decisions).
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}
