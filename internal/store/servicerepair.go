package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Durable repair ranges (func-service-reliability §10.8).
//
// Work is a SET OF RANGES, not "the current job". The distinction is what keeps a newer
// epoch from stranding buckets an older one was still filling: a newer epoch queues its own
// disjoint range and never cancels unfinished historical work, because cancelling it would
// leave buckets no later job is scoped to fill and the contiguity watermark would stall at
// that hole permanently.
//
// `NOTIFY` is only a wake hint. Everything that decides what still needs doing lives in the
// table, so a missed notification costs latency and never correctness.

// RepairReason records why a range exists. It is diagnostic, but ranges only coalesce with
// others of the SAME reason — merging them would erase the origin story of work that is
// about to change a number someone will ask about.
type RepairReason string

const (
	ReasonDeclaration RepairReason = "declaration"
	ReasonEpoch       RepairReason = "epoch"
	ReasonLateData    RepairReason = "late_data"
	ReasonMaintenance RepairReason = "maintenance"
	ReasonAdmin       RepairReason = "admin"
	ReasonBackfill    RepairReason = "backfill"
)

// RepairRange is one unit of durable work.
type RepairRange struct {
	ID         string
	ServiceID  string
	ProjectID  string
	From, To   time.Time
	Reason     RepairReason
	Cursor     time.Time
	Generation int64
	Attempts   int
	// State and LastError are read-surface fields: a range still being computed must show as
	// work in progress rather than as missing data.
	State     string
	LastError string
}

// ErrRangeSuperseded is returned when a batch finds the world moved under it.
var ErrRangeSuperseded = errors.New("store: repair range superseded")

// dbConn is the shape both a pool and a single pinned connection satisfy. Repair work is
// written against it rather than against the pool, because the leader must run its batches
// on the connection that OWNS the advisory lock: if that connection dies the lock is
// released by Postgres and the in-flight transaction is aborted with it, so a deposed leader
// cannot commit behind its successor. A pooled write has no such property — it would commit
// happily after another node had already taken over.
type dbConn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// EnqueueRepairRange records work over [from, to), coalescing with pending ranges of the
// same service and reason.
//
// Coalescing preserves the UNION exactly: the merged row spans the outermost bounds of
// everything it absorbed. A running range is never absorbed — it carries a cursor, and
// widening it under a worker would either replay finished buckets or skip unfinished ones.
func (s *Store) EnqueueRepairRange(ctx context.Context, projectID, serviceID string, from, to time.Time, reason RepairReason) error {
	if !to.After(from) {
		return fmt.Errorf("store: repair range end %s is not after start %s", to, from)
	}
	from = domain.FloorToBucket(from)
	to = domain.CeilToBucket(to)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin enqueue range: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Absorb every pending range of this reason that overlaps or abuts, and take the union.
	var mergedFrom, mergedTo time.Time
	if err := tx.QueryRow(ctx,
		`SELECT LEAST(COALESCE(MIN(range_start), $3), $3), GREATEST(COALESCE(MAX(range_end), $4), $4)
		   FROM service_repair_ranges
		  WHERE service_id = $1 AND reason = $2 AND state = 'pending'
		    AND range_start <= $4 AND range_end >= $3`,
		serviceID, string(reason), from, to).Scan(&mergedFrom, &mergedTo); err != nil {
		return fmt.Errorf("store: compute range union: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM service_repair_ranges
		  WHERE service_id = $1 AND reason = $2 AND state = 'pending'
		    AND range_start <= $4 AND range_end >= $3`,
		serviceID, string(reason), from, to); err != nil {
		return fmt.Errorf("store: absorb pending ranges: %w", err)
	}

	var generation int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		projectID).Scan(&generation); err != nil {
		return fmt.Errorf("store: read maintenance generation: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO service_repair_ranges
		   (service_id, project_id, range_start, range_end, reason, maintenance_generation, cursor_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$3)`,
		serviceID, projectID, mergedFrom, mergedTo, string(reason), generation); err != nil {
		return fmt.Errorf("store: enqueue repair range: %w", err)
	}
	return tx.Commit(ctx)
}

// ClaimRepairRange takes the next runnable range and marks it running.
//
// Ordering is `(next_attempt_at, service_id)` and the claim is `FOR UPDATE SKIP LOCKED`, so
// two leaders overlapping during a handover pick different rows rather than fighting over
// one — the same pattern the pull-job queue already uses.
func (s *Store) ClaimRepairRange(ctx context.Context) (RepairRange, bool, error) {
	return s.claimRepairRangeOn(ctx, s.pool)
}

func (s *Store) claimRepairRangeOn(ctx context.Context, db dbConn) (RepairRange, bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return RepairRange{}, false, fmt.Errorf("store: begin claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var r RepairRange
	var reason string
	err = tx.QueryRow(ctx,
		`UPDATE service_repair_ranges SET state = 'running', updated_at = now()
		  WHERE id = (
		      SELECT id FROM service_repair_ranges
		       WHERE state = 'pending' AND next_attempt_at <= now()
		       ORDER BY next_attempt_at, service_id
		       FOR UPDATE SKIP LOCKED
		       LIMIT 1)
		  RETURNING id, service_id, project_id, range_start, range_end, reason,
		            COALESCE(cursor_at, range_start), maintenance_generation, attempts`).
		Scan(&r.ID, &r.ServiceID, &r.ProjectID, &r.From, &r.To, &reason, &r.Cursor, &r.Generation, &r.Attempts)
	if noRows(err) {
		return RepairRange{}, false, tx.Commit(ctx)
	}
	if err != nil {
		return RepairRange{}, false, fmt.Errorf("store: claim repair range: %w", err)
	}
	r.Reason = RepairReason(reason)
	return r, true, tx.Commit(ctx)
}

// RunRepairRange processes a claimed range until it finishes or the deadline arrives.
//
// The deadline is enforced CALLER-SIDE over the whole slice. `statement_timeout` bounds each
// STATEMENT, not a transaction, so a batch of four statements finishing just under it blows
// the slice four times over; the per-statement values are therefore re-derived from what is
// LEFT rather than set once.
//
// The batch size adapts. A fixed large batch under a tight deadline times out every slice
// and commits nothing — a livelock in which every individual bound is respected and progress
// is zero.
func (s *Store) RunRepairRange(ctx context.Context, r RepairRange, deadline time.Time) error {
	return s.runRepairRangeOn(ctx, s.pool, r, deadline)
}

func (s *Store) runRepairRangeOn(ctx context.Context, db dbConn, r RepairRange, deadline time.Time) error {
	batch := initialRepairBatch
	cursor := r.Cursor
	if cursor.Before(r.From) {
		cursor = r.From
	}

	for cursor.Before(r.To) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Out of slice. The cursor is durable, so the next claim resumes rather than
			// restarts — that is the whole reason work is a table and not a goroutine.
			return s.releaseRepairRange(ctx, db, r.ID, cursor, nil)
		}

		end := cursor.Add(time.Duration(batch) * domain.CanonicalBucket)
		if end.After(r.To) {
			end = r.To
		}
		started := time.Now()
		err := s.runRepairBatch(ctx, db, r, cursor, end, remaining)
		switch {
		case errors.Is(err, ErrRangeSuperseded):
			// The declared inputs moved. Re-enqueue what is left rather than finishing on a
			// stale reading: two batches of one range must not commit under two different
			// "current" maintenance declarations.
			return s.releaseRepairRange(ctx, db, r.ID, cursor, err)
		case err != nil:
			return s.failRepairRange(ctx, db, r, cursor, err)
		}

		spent := time.Since(started)
		batch = adaptRepairBatch(batch, spent, remaining)
		cursor = end
	}
	return s.completeRepairRange(ctx, db, r.ID, cursor)
}

const (
	initialRepairBatch = 60   // one hour of canonical buckets
	maxRepairBatch     = 1440 // §10.10 `recompute_batch_buckets`
	minRepairBatch     = 1
)

// adaptRepairBatch grows a batch that finished comfortably and shrinks one that did not,
// targeting roughly 60% of the time available.
func adaptRepairBatch(batch int, spent, budget time.Duration) int {
	target := budget * 3 / 5
	switch {
	case spent < target/2 && batch < maxRepairBatch:
		batch *= 2
	case spent > target && batch > minRepairBatch:
		batch /= 2
	}
	if batch > maxRepairBatch {
		batch = maxRepairBatch
	}
	if batch < minRepairBatch {
		batch = minRepairBatch
	}
	return batch
}

// runRepairBatch materializes one batch and advances the cursor in ONE transaction, on a
// DEDICATED connection whose server-side timeouts are derived from what is left of the
// slice.
//
// The connection matters. `SET LOCAL` outside a transaction is a no-op, and a context
// deadline cancels the client's wait without bounding how long the server keeps holding a
// lock. Setting `statement_timeout` and `lock_timeout` on this session is what actually
// stops a wait from outliving the slice — and it is the same shape the leader will need
// when its batches move onto the lock-owning connection.
func (s *Store) runRepairBatch(ctx context.Context, db dbConn, r RepairRange, from, to time.Time, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	ms := budget.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	// When the caller handed us the POOL we take one connection for the batch, because the
	// timeouts below are session state and must apply to the same session the work runs on.
	// When the caller handed us a pinned connection — the leader's lock-owning one — we use
	// it as given: that is the whole point of the fence.
	if pool, isPool := db.(*pgxpool.Pool); isPool {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("store: acquire repair connection: %w", err)
		}
		defer conn.Release()
		db = conn
	}
	for _, stmt := range []string{
		fmt.Sprintf("SET statement_timeout = %d", ms),
		fmt.Sprintf("SET lock_timeout = %d", min64(ms, repairLockTimeout.Milliseconds())),
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: set repair timeouts: %w", err)
		}
	}

	current, err := maintenanceGenerationOn(ctx, db, r.ProjectID)
	if err != nil {
		return err
	}
	if current != r.Generation {
		return ErrRangeSuperseded
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin repair batch: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := s.materializeRangeTx(ctx, tx, r.ProjectID, r.ServiceID, from, to); err != nil {
		return err
	}

	// Re-read the generation INSIDE the transaction, after the write. A mutation that landed
	// while this batch ran means the batch read one declaration and the world now has
	// another — and without this, two batches of one range could commit under two different
	// "current" maintenance declarations.
	var after int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		r.ProjectID).Scan(&after); err != nil {
		return fmt.Errorf("store: re-read maintenance generation: %w", err)
	}
	if after != r.Generation {
		return ErrRangeSuperseded
	}

	if _, err := tx.Exec(ctx,
		`UPDATE service_repair_ranges SET cursor_at = $2, updated_at = now() WHERE id = $1`,
		r.ID, to); err != nil {
		return fmt.Errorf("store: advance cursor: %w", err)
	}
	return tx.Commit(ctx)
}

// repairLockTimeout caps a single lock wait inside a batch, on top of the slice remainder.
const repairLockTimeout = 3 * time.Second

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func maintenanceGenerationOn(ctx context.Context, q queryRower, projectID string) (int64, error) {
	var generation int64
	if err := q.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		projectID).Scan(&generation); err != nil {
		return 0, fmt.Errorf("store: read maintenance generation: %w", err)
	}
	return generation, nil
}

// releaseRepairRange puts a range back as pending, keeping its cursor so the next claim
// continues rather than restarts.
func (s *Store) releaseRepairRange(ctx context.Context, db dbConn, id string, cursor time.Time, cause error) error {
	note := ""
	if cause != nil {
		note = cause.Error()
	}
	if _, err := db.Exec(ctx,
		`UPDATE service_repair_ranges
		    SET state = 'pending', cursor_at = $2, last_error = $3,
		        maintenance_generation = COALESCE(
		            (SELECT generation FROM project_maintenance_generation p WHERE p.project_id = service_repair_ranges.project_id), 0),
		        updated_at = now()
		  WHERE id = $1`, id, cursor, note); err != nil {
		return fmt.Errorf("store: release repair range: %w", err)
	}
	return nil
}

func (s *Store) completeRepairRange(ctx context.Context, db dbConn, id string, cursor time.Time) error {
	if _, err := db.Exec(ctx,
		`UPDATE service_repair_ranges SET state = 'complete', cursor_at = $2, last_error = '', updated_at = now()
		  WHERE id = $1`, id, cursor); err != nil {
		return fmt.Errorf("store: complete repair range: %w", err)
	}
	return nil
}

// failRepairRange records an error and backs off. The backoff has a floor so a persistent
// fault cannot become a hot loop, and the range stays claimable rather than being dropped —
// a range nobody retries is a hole in a watermark defined by contiguity.
func (s *Store) failRepairRange(ctx context.Context, db dbConn, r RepairRange, cursor time.Time, cause error) error {
	backoff := repairBackoff(r.Attempts + 1)
	if _, err := db.Exec(ctx,
		`UPDATE service_repair_ranges
		    SET state = 'pending', cursor_at = $2, attempts = attempts + 1,
		        next_attempt_at = now() + $3::interval, last_error = $4, updated_at = now()
		  WHERE id = $1`, r.ID, cursor, backoff.String(), cause.Error()); err != nil {
		return fmt.Errorf("store: fail repair range: %w", err)
	}
	return cause
}

// repairBackoff is 5s doubling to a 5m cap (§10.10).
func repairBackoff(attempts int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}
