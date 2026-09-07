package config

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032: `ledger.carrier_enabled` is a versioned contract, not a default, and phase B2 is the
// version that changes the answer.
//
// A default describes only ABSENCE. B1 shipped the carrier with no payload behind it, so it
// REFUSED `true` — accepting it would have published generation-4 jobs carrying no identity
// (invariant 10h), and coercing it to false would have been the self-healing AGENTS.md rules out,
// with the operator believing a gate is on that is off and no inertness test able to see it.
//
// B2 mints `JobID`, `IssuedAt` and `DueAt` on every dispatch path in the same change that wires
// this flag to selection, so there is no build in which the flag is accepted and the payload is
// absent. The refusal is therefore retired, and what survives is the property it was protecting:
// the value the operator supplied reaches selection UNCHANGED, in either direction.

func validateWithLedger(t *testing.T, enabled bool) error {
	t.Helper()
	c := &Config{}
	c.Ledger.CarrierEnabled = enabled
	return c.Validate()
}

// The mirror of B1's refusal test, and the reason both exist in the record: an assertion that a
// gate is refused and an assertion that it is accepted are the same invariant read at two phases,
// and deleting the first would leave a reader unable to tell which build changed.
func TestThisBinaryAcceptsLedgerCarrierEnabled(t *testing.T) {
	err := validateWithLedger(t, true)
	if err != nil && strings.Contains(err.Error(), "ledger.carrier_enabled") {
		t.Fatalf("Validate still refuses ledger.carrier_enabled=true after the payload landed: %v", err)
	}
}

// The value must not be rewritten in EITHER direction. B1 asserted this against a coercion to
// false; B2 asserts it against a coercion of any kind, because a validator that normalises a gate
// leaves the operator's configuration and the running behaviour describing different things.
func TestTheAcceptedValueReachesSelectionUnchanged(t *testing.T) {
	for _, want := range []bool{true, false} {
		c := &Config{}
		c.Ledger.CarrierEnabled = want
		_ = c.Validate()
		if c.Ledger.CarrierEnabled != want {
			t.Fatalf("Validate rewrote ledger.carrier_enabled from %v to %v", want, c.Ledger.CarrierEnabled)
		}
	}
}

// Absent and false must be indistinguishable: neither may reach a later validation error of its
// own, and both must let validation continue to whatever else is wrong with the config.
func TestAbsentAndFalseAreBothAcceptedIdentically(t *testing.T) {
	absent := (&Config{}).Validate()
	explicit := validateWithLedger(t, false)
	if (absent == nil) != (explicit == nil) {
		t.Fatalf("absent and false disagree: absent=%v explicit=%v", absent, explicit)
	}
	if absent != nil && absent.Error() != explicit.Error() {
		t.Fatalf("absent and false produce different errors:\n  absent:   %v\n  explicit: %v", absent, explicit)
	}
	// Whatever that error is, it must not be about the ledger key — the point is that the key is
	// out of the way and the rest of validation proceeds.
	if absent != nil && strings.Contains(absent.Error(), "ledger.carrier_enabled") {
		t.Fatalf("a config without the key still failed on it: %v", absent)
	}
}

// The two ledger bounds are enforced BEFORE business logic starts, which is what makes the store's
// own defaulting a fallback for a caller that never went through config rather than a second
// policy. Zero is accepted on purpose: it means "unset", and the store applies the documented
// default — a config that must name a number to get the recommended one is a config that drifts.
func TestTheLedgerBoundsAreEnforcedAndZeroMeansDefault(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention int
		gapCap    int
		wantKey   string
	}{
		{name: "retention-below-minimum", retention: domain.MinExpectedRunRetentionDays - 1, wantKey: "expected_run_retention_days"},
		{name: "retention-above-maximum", retention: domain.MaxExpectedRunRetentionDays + 1, wantKey: "expected_run_retention_days"},
		{name: "cap-below-minimum", gapCap: domain.MinExpectedRunGapWindowsMax - 1, wantKey: "expected_run_gap_windows_max"},
		{name: "cap-above-maximum", gapCap: domain.MaxExpectedRunGapWindowsMax + 1, wantKey: "expected_run_gap_windows_max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			c.Ledger.ExpectedRunRetentionDays = tc.retention
			c.Ledger.ExpectedRunGapWindowsMax = tc.gapCap
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantKey) {
				t.Fatalf("out-of-range %s was not refused by name: %v", tc.wantKey, err)
			}
		})
	}
	// Zero for both, and the only complaint left is the one every empty config gets.
	c := &Config{}
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "expected_run") {
		t.Fatalf("zero was treated as out of range: %v", err)
	}
}

// F1 — the LOADER supplies the ledger's defaults, like every sibling sub-config's.
//
// It was the one block missing from `defaults()`: `Validate`'s message promised a default it did
// not supply, and the value was re-derived instead at five runtime call sites across the store and
// the scheduler. No behavioural defect — every reader went through a setter that defaulted — but
// one number written in five places is one number that can be edited in four, and a default that
// lives in the loader is the rule this breached.
//
// The mutation that must kill this: remove the `Ledger` block from `defaults()`.
func TestTheLoaderSuppliesTheLedgerDefaults(t *testing.T) {
	c := defaults()
	if c.Ledger.ExpectedRunRetentionDays != domain.DefaultExpectedRunRetentionDays {
		t.Errorf("the loader defaults the retention to %d, want the domain's %d",
			c.Ledger.ExpectedRunRetentionDays, domain.DefaultExpectedRunRetentionDays)
	}
	if c.Ledger.ExpectedRunGapWindowsMax != domain.DefaultExpectedRunGapWindowsMax {
		t.Errorf("the loader defaults the gap cap to %d, want the domain's %d",
			c.Ledger.ExpectedRunGapWindowsMax, domain.DefaultExpectedRunGapWindowsMax)
	}
	// And what the loader supplies is inside the bounds it enforces, or the default would be a
	// value the validator refuses.
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "expected_run") {
		t.Errorf("the loader's own default is out of range: %v", err)
	}
}
