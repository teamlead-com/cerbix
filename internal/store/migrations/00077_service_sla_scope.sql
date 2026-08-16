-- +goose Up
-- FR-021 phase 2 (§11.3): sla_targets gains service_id as a THIRD exclusive scope beside
-- monitor_id / project_id. Service SLOs are REPORTING ONLY until phase 5 (§13), so a
-- service-scoped burn alert is rejected at the schema itself — no application bug can enable
-- paging semantics the spec deliberately left unowned.
ALTER TABLE sla_targets
    ADD COLUMN service_id uuid REFERENCES services (id) ON DELETE CASCADE;

ALTER TABLE sla_targets DROP CONSTRAINT sla_targets_scope_chk;
ALTER TABLE sla_targets ADD CONSTRAINT sla_targets_scope_chk CHECK (
    (monitor_id IS NOT NULL)::int + (project_id IS NOT NULL)::int + (service_id IS NOT NULL)::int = 1);

ALTER TABLE sla_targets ADD CONSTRAINT sla_targets_service_no_burn_chk CHECK (
    service_id IS NULL OR (burn_alert_enabled = false AND burn_rules = '[]'::jsonb));

CREATE UNIQUE INDEX sla_targets_service_uniq
    ON sla_targets (service_id, window_name) WHERE service_id IS NOT NULL;

-- +goose Down
DROP INDEX sla_targets_service_uniq;
ALTER TABLE sla_targets DROP CONSTRAINT sla_targets_service_no_burn_chk;
ALTER TABLE sla_targets DROP CONSTRAINT sla_targets_scope_chk;
DELETE FROM sla_targets WHERE service_id IS NOT NULL;
ALTER TABLE sla_targets ADD CONSTRAINT sla_targets_scope_chk CHECK (
    (monitor_id IS NULL) <> (project_id IS NULL));
ALTER TABLE sla_targets DROP COLUMN service_id;
