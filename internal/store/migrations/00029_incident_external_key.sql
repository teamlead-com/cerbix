-- +goose Up
-- external_key correlates an incident with an outside alert source (e.g. an
-- Alertmanager fingerprint) so a "resolved" webhook can close the incident its
-- "firing" webhook opened. The partial unique index guarantees at most one OPEN
-- incident per (project, key), making the receiver's open-or-reuse idempotent.
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS external_key text;

CREATE UNIQUE INDEX IF NOT EXISTS incidents_external_key_open_idx
    ON incidents (project_id, external_key)
    WHERE external_key IS NOT NULL AND status <> 'resolved';

-- +goose Down
DROP INDEX IF EXISTS incidents_external_key_open_idx;
ALTER TABLE incidents DROP COLUMN IF EXISTS external_key;
