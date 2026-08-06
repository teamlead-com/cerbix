-- +goose Up
-- Global-admin actions (user.global_admin, user.delete) are instance-level and
-- have no organization; audit entries for them carry org_id = NULL.
ALTER TABLE audit_logs ALTER COLUMN org_id DROP NOT NULL;

-- +goose Down
DELETE FROM audit_logs WHERE org_id IS NULL;
ALTER TABLE audit_logs ALTER COLUMN org_id SET NOT NULL;
