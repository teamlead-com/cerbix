-- +goose Up
-- +goose StatementBegin

-- Phase-1 review fix: a service's owner must belong to the service's project
-- (func-service-reliability §5).
--
-- `services.escalation_policy_id` and `oncall_schedule_id` referenced the routing tables by
-- ID ALONE. The database therefore had no opinion about tenancy, and the create path passed
-- whatever id it was handed: an editor in one project could attach another project's
-- escalation policy or on-call schedule, and the routing of operational response would then
-- point across a tenant boundary — the one place in this feature where being wrong pages the
-- wrong humans.
--
-- The fix is the pattern 00060/00061 already use: a composite FK proves the two rows share a
-- project, in the schema, for every writer that will ever exist.

-- FK targets. Guarded so a re-run is a no-op (the 00043 pattern).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'escalation_policies_id_project_key') THEN
        ALTER TABLE escalation_policies
            ADD CONSTRAINT escalation_policies_id_project_key UNIQUE (id, project_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'oncall_schedules_id_project_key') THEN
        ALTER TABLE oncall_schedules
            ADD CONSTRAINT oncall_schedules_id_project_key UNIQUE (id, project_id);
    END IF;
END $$;

-- Any row that already crossed a boundary is released rather than blocking the migration:
-- losing the reference is a routing gap the UI shows, where a failed migration is an outage.
UPDATE services s SET escalation_policy_id = NULL
 WHERE escalation_policy_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM escalation_policies e
                    WHERE e.id = s.escalation_policy_id AND e.project_id = s.project_id);
UPDATE services s SET oncall_schedule_id = NULL
 WHERE oncall_schedule_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM oncall_schedules o
                    WHERE o.id = s.oncall_schedule_id AND o.project_id = s.project_id);

ALTER TABLE services
    DROP CONSTRAINT IF EXISTS services_escalation_policy_id_fkey,
    DROP CONSTRAINT IF EXISTS services_oncall_schedule_id_fkey;

ALTER TABLE services
    ADD CONSTRAINT services_escalation_policy_tenant_fkey
        FOREIGN KEY (escalation_policy_id, project_id)
        REFERENCES escalation_policies (id, project_id) ON DELETE SET NULL,
    ADD CONSTRAINT services_oncall_schedule_tenant_fkey
        FOREIGN KEY (oncall_schedule_id, project_id)
        REFERENCES oncall_schedules (id, project_id) ON DELETE SET NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE services
    DROP CONSTRAINT IF EXISTS services_escalation_policy_tenant_fkey,
    DROP CONSTRAINT IF EXISTS services_oncall_schedule_tenant_fkey;

ALTER TABLE services
    ADD CONSTRAINT services_escalation_policy_id_fkey
        FOREIGN KEY (escalation_policy_id) REFERENCES escalation_policies (id) ON DELETE SET NULL,
    ADD CONSTRAINT services_oncall_schedule_id_fkey
        FOREIGN KEY (oncall_schedule_id) REFERENCES oncall_schedules (id) ON DELETE SET NULL;
-- +goose StatementEnd
