-- +goose Up
-- sec-hardening-2 H-1 (iter-0209, D-0276): a user is a `human` or a `machine`. A machine is what an
-- OIDC client-credentials token provisions on the bearer path; a human is what the interactive
-- sign-in provisions. The bearer path never authenticates a human row, and a machine row that a
-- person later signs in to becomes human for good, losing everything granted to the machine.
--
-- The backfill is deliberately narrow: only the synthetic `<sub>@clients` address the bearer path
-- gives a token without an email, never signed in, no password, not a global admin. Migration
-- 00111 notes that a human whose sessions were purged before it has a NULL last_seen_at, so a wider
-- rule would file dormant humans — admins included — as machines.
-- Every statement is idempotent, so re-running this section is harmless.
ALTER TABLE users ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'human';

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_kind_check') THEN
        ALTER TABLE users ADD CONSTRAINT users_kind_check CHECK (kind IN ('human', 'machine'));
    END IF;
END
$$;
-- +goose StatementEnd

UPDATE users SET kind = 'machine'
 WHERE kind = 'human'
   AND oidc_sub IS NOT NULL AND last_seen_at IS NULL AND password_hash IS NULL AND NOT is_global_admin
   AND email LIKE '%@clients';

-- The client ids whose client-credentials tokens the bearer path accepts. Empty turns it off.
ALTER TABLE oidc_settings ADD COLUMN IF NOT EXISTS machine_clients text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE oidc_settings DROP COLUMN IF EXISTS machine_clients;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_kind_check;
ALTER TABLE users DROP COLUMN IF EXISTS kind;
