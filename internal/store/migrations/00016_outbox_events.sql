-- +goose Up
-- Transactional outbox: events are written in the same transaction as the state
-- change that produced them (incident create/update, monitor status transition),
-- then delivered by a background worker with retry/backoff. This replaces the
-- best-effort in-memory notification/webhook queues so events survive restarts.
CREATE TABLE outbox_events (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    topic           text        NOT NULL,
    payload         jsonb       NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',   -- pending | delivered | dead
    attempts        int         NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_events_topic_check  CHECK (topic IN ('incident_event','monitor_transition')),
    CONSTRAINT outbox_events_status_check CHECK (status IN ('pending','delivered','dead'))
);

-- Drives the claim query: due pending rows, oldest first.
CREATE INDEX outbox_events_due_idx ON outbox_events (next_attempt_at)
    WHERE status = 'pending';

-- +goose Down
DROP TABLE outbox_events;
