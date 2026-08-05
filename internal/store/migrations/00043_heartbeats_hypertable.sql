-- +goose NO TRANSACTION
-- Adaptive TimescaleDB conversion for heartbeats. When the timescaledb extension
-- is installed (our deploy images auto-create it), the natively-partitioned
-- heartbeats table is rebuilt as a hypertable with native compression
-- (segment by monitor, ~10-20x on this telemetry). Without the extension
-- (vanilla Postgres, RDS, CI) this migration is a no-op and heartbeats stays on
-- declarative daily partitions — the store branches its partition maintenance at
-- runtime. The migration never CREATEs the extension itself: attempting that
-- without shared_preload_libraries is a FATAL that kills the connection, so
-- enabling it is the operator's (or the docker image's) job.
--
-- NO TRANSACTION: each DO block below is its own atomic statement, so a failed
-- conversion rolls back cleanly and the guards make a re-run idempotent.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        RAISE NOTICE 'timescaledb not installed — heartbeats stays on declarative partitions';
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM timescaledb_information.hypertables
               WHERE hypertable_name = 'heartbeats') THEN
        RAISE NOTICE 'heartbeats is already a hypertable — nothing to do';
        RETURN;
    END IF;

    -- A natively-partitioned table cannot be converted in place: build a plain
    -- twin, make it a hypertable, copy, then swap. Daily chunks mirror the old
    -- daily partitions so retention keeps day granularity.
    CREATE TABLE heartbeats_new (
        monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
        ts         timestamptz NOT NULL DEFAULT now(),
        up         boolean NOT NULL,
        latency_ms bigint NOT NULL DEFAULT 0,
        code       int NOT NULL DEFAULT 0,
        msg        text NOT NULL DEFAULT ''
    );
    PERFORM create_hypertable('heartbeats_new', 'ts',
                              chunk_time_interval => INTERVAL '1 day');
    -- ON CONFLICT (monitor_id, ts) DO NOTHING depends on this; it includes the
    -- partitioning column, which hypertables require of unique indexes.
    CREATE UNIQUE INDEX heartbeats_new_monitor_ts_key
        ON heartbeats_new (monitor_id, ts);

    INSERT INTO heartbeats_new (monitor_id, ts, up, latency_ms, code, msg)
        SELECT monitor_id, ts, up, latency_ms, code, msg FROM heartbeats;

    DROP TABLE heartbeats; -- drops the partitioned parent and all partitions
    ALTER TABLE heartbeats_new RENAME TO heartbeats;
    ALTER INDEX heartbeats_new_monitor_ts_key RENAME TO heartbeats_monitor_ts_key;
    ALTER INDEX IF EXISTS heartbeats_new_ts_idx RENAME TO heartbeats_ts_idx;

    -- Native compression: segment by monitor (queries always filter/aggregate
    -- per monitor), order by ts DESC (matches last-N reads). compress_after 7d
    -- keeps the agent edge-buffer backfill window (hours) far inside the
    -- uncompressed tail; late inserts into compressed chunks still work
    -- (TS >= 2.11), just slower.
    ALTER TABLE heartbeats SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'monitor_id',
        timescaledb.compress_orderby = 'ts DESC'
    );
    PERFORM add_compression_policy('heartbeats', compress_after => INTERVAL '7 days');
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE d date;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')
       OR NOT EXISTS (SELECT 1 FROM timescaledb_information.hypertables
                      WHERE hypertable_name = 'heartbeats') THEN
        RETURN; -- declarative mode: 00043 was a no-op, nothing to undo
    END IF;

    -- Decompress everything first, then rebuild the declarative layout of 00017.
    PERFORM remove_compression_policy('heartbeats', if_exists => true);
    PERFORM decompress_chunk(c, if_compressed => true)
       FROM show_chunks('heartbeats') c;

    CREATE TABLE heartbeats_plain (
        monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
        ts         timestamptz NOT NULL DEFAULT now(),
        up         boolean NOT NULL,
        latency_ms bigint NOT NULL DEFAULT 0,
        code       int NOT NULL DEFAULT 0,
        msg        text NOT NULL DEFAULT ''
    ) PARTITION BY RANGE (ts);
    CREATE TABLE heartbeats_plain_default PARTITION OF heartbeats_plain DEFAULT;
    FOR d IN SELECT DISTINCT (ts AT TIME ZONE 'UTC')::date FROM heartbeats LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS heartbeats_p%s PARTITION OF heartbeats_plain FOR VALUES FROM (%L) TO (%L)',
            to_char(d, 'YYYYMMDD'),
            (d::timestamp AT TIME ZONE 'UTC'),
            ((d + 1)::timestamp AT TIME ZONE 'UTC'));
    END LOOP;
    INSERT INTO heartbeats_plain (monitor_id, ts, up, latency_ms, code, msg)
        SELECT monitor_id, ts, up, latency_ms, code, msg FROM heartbeats;

    DROP TABLE heartbeats;
    ALTER TABLE heartbeats_plain RENAME TO heartbeats;
    ALTER TABLE heartbeats_plain_default RENAME TO heartbeats_default;
    CREATE UNIQUE INDEX heartbeats_monitor_ts_key ON heartbeats (monitor_id, ts);
END $$;
-- +goose StatementEnd
