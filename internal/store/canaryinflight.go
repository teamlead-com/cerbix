package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// FR-029 D9 / D9a — the in-flight record. Two rules, one table, one transaction:
//
//   - ONE execution per monitor. A canary holds its delivery for the whole journey, so dispatching
//     the same monitor twice would submit a SECOND external transaction — a side effect at someone
//     else's expense that no idempotency key can undo if the target does not honour one.
//   - At most N per REGION. A worker-local prefetch cannot bound a region with several executors, so
//     the count lives where dispatch is decided.
//
// Both are enforced at the scheduler's dispatch decision, which is already serialized by leader
// election. The rows are for CRASH RECOVERY and the executor's ack, not for mutual exclusion between
// schedulers, which is why a plain count inside the inserting transaction is enough.

// CanaryRegionLimit is the §4b bound: at most four canary executions in flight per region. It is a
// constant rather than a setting because a limit an operator can raise is a limit that stops being a
// limit on the day someone is in a hurry.
const CanaryRegionLimit = 4

// ErrCanaryMonitorInFlight and ErrCanaryRegionSaturated are the two refusals, distinct because they
// mean different things to an operator: the first is this monitor still running, the second is the
// region full of other people's journeys.
var (
	ErrCanaryMonitorInFlight = errors.New("store: this canary monitor is already in flight")
	ErrCanaryRegionSaturated = errors.New("store: the region is at its canary concurrency limit")
)

// ClaimCanaryInflight takes the slot for one scheduled run. `ttl` is the monitor's timeout plus the
// slack the specification fixes, so a crashed executor's slot returns on its own after a bounded
// wait rather than parking the monitor forever.
//
// Expired rows are cleared inside the same transaction: a lazy sweep is enough because the only
// reader is this claim, and a background job for it would be machinery with no second user.
func (s *Store) ClaimCanaryInflight(ctx context.Context, monitorID, region, runKey string, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("store: canary in-flight ttl must be positive")
	}
	// A slot keyed by nothing cannot be released by key, so it would park its monitor for the whole
	// TTL on every single run. Refusing here turns that silent stall into a loud
	// `in_flight_claim_failed`, which is what a caller that forgot to stamp the run deserves.
	if strings.TrimSpace(runKey) == "" {
		return fmt.Errorf("store: canary in-flight claim needs a run key")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin canary claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `DELETE FROM canary_inflight WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("store: expire canary in-flight: %w", err)
	}

	var live int
	if err := tx.QueryRow(ctx,
		`SELECT count(*)::int FROM canary_inflight WHERE region = $1`, region).Scan(&live); err != nil {
		return fmt.Errorf("store: count canary in-flight: %w", err)
	}

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM canary_inflight WHERE monitor_id = $1)`, monitorID).Scan(&exists); err != nil {
		return fmt.Errorf("store: check canary in-flight: %w", err)
	}
	if exists {
		return ErrCanaryMonitorInFlight
	}
	// The monitor's own slot is not one of the region's four twice: the cap is checked only for a
	// monitor that is about to TAKE a slot.
	if live >= CanaryRegionLimit {
		return ErrCanaryRegionSaturated
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO canary_inflight (monitor_id, region, run_key, expires_at)
		 VALUES ($1, $2, $3, now() + $4::interval)`,
		monitorID, region, runKey, ttl.String()); err != nil {
		return fmt.Errorf("store: claim canary in-flight: %w", err)
	}
	return tx.Commit(ctx)
}

// ReleaseCanaryInflight frees the slot THAT RUN took. A release for a run that no longer holds the
// row is a no-op rather than an error: the row may have expired while the executor was finishing,
// and failing here would turn a slow probe into an alert about cerbix.
//
// Keyed by (monitor, run) and not by monitor alone. Keyed by monitor alone, a late result from an
// expired run deletes the row a NEWER run is holding, and the next tick starts a third run beside
// the second — the exact concurrency the lease exists to forbid (reviewer P0-3).
func (s *Store) ReleaseCanaryInflight(ctx context.Context, monitorID, runKey string) error {
	if strings.TrimSpace(runKey) == "" {
		return nil // nothing to match: an unkeyed result releases nothing and the TTL applies
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM canary_inflight WHERE monitor_id = $1 AND run_key = $2`, monitorID, runKey); err != nil {
		return fmt.Errorf("store: release canary in-flight: %w", err)
	}
	return nil
}

// CanaryInflightRegions counts what is running per region, for the scheduler's own bookkeeping and
// for a metric that carries a REGION and never a monitor id.
func (s *Store) CanaryInflightRegions(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT region, count(*)::int FROM canary_inflight WHERE expires_at > now() GROUP BY region`)
	if err != nil {
		return nil, fmt.Errorf("store: canary in-flight by region: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var region string
		var n int
		if err := rows.Scan(&region, &n); err != nil {
			return nil, fmt.Errorf("store: scan canary in-flight: %w", err)
		}
		out[region] = n
	}
	return out, rows.Err()
}

// canaryInflightRow is what a test reads back; it exists so the table's shape is asserted through the
// product's own reader rather than through a query written twice.
type canaryInflightRow struct {
	MonitorID string
	Region    string
	RunKey    string
	ExpiresAt time.Time
}

func (s *Store) canaryInflightByMonitor(ctx context.Context, monitorID string) (canaryInflightRow, bool, error) {
	var r canaryInflightRow
	err := s.pool.QueryRow(ctx,
		`SELECT monitor_id, region, run_key, expires_at FROM canary_inflight WHERE monitor_id = $1`,
		monitorID).Scan(&r.MonitorID, &r.Region, &r.RunKey, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return canaryInflightRow{}, false, nil
	}
	if err != nil {
		return canaryInflightRow{}, false, fmt.Errorf("store: read canary in-flight: %w", err)
	}
	return r, true, nil
}
