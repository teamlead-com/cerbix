package store

import (
	"context"
	"encoding/json"
	"errors"
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
	n, err := s.materializeRangeTx(ctx, tx, projectID, serviceID, from, to, modeOrdinary)
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
func (s *Store) materializeRangeTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, from, to time.Time, mode materializeMode) (int, error) {
	if !to.After(from) {
		return 0, fmt.Errorf("store: materialize range end %s is not after start %s", to, from)
	}
	buckets := 0
	for start := from; start.Before(to); start = start.Add(domain.CanonicalBucket) {
		done, err := s.materializeBucketTx(ctx, tx, projectID, serviceID, start, mode)
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

// ErrEvidenceGone is returned when a recompute would run against destroyed evidence: a
// sealed bucket's epoch names a member whose monitor — and therefore whose heartbeats —
// no longer exists. The sealed fact is the surviving record; rewriting it from a partial
// record would replace measured history with UNKNOWN.
var ErrEvidenceGone = errors.New("store: sealed evidence no longer exists")

// anyMemberMonitorGone reports whether any snapshot member's monitor row has been deleted.
func anyMemberMonitorGone(ctx context.Context, tx pgx.Tx, members []reliability.Member) (bool, error) {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.MonitorID)
	}
	var alive int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM monitors WHERE id = ANY($1)`, ids).Scan(&alive); err != nil {
		return false, fmt.Errorf("store: check member survival: %w", err)
	}
	return alive != len(ids), nil
}

// materializeMode says whether a caller is allowed to rewrite finality.
type materializeMode int

const (
	// modeOrdinary keeps a sealed bucket immutable. This is the forward driver: it is
	// keeping up with the clock and has no authority to restate a settled number.
	modeOrdinary materializeMode = iota
	// modeRecompute rewrites a sealed bucket. Only durable repair runs in this mode, and
	// only because something already audited — a confirmed retroactive maintenance
	// mutation, a re-declaration, an admin recompute — established that the sealed number
	// was computed from inputs that have since been corrected.
	//
	// Without this mode the whole repair path was a no-op over exactly the buckets it
	// existed to fix: it walked the range, hit `state='sealed'`, returned, and left the
	// stale facts in place while the watermark happily advanced over them again.
	modeRecompute
)

// materializeBucketTx computes one bucket. It reports whether a fact was written.
func (s *Store) materializeBucketTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string, start time.Time, mode materializeMode) (bool, error) {
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
	var before bucketSplit
	err := tx.QueryRow(ctx,
		`SELECT state, good_us, bad_us, unknown_us, excluded_us,
		        healthy_us, degraded_us, down_us, health_unknown_us
		   FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start=$2`,
		serviceID, start).Scan(&existingState,
		&before.good, &before.bad, &before.unknown, &before.excluded,
		&before.healthy, &before.degraded, &before.down, &before.healthUnknown)
	if err != nil && !noRows(err) {
		return false, fmt.Errorf("store: read bucket state: %w", err)
	}
	wasSealed := existingState == "sealed"
	if wasSealed && mode == modeOrdinary {
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

	// A RECOMPUTE of a sealed bucket whose evidence is gone must fail closed, not proceed.
	//
	// Deleting a monitor cascades its heartbeats away, but the epoch that governed old
	// buckets still snapshots it as a member. Rerunning the reducer then finds no
	// observations and writes UNKNOWN over what was measured GOOD — a recompute triggered by
	// an unrelated maintenance edit silently degrading sealed history it had no quarrel
	// with. The sealed fact IS the surviving record of that evidence; leaving it in place is
	// the correct answer, and the caller surfaces the range as unrecomputable rather than
	// retrying into the same wall.
	//
	// Ordinary (forward) materialization is exempt on purpose: an unsealed bucket whose
	// evidence disappeared before its accounting closed is genuinely UNKNOWN — nothing final
	// is being restated.
	if mode == modeRecompute && wasSealed {
		if gone, gerr := anyMemberMonitorGone(ctx, tx, members); gerr != nil {
			return false, gerr
		} else if gone {
			return false, fmt.Errorf("%w: bucket %s", ErrEvidenceGone, start.UTC().Format(time.RFC3339))
		}
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
	// A recompute of an already-sealed bucket stays SEALED. Correcting a final number does
	// not make it provisional again: the evidence window closed long ago, and un-sealing it
	// would let the contiguity watermark rewind for a bucket that is now MORE correct.
	if seal || wasSealed {
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

	// Restating a SEALED number is an audited act. Someone quoted that figure; if it changed,
	// the record has to say so, with both sides of the change — a recompute that leaves no
	// trace is indistinguishable from data quietly rotting.
	//
	// The comparison covers BOTH axes. Comparing only good/bad missed a whole class of real
	// restatement: an exclusion landing entirely inside already-degraded time moves health
	// without moving availability at all, and the audit then said nothing had happened.
	after := bucketSplit{
		good: d.Good.Microseconds(), bad: d.Bad.Microseconds(),
		unknown: d.Unknown.Microseconds(), excluded: d.Excluded.Microseconds(),
		healthy: d.Healthy.Microseconds(), degraded: d.Degraded.Microseconds(),
		down: d.Down.Microseconds(), healthUnknown: d.HealthUnknown.Microseconds(),
	}
	if wasSealed && before != after {
		if err := recordRecomputeAuditTx(ctx, tx, projectID, serviceID, start, before, after); err != nil {
			return false, err
		}
	}
	return true, nil
}

// bucketSplit is one bucket on both conserved axes, in microseconds.
type bucketSplit struct {
	good, bad, unknown, excluded           int64
	healthy, degraded, down, healthUnknown int64
}

func (b bucketSplit) String() string {
	return fmt.Sprintf("good=%d bad=%d unknown=%d excluded=%d healthy=%d degraded=%d down=%d health_unknown=%d",
		b.good, b.bad, b.unknown, b.excluded, b.healthy, b.degraded, b.down, b.healthUnknown)
}

// recordRecomputeAuditTx writes the before/after of a sealed bucket that changed, in the same
// transaction as the change itself.
func recordRecomputeAuditTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, bucket time.Time, before, after bucketSplit,
) error {
	target := fmt.Sprintf("service=%s bucket=%s before[%s] after[%s]",
		serviceID, bucket.UTC().Format(time.RFC3339), before, after)
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 SELECT p.org_id, NULL, false, 'service.bucket_recomputed', $2
		   FROM projects p WHERE p.id = $1`,
		projectID, target); err != nil {
		return fmt.Errorf("store: audit recompute: %w", err)
	}
	return nil
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
	// The watermark is contiguous WITHIN THE CURRENT ERA. Walking from
	// `materialization_start` instead would make a declared silence — a period the service
	// said it was measuring nothing — hold the watermark forever, so a service that came back
	// could never report again.
	var start *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT era_start FROM service_materialization WHERE service_id=$1`,
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

// ── The forward driver ──────────────────────────────────────────────────────────────────
//
// Everything above computes buckets when someone asks. This is what asks, continuously, and
// its absence is why phase 1 as first written produced no facts in production at all.
//
// It is deliberately NOT a repair range. Repair ranges are for work whose extent is known
// and must survive a crash — a retroactive maintenance mutation, a re-declaration, an
// admin recompute. Forward materialization has no extent: it is "keep up with the clock",
// its cursor IS its state, and enqueuing it as ranges would mean writing a durable row every
// minute for every service forever.

// advanceCommitReserve is the slice time kept back for the cursor write, the watermark
// recompute and the commit: work stops while there is still budget to write down what
// happened. schedulingTolerance mirrors the scheduler's max_scheduling_tolerance — the net
// the client context adds BEHIND the server bounds, which the server bounds must always
// beat.
const (
	advanceCommitReserve = 60 * time.Millisecond
	schedulingTolerance  = 25 * time.Millisecond
)

// maxBucketsPerAdvance bounds one service's share of a slice. A service adopting 90 days of
// history has 129 600 buckets to walk; without a bound it would hold the slice for minutes
// and starve both dispatch and every other service.
const maxBucketsPerAdvance = 240

// AdvanceServiceMaterializationOn walks ONE due service forward on the given connection and
// reports whether it found anything to do.
//
// One service per call, not all of them: the caller owns the leadership connection and its
// deadline, and a loop that drained every service inside a single call could not be
// interrupted when leadership moved.
func (s *Store) AdvanceServiceMaterializationOn(ctx context.Context, db dbConn, deadline time.Time) (bool, error) {
	// The deadline has to bound the WHOLE slice — BEGIN through commit — not just the gaps
	// between buckets.
	//
	// The first implementation took the deadline as an argument and then checked it only in
	// the loop condition, on a transaction opened with the caller's plain context. One
	// statement inside a bucket — the `service_bucket_ingest` upsert waiting on a concurrent
	// ingest holding the same key, say — could then block indefinitely and hold the
	// scheduler's dispatch tick far past `max_dispatch_delay`. The repair path's timeouts do
	// not help here: the forward pass runs only when the repair queue was empty, so the
	// session may still carry `statement_timeout = 0`.
	budget := time.Until(deadline)
	if budget <= 0 {
		return true, nil
	}
	// The client-side deadline is a NET set at the slice deadline plus the scheduler's own
	// tolerance — never the mechanism. The mechanism is deadlineTx: a client-side check
	// refuses to start any statement inside the commit reserve, and the server bounds are
	// re-derived from the remainder before every statement, so the server always speaks
	// first and BEGIN→commit stays inside max_dispatch_delay + max_scheduling_tolerance
	// (§10.10). An earlier version moved the client deadline two seconds past the slice —
	// which kept the connection alive by abandoning the cadence the slice exists to protect.
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(schedulingTolerance))
	defer cancel()

	rawTx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: begin advance: %w", err)
	}
	defer rawTx.Rollback(ctx) //nolint:errcheck // no-op after commit
	tx := newDeadlineTx(rawTx, deadline, advanceCommitReserve)

	// `now` comes from the DB, like every other instant in this subsystem: the seal decision
	// downstream compares against it, and two clocks would let a fast node seal a bucket a
	// slow one still considers open.
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		return false, fmt.Errorf("store: read clock: %w", err)
	}
	// Only buckets whose accounting window has closed are worth walking: an earlier one
	// would be written provisional and rewritten on the next pass, for every service, every
	// minute.
	horizon := domain.FloorToBucket(now.Add(-LateArrivalGrace))

	var (
		serviceID, projectID string
		cursor               time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT service_id, project_id, COALESCE(materialized_through, materialization_start)
		   FROM service_materialization
		  WHERE COALESCE(materialized_through, materialization_start) < $1
		  ORDER BY COALESCE(materialized_through, materialization_start)
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1`, horizon).Scan(&serviceID, &projectID, &cursor)
	if noRows(err) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, fmt.Errorf("store: claim service for advance: %w", err)
	}

	end := cursor.Add(time.Duration(maxBucketsPerAdvance) * domain.CanonicalBucket)
	if end.After(horizon) {
		end = horizon
	}

	// Bucket at a time so the deadline is honoured mid-range rather than after it. A partial
	// pass is not a failure: the cursor is committed at whatever point it reached and the
	// next slice resumes there. The adapter refuses to START any statement inside the commit
	// reserve — that is what breaks accumulation, which a bound set once (or once per
	// bucket) cannot: many statements each finishing just under such a bound sum to any
	// total. Exhaustion surfaces as errSliceBudget mid-bucket; that bucket's fact was not
	// yet written (the fact insert is its last statement), so committing the cursor at the
	// previous bucket keeps the walked/not-walked boundary exact.
	reached := cursor
	for at := cursor; at.Before(end); at = at.Add(domain.CanonicalBucket) {
		if time.Until(deadline) <= advanceCommitReserve {
			break
		}
		if _, err := s.materializeBucketTx(ctx, tx, projectID, serviceID, at, modeOrdinary); err != nil {
			if errors.Is(err, errSliceBudget) {
				break
			}
			return true, err
		}
		reached = at.Add(domain.CanonicalBucket)
	}
	if reached.Equal(cursor) {
		// The deadline landed before the first bucket. Commit nothing and report work
		// remaining, so the caller backs off to the next sub-tick instead of spinning.
		return true, tx.persistPhase().Commit(ctx)
	}

	// The PERSISTENCE runs through the persist envelope: the reserve drops to zero — this is
	// the time the work loop was forbidden to touch — but every statement, the watermark
	// recompute's several included, and the COMMIT itself stay behind per-statement,
	// remainder-derived bounds. An earlier revision switched back to the raw transaction
	// with one fixed SET here, and statement_timeout restarts per statement: two statements
	// late in the reserve could carry the slice past the client net, which on this
	// connection is the advisory lock's life.
	persist := tx.persistPhase()
	if _, err := persist.Exec(ctx,
		`UPDATE service_materialization SET materialized_through = $2 WHERE service_id = $1`,
		serviceID, reached); err != nil {
		return true, fmt.Errorf("store: advance materialized_through: %w", err)
	}
	// The watermark is recomputed from the FACTS, never set to the cursor: progress and
	// evidence are different claims, and a hole must hold the watermark while the driver
	// walks past it.
	if err := advanceSealedThrough(ctx, persist, serviceID); err != nil {
		return true, err
	}
	return true, persist.Commit(ctx)
}
