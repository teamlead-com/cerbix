package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// enqueueOutboxTx inserts a pending outbox event inside an existing transaction,
// so the event is durable iff the state change that produced it commits. Payload
// must be valid JSON. The claimable class comes from domain.FencedTopic — the
// one topic→class source of truth (FR-021 §14.3): a fenced topic's rows are
// 'pending_fenced', invisible to the legacy claim shape, and the fenced column
// is immutable from here on (the demotion CHECK forbids legacy 'pending' for
// them through every later transition).
func enqueueOutboxTx(ctx context.Context, tx pgx.Tx, topic string, payload []byte) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (topic, payload, status, fenced)
		 VALUES ($1, $2, CASE WHEN $3 THEN 'pending_fenced' ELSE 'pending' END, $3)`,
		topic, string(payload), domain.FencedTopic(topic)); err != nil {
		return fmt.Errorf("store: enqueue outbox: %w", err)
	}
	return nil
}

// enqueueOutboxAtTx is the same enqueue with the row's timestamps STATED rather than defaulted.
//
// The defaults are `now()`, which is the transaction's start. A writer that waited on a row lock
// therefore schedules its event as though it had been enqueued before the wait — and since
// `next_attempt_at` is what the claim orders by, an event caused by a later action can become due
// before the one it followed. Callers that already own a post-lock instant pass it here so the event
// is scheduled on the same clock as the rows that caused it. It is deliberately a separate function:
// changing the default for every topic would re-time paths that never take a row lock at all.
func enqueueOutboxAtTx(ctx context.Context, tx pgx.Tx, topic string, payload []byte, at time.Time) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (topic, payload, status, fenced, created_at, updated_at, next_attempt_at)
		 VALUES ($1, $2, CASE WHEN $3 THEN 'pending_fenced' ELSE 'pending' END, $3, $4, $4, $4)`,
		topic, string(payload), domain.FencedTopic(topic), at); err != nil {
		return fmt.Errorf("store: enqueue outbox at instant: %w", err)
	}
	return nil
}

// EnqueueOutbox queues a standalone outbound event that isn't tied to a store
// mutation (so it can't ride an existing transaction) — e.g. a subscriber
// confirmation email queued straight from an API handler. Payload must be valid
// JSON; delivery is handled by the outbox worker with retry/backoff. Same
// topic→class rule as enqueueOutboxTx.
func (s *Store) EnqueueOutbox(ctx context.Context, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox_events (topic, payload, status, fenced)
		 VALUES ($1, $2, CASE WHEN $3 THEN 'pending_fenced' ELSE 'pending' END, $3)`,
		topic, string(payload), domain.FencedTopic(topic)); err != nil {
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
	// Two claimable classes (FR-021 §14.3): legacy 'pending' rows unconditionally
	// (every deployed owner dispatches those topics), and 'pending_fenced' rows
	// only for topics THIS binary's worker dispatches — so a fenced row enqueued
	// by a newer producer is never claimed, attempt-burned or dead-lettered by an
	// owner that cannot handle it. It just waits for a capable one.
	rows, err := s.pool.Query(ctx, `
		UPDATE outbox_events
		   SET attempts = attempts + 1,
		       next_attempt_at = now() + least(
		           interval '1 hour',
		           interval '10 seconds' * power(2, least(attempts, 12))),
		       claim_token = gen_random_uuid(),
		       updated_at = now()
		 WHERE id IN (
		     SELECT o.id FROM outbox_events o
		      WHERE (o.status = 'pending' OR (o.status = 'pending_fenced' AND o.topic = ANY($2)))
		        AND o.next_attempt_at <= now()
		        -- D-0177's CAUSAL half. Dropping a superseded onset at delivery keeps a subscriber
		        -- from being told an outage began after it ended; it does not keep the two events in
		        -- order. This does: a successor is not handed out while an EARLIER event of the same
		        -- incident is undelivered, so one batch cannot carry both ends of a lifecycle and two
		        -- workers cannot race them.
		        --
		        -- A DEAD predecessor blocks too, deliberately. The alternative is delivering a
		        -- resolution whose opening was parked for an operator — an ending to an announcement
		        -- nobody received. The stream waits for the replay, and the dead row is the thing an
		        -- operator already looks at.
		        AND NOT EXISTS (
		            SELECT 1 FROM outbox_events p
		             WHERE p.topic = 'incident_event' AND o.topic = 'incident_event'
		               AND p.status <> 'delivered'
		               AND p.id <> o.id
		               AND p.payload -> 'incident' ->> 'id' = o.payload -> 'incident' ->> 'id'
		               AND COALESCE((p.payload ->> 'seq')::bigint, 0)
		                   < COALESCE((o.payload ->> 'seq')::bigint, 0))
		      ORDER BY o.next_attempt_at
		      FOR UPDATE SKIP LOCKED
		      LIMIT $1)
		 RETURNING id, topic, payload, attempts, claim_token, next_attempt_at, created_at`,
		limit, domain.FencedTopics())
	if err != nil {
		return nil, fmt.Errorf("store: claim outbox: %w", err)
	}
	defer rows.Close()
	var out []claimed
	for rows.Next() {
		var e domain.OutboxEvent
		var due, created time.Time
		if err := rows.Scan(&e.ID, &e.Topic, &e.Payload, &e.Attempts, &e.ClaimToken, &due, &created); err != nil {
			return nil, fmt.Errorf("store: scan outbox: %w", err)
		}
		out = append(out, claimed{e, due, created})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate outbox: %w", err)
	}
	// `UPDATE … RETURNING` has no defined row order: the ORDER BY inside the sub-select decides which
	// rows are CLAIMED, not the sequence they come back in, so a batch holding an incident's opening
	// and its resolution could hand them to the dispatcher either way round. Sorting here is what
	// makes one claim deliver in the order the rows became due — the cross-worker half of the same
	// question is the per-incident sequence gate in the outbox worker, which this does not replace.
	sort.Slice(out, func(i, j int) bool {
		switch {
		case !out[i].due.Equal(out[j].due):
			return out[i].due.Before(out[j].due)
		case !out[i].created.Equal(out[j].created):
			return out[i].created.Before(out[j].created)
		default:
			return out[i].OutboxEvent.ID < out[j].OutboxEvent.ID
		}
	})
	events := make([]domain.OutboxEvent, 0, len(out))
	for _, c := range out {
		events = append(events, c.OutboxEvent)
	}
	return events, nil
}

// claimed carries the two timestamps the claim orders by. They are not part of the domain event: the
// dispatcher has no business knowing when a row became due, only what to do with it.
type claimed struct {
	domain.OutboxEvent
	due     time.Time
	created time.Time
}

// MarkOutboxDelivered marks an event delivered (terminal success). The claimToken
// CAS ensures only the CURRENT claim owner can set the state: a stale worker whose
// lease expired (the row was re-claimed with a new token) updates zero rows and is
// silently ignored, so it can't overwrite the owner's result.
func (s *Store) MarkOutboxDelivered(ctx context.Context, id, claimToken string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE outbox_events SET status = 'delivered', last_error = '', updated_at = now()
		  WHERE id = $1 AND claim_token = $2`,
		id, claimToken)
	if err != nil {
		return false, fmt.Errorf("store: mark outbox delivered: %w", err)
	}
	// applied=false means the claim_token CAS matched nothing: another worker re-claimed
	// and owns this row now. The caller must not count it as this worker's delivery.
	return tag.RowsAffected() > 0, nil
}

// PurgeDeliveredOutbox deletes delivered outbox rows older than olderThan — housekeeping
// so the table doesn't grow without bound (delivered events are kept briefly for audit,
// then reclaimed). Dead-lettered rows are NEVER purged here (operators inspect/replay
// them). A non-positive olderThan falls back to 7 days. Returns rows deleted. Leader tick.
func (s *Store) PurgeDeliveredOutbox(ctx context.Context, olderThan time.Duration) (int, error) {
	secs := int(olderThan.Seconds())
	if secs <= 0 {
		secs = 7 * 24 * 3600
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM outbox_events WHERE status = 'delivered' AND updated_at < now() - make_interval(secs => $1)`,
		secs)
	if err != nil {
		return 0, fmt.Errorf("store: purge delivered outbox: %w", err)
	}
	return int(tag.RowsAffected()), nil
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
	// The claimable class is restored from the IMMUTABLE fenced column — a fenced
	// row replays as 'pending_fenced', never legacy 'pending' (the demotion CHECK
	// would reject that anyway; this is the write that stays inside the contract).
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		   SET status = CASE WHEN fenced THEN 'pending_fenced' ELSE 'pending' END,
		       attempts = 0, next_attempt_at = now(), last_error = '', updated_at = now()
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
	// Same class restoration as ReplayDeadOutbox, for every dead row at once.
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		   SET status = CASE WHEN fenced THEN 'pending_fenced' ELSE 'pending' END,
		       attempts = 0, next_attempt_at = now(), last_error = '', updated_at = now()
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
func (s *Store) FailOutbox(ctx context.Context, id, claimToken, lastErr string, maxAttempts int) (bool, error) {
	// Same claimToken CAS as MarkOutboxDelivered: a stale worker's failure must not
	// regress a row another worker already delivered (or re-claimed).
	tag, err := s.pool.Exec(ctx,
		`UPDATE outbox_events
		    SET status = CASE WHEN attempts >= $4 THEN 'dead'
		                      WHEN fenced THEN 'pending_fenced'
		                      ELSE 'pending' END,
		        last_error = $3,
		        updated_at = now()
		  WHERE id = $1 AND claim_token = $2`,
		id, claimToken, lastErr, maxAttempts)
	if err != nil {
		return false, fmt.Errorf("store: fail outbox: %w", err)
	}
	// applied=false → the CAS lost (row re-claimed elsewhere); don't count a dead-letter.
	return tag.RowsAffected() > 0, nil
}
