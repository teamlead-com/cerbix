package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrTOTPAlreadyEnabled answers EnableTOTP and SetTOTPSecret when 2FA is already enabled — by an
// earlier or a concurrent confirmation.
var ErrTOTPAlreadyEnabled = errors.New("store: two-factor auth is already enabled")

// ErrTOTPSecretChanged is EnableTOTP's answer when the stored pending secret is no longer the one
// the code was confirmed against: a concurrent disable cleared it, or a re-enrollment replaced it.
var ErrTOTPSecretChanged = errors.New("store: the pending two-factor secret changed")

// SetTOTPSecret stores an (encrypted) pending TOTP secret while 2FA is NOT enabled — the user then
// confirms it with a code. It never touches an enabled 2FA (`… AND NOT totp_enabled`): an
// enrollment that read "not enabled" before a concurrent confirmation committed must not replace
// the live secret and switch 2FA off (review of iter-0212, P1-1). A new secret forgets the last
// accepted step (sec-hardening-2 H-7): its codes have never been used.
// ErrNotFound when the user does not exist; ErrTOTPAlreadyEnabled when 2FA is enabled.
func (s *Store) SetTOTPSecret(ctx context.Context, userID, secret string) error {
	enc, err := s.cipher.Encrypt(secret)
	if err != nil {
		return fmt.Errorf("store: encrypt totp secret: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret = $2, totp_enabled = false, totp_last_step = NULL, updated_at = now()
		  WHERE id = $1 AND NOT totp_enabled`, userID, enc)
	if err != nil {
		return fmt.Errorf("store: set totp secret: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			return fmt.Errorf("store: set totp secret: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		return ErrTOTPAlreadyEnabled
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

// EnableTOTP confirms a pending enrollment against the secret the code was validated with. In ONE
// transaction it locks the user's row (`SELECT … FOR UPDATE`), and enables 2FA and stores the
// recovery codes' hashes only if 2FA is not enabled yet and the stored pending secret is still the
// confirmed one (review of iter-0212, P1-1). The row lock serializes it with DisableTOTP and
// SetTOTPSecret, so a concurrent disable (empty secret) or re-enrollment (another secret) between
// the handler's read and this write makes it refuse: 2FA is never enabled with a secret the person
// did not confirm. Of concurrent confirmations exactly one enables, and the recovery codes stored
// are the ones that confirmation shows (sec-hardening-2 H-7). Both writes or neither: an account
// is never TOTP-required without its recovery codes. The step is NOT recorded, so the confirming
// code may still sign in once (S7).
// ErrNotFound when the user does not exist; ErrTOTPAlreadyEnabled when 2FA is already on;
// ErrTOTPSecretChanged when the pending secret is empty or not the confirmed one.
func (s *Store) EnableTOTP(ctx context.Context, userID, confirmedSecret string, recoveryHashes []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin enable totp: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var enc string
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT totp_secret, totp_enabled FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&enc, &enabled)
	if noRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	if enabled {
		return ErrTOTPAlreadyEnabled
	}
	plain, err := s.cipher.Decrypt(enc)
	if err != nil {
		return fmt.Errorf("store: decrypt totp secret: %w", err)
	}
	if plain == "" || confirmedSecret == "" || subtle.ConstantTimeCompare([]byte(plain), []byte(confirmedSecret)) != 1 {
		return ErrTOTPSecretChanged
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET totp_enabled = true, updated_at = now() WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	if err := replaceRecoveryCodes(ctx, tx, userID, recoveryHashes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AcceptTOTPStep records step as the last TOTP step accepted for the user's sign-in, if it is above
// the one recorded — one compare-and-set, so of two concurrent sign-ins with one code exactly one
// is accepted (sec-hardening-2 H-7, S7). False when the step was already spent (or an earlier one
// was not), or the user does not exist.
func (s *Store) AcceptTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_last_step = $2
		  WHERE id = $1 AND (totp_last_step IS NULL OR totp_last_step < $2)`, userID, step)
	if err != nil {
		return false, fmt.Errorf("store: accept totp step: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DisableTOTP clears the secret, disables 2FA, and drops the user's recovery codes.
func (s *Store) DisableTOTP(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin disable totp: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `UPDATE users SET totp_secret = '', totp_enabled = false, totp_last_step = NULL, updated_at = now() WHERE id = $1`, userID); err != nil {
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
	if err := replaceRecoveryCodes(ctx, tx, userID, hashes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func replaceRecoveryCodes(ctx context.Context, tx pgx.Tx, userID string, hashes []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM totp_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: clear recovery codes: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `INSERT INTO totp_recovery_codes (user_id, code_hash) VALUES ($1, $2)`, userID, h); err != nil {
			return fmt.Errorf("store: insert recovery code: %w", err)
		}
	}
	return nil
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
