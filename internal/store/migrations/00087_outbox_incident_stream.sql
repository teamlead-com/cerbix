-- +goose Up
-- The index behind D-0177's causal claim.
--
-- The claim asks, for every candidate `incident_event` row, whether an EARLIER event of the same
-- incident is still undelivered — and refuses to hand out the successor if one is. Without an index
-- that question is a sequential scan of the outbox on every poll, at the exact moment the worker is
-- trying to be quick.
--
-- Partial on the undelivered rows, because that is the only set the question is ever asked about: a
-- delivered predecessor imposes no order on anything.
CREATE INDEX outbox_incident_stream_idx
    ON outbox_events ((payload -> 'incident' ->> 'id'), ((payload ->> 'seq')::bigint))
 WHERE topic = 'incident_event' AND status <> 'delivered';

-- +goose Down
DROP INDEX IF EXISTS outbox_incident_stream_idx;
