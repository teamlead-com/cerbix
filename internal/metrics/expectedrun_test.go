package metrics

import (
	"bytes"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

// FR-032 §11 — the expected-run storage measurement, asserted at the EXPOSITION.
//
// `TestTheHOTRatioIsUndefinedUntilSomethingHasUpdated` in `internal/store` covers the sampler: it
// proves the ratio is undefined with a zero denominator. Nothing covered the other side — what a
// scrape actually contains — so the series names and their declared TYPE were asserted by no test
// at all, in a requirement whose §11 gate is exactly "watch this number". That is the same
// wiring-boundary gap the audit found one layer up, where the sampler and the registry were both
// correct and nothing connected them.

// ledgerLines returns only the FR-032 lines of a scrape — HELP, TYPE and samples — in order.
func ledgerLines(t *testing.T, reg *Registry) string {
	t.Helper()
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	var kept []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "cerbix_expected_runs") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestTheLedgerFamiliesAreAbsentUntilThePassSamples(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	if got := ledgerLines(t, reg); got != "" {
		t.Fatalf("a registry nobody sampled exports ledger series:\n%s\n"+
			"They belong to the leader's maintenance pass; on a role that never runs it their "+
			"absence is the correct answer, not a fault", got)
	}
}

// The ratio is published ONLY when defined, and its two inputs are published either way.
//
// An undefined ratio has no honest value: 0 and 1 are both lies, in whichever direction happens to
// be convenient. The inputs are still exported, because "nothing has updated yet" is a true thing
// to say and the gate reads it as "not enough sample".
func TestTheHOTRatioIsExportedOnlyWhenDefined(t *testing.T) {
	reg := New(buildinfo.Info{}, "all")
	reg.SetExpectedRunHOT(&ExpectedRunHOTStat{Updates: 0, HOTUpdates: 0, Defined: false})
	got := ledgerLines(t, reg)
	if strings.Contains(got, "cerbix_expected_runs_hot_update_ratio") {
		t.Errorf("the ratio is published with a zero denominator:\n%s", got)
	}
	for _, want := range []string{
		"cerbix_expected_runs_updates 0",
		"cerbix_expected_runs_hot_updates 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from:\n%s", want, got)
		}
	}

	reg.SetExpectedRunHOT(&ExpectedRunHOTStat{Updates: 1000, HOTUpdates: 950, Defined: true})
	got = ledgerLines(t, reg)
	for _, want := range []string{
		"cerbix_expected_runs_updates 1000",
		"cerbix_expected_runs_hot_updates 950",
		"cerbix_expected_runs_hot_update_ratio 0.95",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from:\n%s", want, got)
		}
	}

	// A nil sample UNPUBLISHES the family — the only honest way to say "no measurement" in the
	// Prometheus text format.
	reg.SetExpectedRunHOT(nil)
	if got := ledgerLines(t, reg); got != "" {
		t.Errorf("a nil sample left series behind:\n%s", got)
	}
}

// The three families' names and TYPEs, asserted exactly.
//
// The two inputs are GAUGES and were specified — and shipped — as `_total` COUNTERS. They are
// `pg_stat_user_tables` sums across the RETAINED partitions, so retention dropping a partition
// subtracts that partition's statistics and the series FALLS. A `_total` counter that falls is read
// by Prometheus as a process restart, and `rate()` over it then reports traffic that never
// happened — a metric lying about the one table whose whole subject is not over-claiming. The name
// went with the type, because a `_total` gauge is a contradiction a dashboard author reads as a
// counter anyway.
func TestTheLedgerFamiliesDeclareTheTypesTheyActuallyAre(t *testing.T) {
	reg := New(buildinfo.Info{}, "all")
	reg.SetExpectedRunHOT(&ExpectedRunHOTStat{Updates: 4, HOTUpdates: 3, Defined: true})
	got := ledgerLines(t, reg)

	for _, want := range []string{
		"# TYPE cerbix_expected_runs_updates gauge",
		"# TYPE cerbix_expected_runs_hot_updates gauge",
		"# TYPE cerbix_expected_runs_hot_update_ratio gauge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from:\n%s", want, got)
		}
	}
	if strings.Contains(got, "counter") {
		t.Errorf("a ledger family declares itself a counter, and none of them is one — they are "+
			"pg_stat sums that fall when a partition is dropped:\n%s", got)
	}
	if strings.Contains(got, "_total") {
		t.Errorf("a ledger family carries the `_total` suffix, which names a monotonic counter:\n%s", got)
	}
}
