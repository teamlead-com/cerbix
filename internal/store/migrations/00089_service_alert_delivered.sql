-- +goose Up
-- +goose StatementBegin

-- Coverage means somebody was actually TOLD, not that an announcement was enqueued (D-0179).
--
-- The swallow this closes: the evaluator sees a route, enqueues the onset and latches firing; the
-- channel is deleted before the worker delivers; the worker resolves zero recipients, counts it and
-- terminally succeeds, because no retry can fix a channel that is gone. The latch still says firing,
-- so the next evaluation sees no edge and announces nothing. When the route comes back, coverage
-- re-arms against that latch and the member monitors fall silent for an onset NOBODY received.
--
-- `delivered_seq` is advanced by the outbox worker only when a delivery resolved at least one
-- recipient, and the arming conjunction requires `delivered_seq >= emitted_seq`. Until then members
-- keep paging for themselves, which is the direction §16.1 always takes.
ALTER TABLE service_alert_state
    ADD COLUMN IF NOT EXISTS delivered_seq bigint NOT NULL DEFAULT 0;
ALTER TABLE service_burn_alert_state
    ADD COLUMN IF NOT EXISTS delivered_seq bigint NOT NULL DEFAULT 0;

-- The UPGRADE is deliberately conservative in the OTHER direction, and this is the one place where
-- it is (D-0179). Rows that already exist carry no delivery evidence — the column did not exist when
-- they were written — so a truthful default of 0 would declare every armed service undelivered at
-- once, and every member monitor of every covered service would start paging in the same minute. An
-- upgrade that pages an entire installation is not a safety improvement.
--
-- So existing rows inherit `delivered_seq = emitted_seq`: they keep the coverage they had, and the
-- new rule governs from the first announcement after the upgrade. The window this leaves open is
-- exactly one already-emitted onset per latch, and it closes at that latch's next edge.
UPDATE service_alert_state SET delivered_seq = emitted_seq WHERE emitted_seq > 0;
UPDATE service_burn_alert_state SET delivered_seq = emitted_seq WHERE emitted_seq > 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE service_alert_state DROP COLUMN IF EXISTS delivered_seq;
ALTER TABLE service_burn_alert_state DROP COLUMN IF EXISTS delivered_seq;
-- +goose StatementEnd
