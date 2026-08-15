-- +goose Up
-- Provider ownership for services (spec func-service-reliability §15.2).
--
-- Ownership is per RESOURCE, not per project: a project may hold file-owned services and
-- UI-owned services side by side, and reconciliation compares only against the ones this
-- provider owns. Absence from a bundle therefore never touches a service somebody else owns.
--
-- This mirrors `managed_monitors` (00060) deliberately — same shape, same composite tenant
-- FKs, same orphan marker — because a second ownership model would be a second set of rules
-- for the same question.
CREATE TABLE IF NOT EXISTS managed_services (
    service_id  uuid PRIMARY KEY,
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
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS managed_services_provider_idx
    ON managed_services (provider_id, org_id, project_id);

-- +goose Down
DROP TABLE IF EXISTS managed_services;
