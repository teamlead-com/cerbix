-- +goose Up
-- +goose StatementBegin

-- Alert routing wakes humans. Its tenant identity therefore belongs in the schema rather than in
-- whichever HTTP handler happens to write a relationship today.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'notification_channels_id_project_key') THEN
        ALTER TABLE notification_channels
            ADD CONSTRAINT notification_channels_id_project_key UNIQUE (id, project_id);
    END IF;
END $$;

ALTER TABLE monitor_notifications ADD COLUMN project_id uuid;
UPDATE monitor_notifications mn
   SET project_id = m.project_id
  FROM monitors m
 WHERE m.id = mn.monitor_id;
ALTER TABLE monitor_notifications ALTER COLUMN project_id SET NOT NULL;

ALTER TABLE monitor_notifications
    DROP CONSTRAINT monitor_notifications_monitor_id_fkey,
    DROP CONSTRAINT monitor_notifications_channel_id_fkey,
    ADD CONSTRAINT monitor_notifications_monitor_project_fkey
        FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE,
    ADD CONSTRAINT monitor_notifications_channel_project_fkey
        FOREIGN KEY (channel_id, project_id) REFERENCES notification_channels (id, project_id) ON DELETE CASCADE;

ALTER TABLE oncall_overrides ADD COLUMN project_id uuid;
UPDATE oncall_overrides o
   SET project_id = s.project_id
  FROM oncall_schedules s
 WHERE s.id = o.schedule_id;
ALTER TABLE oncall_overrides ALTER COLUMN project_id SET NOT NULL;

ALTER TABLE oncall_overrides
    DROP CONSTRAINT oncall_overrides_schedule_id_fkey,
    DROP CONSTRAINT oncall_overrides_channel_id_fkey,
    ADD CONSTRAINT oncall_overrides_schedule_project_fkey
        FOREIGN KEY (schedule_id, project_id) REFERENCES oncall_schedules (id, project_id) ON DELETE CASCADE,
    ADD CONSTRAINT oncall_overrides_channel_project_fkey
        FOREIGN KEY (channel_id, project_id) REFERENCES notification_channels (id, project_id) ON DELETE CASCADE;

-- JSONB references cannot use a foreign key. This trigger answers the narrower tenant question:
-- an absent/deleted target is allowed to remain historical configuration, but an id that resolves
-- in another project is never representable. Store writers additionally require a live same-project
-- target so callers receive a typed refusal before SQL reaches this guard.
CREATE OR REPLACE FUNCTION alert_routing_tenant_guard() RETURNS trigger AS $$
DECLARE
    ref jsonb;
    ref_id text;
    ref_type text;
BEGIN
    IF TG_TABLE_NAME IN ('escalation_policies', 'incident_escalation_snapshots') THEN
        FOR ref IN
            SELECT target
              FROM jsonb_array_elements(NEW.steps) step,
                   LATERAL jsonb_array_elements(COALESCE(step->'targets', '[]'::jsonb)) target
        LOOP
            ref_id := ref->>'id';
            ref_type := ref->>'type';
            IF ref_type = 'channel' AND EXISTS (
                SELECT 1 FROM notification_channels
                 WHERE id::text = ref_id AND project_id <> NEW.project_id
            ) THEN
                RAISE EXCEPTION 'escalation target channel belongs to another project'
                    USING ERRCODE = '23514';
            ELSIF ref_type = 'schedule' AND EXISTS (
                SELECT 1 FROM oncall_schedules
                 WHERE id::text = ref_id AND project_id <> NEW.project_id
            ) THEN
                RAISE EXCEPTION 'escalation target schedule belongs to another project'
                    USING ERRCODE = '23514';
            END IF;
        END LOOP;
    ELSIF TG_TABLE_NAME = 'oncall_schedules' THEN
        FOR ref_id IN SELECT jsonb_array_elements_text(NEW.participants)
        LOOP
            IF EXISTS (
                SELECT 1 FROM notification_channels
                 WHERE id::text = ref_id AND project_id <> NEW.project_id
            ) THEN
                RAISE EXCEPTION 'on-call participant belongs to another project'
                    USING ERRCODE = '23514';
            END IF;
        END LOOP;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER escalation_policy_tenant_guard_trg
    BEFORE INSERT OR UPDATE OF project_id, steps ON escalation_policies
    FOR EACH ROW EXECUTE FUNCTION alert_routing_tenant_guard();

CREATE TRIGGER incident_escalation_snapshot_tenant_guard_trg
    BEFORE INSERT OR UPDATE OF project_id, steps ON incident_escalation_snapshots
    FOR EACH ROW EXECUTE FUNCTION alert_routing_tenant_guard();

CREATE TRIGGER oncall_schedule_tenant_guard_trg
    BEFORE INSERT OR UPDATE OF project_id, participants ON oncall_schedules
    FOR EACH ROW EXECUTE FUNCTION alert_routing_tenant_guard();

-- Validate the rows that predate the triggers without changing their declared values.
UPDATE escalation_policies SET project_id = project_id;
UPDATE incident_escalation_snapshots SET project_id = project_id;
UPDATE oncall_schedules SET project_id = project_id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS oncall_schedule_tenant_guard_trg ON oncall_schedules;
DROP TRIGGER IF EXISTS incident_escalation_snapshot_tenant_guard_trg ON incident_escalation_snapshots;
DROP TRIGGER IF EXISTS escalation_policy_tenant_guard_trg ON escalation_policies;
DROP FUNCTION IF EXISTS alert_routing_tenant_guard();

ALTER TABLE oncall_overrides
    DROP CONSTRAINT IF EXISTS oncall_overrides_schedule_project_fkey,
    DROP CONSTRAINT IF EXISTS oncall_overrides_channel_project_fkey,
    ADD CONSTRAINT oncall_overrides_schedule_id_fkey
        FOREIGN KEY (schedule_id) REFERENCES oncall_schedules (id) ON DELETE CASCADE,
    ADD CONSTRAINT oncall_overrides_channel_id_fkey
        FOREIGN KEY (channel_id) REFERENCES notification_channels (id) ON DELETE CASCADE;
ALTER TABLE oncall_overrides DROP COLUMN project_id;

ALTER TABLE monitor_notifications
    DROP CONSTRAINT IF EXISTS monitor_notifications_monitor_project_fkey,
    DROP CONSTRAINT IF EXISTS monitor_notifications_channel_project_fkey,
    ADD CONSTRAINT monitor_notifications_monitor_id_fkey
        FOREIGN KEY (monitor_id) REFERENCES monitors (id) ON DELETE CASCADE,
    ADD CONSTRAINT monitor_notifications_channel_id_fkey
        FOREIGN KEY (channel_id) REFERENCES notification_channels (id) ON DELETE CASCADE;
ALTER TABLE monitor_notifications DROP COLUMN project_id;

ALTER TABLE notification_channels DROP CONSTRAINT IF EXISTS notification_channels_id_project_key;

-- +goose StatementEnd
