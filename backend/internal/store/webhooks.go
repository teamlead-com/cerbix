package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const webhookColumns = "id, org_id, project_id, url, secret, enabled, created_by, created_at"

// scanWebhook scans a row and decrypts the signing secret (a no-op for plaintext
// or when encryption is disabled).
func (s *Store) scanWebhook(row pgx.Row) (domain.Webhook, error) {
	var (
		h    domain.Webhook
		proj *string
	)
	if err := row.Scan(&h.ID, &h.OrgID, &proj, &h.URL, &h.Secret, &h.Enabled, &h.CreatedBy, &h.CreatedAt); err != nil {
		return domain.Webhook{}, err
	}
	if proj != nil {
		h.ProjectID = *proj
	}
	secretPlain, err := s.cipher.Decrypt(h.Secret)
	if err != nil {
		return domain.Webhook{}, fmt.Errorf("store: decrypt webhook secret: %w", err)
	}
	h.Secret = secretPlain
	return h, nil
}

// CreateWebhook inserts a webhook subscription (validated in domain). The signing
// secret is encrypted at rest when a cipher is configured.
func (s *Store) CreateWebhook(ctx context.Context, h domain.Webhook) (domain.Webhook, error) {
	if err := h.Validate(); err != nil {
		return domain.Webhook{}, fmt.Errorf("store: invalid webhook: %w", err)
	}
	var projID *string
	if h.ProjectID != "" {
		projID = &h.ProjectID
	}
	encSecret, err := s.cipher.Encrypt(h.Secret)
	if err != nil {
		return domain.Webhook{}, fmt.Errorf("store: encrypt webhook secret: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO webhooks (org_id, project_id, url, secret, enabled, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+webhookColumns,
		h.OrgID, projID, h.URL, encSecret, h.Enabled, h.CreatedBy)
	created, err := s.scanWebhook(row)
	if err != nil {
		return domain.Webhook{}, fmt.Errorf("store: create webhook: %w", err)
	}
	return created, nil
}

// GetWebhook returns a webhook by id, or ErrNotFound.
func (s *Store) GetWebhook(ctx context.Context, id string) (domain.Webhook, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+webhookColumns+` FROM webhooks WHERE id = $1`, id)
	h, err := s.scanWebhook(row)
	if noRows(err) {
		return domain.Webhook{}, ErrNotFound
	}
	if err != nil {
		return domain.Webhook{}, fmt.Errorf("store: get webhook: %w", err)
	}
	return h, nil
}

// ListWebhooksByOrg lists an org's webhooks, newest first.
func (s *Store) ListWebhooksByOrg(ctx context.Context, orgID string) ([]domain.Webhook, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks: %w", err)
	}
	defer rows.Close()
	return s.collectWebhooks(rows)
}

// ListEnabledWebhooksForProject returns the enabled webhooks that apply to an
// incident in the given project: project-scoped hooks for that project, plus
// org-wide hooks (project_id NULL) of the project's organization.
func (s *Store) ListEnabledWebhooksForProject(ctx context.Context, projectID string) ([]domain.Webhook, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+webhookColumns+` FROM webhooks w
		 WHERE w.enabled AND (
		   w.project_id = $1
		   OR (w.project_id IS NULL AND w.org_id = (SELECT org_id FROM projects WHERE id = $1))
		 )
		 ORDER BY w.created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled webhooks: %w", err)
	}
	defer rows.Close()
	return s.collectWebhooks(rows)
}

func (s *Store) collectWebhooks(rows pgx.Rows) ([]domain.Webhook, error) {
	var out []domain.Webhook
	for rows.Next() {
		h, err := s.scanWebhook(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan webhook: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate webhooks: %w", err)
	}
	return out, nil
}

// DeleteWebhook removes a webhook subscription.
func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhooks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete webhook: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
