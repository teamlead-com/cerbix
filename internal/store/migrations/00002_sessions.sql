-- +goose Up
-- Server-side sessions and transient OIDC login flows.

CREATE TABLE sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash text NOT NULL UNIQUE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

CREATE TABLE auth_flows (
    state         text PRIMARY KEY,
    nonce         text NOT NULL,
    pkce_verifier text NOT NULL,
    redirect_to   text NOT NULL DEFAULT '/',
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);

CREATE INDEX auth_flows_expires_idx ON auth_flows (expires_at);

-- +goose Down
DROP TABLE auth_flows;
DROP TABLE sessions;
