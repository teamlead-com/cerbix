package domain

import (
	"testing"
	"time"
)

// FR-032 — the verdict table, which is the whole of what a surface may claim about a due window.
//
// Four of these invariants are the ones the discharge audit found had NOTHING testing them: 1, 4,
// 5 and 6 — the properties that say what FR-032 is FOR. Coverage had tracked the design's DEFECTS
// rather than the requirement's purpose, because every earlier test was written in answer to a
// rejection.

func at(t time.Time) *time.Time { return &t }

// Invariants 4, 5, 6 and 10 — the three states this requirement exists to distinguish, plus the
// rule that a terminal outranks the absence of everything else.
//
// A window is `covered` ONLY if a terminal outcome exists, and a terminal is never inferred from a
// heartbeat's presence at a nearby instant: the verdict reads timestamps on the window's own row
// and has no access to heartbeats at all, which is what makes invariant 4 structural.
func TestTheThreeStatesThisRequirementExistsToDistinguish(t *testing.T) {
	due := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	base := ExpectedRun{
		MonitorID: "m", DueAt: due, IntervalSeconds: 60,
		JobID: "11111111-1111-4111-8111-111111111111", CarrierGeneration: LedgerMinCarrier,
	}
	for _, tc := range []struct {
		name string
		row  ExpectedRun
		want ExpectedRunVerdict
	}{
		{
			name: "a window nothing was ever issued for",
			row:  ExpectedRun{MonitorID: "m", DueAt: due, IntervalSeconds: 60},
			want: VerdictExpectedNeverIssued,
		},
		{
			name: "a window cerbix deliberately skipped",
			row:  ExpectedRun{MonitorID: "m", DueAt: due, IntervalSeconds: 60, SkipReason: "no_capable_runner"},
			want: VerdictExpectedNeverIssued,
		},
		{
			name: "a run issued and never claimed",
			row:  withIssued(base, due),
			want: VerdictIssuedNeverClaimed,
		},
		{
			name: "a run claimed and never finished",
			row:  withClaimed(withIssued(base, due), due.Add(time.Second)),
			want: VerdictClaimedNeverFinished,
		},
		{
			name: "a run that finished",
			row:  withTerminal(withClaimed(withIssued(base, due), due.Add(time.Second)), due.Add(2*time.Second)),
			want: VerdictCovered,
		},
		{
			name: "a terminal with no claim — a terminal outranks a missing claim",
			row:  withTerminal(withIssued(base, due), due.Add(2*time.Second)),
			want: VerdictCovered,
		},
		{
			name: "a terminal with no claim AND no issue instant",
			row:  withTerminal(base, due.Add(2*time.Second)),
			want: VerdictCovered,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.Verdict(); got != tc.want {
				t.Fatalf("reads %q, want %q", got, tc.want)
			}
		})
	}
	// And the three unfinished states are pairwise DISTINCT, which is the requirement's entire
	// subject. Asserted as a set rather than one at a time, because "each is not covered" would
	// pass with all three collapsed into one verdict.
	never := ExpectedRun{MonitorID: "m", DueAt: due, IntervalSeconds: 60}.Verdict()
	issued := withIssued(base, due).Verdict()
	claimed := withClaimed(withIssued(base, due), due.Add(time.Second)).Verdict()
	if never == issued || issued == claimed || never == claimed {
		t.Fatalf("the three states are not distinguishable: never=%q issued=%q claimed=%q",
			never, issued, claimed)
	}
}

func withIssued(r ExpectedRun, t time.Time) ExpectedRun  { r.IssuedAt = at(t); return r }
func withClaimed(r ExpectedRun, t time.Time) ExpectedRun { r.ClaimedAt = at(t); return r }
func withTerminal(r ExpectedRun, t time.Time) ExpectedRun {
	r.TerminalAt = at(t)
	r.Outcome = "result"
	return r
}

// Invariant 20a and the owner's ruling of 2026-09-04 — lateness above and below the threshold.
//
// A leader absent from 10:01 to 10:42 leaves the 10:01 expectation answered by a probe that
// observed the target at 10:42. Calling that `covered` lets a coverage number count 10:01 as
// proven by an observation forty-one minutes away, which is precisely the over-claim FR-031 exists
// to prevent. `covered_late` licenses no stroke and is excluded from the numerator, while still
// counting in the denominator: a run DID happen and produced an admissible outcome.
//
// The mutation that must fail it is reading the interval from anywhere but the ROW — the monitor's
// current interval, or a revision's base value, either of which misjudges a window probed under
// confirm acceleration.
func TestARunLaterThanItsWindowsOwnIntervalIsCoveredLate(t *testing.T) {
	due := time.Date(2026, 9, 4, 10, 1, 0, 0, time.UTC)
	row := func(intervalSeconds int, issued time.Time) ExpectedRun {
		r := ExpectedRun{
			MonitorID: "m", DueAt: due, IntervalSeconds: intervalSeconds,
			JobID: "11111111-1111-4111-8111-111111111111", CarrierGeneration: LedgerMinCarrier,
		}
		return withTerminal(withIssued(r, issued), issued.Add(time.Second))
	}
	for _, tc := range []struct {
		name     string
		interval int
		issued   time.Time
		want     ExpectedRunVerdict
	}{
		{"a leader gap of forty-one minutes", 60, due.Add(41 * time.Minute), VerdictCoveredLate},
		{"late by exactly one interval", 60, due.Add(60 * time.Second), VerdictCovered},
		{"late by one interval and a microsecond", 60, due.Add(60*time.Second + time.Microsecond), VerdictCoveredLate},
		{"late by less than an interval", 60, due.Add(30 * time.Second), VerdictCovered},
		{"answered early, under confirm acceleration", 10, due.Add(-2 * time.Second), VerdictCovered},
		{"a window spaced at the confirm interval, answered forty seconds late", 10, due.Add(40 * time.Second), VerdictCoveredLate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := row(tc.interval, tc.issued)
			if got := r.Verdict(); got != tc.want {
				t.Fatalf("reads %q, want %q", got, tc.want)
			}
			// The two consequences, asserted separately from the verdict because they are what
			// the verdict is FOR: a stroke, and a coverage numerator.
			wantStroke := tc.want == VerdictCovered
			if r.LicensesStroke() != wantStroke {
				t.Errorf("LicensesStroke is %v for a %q window", r.LicensesStroke(), tc.want)
			}
			if r.CountsInCoverageNumerator() != wantStroke {
				t.Errorf("CountsInCoverageNumerator is %v for a %q window", r.CountsInCoverageNumerator(), tc.want)
			}
		})
	}
	// The SAME window judged against the monitor's current interval instead of its own would read
	// covered, which is the mutation. Stated as an assertion so the difference is visible rather
	// than argued: a 10-second window answered 40 seconds late is late, and a 60-second one is
	// not.
	tight := row(10, due.Add(40*time.Second))
	loose := row(60, due.Add(40*time.Second))
	if tight.Verdict() == loose.Verdict() {
		t.Fatalf("the threshold does not come from the row: both read %q", tight.Verdict())
	}
}

// Invariant 20g — `interval_assumed` is EXPLANATORY ONLY and may never promote a verdict.
//
// It exists so a caller can SEE that a `covered_late` may be an artefact of §14.2's conservative
// threshold. It must never be read as licence to treat such a window as `covered`: not by the
// stroke gate, not by a coverage numerator, not by a future surface arguing the lateness is
// "probably not real". Revision 21 stated the prohibition and shipped no test, so the rule could
// have been IGNORED rather than deleted — which is weaker than either outcome it claimed.
//
// The two mutations are the two a consumer would plausibly write, and both are the same argument
// expressed in code: treat `interval_assumed && covered_late` as covered in the gate, and the same
// promotion in the numerator.
func TestAnAssumedIntervalNeverPromotesAVerdict(t *testing.T) {
	due := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	base := ExpectedRun{
		MonitorID: "m", DueAt: due, IntervalSeconds: 10,
		JobID: "11111111-1111-4111-8111-111111111111", CarrierGeneration: LedgerMinCarrier,
	}
	measured := withTerminal(withIssued(base, due.Add(40*time.Second)), due.Add(41*time.Second))
	assumed := measured
	assumed.IntervalAssumed = true

	if measured.Verdict() != VerdictCoveredLate || assumed.Verdict() != VerdictCoveredLate {
		t.Fatalf("the fixture is not the case under test: measured=%q assumed=%q",
			measured.Verdict(), assumed.Verdict())
	}
	if assumed.LicensesStroke() {
		t.Error("an assumed-threshold covered_late window licenses a stroke: the truthful-rendering " +
			"gate has been widened by accident, which is exactly what invariant 20g exists to make " +
			"require DELETING a rule rather than reinterpreting one")
	}
	if assumed.CountsInCoverageNumerator() {
		t.Error("an assumed-threshold covered_late window counts in the coverage numerator")
	}
	// And the flag changes NOTHING about the verdict in either direction, including for a window
	// that is plainly covered: it is not a modifier, it is an annotation.
	onTime := withTerminal(withIssued(base, due.Add(2*time.Second)), due.Add(3*time.Second))
	onTimeAssumed := onTime
	onTimeAssumed.IntervalAssumed = true
	if onTime.Verdict() != onTimeAssumed.Verdict() {
		t.Errorf("the flag changed a covered window's verdict: %q then %q",
			onTime.Verdict(), onTimeAssumed.Verdict())
	}
}

// Invariant 10c — a window dispatched on a carrier BELOW the ledger minimum reads `unknown`, never
// `issued_never_claimed`. The carrier is tested FIRST, before any timestamp: a result on such a
// carrier could never have correlated to the window at all, so reading its absence of a claim as
// evidence of anything would be a claim about a run the ledger cannot see.
//
// This is the honest verdict, and it is the reason a carrier ROLLBACK costs truth rather than
// correctness.
func TestAWindowBelowTheLedgerCarrierReadsUnknown(t *testing.T) {
	due := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	for generation := 1; generation < LedgerMinCarrier; generation++ {
		row := ExpectedRun{
			MonitorID: "m", DueAt: due, IntervalSeconds: 60,
			JobID: "11111111-1111-4111-8111-111111111111", CarrierGeneration: generation,
			IssuedAt: at(due),
		}
		if got := row.Verdict(); got != VerdictUnknown {
			t.Errorf("a window dispatched on generation %d reads %q, want %q", generation, got, VerdictUnknown)
		}
		if row.LicensesStroke() {
			t.Errorf("a generation-%d window licenses a stroke", generation)
		}
	}
	// A never-DISPATCHED window has no carrier and is not an `unknown` case: it is a stored fact
	// about a run that was expected and never made, which is the distinction FR-032 exists for.
	never := ExpectedRun{MonitorID: "m", DueAt: due, IntervalSeconds: 60}
	if got := never.Verdict(); got != VerdictExpectedNeverIssued {
		t.Errorf("a never-dispatched window reads %q, want %q", got, VerdictExpectedNeverIssued)
	}
	// And the converse, without which the exclusion proves nothing: at the minimum it is eligible.
	eligible := ExpectedRun{
		MonitorID: "m", DueAt: due, IntervalSeconds: 60,
		JobID: "11111111-1111-4111-8111-111111111111", CarrierGeneration: LedgerMinCarrier,
		IssuedAt: at(due),
	}
	if got := eligible.Verdict(); got != VerdictIssuedNeverClaimed {
		t.Errorf("a generation-%d window reads %q, want %q", LedgerMinCarrier, got, VerdictIssuedNeverClaimed)
	}
}
