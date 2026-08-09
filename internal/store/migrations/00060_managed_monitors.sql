-- +goose Up
-- Per-monitor file-provider ownership/provenance (spec func-monitoring-as-code §8/§13).
-- monitor_id is the PRIMARY KEY, so a monitor is owned by AT MOST ONE provider. The unique
-- (provider_id, org_id, project_id, source_uid) binds one source UID to one monitor per
-- provider. Tenant safety is schema-enforced two ways: the composite FK
-- (project_id, org_id) → projects proves the provenance project belongs to its org, and the
-- composite FK (monitor_id, project_id) → monitors proves the owned monitor actually lives
-- in that project (spec §13: "monitor.project_id equals the provenance project").
-- orphaned_at records a valid-absence orphan; it is never a physical delete.

-- The (id, project_id) unique on monitors is the FK target below; guard it so re-runs are
-- idempotent.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'monitors_id_project_key') THEN
        ALTER TABLE monitors ADD CONSTRAINT monitors_id_project_key UNIQUE (id, project_id);
    END IF;
END $$;
-- +goose StatementEnd

CREATE TABLE IF NOT EXISTS managed_monitors (
    monitor_id  uuid PRIMARY KEY,
    provider_id text NOT NULL,
    org_id      uuid NOT NULL,
    project_id  uuid NOT NULL,
    source_uid  text NOT NULL,
    spec_hash   text NOT NULL DEFAULT '',
    source_path text NOT NULL DEFAULT '',
    generation  bigint NOT NULL DEFAULT 0,
    applied_at  timestamptz NOT NULL DEFAULT now(),
    orphaned_at timestamptz,
    UNIQUE (provider_id, org_id, project_id, source_uid),
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS managed_monitors;
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_id_project_key;
