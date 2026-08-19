package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// RecordAudit appends an audit entry. An empty ActorUserID is stored as NULL
// (e.g. a machine actor without a resolved user); an empty OrgID too —
// instance-level actions of a global admin have no organization.
func (s *Store) RecordAudit(ctx context.Context, e domain.AuditEntry) error {
	var actor, org *string
	if e.ActorUserID != "" {
		actor = &e.ActorUserID
	}
	if e.OrgID != "" {
		org = &e.OrgID
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 VALUES ($1, $2, $3, $4, $5)`,
		org, actor, e.ViaToken, e.Action, e.Target)
	if err != nil {
		return fmt.Errorf("store: record audit: %w", err)
	}
	return nil
}

// auditListSQL is the one read shape both listings share. The only difference is the WHERE
// clause, so the enrichment join, the ordering and the bound cannot drift between an org's
// view and the instance's.
const auditListSQL = `
	SELECT a.id, a.org_id, a.actor_user_id, a.via_token, a.action, a.target, a.created_at,
	       u.email, u.display_name
	  FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_user_id
	 WHERE %s
	 ORDER BY a.created_at DESC
	 LIMIT $%d`

// auditLimit clamps a caller's limit into the same window for every audit read.
func auditLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

// ListAuditByOrg returns an org's audit entries, newest first (bounded by limit),
// enriched with the actor's current identity.
func (s *Store) ListAuditByOrg(ctx context.Context, orgID string, limit int) ([]domain.AuditEntry, error) {
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(auditListSQL, "a.org_id = $1", 2), orgID, auditLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

// ListGlobalAudit returns the INSTANCE-level entries — the ones a global admin's actions leave,
// which carry no organization (`RecordAudit` stores an empty OrgID as NULL). They are deliberately
// a separate read rather than an org listing with a wider filter: an org admin must never see
// instance-level history, and a query that could be widened by a parameter is one authz bug away
// from doing exactly that.
func (s *Store) ListGlobalAudit(ctx context.Context, limit int) ([]domain.AuditEntry, error) {
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(auditListSQL, "a.org_id IS NULL", 1), auditLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list global audit: %w", err)
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

// scanAuditEntries reads the shared row shape. `org_id` is scanned as NULLABLE because an
// instance-level entry has none — scanning it into a plain string would fail on exactly the rows
// this listing exists for.
func scanAuditEntries(rows pgx.Rows) ([]domain.AuditEntry, error) {
	var out []domain.AuditEntry
	for rows.Next() {
		var (
			e                       domain.AuditEntry
			org, actor, email, name *string
		)
		if err := rows.Scan(&e.ID, &org, &actor, &e.ViaToken, &e.Action, &e.Target, &e.CreatedAt, &email, &name); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		if org != nil {
			e.OrgID = *org
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
