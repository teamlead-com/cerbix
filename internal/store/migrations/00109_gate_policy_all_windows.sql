-- +goose Up
-- FR-035/NFR-029: v2 policy documents may evaluate every service target. Existing v1 rows keep
-- their exact one-window meaning and revision; no row is rewritten except for the explicit mode.
ALTER TABLE service_gate_policies
    ADD COLUMN window_mode text NOT NULL DEFAULT 'one',
    ALTER COLUMN window_name DROP NOT NULL,
    ADD CONSTRAINT service_gate_policies_window_mode_chk CHECK (window_mode IN ('one', 'all')),
    ADD CONSTRAINT service_gate_policies_mode_window_chk CHECK (
        (window_mode = 'one' AND window_name IS NOT NULL AND window_name <> '') OR
        (window_mode = 'all' AND window_name IS NULL)
    );

ALTER TABLE project_gate_policies
    ADD COLUMN window_mode text NOT NULL DEFAULT 'one',
    ALTER COLUMN window_name DROP NOT NULL,
    ADD CONSTRAINT project_gate_policies_window_mode_chk CHECK (window_mode IN ('one', 'all')),
    ADD CONSTRAINT project_gate_policies_mode_window_chk CHECK (
        (window_mode = 'one' AND window_name IS NOT NULL AND window_name <> '') OR
        (window_mode = 'all' AND window_name IS NULL)
    );

ALTER TABLE service_gate_decisions
    ADD COLUMN window_mode text,
    ADD COLUMN evaluated_windows jsonb;

UPDATE service_gate_decisions
   SET window_mode = 'one', evaluated_windows = '[]'::jsonb
 WHERE state <> 'NOT_CONFIGURED';

ALTER TABLE service_gate_decisions
    DROP CONSTRAINT service_gate_decisions_policy_presence_chk,
    DROP CONSTRAINT service_gate_decisions_payload_chk,
    ADD CONSTRAINT service_gate_decisions_policy_presence_chk CHECK (
        (state = 'NOT_CONFIGURED') = (action IS NULL)
        AND (state = 'NOT_CONFIGURED') = (policy_revision IS NULL)
        AND (state = 'NOT_CONFIGURED') = (policy_snapshot IS NULL)
        AND (
            (state = 'NOT_CONFIGURED' AND window_name IS NULL) OR
            (state <> 'NOT_CONFIGURED' AND window_mode = 'one' AND window_name IS NOT NULL) OR
            (state <> 'NOT_CONFIGURED' AND window_mode = 'all' AND window_name IS NULL)
        )
    ),
    ADD CONSTRAINT service_gate_decisions_payload_chk CHECK (
        octet_length(evidence::text) <= 16384
        AND octet_length(reasons::text) <= 4096
        AND (policy_snapshot IS NULL OR octet_length(policy_snapshot::text) <= 4096)
    ),
    ADD CONSTRAINT service_gate_decisions_window_mode_chk CHECK (
        (state = 'NOT_CONFIGURED' AND window_mode IS NULL AND evaluated_windows IS NULL) OR
        (state <> 'NOT_CONFIGURED' AND window_mode IN ('one', 'all') AND jsonb_typeof(evaluated_windows) = 'array')
    );

-- +goose Down
ALTER TABLE service_gate_decisions DROP CONSTRAINT IF EXISTS service_gate_decisions_window_mode_chk;
ALTER TABLE service_gate_decisions DROP CONSTRAINT IF EXISTS service_gate_decisions_policy_presence_chk;
ALTER TABLE service_gate_decisions DROP CONSTRAINT IF EXISTS service_gate_decisions_payload_chk;
ALTER TABLE service_gate_decisions DROP COLUMN IF EXISTS evaluated_windows;
ALTER TABLE service_gate_decisions DROP COLUMN IF EXISTS window_mode;
ALTER TABLE service_gate_decisions
    ADD CONSTRAINT service_gate_decisions_policy_presence_chk CHECK (
        (state = 'NOT_CONFIGURED') = (action IS NULL)
        AND (state = 'NOT_CONFIGURED') = (policy_revision IS NULL)
        AND (state = 'NOT_CONFIGURED') = (policy_snapshot IS NULL)
        AND (state = 'NOT_CONFIGURED' OR window_name IS NOT NULL)
    ),
    ADD CONSTRAINT service_gate_decisions_payload_chk CHECK (
        octet_length(evidence::text) <= 4096
        AND octet_length(reasons::text) <= 1024
        AND (policy_snapshot IS NULL OR octet_length(policy_snapshot::text) <= 4096)
    );
ALTER TABLE project_gate_policies DROP CONSTRAINT IF EXISTS project_gate_policies_mode_window_chk;
ALTER TABLE project_gate_policies DROP CONSTRAINT IF EXISTS project_gate_policies_window_mode_chk;
ALTER TABLE project_gate_policies ALTER COLUMN window_name SET NOT NULL;
ALTER TABLE project_gate_policies DROP COLUMN IF EXISTS window_mode;
ALTER TABLE service_gate_policies DROP CONSTRAINT IF EXISTS service_gate_policies_mode_window_chk;
ALTER TABLE service_gate_policies DROP CONSTRAINT IF EXISTS service_gate_policies_window_mode_chk;
ALTER TABLE service_gate_policies ALTER COLUMN window_name SET NOT NULL;
ALTER TABLE service_gate_policies DROP COLUMN IF EXISTS window_mode;
