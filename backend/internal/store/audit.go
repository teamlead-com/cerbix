package store

import (
	"context"
	"fmt"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// RecordAudit appends an audit entry. An empty ActorUserID is stored as NULL
// (e.g. a machine actor without a resolved user).
func (s *Store) RecordAudit(ctx context.Context, e domain.AuditEntry) error {
	var actor *string
	if e.ActorUserID != "" {
		actor = &e.ActorUserID
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 VALUES ($1, $2, $3, $4, $5)`,
		e.OrgID, actor, e.ViaToken, e.Action, e.Target)
	if err != nil {
		return fmt.Errorf("store: record audit: %w", err)
	}
	return nil
}

// ListAuditByOrg returns an org's audit entries, newest first (bounded by limit),
// enriched with the actor's current identity.
func (s *Store) ListAuditByOrg(ctx context.Context, orgID string, limit int) ([]domain.AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.org_id, a.actor_user_id, a.via_token, a.action, a.target, a.created_at,
		        u.email, u.display_name
		   FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_user_id
		  WHERE a.org_id = $1
		  ORDER BY a.created_at DESC
		  LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	var out []domain.AuditEntry
	for rows.Next() {
		var (
			e            domain.AuditEntry
			actor, email *string
			name         *string
		)
		if err := rows.Scan(&e.ID, &e.OrgID, &actor, &e.ViaToken, &e.Action, &e.Target, &e.CreatedAt, &email, &name); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		if actor != nil {
			e.ActorUserID = *actor
		}
		if email != nil {
			e.ActorEmail = *email
		}
		if name != nil {
			e.ActorName = *name
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate audit: %w", err)
	}
	return out, nil
}
