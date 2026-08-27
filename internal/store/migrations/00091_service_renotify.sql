-- +goose Up

-- A service's escalation ladder gets a repeat cadence of its own (D-0185).
--
-- FR-023's D8 declared "no renotify knob for services" and justified it with a sentence that was
-- false: it said the policy's own repeat was the mechanism, and the repeat branch requires
-- `RepeatLast AND renotify_seconds > 0` — a number a service had no way to carry. So `repeat_last`
-- on a policy attached to a service did nothing at all (D-0181 corrected the claim and pinned the
-- behaviour with a test).
--
-- This is the knob D8 declined, added deliberately rather than by inventing a default somewhere. It
-- mirrors `monitors.renotify_seconds` exactly, including the meaning of ZERO: off. Every existing
-- service therefore keeps behaving as it does today and the repeat begins only where an operator
-- asks for it, which is the difference between adding a control and imposing a cadence.
ALTER TABLE services
    ADD COLUMN IF NOT EXISTS renotify_seconds int NOT NULL DEFAULT 0
        CONSTRAINT services_renotify_chk CHECK (renotify_seconds >= 0);

-- It is PAGING configuration, so it belongs to the alerting generation like every other paging
-- field: changing it must dis-arm delegation until the new generation has been evaluated, or a
-- service would keep coverage granted under a cadence that no longer exists. The generation is
-- owned by the DATABASE (00082) precisely so no write path has to remember this.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION service_alert_config_bump() RETURNS trigger AS $$
BEGIN
    IF NEW.owns_paging IS DISTINCT FROM OLD.owns_paging
       OR NEW.page_on IS DISTINCT FROM OLD.page_on
       OR NEW.page_on_unknown IS DISTINCT FROM OLD.page_on_unknown
       OR NEW.confirm_evaluations IS DISTINCT FROM OLD.confirm_evaluations
       OR NEW.renotify_seconds IS DISTINCT FROM OLD.renotify_seconds THEN
        NEW.alert_config_generation := OLD.alert_config_generation + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION service_alert_config_bump() RETURNS trigger AS $$
BEGIN
    IF NEW.owns_paging IS DISTINCT FROM OLD.owns_paging
       OR NEW.page_on IS DISTINCT FROM OLD.page_on
       OR NEW.page_on_unknown IS DISTINCT FROM OLD.page_on_unknown
       OR NEW.confirm_evaluations IS DISTINCT FROM OLD.confirm_evaluations THEN
        NEW.alert_config_generation := OLD.alert_config_generation + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
ALTER TABLE services DROP COLUMN IF EXISTS renotify_seconds;
