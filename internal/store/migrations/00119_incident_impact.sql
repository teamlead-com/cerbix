-- FR-042 / NFR-035, spec §5: incident impact. Three columns, each with a CLOSED CHECK:
--   services.impact_by_state     the per-service mapping from alert state to impact (NULL = defaults, D6)
--   projects.default_incident_impact  the impact an incident gets when nothing else decides it
--   incidents.impact_basis       the snapshot of WHY the impact is what it is (D8 shapes)
-- `heartbeats` is untouched: these are ordinary tables in both storage modes.
--
-- Every CHECK is wrapped in COALESCE(..., false) and guarded by CASE: a CHECK that evaluates to NULL
-- passes, and PostgreSQL does not promise AND short-circuit order (`jsonb - text[]` raises on a scalar).

-- +goose Up
ALTER TABLE services ADD COLUMN IF NOT EXISTS impact_by_state jsonb NULL;
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_impact_by_state_chk;
ALTER TABLE services ADD CONSTRAINT services_impact_by_state_chk CHECK (
    impact_by_state IS NULL OR COALESCE(
        CASE WHEN jsonb_typeof(impact_by_state) <> 'object' THEN false ELSE
            (impact_by_state ?& ARRAY['degraded','down','unknown'])
            AND ((impact_by_state - ARRAY['degraded','down','unknown']) = '{}'::jsonb)
            AND jsonb_typeof(impact_by_state -> 'degraded') = 'string'
            AND jsonb_typeof(impact_by_state -> 'down') = 'string'
            AND jsonb_typeof(impact_by_state -> 'unknown') = 'string'
            AND (impact_by_state ->> 'degraded') IN ('none','minor','major','critical')
            AND (impact_by_state ->> 'down')     IN ('none','minor','major','critical')
            AND (impact_by_state ->> 'unknown')  IN ('none','minor','major','critical')
        END, false));

ALTER TABLE projects ADD COLUMN IF NOT EXISTS default_incident_impact text NOT NULL DEFAULT 'major';
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_default_incident_impact_chk;
ALTER TABLE projects ADD CONSTRAINT projects_default_incident_impact_chk
    CHECK (default_incident_impact IN ('none','minor','major','critical'));

ALTER TABLE incidents ADD COLUMN IF NOT EXISTS impact_basis jsonb NULL;
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_impact_basis_chk;
ALTER TABLE incidents ADD CONSTRAINT incidents_impact_basis_chk CHECK (
    impact_basis IS NULL OR COALESCE(
        CASE
            WHEN jsonb_typeof(impact_basis) <> 'object' THEN false
            WHEN jsonb_typeof(impact_basis -> 'source') IS DISTINCT FROM 'string' THEN false
            WHEN (impact_basis ->> 'source') IN ('project_default','manual','alertmanager')
                THEN (impact_basis - 'source') = '{}'::jsonb
            WHEN (impact_basis ->> 'source') = 'service_mapping' THEN
                CASE
                    WHEN NOT (impact_basis ?& ARRAY['source','mapping','state','service'])
                      OR (impact_basis - ARRAY['source','mapping','state','service']) <> '{}'::jsonb
                        THEN false
                    WHEN jsonb_typeof(impact_basis -> 'mapping') <> 'object'
                      OR jsonb_typeof(impact_basis -> 'service') <> 'object'
                      OR jsonb_typeof(impact_basis -> 'state') <> 'string'
                        THEN false
                    ELSE
                        ((impact_basis -> 'mapping') ?& ARRAY['degraded','down','unknown'])
                        AND (((impact_basis -> 'mapping') - ARRAY['degraded','down','unknown']) = '{}'::jsonb)
                        AND jsonb_typeof(impact_basis -> 'mapping' -> 'degraded') = 'string'
                        AND jsonb_typeof(impact_basis -> 'mapping' -> 'down') = 'string'
                        AND jsonb_typeof(impact_basis -> 'mapping' -> 'unknown') = 'string'
                        AND (impact_basis -> 'mapping' ->> 'degraded') IN ('none','minor','major','critical')
                        AND (impact_basis -> 'mapping' ->> 'down')     IN ('none','minor','major','critical')
                        AND (impact_basis -> 'mapping' ->> 'unknown')  IN ('none','minor','major','critical')
                        AND (impact_basis ->> 'state') IN ('degraded','down','unknown')
                        AND ((impact_basis -> 'service') ?& ARRAY['name','slug'])
                        AND (((impact_basis -> 'service') - ARRAY['name','slug']) = '{}'::jsonb)
                        AND jsonb_typeof(impact_basis -> 'service' -> 'name') = 'string'
                        AND jsonb_typeof(impact_basis -> 'service' -> 'slug') = 'string'
                END
            ELSE false
        END, false));

-- +goose Down
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_impact_basis_chk;
ALTER TABLE incidents DROP COLUMN IF EXISTS impact_basis;
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_default_incident_impact_chk;
ALTER TABLE projects DROP COLUMN IF EXISTS default_incident_impact;
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_impact_by_state_chk;
ALTER TABLE services DROP COLUMN IF EXISTS impact_by_state;
