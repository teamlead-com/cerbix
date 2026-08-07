-- +goose Up
-- Push tokens were stored in plaintext in monitors.push_token (a bearer secret in a
-- URL) with a unique index for lookup. Move them to the same secret-at-rest model as
-- every other secret column: a SHA-256 blind index for O(1) lookup on a push ping, and
-- an encrypted value for display to authorized users.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

ALTER TABLE monitors ADD COLUMN push_token_hash text;
ALTER TABLE monitors ADD COLUMN push_token_enc text;

-- Blind index: hex SHA-256, identical to store.HashToken, so a ping's token hashes to
-- the same value the lookup compares against.
UPDATE monitors SET push_token_hash = encode(digest(push_token, 'sha256'), 'hex')
 WHERE push_token IS NOT NULL;

-- Carry the secret into the at-rest column as-is. A SQL migration has no keyring, so it
-- lands UNENCRYPTED — but it carries no cipher prefix, so store reads treat it as
-- plaintext (secret.Decrypt passes unprefixed values through) and the next
-- ReencryptSecrets run (startup, when an encryption key is configured) rewrites it as
-- ciphertext. Same lifecycle as webhook secrets, channel creds, SMTP password, etc.
UPDATE monitors SET push_token_enc = push_token WHERE push_token IS NOT NULL;

DROP INDEX IF EXISTS monitors_push_token_uniq;
CREATE UNIQUE INDEX monitors_push_token_hash_uniq ON monitors (push_token_hash)
  WHERE push_token_hash IS NOT NULL;

ALTER TABLE monitors DROP COLUMN push_token;

-- +goose Down
-- Best-effort restore. If push_token_enc was already re-encrypted (a key is configured),
-- the restored push_token holds ciphertext, not the original token — acceptable for a
-- dev-only rollback.
ALTER TABLE monitors ADD COLUMN push_token text;
UPDATE monitors SET push_token = push_token_enc WHERE push_token_enc IS NOT NULL;
DROP INDEX IF EXISTS monitors_push_token_hash_uniq;
CREATE UNIQUE INDEX monitors_push_token_uniq ON monitors (push_token) WHERE push_token IS NOT NULL;
ALTER TABLE monitors DROP COLUMN push_token_enc;
ALTER TABLE monitors DROP COLUMN push_token_hash;
