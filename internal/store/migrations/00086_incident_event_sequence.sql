-- +goose Up
-- The ordering fence for `incident_event`, which `service_alert` has had since FR-021 §16.5 and this
-- topic never did.
--
-- The outbox is at-least-once and its claim is unordered by construction: `UPDATE … RETURNING` has no
-- row order, several workers claim concurrently, and a retry re-delivers minutes later. So a
-- subscriber could be told an incident RESOLVED and then, from a retried older row, that it OPENED —
-- a sequence the product never had, describing an outage as beginning after it ended.
--
-- One counter per incident, advanced by every path that enqueues a lifecycle event for it, and
-- stamped into the payload. Delivery compares the two: a stale ONSET is dropped, and a resolution
-- never is (§16.5's polarity, for the same reason — an ending that cannot be delivered leaves people
-- watching something that finished).
ALTER TABLE incidents ADD COLUMN event_seq bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE incidents DROP COLUMN IF EXISTS event_seq;
