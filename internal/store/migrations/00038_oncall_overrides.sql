-- +goose Up
-- On-call overrides: temporarily replace who is on call for a schedule during a window
-- (vacation cover). While starts_at <= now < ends_at, channel_id is on call regardless
-- of the rotation.
CREATE TABLE oncall_overrides (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id uuid NOT NULL REFERENCES oncall_schedules (id) ON DELETE CASCADE,
    channel_id  uuid NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    starts_at   timestamptz NOT NULL,
    ends_at     timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oncall_overrides_schedule_idx ON oncall_overrides (schedule_id, starts_at);

-- +goose Down
DROP TABLE IF EXISTS oncall_overrides;
