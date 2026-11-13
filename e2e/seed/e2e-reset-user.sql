-- A dedicated local account for e2e/tests/mail.spec.ts (sec-hardening-2 H-5, owner ruling
-- 2026-11-13). A password reset ends every session of the account, so the suite must never reset
-- the shared admin whose session every spec reuses — and cerbix has no product path that creates a
-- second local account, so the dev stack seeds this one. `make dev-ready-single` applies it to the
-- dev database; dev stacks only.
--
-- The hash is a real argon2id hash of a random secret that was thrown away: nobody can sign in to
-- this account until a reset gives it a password. ON CONFLICT DO NOTHING keeps whatever password
-- the last run left — the spec never needs it.
INSERT INTO users (email, display_name, password_hash)
VALUES ('e2e-reset@cerbix.local', 'E2E reset',
        '$argon2id$v=19$m=65536,t=1,p=4$s4eO8DrfHinheIGSu3q6tg$ZVcMuK6zJLDKXRdCMY9bKjqucvSkRrNhXPjvqaonJV8')
ON CONFLICT (email) WHERE password_hash IS NOT NULL DO NOTHING;
