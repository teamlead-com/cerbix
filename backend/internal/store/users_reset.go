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
