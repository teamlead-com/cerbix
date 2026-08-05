-- +goose Up
-- Database-managed pull-agent tokens: an alternative to config `pull.agents` that lets an
-- operator issue and revoke per-region agent tokens at runtime (no redeploy), storing only
-- a hash. A token authorizes exactly one region.
CREATE TABLE agent_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    region     text NOT NULL,
    token_hash text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

-- +goose Down
DROP TABLE IF EXISTS agent_tokens;
