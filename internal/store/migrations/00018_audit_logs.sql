-- +goose Up
-- Audit log of access-relevant actions within an organization (member changes,
-- API-token issuance/revocation). Actor is a soft FK so history survives user
-- deletion; the org FK cascades so an org's audit trail is removed with it.

CREATE TABLE audit_logs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    via_token     boolean NOT NULL DEFAULT false,
    action        text NOT NULL,
    target        text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_org_idx ON audit_logs (org_id, created_at DESC);

-- +goose Down
DROP TABLE audit_logs;
