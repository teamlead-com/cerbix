-- +goose Up
-- Notification channels and their monitor links (FR-010).

CREATE TABLE notification_channels (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    type       text NOT NULL,
    name       text NOT NULL,
    config     jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_channels_type_chk CHECK (type IN ('webhook', 'slack', 'telegram'))
);

CREATE INDEX notification_channels_project_idx ON notification_channels (project_id);

CREATE TABLE monitor_notifications (
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    channel_id uuid NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    PRIMARY KEY (monitor_id, channel_id)
);

CREATE INDEX monitor_notifications_channel_idx ON monitor_notifications (channel_id);

-- +goose Down
DROP TABLE monitor_notifications;
DROP TABLE notification_channels;
