-- +goose Up
-- Confirmation-phase acceleration (D-0099): while a failure is being confirmed
-- (first fail .. verdict) the scheduler probes at confirm_interval_seconds
-- instead of interval_seconds, cutting time-to-alert from interval*threshold to
-- seconds. 0 disables; effective only with failure_threshold > 1. Existing
-- monitors get the 10s default — a no-op until their threshold exceeds 1.
ALTER TABLE monitors ADD COLUMN confirm_interval_seconds integer NOT NULL DEFAULT 10;

-- +goose Down
ALTER TABLE monitors DROP COLUMN confirm_interval_seconds;
