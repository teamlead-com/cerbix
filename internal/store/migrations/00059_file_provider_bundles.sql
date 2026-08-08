-- +goose Up
-- Monitoring as Code persistence (spec func-monitoring-as-code §13). One row per resolved
-- (provider, organization, project) bundle: its tenant-safe relative source path, canonical
-- content hash (last-known-good), monotonic applied generation, status, and a bounded error
-- (never raw YAML or secrets). The composite FK (project_id, org_id) → projects(id, org_id)
-- proves the bundle's project belongs to its organization at the schema level.
CREATE TABLE file_provider_bundles (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id  text NOT NULL,
    org_id       uuid NOT NULL,
    project_id   uuid NOT NULL,
    source_path  text NOT NULL DEFAULT '',
    content_hash text NOT NULL DEFAULT '',
    generation   bigint NOT NULL DEFAULT 0,
    status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'applied', 'rejected', 'error', 'degraded')),
    last_error   text NOT NULL DEFAULT '',
    applied_at   timestamptz,
    attempted_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, org_id, project_id),
    FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS file_provider_bundles;
