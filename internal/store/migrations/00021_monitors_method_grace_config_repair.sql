-- +goose Up
-- Repair: add monitors.method / grace_seconds / config on databases that were
-- migrated before these columns were part of 00003_monitors.sql. Idempotent
-- (IF NOT EXISTS), so it is a no-op on fresh databases that already have them.
-- These columns are read by every monitor query (list/scan), so a database
-- missing them fails list_enabled_monitors / stale_push_monitors at runtime.

ALTER TABLE monitors ADD COLUMN IF NOT EXISTS method        text  NOT NULL DEFAULT 'GET';
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS grace_seconds int   NOT NULL DEFAULT 0;
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS config        jsonb NOT NULL DEFAULT '{}';

-- +goose Down
-- Intentionally a no-op: these columns belong to the base monitors schema
-- (00003); dropping them here would corrupt a correctly-migrated database.
SELECT 1;
