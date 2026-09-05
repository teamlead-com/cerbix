package domain

import (
	"testing"
	"time"
)

// FR-032 phase F, invariant 27e (§7.4, §17.14) — the state the model did not have.
//
// A window whose identity the core minted and committed, and for which no dispatch is recorded, may
// read NEITHER absence verdict. `expected_never_issued` asserts that no run happened, which is the
// opposite of what is known; `issued_never_claimed` asserts that a job was PUBLISHED, which is
// exactly what a reserved window cannot say. The defect that made this necessary was measured on a
// running instance: every `expected_never_issued` fact it held was false.
//
// The POSITION of the test inside `Verdict()` is part of the specification, so these cases pin it
// from three sides: a reserved row must not be read as an issue that vanished (the test may not move
// after the absence tests), a terminal must still outrank it (it may not move before the terminal
// test), and a window below the ledger carrier is `unknown` whatever else is true (it may not move
// before rule 1).
func TestAReservedWindowIsNeitherMissedNorIssued(t *testing.T) {
	due := time.Date(2026, 9, 5, 14, 13, 50, 0, time.UTC)
	reserved := ExpectedRun{
		MonitorID: "m", DueAt: due, IntervalSeconds: 60,
		JobID: "22222222-2222-4222-8222-222222222222", CarrierGeneration: LedgerMinCarrier,
		ReservedAt: at(due),
	}
	for _, tc := range []struct {
		name string
		row  ExpectedRun
		want ExpectedRunVerdict
	}{
		{
			// The process died between RESERVE and PUBLISH. Nothing knows whether the job left.
			name: "identity minted, no dispatch recorded",
			row:  reserved,
			want: VerdictReserved,
		},
		{
			// The publish failed and said so. Same knowledge, plus the reason — and the reason may
			// not change the verdict, or two rows describing one state would read differently.
			name: "the publish failed and the row says why",
			row:  withWithheld(reserved, "transport_unavailable"),
			want: VerdictReserved,
		},
		{
			// A claim PROVES the job was published and only the confirmation was lost, so the
			// honest reading is the one that names a run in flight.
			name: "an executor claimed it, which proves the publish",
			row:  withClaimed(reserved, due.Add(time.Second)),
			want: VerdictClaimedNeverFinished,
		},
		{
			// 27h's rendering half: the fill may not write `issued_at` on a reserved row, so an
			// early terminal leaves it NULL — and a terminal outranks every absence.
			name: "a terminal arrived before the confirm",
			row:  withTerminal(reserved, due.Add(2*time.Second)),
			want: VerdictCovered,
		},
		{
			// The ordinary path: CONFIRM ran, so the window is an ordinary issued one and the
			// reserved state is behind it.
			name: "the confirm landed and nothing answered",
			row:  withIssued(reserved, due),
			want: VerdictIssuedNeverClaimed,
		},
		{
			// Rule 1 stays first. Below the ledger carrier no result can correlate, so the ledger
			// cannot answer whatever the core recorded about its own intent.
			name: "reserved below the ledger carrier",
			row:  withCarrier(reserved, LedgerMinCarrier-1),
			want: VerdictUnknown,
		},
		{
			// And the absence verdict keeps its own meaning: a window with no identity at all is
			// still the fact this requirement exists to record.
			name: "no identity was ever minted",
			row:  ExpectedRun{MonitorID: "m", DueAt: due, IntervalSeconds: 60},
			want: VerdictExpectedNeverIssued,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.Verdict(); got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// A reserved window licenses no stroke and counts in the denominator only. Asserted rather than
// inferred from `== VerdictCovered`: both methods are one expression today, and a future reading
// that "a reserved window was probably fine" must require DELETING a rule rather than
// reinterpreting one (the argument invariant 20g exists for).
func TestAReservedWindowLicensesNothing(t *testing.T) {
	due := time.Date(2026, 9, 5, 14, 13, 50, 0, time.UTC)
	row := ExpectedRun{
		MonitorID: "m", DueAt: due, IntervalSeconds: 60,
		JobID: "22222222-2222-4222-8222-222222222222", CarrierGeneration: LedgerMinCarrier,
		ReservedAt: at(due),
	}
	if row.Verdict() != VerdictReserved {
		t.Fatalf("fixture is not reserved: %q", row.Verdict())
	}
	if row.LicensesStroke() {
		t.Fatal("a reserved window licensed a stroke: the panel would draw across time nothing proves")
	}
	if row.CountsInCoverageNumerator() {
		t.Fatal("a reserved window counted as coverage")
	}
}

func withWithheld(r ExpectedRun, reason string) ExpectedRun { r.WithheldReason = reason; return r }
func withCarrier(r ExpectedRun, gen int) ExpectedRun        { r.CarrierGeneration = gen; return r }
