-- +goose Up
-- Generalized instance-wide settings (global-admin, editable from the UI). One
-- singleton row, one JSONB column per settings group. Adding a group is a new
-- column + Go type — no new table, no new access pattern. Each group's JSON carries
-- a "configured" flag: once true it overrides the config-file bootstrap / defaults.
CREATE TABLE instance_settings (
    id               boolean     PRIMARY KEY DEFAULT true,
    branding         jsonb       NOT NULL DEFAULT '{}',
    auth_policy      jsonb       NOT NULL DEFAULT '{}',
    alerting         jsonb       NOT NULL DEFAULT '{}',
    monitor_defaults jsonb       NOT NULL DEFAULT '{}',
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT instance_settings_singleton CHECK (id)
);

-- +goose Down
DROP TABLE instance_settings;
