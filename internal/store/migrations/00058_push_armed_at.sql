-- +goose Up
-- Server-owned liveness re-arm point for push monitors. A push monitor's dead-man
-- freshness is max(push_armed_at, last_result_ts) with created_at as the floor:
-- last_result_ts tracks REAL pings only, and push_armed_at is stamped when a
-- disabled push monitor is re-enabled so the new liveness window starts from the
-- enable moment (a pre-disable ping is not proof of liveness after re-enable).
-- Kept separate from last_result_ts so re-arm never fabricates a fake observation.
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS push_armed_at timestamptz;

-- +goose Down
ALTER TABLE monitors DROP COLUMN IF EXISTS push_armed_at;
