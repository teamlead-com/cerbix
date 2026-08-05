package store

import (
	"context"
	"fmt"
)

// SetTOTPSecret stores an (encrypted) pending TOTP secret and marks 2FA not yet
// enabled — the user confirms with a code to enable it.
func (s *Store) SetTOTPSecret(ctx context.Context, userID, secret string) error {
	enc, err := s.cipher.Encrypt(secret)
	if err != nil {
		return fmt.Errorf("store: encrypt totp secret: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret = $2, totp_enabled = false, updated_at = now() WHERE id = $1`, userID, enc)
	if err != nil {
		return fmt.Errorf("store: set totp secret: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetTOTP returns a user's decrypted TOTP secret and whether 2FA is enabled.
func (s *Store) GetTOTP(ctx context.Context, userID string) (secret string, enabled bool, err error) {
	var enc string
	e := s.pool.QueryRow(ctx, `SELECT totp_secret, totp_enabled FROM users WHERE id = $1`, userID).Scan(&enc, &enabled)
	if noRows(e) {
		return "", false, ErrNotFound
	}
	if e != nil {
		return "", false, fmt.Errorf("store: get totp: %w", e)
	}
	plain, e := s.cipher.Decrypt(enc)
	if e != nil {
		return "", false, fmt.Errorf("store: decrypt totp secret: %w", e)
	}
	return plain, enabled, nil
}

// EnableTOTP marks 2FA enabled (after the user confirms a code).
func (s *Store) EnableTOTP(ctx context.Context, userID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET totp_enabled = true, updated_at = now() WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableTOTP clears the secret, disables 2FA, and drops the user's recovery codes.
func (s *Store) DisableTOTP(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin disable totp: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `UPDATE users SET totp_secret = '', totp_enabled = false, updated_at = now() WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("store: disable totp: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM totp_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: clear recovery codes: %w", err)
	}
	return tx.Commit(ctx)
}

// ReplaceRecoveryCodes deletes any existing recovery codes and stores the given
// hashes (one per code).
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin recovery codes: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `DELETE FROM totp_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: clear recovery codes: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `INSERT INTO totp_recovery_codes (user_id, code_hash) VALUES ($1, $2)`, userID, h); err != nil {
			return fmt.Errorf("store: insert recovery code: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// ConsumeRecoveryCode marks a matching unused recovery code as used, returning
// true if one was consumed (single-use).
func (s *Store) ConsumeRecoveryCode(ctx context.Context, userID, codeHash string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE totp_recovery_codes SET used_at = now()
		  WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, codeHash)
	if err != nil {
		return false, fmt.Errorf("store: consume recovery code: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
