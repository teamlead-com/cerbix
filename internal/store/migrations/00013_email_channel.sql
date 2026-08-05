-- +goose Up
-- Add the email channel type to the notification-channel type check (FR-010).

ALTER TABLE notification_channels DROP CONSTRAINT notification_channels_type_chk;
ALTER TABLE notification_channels
    ADD CONSTRAINT notification_channels_type_chk CHECK (type IN ('webhook', 'slack', 'telegram', 'email'));

-- +goose Down
ALTER TABLE notification_channels DROP CONSTRAINT notification_channels_type_chk;
ALTER TABLE notification_channels
    ADD CONSTRAINT notification_channels_type_chk CHECK (type IN ('webhook', 'slack', 'telegram'));
