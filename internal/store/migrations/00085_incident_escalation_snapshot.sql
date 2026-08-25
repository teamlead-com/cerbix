-- +goose Up
-- FR-023 non-goal, enforced by the schema instead of by hope.
--
-- The service ladder read `services.escalation_policy_id` LIVE and timed its steps from
-- `incidents.started_at`. Both halves are current, the incident is old, and the combination pages
-- retroactively: attach a policy to a service whose incident has been open for two hours and the next
-- pass finds every step's delay already elapsed. Replace the policy mid-incident and the steps the
-- ladder already took belong to a ladder that no longer exists. §8 of func-service-escalation lists
-- "retroactive escalation of incidents opened before a policy was attached" as a NON-GOAL, so this
-- was the spec being contradicted rather than a preference.
--
-- Pinning `escalation_policy_id` on the incident would not have been enough. `escalation_policies`
-- carries its ladder in a `steps` jsonb column and has no version, so editing a policy IN PLACE moves
-- the ladder under every open incident that names it — the same defect, reached by a third route. The
-- snapshot therefore holds the STEPS, not a reference to them.
CREATE TABLE incident_escalation_snapshots (
    incident_id uuid PRIMARY KEY REFERENCES incidents (id) ON DELETE CASCADE,
    project_id  uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    -- The policy this was taken from, kept for the record and deliberately NOT a foreign key: the
    -- ladder an incident ran has to remain readable after the policy is deleted, exactly as the
    -- member snapshot outlives its declaration.
    policy_id   uuid,
    policy_name text NOT NULL,
    repeat_last boolean NOT NULL DEFAULT false,
    steps       jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT incident_escalation_snapshots_steps_chk CHECK (jsonb_typeof(steps) = 'array')
);

-- +goose Down
DROP TABLE IF EXISTS incident_escalation_snapshots;
