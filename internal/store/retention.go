package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// heartbeatPartitionPrefix names the daily range partitions of heartbeats
// (heartbeats_pYYYYMMDD, UTC-aligned).
const heartbeatPartitionPrefix = "heartbeats_p"

// pgTimestamp formats a time as a Postgres timestamptz literal.
const pgTimestamp = "2006-01-02 15:04:05-07:00"

// EnsureHeartbeatPartitions creates daily partitions for the UTC days in
// [today, today+ahead], so heartbeat inserts always land in a dated (droppable)
// partition rather than the default. Best-effort: a day whose rows already sit in
// the default partition can't get a new partition — that day is left in the
// default (and purged by cutoff), reported as a joined error the caller may log.
// On a hypertable this is a no-op: TimescaleDB creates chunks on demand.
func (s *Store) EnsureHeartbeatPartitions(ctx context.Context, ahead int) error {
	if s.timescale {
		return nil
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var errs []error
	for i := 0; i <= ahead; i++ {
		day := today.AddDate(0, 0, i)
		name := heartbeatPartitionPrefix + day.Format("20060102")
		q := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF heartbeats FOR VALUES FROM ('%s') TO ('%s')`,
			name, day.Format(pgTimestamp), day.AddDate(0, 0, 1).Format(pgTimestamp))
		if _, err := s.pool.Exec(ctx, q); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// heartbeatPartitionNames lists the dated daily partitions of heartbeats
// (heartbeats_pYYYYMMDD), excluding the default partition. Single source for
// "which tables are the managed daily partitions", shared by retention and the
// test reset.
func (s *Store) heartbeatPartitionNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'heartbeats' AND c.relname ~ '^heartbeats_p[0-9]{8}$'`)
	if err != nil {
		return nil, fmt.Errorf("store: list heartbeat partitions: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: scan partition: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate partitions: %w", err)
	}
	return names, nil
}

// PurgeOldHeartbeats enforces raw retention: it drops every dated heartbeat
// partition whose whole day is older than cutoff (a cheap DDL DROP, not a big
// DELETE) and then clears any straggler rows from the default partition. Returns
// the number of partitions dropped. On a hypertable the same contract is served
// by drop_chunks, which drops whole day-chunks entirely before the cutoff —
// retention stays driven by the heartbeats.retention_days config, not by a
// TimescaleDB retention policy that would need syncing.
func (s *Store) PurgeOldHeartbeats(ctx context.Context, cutoff time.Time) (int, error) {
	cutoff = cutoff.UTC()
	if s.timescale {
		var dropped int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM drop_chunks('heartbeats', older_than => $1::timestamptz)`,
			cutoff,
		).Scan(&dropped); err != nil {
			return 0, fmt.Errorf("store: drop heartbeat chunks: %w", err)
		}
		return dropped, nil
	}
	names, err := s.heartbeatPartitionNames(ctx)
	if err != nil {
		return 0, err
	}

	dropped := 0
	for _, name := range names {
		day, err := time.ParseInLocation("20060102", strings.TrimPrefix(name, heartbeatPartitionPrefix), time.UTC)
		if err != nil {
			continue // not a dated partition we manage
		}
		// The partition covers [day, day+1); drop it only once its whole range is
		// before the cutoff. name is catalog-sourced and regex-validated, so the
		// identifier is safe to interpolate.
		if !day.AddDate(0, 0, 1).After(cutoff) {
			if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
				return dropped, fmt.Errorf("store: drop partition %s: %w", name, err)
			}
			dropped++
		}
	}
	// Clear any rows that leaked into the default partition (e.g. a gap when the
	// maintenance job wasn't running) and are now past retention.
	if _, err := s.pool.Exec(ctx, `DELETE FROM heartbeats_default WHERE ts < $1`, cutoff); err != nil {
		return dropped, fmt.Errorf("store: purge default partition: %w", err)
	}
	return dropped, nil
}
