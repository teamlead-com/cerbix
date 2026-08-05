package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// API-token secrets are hashed with the same HashToken (hex SHA-256) used for
// session tokens — both are high-entropy random secrets, not passwords.

const apiTokenColumns = "id, org_id, project_id, name, role, created_by, last_used_at, created_at"

func scanApiToken(row pgx.Row) (domain.ApiToken, error) {
	var (
		t        domain.ApiToken
		proj     *string
		lastUsed *time.Time
	)
	if err := row.Scan(&t.ID, &t.OrgID, &proj, &t.Name, &t.Role, &t.CreatedBy, &lastUsed, &t.CreatedAt); err != nil {
		return domain.ApiToken{}, err
	}
	if proj != nil {
		t.ProjectID = *proj
	}
	t.LastUsedAt = lastUsed
	return t, nil
}

// CreateApiToken inserts a token, storing only the supplied hash of the secret.
func (s *Store) CreateApiToken(ctx context.Context, t domain.ApiToken, hash string) (domain.ApiToken, error) {
	if err := t.Validate(); err != nil {
		return domain.ApiToken{}, fmt.Errorf("store: invalid api token: %w", err)
	}
	var projID *string
	if t.ProjectID != "" {
		projID = &t.ProjectID
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO api_tokens (org_id, project_id, name, role, token_hash, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+apiTokenColumns,
		t.OrgID, projID, t.Name, t.Role, hash, t.CreatedBy)
	created, err := scanApiToken(row)
	if err != nil {
		return domain.ApiToken{}, fmt.Errorf("store: create api token: %w", err)
	}
	return created, nil
}

// ApiTokenByHash looks up a token by its secret hash, or ErrNotFound.
func (s *Store) ApiTokenByHash(ctx context.Context, hash string) (domain.ApiToken, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE token_hash = $1`, hash)
	t, err := scanApiToken(row)
	if noRows(err) {
		return domain.ApiToken{}, ErrNotFound
	}
	if err != nil {
		return domain.ApiToken{}, fmt.Errorf("store: api token by hash: %w", err)
	}
	return t, nil
}

// TouchApiToken records that a token was used just now. Best-effort: a missing
// token is not an error to the caller path.
func (s *Store) TouchApiToken(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: touch api token: %w", err)
	}
	return nil
}

// ListApiTokensByOrg lists an org's tokens (no secrets), newest first.
func (s *Store) ListApiTokensByOrg(ctx context.Context, orgID string) ([]domain.ApiToken, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list api tokens: %w", err)
	}
	defer rows.Close()
	var out []domain.ApiToken
	for rows.Next() {
		t, err := scanApiToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan api token: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate api tokens: %w", err)
	}
	return out, nil
}

// GetApiToken returns a token by id, or ErrNotFound.
func (s *Store) GetApiToken(ctx context.Context, id string) (domain.ApiToken, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE id = $1`, id)
	t, err := scanApiToken(row)
	if noRows(err) {
		return domain.ApiToken{}, ErrNotFound
	}
	if err != nil {
		return domain.ApiToken{}, fmt.Errorf("store: get api token: %w", err)
	}
	return t, nil
}

// DeleteApiToken revokes a token.
func (s *Store) DeleteApiToken(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete api token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
