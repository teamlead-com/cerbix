-- +goose Up
-- OIDC identity is (issuer, subject), not subject alone. Two different identity
-- providers can legitimately mint the same `sub`, and keying uniqueness on the
-- subject alone let a second provider's user silently take over the first's
-- account (identity confusion / account takeover). Add the issuer column and
-- re-key uniqueness to the (issuer, subject) pair. Existing OIDC rows keep a NULL
-- issuer until the owner's next login claims it (store.UpsertUserByOIDCIdentity).
ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_issuer text;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_oidc_sub_key;
CREATE UNIQUE INDEX IF NOT EXISTS users_oidc_issuer_sub_key
    ON users (oidc_issuer, oidc_sub) WHERE oidc_sub IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS users_oidc_issuer_sub_key;
ALTER TABLE users ADD CONSTRAINT users_oidc_sub_key UNIQUE (oidc_sub);
ALTER TABLE users DROP COLUMN IF EXISTS oidc_issuer;
