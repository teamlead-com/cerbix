package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase B2 (D-0237): the durable expected-run ledger.
//
// The load-bearing rule of this file is that there is exactly ONE statement that moves an
// expectation forward (spec §7.1, invariant 2a). Revision 4 of the design had two — the issue path
// and the policy-skip path — and was rejected because they diverged on the obligation they shared:
// the skip one argued its way out of gap materialization, so a leader whose first action after an
// absence was a skip advanced past every intervening window while writing neither the windows nor
// the truncation fence. The advance OVERWRITES the only record of the old expectation, so evidence
// not written in the same statement can never be recovered. Callers therefore differ only in their
// arguments.

// expectedRunSkipReason values are the CHECK'd vocabulary of `expected_runs.skip_reason`. They are
// constants rather than literals at the call sites because each one names a scheduler rule, and a
// typo would be accepted by Go and refused by the database at 3am.
const (
	// SkipNoCapableRunner is advance rule 3: no region announced it can run this canary.
	SkipNoCapableRunner = "no_capable_runner"
	// SkipNoInflightSlot is advance rule 3: the canary's in-flight lease was already held.
	SkipNoInflightSlot = "no_inflight_slot"
	// SkipCredentialUnresolved is advance rule 4: the authoritative read could not resolve or
	// seal this monitor's secrets.
	SkipCredentialUnresolved = "credential_unresolved"
	// SkipNoCapableExecutor is advance rule 4: the region proved no executor able to open the
	// carrier this job needs.
	SkipNoCapableExecutor = "no_capable_executor"
	// SkipTransportBackoff is advance rule 4: the publish itself failed. **Nothing writes it since
	// phase F** (§7.4): the window is reserved BEFORE the publish is attempted, so a transport
	// refusal is a withholding on an existing row and never a skip, which would say cerbix chose
	// not to run the window it had just chosen to run. The constant and §6.1's CHECK value stay so
	// rows written before phase F still read.
	SkipTransportBackoff = "transport_backoff"
)

// ExpectationAdvance is one monitor's forward move through §7.1's primitive.
//
// JobID set means a dispatch succeeded; SkipReason set means it did not happen, and why. Exactly
// one of the two is set — Validate refuses both and neither, because a row with both would violate
// §6.1's carrier CHECK and a row with neither would record an advance nothing caused.
//
// NextDue and IntervalInForce are separate and deliberately so: for a backoff they DIFFER, and
// deriving one from the other is invariant 2b's defect — a backoff delay that became
// `interval_in_force` would misspace every later gap window, invisibly and permanently.
type ExpectationAdvance struct {
	MonitorID  string
	JobID      string
	SkipReason string
	// Confirming says the NextDue below was produced by the monitor's confirm interval rather than
	// its base one. It travels with the interval because the two describe the same instant.
	Confirming bool
	// NextDue is the new expectation. For a backoff it carries the delay; for an issue or a
	// policy skip it is the caller's `now + interval`, which is the same value the leader has
	// already written to its in-memory nextRun map. Persisting the instant the leader ALREADY
	// computes is what keeps invariant 1 true: no monitor's probe instant changes.
	NextDue time.Time
	// IntervalInForce is the interval that PRODUCED NextDue, which is §6.2's own definition of the
	// column it writes. For an ordinary advance that is the monitor's effective interval; for a
	// credential BACKOFF it is the delay, because the delay is what produced the instant (C5). It
	// used to be documented as "never a backoff delay", and the schedule then claimed a cadence
	// step across a jump the cadence did not make: the instants between were on no grid, and the
	// window at the far end carried a lateness threshold shorter than the span that spaced it.
	IntervalInForce int
	// CarrierGeneration is the carrier the PUBLISHER selected for this job, and it is meaningful
	// only alongside a JobID. It is never recovered from a payload: on AMQP it is the queue the
	// leader published to, on pull the generation stamped into the row, in-process the field the
	// core itself put on the job (§13's transport table, invariant 10d).
	CarrierGeneration int
	// ExpectedDue and ExpectedRevision are the optimistic fence (§13.2, invariant 25e). TWO
	// predicates, because fencing on `next_due_at` alone fenced the one datum §10 guarantees does
	// NOT change: a config write between publish and here passed the old check while having
	// changed the revision, the interval and possibly the region.
	ExpectedDue      time.Time
	ExpectedRevision int64
	// ReservedAt is the instant the core MINTED this job's identity — the same value the job
	// carries on the wire as `JobIssuedAt`. It is written to `reserved_at` verbatim rather than
	// re-read from the leader's clock, so the durable instant and the one an executor echoes are
	// the SAME value and not two readings of two clocks (§7.4's identity table). The old statement
	// wrote the tick's `now` here, which is a second clock and is the direction §17.12's fifth
	// finding had to be repaired in once already.
	ReservedAt time.Time
	// Region is the region the job was PUBLISHED to, an authoritative-read fact on the
	// credentialed path and the snapshot's on the plain one. Both are the publisher's decision.
	Region string
}

// Validate refuses an item the ledger cannot represent. It is called for every element rather than
// trusted, because a single bad element aborts the whole tick's statement and would take every
// other monitor's evidence down with it.
func (a ExpectationAdvance) Validate() error {
	if a.MonitorID == "" {
		return errors.New("store: expectation advance without a monitor")
	}
	if (a.JobID == "") == (a.SkipReason == "") {
		return fmt.Errorf("store: expectation advance for %s must carry exactly one of job_id and skip_reason", a.MonitorID)
	}
	if (a.JobID != "") != (a.CarrierGeneration > 0) {
		// §6.1's `expected_runs_carrier_iff_job` says the same thing in the database. Saying it
		// here too is not redundancy: the CHECK aborts a batch, this names the offending monitor.
		return fmt.Errorf("store: expectation advance for %s must carry a carrier generation exactly when it carries a job", a.MonitorID)
	}
	if a.IntervalInForce <= 0 {
		return fmt.Errorf("store: expectation advance for %s has a non-positive interval", a.MonitorID)
	}
	if a.NextDue.IsZero() || a.ExpectedDue.IsZero() {
		return fmt.Errorf("store: expectation advance for %s is missing an instant", a.MonitorID)
	}
	if a.ExpectedRevision < 1 {
		return fmt.Errorf("store: expectation advance for %s has no expected revision", a.MonitorID)
	}
	if a.Region == "" {
		return fmt.Errorf("store: expectation advance for %s has no region", a.MonitorID)
	}
	// A job with no minted instant would be reserved at NULL, which §6.1 cannot express and which
	// would put an executor's echo back in charge of the only instant the core owns (§7.4).
	if a.JobID != "" && a.ReservedAt.IsZero() {
		return fmt.Errorf("store: expectation advance for %s carries a job with no minted instant", a.MonitorID)
	}
	return nil
}

// expectedRunAdmissibleSQL is §8.1's admissibility predicate — the ONE boundary every event
// statement uses, written once and used verbatim (invariant 7a).
//
// Revision 5 had two event statements with two different guards, and the claim one was WRONG: it
// admitted `job_id IS NULL`, which is also true of a deliberately skipped window, and carried no
// revision predicate at all — so a late or stale claim could mark a window cerbix CHOSE not to run
// as claimed, breaking invariants 7 and 10b through the SQL that was supposed to uphold them
// (reviewer P1-1 at party [231]). That is the [229] lesson in a second place: two statements
// sharing an obligation diverge on it.
//
// It takes the parameter POSITIONS rather than being a bare string, because the two statements
// that use it bind their arguments in different orders — and a predicate copied to renumber it is
// a predicate that has been copied.
func expectedRunAdmissibleSQL(jobParam, revisionParam string) string {
	return `(expected_runs.job_id = ` + jobParam + `
        OR (expected_runs.job_id IS NULL                -- a window with no run,
            AND expected_runs.skip_reason IS NULL))     -- but NOT a deliberate skip
   AND expected_runs.execution_revision = ` + revisionParam
}

// expectedRunGapCTESQL is the ONE expression of "which windows were missed, and was anything older
// cut off" — the obligation §7's rejection of revision 4 is about.
//
// TWO statements materialize gap windows: §7.1's forward-moving primitive and §10's configuration
// segment close. They are necessarily different statements — one advances `next_due_at` and writes
// the window it answers, the other must touch neither — so the lesson of party [229] ("two
// statements sharing one obligation will diverge on it") is honoured by sharing the TEXT rather
// than by pretending one statement can do both. Each caller supplies its own `picked` CTE exposing
// (monitor_id, project_id, schedule_revision, monitor_region, interval_in_force, first_expected)
// and its own bound parameters; everything about the cap, the clip, the numbering and the fence is
// written once, here.
func expectedRunGapCTESQL(upperBound, capParam, floorParam string) string {
	// The two instants are CAST explicitly. Without it PostgreSQL has no type to infer them from —
	// a bare parameter in `$n - interval '1 microsecond'` resolves to `interval`, and the statement
	// fails on a function signature that names types nobody wrote. Found by running it, which is
	// the reason a bare parameter in an arithmetic position is worth naming here rather than left
	// as a habit.
	upperBound += "::timestamptz"
	floorParam += "::timestamptz"
	return `
-- Windows strictly between the old expectation and the bound, clipped at the retention floor
-- because a window older than that would be dropped unread. Numbered PER MONITOR, newest first,
-- so the cap keeps the most recent and the choice is deterministic. Revision 2 used a batch-wide
-- LIMIT and was rejected for it: one monitor's volume erased another monitor's evidence, and
-- invariant 24b exists to keep that from returning.
--
-- The series is over STEP INDICES and the instant is derived from the index, which is not a
-- rewrite for taste. Two properties come from it, and the previous timestamp form had neither:
--
--  1. Every window lands on the monitor's OWN grid, first_expected + k * interval_in_force.
--     generate_series(start, stop, step) emits start + k*step, so the moment start became
--     the retention floor rather than first_expected — which it already did whenever the clip
--     bit — the gap windows were materialized at instants no expectation ever fell on. They were
--     harmless (a never-issued window is never adopted, and its due_at is only ever compared for
--     equality against an instant the core minted) but they made due_at stop meaning "the
--     instant a run was expected", which is this ledger's entire subject.
--  2. The cap bounds the SERIES and not only the INSERT. §9.3 says it "exists to bound one
--     statement's work"; applied after row_number() it bounded the rows written while the
--     series and the window function still materialized every window back to the retention
--     floor — up to retention_days * 86400 / interval_seconds per monitor, for every monitor in
--     the batch, inside the one statement a returning leader runs first.
--
-- TWO consequences of (2) are worth stating exactly, because the obvious reading of them is
-- wrong and this design has paid for that reading before.
--
-- The series now yields AT MOST cap indices — never cap+1 — so rn <= cap below drops nothing
-- and is a no-op in every shape (measured: cap-binding integral and non-integral gaps, a
-- clip-binding gap, a gap shorter than the cap, and an empty series). It is KEPT anyway, and not
-- as decoration: it is a hard ceiling on rows written that does not depend on the index arithmetic
-- above being right, and the same CTE is used by §10's segment close, which passes its own bounds.
-- A guard whose only job is to be redundant has to say so, or the next reader deletes it as dead
-- or trusts it as load-bearing, and both are wrong.
--
-- The FENCE does not depend on that filter dropping anything, which is the part that would
-- otherwise look broken. It fires on min(due_at) > min(first_expected) — a property of where the
-- SERIES STARTS. The start is above first_expected exactly when one of the two lower bounds bit,
-- which is exactly when older windows were skipped, so the fence is neither missed when truncation
-- happened nor set when it did not.
candidate AS (
    SELECT p.monitor_id, p.project_id, p.schedule_revision, p.monitor_region,
           p.interval_in_force, p.first_expected,
           p.first_expected + make_interval(secs => p.interval_in_force::float8 * k) AS due_at,
           row_number() OVER (PARTITION BY p.monitor_id ORDER BY k DESC) AS rn
      FROM picked p
      CROSS JOIN LATERAL generate_series(
              -- The first index at or after BOTH lower bounds: the retention clip, because a
              -- window older than that would be dropped unread, and the cap expressed as an
              -- instant. The multiplication is float8 because make_interval takes one and
              -- because int4 overflows at the documented maximum — 100000 * 86400 is 8.64e9.
              GREATEST(0::bigint,
                       ceil(EXTRACT(epoch FROM (
                                GREATEST(` + floorParam + `,
                                         ` + upperBound + ` - make_interval(
                                             secs => p.interval_in_force::float8 * ` + capParam + `::float8))
                                - p.first_expected)) / p.interval_in_force)::bigint),
              -- The last index strictly before the bound. Negative when the bound has not reached
              -- the first expected window at all, which makes the series empty — the normal case
              -- on a live leader, where nothing was missed.
              floor(EXTRACT(epoch FROM (
                       ` + upperBound + ` - interval '1 microsecond' - p.first_expected))
                    / p.interval_in_force)::bigint) AS k
),
missed AS (
    INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id,
                               execution_revision, region, interval_seconds)
    -- A never-issued window has no published job, so its revision and region come from the
    -- schedule and the monitor: it is attributed to the configuration that was in force while
    -- nothing ran, not to a run that never happened.
    SELECT c.project_id, c.monitor_id, c.due_at, NULL, c.schedule_revision, c.monitor_region,
           c.interval_in_force
      FROM candidate c
     WHERE c.rn <= ` + capParam + `
    ON CONFLICT (monitor_id, due_at) DO NOTHING
),
-- ONE rule covers BOTH truncation causes: if the oldest window materialized is later than the
-- first window expected, something older was skipped — cap or clip, it does not matter which.
-- Before the fence the span is treated exactly as pre-ledger_from: not stored, claimable as
-- nothing (invariant 24a).
fence AS (
    SELECT c.monitor_id, min(c.due_at) AS truncated_before
      FROM candidate c
     WHERE c.rn <= ` + capParam + `
     GROUP BY c.monitor_id
    HAVING min(c.due_at) > min(c.first_expected)
)`
}

// reserveExpectationsSQL is §7.1's primitive, moved ahead of the dispatch by §7.4.
//
// $1 monitor_id[]  $2 job_id[]  $3 skip_reason[]  $4 next_due[]  $5 interval_in_force[]
// $6 now  $7 cap  $8 retention_floor  $9 carrier_generation[]  $10 expected_due[]
// $11 expected_revision[]  $12 region[]  $13 confirming[]  $14 reserved_at[]
var reserveExpectationsSQL = `
WITH picked AS (
    SELECT s.monitor_id, s.project_id, s.next_due_at AS due_at, s.interval_in_force,
           -- The two sources are projected under DISTINCT names on purpose: a single "region"
           -- or "execution_revision" in scope is how a "comes from the job" rule silently
           -- becomes "comes from whatever the planner resolved".
           s.execution_revision AS schedule_revision, m.region AS monitor_region,
           v.expected_revision  AS job_revision,      v.region AS job_region,
           v.job_id, v.skip_reason, v.next_due, v.new_interval, v.carrier, v.confirming,
           v.reserved_at,
           s.next_due_at + make_interval(secs => s.interval_in_force) AS first_expected
      FROM monitor_schedule s
      JOIN unnest($1::uuid[], $2::uuid[], $3::text[], $4::timestamptz[], $5::int[], $9::int[],
                  $10::timestamptz[], $11::bigint[], $12::text[], $13::boolean[], $14::timestamptz[])
             AS v(monitor_id, job_id, skip_reason, next_due, new_interval, carrier,
                  expected_due, expected_revision, region, confirming, reserved_at)
        ON v.monitor_id = s.monitor_id
      JOIN monitors m ON m.id = s.monitor_id
     -- The optimistic fence. TWO predicates, because the first one alone fenced the only datum
     -- that provably does NOT change: §10 leaves next_due_at untouched on purpose, so a config
     -- write between publish and here PASSED the old check while having changed the revision, the
     -- interval and possibly the region (reviewer P0 at party [237]).
     -- "m.execution_revision" is deliberately the column the ingest gate reads in
     -- RecordScheduledResult, so this fence and the result-rejection rule cannot drift.
     WHERE s.next_due_at = v.expected_due
       AND m.execution_revision = v.expected_revision
     ORDER BY s.monitor_id
       FOR UPDATE OF s
),
-- The window this action answers. ONE shape for both callers: issued rows carry a job and an
-- issued_at, skipped rows carry a reason and neither.
current_window AS (
    INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id, execution_revision,
                               region, reserved_at, skip_reason, carrier_generation,
                               interval_seconds)
    -- Every RUN fact comes from the PUBLISHED job: revision, region, carrier. Only the WINDOW
    -- facts come from the schedule. That is what makes a crossed generation impossible rather
    -- than merely detected (invariant 25d).
    SELECT p.project_id, p.monitor_id, p.due_at, p.job_id, p.job_revision, p.job_region,
           -- RESERVED, not issued (§7.4). This statement now runs BEFORE the dispatch, so the
           -- instant it writes is the one the identity was MINTED at — p.reserved_at, the value
           -- the job carries on the wire, and not the tick's own clock. The issued instant stays
           -- NULL until the CONFIRM says the publish returned, and no other writer may set it
           -- (invariant 27h).
           CASE WHEN p.job_id IS NOT NULL THEN p.reserved_at END,
           p.skip_reason,
           p.carrier,
           -- The interval that SPACED this window is the one in force BEFORE this advance, never
           -- new_interval: that one spaces the NEXT window (invariant 20c).
           p.interval_in_force
      FROM picked p
    -- C2: DO NOTHING, and therefore RETURNING. The gate that lets a job leave the process used to
    -- read the rows the SCHEDULE UPDATE returned, which is a different set: the update runs for
    -- every picked monitor, while this insert writes nothing when a row for that monitor and that
    -- due instant already exists. A repeated due instant -- a leader restart, or a
    -- confirm-accelerated tick landing on the same instant -- therefore passed the gate, published
    -- a job with a NEW identity, and left the window row carrying the OLD one. The result then
    -- correlates to nothing and the run is recorded nowhere: the ordering invariant broken on a
    -- path that reports success.
    --
    -- These rows are the windows THIS statement wrote, and they are what the outer SELECT returns.
    ON CONFLICT (monitor_id, due_at) DO NOTHING
    RETURNING monitor_id, due_at
),` + expectedRunGapCTESQL("$6", "$7", "$8") + `,
-- The schedule still advances for EVERY picked monitor, including one whose window was already
-- there: the instant was reserved by an earlier tick, and leaving next_due_at behind would make the
-- monitor pick the same instant again for ever. What narrows is the RETURN, not the advance.
advanced AS (
UPDATE monitor_schedule s
   -- The THREE columns that describe one instant are written by ONE statement, always. That is an
   -- amendment to §7.1 and it fixes a real over-claim the spec's own shape produced: §6.2 defines
   -- interval_in_force as "the interval that PRODUCED next_due_at", while §10 step 3 had the
   -- configuration write overwrite it with the NEW interval and deliberately leave next_due_at
   -- alone. After a 60s→300s edit the standing window was therefore stamped with a 300-second
   -- lateness threshold although it had been spaced at 60 — a run answering it four minutes late
   -- read "covered", which licenses a stroke. Every other trade in this design points the residual
   -- at WITHHOLDING; that one pointed the other way, and a direction violation is not excused by
   -- being small. With the pair written together the column's definition is true at every instant
   -- and the configuration write has nothing to recompute.
   SET next_due_at          = p.next_due,
       interval_in_force    = p.new_interval,
       -- confirm_phase joins them for the same reason: it says "next_due_at was produced by the
       -- CONFIRM interval", which is a property of the same instant and cannot be maintained by a
       -- second writer without the two disagreeing (invariant 2c).
       confirm_phase        = p.confirming,
       -- GREATEST ignores NULL arguments in PostgreSQL, returning NULL only when all are NULL:
       -- a monitor with no truncation this tick keeps the fence it had, a first truncation sets
       -- it, and a later one can only move it forward. In standard SQL the NULL would propagate
       -- and ERASE the fence — exactly the failure this statement exists to prevent — so the
       -- dependency is named rather than assumed.
       gap_truncated_before = GREATEST(s.gap_truncated_before, f.truncated_before),
       updated_at           = statement_timestamp()
  FROM picked p
  LEFT JOIN fence f ON f.monitor_id = p.monitor_id
 WHERE s.monitor_id = p.monitor_id
RETURNING s.monitor_id
)
-- The identities this statement DURABLY reserved, and the reason it must return them rather than a
-- count: a count cannot say WHICH items the fence refused, and the caller's next act is to publish
-- the jobs these rows describe. Publishing on a count is publishing on an assumption — with a
-- partial result the caller would send a job whose window was never written, which is the ordering
-- invariant broken on the one path that looks like success (reviewer P0 at party [28]).
--
-- The JOIN is the second half of that, and it is C2: an item qualifies when its WINDOW ROW was
-- written here AND its schedule moved. Reading the update alone answered a different question —
-- "did the schedule advance" — which is true for a monitor whose window belongs to another job.
SELECT w.monitor_id, w.due_at
  FROM current_window w
  JOIN advanced a ON a.monitor_id = w.monitor_id`

// ReserveExpectations moves every given monitor's expectation forward, writing the window it
// answers, the windows it skipped past and the truncation fence in the SAME statement. It returns
// THE WINDOWS IT RESERVED — not a count — because the caller's next act is to publish the jobs they
// describe, and a count cannot say which items the fence refused: a monitor whose configuration
// changed between the decision and here, or one with no schedule row (a push monitor, a disabled
// one, or one whose row a concurrent write removed).
//
// An item absent from the result was written NOWHERE — it never enters `picked`, so no window, no
// gap and no fence row exists for it — and its job must not be published. It is also definitely
// unwritten rather than unknown, which is what separates it from an ERROR: a caller that gets an
// error does not know whether the statement committed and must re-submit the same payload, while a
// caller holding a short result knows the missing items are stale and may let the monitor be
// decided again from its current configuration.
//
// It RESERVES (§7.4, phase F): it runs BEFORE the dispatch, and the window it writes carries
// `reserved_at` rather than `issued_at`. The rename is the point — this used to run after the
// publish, and when it failed the leader discarded a payload that named jobs already in flight,
// after which the next tick republished a stale identity and a later gap asserted that no run had
// happened at an instant one had. An instant is now either recorded or the schedule never passed
// it, so a caller that gets an error here has published NOTHING for the items it submitted and must
// retry them unchanged rather than dispatch past them (invariants 27, 27a, 27b, 27c).
//
// The per-monitor cap and the retention floor are read from the STORE's policy rather than passed
// in. That is deliberate: §10's segment close needs exactly the same two bounds from a different
// role's process, and a value each caller supplied for itself is a bound two callers would
// eventually disagree about — the shape of every divergence this design has been bitten by.
func (s *Store) ReserveExpectations(ctx context.Context, now time.Time, items []ExpectationAdvance) ([]ExpectationReservation, []ExpectationRejection, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	// The MIDNIGHT-ALIGNED cutoff, the same one the purge enforces and the correlation reads (C8).
	// A rolling floor here refused to materialize a missed window on the grounds that it "would be
	// dropped unread" when nothing was going to drop it for up to another day.
	gapCap, retentionFloor := s.expectedRunGapWindowsMax(), s.ExpectedRunRetentionCutoff(now)
	monitorIDs := make([]string, 0, len(items))
	jobIDs := make([]*string, 0, len(items))
	skipReasons := make([]*string, 0, len(items))
	nextDue := make([]time.Time, 0, len(items))
	intervals := make([]int32, 0, len(items))
	carriers := make([]*int32, 0, len(items))
	expectedDue := make([]time.Time, 0, len(items))
	expectedRev := make([]int64, 0, len(items))
	regions := make([]string, 0, len(items))
	confirming := make([]bool, 0, len(items))
	reservedAt := make([]*time.Time, 0, len(items))
	var rejected []ExpectationRejection
	for _, it := range items {
		// C1: per ELEMENT, which is what `Validate`'s own comment already promised. The bad item is
		// left OUT of the statement and reported; the rest of the batch proceeds. Returning here
		// made one unrepresentable item hold every ledgered dispatch, on this tick and — because
		// the held payload is re-submitted unchanged — on every tick after it.
		if err := it.Validate(); err != nil {
			rejected = append(rejected, ExpectationRejection{MonitorID: it.MonitorID, Reason: err.Error()})
			continue
		}
		monitorIDs = append(monitorIDs, it.MonitorID)
		var jobID *string
		var carrier *int32
		var reserved *time.Time
		if it.JobID != "" {
			job := it.JobID
			jobID = &job
			c := int32(it.CarrierGeneration)
			carrier = &c
			// The minted instant travels with the identity it belongs to. A skipped window has no
			// job and therefore no reservation, which is what §6.1's biconditional already says
			// about the pair beside it.
			at := it.ReservedAt
			reserved = &at
		}
		jobIDs = append(jobIDs, jobID)
		carriers = append(carriers, carrier)
		reservedAt = append(reservedAt, reserved)
		var skip *string
		if it.SkipReason != "" {
			reason := it.SkipReason
			skip = &reason
		}
		skipReasons = append(skipReasons, skip)
		nextDue = append(nextDue, it.NextDue)
		intervals = append(intervals, int32(it.IntervalInForce))
		expectedDue = append(expectedDue, it.ExpectedDue)
		expectedRev = append(expectedRev, it.ExpectedRevision)
		regions = append(regions, it.Region)
		confirming = append(confirming, it.Confirming)
	}
	// Every item was unrepresentable. There is nothing to reserve, and running the statement with
	// empty arrays would be a query that answers a question nobody asked.
	if len(monitorIDs) == 0 {
		return nil, rejected, nil
	}
	rows, err := s.pool.Query(ctx, reserveExpectationsSQL,
		monitorIDs, jobIDs, skipReasons, nextDue, intervals,
		now, gapCap, retentionFloor, carriers, expectedDue, expectedRev, regions, confirming, reservedAt)
	if err != nil {
		return nil, nil, fmt.Errorf("store: reserve expectations: %w", err)
	}
	defer rows.Close()
	out := make([]ExpectationReservation, 0, len(items))
	for rows.Next() {
		var r ExpectationReservation
		if err := rows.Scan(&r.MonitorID, &r.DueAt); err != nil {
			return nil, nil, fmt.Errorf("store: scan reservation: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		// The rows are the RESULT, so a failure part-way through reading them is a failure to know
		// what was reserved — and a caller that publishes on a partial read is the defect this
		// return type exists to remove.
		return nil, nil, fmt.Errorf("store: reserve expectations: %w", err)
	}
	return out, rejected, nil
}

// ExpectationReservation is one window this statement durably wrote: the key the caller must match
// its pending dispatch against before that dispatch may leave the process (§7.4, invariant 27).
type ExpectationReservation struct {
	MonitorID string
	DueAt     time.Time
}

// ExpectationRejection is one item the batch REFUSED before the statement ran, and why.
//
// C1. `Validate` is called for every element, and its own comment says why: "a single bad element
// aborts the whole tick's statement and would take every other monitor's evidence down with it".
// The loop returned on the first failure anyway, so a single unrepresentable item became a batch
// error — and the scheduler reads a batch error as "hold every ledgered dispatch this tick". One
// bad monitor therefore stopped all ledgered probing, and, because the held payload is re-submitted
// unchanged, it stopped it again on the next tick and every tick after.
//
// A rejected item is reported rather than merely dropped, because the caller is the only party that
// can say which monitor it was and count it: this package has no logger and no metrics.
type ExpectationRejection struct {
	MonitorID string
	Reason    string
}

// The withholding vocabulary (§7.4). A reserved window that never became a published one says WHY,
// and the set is closed for the same reason `skip_reason` is: a free-form string is a column two
// writers spell differently.
const (
	// WithheldPublishFailed: the transport refused the job the ledger had already reserved.
	WithheldPublishFailed = "publish_failed"
)

// withheldReasonAdmissible reports whether a confirm may persist this reason.
//
// The empty string is the ordinary case — a publish that returned success withholds nothing. Every
// other value must be one this vocabulary defines, and the check exists at BOTH boundaries for the
// same reason §6.1's CHECKs do: the database refuses a value whatever wrote it, and this refusal
// names the offending item instead of aborting a batch. The vocabulary was closed in the constant,
// in `openapi.yaml` and in the runbook while both boundaries admitted any string — found by the
// final-tree audit, and it is the arc's own defect class one more time.
func withheldReasonAdmissible(reason string) bool {
	return reason == "" || reason == WithheldPublishFailed
}

// ExpectationConfirm is one reserved window's outcome at the transport, submitted after the
// publish attempt (§7.4's third step).
//
// It names the window by its KEY and its identity — never by the monitor alone — because the whole
// point of the ordering is that this row and the job that was published are the same object. A
// confirm that cannot match its job matches nothing, which is the state a stale identity would have
// produced and is exactly what phase F exists to make unreachable.
type ExpectationConfirm struct {
	MonitorID string
	DueAt     time.Time
	JobID     string
	// WithheldReason is empty when the publish returned success. When it is set the window keeps
	// its `reserved` verdict and carries the reason, because a publish that failed is not a run
	// that was issued and is not a window nothing was due in.
	WithheldReason string
	// IssuedAt is the instant THIS job was published, taken at the publish and carried here.
	//
	// C7: the statement used to write the tick's own clock for every item in the batch. That clock
	// is read at the START of the tick, before the snapshot refresh, before the authoritative read,
	// before the reserve and before every publish in it — so `issued_at` was systematically EARLIER
	// than the moment the job left the process, and lateness, measured as `issued_at - due_at`, was
	// systematically SMALLER than the truth. Every other trade in this design points its residual
	// at withholding; that one pointed at over-claiming, which the design says in as many words it
	// refuses.
	//
	// It travels per ITEM rather than as one batch instant because the batch's publishes are not
	// simultaneous: a slow region's job leaves long after a fast one's, and one instant for both
	// would be wrong for at least one of them by construction. Ignored for a withheld confirm,
	// which writes no issue instant at all.
	IssuedAt time.Time
}

// confirmExpectationsSQL is §7.4's CONFIRM.
//
// $1 monitor_id[]  $2 due_at[]  $3 job_id[]  $4 withheld_reason[]  $5 issued_at[]
//
// `issued_at` is written HERE and nowhere else. The predicate carries the identity binding of
// §7.4 — a row is confirmed only if it is still the row this job reserved — and `issued_at IS NULL`
// makes a repeated confirm a no-op rather than a second instant. `monitor_schedule.last_issued_at`
// follows the same rule: it means the last instant a job was PUBLISHED, so it moves here and not in
// the reserve, where its name would have become false.
var confirmExpectationsSQL = `
WITH v AS (
    SELECT * FROM unnest($1::uuid[], $2::timestamptz[], $3::uuid[], $4::text[], $5::timestamptz[])
        AS t(monitor_id, due_at, job_id, withheld_reason, issued_at)
),
confirmed AS (
    UPDATE expected_runs e
       -- PER ITEM (C7). The instant is the one taken when THIS job was published, not the tick's
       -- start clock: the tick reads its clock before the snapshot refresh, the authoritative read,
       -- the reserve and every publish in it, so a single batch instant shrank every measured
       -- lateness -- the one direction this design says it refuses.
       SET issued_at       = CASE WHEN v.withheld_reason = '' THEN v.issued_at ELSE e.issued_at END,
           withheld_reason = v.withheld_reason
      FROM v
     WHERE e.monitor_id = v.monitor_id
       AND e.due_at     = v.due_at
       AND e.job_id     = v.job_id
       AND e.reserved_at IS NOT NULL
       AND e.issued_at IS NULL
    RETURNING e.monitor_id, v.withheld_reason AS withheld_reason, v.issued_at AS issued_at
),
-- Data-modifying CTEs run to completion whether or not the primary query reads them, so the
-- schedule moves even though the count below comes from the confirmed set.
scheduled AS (
    UPDATE monitor_schedule s
       -- The same instant, from the same source: "the last instant a job was PUBLISHED" is a claim
       -- about the publish, so re-reading the tick's clock here would restate C7's defect in the
       -- column whose name promises otherwise.
       SET last_issued_at = c.issued_at,
           updated_at     = statement_timestamp()
      FROM confirmed c
     WHERE s.monitor_id = c.monitor_id AND c.withheld_reason = ''
    RETURNING s.monitor_id
)
-- The COUNT is of windows confirmed, and it has to be: taking the row count from the statement
-- above counted SCHEDULE rows, so a withheld confirm — which deliberately does not move the
-- schedule — reported zero for work it had done, and a successful one reported a number about a
-- different table. Nothing read it, which is why it survived; a count that means something else is
-- a fact nobody can use and everybody may quote.
SELECT count(*) FROM confirmed`

// ConfirmExpectations records what the transport did with each reserved window: `issued_at` for a
// publish that returned success, a withholding reason for one that did not.
//
// A window that reaches neither — the process died in between, or this call itself failed — keeps
// its `reserved` verdict, which is the honest deferred-loss record and is neither absence verdict
// (§7.4, invariant 27e). Nothing here can invent a window: every row it touches was written by the
// reserve, and a confirm whose job no longer matches the row updates nothing.
func (s *Store) ConfirmExpectations(ctx context.Context, now time.Time, items []ExpectationConfirm) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	monitorIDs := make([]string, len(items))
	dueAt := make([]time.Time, len(items))
	jobIDs := make([]string, len(items))
	reasons := make([]string, len(items))
	issuedAt := make([]time.Time, len(items))
	for i, it := range items {
		if it.MonitorID == "" || it.JobID == "" || it.DueAt.IsZero() {
			return 0, fmt.Errorf("store: expectation confirm %d is missing its window identity", i)
		}
		if !withheldReasonAdmissible(it.WithheldReason) {
			return 0, fmt.Errorf("store: expectation confirm for %s carries an undefined withheld reason %q",
				it.MonitorID, it.WithheldReason)
		}
		monitorIDs[i], dueAt[i], jobIDs[i], reasons[i] = it.MonitorID, it.DueAt.UTC(), it.JobID, it.WithheldReason
		// A successful confirm MUST carry the instant its job was published (C7). Falling back to
		// the tick's clock would be the defect with a nil check in front of it, so it is refused
		// instead: the caller owns the publish and is the only party that can observe it.
		issuedAt[i] = it.IssuedAt.UTC()
		if it.WithheldReason == "" && it.IssuedAt.IsZero() {
			return 0, fmt.Errorf("store: expectation confirm for %s reports a publish with no instant", it.MonitorID)
		}
		if it.IssuedAt.IsZero() {
			// A withheld confirm writes no issue instant; the array still needs a value, and `now`
			// is the honest one for a column nothing reads on this path.
			issuedAt[i] = now.UTC()
		}
	}
	var confirmed int
	if err := s.pool.QueryRow(ctx, confirmExpectationsSQL,
		monitorIDs, dueAt, jobIDs, reasons, issuedAt).Scan(&confirmed); err != nil {
		return 0, fmt.Errorf("store: confirm expectations: %w", err)
	}
	return confirmed, nil
}

// DueExpectation is one monitor's standing expectation plus the identity the core mints for the
// job that will answer it.
//
// The identity comes from the DATABASE in the same statement that reads the expectation —
// `gen_random_uuid()` and `statement_timestamp()`, the shape MaterializeExecutionConfigs already
// uses — so `observed_at >= job_issued_at` compares an executor's clock against the core's rather
// than two readings of the same unknown clock, and `DueAt` is minted from
// `monitor_schedule.next_due_at` rather than from the leader's 15-second snapshot, which would be
// stale by design (invariant 25a).
type DueExpectation struct {
	MonitorID       string
	DueAt           time.Time
	IntervalInForce int
	Revision        int64
	JobID           string
	IssuedAt        time.Time
}

// LoadDueExpectations reads the standing expectation for the given monitors and mints one job
// identity per row. A monitor absent from the result has no schedule row — it does not
// participate, or a concurrent write removed it — and the caller must then publish it BELOW the
// ledger carrier, because a generation-4 job is DEFINED by carrying `DueAt` and one without it is a
// protocol violation rather than a rolling-upgrade case (invariant 10i).
func (s *Store) LoadDueExpectations(ctx context.Context, monitorIDs []string) (map[string]DueExpectation, error) {
	if len(monitorIDs) == 0 {
		return map[string]DueExpectation{}, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT s.monitor_id, s.next_due_at, s.interval_in_force, s.execution_revision,
		        gen_random_uuid()::text, statement_timestamp()
		   FROM monitor_schedule s
		  WHERE s.monitor_id = ANY($1::uuid[])`, monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("store: load due expectations: %w", err)
	}
	defer rows.Close()
	out := make(map[string]DueExpectation, len(monitorIDs))
	for rows.Next() {
		var d DueExpectation
		if err := rows.Scan(&d.MonitorID, &d.DueAt, &d.IntervalInForce, &d.Revision, &d.JobID, &d.IssuedAt); err != nil {
			return nil, fmt.Errorf("store: scan due expectation: %w", err)
		}
		out[d.MonitorID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate due expectations: %w", err)
	}
	return out, nil
}

// Advance rule 5 — confirm acceleration — has NO statement in this file, and that is a change
// from §7.3 worth stating rather than leaving as an absence.
//
// §7.3 gave rule 5 its own write: set `interval_in_force = ConfirmInterval()` and
// `confirm_phase = true`, then restore both when the acceleration expires. That write happens
// BEFORE any advance has produced a `next_due_at` from the confirm interval, so it left the column
// describing an instant it had not produced — the same mis-description §10's step 3 produced from
// the other direction. Both are gone: the advance writes all three columns together, so the
// accelerated spacing is recorded by the statement that actually applies it.
//
// What rule 5 still does is pull the leader's in-memory `nextRun` earlier, and only when the
// standing expectation is LATER than the accelerated instant — so an expectation already in the
// past is never moved, no intervening window can be skipped, and there is nothing to materialize
// or fence. Invariant 2d asked for a statement structurally incapable of moving `next_due_at`
// forward; a rule with no statement is incapable of it in the strongest available sense.

// expectedRunPartitionPrefix names the daily range partitions of expected_runs
// (expected_runs_pYYYYMMDD, UTC-aligned), mirroring heartbeatPartitionPrefix.
const expectedRunPartitionPrefix = "expected_runs_p"

// expectedRunFillfactor is the storage parameter every partition is created with.
//
// It is on the PARTITION and never only on the parent, and that is a defect the reviewer found in
// the spec's own DDL: in PostgreSQL storage parameters are per-relation and
// `CREATE TABLE ... PARTITION OF` does NOT inherit them, so a `WITH (fillfactor = 70)` on the
// partitioned parent would have applied to nothing that ever holds a row. Invariant 23a therefore
// asserts it by reading `pg_class.reloptions` on a NEWLY created partition — parent DDL proves
// nothing.
//
// 70 leaves ~18 tuples' worth of free space per 8 KB page at ~136 bytes per tuple. That is NOT a
// proof on its own and the spec says so: each row takes up to two updates, so a page whose rows
// all update twice would want ~84 new versions against ~18 slots. What closes the gap is HOT
// pruning, which reclaims a dead version in a HOT chain on page access without vacuum — so the
// requirement is headroom for the versions live at ONE moment, which depends on arrival spread and
// cannot be computed from the schema. Hence the measured gauge of phase D rather than an assertion
// here.
const expectedRunFillfactor = 70

// EnsureExpectedRunPartitions creates daily partitions for the UTC days in [today, today+ahead].
//
// `expected_runs` is declaratively partitioned in BOTH storage modes: TimescaleDB never owns it.
// The reason is not preference — a hypertable creates chunks on demand and would make the
// `fillfactor` question and the retention path two different mechanisms depending on which image
// the operator installed, and this ledger's whole subject is being able to say what a span means.
//
// Best-effort, exactly as the heartbeat twin is: a day whose rows already sit in the DEFAULT
// partition cannot get a dated one, and that day stays in the default until retention purges it.
// The default is what makes that safe rather than lossy, and it is the direction this ledger must
// fail in — an insert lost to a missing partition would erase the very fact the ledger exists to
// keep (invariant 16a).
func (s *Store) EnsureExpectedRunPartitions(ctx context.Context, ahead int) error {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var errs []error
	for i := 0; i <= ahead; i++ {
		day := today.AddDate(0, 0, i)
		name := expectedRunPartitionPrefix + day.Format("20060102")
		q := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF expected_runs
			   FOR VALUES FROM ('%s') TO ('%s') WITH (fillfactor = %d)`,
			name, day.Format(pgTimestamp), day.AddDate(0, 0, 1).Format(pgTimestamp),
			expectedRunFillfactor)
		if _, err := s.pool.Exec(ctx, q); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// expectedRunTerminal is one admissible outcome for one window.
//
// Every field is either minted by the CORE and copied back verbatim by the executor (DueAt, JobID,
// IssuedAt) or read from the monitor's own row. NOTHING here is an executor's invention, and the
// carrier is the one field where that took a correction — see the note above
// `fillExpectedRunTerminalSQL`.
type expectedRunTerminal struct {
	MonitorID string
	DueAt     time.Time
	JobID     string
	Revision  int64
	// TerminalAt is the instant the outcome became known to the core. It is the ingest
	// transaction's own clock rather than the observation timestamp: `terminal_at` answers "when
	// did an admissible outcome exist", and the observation instant is already recorded on the
	// heartbeat.
	TerminalAt time.Time
	IssuedAt   time.Time
	// Outcome is 'result' or 'probe_error', the two members §6.1's CHECK admits.
	Outcome string
}

// The carrier a correlatable result proves is `domain.LedgerMinCarrier`, written at the call site
// below rather than wrapped in a function of its own (G6). The wrapper existed only to give this
// note somewhere to live, and a function whose body is a constant is a name a reader has to follow
// to learn nothing. The note is what mattered, so it stays; the indirection does not.
//
// The source of that carrier is a finding rather than a transcription.
//
// §8.3 names `dispatch.DeliveredJob.CarrierGeneration` — the transport adapter's observation of
// the queue or claimed row a job arrived on. That value exists in the EXECUTOR's process. The
// result travels back as a `domain.Heartbeat`, so carrying the adapter's observation to the core
// would mean putting it in the payload, which is precisely the source the design forbids and the
// P0 that killed revision 6.
//
// The implementable source is the PUBLISHER's own rule, which needs no wire field at all: `DueAt`
// reaches an executor only on a generation-4 dispatch, because the leader caps a monitor with no
// standing expectation below the ledger carrier for exactly that reason. So a result that
// correlates AT ALL — one carrying a parseable `JobID` and a `DueAt` that passes §13.1's
// validation — proves its job rode generation 4. The payload's own `ProtocolVersion` is never
// read, which is what invariant 10d actually asks for.

// fillExpectedRunTerminalSQL is §8.3's upsert.
//
// $1 monitor  $2 due_at  $3 job_id  $4 revision  $5 carrier  $6 issued_at  $7 terminal_at
// $8 outcome
var fillExpectedRunTerminalSQL = `
INSERT INTO expected_runs (project_id, monitor_id, due_at, job_id, execution_revision,
                           region, carrier_generation, interval_seconds, interval_assumed,
                           issued_at, terminal_at, outcome)
SELECT m.project_id, m.id, $2, $3, $4, m.region, $5,
       -- The CONSERVATIVE threshold of §14.2. The core does not know which of the revision's two
       -- legitimate intervals spaced an orphan window, so it takes the TIGHTEST: an unrecorded
       -- window can then read covered_late when the truth might have been covered, which is an
       -- error toward WITHHOLDING and the only direction this design accepts.
       --
       -- NULLIF(confirm, 0) is load-bearing and is a defect found in implementation: the spec
       -- writes the threshold as MIN(interval_seconds, confirm_interval_seconds), and a monitor
       -- with confirm acceleration DISABLED carries confirm_interval_seconds = 0. A literal
       -- minimum would be 0, and a 0-second threshold makes every orphan window covered_late no
       -- matter how promptly it was answered — over-withholding so total it destroys the verdict.
       -- LEAST ignores NULL arguments in PostgreSQL, so nulling the disabled value takes the
       -- minimum over the LEGITIMATE intervals, which is what "its two legitimate values" meant.
       COALESCE(LEAST(r.interval_seconds, NULLIF(r.confirm_interval_seconds, 0)),
                LEAST(m.interval_seconds, NULLIF(m.confirm_interval_seconds, 0))),
       true, $6, $7, $8
  FROM monitors m
  -- The revision timeline is what makes execution_revision mean anything historically (phase A).
  -- A revision with no row falls back to the monitor's CURRENT fields, which is the only value
  -- available and is why the timeline's limitation is stated rather than discovered: revisions
  -- that predate migration 00100 cannot be reconstructed.
  LEFT JOIN monitor_execution_revisions r
         ON r.monitor_id = m.id AND r.project_id = m.project_id AND r.execution_revision = $4
 -- The ORPHAN path is the one write with no schedule row to consult, so it is the one write that
 -- must apply the participation rule itself — through the SAME expression the schedule sync uses,
 -- not a second copy of it. Without this a crafted result carrying a job id and a due instant
 -- would create a window for a PUSH monitor, which §15 excludes: "checkStalePush" already detects
 -- and records a push that did not arrive, and a window beside it is a second mechanism for one
 -- obligation (invariant 20b).
 WHERE m.id = $1 AND ` + scheduleParticipantSQL + `
   -- A window ANSWERED EARLY may be filled but never INVENTED, and that asymmetry is what lets
   -- correlation accept due_at > issued_at at all (§13.1).
   --
   -- The core dispatches from the leader's in-memory nextRun, which is legitimately EARLIER than
   -- the persisted next_due_at in two ordinary states: on leadership acquisition, where the map
   -- is empty and every monitor is due at once while the standing expectation may still be in the
   -- future; and under confirm acceleration, which pulls nextRun in and — by §7.3 — writes
   -- nothing to the schedule. Both mint a job whose DueAt postdates its own IssuedAt, and
   -- refusing correlation for them made every such run read issued_never_claimed forever
   -- although the probe ran and its heartbeat landed. That is invariant 25's "never produces a
   -- false issued_never_claimed" broken by the core's own mint rather than by a bad result.
   --
   -- What the old refusal actually protected is the ORPHAN insert: an executor-supplied instant in
   -- the future would otherwise create a window for time nothing has been dispatched for. So the
   -- protection moves here, where it belongs — an early answer may only land on a row that already
   -- exists, and the row can only exist because the core wrote it.
   --
   -- Both instants are CAST. In the outer SELECT the planner takes $2's type from the INSERT's
   -- target column; inside this subquery it has no such context and resolved it to text, so the
   -- statement failed with "operator does not exist: timestamp with time zone = text". Found by
   -- running it, which is the second time in this file a bare parameter in a comparison has cost
   -- a run.
   AND ($2::timestamptz <= $6::timestamptz
        OR EXISTS (SELECT 1 FROM expected_runs e
                    WHERE e.monitor_id = $1 AND e.due_at = $2::timestamptz))
ON CONFLICT (monitor_id, due_at) DO UPDATE
   -- COALESCE(existing, new) is first-writer-wins, for the two facts that cannot arrive twice
   -- with different values. It is what makes ADOPTION fill the carrier on a no-job window while
   -- a row that already has one keeps it.
   SET job_id             = COALESCE(expected_runs.job_id, $3),
       carrier_generation = COALESCE(expected_runs.carrier_generation, $5),
       -- issued_at joins them, and it did NOT: it was LEAST(COALESCE(existing, $6), $6), which
       -- takes the MINIMUM of the core's own instant and the executor's echo of it. That is the
       -- one merge in this statement whose argument is not server-owned, and it moves the verdict
       -- in the direction this design refuses -- a smaller issued_at is a smaller
       -- issued_at - due_at, so a covered_late window is promoted to covered and licenses a
       -- stroke. Invariant 20e says no executor-supplied value may do that; it guarded the
       -- THRESHOLD, which is safe, and not the MEASUREMENT, which was not.
       --
       -- COALESCE is first-writer-wins and the first writer is §7.1's advance, so a window the
       -- core recorded keeps the core's instant. The ORPHAN path is unaffected and is the reason
       -- $6 appears at all: there the core never wrote one, and §8.3 states that trust boundary.
       -- 27h. On a RESERVED row the issued instant belongs to the CONFIRM and to nothing else,
       -- so the fill leaves it alone: an early terminal on a reserved window keeps it NULL and
       -- reads covered by rule 2, which is honest — the run happened and completed. Restoring the
       -- plain COALESCE here would hand the instant back to the executor's echo, which is the
       -- shape §17.12's fifth finding already had to remove once. The ORPHAN path is unaffected:
       -- a row the core never reserved has no reservation, and there $6 is the only instant
       -- there is.
       issued_at          = CASE WHEN expected_runs.reserved_at IS NOT NULL
                                 THEN expected_runs.issued_at
                                 ELSE COALESCE(expected_runs.issued_at, $6) END,
       -- The attribute is selected by a CASE over the OLD timestamp and the timestamp is
       -- minimised, so the pair always describes the SAME delivery whichever order two arrive in
       -- (invariant 7c). Both SET expressions see the pre-update row in PostgreSQL, so the CASE
       -- reads the old timestamp regardless of clause order — a property worth naming, because a
       -- reader who assumed sequential assignment would "fix" it into a bug. Reverting either to
       -- COALESCE(existing, new) is the shape this statement shipped with through revision 10 and
       -- is the mutation the reversed-arrival test must kill.
       outcome            = CASE WHEN expected_runs.terminal_at IS NULL
                                      OR $7 < expected_runs.terminal_at THEN $8
                                 ELSE expected_runs.outcome END,
       terminal_at        = LEAST(COALESCE(expected_runs.terminal_at, $7), $7)
 -- §8.1's ONE admissibility predicate: the same run, or a window with no run AND no skip_reason,
 -- and the same generation. The skip clause is what stops a window cerbix CHOSE not to run from
 -- being reported as one that happened (invariant 10b), and the revision equality is what stops a
 -- result admissible for the monitor's CURRENT generation from filling a window materialized under
 -- a different one.
 WHERE ` + expectedRunAdmissibleSQL("$3", "$4")

// fillExpectedRunTerminalTx records an admissible outcome, creating the window's row if the ledger
// never entered the run (§7's crash-after-publish). It runs in the SAME transaction as the
// heartbeat insert and BEHIND THE SAME GATE — a refused result must fill nothing, and making the
// statement unreachable for one is stronger than remembering a condition (invariant 10a).
//
// Zero rows affected is a NORMAL outcome and never an error: a conflict on a row that fails the
// admissibility predicate means this result correlates to no window.
func fillExpectedRunTerminalTx(ctx context.Context, tx pgx.Tx, in expectedRunTerminal) error {
	// A revision below 1 names no generation, and this was the ONE ledger write without the guard
	// its two siblings already have (`noteExpectedRunRefusalTx` and `RecordRunClaim`). It is
	// reachable: with `result.revision_mode: observe` a result carrying `execution_revision: 0`
	// passes the ingest gate, and the statement below would then INSERT an orphan window stamped
	// `execution_revision = 0` — a generation no timeline row can ever describe, reading `covered`.
	// The admissibility predicate could not have caught it either, since a real row's revision is
	// never 0 and the conflict branch would simply match nothing.
	if in.Revision < 1 {
		return nil
	}
	// The column list names carrier_generation and interval_seconds explicitly, and both are
	// there because they were once only in the prose: revision 7 left the carrier out (reviewer
	// P0-1 at party [235]) and revision 15 did the same to the interval. §6.1's CHECK requires
	// the carrier non-NULL whenever job_id is, so an orphan insert omitting it FAILS the
	// constraint — the reconciliation path would have errored on exactly the case it exists to
	// serve (invariant 25c).
	_, err := tx.Exec(ctx, fillExpectedRunTerminalSQL,
		in.MonitorID, in.DueAt, in.JobID, in.Revision, domain.LedgerMinCarrier,
		in.IssuedAt, in.TerminalAt, in.Outcome)
	if err != nil {
		return fmt.Errorf("store: fill expected run terminal: %w", err)
	}
	return nil
}

// noteExpectedRunRefusalTx records that a result arrived and was inadmissible — a different fact
// from silence, and one nothing in cerbix recorded before.
//
// It is an UPDATE and NEVER an insert, and the asymmetry with the terminal is deliberate. A
// terminal proves both that the run happened and that it produced an admissible outcome, so it may
// create its own row. A refusal proves only that something was DELIVERED: creating a row from it
// would invent an issued run whose sole evidence is inadmissible, and it would occupy the window's
// primary key against a legitimate re-dispatch — the second horn of the trilemma at party [239].
//
// The guard is `job_id = $4` ALONE, without §8.1's no-job disjunct: a refusal must never adopt a
// window, because adoption asserts a run happened there (invariant 10f). Zero rows affected is the
// normal outcome for a refusal whose job the ledger never entered.
func noteExpectedRunRefusalTx(ctx context.Context, tx pgx.Tx, monitorID string, dueAt time.Time, jobID string, revision int64, at time.Time, reason string) error {
	// A revision below 1 is the missing-revision case, which invariant 25 lists among the shapes
	// that correlate to NO window: with nothing to compare, the guard's `execution_revision = $4`
	// could only ever match by accident.
	if jobID == "" || dueAt.IsZero() || reason == "" || revision < 1 {
		// A result carrying no correlation cannot annotate a window. It is still recorded
		// wherever refused results are already counted — cerbix_result_ignored_total and its
		// siblings — and the ledger adds nothing by duplicating that.
		return nil
	}
	_, err := tx.Exec(ctx, `
UPDATE expected_runs
   -- One event, two columns, ONE comparison (invariant 7c). Revision 10 took
   -- refused_at = LEAST(...) beside refused_reason = COALESCE(...), so a later refusal arriving
   -- first followed by an earlier one left the EARLIER timestamp beside the LATER reason and the
   -- pair stopped describing one event.
   SET refused_reason = CASE WHEN refused_at IS NULL OR $5 < refused_at THEN $6
                             ELSE refused_reason END,
       refused_at     = LEAST(COALESCE(refused_at, $5), $5)
 WHERE monitor_id = $1 AND due_at = $2
   AND job_id = $3
   AND execution_revision = $4`,
		monitorID, dueAt, jobID, revision, at, reason)
	if err != nil {
		return fmt.Errorf("store: note expected run refusal: %w", err)
	}
	return nil
}

// expectedRunRef is the correlation a result carries: which window it answers.
//
// The zero value means NO correlation, and that is a first-class outcome rather than an error. A
// result that carries no usable identity is still recorded as a heartbeat exactly as today and
// contributes nothing to the ledger (invariant 25). It is never coerced, never defaulted and never
// silently dropped.
type expectedRunRef struct {
	DueAt    time.Time
	JobID    string
	IssuedAt time.Time
	OK       bool
}

// validJobUUID reports whether a wire `job_id` is a UUID, without coercing it.
//
// The wire type stays `string` because `dispatch.CheckJob.JobID` is JSON on a queue that may hold
// messages published by the PREVIOUS version across a rolling upgrade, while the column is `uuid`
// — it saves ~42 bytes per window across the tuple and the partial index and the value is already
// a UUID, since `gen_random_uuid()::text` merely stringifies it. So the boundary parses and
// validates rather than casting: a `job_id` that is absent or not a UUID means this result
// correlates to no window (§13).
//
// Hand-written rather than importing a UUID package: this is the whole of what the boundary needs,
// and it refuses the shapes a permissive parser accepts — braces, a urn prefix, or the 32-hex
// form without dashes — none of which any producer here emits.
func validJobUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// correlateExpectedRun applies §13.1's validation table to one result.
//
// Every failure below refuses CORRELATION and nothing else — the heartbeat still lands, and the
// window simply stays unanswered, which is the withholding direction this design accepts
// everywhere. A violation is never treated as `due_at = epoch`, because an invented window would
// either fabricate coverage or collide with a real expectation.
func (s *Store) correlateExpectedRun(hb domain.Heartbeat, dbNow time.Time) expectedRunRef {
	if !validJobUUID(hb.JobID) || hb.DueAt.IsZero() || hb.JobIssuedAt.IsZero() {
		return expectedRunRef{}
	}
	// "An expectation cannot POSTDATE its own dispatch" WAS the rule here, and it was wrong about
	// this system rather than merely strict. The leader dispatches from its in-memory `nextRun`,
	// not from `monitor_schedule.next_due_at`, and two ordinary states put the first EARLIER than
	// the second: leadership acquisition, where the map is created empty so every monitor is due
	// at once while its standing expectation may still be in the future; and confirm acceleration,
	// which pulls `nextRun` in and — by §7.3, deliberately — writes nothing to the schedule.
	//
	// Both mint a job whose `DueAt` postdates its own `IssuedAt`, because the core minted BOTH from
	// the same statement. Refusing them meant the window the core had just written could never be
	// answered by the run it was written for: it read `issued_never_claimed` forever, although the
	// probe ran and its heartbeat landed. Measured on a running instance, every window written by a
	// leader restart was in that state. Invariant 25 requires that a result "never produces a false
	// `issued_never_claimed`", and this refusal was producing them at every failover and every
	// confirm-accelerated probe.
	//
	// Answering a window EARLY is not an anomaly the ledger has to defend against — `Verdict()`
	// already measures lateness as `issued_at - due_at` and reads a negative one as `covered`. What
	// the refusal really protected is the ORPHAN insert, and that protection now lives in
	// `fillExpectedRunTerminalSQL`, which will adopt an early window but never invent one.
	//
	// Outside the retention window there is no row to answer and no partition to hold one: a
	// window that far back was dropped unread, and writing it now would make a span the ledger
	// cannot bound look answered. That bound stays, and it is the only one left, because a future
	// `due_at` can now only ever match a row the core itself wrote.
	//
	// It reads the ALIGNED cutoff, which is C8: this used to be a rolling `now - retention` while
	// the purge dropped partitions on a midnight-aligned one, so up to a day of windows were
	// stored, listed, and refused here. A result was rejected as "outside retention" for a row the
	// operator could see on the screen beside it.
	if hb.DueAt.Before(s.ExpectedRunRetentionCutoff(dbNow)) {
		return expectedRunRef{}
	}
	return expectedRunRef{DueAt: hb.DueAt, JobID: hb.JobID, IssuedAt: hb.JobIssuedAt, OK: true}
}

// Expected-run terminal outcomes — the two members §6.1's CHECK admits. Constants because a typo
// in a literal is accepted by Go and refused by the database, at whatever hour the first
// probe_error of the week arrives.
const (
	ExpectedRunOutcomeResult     = "result"
	ExpectedRunOutcomeProbeError = "probe_error"
)

// refuseWithLedger records that a result arrived and was inadmissible, then commits the outcome.
//
// It exists so the refusal has ONE call shape across the five rejecting paths of
// RecordScheduledResult. Five sites each writing their own `noteExpectedRunRefusalTx` call is the
// [231] P1-1 defect in miniature: two statements sharing an obligation diverge on it, and five
// would diverge faster.
//
// A result that correlates to no window refuses silently and correctly. Such a refusal is already
// counted where discarded results are counted — cerbix_result_ignored_total,
// cerbix_result_missing_revision_total, cerbix_result_clock_skew_total,
// cerbix_result_observed_before_issue_total — and the ledger adds nothing by duplicating that.
func (s *Store) refuseWithLedger(ctx context.Context, tx pgx.Tx, hb domain.Heartbeat, ref expectedRunRef, at time.Time, out ResultOutcome) (ResultOutcome, error) {
	if ref.OK {
		if err := noteExpectedRunRefusalTx(ctx, tx, hb.MonitorID, ref.DueAt, ref.JobID,
			hb.ExecutionRevision, at, out.Reason); err != nil {
			return ResultOutcome{}, err
		}
	}
	return s.commitOutcome(ctx, tx, out)
}

// recordExpectedRunClaimSQL is §8.1's claim merge: the admissibility predicate plus ONE column.
//
// $1 monitor  $2 due_at  $3 job_id  $4 revision  $5 claimed_at
//
// `LEAST(COALESCE(claimed_at, $5), $5)` is the whole of it: idempotent, commutative, and monotone
// downward, so replays and reorderings converge on the same value. "Earliest wins" rather than
// "first writer wins" because the first CLAIM is the real one — a duplicate delivery arriving later
// must not redate it.
//
// It is an UPDATE and never an insert, and the reasoning is the refusal statement's rather than the
// terminal's: a claim proves that an executor took a job off the transport, not that a run produced
// an admissible outcome. Creating a row from one would invent an issued run whose only evidence is
// that somebody started it, and it would occupy the window's primary key against the legitimate
// materialization.
//
// Zero rows affected is therefore a legal outcome, and RESTATED here against a re-measurement
// (audit-gap package 3, item C4) because what stood in its place described an ordering phase F
// inverted.
//
// The old text: §7.1 flushes the advance once per tick AFTER every publish in it, so a fast
// transport's claim routinely reaches the core before the window exists — "one claim landed out of
// sixty-two windows". Phase F (§7.4) reserves BEFORE the publish, so the window is committed before
// the job leaves the process and a claim cannot outrun it. Re-measured on a `role=all` dev instance
// over a full day: 20050 of 20052 generation-4 issued windows carry a `claimed_at`, and the two
// that do not were never reserved — they are §8.3 adoptions, with no row for any claim to have
// matched.
//
// What zero rows means now is one of the shapes invariant 25 already names: an adoption, a job id
// or revision this window does not carry, or a window past the correlation floor. The caller is
// still told, and still retries within its bound; see `internal/ingest`, where the same
// re-measurement is written down beside the number it no longer justifies.
var recordExpectedRunClaimSQL = `
UPDATE expected_runs
   SET claimed_at = LEAST(COALESCE(claimed_at, $5), $5)
 WHERE monitor_id = $1 AND due_at = $2
   AND ` + expectedRunAdmissibleSQL("$3", "$4")

// RecordRunClaim fills `claimed_at` for the window this claim answers, and reports whether a window
// took it.
//
// It is NOT in the heartbeat transaction and needs no transaction of its own: it is one idempotent
// statement over one row, and it shares nothing with any other write. That is the difference
// between a claim and a terminal — the terminal must land with the heartbeat it accompanies or
// neither, while a claim accompanies nothing.
//
// `matched` false means "no window took this claim", and it names no reason. Under phase F the
// window is committed before its job is published, so "not committed yet" is not among them; what
// remains is a claim that correlates to nothing at all (invariant 25) and one whose job id or
// revision the window does not carry. The caller's response is the same for all of them and is
// bounded — a few re-offers, then let it go — and none of them is a state a re-offer changes. An
// error is reserved for a claim this process could not evaluate.
func (s *Store) RecordRunClaim(ctx context.Context, hb domain.Heartbeat) (matched bool, err error) {
	if hb.Claim == nil || hb.Claim.At.IsZero() {
		return false, errors.New("store: run claim carries no instant")
	}
	if hb.MonitorID == "" || hb.ExecutionRevision < 1 {
		return false, errors.New("store: invalid run claim")
	}
	var dbNow time.Time
	if err := s.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&dbNow); err != nil {
		return false, fmt.Errorf("store: run claim clock: %w", err)
	}
	// The SAME correlation validation every other event uses, so a claim carrying a bad job id or
	// a window outside retention is refused exactly as a result would be (invariant 25). A claim
	// is the one event with an executor-supplied INSTANT, and that instant is deliberately NOT
	// validated against the core's clock: `claimed_at` decides no verdict, so a skewed one costs a
	// diagnostic's precision rather than a coverage claim, and rejecting it would lose the only
	// evidence that the run started at all.
	ref := s.correlateExpectedRun(hb, dbNow)
	if !ref.OK {
		return false, nil
	}
	ct, err := s.pool.Exec(ctx, recordExpectedRunClaimSQL,
		hb.MonitorID, ref.DueAt, ref.JobID, hb.ExecutionRevision, hb.Claim.At)
	if err != nil {
		return false, fmt.Errorf("store: record run claim: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}
