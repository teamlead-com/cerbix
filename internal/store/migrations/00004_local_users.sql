-- +goose Up
-- Support local (non-OIDC) users: oidc_sub becomes optional and a
-- password hash is added. A user is OIDC-backed (oidc_sub set) or local
-- (password_hash set). Local accounts are keyed by a unique email.

ALTER TABLE users ALTER COLUMN oidc_sub DROP NOT NULL;
ALTER TABLE users ADD COLUMN password_hash text;

CREATE UNIQUE INDEX users_local_email_uniq ON users (email) WHERE password_hash IS NOT NULL;

-- +goose Down
DROP INDEX users_local_email_uniq;
ALTER TABLE users DROP COLUMN password_hash;
ALTER TABLE users ALTER COLUMN oidc_sub SET NOT NULL;
