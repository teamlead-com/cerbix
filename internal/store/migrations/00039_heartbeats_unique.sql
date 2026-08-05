-- +goose Up
-- Idempotent heartbeats: a unique (monitor_id, ts) lets both the live insert and the
-- pull-agent historical backfill use ON CONFLICT DO NOTHING, so a replayed/duplicated
-- heartbeat (e.g. a partial-success post retried from an agent's edge buffer) is not
-- double-counted in SLA. The unique index includes ts (the partition key), as required
-- for a unique index on a range-partitioned table; it also serves the ts-ordered reads,
-- so the old non-unique index is dropped.
-- Dedupe any pre-existing exact (monitor_id, ts) duplicates first so the index builds.
DELETE FROM heartbeats a USING heartbeats b
 WHERE a.ctid < b.ctid AND a.monitor_id = b.monitor_id AND a.ts = b.ts;
DROP INDEX IF EXISTS heartbeats_monitor_ts_idx;
CREATE UNIQUE INDEX heartbeats_monitor_ts_key ON heartbeats (monitor_id, ts);

-- +goose Down
DROP INDEX IF EXISTS heartbeats_monitor_ts_key;
CREATE INDEX heartbeats_monitor_ts_idx ON heartbeats (monitor_id, ts DESC);
