package domain

import "time"

// FR-032 (D-0237): the verdict a window's stored timestamps support.
//
// No verdict is ever STORED (invariant 19). Every one below is computed from the columns present,
// which is what lets a late terminal event change a window's reading with nothing to keep
// consistent — the row is not deleted and not rewritten, one more non-NULL column just answers the
// question differently (§8.6, invariant 18).

// LedgerMinCarrier is the lowest carrier generation whose dispatch carries job identity, so it is
// the lowest generation a window can be ANSWERED on.
//
// It duplicates `dispatch.ProtocolV4` by value and cannot import it: `dispatch` imports `domain`,
// so the dependency only runs one way. `TestTheLedgerMinimumCarrierMatchesTheProtocolGeneration`
// in `internal/dispatch` pins the two together, which is the whole reason a bare 4 is acceptable
// here — a constant with no pin is the "two expressions of one rule" shape this design has been
// bitten by three times.
const LedgerMinCarrier = 4

// Expected-run ledger policy bounds (FR-032 §9.3, §12.3).
//
// They live in `domain` and not in `internal/config` for one reason: the config layer validates
// them and the store layer applies them, and neither may import the other — config is the
// bootstrap boundary and the store is behind it. A number typed into both places is the shape this
// design has been bitten by three times, so the constants have one home that both already depend
// on.
//
// The retention default is the owner's ruling of 2026-09-04: all monitors participate, 14 days.
// The minimum and maximum are the enforced bounds the reviewer required at party [222]. The cap is
// a bounded VALVE rather than a policy — the retention clip is the rule, and this exists only to
// bound one statement's work when a leader has been absent for a long time.
const (
	DefaultExpectedRunRetentionDays = 14
	MinExpectedRunRetentionDays     = 2
	MaxExpectedRunRetentionDays     = 90
	DefaultExpectedRunGapWindowsMax = 10000
	MinExpectedRunGapWindowsMax     = 100
	MaxExpectedRunGapWindowsMax     = 100000
)

// ExpectedRunVerdict is the reading of one due window.
type ExpectedRunVerdict string

const (
	// VerdictCovered means a terminal outcome exists and the run answered the window within the
	// interval that spaced it. It is the ONLY verdict that may license a stroke on the FR-031
	// Response time panel (invariant 20).
	VerdictCovered ExpectedRunVerdict = "covered"
	// VerdictCoveredLate means a terminal outcome exists but the run was later than the window's
	// own interval. The owner ruled for this split on 2026-09-04: a leader absent from 10:01 to
	// 10:42 leaves the 10:01 expectation answered by a probe that observed the target at 10:42,
	// and calling that `covered` would let a coverage number count 10:01 as proven by an
	// observation forty-one minutes away. It licenses NO stroke and is excluded from the coverage
	// numerator, while still counting in the denominator (§14.1, invariant 20a).
	VerdictCoveredLate ExpectedRunVerdict = "covered_late"
	// VerdictExpectedNeverIssued means the window was expected and no run was ever issued for it.
	// A window cerbix deliberately SKIPPED reads the same way, with its `skip_reason` carried
	// beside the verdict: both are "expected, nothing ran", only the reason differs, and inventing
	// a second verdict for the deliberate case would be product semantics nobody ruled on.
	VerdictExpectedNeverIssued ExpectedRunVerdict = "expected_never_issued"
	// VerdictIssuedNeverClaimed means a job was published and no executor ever reported taking it
	// off the transport. This is the loss `internal/dispatch/amqp.go` has always accepted — a
	// delivery is acked once handed to the in-process channel — made visible for the first time
	// (§8.5).
	VerdictIssuedNeverClaimed ExpectedRunVerdict = "issued_never_claimed"
	// VerdictClaimedNeverFinished means an executor reported starting the probe and no admissible
	// outcome ever arrived.
	VerdictClaimedNeverFinished ExpectedRunVerdict = "claimed_never_finished"
	// VerdictUnknown means the ledger cannot answer for this window: it was dispatched on a
	// carrier that does not carry job identity, so no result could ever correlate to it. The
	// honest verdict, and the reason a carrier rollback costs TRUTH rather than correctness
	// (invariant 10c).
	VerdictUnknown ExpectedRunVerdict = "unknown"
	// VerdictReserved means the core minted this window's identity, made it durable, and no
	// dispatch is recorded for it (§7.4, phase F). It is NEITHER absence verdict and that is the
	// whole point: `expected_never_issued` asserts that no run happened, which is the opposite of
	// what is known here, and `issued_never_claimed` asserts that a job was PUBLISHED, which is
	// exactly what a reserved window cannot say. The state it describes is a process death or a
	// transport failure between RESERVE and CONFIRM — the honest deferred-loss record. It licenses
	// no stroke and is excluded from the coverage numerator while counting in its denominator: a
	// window that was due was due (invariants 27e, 27f).
	VerdictReserved ExpectedRunVerdict = "reserved"
)

// ExpectedRun is one stored due window. The zero value is not meaningful; every field is read from
// `expected_runs`.
type ExpectedRun struct {
	MonitorID string
	DueAt     time.Time
	// JobID is empty when no job was ever issued for this window.
	JobID string
	// CarrierGeneration is 0 exactly when JobID is empty — §6.1's CHECK enforces the biconditional
	// in the database, so the pair can never disagree here.
	CarrierGeneration int
	ExecutionRevision int64
	Region            string
	// IntervalSeconds is the interval that SPACED this window and the threshold lateness is judged
	// against. Never the monitor's current interval (invariant 13) and never a revision's base
	// value, which would misjudge a window probed under confirm acceleration (invariant 20a).
	IntervalSeconds int
	// IntervalAssumed says the threshold above was ASSUMED rather than observed: the conservative
	// timeline minimum of §14.2, used only on an orphan row. EXPLANATORY ONLY — invariant 20g
	// forbids reading it as licence to promote a `covered_late` window to `covered`.
	IntervalAssumed bool
	// ReservedAt is when the core minted this window's identity and committed it, BEFORE the job
	// was handed to any transport (§7.4). It is also the instant the job carries on the wire, so an
	// executor's echo can never introduce a value the core did not already own — invariant 20e held
	// structurally rather than by a merge expression. NULL on every row written before phase F and
	// on every orphan row, which is why its absence is never read as evidence of anything.
	ReservedAt *time.Time
	// WithheldReason says why a reserved window never became a published one. Empty otherwise.
	WithheldReason string
	IssuedAt       *time.Time
	ClaimedAt      *time.Time
	TerminalAt     *time.Time
	Outcome        string
	RefusedAt      *time.Time
	RefusedReason  string
	SkipReason     string
}

// Verdict computes the window's reading. The order of the tests is the specification:
//
//  1. A window dispatched below LedgerMinCarrier is `unknown` FIRST, before any timestamp is
//     consulted. Invariant 10c requires `unknown` and never `issued_never_claimed`, and testing
//     the timestamps first would produce the latter for exactly those windows.
//  2. A terminal outcome outranks a missing claim AND a missing issue instant (invariant 10), so
//     it is tested before either of them. With no `issued_at` there is nothing to measure lateness
//     against and the verdict is plain `covered`, which is what the discharge audit's row for
//     invariant 10 states in words: "a terminal with no claim and no issued_at must still yield
//     covered".
//  3. A RESERVED window — identity minted and committed, no dispatch recorded — is `reserved`, and
//     it is tested BEFORE the absence tests so a row the core never confirmed can never be read as
//     a published job that vanished. A CLAIM overrides it: an executor that took the job off the
//     transport proves the publish happened and only the confirmation was lost, so that row is
//     `claimed_never_finished` and not `reserved` (§7.4, invariant 27e).
//  4. Only then do absence tests run, from the outside in.
func (r ExpectedRun) Verdict() ExpectedRunVerdict {
	if r.JobID != "" && r.CarrierGeneration < LedgerMinCarrier {
		return VerdictUnknown
	}
	if r.TerminalAt != nil {
		if r.IssuedAt == nil {
			return VerdictCovered
		}
		if r.IssuedAt.Sub(r.DueAt) > time.Duration(r.IntervalSeconds)*time.Second {
			return VerdictCoveredLate
		}
		return VerdictCovered
	}
	if r.ReservedAt != nil && r.IssuedAt == nil && r.ClaimedAt == nil {
		return VerdictReserved
	}
	if r.JobID == "" {
		return VerdictExpectedNeverIssued
	}
	if r.ClaimedAt == nil {
		return VerdictIssuedNeverClaimed
	}
	return VerdictClaimedNeverFinished
}

// LicensesStroke reports whether a stroke may be drawn across this window on the FR-031 Response
// time panel. It is a method rather than a comparison at the call site so the rule has ONE owner:
// invariant 20g exists because "the lateness was only assumed, so it probably was not real" is the
// argument a future consumer will make, and it must require DELETING a rule rather than
// reinterpreting one. IntervalAssumed is deliberately not read here at all.
func (r ExpectedRun) LicensesStroke() bool { return r.Verdict() == VerdictCovered }

// CountsInCoverageNumerator mirrors LicensesStroke for the coverage fraction: `covered_late` is
// excluded from the numerator and still counted in the denominator, because a run DID happen and
// produced an admissible outcome — the statement being made is that the observation is too far
// from the window to prove it (§14.1).
func (r ExpectedRun) CountsInCoverageNumerator() bool { return r.Verdict() == VerdictCovered }
