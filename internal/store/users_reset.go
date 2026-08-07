package store

import (
	"context"
	"fmt"
	"time"
)

// CreatePasswordResetToken stores a single-use password-reset token hash for a
// user with the given expiry. Only the hash is persisted.
func (s *Store) CreatePasswordResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO password_reset_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, tokenHash, expiresAt)
	if err != nil {
		return fmt.Errorf("store: create password reset token: %w", err)
	}
	return nil
}

// ConsumePasswordResetToken atomically marks a valid (unused, unexpired) token as
// used and returns its user id. Returns ErrNotFound when the token is unknown,
// already used, or expired.
func (s *Store) ConsumePasswordResetToken(ctx context.Context, tokenHash string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx,
		`UPDATE password_reset_tokens SET used_at = now()
		  WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		  RETURNING user_id`, tokenHash).Scan(&userID)
	if noRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: consume password reset token: %w", err)
	}
	return userID, nil
}

// ResetPasswordWithToken consumes a valid reset token AND sets the new password hash in ONE
// transaction, so a failure to write the password never burns the token (leaving the user
// with a dead link and an unchanged password). Returns the affected user id. ErrNotFound if
// the token is unknown/used/expired or the user is gone.
func (s *Store) ResetPasswordWithToken(ctx context.Context, tokenHash, passwordHash string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin reset password: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var userID string
	err = tx.QueryRow(ctx,
		`UPDATE password_reset_tokens SET used_at = now()
		  WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		  RETURNING user_id`, tokenHash).Scan(&userID)
	if noRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: consume reset token: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, userID, passwordHash)
	if err != nil {
		return "", fmt.Errorf("store: reset set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound // user deleted between token issue and reset → rollback consumes nothing
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit reset password: %w", err)
	}
	return userID, nil
}
