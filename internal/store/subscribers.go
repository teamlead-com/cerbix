package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const subscriberColumns = "id, status_page_id, email, confirm_token, confirmed_at, created_at"

func scanSubscriber(row pgx.Row) (domain.Subscriber, error) {
	var (
		s           domain.Subscriber
		confirmedAt *time.Time
	)
	if err := row.Scan(&s.ID, &s.StatusPageID, &s.Email, &s.ConfirmToken, &confirmedAt, &s.CreatedAt); err != nil {
		return domain.Subscriber{}, err
	}
	s.ConfirmedAt = confirmedAt
	return s, nil
}

// CreateSubscriber inserts a subscriber (unconfirmed) or, if the email is already
// subscribed to the page, handles a re-subscribe: it re-issues the confirm token ONLY
// while still unconfirmed (so a fresh confirmation link is sent), but for an
// already-confirmed subscriber it KEEPS the existing token — that same token is the
// unsubscribe link, and silently rotating it would break the subscriber's existing
// unsubscribe URL (no new email is sent in that case). The returned row's ConfirmedAt
// tells the caller whether to send a confirmation email.
func (s *Store) CreateSubscriber(ctx context.Context, sub domain.Subscriber) (domain.Subscriber, error) {
	if err := sub.Validate(); err != nil {
		return domain.Subscriber{}, fmt.Errorf("store: invalid subscriber: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO subscribers (status_page_id, email, confirm_token)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (status_page_id, email)
		 DO UPDATE SET confirm_token = CASE
		     WHEN subscribers.confirmed_at IS NULL THEN EXCLUDED.confirm_token
		     ELSE subscribers.confirm_token
		   END
		 RETURNING `+subscriberColumns,
		sub.StatusPageID, sub.Email, sub.ConfirmToken)
	created, err := scanSubscriber(row)
	if err != nil {
		return domain.Subscriber{}, fmt.Errorf("store: create subscriber: %w", err)
	}
	return created, nil
}

// ConfirmSubscriber marks a subscriber confirmed by token (idempotent), returning
// the row. ErrNotFound for an unknown token.
func (s *Store) ConfirmSubscriber(ctx context.Context, token string) (domain.Subscriber, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE subscribers SET confirmed_at = COALESCE(confirmed_at, now())
		  WHERE confirm_token = $1 RETURNING `+subscriberColumns, token)
	sub, err := scanSubscriber(row)
	if noRows(err) {
		return domain.Subscriber{}, ErrNotFound
	}
	if err != nil {
		return domain.Subscriber{}, fmt.Errorf("store: confirm subscriber: %w", err)
	}
	return sub, nil
}

// DeleteSubscriberByToken unsubscribes by token. ErrNotFound if the token is
// unknown.
func (s *Store) DeleteSubscriberByToken(ctx context.Context, token string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM subscribers WHERE confirm_token = $1`, token)
	if err != nil {
		return fmt.Errorf("store: delete subscriber: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ConfirmedSubscriberEmailsForProject lists the distinct confirmed subscriber emails across every
// status page that REPORTS this project — the recipients for that project's incident emails.
//
// It is the INVERSE of `StatusPageProjectIDs` and must stay so: a subscriber is entitled to mail
// about exactly the incidents their page shows them. The old spelling was `components → monitors`,
// which is neither half of that axis. It missed the page's own project, and it missed every
// service-backed component — so a page made only of Service components rendered the incident and
// emailed nobody, which is the worst of both.
func (s *Store) ConfirmedSubscriberEmailsForProject(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT sub.email
		  FROM subscribers sub
		  JOIN status_pages sp ON sp.id = sub.status_page_id
		 WHERE sub.confirmed_at IS NOT NULL
		   AND (sp.project_id = $1
		        OR EXISTS (SELECT 1 FROM components c
		                    WHERE c.status_page_id = sp.id AND c.source_project = $1))`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: subscriber emails for project: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("store: scan subscriber email: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subscriber emails: %w", err)
	}
	return out, nil
}

// ListSubscribersByPage returns a page's subscribers, newest first — the
// owner-facing view (confirmed and pending alike).
func (s *Store) ListSubscribersByPage(ctx context.Context, pageID string) ([]domain.Subscriber, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+subscriberColumns+` FROM subscribers WHERE status_page_id = $1 ORDER BY created_at DESC`, pageID)
	if err != nil {
		return nil, fmt.Errorf("store: list subscribers: %w", err)
	}
	defer rows.Close()
	var out []domain.Subscriber
	for rows.Next() {
		sub, err := scanSubscriber(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan subscriber: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subscribers: %w", err)
	}
	return out, nil
}

// DeleteSubscriber removes a subscriber of the given page (the page id guards
// against cross-page deletion by id).
func (s *Store) DeleteSubscriber(ctx context.Context, pageID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM subscribers WHERE id = $1 AND status_page_id = $2`, id, pageID)
	if err != nil {
		return fmt.Errorf("store: delete subscriber: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
