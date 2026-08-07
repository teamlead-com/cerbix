-- +goose Up
-- Per-monitor monotonic transition counter. Bumped in the same transaction as
-- each applied status flip and carried in the transition outbox event, so the
-- delivery worker can drop a stale DOWN (or reminder) that a newer transition has
-- already superseded — preventing a reordered/retried outbox event from firing a
-- down alert after the recovery was delivered.
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS state_sequence bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE monitors DROP COLUMN IF EXISTS state_sequence;
