package store

import (
	"context"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §15.0 — the MONITOR half of a status page, batched ([314] P1-3).
//
// The shipped renderer read every monitor component with three separate per-component queries
// (`GetMonitor`, `MonitorSLI`, `MonitorDailyAvailability`). I first argued that this was
// pre-existing behaviour and therefore out of scope; the honest reading is that phase 4's
// acceptance claims the unauthenticated render is bounded, so amplification left in place is
// amplification claimed to be gone. Three statements for the whole page, whatever it holds.
//
// The maintenance exclusion is written the same way the shipped per-monitor queries write it — a
// heartbeat under a window is left out of BOTH the numerator and the denominator — because a page
// that counted maintenance differently from the monitor's own SLA view would be two answers about
// one monitor.

// MonitorPageProjection is everything a monitor-backed component needs.
type MonitorPageProjection struct {
	MonitorID string
	ProjectID string
	Status    domain.MonitorStatus
	// Uptime is the 90-day uptime percentage over non-maintenance heartbeats, or nil when the
	// monitor produced no heartbeat at all in the window — never 0% standing in for silence.
	Uptime *float64
	// UptimeSince is the UTC day Uptime's data starts when the window is incomplete (D-0268).
	UptimeSince *time.Time
	Daily       []DailyAvailability
}

// MonitorPageProjections resolves every monitor-backed component on a page in a fixed number of
// statements whatever it holds: identity, the 90-day aggregate (raw and rollup parts in one
// statement), and the day strips.
func (s *Store) MonitorPageProjections(
	ctx context.Context, monitorIDs []string, since time.Time, withHistory bool,
) (map[string]MonitorPageProjection, error) {
	out := make(map[string]MonitorPageProjection, len(monitorIDs))
	ids := dedupe(monitorIDs)
	if len(ids) == 0 {
		return out, nil
	}

	// (1) Identity and live status. A monitor that is gone simply does not come back, and the
	// caller renders that as the deleted-monitor case rather than as health.
	rows, err := s.pool.Query(ctx,
		`SELECT id, project_id, status FROM monitors WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: monitor page status: %w", err)
	}
	for rows.Next() {
		var p MonitorPageProjection
		var status string
		if err := rows.Scan(&p.MonitorID, &p.ProjectID, &status); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan monitor page status: %w", err)
		}
		p.Status = domain.MonitorStatus(status)
		out[p.MonitorID] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 || !withHistory {
		return out, nil
	}

	// (2) The 90-day aggregate for every monitor at once — raw heartbeats in the window plus the
	// rollup days older than raw retention, with the SAME rule the monitor's own SLA view uses
	// (D-0268), in ONE statement so the raw counts and the rollup boundary come from one snapshot.
	ceil, day, today := windowDays(since)
	rows, err = s.pool.Query(ctx, `
		WITH scope AS (SELECT unnest($1::uuid[]) AS id), `+windowRollupCTEs(2, 3, 4, 5)+`,
		raw AS (
		    SELECT h.monitor_id AS id,
		           count(*)::bigint AS total,
		           count(*) FILTER (WHERE h.up)::bigint AS up,
		           min(h.ts) AS first_ts
		      FROM heartbeats h
		     WHERE h.monitor_id = ANY($1) AND h.ts >= $2
		       AND NOT EXISTS (
		           SELECT 1 FROM maintenance_windows mw
		            WHERE mw.starts_at <= h.ts AND `+maintEffectiveEnd+` > h.ts
		              AND (mw.monitor_id = h.monitor_id
		                   OR (mw.monitor_id IS NULL
		                       AND mw.project_id = (SELECT m.project_id FROM monitors m WHERE m.id = h.monitor_id)))
		       )
		     GROUP BY h.monitor_id
		)
		SELECT sc.id,
		       COALESCE(raw.total, 0), COALESCE(raw.up, 0), raw.first_ts,
		       COALESCE(roll.total, 0), COALESCE(roll.up, 0),
		       (roll.first_day::timestamp AT TIME ZONE 'UTC'),
		       COALESCE(cov.complete, false)
		  FROM scope sc
		  LEFT JOIN raw ON raw.id = sc.id
		  LEFT JOIN roll ON roll.id = sc.id
		  LEFT JOIN cov ON cov.id = sc.id`, ids, since, ceil, day, today)
	if err != nil {
		return nil, fmt.Errorf("store: monitor page sli: %w", err)
	}
	for rows.Next() {
		var id string
		var rawTotal, rawUp, rollTotal, rollUp int64
		var firstRaw, firstRollup *time.Time
		var complete bool
		if err := rows.Scan(&id, &rawTotal, &rawUp, &firstRaw, &rollTotal, &rollUp, &firstRollup, &complete); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan monitor page sli: %w", err)
		}
		p, ok := out[id]
		if !ok {
			continue
		}
		total, up, dataFrom, _ := windowCoverage(newRollupPart(rollTotal, rollUp, firstRollup, complete), rawTotal, rawUp, firstRaw)
		if total > 0 {
			u := float64(up) / float64(total) * 100
			p.Uptime = &u
			p.UptimeSince = dataFrom
		}
		out[id] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// (3) The strips: completed days from the rollup UNION today from raw, exactly the two sources
	// `MonitorDailyAvailability` uses per monitor — the same numbers, one statement.
	rows, err = s.pool.Query(ctx, `
		SELECT monitor_id, day, up, total FROM (
		    SELECT hd.monitor_id, hd.day, hd.up, hd.total
		      FROM heartbeats_daily hd
		     WHERE hd.monitor_id = ANY($1) AND hd.day >= $2::date
		       AND hd.day < (now() AT TIME ZONE 'UTC')::date
		    UNION ALL
		    SELECT h.monitor_id, (now() AT TIME ZONE 'UTC')::date AS day,
		           count(*) FILTER (WHERE h.up)::bigint, count(*)::bigint
		      FROM heartbeats h
		     WHERE h.monitor_id = ANY($1) AND h.ts >= (now() AT TIME ZONE 'UTC')::date
		       AND NOT EXISTS (
		           SELECT 1 FROM maintenance_windows mw
		            WHERE mw.starts_at <= h.ts AND `+maintEffectiveEnd+` > h.ts
		              AND (mw.monitor_id = h.monitor_id
		                   OR (mw.monitor_id IS NULL
		                       AND mw.project_id = (SELECT m.project_id FROM monitors m WHERE m.id = h.monitor_id)))
		       )
		     GROUP BY h.monitor_id
		    HAVING count(*) > 0
		) d ORDER BY monitor_id, day`, ids, since)
	if err != nil {
		return nil, fmt.Errorf("store: monitor page daily: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var d DailyAvailability
		if err := rows.Scan(&id, &d.Day, &d.Up, &d.Total); err != nil {
			return nil, fmt.Errorf("store: scan monitor page daily: %w", err)
		}
		if d.Total > 0 {
			d.UptimePercent = float64(d.Up) / float64(d.Total) * 100
		}
		p, ok := out[id]
		if !ok {
			continue
		}
		p.Daily = append(p.Daily, d)
		out[id] = p
	}
	return out, rows.Err()
}
