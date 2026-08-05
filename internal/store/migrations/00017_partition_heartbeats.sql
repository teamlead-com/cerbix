-- +goose Up
-- Convert heartbeats to a RANGE-partitioned table on ts (daily partitions) so raw
-- retention is a cheap DROP of whole day-partitions instead of a large DELETE. A
-- DEFAULT partition guarantees inserts never fail even if the partition-maintenance
-- job falls behind; the scheduler leader pre-creates upcoming daily partitions and
-- drops those older than the retention window. Postgres runs UTC, so day bounds are
-- UTC-aligned to match the daily rollup.

ALTER TABLE heartbeats RENAME TO heartbeats_old;
-- Renaming the table keeps its index name, which would collide with the new one.
ALTER INDEX heartbeats_monitor_ts_idx RENAME TO heartbeats_old_monitor_ts_idx;

CREATE TABLE heartbeats (
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    ts         timestamptz NOT NULL DEFAULT now(),
    up         boolean NOT NULL,
    latency_ms bigint NOT NULL DEFAULT 0,
    code       int NOT NULL DEFAULT 0,
    msg        text NOT NULL DEFAULT ''
) PARTITION BY RANGE (ts);

CREATE INDEX heartbeats_monitor_ts_idx ON heartbeats (monitor_id, ts DESC);

CREATE TABLE heartbeats_default PARTITION OF heartbeats DEFAULT;

-- Create a daily partition for each UTC day present in the old data so the copied
-- rows land in droppable dated partitions rather than the default.
-- +goose StatementBegin
DO $$
DECLARE d date;
BEGIN
    FOR d IN SELECT DISTINCT (ts AT TIME ZONE 'UTC')::date AS day FROM heartbeats_old LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS heartbeats_p%s PARTITION OF heartbeats FOR VALUES FROM (%L) TO (%L)',
            to_char(d, 'YYYYMMDD'),
            (d::timestamp AT TIME ZONE 'UTC'),
            ((d + 1)::timestamp AT TIME ZONE 'UTC'));
    END LOOP;
END $$;
-- +goose StatementEnd

INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg)
    SELECT monitor_id, ts, up, latency_ms, code, msg FROM heartbeats_old;

DROP TABLE heartbeats_old;

-- +goose Down
ALTER TABLE heartbeats RENAME TO heartbeats_part;
ALTER INDEX heartbeats_monitor_ts_idx RENAME TO heartbeats_part_monitor_ts_idx;

CREATE TABLE heartbeats (
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    ts         timestamptz NOT NULL DEFAULT now(),
    up         boolean NOT NULL,
    latency_ms bigint NOT NULL DEFAULT 0,
    code       int NOT NULL DEFAULT 0,
    msg        text NOT NULL DEFAULT ''
);
CREATE INDEX heartbeats_monitor_ts_idx ON heartbeats (monitor_id, ts DESC);

INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg)
    SELECT monitor_id, ts, up, latency_ms, code, msg FROM heartbeats_part;

DROP TABLE heartbeats_part;
