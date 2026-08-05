package store

import (
	"context"
	"encoding/json"
	"fmt"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// regionProject is one project's monitor count within a region.
type regionProject struct {
	projectID string
	count     int
}

// EvaluateRegionWorkerAlerts compares the worker-pool regions that currently have
// enabled, scheduled monitors against the set of regions with a live worker (from the
// caller's RabbitMQ management lookup) and enqueues an alert on each edge: a region
// that has been without a worker for at least graceSeconds (missing), or one that
// regained a worker after having alerted (recovered).
//
// State is latched in region_worker_alerts so an alert fires once per transition, not
// every tick; a row exists while a region is observed without a worker, and its
// notified_at is set once the alert has actually been sent (so the grace period
// suppresses a brief worker reconnect at startup). Because notification channels are
// per-project, one event is enqueued per affected project. Callers MUST pass a live set
// obtained from a successful lookup — never call this with a stale/empty set on lookup
// failure, or every region would appear missing.
func (s *Store) EvaluateRegionWorkerAlerts(ctx context.Context, live map[string]bool, graceSeconds int) (fired, resolved int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin region eval: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Regions with enabled, scheduled (non-push) monitors, broken down by project.
	rows, err := tx.Query(ctx,
		`SELECT region, project_id, count(*)::int
		   FROM monitors
		  WHERE enabled AND type <> 'push'
		  GROUP BY region, project_id`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: select region monitors: %w", err)
	}
	needed := map[string][]regionProject{}
	total := map[string]int{}
	for rows.Next() {
		var region, projectID string
		var count int
		if err := rows.Scan(&region, &projectID, &count); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scan region monitors: %w", err)
		}
		needed[region] = append(needed[region], regionProject{projectID: projectID, count: count})
		total[region] += count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: iterate region monitors: %w", err)
	}

	// Existing state, locked for this transaction.
	stateRows, err := tx.Query(ctx,
		`SELECT region, notified_at IS NOT NULL, (now() - since) >= make_interval(secs => $1)
		   FROM region_worker_alerts
		  FOR UPDATE`, graceSeconds)
	if err != nil {
		return 0, 0, fmt.Errorf("store: select region state: %w", err)
	}
	type regionState struct{ notified, gracePassed bool }
	state := map[string]regionState{}
	for stateRows.Next() {
		var region string
		var rs regionState
		if err := stateRows.Scan(&region, &rs.notified, &rs.gracePassed); err != nil {
			stateRows.Close()
			return 0, 0, fmt.Errorf("store: scan region state: %w", err)
		}
		state[region] = rs
	}
	stateRows.Close()
	if err := stateRows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: iterate region state: %w", err)
	}

	enqueue := func(region string, missing bool) error {
		for _, rp := range needed[region] {
			payload, err := json.Marshal(domain.RegionWorkerAlert{
				Region: region, ProjectID: rp.projectID, MonitorCount: rp.count, Missing: missing,
			})
			if err != nil {
				return fmt.Errorf("store: marshal region alert: %w", err)
			}
			if err := enqueueOutboxTx(ctx, tx, domain.TopicRegionWorkerAlert, payload); err != nil {
				return err
			}
		}
		return nil
	}

	for region := range needed {
		rs, tracked := state[region]
		if !live[region] {
			// Observed without a worker.
			if !tracked {
				// First observation: start the grace clock, do not alert yet.
				if _, err := tx.Exec(ctx,
					`INSERT INTO region_worker_alerts (region, missing) VALUES ($1, true)`, region); err != nil {
					return 0, 0, fmt.Errorf("store: track region missing: %w", err)
				}
				continue
			}
			if !rs.notified && rs.gracePassed {
				if err := enqueue(region, true); err != nil {
					return 0, 0, err
				}
				if _, err := tx.Exec(ctx,
					`UPDATE region_worker_alerts SET notified_at = now() WHERE region = $1`, region); err != nil {
					return 0, 0, fmt.Errorf("store: mark region notified: %w", err)
				}
				fired++
			}
			continue
		}
		// A worker is present: clear tracking, and if we had alerted, send recovery.
		if tracked {
			if rs.notified {
				if err := enqueue(region, false); err != nil {
					return 0, 0, err
				}
				resolved++
			}
			if _, err := tx.Exec(ctx, `DELETE FROM region_worker_alerts WHERE region = $1`, region); err != nil {
				return 0, 0, fmt.Errorf("store: clear region state: %w", err)
			}
		}
	}

	// Drop state for regions that no longer have any enabled monitors (nothing to
	// alert about, and no project to notify). Left as-is they would leak rows.
	for region := range state {
		if _, stillNeeded := needed[region]; !stillNeeded {
			if _, err := tx.Exec(ctx, `DELETE FROM region_worker_alerts WHERE region = $1`, region); err != nil {
				return 0, 0, fmt.Errorf("store: prune region state: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: commit region eval: %w", err)
	}
	return fired, resolved, nil
}
