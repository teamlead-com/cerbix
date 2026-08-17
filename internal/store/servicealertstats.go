package store

import (
	"context"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/metrics"
)

// FR-021 §16.6b — the two SAMPLED gauges of the alerting evaluator.
//
// The evaluator itself returns what one slice DID (evaluated, onsets, closes, errors, lag); those
// are counters and they are the leader's own arithmetic. `active` and `backlog` are the two
// questions a slice cannot answer, because both are about rows the slice did not touch: how many
// alerts are open right now, and how much work is waiting. They are read here, out of band, on the
// same sampling cadence as ServiceReliabilityStats and with the same discipline — every count is an
// index-backed probe capped at capSQL rows, so the sample's cost is fixed no matter how much
// alerting history an installation accumulates. A gauge that says "at least a thousand" answers
// every operational question a bigger exact number would.
//
// The result carries NO tenant, service, target or rule dimension, and it is not an oversight: this
// surface is reachable by anyone who can create a service, and a per-tenant label would let them
// grow the metrics endpoint without limit.

// ServiceAlertLeaseMultiplier re-exports the freshness multiplier the evaluators write their leases
// with, for the readiness predicate of §16.6b: an evaluation whose lag exceeds
// `lease_multiplier × cadence` marks the SCHEDULER not-ready. It is an alias, not a second copy —
// a readiness bound that drifted from the lease it is supposed to describe would either wedge a
// healthy leader or stay ready through a stall.
const ServiceAlertLeaseMultiplier = serviceAlertLeaseMultiplier

// ServiceAlertStats samples the alerting evaluator's open episodes and evaluation backlog.
//
// ONE round trip, four capped probes. "Due" is the DB-clock predicate of §16.5a read from the other
// side — a row is fresh while `now() < lease_until`, so it is due once that stops being true, and a
// row that has never been evaluated has no lease at all and is due by definition. The UNIT of each
// backlog is the unit its evaluator slices in: a SERVICE for the live arm, a TARGET for the burn
// arm, so backlog and slice cap are comparable numbers rather than two different sorts of thing.
func (s *Store) ServiceAlertStats(ctx context.Context) (metrics.ServiceAlertStat, error) {
	var st metrics.ServiceAlertStat
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM (
		            SELECT 1 FROM service_alert_episodes
		             WHERE closed_at IS NULL AND signal = 'health' LIMIT `+capSQL+`) ah),
		       (SELECT count(*) FROM (
		            SELECT 1 FROM service_alert_episodes
		             WHERE closed_at IS NULL AND signal = 'burn' LIMIT `+capSQL+`) ab),
		       -- The live arm slices SERVICES that own paging; one with no state row has never been
		       -- evaluated, which is the most backlogged a service can be — until it is evaluated its
		       -- members are paging for themselves.
		       (SELECT count(*) FROM (
		            SELECT 1
		              FROM services s
		              LEFT JOIN service_alert_state st ON st.service_id = s.id
		             WHERE s.owns_paging
		               AND (st.service_id IS NULL OR st.lease_until <= now())
		             LIMIT `+capSQL+`) bh),
		       -- The burn arm slices TARGETS, and a target is as stale as its STALEST rule: the same
		       -- min() the evaluator orders its slice by, read as a freshness question.
		       (SELECT count(*) FROM (
		            SELECT 1
		              FROM sla_targets t
		              JOIN services s ON s.id = t.service_id
		              LEFT JOIN LATERAL (
		                  SELECT min(b.lease_until) AS lease
		                    FROM service_burn_alert_state b
		                   WHERE b.service_id = s.id AND b.project_id = s.project_id
		                     AND b.sla_target_id = t.id
		              ) bs ON true
		             WHERE s.owns_paging AND t.burn_alert_enabled AND t.burn_rules <> '[]'::jsonb
		               AND (bs.lease IS NULL OR bs.lease <= now())
		             LIMIT `+capSQL+`) bb)`).
		Scan(&st.ActiveHealth, &st.ActiveBurn, &st.BacklogHealth, &st.BacklogBurn); err != nil {
		return st, fmt.Errorf("store: service alert stats: %w", err)
	}
	return st, nil
}
