package config

import (
	"strings"
	"testing"
)

// FR-032: `ledger.carrier_enabled` is a versioned contract, not a default.
//
// A default describes only ABSENCE. An operator can supply the key, and both silent readings are
// wrong: accepting it in a binary with no V4 payload publishes generation-4 jobs carrying no
// identity (invariant 10h), and coercing it to false is the self-healing runtime AGENTS.md rules
// out — the operator would believe a gate is on that is off, and no inertness test can see that.

func validateWithLedger(t *testing.T, enabled bool) error {
	t.Helper()
	c := &Config{}
	c.Ledger.CarrierEnabled = enabled
	return c.Validate()
}

func TestABinaryOfThisGenerationRefusesLedgerCarrierEnabled(t *testing.T) {
	err := validateWithLedger(t, true)
	if err == nil {
		t.Fatal("Validate accepted ledger.carrier_enabled=true; a binary with no V4 payload would " +
			"publish generation-4 jobs carrying no identity")
	}
	// The refusal has to be actionable, not merely a failure: an operator reading it must learn
	// which key, and why setting it did nothing they wanted.
	for _, want := range []string{"ledger.carrier_enabled", "inert", "phase B2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q; message: %v", want, err)
		}
	}
}

// The refusal must not be a coercion. This is the mutation no inertness test can catch: with the
// value silently rewritten to false, every "is it inert?" assertion still passes while the
// operator believes the gate is on.
func TestTheRefusalDoesNotRewriteTheValue(t *testing.T) {
	c := &Config{}
	c.Ledger.CarrierEnabled = true
	_ = c.Validate()
	if !c.Ledger.CarrierEnabled {
		t.Fatal("Validate coerced ledger.carrier_enabled to false instead of refusing it")
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
