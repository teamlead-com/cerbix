-- +goose Up
-- execution_revision is the monitor's CONFIG GENERATION (spec func-result-protocol §3),
-- distinct from updated_at (a row version that churns on every status write). It is bumped
-- ONLY by a semantic config change (UpdateMonitor), and a scheduled result carries the
-- revision it was produced under; RecordScheduledResult rejects a result whose revision no
-- longer matches, so a probe of an old configuration cannot mutate the new one. BIGINT, not
-- a timestamp; starts at 1.
ALTER TABLE monitors ADD COLUMN execution_revision bigint NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE monitors DROP COLUMN execution_revision;
