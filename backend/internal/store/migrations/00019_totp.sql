-- +goose Up
-- Two-factor auth (TOTP) for local accounts. The secret is encrypted at rest
-- (via the app cipher); recovery codes are stored as hashes and consumed on use.

ALTER TABLE users ADD COLUMN totp_secret  text    NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_enabled boolean NOT NULL DEFAULT false;

CREATE TABLE totp_recovery_codes (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash text NOT NULL,
    used_at   timestamptz
);

CREATE INDEX totp_recovery_user_idx ON totp_recovery_codes (user_id);

-- +goose Down
DROP TABLE totp_recovery_codes;
ALTER TABLE users DROP COLUMN totp_enabled;
ALTER TABLE users DROP COLUMN totp_secret;
