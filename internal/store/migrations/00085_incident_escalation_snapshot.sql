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
    incident_id uuid PRIMARY KEY,
    project_id  uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    -- The policy this was taken from, kept for the record and deliberately NOT a foreign key: the
    -- ladder an incident ran has to remain readable after the policy is deleted, exactly as the
    -- member snapshot outlives its declaration.
    policy_id   uuid,
    policy_name text NOT NULL,
    repeat_last boolean NOT NULL DEFAULT false,
    steps       jsonb NOT NULL,
    -- The instant this incident's step OFFSETS are measured from. For an incident opened normally it
    -- is the incident's own `started_at`, which is the behaviour the ladder has always had. For a row
    -- written by the backfill below it is the MIGRATION's instant, and that difference is the whole
    -- reason the column exists: see the backfill's own note.
    due_base    timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT incident_escalation_snapshots_steps_chk CHECK (jsonb_typeof(steps) = 'array'),
    -- COMPOSITE, not two independent keys. With `incident_id → incidents(id)` and
    -- `project_id → projects(id)` written separately, each column is individually valid while the
    -- PAIR is not: direct SQL happily attaches project B's ladder to project A's incident, and the
    -- row that decides who gets paged is exactly the wrong row to leave un-tenanted. The
    -- (id, project_id) key this points at has existed since 00080 for the same reason.
    CONSTRAINT incident_escalation_snapshots_incident_fkey
        FOREIGN KEY (incident_id, project_id) REFERENCES incidents (id, project_id) ON DELETE CASCADE
);

-- BACKFILL, and it is not optional. The selection that reads this table INNER JOINs it, so without
-- this statement every service auto-incident that was already open at upgrade time silently stops
-- climbing its ladder: the rows exist, the service still has its policy, and nothing ever pages
-- again. An upgrade performed DURING an outage is exactly when that matters, and the failure is
-- invisible — no error, no log line, just a ladder that never advances.
--
-- It runs in the migration rather than in application code so the table is never observable in the
-- empty-but-required state: after this transaction the invariant "every open service incident that
-- can escalate has a snapshot" holds for old rows and new ones alike.
--
-- Incidents whose service has NO policy attached get nothing, which is the same thing a fresh open
-- would do for them today.
--
-- `due_base` is deliberately LEFT AT ITS DEFAULT — the migration's own instant — and not set to
-- `i.started_at`. Copying the incident's start would trade one upgrade defect for its mirror image:
-- steps are fired for every offset that has already elapsed, in a single pass, so a ladder attached to
-- a two-hour-old incident would empty itself into the on-call rotation the moment the first pass ran
-- after the upgrade. And it cannot be narrowed by looking at the data, because WHEN a policy was
-- attached is not recorded anywhere — an incident that opened with no policy at all is
-- indistinguishable here from one that had this policy from the start.
--
-- So a carried-over ladder starts at the upgrade: its first step fires on the next pass, and its later
-- steps wait their real delays. That is the honest reading of what we know, and it is the same
-- direction §13 chose for attaching a policy to an already-open incident — cerbix does not page for
-- time nobody decided to page for.
INSERT INTO incident_escalation_snapshots
    (incident_id, project_id, policy_id, policy_name, repeat_last, steps)
SELECT i.id, i.project_id, p.id, p.name, p.repeat_last, p.steps
  FROM incidents i
  JOIN services s ON s.id = i.service_id AND s.project_id = i.project_id
  JOIN escalation_policies p ON p.id = s.escalation_policy_id AND p.project_id = s.project_id
 WHERE i.source = 'auto'
   AND i.status <> 'resolved'
   AND i.service_id IS NOT NULL
ON CONFLICT (incident_id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS incident_escalation_snapshots;
