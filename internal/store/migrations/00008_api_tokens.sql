-- +goose Up
-- Service-account API tokens (FR-012 phase 2b). Only a hash of the secret is stored.

CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id   uuid REFERENCES projects (id) ON DELETE CASCADE, -- NULL = org-scoped token
    name         text NOT NULL,
    role         text NOT NULL,
    token_hash   text NOT NULL UNIQUE,
    created_by   text NOT NULL DEFAULT '',
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT api_tokens_role_chk CHECK (role IN ('org_admin', 'project_admin', 'editor', 'viewer'))
);

CREATE INDEX api_tokens_org_idx ON api_tokens (org_id);

-- +goose Down
DROP TABLE api_tokens;
