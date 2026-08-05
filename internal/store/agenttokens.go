package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const agentTokenColumns = "id, name, region, created_at, revoked_at"

// CreateAgentToken inserts a pull-agent token, storing only the supplied hash.
func (s *Store) CreateAgentToken(ctx context.Context, name, region, hash string) (domain.AgentToken, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO agent_tokens (name, region, token_hash) VALUES ($1,$2,$3) RETURNING `+agentTokenColumns,
		name, region, hash)
	return scanAgentToken(row)
}

// ResolveAgentTokenRegion returns the region a live (non-revoked) token authorizes.
func (s *Store) ResolveAgentTokenRegion(ctx context.Context, hash string) (region string, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT region FROM agent_tokens WHERE token_hash = $1 AND revoked_at IS NULL`, hash).Scan(&region)
	if noRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: resolve agent token: %w", err)
	}
	return region, true, nil
}

// ListAgentTokens returns all agent tokens (newest first), without the secret.
func (s *Store) ListAgentTokens(ctx context.Context) ([]domain.AgentToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentTokenColumns+` FROM agent_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list agent tokens: %w", err)
	}
	defer rows.Close()
	var out []domain.AgentToken
	for rows.Next() {
		t, err := scanAgentToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan agent token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAgentToken marks a token revoked (idempotent). ErrNotFound if it does not exist.
func (s *Store) RevokeAgentToken(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE agent_tokens SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: revoke agent token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanAgentToken(row pgx.Row) (domain.AgentToken, error) {
	var t domain.AgentToken
	if err := row.Scan(&t.ID, &t.Name, &t.Region, &t.CreatedAt, &t.RevokedAt); err != nil {
		return domain.AgentToken{}, err
	}
	return t, nil
}
