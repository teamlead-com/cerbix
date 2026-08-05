package store

import (
	"context"
	"fmt"
	"time"
)

// AuthFlow is a transient OIDC login flow, keyed by the OAuth2 state parameter.
type AuthFlow struct {
	State        string
	Nonce        string
	PKCEVerifier string
	RedirectTo   string
	ExpiresAt    time.Time
}

// CreateAuthFlow stores a pending login flow.
func (s *Store) CreateAuthFlow(ctx context.Context, f AuthFlow) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO auth_flows (state, nonce, pkce_verifier, redirect_to, expires_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		f.State, f.Nonce, f.PKCEVerifier, f.RedirectTo, f.ExpiresAt)
	if err != nil {
		return fmt.Errorf("store: create auth flow: %w", err)
	}
	return nil
}

// TakeAuthFlow atomically consumes a non-expired flow by state (delete + return),
// so a state can be used at most once. Returns ErrNotFound if missing or expired.
func (s *Store) TakeAuthFlow(ctx context.Context, state string) (AuthFlow, error) {
	var f AuthFlow
	err := s.pool.QueryRow(ctx,
		`DELETE FROM auth_flows WHERE state = $1 AND expires_at > now()
		 RETURNING state, nonce, pkce_verifier, redirect_to, expires_at`,
		state).
		Scan(&f.State, &f.Nonce, &f.PKCEVerifier, &f.RedirectTo, &f.ExpiresAt)
	if noRows(err) {
		return AuthFlow{}, ErrNotFound
	}
	if err != nil {
		return AuthFlow{}, fmt.Errorf("store: take auth flow: %w", err)
	}
	return f, nil
}

// DeleteExpiredAuthFlows removes stale flows and returns the count.
func (s *Store) DeleteExpiredAuthFlows(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_flows WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired auth flows: %w", err)
	}
	return tag.RowsAffected(), nil
}
