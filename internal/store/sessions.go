package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Session is a server-side session. The raw token is never stored; only its
// SHA-256 hash is persisted, and the raw token lives in the client cookie.
type Session struct {
	ID        string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// HashToken returns the hex SHA-256 of a raw session token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession persists a session for userID, keyed by the hash of the raw token.
func (s *Store) CreateSession(ctx context.Context, userID, token string, expiresAt time.Time) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)
		 RETURNING id, user_id, created_at, expires_at`,
		HashToken(token), userID, expiresAt).
		Scan(&sess.ID, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("store: create session: %w", err)
	}
	return sess, nil
}

// SessionByToken returns a non-expired session for the raw token, or ErrNotFound.
func (s *Store) SessionByToken(ctx context.Context, token string) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, created_at, expires_at FROM sessions
		 WHERE token_hash = $1 AND expires_at > now()`,
		HashToken(token)).
		Scan(&sess.ID, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt)
	if noRows(err) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: session by token: %w", err)
	}
	return sess, nil
}

// DeleteSession removes the session for the raw token (idempotent).
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, HashToken(token)); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// DeleteSessionsByUser removes every session for a user EXCEPT the one whose raw
// token is exceptToken (pass "" to drop all). Called on a password change/reset so a
// stolen or stale session can't outlive the credential rotation, while the actor's
// current session stays valid. Returns the number of sessions removed.
func (s *Store) DeleteSessionsByUser(ctx context.Context, userID, exceptToken string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		// A blank exceptToken hashes to a value no real token matches, so the
		// `<>` predicate keeps nothing — i.e. all of the user's sessions are dropped.
		`DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2`, userID, HashToken(exceptToken))
	if err != nil {
		return 0, fmt.Errorf("store: delete sessions by user: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredSessions removes sessions past their expiry and returns the count.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
