-- +goose Up
-- The fenced class becomes the DATABASE's rule, not the producing binary's.
--
-- `enqueueOutboxTx` decides the class from `domain.FencedTopics()` — of whichever binary is running.
-- During a rolling upgrade an OLD api or scheduler is still inserting, still says
-- `FencedTopic('incident_event') == false`, and writes a legacy row. The consumer-side fence then
-- protects nothing for exactly the window it was built for: an old worker claims those rows with the
-- old claim and delivers an incident's events in whatever order it likes.
--
-- A trigger settles it for every producer, of every version, including ones nobody has written yet.
-- The topic list is duplicated from Go on purpose and guarded by
-- `TestOutboxFencedClassMatchesTheBinary`, the same instrument the topic whitelist already has: two
-- copies with a gate beat one copy that only one side can enforce.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION outbox_enforce_fenced_class() RETURNS trigger AS $$
BEGIN
    IF NEW.topic IN ('incident_correlation', 'service_alert', 'incident_event') THEN
        -- Only the PENDING class is rewritten: a row inserted as delivered or dead by a repair path
        -- is not a claim decision and is left exactly as written.
        IF NEW.status = 'pending' THEN
            NEW.status := 'pending_fenced';
        END IF;
        NEW.fenced := true;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER outbox_enforce_fenced_class_trg
    BEFORE INSERT ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_enforce_fenced_class();

-- Rows an old producer already wrote in the legacy class during the rollout window: same treatment,
-- once. They are undelivered and unclaimed-by-anyone-capable, so promoting them is safe and is the
-- difference between "ordered after convergence" and "ordered".
UPDATE outbox_events
   SET status = 'pending_fenced', fenced = true
 WHERE topic IN ('incident_correlation', 'service_alert', 'incident_event')
   AND status = 'pending';

-- +goose Down
DROP TRIGGER IF EXISTS outbox_enforce_fenced_class_trg ON outbox_events;
DROP FUNCTION IF EXISTS outbox_enforce_fenced_class();
