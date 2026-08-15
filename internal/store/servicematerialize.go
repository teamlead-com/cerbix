package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/reliability"
)

// Materialization and sealing (func-service-reliability §10.2, §10.4, §10.5).
//
// This file computes facts for a bucket range and seals the ones whose accounting window
// has closed. It is the SEAL side of the handshake whose ingest side lives in
// serviceingest.go; the leader loop that decides which ranges to run arrives with durable
// ranges.

// LateArrivalGrace is how long after a bucket ends its accounting stays open (§10.10).
//
// It is deliberately NOT `result.allowed_skew`. That bounds a worker clock running FAST —
// ingest rejects `ts > now + skew` — and says nothing about how late a result may arrive.
// Old results are accepted up to raw retention and an agent's historical backfill can land
// much later still, so finality needs its own policy rather than a borrowed one.
const LateArrivalGrace = 2 * time.Minute

// MaterializeServiceRange computes every canonical bucket in [from, to) for one service and
// seals the ones whose grace has elapsed.
//
// from and to must be bucket-aligned; the caller's ranges are, because a sub-bucket
// partition cannot coexist with a whole-bucket primary key and a conservation CHECK.
func (s *Store) MaterializeServiceRange(ctx context.Context, projectID, serviceID string, from, to time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin materialize: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	n, err := s.materializeRangeTx(ctx, tx, projectID, serviceID, from, to)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit materialize: %w", err)
	}
	return n, nil
}

// materializeRangeTx is the body, separated so a caller that must run on a SPECIFIC
// connection — the repair path, which sets server-side timeouts on its own session, and
// later the leader running on its lock-owning connection — can supply its own transaction
// rather than taking a fresh one from the pool.
func (s *Store) materializeRangeTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to time.Time) (int, error) {
	if !to.After(from) {
		return 0, fmt.Errorf("store: materialize range end %s is not after start %s", to, from)
	}
	buckets := 0
	for start := from; start.Before(to); start = start.Add(domain.CanonicalBucket) {
		done, err := s.materializeBucketTx(ctx, tx, projectID, serviceID, start)
		if err != nil {
			return 0, err
		}
		if done {
			buckets++
		}
	}
	if err := advanceSealedThrough(ctx, tx, serviceID); err != nil {
		return 0, err
	}
	return buckets, nil
}

// materializeBucketTx computes one bucket. It reports whether a fact was written.
func (s *Store) materializeBucketTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, start time.Time) (bool, error) {
	end := start.Add(domain.CanonicalBucket)

	// The seal is a compare-and-swap, and this is where it starts: MATERIALIZE the ingest
	// row and lock it BEFORE reading anything. Locking only rows that happen to exist would
	// leave a phantom — a bucket that received no heartbeat has no row to lock, and a
	// concurrent ingest could insert one and commit inside the window between the read and
	// the write. The upsert closes it, because that ingest then blocks on the same key.
	var generation int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO service_bucket_ingest (service_id, project_id, bucket_start, ingest_generation)
		 VALUES ($1,$2,$3,0)
		 ON CONFLICT (service_id, bucket_start)
		 DO UPDATE SET ingest_generation = service_bucket_ingest.ingest_generation
		 RETURNING ingest_generation`,
		serviceID, projectID, start).Scan(&generation); err != nil {
		return false, fmt.Errorf("store: lock ingest row: %w", err)
	}

	// A sealed bucket is immutable to ordinary materialization. Rewriting it is an audited
	// recompute or repair, and neither of those comes through here.
	var existingState string
	err := tx.QueryRow(ctx,
		`SELECT state FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		serviceID, start).Scan(&existingState)
	if err != nil && !noRows(err) {
		return false, fmt.Errorf("store: read bucket state: %w", err)
	}
	if existingState == "sealed" {
		return false, nil
	}

	epochID, members, policies, err := epochAt(ctx, tx, serviceID, start)
	if err != nil {
		return false, err
	}
	if epochID == "" || len(members) == 0 {
		// No epoch governs this bucket, or the service declared no reliability inputs then.
		// A service with an empty SLI produces no facts at all — it reports availability as
		// unavailable rather than as anything.
		return false, nil
	}

	observations, err := observationsFor(ctx, tx, members, start, end)
	if err != nil {
		return false, err
	}
	spans, err := maintenanceSpansFor(ctx, tx, projectID, members, start, end)
	if err != nil {
		return false, err
	}

	bucket, err := reliability.Reduce(reliability.Input{
		Start: start, End: end,
		Members: members, Observations: observations, Maintenance: spans,
		Policies: policies,
	})
	if err != nil {
		return false, fmt.Errorf("store: reduce bucket %s: %w", start, err)
	}

	// Sealing is part of the correctness model, not an optimization: without it the budget
	// drifts between two viewings and the number stops being worth quoting.
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		return false, fmt.Errorf("store: read clock: %w", err)
	}
	seal := !now.Before(end.Add(LateArrivalGrace))

	var maintenanceGeneration int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		projectID).Scan(&maintenanceGeneration); err != nil {
		return false, fmt.Errorf("store: read maintenance generation: %w", err)
	}

	provenance, err := json.Marshal(bucket.Provenance)
	if err != nil {
		return false, fmt.Errorf("store: encode provenance: %w", err)
	}

	state := "provisional"
	var sealedAt *time.Time
	var sealedGeneration *int64
	if seal {
		state = "sealed"
		sealedAt = &now
		sealedGeneration = &generation
	}

	d := bucket.Durations
	if _, err := tx.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us,
		    good_us, bad_us, unknown_us, excluded_us,
		    healthy_us, degraded_us, down_us, health_unknown_us,
		    state, sealed_at, sealed_ingest_generation, maintenance_generation, provenance, computed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18, now())
		 ON CONFLICT (service_id, bucket_start) DO UPDATE SET
		    epoch_id = EXCLUDED.epoch_id,
		    good_us = EXCLUDED.good_us, bad_us = EXCLUDED.bad_us,
		    unknown_us = EXCLUDED.unknown_us, excluded_us = EXCLUDED.excluded_us,
		    healthy_us = EXCLUDED.healthy_us, degraded_us = EXCLUDED.degraded_us,
		    down_us = EXCLUDED.down_us, health_unknown_us = EXCLUDED.health_unknown_us,
		    state = EXCLUDED.state, sealed_at = EXCLUDED.sealed_at,
		    sealed_ingest_generation = EXCLUDED.sealed_ingest_generation,
		    maintenance_generation = EXCLUDED.maintenance_generation,
		    provenance = EXCLUDED.provenance, computed_at = now()`,
		serviceID, projectID, epochID, start, domain.CanonicalBucket.Microseconds(),
		d.Good.Microseconds(), d.Bad.Microseconds(), d.Unknown.Microseconds(), d.Excluded.Microseconds(),
		d.Healthy.Microseconds(), d.Degraded.Microseconds(), d.Down.Microseconds(), d.HealthUnknown.Microseconds(),
		state, sealedAt, sealedGeneration, maintenanceGeneration, provenance); err != nil {
		return false, fmt.Errorf("store: write bucket %s: %w", start, err)
	}
	return true, nil
}

// epochAt returns the epoch governing an instant, its snapshotted members translated into
// evaluator input, and the policies of the declaration it resolves to.
//
// The epoch is resolved PER BUCKET, not per range: a range spanning a boundary evaluates
// each part under the epoch in force there, which is what lets a newer epoch queue its own
// work without cancelling anything.
func epochAt(ctx context.Context, tx pgx.Tx, serviceID string, at time.Time) (string, []reliability.Member, domain.ServicePolicies, error) {
	var epochID string
	var snapshot, policyJSON []byte
	err := tx.QueryRow(ctx,
		`SELECT e.id, e.snapshot, r.policies
		   FROM service_evaluation_epochs e
		   JOIN service_definition_revisions r ON r.id = e.revision_id
		  WHERE e.service_id = $1 AND e.state = 'effective' AND e.effective_at <= $2
		  ORDER BY e.effective_at DESC, e.epoch_seq DESC
		  LIMIT 1`, serviceID, at).Scan(&epochID, &snapshot, &policyJSON)
	if noRows(err) {
		return "", nil, domain.ServicePolicies{}, nil
	}
	if err != nil {
		return "", nil, domain.ServicePolicies{}, fmt.Errorf("store: resolve epoch at %s: %w", at, err)
	}

	var snap []domain.EpochMember
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return "", nil, domain.ServicePolicies{}, fmt.Errorf("store: decode epoch snapshot: %w", err)
	}
	var policies domain.ServicePolicies
	if err := json.Unmarshal(policyJSON, &policies); err != nil {
		return "", nil, domain.ServicePolicies{}, fmt.Errorf("store: decode policies: %w", err)
	}

	members := make([]reliability.Member, 0, len(snap))
	for _, m := range snap {
		members = append(members, reliability.Member{
			MonitorID:  m.MonitorID,
			Type:       m.Semantics.Type,
			Region:     m.Semantics.Region,
			Enabled:    m.Semantics.Enabled,
			StaleAfter: m.StaleAfter,
			ArmedAt:    m.ArmedAt,
		})
	}
	return epochID, members, policies, nil
}

// observationsFor reads the heartbeats the bucket needs, INCLUDING the last one before it
// began. Sample-and-hold cannot start a bucket without the observation in force when it
// opened, and dropping it would make every bucket start UNKNOWN.
func observationsFor(ctx context.Context, tx pgx.Tx, members []reliability.Member, start, end time.Time) ([]reliability.Observation, error) {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.MonitorID)
	}
	rows, err := tx.Query(ctx,
		`SELECT monitor_id, ts, up FROM heartbeats
		  WHERE monitor_id = ANY($1) AND ts >= $2 AND ts < $3
		 UNION ALL
		 SELECT DISTINCT ON (monitor_id) monitor_id, ts, up FROM heartbeats
		  WHERE monitor_id = ANY($1) AND ts < $2
		  ORDER BY monitor_id, ts DESC`, ids, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: read observations: %w", err)
	}
	defer rows.Close()
	var out []reliability.Observation
	for rows.Next() {
		var o reliability.Observation
		if err := rows.Scan(&o.MonitorID, &o.Ts, &o.Up); err != nil {
			return nil, fmt.Errorf("store: scan observation: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// maintenanceSpansFor reads the exclusions in force over a bucket.
//
// The effective span is `[starts_at, min(ends_at, cancel_effective_at))` REGARDLESS of
// archived_at. An archived window keeps its effect on time it already covered: archiving
// says "no longer in active inventory", not "this maintenance never happened", and letting
// a later recompute drop it would turn every archive into an annul — a sealed bucket
// changing with no preview, no raw fence and no audited intent. Only an explicit annul
// removes a span from this input.
func maintenanceSpansFor(ctx context.Context, tx pgx.Tx, projectID string, members []reliability.Member, start, end time.Time) ([]reliability.MaintenanceSpan, error) {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.MonitorID)
	}
	rows, err := tx.Query(ctx,
		`SELECT id, COALESCE(monitor_id::text, ''), starts_at,
		        LEAST(ends_at, COALESCE(cancel_effective_at, ends_at))
		   FROM maintenance_windows
		  WHERE project_id = $1
		    AND (monitor_id IS NULL OR monitor_id = ANY($2))
		    AND starts_at < $4
		    AND LEAST(ends_at, COALESCE(cancel_effective_at, ends_at)) > $3
		  ORDER BY id`, projectID, ids, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: read maintenance spans: %w", err)
	}
	defer rows.Close()
	var out []reliability.MaintenanceSpan
	for rows.Next() {
		var sp reliability.MaintenanceSpan
		if err := rows.Scan(&sp.ID, &sp.MonitorID, &sp.From, &sp.To); err != nil {
			return nil, fmt.Errorf("store: scan maintenance span: %w", err)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// advanceSealedThrough recomputes the watermark by CONTIGUITY: the greatest boundary such
// that every bucket before it exists and is sealed.
//
// A materialization hole HOLDS it rather than being jumped over. That is what lets one
// scalar answer "did we materialize this window" honestly, and what makes a stalled service
// visible as a lagging timestamp instead of a plausible chart. Taking `MAX(bucket_start)`
// over sealed rows would skip straight past a gap and report a window that has holes in it.
func advanceSealedThrough(ctx context.Context, tx pgx.Tx, serviceID string) error {
	var start *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT materialization_start FROM service_materialization WHERE service_id=$1`,
		serviceID).Scan(&start); err != nil {
		if noRows(err) {
			return nil
		}
		return fmt.Errorf("store: read materialization start: %w", err)
	}
	if start == nil {
		return nil
	}

	// The first gap is either the first non-sealed bucket, or the first missing one. Both
	// are found by walking sealed buckets in order and stopping where the chain breaks.
	// The watermark is where the chain of sealed, contiguous buckets STOPS — and the two
	// ways it can stop land on different instants, which is worth spelling out because
	// conflating them puts the watermark a bucket past a hole:
	//
	//   * a bucket that exists but is not sealed stops it at that bucket's own start;
	//   * a MISSING bucket stops it at the end of the last one present, which is the start
	//     of the slot nobody filled — not at the start of the next row that does exist.
	var through *time.Time
	if err := tx.QueryRow(ctx,
		`WITH ordered AS (
		     SELECT bucket_start, state,
		            LAG(bucket_start) OVER (ORDER BY bucket_start) AS prev
		       FROM service_reliability_buckets
		      WHERE service_id = $1 AND bucket_start >= $2
		 ),
		 stops AS (
		     SELECT bucket_start AS at FROM ordered WHERE state <> 'sealed'
		     UNION ALL
		     SELECT prev + interval '1 minute' FROM ordered
		      WHERE prev IS NOT NULL AND bucket_start <> prev + interval '1 minute'
		     UNION ALL
		     SELECT $2::timestamptz FROM ordered WHERE prev IS NULL AND bucket_start <> $2
		 )
		 SELECT COALESCE(
		     (SELECT MIN(at) FROM stops),
		     (SELECT MAX(bucket_start) + interval '1 minute' FROM ordered)
		 )`, serviceID, *start).Scan(&through); err != nil {
		return fmt.Errorf("store: compute sealed_through: %w", err)
	}
	if through == nil {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE service_materialization SET sealed_through = $2 WHERE service_id = $1`,
		serviceID, *through); err != nil {
		return fmt.Errorf("store: advance sealed_through: %w", err)
	}
	return nil
}
