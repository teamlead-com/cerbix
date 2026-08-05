-- +goose Up
-- Outbound webhook subscriptions (FR-012 phase 2b). The secret is stored to sign
-- deliveries (the server must reproduce the HMAC), so it is kept in plaintext.

CREATE TABLE webhooks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id uuid REFERENCES projects (id) ON DELETE CASCADE, -- NULL = org-wide
    url        text NOT NULL,
    secret     text NOT NULL DEFAULT '',
    enabled    boolean NOT NULL DEFAULT true,
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webhooks_org_idx ON webhooks (org_id);
CREATE INDEX webhooks_project_idx ON webhooks (project_id);

-- +goose Down
DROP TABLE webhooks;
