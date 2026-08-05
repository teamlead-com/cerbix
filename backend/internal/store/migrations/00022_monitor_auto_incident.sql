-- +goose Up
-- Make auto-incidents optional per monitor. Default true preserves the existing
-- behavior (every monitor opens an incident on going down) for current rows.

ALTER TABLE monitors ADD COLUMN IF NOT EXISTS auto_incident boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE monitors DROP COLUMN IF EXISTS auto_incident;
