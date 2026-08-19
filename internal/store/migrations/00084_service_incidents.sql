-- +goose Up
-- FR-022 (func-service-incidents, D-0170): a Service can own an incident.
--
-- Three shapes, and each one is a decision the spec argues for rather than a convenience:
--
--   * the anchor is EXCLUSIVE and enforced here, not in the store. Two anchors on one row is not a state
--     the application should have to remember to avoid — and the CHECK says "at most one", never "exactly
--     one", because a manual project-level incident carries neither today and must keep working;
--   * the FK is COMPOSITE against `services (id, project_id)`. A same-project guarantee that lives in
--     application code is one bug away from crossing tenants (FR-021 invariant 48 exists for that), and
--     the column-list `ON DELETE SET NULL (service_id)` is PG15's form: a bare SET NULL clears EVERY
--     referencing column, including the NOT NULL `project_id`, which is the trap iter-0125 hit;
--   * one OPEN auto-incident per service, by partial unique index, mirroring 00049's per-monitor rule. A
--     flapping service must not accumulate incidents, and the database is the only place that can promise
--     it under concurrent evaluators.
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS service_id uuid;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'incidents_service_fk') THEN
        ALTER TABLE incidents
            ADD CONSTRAINT incidents_service_fk
            FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id)
            ON DELETE SET NULL (service_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'incidents_one_anchor_chk') THEN
        ALTER TABLE incidents
            ADD CONSTRAINT incidents_one_anchor_chk
            CHECK ((monitor_id IS NOT NULL)::int + (service_id IS NOT NULL)::int <= 1);
    END IF;
END $$;
-- +goose StatementEnd

CREATE UNIQUE INDEX IF NOT EXISTS incidents_service_open_auto_idx
    ON incidents (service_id)
    WHERE source = 'auto' AND status <> 'resolved' AND service_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS incidents_service_idx ON incidents (service_id, started_at DESC)
    WHERE service_id IS NOT NULL;

-- The member set AS OF the open instant (spec D6). A postmortem is read after the world moved: a member
-- may be renamed, removed from the declaration or deleted outright, and a document that says "3 members"
-- without naming them is one nobody trusts. Same device as phase 5's immutable recipient snapshot, and
-- deliberately NOT a live join.
CREATE TABLE IF NOT EXISTS incident_member_snapshots (
    incident_id uuid PRIMARY KEY REFERENCES incidents (id) ON DELETE CASCADE,
    project_id  uuid NOT NULL,
    members     jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT incident_member_snapshots_tenant_fk
        FOREIGN KEY (incident_id, project_id) REFERENCES incidents (id, project_id) ON DELETE CASCADE,
    CONSTRAINT incident_member_snapshots_members_array_chk
        CHECK (jsonb_typeof(members) = 'array')
);

-- +goose Down
DROP TABLE IF EXISTS incident_member_snapshots;
DROP INDEX IF EXISTS incidents_service_idx;
DROP INDEX IF EXISTS incidents_service_open_auto_idx;
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_one_anchor_chk;
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_service_fk;
ALTER TABLE incidents DROP COLUMN IF EXISTS service_id;
