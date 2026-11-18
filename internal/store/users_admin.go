package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// ListAllUsers returns every user of the instance (global-admin surface),
// user-keyed with the memberships aggregated per user — users outside any
// organization come back with an empty memberships list. q filters by a
// case-insensitive substring of email or display name.
func (s *Store) ListAllUsers(ctx context.Context, q string) ([]domain.AdminUser, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT u.id, COALESCE(u.oidc_sub, ''), u.email, u.display_name, u.is_global_admin,
		        u.password_hash IS NOT NULL, u.created_at, u.updated_at,
		        u.last_seen_at, u.kind
		   FROM users u
		  WHERE $1 = '' OR u.email ILIKE '%' || $1 || '%' OR u.display_name ILIKE '%' || $1 || '%'
		  ORDER BY u.created_at, u.email`, q)
	if err != nil {
		return nil, fmt.Errorf("store: list all users: %w", err)
	}
	defer rows.Close()
	var out []domain.AdminUser
	index := map[string]int{}
	for rows.Next() {
		var (
			u        domain.AdminUser
			hasLocal bool
			lastSeen *time.Time
		)
		if err := rows.Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.IsGlobalAdmin,
			&hasLocal, &u.CreatedAt, &u.UpdatedAt, &lastSeen, &u.Kind); err != nil {
			return nil, fmt.Errorf("store: scan user: %w", err)
		}
		switch {
		case hasLocal && u.OIDCSub != "":
			u.AuthType = "both"
		case u.OIDCSub != "":
			u.AuthType = "oidc"
		case hasLocal:
			u.AuthType = "local"
		default:
			u.AuthType = "none"
		}
		u.LastActiveAt = lastSeen
		u.Memberships = []domain.AdminUserMembership{}
		index[u.ID] = len(out)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate users: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	mrows, err := s.pool.Query(ctx,
		`SELECT m.user_id, m.org_id, o.name, COALESCE(m.project_id::text, ''), COALESCE(p.name, ''), m.role
		   FROM memberships m
		   JOIN organizations o ON o.id = m.org_id
		   LEFT JOIN projects p ON p.id = m.project_id
		  ORDER BY o.name, m.project_id NULLS FIRST`)
	if err != nil {
		return nil, fmt.Errorf("store: list user memberships: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var (
			userID string
			m      domain.AdminUserMembership
			role   string
		)
		if err := mrows.Scan(&userID, &m.OrgID, &m.OrgName, &m.ProjectID, &m.ProjectName, &role); err != nil {
			return nil, fmt.Errorf("store: scan user membership: %w", err)
		}
		m.Role = domain.Role(role)
		if i, ok := index[userID]; ok {
			out[i].Memberships = append(out[i].Memberships, m)
		}
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate user memberships: %w", err)
	}
	return out, nil
}

// deleteUserAttempts bounds DeleteUser's restarts when a channel naming the user is held by a writer
// that may be waiting for the user in turn (see deleteUserOnce).
const deleteUserAttempts = 5

// DeleteUser removes a user; memberships, sessions, TOTP state and reset
// tokens go with it via ON DELETE CASCADE, audit entries keep a NULL actor.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	for attempt := 1; ; attempt++ {
		err := s.deleteUserOnce(ctx, id)
		if !isLockNotAvailable(err) || attempt == deleteUserAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 20 * time.Millisecond):
		}
	}
}

// deleteUserOnce is one attempt of DeleteUser, in one transaction.
//
// FIRST the user's phone channels (FR-041a §4.3): in the other order the foreign key has already set
// recipient_user_id NULL and this finds no row. The channel survives, disabled, with the reason as
// its trace.
//
// A create, retarget or edit of a `user` channel takes the channel's row lock, then KEY SHARE on the
// person it names (recipientEligibleTx). So: disable the channels already committed, waiting for
// their row locks in the writers' order (channel, then user); then lock the user FOR UPDATE, which
// waits out every such writer in flight; then disable again, to catch what they committed. A writer
// that comes after the user lock finds no user and is refused as recipient_not_eligible.
//
// The second pass runs while the user is held, so it must not WAIT for a channel: a writer queued on
// a channel that was just retargeted to this user holds that channel and is about to ask for the
// user — a deadlock. Its locks are taken NOWAIT instead; on 55P03 the attempt rolls back, which
// releases the user, the writer finishes, and the next attempt's first pass waits for the channel in
// the ordinary order.
func (s *Store) deleteUserOnce(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin delete user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	disable := func() error {
		if _, err := tx.Exec(ctx, `UPDATE notification_channels SET enabled = false, disabled_reason = 'recipient_deleted'
			 WHERE recipient_user_id = $1`, id); err != nil {
			return fmt.Errorf("store: disable deleted user's channels: %w", err)
		}
		return nil
	}
	if err := disable(); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&exists); err != nil {
		if noRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: lock user: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM notification_channels WHERE recipient_user_id = $1
		 FOR NO KEY UPDATE NOWAIT`, id); err != nil {
		return fmt.Errorf("store: lock deleted user's channels: %w", err)
	}
	if err := disable(); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit delete user: %w", err)
	}
	return nil
}

// isLockNotAvailable reports a NOWAIT lock that was held by another transaction (55P03).
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// CountGlobalAdmins reports how many users hold the global-admin flag —
// the guard against demoting or deleting the last one.
func (s *Store) CountGlobalAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_global_admin`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count global admins: %w", err)
	}
	return n, nil
}
