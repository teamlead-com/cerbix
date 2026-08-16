package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/metrics"
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

// EnsureServiceFactPartitions creates monthly partitions of service_reliability_buckets for
// [this month, this month + ahead months]. Native range partitions exist in BOTH storage
// modes (00064): facts are the sealed PRODUCT, not raw evidence, so TimescaleDB never owns
// them and there is no hypertable branch here. Migration 00064 pre-created a window around
// its own run date and nothing maintained it afterwards — roughly six months after an
// install, every fact insert would land in the DEFAULT partition: safe (inserts never lose
// data) and unmanageable (one partition growing forever). Best-effort like the heartbeat
// twin; the DEFAULT partition keeps inserts safe if this falls behind. Facts are never
// purged here — sealed facts are the record the reliability subsystem exists to keep, and
// raw retention governs heartbeats, not the numbers computed from them.
func (s *Store) EnsureServiceFactPartitions(ctx context.Context, aheadMonths int) error {
	return s.ensureServiceFactPartitionsAt(ctx, time.Now().UTC(), aheadMonths)
}

// ensureServiceFactPartitionsAt is the body under an injectable clock, so a rollover — a
// standalone staging or stranded DEFAULT rows left in a month that is no longer "current" —
// is testable without waiting for one.
func (s *Store) ensureServiceFactPartitionsAt(ctx context.Context, now time.Time, aheadMonths int) error {
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var errs []error
	// Past recovery runs FIRST: a hot current-month adoption consuming the pass budget must
	// not starve the bounded one-unit-per-cadence catch-up of rolled-over work.
	if err := s.recoverPastFactMonth(ctx, month); err != nil {
		errs = append(errs, err)
	}
	for i := 0; i <= aheadMonths; i++ {
		m := month.AddDate(0, i, 0)
		// The name matches migration 00064's to_char(m, 'YYYYMM') so the two creators are
		// idempotent against each other.
		name := serviceFactPartitionName(m)
		// The state is read FIRST, because success from CREATE ... IF NOT EXISTS is a lie
		// for a STANDALONE table: an interrupted adoption's staging table makes the CREATE
		// print "already exists, skipping" and attach nothing, and an Ensure that trusted
		// it reported healthy forever over an unattached month. Standalone → resume the
		// adoption; attached → done; absent → create, adopting on the default-row conflict.
		state, err := s.factPartitionStateOf(ctx, name)
		if err == nil {
			switch state {
			case factPartitionAttached:
				continue
			case factPartitionStandalone:
				err = s.adoptDefaultServiceFactMonth(ctx, name, m)
			case factPartitionAbsent:
				_, err = s.pool.Exec(ctx, fmt.Sprintf(
					`CREATE TABLE IF NOT EXISTS %s PARTITION OF service_reliability_buckets FOR VALUES FROM ('%s') TO ('%s')`,
					name, m.Format(pgTimestamp), m.AddDate(0, 1, 0).Format(pgTimestamp)))
				switch {
				case err != nil && isDefaultPartitionConflict(err):
					// Rows for this month already sit in DEFAULT — one missed cadence is
					// enough — and Postgres refuses to carve a partition whose range the
					// default holds rows for. Recovery adopts the month.
					err = s.adoptDefaultServiceFactMonth(ctx, name, m)
				case err == nil:
					// The absent→create window is not atomic against a concurrent creator:
					// IF NOT EXISTS can skip a standalone relation that appeared after the
					// state read and still report success. Re-verify; standalone → adopt.
					if st2, verr := s.factPartitionStateOf(ctx, name); verr != nil {
						err = verr
					} else if st2 == factPartitionStandalone {
						err = s.adoptDefaultServiceFactMonth(ctx, name, m)
					}
				}
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// serviceFactPartitionName is the ONE spelling of a month partition's name, shared by the
// creators, the recovery probes and the operator command; it matches migration 00064's
// to_char(m, 'YYYYMM').
func serviceFactPartitionName(month time.Time) string {
	return "service_reliability_buckets_" + month.Format("200601")
}

// recoverPastFactMonth finds at most ONE past month needing adoption: the oldest standalone
// staging table (an interrupted adoption that rolled over), else the oldest past month with
// rows still in DEFAULT. The DEFAULT probe is a single index probe on the month index; the
// staging probe is a catalog scan over the bounded partition namespace.
func (s *Store) recoverPastFactMonth(ctx context.Context, currentMonth time.Time) error {
	// The standalone probe considers ONLY months strictly before the current one — the main
	// loop owns current..ahead. Without that filter, any current/future standalone (an
	// adoption the main loop is mid-way through, or an interrupted future staging) shadowed
	// the probe: it matched first, was dismissed as "not past", and the function returned
	// before ever probing DEFAULT — past months lost their recovery for as long as the
	// future leftover existed. Zero-padded YYYYMM makes the lexicographic bound correct.
	currentName := serviceFactPartitionName(currentMonth.UTC())
	var name *string
	// Scoped to the PARENT'S OWN NAMESPACE by OID join — a same-named relation in another
	// schema must neither surface here nor shadow a real candidate.
	if err := s.pool.QueryRow(ctx, `
		SELECT c.relname FROM pg_class c
		  JOIN pg_class parent ON parent.oid = 'service_reliability_buckets'::regclass
		 WHERE c.relnamespace = parent.relnamespace
		   AND c.relname ~ '^service_reliability_buckets_[0-9]{6}$'
		   AND c.relname < $1
		   AND c.relkind = 'r'
		   AND NOT EXISTS (SELECT 1 FROM pg_inherits
		                    WHERE inhrelid = c.oid
		                      AND inhparent = parent.oid)
		 ORDER BY c.relname LIMIT 1`, currentName).Scan(&name); err != nil && !noRows(err) {
		return fmt.Errorf("store: find standalone month: %w", err)
	}
	if name == nil {
		var oldest *time.Time
		// date_trunc over a timestamptz truncates in the SESSION zone: under Asia/Baku,
		// December truncated to 2025-12-01 00:00+04 = 2025-11-30 20:00Z and the UTC-side
		// formatting named month 202511 — the wrong partition got adopted and the real
		// month stayed unreachable. Truncating the UTC PROJECTION (a plain timestamp) is
		// session-proof; pgx hands a timestamp back as UTC.
		if err := s.pool.QueryRow(ctx,
			`SELECT date_trunc('month', min(bucket_start) AT TIME ZONE 'UTC') FROM service_reliability_buckets_default
			  WHERE bucket_start < $1`, currentMonth).Scan(&oldest); err != nil {
			return fmt.Errorf("store: find stranded past month: %w", err)
		}
		if oldest == nil {
			return nil
		}
		n := serviceFactPartitionName(oldest.UTC())
		name = &n
	}
	monthStr := strings.TrimPrefix(*name, "service_reliability_buckets_")
	m, err := time.ParseInLocation("200601", monthStr, time.UTC)
	if err != nil {
		return fmt.Errorf("store: parse month table %s: %w", *name, err)
	}
	if !m.Before(currentMonth) {
		return nil // current/future months are the main loop's job
	}
	if err := s.adoptDefaultServiceFactMonth(ctx, *name, m); err != nil {
		return fmt.Errorf("%s: %w", *name, err)
	}
	return nil
}

// isDefaultPartitionConflict matches Postgres refusing CREATE ... PARTITION OF because the
// DEFAULT partition already holds rows of the new partition's range.
func isDefaultPartitionConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	// 23514 check_violation: "updated partition constraint for default partition ... would
	// be violated by some row".
	return pgErr.Code == "23514"
}

// factPartitionState reports where a named month table stands: absent, ATTACHED to the
// partitioned parent, or STANDALONE — a staging table left by an interrupted adoption. The
// distinction matters because CREATE TABLE IF NOT EXISTS ... PARTITION OF sees a standalone
// table, prints "relation already exists, skipping", returns SUCCESS, and attaches nothing:
// an Ensure that trusted that success reported healthy forever while an interrupted
// adoption's staging table sat invisible next to the parent.
type factPartitionState int

const (
	factPartitionAbsent factPartitionState = iota
	factPartitionAttached
	factPartitionStandalone
)

func (s *Store) factPartitionStateOf(ctx context.Context, name string) (factPartitionState, error) {
	// Resolved by regclass OID in the CURRENT search path, never by bare relname: a
	// same-named relation in another schema must not classify this month's state.
	var exists, attached bool
	if err := s.pool.QueryRow(ctx, `
		SELECT to_regclass($1) IS NOT NULL,
		       EXISTS (SELECT 1 FROM pg_inherits
		                WHERE inhrelid = to_regclass($1)
		                  AND inhparent = 'service_reliability_buckets'::regclass)`,
		name).Scan(&exists, &attached); err != nil {
		return factPartitionAbsent, fmt.Errorf("store: partition state %s: %w", name, err)
	}
	switch {
	case attached:
		return factPartitionAttached, nil
	case exists:
		return factPartitionStandalone, nil
	default:
		return factPartitionAbsent, nil
	}
}

// factColumns is the full fact column list, PK first, for the adoption copy's upsert.
const factColumns = `service_id, bucket_start, project_id, epoch_id, bucket_size_us,
	good_us, bad_us, unknown_us, excluded_us, healthy_us, degraded_us, down_us,
	health_unknown_us, state, sealed_at, sealed_ingest_generation, maintenance_generation,
	provenance, computed_at`

// factUpsertSet updates every non-PK column on conflict, so a re-copied row converges on the
// DEFAULT partition's current content no matter how many passes touched it.
const factUpsertSet = `project_id = EXCLUDED.project_id, epoch_id = EXCLUDED.epoch_id,
	bucket_size_us = EXCLUDED.bucket_size_us, good_us = EXCLUDED.good_us,
	bad_us = EXCLUDED.bad_us, unknown_us = EXCLUDED.unknown_us,
	excluded_us = EXCLUDED.excluded_us, healthy_us = EXCLUDED.healthy_us,
	degraded_us = EXCLUDED.degraded_us, down_us = EXCLUDED.down_us,
	health_unknown_us = EXCLUDED.health_unknown_us, state = EXCLUDED.state,
	sealed_at = EXCLUDED.sealed_at, sealed_ingest_generation = EXCLUDED.sealed_ingest_generation,
	maintenance_generation = EXCLUDED.maintenance_generation, provenance = EXCLUDED.provenance,
	computed_at = EXCLUDED.computed_at`

// adoptDefaultServiceFactMonth moves one month's stranded rows out of the DEFAULT partition
// and attaches a proper monthly partition, with the PARENT COPY AUTHORITATIVE until the very
// last instant:
//
//   - the long phase COPIES (keyset upserts, month-index order) into a standalone staging
//     table and never deletes from DEFAULT — every parent read keeps seeing every fact at
//     every committed point, and an error, shutdown or timeout anywhere leaves nothing
//     stranded: the next pass resumes onto the same staging.
//   - the staging table carries the PARENT'S REFERENTIAL LIFECYCLE from the first row: the
//     services/epochs foreign keys are ensured on every pass (creation AND resume), so
//     deleting a service cascades through the staging exactly as through the parent — the
//     one-transaction deletion contract (§"Facts") holds mid-adoption. Rows orphaned before
//     the keys existed (an interrupted older pass) are swept by anti-join first, because
//     ADD CONSTRAINT validates and a ghost row would wedge the resume forever.
//   - the month CHECK is ensured and VALIDATED on every pass too — not only at creation —
//     so a crash between CREATE and ALTER cannot leave a staging whose ATTACH needs a full
//     validation scan inside the fence's statement budget.
//   - only the short fenced transaction — DELETE…RETURNING of everything still in DEFAULT,
//     imposed on the staging by a DISTINCT-filtered upsert, then ATTACH — takes the parent
//     lock, statement-bounded. The fence is authoritative BY CONSTRUCTION: no watermark
//     logic ever decides correctness. If a month is too hot or too large to cut inside the
//     bound, the fence aborts, the parent stays authoritative, and the cadence retries.
func (s *Store) adoptDefaultServiceFactMonth(ctx context.Context, name string, month time.Time) error {
	return s.adoptServiceFactMonth(ctx, name, month, adoptionPolicy{
		fenceBudget: adoptionFenceBudget, rowGate: adoptionFenceRowGate,
	})
}

// adoptionPolicy separates the AUTOMATIC cadence (leader-safe 5s fence, declared row gate)
// from the OPERATOR command (explicit budget chosen for a maintenance window, no gate — the
// gate exists to keep the leader's cadence from repeating a doomed all-row fence, and the
// operator invoking `cerbix adopt-fact-month` has decided to pay the lock hold deliberately).
// One implementation, two policies: the D-0161 recovery mode shares every line of the
// automatic path instead of drifting beside it.
type adoptionPolicy struct {
	fenceBudget time.Duration
	rowGate     int // 0 disables the gate (operator mode)
}

// AdoptServiceFactMonthOperator is the operator recovery entry behind
// `cerbix adopt-fact-month` (D-0161): the same copy-authoritative adoption as the automatic
// cadence, with an operator-supplied fence budget and the row gate off. It covers both
// operator cases — a month past the declared bound, and a month that persistently fails the
// automatic fence after quiescence (a row count is not a wall-clock guarantee on arbitrary
// storage). The month is normalized to its UTC first instant; an already-attached month is a
// no-op success, so the command is idempotent.
func (s *Store) AdoptServiceFactMonthOperator(ctx context.Context, month time.Time, fenceBudget time.Duration) error {
	if fenceBudget <= 0 {
		return fmt.Errorf("store: operator adoption needs a positive fence budget")
	}
	m := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	name := serviceFactPartitionName(m)
	return s.adoptServiceFactMonth(ctx, name, m, adoptionPolicy{fenceBudget: fenceBudget})
}

func (s *Store) adoptServiceFactMonth(ctx context.Context, name string, month time.Time, policy adoptionPolicy) error {
	next := month.AddDate(0, 1, 0)
	from, to := month.Format(pgTimestamp), next.Format(pgTimestamp)

	state, err := s.factPartitionStateOf(ctx, name)
	if err != nil {
		return err
	}
	if state == factPartitionAttached {
		return nil
	}
	if state == factPartitionAbsent {
		if _, err := s.pool.Exec(ctx,
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (LIKE service_reliability_buckets INCLUDING ALL)`, name)); err != nil {
			return fmt.Errorf("store: stage adoption table %s: %w", name, err)
		}
	}
	if err := s.ensureStagingLifecycle(ctx, name, from, to); err != nil {
		return err
	}

	// Long phase, parent untouched: keyset copy in the MONTH INDEX's own order —
	// (bucket_start, service_id) — with the cursor derived from the ORDERED pick itself,
	// never from RETURNING order (which is not contractual and could stall progress on
	// re-copied batches).
	copyBatch := fmt.Sprintf(`
		WITH pick AS (
			SELECT %s FROM service_reliability_buckets_default
			 WHERE bucket_start >= '%s' AND bucket_start < '%s'
			   AND (bucket_start, service_id) > ($1::timestamptz, $2::uuid)
			 ORDER BY bucket_start, service_id LIMIT %d),
		ins AS (
			INSERT INTO %s (%s) SELECT %s FROM pick
			ON CONFLICT (service_id, bucket_start) DO UPDATE SET %s)
		SELECT bucket_start, service_id FROM pick
		 ORDER BY bucket_start DESC, service_id DESC LIMIT 1`,
		factColumns, from, to, adoptionBatchRows,
		name, factColumns, factColumns, factUpsertSet)
	// Progress is derived from DURABLE state, not memory: resume at the staging's own max
	// key. A cursor that restarted at zero re-upserted the whole prefix every retry, so a
	// month too large for one maintenance pass never reached the fence — a livelock the
	// pass timeout made permanent. Content divergence below the resume point is the FENCE's
	// job (authoritative sweep), so skipping the prefix is correct by construction.
	cursorBucket, cursorSvc := time.Time{}, "00000000-0000-0000-0000-000000000000"
	{
		var b time.Time
		var sv string
		err := s.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT bucket_start, service_id FROM %s
			  ORDER BY bucket_start DESC, service_id DESC LIMIT 1`, name)).Scan(&b, &sv)
		if err != nil && !noRows(err) {
			return fmt.Errorf("store: adoption resume point %s: %w", name, err)
		}
		if err == nil {
			cursorBucket, cursorSvc = b, sv
		}
	}
	for {
		var lastBucket time.Time
		var lastSvc string
		err := s.pool.QueryRow(ctx, copyBatch, cursorBucket, cursorSvc).Scan(&lastBucket, &lastSvc)
		if noRows(err) {
			break
		}
		if err != nil {
			return fmt.Errorf("store: adoption copy %s: %w", name, err)
		}
		cursorBucket, cursorSvc = lastBucket, lastSvc
	}

	// The fenced workload is inherently O(rows still in DEFAULT for the month): hole-free
	// incremental shrinking does not exist under native partitioning (any row removed from
	// DEFAULT before ATTACH is invisible through the parent — the P0 this design replaced).
	// So the supported bound is DECLARED and gated (D-0161, spec §10.11). The gate runs
	// TWICE: a cheap preflight here, before any parent lock, refuses the obvious oversize
	// month without ever queueing on the parent — and it runs AGAIN inside the fence, under
	// the ACCESS EXCLUSIVE lock, because this unlocked count is advisory by construction: a
	// fact writer can commit more rows between it and the lock acquisition, and a bound
	// enforced only here would be a TOCTOU, not a bound. The refusal surfaces through the
	// failing maintenance metric; recovery is the operator command (`cerbix adopt-fact-month`).
	if policy.rowGate > 0 {
		var fenceRows int
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(*) FROM (
				SELECT 1 FROM service_reliability_buckets_default
				 WHERE bucket_start >= '%s' AND bucket_start < '%s' LIMIT %d) t`,
			from, to, policy.rowGate+1)).Scan(&fenceRows); err != nil {
			return fmt.Errorf("store: adoption fence size %s: %w", name, err)
		}
		if fenceRows > policy.rowGate {
			return fmt.Errorf("store: adoption cutover %s: month exceeds the supported fence bound (%d+ rows > %d); run `cerbix adopt-fact-month` in a maintenance window",
				name, fenceRows, policy.rowGate)
		}
	}

	// The fence: AUTHORITATIVE by construction. Under the parent lock (locking a
	// partitioned parent locks its partitions, so every writer has drained), the DELETE's
	// RETURNING is the final content of every row still in DEFAULT, and the upsert imposes
	// it on the staging. The long-phase copy is a warmer that shrinks this sweep's
	// DISTINCT-filtered write set; it never decides correctness — a computed_at watermark
	// here was UNSOUND (assigned at the writer's transaction start) and is a named, killed
	// mutant. Statement-bounded; an abort leaves the parent authoritative and the staging
	// resumable.
	// ONE ABSOLUTE deadline over the whole cutover — Begin through COMMIT. The previous
	// shape SET a 5s statement_timeout, and statement_timeout RESTARTS per statement: LOCK,
	// sweep and ATTACH could each spend almost the "bound" while the parent sat under
	// ACCESS EXCLUSIVE. deadlineTx re-derives the server bounds from the shrinking remainder
	// before every statement (commit included) with the client net one tolerance behind —
	// the same mechanism, and the same lesson, as every slice path.
	// The fence deadline is CAPPED BY THE CALLER (claimRepairRangeBounded is the local
	// precedent): minting a fresh 5s when the maintenance context has 100ms left tells the
	// server it may run for seconds while the client net cancels in milliseconds — the net
	// wins, the connection dies, and deadlineTx's server-first invariant is broken from the
	// outside. An exhausted tail is refused BEFORE Begin; the parent stays authoritative and
	// the next cadence brings a full budget.
	deadline := time.Now().Add(policy.fenceBudget)
	if callerDeadline, ok := ctx.Deadline(); ok {
		if capped := callerDeadline.Add(-schedulingTolerance); capped.Before(deadline) {
			deadline = capped
		}
	}
	if time.Until(deadline) <= 0 {
		return fmt.Errorf("store: adoption cutover %s: %w", name, errSliceBudget)
	}
	fctx, cancel := context.WithDeadline(ctx, deadline.Add(schedulingTolerance))
	defer cancel()
	rawTx, err := s.pool.Begin(fctx)
	if err != nil {
		return fmt.Errorf("store: begin adoption cutover: %w", err)
	}
	defer rawTx.Rollback(ctx) //nolint:errcheck // no-op after commit
	tx := newDeadlineTx(rawTx, deadline, 0)
	if _, err := tx.Exec(fctx, `LOCK TABLE service_reliability_buckets IN ACCESS EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("store: adoption cutover %s: %w", name, err)
	}
	// The AUTHORITATIVE gate: re-counted under the parent lock, where every writer has
	// drained and the count can no longer move. The unlocked preflight above is a cheap
	// courtesy; only this check makes "no all-row fence past the bound" an invariant instead
	// of a race. A refusal here rolls back, releases the lock, and the parent stays
	// authoritative — bounded damage is the lock-queue wait, never the oversize DELETE.
	if policy.rowGate > 0 {
		var lockedRows int
		if err := tx.QueryRow(fctx, fmt.Sprintf(
			`SELECT count(*) FROM (
				SELECT 1 FROM service_reliability_buckets_default
				 WHERE bucket_start >= '%s' AND bucket_start < '%s' LIMIT %d) t`,
			from, to, policy.rowGate+1)).Scan(&lockedRows); err != nil {
			return fmt.Errorf("store: adoption fence size under lock %s: %w", name, err)
		}
		if lockedRows > policy.rowGate {
			return fmt.Errorf("store: adoption cutover %s: month exceeds the supported fence bound under the parent lock (%d+ rows > %d); run `cerbix adopt-fact-month` in a maintenance window",
				name, lockedRows, policy.rowGate)
		}
	}
	steps := []string{
		fmt.Sprintf(`
			WITH moved AS (
				DELETE FROM service_reliability_buckets_default
				 WHERE bucket_start >= '%s' AND bucket_start < '%s' RETURNING *)
			INSERT INTO %s (%s) SELECT %s FROM moved
			ON CONFLICT (service_id, bucket_start) DO UPDATE SET %s
			WHERE (%s.project_id, %s.epoch_id, %s.good_us, %s.bad_us, %s.unknown_us,
			       %s.excluded_us, %s.healthy_us, %s.degraded_us, %s.down_us,
			       %s.health_unknown_us, %s.state, %s.computed_at)
			   IS DISTINCT FROM
			      (EXCLUDED.project_id, EXCLUDED.epoch_id, EXCLUDED.good_us, EXCLUDED.bad_us,
			       EXCLUDED.unknown_us, EXCLUDED.excluded_us, EXCLUDED.healthy_us,
			       EXCLUDED.degraded_us, EXCLUDED.down_us, EXCLUDED.health_unknown_us,
			       EXCLUDED.state, EXCLUDED.computed_at)`,
			from, to, name, factColumns, factColumns, factUpsertSet,
			name, name, name, name, name, name, name, name, name, name, name, name),
		fmt.Sprintf(`ALTER TABLE service_reliability_buckets ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')`,
			name, from, to),
	}
	for _, q := range steps {
		if _, err := tx.Exec(fctx, q); err != nil {
			return fmt.Errorf("store: adoption cutover %s: %w", name, err)
		}
	}
	return tx.Commit(fctx)
}

// adoptionFenceBudget is the TOTAL cutover budget — queueing for the parent lock, the
// authoritative sweep, the ATTACH and the COMMIT together, transaction-wide.
const adoptionFenceBudget = 5 * time.Second

// adoptionFenceMaxRows is the declared supported bound on a fenced month move (D-0161,
// spec §10.11). Conservative against the 5s budget (an index-driven DELETE clears well over
// this in a second on commodity hardware); a month past it needs the operator command
// (`cerbix adopt-fact-month`), not an endlessly re-aborting cadence. The bound is a row
// count, not a wall-clock proof: a month that persistently times out under the bound after
// quiescence takes the same operator path.
const adoptionFenceMaxRows = 100000

// adoptionFenceRowGate is overridable in tests to construct the oversize path without
// planting six figures of rows.
var adoptionFenceRowGate = adoptionFenceMaxRows

// ensureStagingLifecycle makes a staging table safe to LIVE with, idempotently, on every
// pass: orphans swept, the parent's foreign keys present (deletion cascades through the
// staging), the month CHECK present and validated (the fenced ATTACH validates by
// implication instead of scanning). LIKE ... INCLUDING ALL copies NO foreign keys, so a
// staging without this call retained tenant fact rows a service deletion had already
// removed everywhere else — and ADD CONSTRAINT would then fail on the ghost forever.
func (s *Store) ensureStagingLifecycle(ctx context.Context, name, from, to string) error {
	// Once BOTH validated foreign keys exist, new orphans are impossible and the anti-join
	// sweeps are pure rescans of the whole staging on every retry — skip straight to the
	// idempotent constraint adds. "Exist" is verified BY THE COMPLETE DEFINITION, never by
	// name and never by a partial predicate: contype, validation, BOTH column-name arrays
	// (constrained and referenced, in declared order — a name-carrying FK over the WRONG
	// columns cascaded nothing), referenced relation, ON DELETE and ON UPDATE actions, MATCH
	// type, deferrability AND its initial state (INITIALLY IMMEDIATE wearing the deferred
	// key's name changes deletion-time semantics). An unvalidated key with the correct shape
	// is NOT accepted either: it has not proven the existing rows, so trusting it would skip
	// the orphan sweep over rows it never checked. Anything reserved-named and non-exact is
	// a squatter and fails closed.
	var fkCount, squatters int
	err := s.pool.QueryRow(ctx, `
		WITH cons AS (
			SELECT c.conname, c.contype, c.convalidated, c.confrelid, c.confdeltype,
			       c.confupdtype, c.confmatchtype, c.condeferrable, c.condeferred,
			       (SELECT array_agg(a.attname::text ORDER BY o.ord)
			          FROM unnest(c.conkey) WITH ORDINALITY o(attnum, ord)
			          JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = o.attnum) AS cols,
			       (SELECT array_agg(a.attname::text ORDER BY o.ord)
			          FROM unnest(c.confkey) WITH ORDINALITY o(attnum, ord)
			          JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = o.attnum) AS refcols
			  FROM pg_constraint c
			 WHERE c.conrelid = to_regclass($1) AND c.conname IN ($2, $3)),
		judged AS (
			SELECT (contype = 'f' AND convalidated
			        AND confupdtype = 'a' AND confmatchtype = 's'
			        AND refcols = ARRAY['id', 'project_id']::text[]
			        AND ((conname = $2 AND confrelid = 'services'::regclass
			              AND confdeltype = 'c' AND NOT condeferrable AND NOT condeferred
			              AND cols = ARRAY['service_id', 'project_id']::text[])
			          OR (conname = $3 AND confrelid = 'service_evaluation_epochs'::regclass
			              AND confdeltype = 'a' AND condeferrable AND condeferred
			              AND cols = ARRAY['epoch_id', 'project_id']::text[]))) AS exact
			  FROM cons)
		SELECT count(*) FILTER (WHERE exact), count(*) FILTER (WHERE NOT exact) FROM judged`,
		name, name+"_service_fk", name+"_epoch_fk").Scan(&fkCount, &squatters)
	if err != nil {
		return fmt.Errorf("store: staging constraint state %s: %w", name, err)
	}
	if squatters > 0 {
		// FAIL CLOSED: a wrong constraint wearing the right name cannot be repaired blindly
		// (dropping an unknown operator-made constraint is not this code's call), and
		// proceeding would let 42710 mask the missing real key.
		return fmt.Errorf("store: staging %s carries a constraint with a reserved name but the wrong definition; refusing to adopt", name)
	}
	sweepDone := fkCount == 2
	steps := []string{
		// Orphan sweep BEFORE the keys: rows copied by an interrupted pass that predates
		// the constraints would fail their validation and wedge every retry.
		fmt.Sprintf(`DELETE FROM %s t WHERE NOT EXISTS (
			SELECT 1 FROM services sv WHERE sv.id = t.service_id AND sv.project_id = t.project_id)`, name),
		fmt.Sprintf(`DELETE FROM %s t WHERE NOT EXISTS (
			SELECT 1 FROM service_evaluation_epochs e WHERE e.id = t.epoch_id AND e.project_id = t.project_id)`, name),
		fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s_service_fk
			FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE`, name, name),
		fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s_epoch_fk
			FOREIGN KEY (epoch_id, project_id) REFERENCES service_evaluation_epochs (id, project_id)
			ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED`, name, name),
		fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s_month_chk
			CHECK (bucket_start >= '%s' AND bucket_start < '%s')`, name, name, from, to),
	}
	for i, q := range steps {
		if sweepDone && i < 2 { // the two anti-join sweeps
			continue
		}
		if _, err := s.pool.Exec(ctx, q); err != nil && !isDuplicateObject(err) {
			return fmt.Errorf("store: staging lifecycle %s: %w", name, err)
		}
	}
	return nil
}

// isDuplicateObject matches 42710 (constraint already exists) for resume idempotence.
func isDuplicateObject(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42710"
}

// adoptionBatchRows bounds one copy batch. There are no delta "warmer" passes anymore: the
// fence's authoritative sweep is the only reconciliation, and correctness-neutral warmers
// with no examined-row bound were exactly the shape round 1/2 rejected.
const adoptionBatchRows = 500

// capSQL saturates each sampled queue-depth count. The counts are LIMIT-capped, not
// unbounded aggregates: a gauge that says "at least a thousand" answers every operational
// question a bigger exact number would, and the sample's cost stays fixed no matter how much
// parked history accumulates.
const capSQL = "1000"

// ServiceReliabilityStats samples the service subsystem, BOUNDED by construction and pinned
// by a plan regression: each state count is an index-backed probe capped at capSQL rows
// (00067 indexes the claimable states, 00074 the terminal error state), the worst lag is ONE
// probe of 00074's expression index (min of COALESCE(sealed_through, era_start), GREATEST-
// clamped at zero because a fresh adoption's era is legitimately in the future), and the
// event totals read the three-row persisted aggregate the owning transactions maintain.
func (s *Store) ServiceReliabilityStats(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
	var st metrics.ServiceReliabilityStat
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM (SELECT 1 FROM service_repair_ranges WHERE state = 'pending' LIMIT `+capSQL+`) p),
		       (SELECT count(*) FROM (SELECT 1 FROM service_repair_ranges WHERE state = 'running' LIMIT `+capSQL+`) r),
		       (SELECT count(*) FROM (SELECT 1 FROM service_repair_ranges WHERE state = 'error' LIMIT `+capSQL+`) e),
		       COALESCE(GREATEST(0, extract(epoch FROM statement_timestamp() -
		           (SELECT min(COALESCE(sealed_through, era_start)) FROM service_materialization))), 0)`).
		Scan(&st.RepairPending, &st.RepairRunning, &st.RepairErrored, &st.WatermarkLagSeconds); err != nil {
		return st, fmt.Errorf("store: repair queue stats: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, value FROM service_metric_events`)
	if err != nil {
		return st, fmt.Errorf("store: metric events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var value int64
		if err := rows.Scan(&kind, &value); err != nil {
			return st, fmt.Errorf("store: scan metric event: %w", err)
		}
		switch kind {
		case metricEventEpochFanout:
			st.EpochFanoutTotal = value
		case metricEventLateArrivals:
			st.LateArrivalsTotal = value
		case metricEventLateOverflow:
			st.LateOverflowTotal = value
		}
	}
	return st, rows.Err()
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
