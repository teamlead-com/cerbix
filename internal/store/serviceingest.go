package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The ingest side of the seal/ingest handshake (func-service-reliability §10.4).
//
// Every heartbeat that is ACTUALLY INSERTED marks the buckets it belongs to, for every
// service whose SLI declared its monitor at that instant. The seal side — which locks the
// same rows before computing — arrives with the materializer.
//
// Three properties here were each a design finding rather than an obvious choice:
//
//   - It is gated on INSERTION, not on delivery. Every ingest path is
//     `ON CONFLICT (monitor_id, ts) DO NOTHING`, so an ordinary redelivery of an
//     already-counted heartbeat inserts nothing. Marking on delivery would file that
//     duplicate as data the seal excluded — false evidence in the exact surface built to
//     explain a disagreement.
//   - Membership is resolved AS OF the heartbeat's own bucket, never as of now. Historical
//     ingest is revision-exempt and can arrive days late, so today's membership is the wrong
//     question in both directions: a member removed from the SLI afterwards still routes an
//     old heartbeat, because the old fact was produced by an epoch containing it, and a
//     member added afterwards does not.
//   - A monitor in no service's SLI at that instant writes nothing at all, which is what
//     makes "zero services costs nothing" a property rather than a claim.

// noteHeartbeatForServices marks the bucket containing ts dirty for every service whose SLI
// declared this monitor then — or records an aggregated late arrival when that bucket is
// already sealed.
//
// It runs inside the ingesting transaction, so the mark and the heartbeat commit together.
// noteHeartbeatsForServices marks a WHOLE BATCH, taking the (service_id, bucket_start) keys in
// one global order.
//
// Sorting the heartbeats by (monitor_id, ts) and marking one at a time is NOT the same thing,
// and it deadlocks: with monitors A<B<C where A and C belong to service 2 and B to service 1,
// a batch [A,B] takes service 2 then waits on service 1 while a batch [B,C] takes service 1
// then waits on service 2. The mandatory order in §15.4 is over the KEYS actually locked, so
// they have to be resolved first and sorted as keys — per-heartbeat sorting can only order
// what one heartbeat happens to touch.
func (s *Store) noteHeartbeatsForServices(ctx context.Context, tx pgx.Tx, beats []domain.Heartbeat) error {
	type key struct {
		serviceID, projectID, monitorID string
		bucket                          time.Time
		ts                              time.Time
	}
	var keys []key
	for _, hb := range beats {
		if hb.MonitorID == "" || hb.Ts.IsZero() {
			continue
		}
		var bucketStart time.Time
		if err := tx.QueryRow(ctx,
			`SELECT date_bin('1 minute', $1::timestamptz, 'epoch'::timestamptz)`, hb.Ts).Scan(&bucketStart); err != nil {
			return fmt.Errorf("store: resolve heartbeat bucket: %w", err)
		}
		affected, err := servicesDeclaringMonitorAt(ctx, tx, hb.MonitorID, bucketStart)
		if err != nil {
			return err
		}
		if len(affected) > s.serviceLimits().ServicesPerMonitor {
			return fmt.Errorf("store: monitor %s was a reliability input for %d services at %s, above the %d cap",
				hb.MonitorID, len(affected), bucketStart, s.serviceLimits().ServicesPerMonitor)
		}
		for _, a := range affected {
			keys = append(keys, key{a.serviceID, a.projectID, hb.MonitorID, bucketStart, hb.Ts})
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].serviceID != keys[j].serviceID {
			return keys[i].serviceID < keys[j].serviceID
		}
		return keys[i].bucket.Before(keys[j].bucket)
	})
	for _, k := range keys {
		if err := s.markBucket(ctx, tx, k.projectID, k.serviceID, k.monitorID, k.bucket, k.ts); err != nil {
			return err
		}
		if err := repairIfBehindWatermark(ctx, tx, k.projectID, k.serviceID, k.bucket); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) noteHeartbeatForServices(ctx context.Context, tx pgx.Tx, monitorID string, ts time.Time) error {
	if monitorID == "" || ts.IsZero() {
		return nil
	}
	var bucketStart time.Time
	if err := tx.QueryRow(ctx,
		`SELECT date_bin('1 minute', $1::timestamptz, 'epoch'::timestamptz)`, ts).Scan(&bucketStart); err != nil {
		return fmt.Errorf("store: resolve heartbeat bucket: %w", err)
	}

	affected, err := servicesDeclaringMonitorAt(ctx, tx, monitorID, bucketStart)
	if err != nil {
		return err
	}
	if len(affected) == 0 {
		return nil
	}
	if len(affected) > s.serviceLimits().ServicesPerMonitor {
		return fmt.Errorf("store: monitor %s was a reliability input for %d services at %s, above the %d cap",
			monitorID, len(affected), bucketStart, s.serviceLimits().ServicesPerMonitor)
	}

	// Ascending (service_id, bucket_start) — §15.4. A historical batch touches many of these
	// keys at once, and two overlapping batches taking them in opposite orders deadlock.
	for _, a := range affected {
		if err := s.markBucket(ctx, tx, a.projectID, a.serviceID, monitorID, bucketStart, ts); err != nil {
			return err
		}
		// Evidence that arrives BEHIND the watermark makes a sealed number wrong, and the
		// mark alone cannot fix it: ordinary materialization will not touch a sealed bucket,
		// and the forward driver has already walked past. Queue the correction in the SAME
		// transaction as the heartbeat, or the two can separate — leaving a fact that is
		// known to be wrong with nothing scheduled to put it right.
		if err := repairIfBehindWatermark(ctx, tx, a.projectID, a.serviceID, bucketStart); err != nil {
			return err
		}
	}
	return nil
}

// repairIfBehindWatermark queues a late-data recompute when the bucket is already sealed.
func repairIfBehindWatermark(ctx context.Context, tx pgx.Tx, projectID, serviceID string, bucketStart time.Time) error {
	var sealedThrough *time.Time
	err := tx.QueryRow(ctx,
		`SELECT sealed_through FROM service_materialization WHERE service_id = $1`,
		serviceID).Scan(&sealedThrough)
	if noRows(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read watermark for late data: %w", err)
	}
	if sealedThrough == nil || !bucketStart.Before(*sealedThrough) {
		return nil
	}
	return enqueueRepairRangeTx(ctx, tx, projectID, serviceID,
		bucketStart, bucketStart.Add(domain.CanonicalBucket), ReasonLateData, "")
}

// affectedService is one service the heartbeat belongs to, with the tenant key read from the
// MONITOR rather than passed in: the monitor's own project is the authority on which tenant
// this heartbeat is, and the ingest paths already hold that row.
type affectedService struct {
	serviceID string
	projectID string
}

// servicesDeclaringMonitorAt answers "which services declared this monitor as a reliability
// input at this instant", from the revision governing that instant rather than from today's
// reference rows.
//
// The first CTE exists for the common case: most monitors have never been an SLI member of
// anything, and it turns that case into one index probe instead of a scan over the project's
// revision history.
func servicesDeclaringMonitorAt(ctx context.Context, tx pgx.Tx, monitorID string, at time.Time) ([]affectedService, error) {
	rows, err := tx.Query(ctx,
		`WITH mon AS (
		     SELECT project_id FROM monitors WHERE id = $1
		 ),
		 candidate AS (
		     SELECT DISTINCT r.service_id
		       FROM service_definition_members m
		       JOIN service_definition_revisions r ON r.id = m.revision_id
		       JOIN mon ON mon.project_id = m.project_id
		      WHERE m.monitor_id = $1 AND m.role = 'sli'
		 ),
		 governing AS (
		     SELECT DISTINCT ON (r.service_id) r.service_id, r.project_id, r.id
		       FROM service_definition_revisions r
		       JOIN candidate c ON c.service_id = r.service_id
		      WHERE r.state = 'effective' AND r.effective_at <= $2
		      ORDER BY r.service_id, r.effective_at DESC, r.revision DESC
		 )
		 SELECT g.service_id, g.project_id
		   FROM governing g
		   JOIN service_definition_members m2
		     ON m2.revision_id = g.id AND m2.role = 'sli' AND m2.monitor_id = $1
		  ORDER BY g.service_id`, monitorID, at)
	if err != nil {
		return nil, fmt.Errorf("store: services declaring monitor at %s: %w", at, err)
	}
	defer rows.Close()
	var out []affectedService
	for rows.Next() {
		var a affectedService
		if err := rows.Scan(&a.serviceID, &a.projectID); err != nil {
			return nil, fmt.Errorf("store: scan service: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// markBucket bumps the ingest generation for one (service, bucket) — or, if that bucket's
// fact is already SEALED, records the arrival instead of dirtying it.
//
// The upsert takes the ingest row's lock, and the fact's state is read UNDER that lock. The
// ingest row is the mutual-exclusion point; the fact is the authority.
func (s *Store) markBucket(ctx context.Context, tx pgx.Tx, projectID, serviceID, monitorID string, bucketStart, ts time.Time) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO service_bucket_ingest (service_id, project_id, bucket_start, ingest_generation)
		 VALUES ($1,$2,$3,1)
		 ON CONFLICT (service_id, bucket_start)
		 DO UPDATE SET ingest_generation = service_bucket_ingest.ingest_generation + 1`,
		serviceID, projectID, bucketStart); err != nil {
		return fmt.Errorf("store: mark service bucket: %w", err)
	}

	var sealed bool
	err := tx.QueryRow(ctx,
		`SELECT state = 'sealed' FROM service_reliability_buckets
		  WHERE service_id = $1 AND bucket_start = $2`, serviceID, bucketStart).Scan(&sealed)
	if noRows(err) {
		// Not materialized yet: the generation bump is all this bucket needs.
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read bucket state: %w", err)
	}
	if !sealed {
		return nil
	}

	// Late arrivals are AGGREGATED per (service, bucket, monitor), never one retained row
	// per event: a single historical batch landing after a seal, multiplied by the per-monitor
	// service fan-out, would otherwise create millions of rows. The unique key makes
	// redelivery idempotent, so a genuinely late row cannot multiply the evidence it leaves.
	var arrivals int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO service_late_arrivals
		     (service_id, project_id, bucket_start, monitor_id, arrivals, first_received_at, last_received_at, examples)
		 VALUES ($1,$2,$3,$4,1, now(), now(), jsonb_build_array($5::text))
		 ON CONFLICT (service_id, bucket_start, monitor_id) DO UPDATE
		    SET arrivals         = service_late_arrivals.arrivals + 1,
		        last_received_at = now(),
		        examples = CASE
		            WHEN jsonb_array_length(service_late_arrivals.examples) < $6
		            THEN service_late_arrivals.examples || jsonb_build_array($5::text)
		            ELSE service_late_arrivals.examples END,
		        overflow = CASE
		            WHEN jsonb_array_length(service_late_arrivals.examples) < $6
		            THEN service_late_arrivals.overflow
		            ELSE service_late_arrivals.overflow + 1 END
		 RETURNING arrivals`,
		serviceID, projectID, bucketStart, monitorID, ts.UTC().Format(time.RFC3339Nano), MaxLateExamples).Scan(&arrivals); err != nil {
		return fmt.Errorf("store: record late arrival: %w", err)
	}
	// One event per call; this call overflowed its example slot exactly when the running
	// arrival count has passed the bound (examples fill first, overflow counts the rest).
	// Bumped in the SAME transaction as the late-arrival row: if the outer heartbeat write
	// rolls back — a failed repair enqueue behind the watermark, say — the event dies with
	// it instead of counting an arrival that never durably happened.
	var overflowed int64
	if arrivals > int64(MaxLateExamples) {
		overflowed = 1
	}
	if err := bumpMetricEventTx(ctx, tx, metricEventLateArrivals, 1); err != nil {
		return err
	}
	if err := bumpMetricEventTx(ctx, tx, metricEventLateOverflow, overflowed); err != nil {
		return err
	}
	return nil
}

// MaxLateExamples bounds the example timestamps kept per aggregated late-arrival record
// (§10.10). Anything beyond it increments the overflow counter, so the record stays a fixed
// small structure rather than an event log.
const MaxLateExamples = 8
