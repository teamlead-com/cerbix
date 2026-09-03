-- +goose Up
-- FR-029 invariant 6, pull transport: which capability a job REQUIRES, so an agent can only claim
-- what it announced it can run.
--
-- The AMQP half gets this from physics — a canary rides its own queue and an executor that cannot
-- run one never binds it. The pull half has no such queue: every agent in a region claims from the
-- same table, so a mixed fleet mid-upgrade meant the OLD agent could take a canary and fail it,
-- while the new one polled an empty queue. A capability CHECK does not stop a consumer from
-- consuming (D-0160); the claim itself has to filter.
--
-- NULL means "any agent may run this", which is every job that is not a canary and every row that
-- already exists. Nothing about the ordinary path changes.
ALTER TABLE pull_jobs ADD COLUMN IF NOT EXISTS workflow_kind text;

-- +goose Down
ALTER TABLE pull_jobs DROP COLUMN IF EXISTS workflow_kind;
