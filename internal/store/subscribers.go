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
// subscribed to the page, re-issues its confirm token (keeping any prior
// confirmation). The returned row's ConfirmedAt tells the caller whether to send
// a confirmation email.
func (s *Store) CreateSubscriber(ctx context.Context, sub domain.Subscriber) (domain.Subscriber, error) {
	if err := sub.Validate(); err != nil {
		return domain.Subscriber{}, fmt.Errorf("store: invalid subscriber: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO subscribers (status_page_id, email, confirm_token)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (status_page_id, email)
		 DO UPDATE SET confirm_token = EXCLUDED.confirm_token
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

// ConfirmedSubscriberEmailsForProject lists the distinct confirmed subscriber
// emails across every status page that includes a component tied to a monitor in
// the given project — the recipients for that project's incident emails.
func (s *Store) ConfirmedSubscriberEmailsForProject(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT sub.email
		  FROM subscribers sub
		  JOIN components c ON c.status_page_id = sub.status_page_id
		  JOIN monitors m ON m.id = c.monitor_id
		 WHERE m.project_id = $1 AND sub.confirmed_at IS NOT NULL`, projectID)
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
