-- +goose Up
-- The hypertable swap in 00043 created the table as heartbeats_new, so its FK
-- kept the heartbeats_new_* name after the rename — confusing in error logs.
-- Guarded: the declarative-partition mode never had the old name.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'heartbeats_new_monitor_id_fkey' AND conrelid = 'heartbeats'::regclass
    ) THEN
        ALTER TABLE heartbeats RENAME CONSTRAINT heartbeats_new_monitor_id_fkey TO heartbeats_monitor_id_fkey;
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'heartbeats_monitor_id_fkey' AND conrelid = 'heartbeats'::regclass
    ) AND EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        ALTER TABLE heartbeats RENAME CONSTRAINT heartbeats_monitor_id_fkey TO heartbeats_new_monitor_id_fkey;
    END IF;
END $$;
-- +goose StatementEnd
