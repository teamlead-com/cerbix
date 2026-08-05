-- +goose Up
-- Email/SMTP settings group for instance_settings (Tier: settings CUT 2). The SMTP
-- password inside this JSONB is encrypted at rest via the app keyring (secret.Cipher).
ALTER TABLE instance_settings ADD COLUMN IF NOT EXISTS mail jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE instance_settings DROP COLUMN IF EXISTS mail;
