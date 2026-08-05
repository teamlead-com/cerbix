package store

import (
	"context"
	"fmt"
	"time"
)

// DailyAvailability is one day's uptime for a monitor or project. Days are UTC.
type DailyAvailability struct {
	Day           time.Time `json:"day"`
	Up            int64     `json:"up"`
	Total         int64     `json:"total"`
	UptimePercent float64   `json:"uptime_percent"`
}

// RollupDailyAvailability recomputes per-(monitor, day) heartbeat aggregates for
// the completed days in [from, to), excluding maintenance windows. It deletes and
// re-inserts the range in one transaction so a day that dropped to zero (e.g. a
// retroactive maintenance window) is removed, not left stale. Callers pass
// to = start-of-today so today (still accumulating) is read live, not rolled up.
func (s *Store) RollupDailyAvailability(ctx context.Context, from, to time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin rollup: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx,
		`DELETE FROM heartbeats_daily WHERE day >= ($1 AT TIME ZONE 'UTC')::date AND day < ($2 AT TIME ZONE 'UTC')::date`,
		from, to); err != nil {
		return fmt.Errorf("store: rollup clear range: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO heartbeats_daily (monitor_id, day, up, total)
		 SELECT h.monitor_id,
		        (h.ts AT TIME ZONE 'UTC')::date AS day,
		        count(*) FILTER (WHERE h.up)::bigint,
		        count(*)::bigint
		 FROM heartbeats h
		 WHERE h.ts >= $1 AND h.ts < $2
		   AND NOT EXISTS (
		       SELECT 1 FROM maintenance_windows mw
		       WHERE mw.starts_at <= h.ts AND mw.ends_at > h.ts
		         AND (mw.monitor_id = h.monitor_id
		              OR (mw.monitor_id IS NULL
		                  AND mw.project_id = (SELECT project_id FROM monitors WHERE id = h.monitor_id)))
		   )
		 GROUP BY h.monitor_id, day`,
		from, to); err != nil {
		return fmt.Errorf("store: rollup insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit rollup: %w", err)
	}
	return nil
}

func scanDaily(rows interface{ Scan(...any) error }) (DailyAvailability, error) {
	var d DailyAvailability
	if err := rows.Scan(&d.Day, &d.Up, &d.Total); err != nil {
		return DailyAvailability{}, err
	}
	if d.Total > 0 {
		d.UptimePercent = float64(d.Up) / float64(d.Total) * 100
	}
	return d, nil
}

// MonitorDailyAvailability returns a monitor's per-day availability since the
// given day: completed days from the rollup plus today computed live from raw
// heartbeats (maintenance excluded), oldest first.
func (s *Store) MonitorDailyAvailability(ctx context.Context, monitorID string, since time.Time) ([]DailyAvailability, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT day, up, total FROM heartbeats_daily
		 WHERE monitor_id = $1 AND day >= $2::date AND day < (now() AT TIME ZONE 'UTC')::date
		 ORDER BY day`,
		monitorID, since)
	if err != nil {
		return nil, fmt.Errorf("store: monitor daily availability: %w", err)
	}
	out, err := collectDaily(rows)
	if err != nil {
		return nil, err
	}
	today, ok, err := s.monitorTodayAvailability(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	if ok {
		out = append(out, today)
	}
	return out, nil
}

func (s *Store) monitorTodayAvailability(ctx context.Context, monitorID string) (DailyAvailability, bool, error) {
	var d DailyAvailability
	err := s.pool.QueryRow(ctx,
		`SELECT (now() AT TIME ZONE 'UTC')::date,
		        count(*) FILTER (WHERE h.up)::bigint, count(*)::bigint
		 FROM heartbeats h
		 WHERE h.monitor_id = $1 AND h.ts >= (now() AT TIME ZONE 'UTC')::date
		   AND NOT EXISTS (
		       SELECT 1 FROM maintenance_windows mw
		       WHERE mw.starts_at <= h.ts AND mw.ends_at > h.ts
		         AND (mw.monitor_id = $1
		              OR (mw.monitor_id IS NULL
		                  AND mw.project_id = (SELECT project_id FROM monitors WHERE id = $1)))
		   )`, monitorID).Scan(&d.Day, &d.Up, &d.Total)
	if err != nil {
		return DailyAvailability{}, false, fmt.Errorf("store: monitor today availability: %w", err)
	}
	if d.Total == 0 {
		return DailyAvailability{}, false, nil
	}
	d.UptimePercent = float64(d.Up) / float64(d.Total) * 100
	return d, true, nil
}

// ProjectDailyAvailability returns a project's per-day availability (summed over
// its monitors), completed days from the rollup plus today from raw.
func (s *Store) ProjectDailyAvailability(ctx context.Context, projectID string, since time.Time) ([]DailyAvailability, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT hd.day, sum(hd.up)::bigint, sum(hd.total)::bigint
		 FROM heartbeats_daily hd JOIN monitors m ON m.id = hd.monitor_id
		 WHERE m.project_id = $1 AND hd.day >= $2::date AND hd.day < (now() AT TIME ZONE 'UTC')::date
		 GROUP BY hd.day ORDER BY hd.day`,
		projectID, since)
	if err != nil {
		return nil, fmt.Errorf("store: project daily availability: %w", err)
	}
	out, err := collectDaily(rows)
	if err != nil {
		return nil, err
	}
	var d DailyAvailability
	err = s.pool.QueryRow(ctx,
		`SELECT (now() AT TIME ZONE 'UTC')::date,
		        count(*) FILTER (WHERE h.up)::bigint, count(*)::bigint
		 FROM heartbeats h JOIN monitors m ON m.id = h.monitor_id
		 WHERE m.project_id = $1 AND h.ts >= (now() AT TIME ZONE 'UTC')::date
		   AND NOT EXISTS (
		       SELECT 1 FROM maintenance_windows mw
		       WHERE mw.starts_at <= h.ts AND mw.ends_at > h.ts
		         AND (mw.monitor_id = h.monitor_id OR (mw.monitor_id IS NULL AND mw.project_id = $1))
		   )`, projectID).Scan(&d.Day, &d.Up, &d.Total)
	if err != nil {
		return nil, fmt.Errorf("store: project today availability: %w", err)
	}
	if d.Total > 0 {
		d.UptimePercent = float64(d.Up) / float64(d.Total) * 100
		out = append(out, d)
	}
	return out, nil
}

func collectDaily(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]DailyAvailability, error) {
	defer rows.Close()
	var out []DailyAvailability
	for rows.Next() {
		d, err := scanDaily(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan daily availability: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate daily availability: %w", err)
	}
	return out, nil
}
