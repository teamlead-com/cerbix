package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Decision-ledger partition maintenance (func-reliability-gate.md D10, §7 "Partitions",
// "Ownership", "Fencing and the loop"; iter-0163 changeset 3).
//
// The ledger `service_gate_decisions` is RANGE-partitioned by UTC day and has NO default
// partition, so its maintenance is what keeps decisions writable (partitions ahead of the
// clock) and bounded (partitions behind retention removed). Three facts shape everything here:
//
//   - AUTHORITY is a session-level advisory lock on ONE pinned connection, and every statement
//     of a pass — DDL and registry writes alike — runs on that connection. If it dies,
//     PostgreSQL releases the lock AND the connection can carry no further statement, so a
//     deposed node cannot detach or drop anything after the loss: fencing by construction.
//   - the pass has ONE timeline, measured from passStart: work until +27 s (creation in
//     [0, 12 s), removal in [12 s, 27 s)), release proof in [27 s, 30 s]; that total is the
//     scheduler's subCadenceTimeout. Slices are enforced PER STATEMENT as a wall bound —
//     statement_timeout = min(10 s, slice end − now), lock_timeout = min(2 s, that), the
//     transaction's context deadline one net behind the slice end — never as a sum of timers
//     and never as a check between statements.
//   - OWNERSHIP is the marker, not the OID: a relation is ours when its COMMENT is
//     'cerbix:gate-ledger:<owner_token>' AND its shape matches the day; the registry's relid
//     is a locator refreshed when the marker matches a new OID. Every transition PostgreSQL
//     allows inside a transaction commits with its registry write; DETACH … CONCURRENTLY alone
//     runs behind a committed `detaching` intent and every crash outcome around it is
//     reconciled on the next pass. Only a marker or shape mismatch, or a state no crash
//     produces, refuses — that day alone, counted partition_identity.

// GateMaintenanceLockKey is the gate maintenance session's advisory-lock key: slot 3 of the
// "cerbix" + slot namespace (slot 1 scheduler leadership, slot 2 migrations). The three are
// asserted pairwise distinct by tests in this package and in the scheduler's.
const GateMaintenanceLockKey int64 = 0x6365726269780003

// The one timeline of a pass, measured from passStart (D10, review rounds 11–12).
const (
	// GatePassLifecycle is the whole acquire → work → cleanup lifecycle. It equals the
	// scheduler's subCadenceTimeout, and the scheduler asserts that equality in a test.
	GatePassLifecycle = 30 * time.Second
	// gateWorkDeadline is where work stops; the release proof owns the rest.
	gateWorkDeadline = 27 * time.Second
	// gateCreationSlice is the end of the creation slice [0, 12 s); removal runs [12 s, 27 s).
	gateCreationSlice = 12 * time.Second
	// gateReleaseBudget bounds the release proof: min(now + 3 s, passStart + 30 s).
	gateReleaseBudget = 3 * time.Second
	// gateStatementMax / gateLockTimeoutMax cap one statement's server bounds; the slice
	// remainder clamps them further.
	gateStatementMax   = 10 * time.Second
	gateLockTimeoutMax = 2 * time.Second
	// gateMinStatement: a statement is not started at all with less than this left in its
	// slice, because a bound below it cannot complete useful work.
	gateMinStatement = 500 * time.Millisecond
	// gateContextNet is how far behind the slice end the client context deadline sits: the
	// server bound speaks first (a 57014/55P03 keeps the connection), the client net only
	// catches a server that never answers — and when it fires pgx closes the pinned connection,
	// which is the fence, not a failure of it.
	gateContextNet = 100 * time.Millisecond
)

// GateMaintenanceConfig mirrors the five gate.decision_* keys (§5a), validated by config.Load.
type GateMaintenanceConfig struct {
	RetentionDays      int
	LeadDays           int
	CreateMax          int
	PurgeEvery         time.Duration
	PurgeMaxPartitions int
}

// GateLedgerGauges are the four D10 gauges, computed from the registry joined to the catalog by
// relid — never by counting rows.
type GateLedgerGauges struct {
	PendingDrop            int
	OldestAgeSeconds       float64
	WritableHorizonSeconds float64
	Bytes                  int64
}

// GateMaintenanceReport is what one pass did and saw. Statement failures are COUNTED through the
// metrics interface as they happen and listed here for the caller's log; the pass itself returns
// an error only when it could not continue (the pinned connection died, the context ended).
type GateMaintenanceReport struct {
	// CreationSkipped: authority arrived at or after t = 12 s. RemovalSkipped: fewer than 500 ms
	// were left before t = 27 s, so the pass did only its cleanup.
	CreationSkipped bool
	RemovalSkipped  bool
	Created         int // create transactions committed
	Attached        int // attach transactions committed
	Detached        int // DETACH … CONCURRENTLY completed (state detached written)
	Finalized       int // DETACH … FINALIZE completed
	Dropped         int // DROP TABLE committed
	// Refusals are partition_identity refusals, one line per day, already counted.
	Refusals []string
	// Failures are statement failures (lock_timeout / statement_timeout / error), already counted.
	Failures []string
	// Gauges are valid only when GaugesValid: the gauge query ran after the work.
	Gauges      GateLedgerGauges
	GaugesValid bool
}

// GateMaintenanceMetrics is the counter the pass moves: cerbix_gate_maintenance_errors_total
// {kind} with exactly lock_timeout | statement_timeout | partition_identity | error. Implemented
// by *metrics.Registry; nil-safe.
type GateMaintenanceMetrics interface {
	RecordGateMaintenanceError(kind string) error
}

// Maintenance error kinds (the closed label set of the family).
const (
	gateErrLockTimeout       = "lock_timeout"
	gateErrStatementTimeout  = "statement_timeout"
	gateErrPartitionIdentity = "partition_identity"
	gateErrOther             = "error"
)

// gateLedgerPartitionNameRE is the ONE spelling of a day partition's name; anything else in the registry
// is refused before it can reach an identifier position.
var gateLedgerPartitionNameRE = regexp.MustCompile(`^service_gate_decisions_p[0-9]{8}$`)

const gateLedgerTable = "service_gate_decisions"

func gateRelname(day time.Time) string {
	return "service_gate_decisions_p" + day.UTC().Format("20060102")
}

// gateDayBounds renders the [day, day+1) bounds the way the migration's bootstrap did — explicit
// +00 so the partition constraint is the same text under any session TimeZone.
func gateDayBounds(day time.Time) (lo, hi string) {
	d := day.UTC()
	return d.Format("2006-01-02") + " 00:00:00+00", d.AddDate(0, 0, 1).Format("2006-01-02") + " 00:00:00+00"
}

// gateMaintenanceHooks are TEST-ONLY fault-injection points; production passes nil. They let the
// §7 matrix crash the pass at every statement boundary, blackhole a RESET, delay acquisition and
// make the unlock answer wrongly — the mechanisms the tests must reach, not simulate.
type gateMaintenanceHooks struct {
	// beforeAcquire runs before the pool acquisition (a delayed pool.Acquire).
	beforeAcquire func()
	// beforeStatement runs on the pass's connection right before each guarded statement, with
	// the transaction when there is one. An error aborts the pass at that boundary.
	beforeStatement func(ctx context.Context, stage string, conn dbConn) error
	// beforeRelease runs under the release proof's own context, before its RESETs.
	beforeRelease func(ctx context.Context, conn *pgxpool.Conn) error
	// unlockSQL replaces the unlock statement ($1 = the key).
	unlockSQL string
}

// RunGateLedgerMaintenancePass is one complete pass lifecycle on the gate's OWN fenced session:
// acquire the gate lock on a pinned connection (inside the timeline — pool.Acquire blocks even
// though pg_try_advisory_lock does not), run the work under the 27 s deadline, and release as a
// proof under min(now + 3 s, passStart + 30 s). acquired=false means the lock is held elsewhere
// (the pass is skipped) or acquisition itself failed. The report is valid whenever acquired.
func (s *Store) RunGateLedgerMaintenancePass(
	ctx context.Context, passStart time.Time, cfg GateMaintenanceConfig,
	clock func() time.Time, metrics GateMaintenanceMetrics,
) (GateMaintenanceReport, bool, error) {
	return s.runGateLedgerMaintenancePass(ctx, passStart, cfg, clock, metrics, nil)
}

func (s *Store) runGateLedgerMaintenancePass(
	ctx context.Context, passStart time.Time, cfg GateMaintenanceConfig,
	clock func() time.Time, metrics GateMaintenanceMetrics, hooks *gateMaintenanceHooks,
) (GateMaintenanceReport, bool, error) {
	if clock == nil {
		clock = time.Now
	}
	if hooks != nil && hooks.beforeAcquire != nil {
		hooks.beforeAcquire()
	}
	// Acquisition is INSIDE the timeline: an Acquire that has not returned by the work deadline
	// is given up, so a pool starved for 40 s cannot make a 30 s lifecycle 43.
	budget := passStart.Add(gateWorkDeadline).Sub(clock())
	if budget < gateMinStatement {
		budget = gateMinStatement
	}
	actx, cancel := context.WithTimeout(ctx, budget)
	ls, ok, err := s.TryBecomeLeaderSession(actx, GateMaintenanceLockKey)
	cancel()
	if err != nil || !ok {
		return GateMaintenanceReport{}, false, err
	}
	rep, werr := ls.runGateLedgerMaintenance(ctx, passStart, cfg, clock, metrics, hooks)
	// The cleanup deadline is ABSOLUTE: min(now + 3 s, passStart + 30 s), never "now + 3 s"
	// after a late work return.
	deadline := clock().Add(gateReleaseBudget)
	if end := passStart.Add(GatePassLifecycle); end.Before(deadline) {
		deadline = end
	}
	rerr := ls.releaseProved(deadline, clock, hooks)
	return rep, true, errors.Join(werr, rerr)
}

// RunGateLedgerMaintenance runs the work of one pass on this session's pinned connection —
// EVERY statement on ls.conn, never the pool — under the one timeline measured from passStart.
// When the session was acquired late (passStart predates acquisition), creation is skipped once
// t ≥ 12 s and removal begins at once if ≥ 500 ms remain before t = 27 s; otherwise the pass
// does nothing and the caller proceeds to the release proof.
func (ls *LeaderSession) RunGateLedgerMaintenance(
	ctx context.Context, passStart time.Time, cfg GateMaintenanceConfig,
	clock func() time.Time, metrics GateMaintenanceMetrics,
) (GateMaintenanceReport, error) {
	return ls.runGateLedgerMaintenance(ctx, passStart, cfg, clock, metrics, nil)
}

func (ls *LeaderSession) runGateLedgerMaintenance(
	ctx context.Context, passStart time.Time, cfg GateMaintenanceConfig,
	clock func() time.Time, metrics GateMaintenanceMetrics, hooks *gateMaintenanceHooks,
) (GateMaintenanceReport, error) {
	if clock == nil {
		clock = time.Now
	}
	p := &gatePass{ls: ls, cfg: cfg, clock: clock, metrics: metrics, hooks: hooks, passStart: passStart}
	createEnd := passStart.Add(gateCreationSlice)
	workEnd := passStart.Add(gateWorkDeadline)

	now := clock()
	creation := now.Before(createEnd)
	if !creation {
		p.rep.CreationSkipped = true
	}
	if workEnd.Sub(now) < gateMinStatement {
		p.rep.RemovalSkipped = true
		return p.rep, nil
	}
	// The preamble — relid rebind, the database's UTC day, the ownership survey — serves both
	// phases, so it runs under the work deadline; none of its statements can wait on a lock.
	rows, today, err := p.preamble(ctx, workEnd)
	if err != nil {
		return p.rep, err
	}
	if creation {
		if err := p.creation(ctx, createEnd, rows, today); err != nil && !errors.Is(err, errSliceBudget) {
			return p.rep, err
		}
	}
	if err := p.removal(ctx, workEnd, rows); err != nil && !errors.Is(err, errSliceBudget) {
		return p.rep, err
	}
	if err := p.gauges(ctx, workEnd); err != nil && !errors.Is(err, errSliceBudget) {
		return p.rep, err
	}
	return p.rep, nil
}

// gatePass is the state of one pass on one session.
type gatePass struct {
	ls        *LeaderSession
	cfg       GateMaintenanceConfig
	clock     func() time.Time
	metrics   GateMaintenanceMetrics
	hooks     *gateMaintenanceHooks
	passStart time.Time
	rep       GateMaintenanceReport
	// dead: the pinned connection is closed; nothing more can run on it and the pass ends.
	dead bool
}

// errGateSessionDead ends a pass whose pinned connection is gone: the lock went with it, so the
// successor is the only one who may act now.
var errGateSessionDead = errors.New("store: gate maintenance session connection is closed")

// bounds derives one statement's server bounds from the slice remainder as a WALL bound:
// statement_timeout = min(10 s, remaining), lock_timeout = min(2 s, statement_timeout). ok=false
// with fewer than 500 ms left — the statement is not started.
func (p *gatePass) bounds(sliceEnd time.Time) (remaining, statement, lock time.Duration, ok bool) {
	remaining = sliceEnd.Sub(p.clock())
	if remaining < gateMinStatement {
		return remaining, 0, 0, false
	}
	statement = remaining
	if statement > gateStatementMax {
		statement = gateStatementMax
	}
	lock = statement
	if lock > gateLockTimeoutMax {
		lock = gateLockTimeoutMax
	}
	return remaining, statement, lock, true
}

func (p *gatePass) hook(ctx context.Context, stage string, conn dbConn) error {
	if p.hooks != nil && p.hooks.beforeStatement != nil {
		return p.hooks.beforeStatement(ctx, stage, conn)
	}
	return nil
}

// count records one maintenance error of the given kind.
func (p *gatePass) count(kind string) {
	if p.metrics != nil {
		_ = p.metrics.RecordGateMaintenanceError(kind)
	}
}

// fail classifies and counts one failed statement: 55P03 → lock_timeout, 57014 →
// statement_timeout, anything else → error. It also notices a dead connection.
func (p *gatePass) fail(stage string, err error) error {
	kind := gateErrOther
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "55P03":
			kind = gateErrLockTimeout
		case "57014":
			kind = gateErrStatementTimeout
		}
	}
	p.count(kind)
	p.rep.Failures = append(p.rep.Failures, fmt.Sprintf("%s: %s: %v", stage, kind, err))
	if p.ls.conn.Conn().IsClosed() {
		p.dead = true
	}
	return fmt.Errorf("store: gate maintenance %s: %w", stage, err)
}

// refuse records one partition_identity refusal for a day: counted, listed, and that day is left
// alone for this pass while the others go on.
func (p *gatePass) refuse(day time.Time, reason string) {
	p.count(gateErrPartitionIdentity)
	p.rep.Refusals = append(p.rep.Refusals, fmt.Sprintf("%s (%s): %s", day.UTC().Format("2006-01-02"), gateRelname(day), reason))
}

// resetSession pairs every session-level SET with RESET lock_timeout; RESET statement_timeout,
// immediately after the statement it guarded, on success and on every error path. It runs under
// a short bound of its own, detached from the pass's cancellation: a step-down that cancels the
// work must still be able to leave the session clean — and if it cannot, the release proof will
// not hand the connection back.
func (p *gatePass) resetSession(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	for _, q := range []string{`RESET lock_timeout`, `RESET statement_timeout`} {
		if _, err := p.ls.conn.Exec(rctx, q); err != nil {
			if p.dead {
				// The statement before this one already failed and was counted; the RESET
				// failing on the dead connection is the fence doing its work, not a second error.
				return fmt.Errorf("store: gate maintenance reset: %w", err)
			}
			return p.fail("reset", err)
		}
	}
	return nil
}

// guarded runs one AUTOCOMMIT statement (or a read that can wait on a lock) on the pinned
// connection under session-level SET bounds, followed IMMEDIATELY by the RESET pair.
// DETACH … CONCURRENTLY and … FINALIZE cannot run inside a transaction, which is why these use
// session SETs at all. When count is false the statement is a sampling READ (gauges), whose
// failure is neither counted in the maintenance family nor fatal — but whose bounds and RESET
// still apply, so it cannot leak a setting or wait past its clamp.
func (p *gatePass) guarded(ctx context.Context, sliceEnd time.Time, stage string, fn func(ctx context.Context) error) error {
	return p.guardedImpl(ctx, sliceEnd, stage, true, fn)
}

func (p *gatePass) guardedImpl(ctx context.Context, sliceEnd time.Time, stage string, count bool, fn func(ctx context.Context) error) error {
	if p.dead {
		return errGateSessionDead
	}
	remaining, st, lt, ok := p.bounds(sliceEnd)
	if !ok {
		return errSliceBudget
	}
	sctx, cancel := context.WithTimeout(ctx, remaining+gateContextNet)
	defer cancel()
	classify := func(stage string, err error) error {
		if count {
			return p.fail(stage, err)
		}
		if p.ls.conn.Conn().IsClosed() {
			p.dead = true
		}
		return fmt.Errorf("store: gate maintenance %s: %w", stage, err)
	}
	if _, err := p.ls.conn.Exec(sctx, fmt.Sprintf(`SET statement_timeout = %d`, st.Milliseconds())); err != nil {
		return classify(stage, err)
	}
	if _, err := p.ls.conn.Exec(sctx, fmt.Sprintf(`SET lock_timeout = %d`, lt.Milliseconds())); err != nil {
		return errors.Join(classify(stage, err), p.resetSession(ctx))
	}
	err := p.hook(sctx, stage, p.ls.conn)
	if err == nil {
		if err = fn(sctx); err != nil {
			err = classify(stage, err)
		}
	}
	if rerr := p.resetSession(ctx); rerr != nil {
		err = errors.Join(err, rerr)
	}
	if p.dead {
		err = errors.Join(err, errGateSessionDead)
	}
	return err
}

// read runs one catalog/registry READ that takes no heavyweight lock, under the slice's 500 ms
// rule and context net only.
func (p *gatePass) read(ctx context.Context, sliceEnd time.Time, stage string, fn func(ctx context.Context) error) error {
	if p.dead {
		return errGateSessionDead
	}
	remaining, _, _, ok := p.bounds(sliceEnd)
	if !ok {
		return errSliceBudget
	}
	rctx, cancel := context.WithTimeout(ctx, remaining+gateContextNet)
	defer cancel()
	if err := p.hook(rctx, stage, p.ls.conn); err != nil {
		return err
	}
	if err := fn(rctx); err != nil {
		return p.fail(stage, err)
	}
	return nil
}

// gateTx is a transaction whose every statement re-derives SET LOCAL statement_timeout and
// lock_timeout from the slice remainder before running, COMMIT included.
type gateTx struct {
	p        *gatePass
	tx       pgx.Tx
	sliceEnd time.Time
}

func (t *gateTx) setLocal(ctx context.Context, stage string) error {
	_, st, lt, ok := t.p.bounds(t.sliceEnd)
	if !ok {
		return errSliceBudget
	}
	if _, err := t.tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, st.Milliseconds())); err != nil {
		return t.p.fail(stage, err)
	}
	if _, err := t.tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = %d`, lt.Milliseconds())); err != nil {
		return t.p.fail(stage, err)
	}
	return nil
}

func (t *gateTx) exec(ctx context.Context, stage, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := t.setLocal(ctx, stage); err != nil {
		return pgconn.CommandTag{}, err
	}
	if err := t.p.hook(ctx, stage, t.tx); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return tag, t.p.fail(stage, err)
	}
	return tag, nil
}

// queryRowScan runs one bounded single-row query. pgx.ErrNoRows comes back unwrapped and
// uncounted — it is an answer, not a failure.
func (t *gateTx) queryRowScan(ctx context.Context, stage, sql string, args []any, dest ...any) error {
	if err := t.setLocal(ctx, stage); err != nil {
		return err
	}
	if err := t.p.hook(ctx, stage, t.tx); err != nil {
		return err
	}
	if err := t.tx.QueryRow(ctx, sql, args...).Scan(dest...); err != nil {
		if noRows(err) {
			return err
		}
		return t.p.fail(stage, err)
	}
	return nil
}

// inTx runs fn in one transaction on the pinned connection, bounded per statement, and commits
// it bounded the same way. The client context deadline is the slice end plus one net; a
// statement blocked at the boundary is ended by the server bound first and, failing that, by
// the net — either way its transaction is rolled back by the slice end.
func (p *gatePass) inTx(ctx context.Context, sliceEnd time.Time, stage string, fn func(ctx context.Context, tx *gateTx) error) error {
	if p.dead {
		return errGateSessionDead
	}
	remaining, _, _, ok := p.bounds(sliceEnd)
	if !ok {
		return errSliceBudget
	}
	tctx, cancel := context.WithTimeout(ctx, remaining+gateContextNet)
	defer cancel()
	raw, err := p.ls.conn.Begin(tctx)
	if err != nil {
		return p.fail(stage, err)
	}
	tx := &gateTx{p: p, tx: raw, sliceEnd: sliceEnd}
	if err := fn(tctx, tx); err != nil {
		// The rollback must be able to run even when the work context is already gone, or the
		// session stays in an aborted transaction; it is bounded, and if it fails the release
		// proof discards the connection rather than returning it.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		if rerr := raw.Rollback(rctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) && !p.dead {
			err = errors.Join(err, p.fail(stage+".rollback", rerr))
		}
		rcancel()
		if p.dead {
			err = errors.Join(err, errGateSessionDead)
		}
		return err
	}
	if err := tx.setLocal(tctx, stage+".commit"); err != nil {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		_ = raw.Rollback(rctx)
		rcancel()
		return err
	}
	if err := p.hook(tctx, stage+".commit", raw); err != nil {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		_ = raw.Rollback(rctx)
		rcancel()
		return err
	}
	if err := raw.Commit(tctx); err != nil {
		err = p.fail(stage+".commit", err)
		if p.dead {
			err = errors.Join(err, errGateSessionDead)
		}
		return err
	}
	return nil
}

// ── The registry survey: ownership by marker and shape, per day ──────────────────────────────

// gateDayRow is one registry row joined to the catalog. The relation is located by NAME first
// (to_regclass(relname), our marker required), else by relid (our marker required, the relation
// may have been renamed by hand); oid is 0 when neither locates an owned relation.
type gateDayRow struct {
	day        time.Time
	relname    string
	token      string
	relid      uint32
	state      string
	detachedAt *time.Time

	nameExists, nameMarked bool
	oidExists, oidMarked   bool
	oid                    uint32
	curRelname             *string

	attached, pending bool
	// boundOK / checkOK are the SHAPE, known only after inspect(); true until then.
	boundOK, checkOK bool
	shapeChecked     bool
	pastCutoff       bool
	dropDue          bool
}

// gateSurveySQL is the ownership survey: registry ⨝ catalog by name and by relid, marker,
// attachment and detach-pending state, cutoff and drop-due — every function in it is LOCK-FREE
// (to_regclass, obj_description, pg_inherits), so a partition somebody holds ACCESS EXCLUSIVE on
// cannot stall the survey of every other day. The SHAPE (pg_get_expr of the bounds,
// pg_get_constraintdef of the day CHECK) needs ACCESS SHARE on the relation and is checked per
// day, under lock_timeout, right before the act (gateShapeSQL). `%s` is an optional day filter.
const gateSurveySQL = `
WITH r AS (
    SELECT p.day, p.relname, p.owner_token, p.relid, p.state, p.detached_at,
           ((p.day + 1)::timestamp AT TIME ZONE 'UTC') AS hi,
           'cerbix:gate-ledger:' || p.owner_token::text AS marker,
           to_regclass(p.relname)::oid                  AS name_oid
      FROM service_gate_decision_partitions p
     WHERE p.state <> 'dropped' %s
), loc AS (
    SELECT r.*,
           (r.name_oid IS NOT NULL AND obj_description(r.name_oid, 'pg_class') = r.marker) AS name_marked,
           EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = r.relid) AS oid_exists,
           COALESCE((SELECT obj_description(c.oid, 'pg_class') = r.marker FROM pg_class c WHERE c.oid = r.relid), false) AS oid_marked
      FROM r
), pick AS (
    SELECT loc.*,
           CASE WHEN loc.name_oid IS NOT NULL AND loc.name_marked THEN loc.name_oid
                WHEN loc.name_oid IS NULL AND loc.oid_exists AND loc.oid_marked THEN loc.relid
           END AS oid
      FROM loc
)
SELECT pick.day, pick.relname, pick.owner_token::text, pick.relid, pick.state, pick.detached_at,
       pick.name_oid IS NOT NULL, pick.name_marked, pick.oid_exists, pick.oid_marked,
       COALESCE(pick.oid, 0), c.relname,
       i.inhrelid IS NOT NULL, COALESCE(i.inhdetachpending, false),
       pick.hi <= now() - make_interval(days => $1),
       pick.detached_at IS NOT NULL AND pick.detached_at <= now() - ($2::float8 * interval '1 second')
  FROM pick
  LEFT JOIN pg_class c ON c.oid = pick.oid
  LEFT JOIN pg_inherits i ON i.inhrelid = c.oid AND i.inhparent = 'service_gate_decisions'::regclass
 ORDER BY pick.day`

// gateShapeSQL is the per-day shape check, rendered by format() in the SAME session as
// pg_get_expr and pg_get_constraintdef render the catalog's text, so the comparison is
// session-TimeZone-proof. Both functions take ACCESS SHARE on the relation: this runs guarded.
const gateShapeSQL = `
SELECT COALESCE(pg_get_expr(c.relpartbound, c.oid) = format('FOR VALUES FROM (%L) TO (%L)', $2::timestamptz, $3::timestamptz), false),
       EXISTS (SELECT 1 FROM pg_constraint k
                WHERE k.conrelid = c.oid AND k.contype = 'c' AND k.conname = $4
                  AND pg_get_constraintdef(k.oid) = format(
                      'CHECK (((evaluated_at >= %L::timestamp with time zone) AND (evaluated_at < %L::timestamp with time zone)))',
                      $2::timestamptz, $3::timestamptz))
  FROM pg_class c WHERE c.oid = $1::oid`

func (p *gatePass) scanSurvey(rows pgx.Rows) ([]gateDayRow, error) {
	defer rows.Close()
	var out []gateDayRow
	for rows.Next() {
		var r gateDayRow
		if err := rows.Scan(&r.day, &r.relname, &r.token, &r.relid, &r.state, &r.detachedAt,
			&r.nameExists, &r.nameMarked, &r.oidExists, &r.oidMarked, &r.oid, &r.curRelname,
			&r.attached, &r.pending, &r.pastCutoff, &r.dropDue); err != nil {
			return nil, err
		}
		r.day = time.Date(r.day.Year(), r.day.Month(), r.day.Day(), 0, 0, 0, 0, time.UTC)
		// Shape is unknown until inspect() checks it; the survey classifies on state and
		// attachment alone, so an unknown shape must not read as a mismatch here.
		r.boundOK, r.checkOK = true, true
		out = append(out, r)
	}
	return out, rows.Err()
}

// survey reads every non-dropped registry row joined to the catalog (day == zero: all days).
func (p *gatePass) survey(ctx context.Context, sliceEnd time.Time, day time.Time) ([]gateDayRow, error) {
	var out []gateDayRow
	filter := ""
	args := []any{p.cfg.RetentionDays, p.cfg.PurgeEvery.Seconds()}
	if !day.IsZero() {
		filter = "AND p.day = $3::date"
		args = append(args, day.UTC().Format("2006-01-02"))
	}
	err := p.read(ctx, sliceEnd, "survey", func(ctx context.Context) error {
		rows, err := p.ls.conn.Query(ctx, fmt.Sprintf(gateSurveySQL, filter), args...)
		if err != nil {
			return err
		}
		out, err = p.scanSurvey(rows)
		return err
	})
	return out, err
}

// gateVerdict is what the reconciliation table says about one registry row.
type gateVerdict int

const (
	gateVerdictRefuse       gateVerdict = iota
	gateVerdictAttach                   // created, standalone, ours → attach
	gateVerdictHold                     // attached, ours, shape ok → detach when past the cutoff
	gateVerdictDetach                   // detaching, still attached → run the DETACH
	gateVerdictFinalize                 // detaching, detach-pending → FINALIZE
	gateVerdictMarkDetached             // detaching, standalone → the state write was lost
	gateVerdictDrop                     // detached, standalone, ours → drop when due
)

// verdict applies D10's reconciliation table. Refusal is reserved for what no crash produces.
func (r gateDayRow) verdict() (gateVerdict, string) {
	if r.relname != gateRelname(r.day) || !gateLedgerPartitionNameRE.MatchString(r.relname) {
		return gateVerdictRefuse, fmt.Sprintf("registry relname %q is not the day's deterministic name", r.relname)
	}
	if r.oid == 0 {
		switch {
		case r.nameExists && !r.nameMarked:
			return gateVerdictRefuse, "a relation under our name carries no marker or another owner's"
		case !r.nameExists && r.oidExists && !r.oidMarked:
			return gateVerdictRefuse, "no relation under our name and the relation at relid is not ours"
		default:
			return gateVerdictRefuse, "relation is gone while the registry state is not dropped"
		}
	}
	switch r.state {
	case "created":
		if r.attached {
			return gateVerdictRefuse, "registered created but already attached to the ledger"
		}
		if !r.checkOK {
			return gateVerdictRefuse, "standalone relation lacks the day's bounds CHECK"
		}
		return gateVerdictAttach, ""
	case "attached":
		if !r.attached {
			return gateVerdictRefuse, "registered attached but the relation is not attached to the ledger (detached by hand)"
		}
		if r.pending {
			return gateVerdictRefuse, "detach pending without a detaching intent in the registry"
		}
		if !r.boundOK {
			return gateVerdictRefuse, "partition bounds differ from the day's"
		}
		return gateVerdictHold, ""
	case "detaching":
		if r.attached {
			if !r.boundOK {
				return gateVerdictRefuse, "partition bounds differ from the day's"
			}
			if r.pending {
				return gateVerdictFinalize, ""
			}
			return gateVerdictDetach, ""
		}
		if !r.checkOK {
			return gateVerdictRefuse, "standalone relation lacks the day's bounds CHECK"
		}
		return gateVerdictMarkDetached, ""
	case "detached":
		if r.attached {
			return gateVerdictRefuse, "registered detached but the relation is attached again (attached by hand)"
		}
		if !r.checkOK {
			return gateVerdictRefuse, "standalone relation lacks the day's bounds CHECK"
		}
		return gateVerdictDrop, ""
	}
	return gateVerdictRefuse, fmt.Sprintf("unknown registry state %q", r.state)
}

// ident is the relation's CURRENT quoted identifier for DDL — the catalog's name for the located
// oid, which differs from the registry's only after a manual rename.
func (r gateDayRow) ident() string {
	name := r.relname
	if r.curRelname != nil {
		name = *r.curRelname
	}
	return pgx.Identifier{name}.Sanitize()
}

// preamble: rebind relids by marker, read the database's UTC day, survey the registry.
func (p *gatePass) preamble(ctx context.Context, sliceEnd time.Time) ([]gateDayRow, time.Time, error) {
	// Ownership is the marker, not the OID (pg_dump/restore gives every relation a new OID, and
	// OIDs are reused after DROP): a registry row whose named relation carries our marker under
	// a different OID gets its locator refreshed here, in one bounded write.
	err := p.inTx(ctx, sliceEnd, "rebind", func(ctx context.Context, tx *gateTx) error {
		_, err := tx.exec(ctx, "rebind", `
			UPDATE service_gate_decision_partitions p SET relid = c.oid
			  FROM pg_class c
			 WHERE p.state <> 'dropped'
			   AND c.oid = to_regclass(p.relname)
			   AND p.relid <> c.oid
			   AND obj_description(c.oid, 'pg_class') = 'cerbix:gate-ledger:' || p.owner_token::text`)
		return err
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	var today time.Time
	if err := p.read(ctx, sliceEnd, "today", func(ctx context.Context) error {
		return p.ls.conn.QueryRow(ctx, `SELECT (now() AT TIME ZONE 'UTC')::date`).Scan(&today)
	}); err != nil {
		return nil, time.Time{}, err
	}
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	rows, err := p.survey(ctx, sliceEnd, time.Time{})
	if err != nil {
		return nil, time.Time{}, err
	}
	// Every unreconcilable row is refused NOW, once per pass, whichever phase would have touched
	// it — a partition detached by hand is refused whether or not it is due for anything.
	for _, r := range rows {
		if v, reason := r.verdict(); v == gateVerdictRefuse {
			p.refuse(r.day, reason)
		}
	}
	return rows, today, nil
}

// inspect re-reads ONE day right before acting on it: ownership is validated before every act,
// on the same connection, however recently the survey ran.
//
// A day with no registry row any more (found=false) is nothing to act on and nothing to refuse:
// the row is what an act is validated against.
func (p *gatePass) inspect(ctx context.Context, sliceEnd time.Time, day time.Time) (row gateDayRow, v gateVerdict, found bool, err error) {
	rows, err := p.survey(ctx, sliceEnd, day)
	if err != nil {
		return gateDayRow{}, gateVerdictRefuse, false, err
	}
	if len(rows) != 1 {
		return gateDayRow{}, gateVerdictRefuse, false, nil
	}
	row = rows[0]
	if row.oid != 0 {
		// The shape check takes ACCESS SHARE on the relation, so it runs under lock_timeout like
		// the DDL it precedes: a partition held ACCESS EXCLUSIVE by hand refuses THIS day within
		// the bound and the pass goes on with the others.
		lo, hi := gateDayBounds(day)
		if err := p.guarded(ctx, sliceEnd, "shape", func(ctx context.Context) error {
			return p.ls.conn.QueryRow(ctx, gateShapeSQL, row.oid, lo, hi, row.relname+"_day_chk").Scan(&row.boundOK, &row.checkOK)
		}); err != nil {
			return row, gateVerdictRefuse, true, err
		}
		row.shapeChecked = true
	}
	v, _ = row.verdict()
	return row, v, true, nil
}

// expect validates that a day's fresh inspection still says `want`; anything else is refused
// (counted, when the verdict is a refusal) and the act is not performed.
func (p *gatePass) expect(ctx context.Context, sliceEnd time.Time, day time.Time, want gateVerdict, act string) (gateDayRow, error) {
	row, v, found, err := p.inspect(ctx, sliceEnd, day)
	if err != nil {
		return row, err
	}
	if !found {
		return row, errGateRefused
	}
	if v == want {
		return row, nil
	}
	if v == gateVerdictRefuse {
		_, reason := row.verdict()
		p.refuse(day, act+": "+reason)
	}
	return row, errGateRefused
}

// ── Creation: standalone build, then attach under SHARE UPDATE EXCLUSIVE ─────────────────────

// creation keeps [today, today + LeadDays] attached, nearest horizon first, at most CreateMax
// days per pass. Each day is a create transaction (CREATE TABLE … LIKE + day CHECK, the local
// UNIQUE (id), the registry INSERT minting the owner token, the COMMENT marker — one commit) and
// an attach transaction (ATTACH PARTITION + state = attached — one commit). NEVER
// CREATE TABLE … as a PARTITION-OF child, which takes ACCESS EXCLUSIVE on the parent. A statement failure
// on one day moves on to the next; the day is finished by a later pass.
func (p *gatePass) creation(ctx context.Context, sliceEnd time.Time, rows []gateDayRow, today time.Time) error {
	byDay := make(map[string]gateDayRow, len(rows))
	for _, r := range rows {
		byDay[r.day.Format("2006-01-02")] = r
	}
	created := 0
	for i := 0; i <= p.cfg.LeadDays && created < p.cfg.CreateMax; i++ {
		day := today.AddDate(0, 0, i)
		row, registered := byDay[day.Format("2006-01-02")]
		if registered {
			v, _ := row.verdict()
			switch v {
			case gateVerdictHold:
				continue // attached and ours
			case gateVerdictAttach:
				if err := p.attach(ctx, sliceEnd, day); err != nil {
					if errors.Is(err, errSliceBudget) || p.dead {
						return err
					}
				}
				created++
				continue
			case gateVerdictRefuse:
				continue // refused in the preamble; this day is left alone
			default:
				// A future day in a removal state is not a crash outcome; the preamble did not
				// refuse it because its shape may be fine, so it is refused here.
				p.refuse(day, fmt.Sprintf("registry state %q inside the creation window", row.state))
				continue
			}
		}
		if err := p.create(ctx, sliceEnd, day); err != nil {
			if errors.Is(err, errSliceBudget) || p.dead {
				return err
			}
			if errors.Is(err, errGateRefused) {
				continue
			}
			created++ // the attempt spent a day of the budget
			continue
		}
		created++
		if err := p.attach(ctx, sliceEnd, day); err != nil {
			if errors.Is(err, errSliceBudget) || p.dead {
				return err
			}
		}
	}
	return nil
}

// errGateRefused marks an identity refusal raised by an act (already counted).
var errGateRefused = errors.New("store: gate maintenance refused the day")

// create builds one day STANDALONE and registers it as `created`, in one transaction.
func (p *gatePass) create(ctx context.Context, sliceEnd time.Time, day time.Time) error {
	relname := gateRelname(day)
	lo, hi := gateDayBounds(day)
	// A relation already under the day's name, with no registry row, is not ours: it is never
	// attached, never dropped, and the day is refused.
	var taken bool
	if err := p.read(ctx, sliceEnd, "create.probe", func(ctx context.Context) error {
		return p.ls.conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, relname).Scan(&taken)
	}); err != nil {
		return err
	}
	if taken {
		p.refuse(day, "a relation already exists under the day's name with no registry row")
		return errGateRefused
	}
	ident := pgx.Identifier{relname}.Sanitize()
	err := p.inTx(ctx, sliceEnd, "create", func(ctx context.Context, tx *gateTx) error {
		if _, err := tx.exec(ctx, "create", fmt.Sprintf(
			`CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES, `+
				`CONSTRAINT %s CHECK (evaluated_at >= '%s'::timestamptz AND evaluated_at < '%s'::timestamptz))`,
			ident, gateLedgerTable, pgx.Identifier{relname + "_day_chk"}.Sanitize(), lo, hi)); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, "create.index", fmt.Sprintf(
			`CREATE UNIQUE INDEX %s ON %s (id)`, pgx.Identifier{relname + "_id_uniq"}.Sanitize(), ident)); err != nil {
			return err
		}
		// The owner token is minted by the database inside this transaction and is the marker.
		var token string
		if err := tx.queryRowScan(ctx, "create.insert", `
			INSERT INTO service_gate_decision_partitions (day, relname, owner_token, relid, state, created_at)
			VALUES ($1::date, $2, gen_random_uuid(), to_regclass($2)::oid, 'created', now())
			RETURNING owner_token::text`, []any{day.Format("2006-01-02"), relname}, &token); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, "create.comment", fmt.Sprintf(
			`COMMENT ON TABLE %s IS %s`, ident, pgQuoteLiteral("cerbix:gate-ledger:"+token))); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P07" {
			// The name was taken between the probe and the CREATE: a concurrent creator that is
			// not us. Refused, never adopted.
			p.refuse(day, "a relation appeared under the day's name during creation")
			return errGateRefused
		}
		return err
	}
	p.rep.Created++
	return nil
}

// attach makes a `created` day a partition: ATTACH PARTITION (SHARE UPDATE EXCLUSIVE on the
// parent; the standalone table's day CHECK makes the attach scan-free) and state = attached in
// one transaction, after validating ownership and shape on the same connection.
func (p *gatePass) attach(ctx context.Context, sliceEnd time.Time, day time.Time) error {
	row, err := p.expect(ctx, sliceEnd, day, gateVerdictAttach, "attach")
	if err != nil {
		return err
	}
	lo, hi := gateDayBounds(day)
	err = p.inTx(ctx, sliceEnd, "attach", func(ctx context.Context, tx *gateTx) error {
		if _, err := tx.exec(ctx, "attach", fmt.Sprintf(
			`ALTER TABLE %s ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')`, gateLedgerTable, row.ident(), lo, hi)); err != nil {
			return err
		}
		tag, err := tx.exec(ctx, "attach.update", `
			UPDATE service_gate_decision_partitions
			   SET state = 'attached', attached_at = now(), relid = $2
			 WHERE day = $1::date AND state = 'created'`, day.Format("2006-01-02"), row.oid)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return p.fail("attach.update", fmt.Errorf("registry row for %s is no longer created", day.Format("2006-01-02")))
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.rep.Attached++
	return nil
}

// ── Removal: stage-ops, finalize → drops whose gates are open → detaches, oldest first ────────

// removal performs at most PurgeMaxPartitions stage-ops (FINALIZE, DROP, DETACH — one each).
// The `detaching` → standalone reconciliation is a registry write, not a stage-op.
func (p *gatePass) removal(ctx context.Context, sliceEnd time.Time, rows []gateDayRow) error {
	var finalize, mark, drop, detach []gateDayRow
	for _, r := range rows {
		v, _ := r.verdict()
		switch v {
		case gateVerdictFinalize:
			finalize = append(finalize, r)
		case gateVerdictMarkDetached:
			mark = append(mark, r)
		case gateVerdictDrop:
			if r.dropDue {
				drop = append(drop, r)
			}
		case gateVerdictDetach:
			detach = append(detach, r)
		case gateVerdictHold:
			if r.pastCutoff {
				detach = append(detach, r)
			}
		}
	}
	ops := 0
	next := func(err error) error {
		if err != nil && (errors.Is(err, errSliceBudget) || p.dead) {
			return err
		}
		return nil
	}
	for _, r := range finalize {
		if ops >= p.cfg.PurgeMaxPartitions {
			return nil
		}
		ops++
		if err := next(p.finalize(ctx, sliceEnd, r.day)); err != nil {
			return err
		}
	}
	for _, r := range mark {
		if err := next(p.markDetached(ctx, sliceEnd, r.day, false)); err != nil {
			return err
		}
	}
	for _, r := range drop {
		if ops >= p.cfg.PurgeMaxPartitions {
			return nil
		}
		did, err := p.drop(ctx, sliceEnd, r.day)
		if did {
			ops++
		}
		if err := next(err); err != nil {
			return err
		}
	}
	for _, r := range detach {
		if ops >= p.cfg.PurgeMaxPartitions {
			return nil
		}
		ops++
		if err := next(p.detach(ctx, sliceEnd, r.day)); err != nil {
			return err
		}
	}
	return nil
}

// detach is the three-step DETACH: the `detaching` intent commits first, the
// DETACH … CONCURRENTLY runs autocommit under session bounds, `detached` commits after. A row
// already `detaching` (the intent survived a crash) resumes at the statement it lost.
func (p *gatePass) detach(ctx context.Context, sliceEnd time.Time, day time.Time) error {
	row, v, found, err := p.inspect(ctx, sliceEnd, day)
	if err != nil {
		return err
	}
	if !found {
		return errGateRefused
	}
	switch v {
	case gateVerdictHold:
		if !row.pastCutoff {
			return nil
		}
		if err := p.inTx(ctx, sliceEnd, "detaching", func(ctx context.Context, tx *gateTx) error {
			tag, err := tx.exec(ctx, "detaching", `
				UPDATE service_gate_decision_partitions SET state = 'detaching'
				 WHERE day = $1::date AND state = 'attached'`, day.Format("2006-01-02"))
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return p.fail("detaching", fmt.Errorf("registry row for %s is no longer attached", day.Format("2006-01-02")))
			}
			return nil
		}); err != nil {
			return err
		}
	case gateVerdictDetach:
		// the intent is already committed
	case gateVerdictFinalize:
		return p.finalize(ctx, sliceEnd, day)
	case gateVerdictMarkDetached:
		return p.markDetached(ctx, sliceEnd, day, false)
	default:
		if v == gateVerdictRefuse {
			_, reason := row.verdict()
			p.refuse(day, "detach: "+reason)
		}
		return errGateRefused
	}
	if err := p.guarded(ctx, sliceEnd, "detach", func(ctx context.Context) error {
		_, err := p.ls.conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s CONCURRENTLY`, gateLedgerTable, row.ident()))
		return err
	}); err != nil {
		return err
	}
	return p.markDetached(ctx, sliceEnd, day, true)
}

// finalize completes a detach the previous pass left detach-pending, then writes `detached`.
func (p *gatePass) finalize(ctx context.Context, sliceEnd time.Time, day time.Time) error {
	row, err := p.expect(ctx, sliceEnd, day, gateVerdictFinalize, "finalize")
	if err != nil {
		return err
	}
	if err := p.guarded(ctx, sliceEnd, "finalize", func(ctx context.Context) error {
		_, err := p.ls.conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s FINALIZE`, gateLedgerTable, row.ident()))
		return err
	}); err != nil {
		return err
	}
	p.rep.Finalized++
	return p.markDetached(ctx, sliceEnd, day, false)
}

// markDetached writes state = detached for a `detaching` row whose relation is standalone. count
// says whether this completes a DETACH of this pass (reported as Detached) rather than a FINALIZE
// or the reconciliation of a lost state write.
func (p *gatePass) markDetached(ctx context.Context, sliceEnd time.Time, day time.Time, count bool) error {
	err := p.inTx(ctx, sliceEnd, "detached", func(ctx context.Context, tx *gateTx) error {
		tag, err := tx.exec(ctx, "detached", `
			UPDATE service_gate_decision_partitions p
			   SET state = 'detached', detached_at = now(), relid = COALESCE(to_regclass(p.relname)::oid, p.relid)
			 WHERE p.day = $1::date AND p.state = 'detaching'
			   AND NOT EXISTS (SELECT 1 FROM pg_inherits i
			                    WHERE i.inhrelid = COALESCE(to_regclass(p.relname)::oid, p.relid)
			                      AND i.inhparent = 'service_gate_decisions'::regclass)`, day.Format("2006-01-02"))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return p.fail("detached", fmt.Errorf("registry row for %s is not a detaching row with a standalone relation", day.Format("2006-01-02")))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if count {
		p.rep.Detached++
	}
	return nil
}

// drop removes a detached partition's table once detached_at <= now() − purge_every (inclusive,
// database clock) and no backend of this database holds a transaction older than the detach —
// the cached-plan hazard 15.9 fixed. DROP TABLE and state = dropped commit together. did=false
// means the gate was closed: no stage-op was spent.
func (p *gatePass) drop(ctx context.Context, sliceEnd time.Time, day time.Time) (bool, error) {
	row, err := p.expect(ctx, sliceEnd, day, gateVerdictDrop, "drop")
	if err != nil {
		return false, err
	}
	did := false
	err = p.inTx(ctx, sliceEnd, "drop", func(ctx context.Context, tx *gateTx) error {
		var open bool
		if err := tx.queryRowScan(ctx, "drop.gate", `
			SELECT p.detached_at <= now() - ($2::float8 * interval '1 second')
			   AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a
			                    WHERE a.datname = current_database()
			                      AND a.xact_start < p.detached_at
			                      AND a.pid <> pg_backend_pid())
			  FROM service_gate_decision_partitions p
			 WHERE p.day = $1::date AND p.state = 'detached'`,
			[]any{day.Format("2006-01-02"), p.cfg.PurgeEvery.Seconds()}, &open); err != nil {
			if noRows(err) {
				return errGateRefused
			}
			return err
		}
		if !open {
			return errGateDropGateClosed
		}
		did = true
		if _, err := tx.exec(ctx, "drop", fmt.Sprintf(`DROP TABLE %s`, row.ident())); err != nil {
			return err
		}
		tag, err := tx.exec(ctx, "drop.update", `
			UPDATE service_gate_decision_partitions SET state = 'dropped', dropped_at = now()
			 WHERE day = $1::date AND state = 'detached'`, day.Format("2006-01-02"))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return p.fail("drop.update", fmt.Errorf("registry row for %s is no longer detached", day.Format("2006-01-02")))
		}
		return nil
	})
	if errors.Is(err, errGateDropGateClosed) {
		return false, nil
	}
	if err != nil {
		return did, err
	}
	p.rep.Dropped++
	return true, nil
}

// errGateDropGateClosed: the drop is not due yet or an older transaction is still open.
var errGateDropGateClosed = errors.New("store: gate maintenance drop gate closed")

// ── Gauges: from the registry joined to the catalog by relid, no row counts ─────────────────

// gauges computes the four D10 gauges after the work. pg_total_relation_size takes an
// ACCESS SHARE lock, so the query runs guarded like a DDL statement.
func (p *gatePass) gauges(ctx context.Context, sliceEnd time.Time) error {
	var g GateLedgerGauges
	err := p.guardedImpl(ctx, sliceEnd, "gauges", false, func(ctx context.Context) error {
		return p.ls.conn.QueryRow(ctx, `
			WITH reg AS (
				SELECT p.state, c.oid, i.inhrelid IS NOT NULL AS attached,
				       ((p.day + 1)::timestamp AT TIME ZONE 'UTC') AS upper
				  FROM service_gate_decision_partitions p
				  LEFT JOIN pg_class c ON c.oid = p.relid
				  LEFT JOIN pg_inherits i ON i.inhrelid = c.oid AND i.inhparent = 'service_gate_decisions'::regclass
				 WHERE p.state <> 'dropped'),
			cut AS (SELECT now() - make_interval(days => $1) AS cutoff)
			SELECT (count(*) FILTER (WHERE (state = 'attached' AND attached AND upper <= cutoff)
			                            OR state IN ('detaching', 'detached')))::int,
			       COALESCE(extract(epoch FROM now() - min(upper) FILTER (WHERE state = 'attached' AND attached AND upper <= cutoff)), 0)::float8,
			       COALESCE(GREATEST(0, extract(epoch FROM max(upper) FILTER (WHERE state = 'attached' AND attached) - now())), 0)::float8,
			       COALESCE(sum(pg_total_relation_size(oid)) FILTER (WHERE oid IS NOT NULL), 0)::bigint
			  FROM reg, cut`, p.cfg.RetentionDays).
			Scan(&g.PendingDrop, &g.OldestAgeSeconds, &g.WritableHorizonSeconds, &g.Bytes)
	})
	if err != nil {
		// A sampling read that could not run — a locked partition, a spent slice, a dead
		// connection — leaves the gauges unpublished (GaugesValid stays false) and, unless the
		// connection died, is not fatal: the release proof and the next pass follow. A dead
		// connection propagates so the caller stops.
		p.rep.Failures = append(p.rep.Failures, "gauges: "+err.Error())
		if p.dead {
			return err
		}
		return nil
	}
	p.rep.Gauges, p.rep.GaugesValid = g, true
	return nil
}

// ── Release as a proof ──────────────────────────────────────────────────────────────────────

// ReleaseProved releases the gate session under its own absolute deadline — the [27 s, 30 s]
// slice of the pass timeline, never context.Background() unbounded, never the work context that
// may already be cancelled — and must OBSERVE both RESETs succeed and pg_advisory_unlock return
// a boolean (true released, false not held: both known). On any error, timeout or NULL the
// connection is hijacked from the pool and closed, never Release()d back, so neither a
// lock-owning nor a timeout-carrying connection can reach an unrelated borrower. It replaces
// Release() for the gate session only; Release() keeps its behaviour for every other caller.
func (ls *LeaderSession) ReleaseProved(deadline time.Time) error {
	return ls.releaseProved(deadline, time.Now, nil)
}

func (ls *LeaderSession) releaseProved(deadline time.Time, now func() time.Time, hooks *gateMaintenanceHooks) error {
	budget := deadline.Sub(now())
	if budget < 0 {
		budget = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	err := ls.proveRelease(ctx, hooks)
	if err == nil {
		ls.conn.Release()
		return nil
	}
	raw := ls.conn.Hijack()
	closeBudget := deadline.Sub(now())
	if closeBudget < 200*time.Millisecond {
		closeBudget = 200 * time.Millisecond
	}
	cctx, ccancel := context.WithTimeout(context.Background(), closeBudget)
	defer ccancel()
	_ = raw.Close(cctx)
	return fmt.Errorf("store: gate session release unproven, connection closed: %w", err)
}

func (ls *LeaderSession) proveRelease(ctx context.Context, hooks *gateMaintenanceHooks) error {
	if hooks != nil && hooks.beforeRelease != nil {
		if err := hooks.beforeRelease(ctx, ls.conn); err != nil {
			return err
		}
	}
	for _, q := range []string{`RESET lock_timeout`, `RESET statement_timeout`} {
		if _, err := ls.conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	unlock := `SELECT pg_advisory_unlock($1)`
	if hooks != nil && hooks.unlockSQL != "" {
		unlock = hooks.unlockSQL
	}
	var released *bool
	if err := ls.conn.QueryRow(ctx, unlock, ls.key).Scan(&released); err != nil {
		return fmt.Errorf("pg_advisory_unlock: %w", err)
	}
	if released == nil {
		return errors.New("pg_advisory_unlock returned NULL: outcome unknown")
	}
	return nil
}

// pgQuoteLiteral quotes s as a SQL string literal for the one statement that cannot take a
// parameter (COMMENT ON). The input here is a fixed prefix plus a uuid the database minted.
func pgQuoteLiteral(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}
