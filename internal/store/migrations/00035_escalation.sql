-- +goose Up
-- On-call escalation: per-project notification ladders and rotations, layered on a
-- down monitor's open auto-incident. A policy's steps fire at increasing offsets from
-- the incident start; acknowledgement or recovery stops the ladder.
CREATE TABLE escalation_policies (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name        text NOT NULL,
    repeat_last boolean NOT NULL DEFAULT false,
    steps       jsonb NOT NULL DEFAULT '[]',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX escalation_policies_project_idx ON escalation_policies (project_id);

CREATE TABLE oncall_schedules (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name          text NOT NULL,
    shift_seconds integer NOT NULL,
    anchor_at     timestamptz NOT NULL,
    participants  jsonb NOT NULL DEFAULT '[]',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oncall_schedules_project_idx ON oncall_schedules (project_id);

-- A monitor may reference an escalation policy for its down alerts. Detaching the
-- policy leaves the monitor on the flat (all-channels) notification path.
ALTER TABLE monitors
    ADD COLUMN escalation_policy_id uuid REFERENCES escalation_policies (id) ON DELETE SET NULL;

-- Escalation state and acknowledgement live on the incident.
ALTER TABLE incidents
    ADD COLUMN acknowledged_at  timestamptz,
    ADD COLUMN acknowledged_by  text,
    ADD COLUMN escalation_step  integer NOT NULL DEFAULT 0,
    ADD COLUMN last_escalated_at timestamptz;

-- Allow the new escalation outbox topic through the whitelist CHECK.
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report', 'region_worker_alert', 'escalation_step'));

-- +goose Down
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report', 'region_worker_alert'));
ALTER TABLE incidents
    DROP COLUMN IF EXISTS acknowledged_at,
    DROP COLUMN IF EXISTS acknowledged_by,
    DROP COLUMN IF EXISTS escalation_step,
    DROP COLUMN IF EXISTS last_escalated_at;
ALTER TABLE monitors DROP COLUMN IF EXISTS escalation_policy_id;
DROP TABLE IF EXISTS oncall_schedules;
DROP TABLE IF EXISTS escalation_policies;
