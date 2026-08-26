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

-- Rows an old producer already wrote in the legacy class during the rollout window.
--
-- `claim_token` is REPLACED as part of the promotion, because a row can already be claimed: an old
-- worker that took it before this migration ran still holds the previous token, and without this its
-- `MarkOutboxDelivered` would win the CAS and mark the row done. Replacing the token cannot recall an
-- external call that worker may already have made — nothing here can, see D-0177 — but it stops a
-- pre-barrier claim from also settling the row and releasing the successor behind it.
UPDATE outbox_events
   SET status = 'pending_fenced', fenced = true, claim_token = gen_random_uuid()
 WHERE topic IN ('incident_correlation', 'service_alert', 'incident_event')
   AND status = 'pending';

-- DEAD rows need the flag too, and they are not covered above. `ReplayDeadOutbox` restores a row's
-- claimable class from the PERSISTED `fenced` column, so a dead pre-barrier row replayed months from
-- now would come back as legacy `pending` — straight into the hands of exactly the worker the barrier
-- exists to keep away from it. The status stays `dead`: this is about which class it returns to, not
-- about resurrecting it.
UPDATE outbox_events
   SET fenced = true
 WHERE topic IN ('incident_correlation', 'service_alert', 'incident_event')
   AND status = 'dead'
   AND NOT fenced;

-- +goose Down
DROP TRIGGER IF EXISTS outbox_enforce_fenced_class_trg ON outbox_events;
DROP FUNCTION IF EXISTS outbox_enforce_fenced_class();
