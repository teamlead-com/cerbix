-- +goose Up
-- Free-form labels on monitors (e.g. env:prod, team:payments, tier:1) for
-- filtering and grouping across the app. Stored as a text[] with a GIN index so
-- tag membership queries stay cheap as the fleet grows.

ALTER TABLE monitors ADD COLUMN IF NOT EXISTS tags text[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS monitors_tags_idx ON monitors USING gin (tags);

-- +goose Down
DROP INDEX IF EXISTS monitors_tags_idx;
ALTER TABLE monitors DROP COLUMN IF EXISTS tags;
