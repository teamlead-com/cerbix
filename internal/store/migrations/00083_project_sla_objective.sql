-- +goose Up
-- A project-level SLO objective (func-audit-gaps-2 "a project-level objective is a real feature, not
-- a gap fix"; iter-0155, owner decision: REPORTING ONLY).
--
-- `sla_targets` has carried `project_id` as one of three exclusive scopes since 00077, and no code
-- ever wrote it: the column was dormant schema. Making it writable needs exactly one thing from the
-- database — the guarantee that a project-scoped target cannot page.
--
-- Why a CHECK rather than a convention in the store: "reporting only" is a promise about what an
-- operator's edit can cause, and phase 5 (§16.4) taught the cost of leaving that promise to the
-- application. A service-scoped target was rejected at the schema for exactly this reason until the
-- phase that owned service paging arrived and lifted it deliberately, in a migration, with a decision
-- behind it. A project-level pager will need the same: an arming rule, a routing answer and a close
-- semantics. Until then the row simply cannot say "page me", whoever writes it and however.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'sla_targets_project_no_burn_chk'
    ) THEN
        ALTER TABLE sla_targets
            ADD CONSTRAINT sla_targets_project_no_burn_chk
            CHECK (project_id IS NULL OR (burn_alert_enabled = false AND (burn_rules IS NULL OR burn_rules = '[]'::jsonb)));
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE sla_targets DROP CONSTRAINT IF EXISTS sla_targets_project_no_burn_chk;
