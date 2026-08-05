package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// enqueueOutboxTx inserts a pending outbox event inside an existing transaction,
// so the event is durable iff the state change that produced it commits. Payload
// must be valid JSON.
func enqueueOutboxTx(ctx context.Context, tx pgx.Tx, topic string, payload []byte) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (topic, payload) VALUES ($1, $2)`,
		topic, string(payload)); err != nil {
		return fmt.Errorf("store: enqueue outbox: %w", err)
	}
	return nil
}

// EnqueueOutbox queues a standalone outbound event that isn't tied to a store
// mutation (so it can't ride an existing transaction) — e.g. a subscriber
// confirmation email queued straight from an API handler. Payload must be valid
// JSON; delivery is handled by the outbox worker with retry/backoff.
func (s *Store) EnqueueOutbox(ctx context.Context, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox_events (topic, payload) VALUES ($1, $2)`,
		topic, string(payload)); err != nil {
		return fmt.Errorf("store: enqueue outbox: %w", err)
	}
	return nil
}

// ClaimDueOutbox atomically claims up to limit due pending events for delivery.
// It locks candidate rows with FOR UPDATE SKIP LOCKED (so concurrent workers and
// replicas never claim the same row), increments attempts, and pushes
// next_attempt_at forward by an exponential backoff — that doubles as a lease, so
// a worker that crashes mid-delivery leaves the row to become due again on its
// own. The delivery itself happens outside any row lock.
func (s *Store) ClaimDueOutbox(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE outbox_events
		   SET attempts = attempts + 1,
		       next_attempt_at = now() + least(
		           interval '1 hour',
		           interval '10 seconds' * power(2, least(attempts, 12))),
		       updated_at = now()
		 WHERE id IN (
		     SELECT id FROM outbox_events
		      WHERE status = 'pending' AND next_attempt_at <= now()
		      ORDER BY next_attempt_at
		      FOR UPDATE SKIP LOCKED
		      LIMIT $1)
		 RETURNING id, topic, payload, attempts`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: claim outbox: %w", err)
	}
	defer rows.Close()
	var out []domain.OutboxEvent
	for rows.Next() {
		var e domain.OutboxEvent
		if err := rows.Scan(&e.ID, &e.Topic, &e.Payload, &e.Attempts); err != nil {
			return nil, fmt.Errorf("store: scan outbox: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate outbox: %w", err)
	}
	return out, nil
}

// MarkOutboxDelivered marks an event delivered (terminal success).
func (s *Store) MarkOutboxDelivered(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE outbox_events SET status = 'delivered', last_error = '', updated_at = now() WHERE id = $1`,
		id); err != nil {
		return fmt.Errorf("store: mark outbox delivered: %w", err)
	}
	return nil
}

// ListDeadOutbox returns dead-lettered events (delivery gave up after max
// attempts), newest failure first, for operator inspection.
func (s *Store) ListDeadOutbox(ctx context.Context, limit int) ([]domain.OutboxEventView, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, topic, status, attempts, last_error, payload, created_at, updated_at
		  FROM outbox_events
		 WHERE status = 'dead'
		 ORDER BY updated_at DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list dead outbox: %w", err)
	}
	defer rows.Close()
	var out []domain.OutboxEventView
	for rows.Next() {
		var e domain.OutboxEventView
		if err := rows.Scan(&e.ID, &e.Topic, &e.Status, &e.Attempts, &e.LastError, &e.Payload, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan dead outbox: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate dead outbox: %w", err)
	}
	return out, nil
}

// ReplayDeadOutbox requeues a single dead event: it resets to pending with a
// cleared attempt count so the worker delivers it again. ErrNotFound if there is
// no dead event with that id.
func (s *Store) ReplayDeadOutbox(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		   SET status = 'pending', attempts = 0, next_attempt_at = now(), last_error = '', updated_at = now()
		 WHERE id = $1 AND status = 'dead'`, id)
	if err != nil {
		return fmt.Errorf("store: replay dead outbox: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplayAllDeadOutbox requeues every dead event and returns how many were reset.
func (s *Store) ReplayAllDeadOutbox(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		   SET status = 'pending', attempts = 0, next_attempt_at = now(), last_error = '', updated_at = now()
		 WHERE status = 'dead'`)
	if err != nil {
		return 0, fmt.Errorf("store: replay all dead outbox: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// FailOutbox records a failed delivery attempt. The row stays pending (it becomes
// due again at its leased next_attempt_at) until attempts reaches maxAttempts, at
// which point it is parked as dead for operator inspection. attempts was already
// incremented by ClaimDueOutbox.
func (s *Store) FailOutbox(ctx context.Context, id, lastErr string, maxAttempts int) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE outbox_events
		    SET status = CASE WHEN attempts >= $3 THEN 'dead' ELSE 'pending' END,
		        last_error = $2,
		        updated_at = now()
		  WHERE id = $1`,
		id, lastErr, maxAttempts); err != nil {
		return fmt.Errorf("store: fail outbox: %w", err)
	}
	return nil
}
