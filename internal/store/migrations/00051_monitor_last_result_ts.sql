-- +goose Up
-- last_result_ts is the probe timestamp of the most recent result that was APPLIED
-- to the monitor's live status. It is the freshness watermark used by RecordResult:
-- a result whose ts is not strictly newer is still recorded as a heartbeat (SLA is
-- never lost) but must not mutate live status — so a slow/late probe finishing after
-- a newer one can no longer override the current state. NULL means "never applied".
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS last_result_ts timestamptz;

-- +goose Down
ALTER TABLE monitors DROP COLUMN IF EXISTS last_result_ts;
