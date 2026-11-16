-- +goose Up
-- +goose StatementBegin
-- FR-043 §5, I13a (spec revision 7; owner's decision 2026-11-16; iter-0214, D-0281): a writer that
-- does not know about the recovery run ends it by moving the monitor out of `down`, and the check
-- result is never lost.
--
-- The run-state CHECK of 00117 (monitors_recovery_run_state) allows a run only on a `down` row. A
-- binary without FR-043 — one role during a rolling upgrade, or every role after a rollback — writes
-- `status = 'up'` (or 'pending') and never touches the run columns, so the CHECK refused that write
-- and, because the status write shares its transaction with the heartbeat insert, the check result
-- was rolled back with it. This BEFORE UPDATE trigger clears the run on any row whose new status is
-- not `down`, before the CHECK is evaluated, so that write succeeds: under the old binary the
-- threshold does not apply (the monitor goes up on its first success) and nothing is lost; back on
-- this release the threshold applies again.
--
-- It is a guard for writers this code does not control, NOT the mechanism: every status writer in
-- this release resets the run itself, in its own statement. The CHECKs are not weakened. A failure
-- written by an old binary keeps the row `down` and its run as it was — a valid row.
CREATE OR REPLACE FUNCTION monitors_recovery_run_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.status <> 'down' THEN
        NEW.consecutive_successes := 0;
        NEW.recovery_started_at := NULL;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS monitors_recovery_run_guard_trg ON monitors;
CREATE TRIGGER monitors_recovery_run_guard_trg
    BEFORE UPDATE ON monitors
    FOR EACH ROW EXECUTE FUNCTION monitors_recovery_run_guard();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS monitors_recovery_run_guard_trg ON monitors;
DROP FUNCTION IF EXISTS monitors_recovery_run_guard();
-- +goose StatementEnd
