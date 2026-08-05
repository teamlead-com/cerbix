-- +goose Up
-- Link auto-opened incidents to the monitor that triggered them (FR-012 phase 2b).

ALTER TABLE incidents
    ADD COLUMN monitor_id uuid REFERENCES monitors (id) ON DELETE SET NULL;

-- Fast lookup of the open auto-incident for a monitor (dedup on transitions).
CREATE INDEX incidents_open_auto_monitor_idx
    ON incidents (monitor_id)
    WHERE source = 'auto' AND status <> 'resolved';

-- +goose Down
DROP INDEX incidents_open_auto_monitor_idx;
ALTER TABLE incidents DROP COLUMN monitor_id;
