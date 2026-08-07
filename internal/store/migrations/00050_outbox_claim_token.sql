-- +goose Up
-- Per-claim token for the outbox worker. ClaimDueOutbox stamps a fresh token on
-- each claim; MarkOutboxDelivered/FailOutbox then CAS on it, so a worker whose
-- lease expired mid-delivery (another worker re-claimed the row) can no longer
-- regress the row's terminal state — its stale update matches no token and is a
-- no-op. NULL until first claimed.
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS claim_token uuid;

-- +goose Down
ALTER TABLE outbox_events DROP COLUMN IF EXISTS claim_token;
