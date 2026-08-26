-- +goose Up
-- +goose NO TRANSACTION

-- Repair of DURABLE state, not of future writes (D-0182).
--
-- The lifecycle fixes earlier in this arc stopped new damage: a resolved incident is terminal in the
-- WRITE, a service incident resolves with its episode, and a member snapshot names the declaration
-- that governed. None of them touched the rows already written by the versions that did not. A
-- future-write test is not evidence for a defect that has already written customer history.
--
-- Two classes are repaired here because the correct value is DERIVABLE from the row itself. Two more
-- are only COUNTED, because repairing them would mean guessing at history, and the runbook carries
-- what to do about those instead. NO TRANSACTION plus guarded idempotent blocks is the house pattern
-- for a DDL-and-data migration (00043).

-- +goose StatementBegin
DO $$
DECLARE
    resurrected  bigint;
    unstamped    bigint;
    suspect_rev  bigint;
    orphaned     bigint;
    stranded     bigint;
BEGIN
    -- (1) RESURRECTED rows: `resolved_at` is set and the status walked backwards off `resolved`.
    -- Before the CAS landed, a writer holding a status read from before somebody else's resolve
    -- could commit it afterwards. The row then occupies the partial unique indexes that allow ONE
    -- open incident per subject, so the NEXT outage cannot open one and the second failure is
    -- invisible — which is the reason this is repaired rather than merely reported.
    --
    -- `resolved_at` is the durable fact here: it is only ever stamped BY a resolve, and no path
    -- clears it. The status is the field that lied.
    SELECT count(*) INTO resurrected
      FROM incidents WHERE resolved_at IS NOT NULL AND status <> 'resolved';

    IF resurrected > 0 THEN
        INSERT INTO incident_updates (incident_id, status, body, author, created_at)
        SELECT i.id, 'resolved',
               '🔧 Repaired: this incident carried a resolution time while its status had moved back '
               || 'off resolved, which is a state no lifecycle produces. The status was corrected to '
               || 'match the resolution already recorded.',
               'system', now()
          FROM incidents i
         WHERE i.resolved_at IS NOT NULL AND i.status <> 'resolved'
           -- The idempotency marker pattern: a repeated run adds no second note.
           AND NOT EXISTS (SELECT 1 FROM incident_updates u
                            WHERE u.incident_id = i.id AND u.body LIKE '🔧 Repaired:%');

        UPDATE incidents SET status = 'resolved', updated_at = now()
         WHERE resolved_at IS NOT NULL AND status <> 'resolved';
    END IF;
    RAISE NOTICE 'incident repair: % resurrected row(s) resolved', resurrected;

    -- (2) The mirror: `resolved` with no resolution time. Older code could reach it, and the CHECK
    -- below is a biconditional. `updated_at` is the closest durable instant the row still carries —
    -- it is not invented, and it is never later than the resolve that set the status.
    SELECT count(*) INTO unstamped
      FROM incidents WHERE status = 'resolved' AND resolved_at IS NULL;
    UPDATE incidents SET resolved_at = updated_at
     WHERE status = 'resolved' AND resolved_at IS NULL;
    RAISE NOTICE 'incident repair: % resolved row(s) stamped from updated_at', unstamped;

    -- (3) STRANDED auto-incidents for a service that is demonstrably not in trouble: the health
    -- episode they belong to is closed (or never existed) and the service is not firing now. Before
    -- the lifecycle close landed, disowning a service ended its ALERT and left its incident open
    -- forever — the operator reads "investigating" on a service that recovered hours ago, and the
    -- per-service index blocks the next one.
    --
    -- Deliberately narrow: the service must still EXIST and must not be firing, so nothing here can
    -- close an incident for an outage that is still happening.
    SELECT count(*) INTO stranded
      FROM incidents i
      JOIN services s ON s.id = i.service_id
      LEFT JOIN service_alert_state st ON st.service_id = s.id
     WHERE i.source = 'auto' AND i.status <> 'resolved' AND i.service_id IS NOT NULL
       AND COALESCE(st.live_firing, false) = false
       AND NOT EXISTS (SELECT 1 FROM service_alert_episodes e
                        WHERE e.service_id = s.id AND e.signal = 'health' AND e.closed_at IS NULL);

    IF stranded > 0 THEN
        INSERT INTO incident_updates (incident_id, status, body, author, created_at)
        SELECT i.id, 'resolved',
               '🔧 Repaired: the alert this incident was opened for has ended and the service is not '
               || 'firing. Earlier versions ended the alert without ending the incident, which also '
               || 'blocked the next outage from opening one.',
               'system', now()
          FROM incidents i
          JOIN services s ON s.id = i.service_id
          LEFT JOIN service_alert_state st ON st.service_id = s.id
         WHERE i.source = 'auto' AND i.status <> 'resolved' AND i.service_id IS NOT NULL
           AND COALESCE(st.live_firing, false) = false
           AND NOT EXISTS (SELECT 1 FROM service_alert_episodes e
                            WHERE e.service_id = s.id AND e.signal = 'health' AND e.closed_at IS NULL)
           AND NOT EXISTS (SELECT 1 FROM incident_updates u
                            WHERE u.incident_id = i.id AND u.body LIKE '🔧 Repaired:%');

        UPDATE incidents i SET status = 'resolved', resolved_at = now(), updated_at = now()
          FROM services s
          LEFT JOIN service_alert_state st ON st.service_id = s.id
         WHERE s.id = i.service_id
           AND i.source = 'auto' AND i.status <> 'resolved' AND i.service_id IS NOT NULL
           AND COALESCE(st.live_firing, false) = false
           AND NOT EXISTS (SELECT 1 FROM service_alert_episodes e
                            WHERE e.service_id = s.id AND e.signal = 'health' AND e.closed_at IS NULL);
    END IF;
    RAISE NOTICE 'incident repair: % stranded service incident(s) resolved', stranded;

    -- (4) BOUNDED, NOT REPAIRED, and not even provably identified — member snapshots that may have
    -- been taken from a revision which had not started governing.
    --
    -- Two things prevent a repair, and the second also prevents a diagnosis. `incident_member_snapshots`
    -- stores the member LIST and no revision id, so a wrong snapshot cannot be recognised by
    -- inspection: there is nothing in the row saying where it came from. And the obvious substitute —
    -- "rebuild from the revision effective at `started_at`" — is the guess the review specifically
    -- warned against: `started_at` defaults to the TRANSACTION clock while the evaluator selected its
    -- revision at `statement_timestamp()`, so a transaction crossing a boundary can carry a
    -- `started_at` earlier than the revision that genuinely governed. Rewriting immutable customer
    -- history on that would be worse than leaving it.
    --
    -- What is computable is the BOUND: open service incidents whose service had a revision take
    -- effect after they began. Every affected row is in that set; so are rows that are perfectly
    -- correct. It is reported as a bound and named as one, and the runbook says what an operator can
    -- do with the list.
    SELECT count(*) INTO suspect_rev
      FROM incident_member_snapshots ms
      JOIN incidents i ON i.id = ms.incident_id
     WHERE i.service_id IS NOT NULL
       AND EXISTS (SELECT 1 FROM service_definition_revisions r
                    WHERE r.service_id = i.service_id AND r.effective_at > i.started_at);
    IF suspect_rev > 0 THEN
        RAISE WARNING 'incident repair: % member snapshot(s) COULD have been taken from a revision '
                      'that was not yet governing — an upper bound, not a defect count, and not '
                      'repairable from the stored data. See docs/runbook.md (D-0182)', suspect_rev;
    END IF;

    -- (5) COUNTED, NOT REPAIRED — auto-incidents with NO anchor at all. Both the monitor and the
    -- service are gone, so nothing identifies what they were about. Guessing an owner would attach
    -- somebody else's history to them.
    SELECT count(*) INTO orphaned
      FROM incidents
     WHERE source = 'auto' AND status <> 'resolved'
       AND service_id IS NULL AND monitor_id IS NULL;
    IF orphaned > 0 THEN
        RAISE WARNING 'incident repair: % anchorless auto-incident(s) remain open — NOT repaired, '
                      'see docs/runbook.md (D-0182)', orphaned;
    END IF;
END
$$;
-- +goose StatementEnd

-- The invariant the repair above establishes, held by the DATABASE from here on. Both directions:
-- every path that sets `resolved` stamps the time, and no path un-resolves. It is a CHECK and not a
-- comment because "resolved is terminal" had been true in a prior READ and false in the write.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'incidents_resolved_stamp_chk') THEN
        ALTER TABLE incidents ADD CONSTRAINT incidents_resolved_stamp_chk
            CHECK ((status = 'resolved') = (resolved_at IS NOT NULL));
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose NO TRANSACTION
-- +goose StatementBegin
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_resolved_stamp_chk;
-- +goose StatementEnd
