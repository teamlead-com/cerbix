-- +goose Up
-- Alert confirmations: require N consecutive failed checks before a monitor is
-- declared down (and thus alerts/opens an incident). Default 1 preserves the
-- current immediate behavior. consecutive_failures is the live counter the
-- ingest pipeline maintains.

ALTER TABLE monitors ADD COLUMN IF NOT EXISTS failure_threshold   int NOT NULL DEFAULT 1;
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS consecutive_failures int NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE monitors DROP COLUMN IF EXISTS failure_threshold;
ALTER TABLE monitors DROP COLUMN IF EXISTS consecutive_failures;
