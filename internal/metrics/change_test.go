package metrics

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

// FR-025 D15 (invariant 20): the change-intelligence families. Closed label sets, exact series,
// no per-service/source/identity label anywhere, and nothing exported until observed.

// changeLines returns only the FR-025 lines of a scrape — HELP, TYPE and samples — in order.
func changeLines(t *testing.T, reg *Registry) string {
	t.Helper()
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	var kept []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "cerbix_change") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestChangeFamiliesAbsentUntilObserved(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	if got := changeLines(t, reg); got != "" {
		t.Fatalf("a fresh registry must export no change series, got:\n%s", got)
	}
}

// D3/D15: recorded vs replayed are two outcomes of one family; every series is exact and the
// kind/phase sets are closed — a service or identity shaped value is refused, never a label.
func TestChangesRecordedCounterRecordsExactSeries(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	records := []struct {
		kind, phase string
		replayed    bool
	}{
		{"deploy", "started", false},
		{"deploy", "succeeded", false},
		{"deploy", "succeeded", false},
		{"deploy", "succeeded", true},
		{"rollback", "failed", false},
		{"flag", "cancelled", true},
	}
	for _, rec := range records {
		if err := reg.RecordChangeRecorded(rec.kind, rec.phase, rec.replayed); err != nil {
			t.Fatalf("RecordChangeRecorded(%q, %q, %v): %v", rec.kind, rec.phase, rec.replayed, err)
		}
	}
	got := changeLines(t, reg)
	want := []string{
		"# TYPE cerbix_changes_recorded_total counter",
		`cerbix_changes_recorded_total{kind="deploy",phase="started",outcome="recorded"} 1`,
		`cerbix_changes_recorded_total{kind="deploy",phase="succeeded",outcome="recorded"} 2`,
		`cerbix_changes_recorded_total{kind="deploy",phase="succeeded",outcome="replayed"} 1`,
		`cerbix_changes_recorded_total{kind="flag",phase="cancelled",outcome="replayed"} 1`,
		`cerbix_changes_recorded_total{kind="rollback",phase="failed",outcome="recorded"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	if n := strings.Count(got, "cerbix_changes_recorded_total{"); n != 5 {
		t.Fatalf("expected exactly 5 recorded series, found %d in:\n%s", n, got)
	}
	for _, other := range []string{"cerbix_change_record_rejected_total", "cerbix_change_correlations_total",
		"cerbix_change_correlation_errors_total", "cerbix_change_compare_total", "cerbix_changes_retained"} {
		if strings.Contains(got, other) {
			t.Fatalf("recording a change moved an unrelated family %s:\n%s", other, got)
		}
	}
	for _, bad := range []struct{ kind, phase string }{
		{"config", "started"}, {"", "started"}, {"deploy", "running"}, {"deploy", ""},
		{"checkout", "succeeded"}, {"deploy", "github-actions"},
	} {
		reg := New(buildinfo.Info{}, "api")
		if err := reg.RecordChangeRecorded(bad.kind, bad.phase, false); !errors.Is(err, ErrChangeMetricLabel) {
			t.Fatalf("RecordChangeRecorded(%q, %q): want ErrChangeMetricLabel, got %v", bad.kind, bad.phase, err)
		}
		if got := changeLines(t, reg); got != "" {
			t.Fatalf("a refused record must record nothing, got:\n%s", got)
		}
	}
}

// One test per closed single-label family: every allowed value lands on its own series, an
// unknown value is refused and leaves the family untouched.
func TestChangeSingleLabelFamiliesAreClosed(t *testing.T) {
	families := []struct {
		metric, label string
		values        []string
		record        func(*Registry, string) error
	}{
		{
			metric: "cerbix_change_record_rejected_total", label: "reason",
			values: []string{"phase_order", "phase_exists", "kind_mismatch", "decision_unknown",
				"occurred_at_before_start", "occurred_at_out_of_bounds", "source_invalid", "external_id_invalid",
				"ref_invalid", "url_invalid", "kind_invalid", "phase_invalid", "body_invalid",
				"process_inflight", "principal_inflight", "process_rate", "principal_rate"},
			record: (*Registry).RecordChangeRecordRejected,
		},
		{
			metric: "cerbix_change_compare_total", label: "outcome",
			values: []string{"figure", "withheld", "pending"},
			record: (*Registry).RecordChangeCompare,
		},
	}
	for _, fam := range families {
		t.Run(fam.metric, func(t *testing.T) {
			reg := New(buildinfo.Info{}, "api")
			for i, v := range fam.values {
				for n := 0; n <= i; n++ { // value i is recorded i+1 times
					if err := fam.record(reg, v); err != nil {
						t.Fatalf("%s(%q): %v", fam.metric, v, err)
					}
				}
			}
			for _, bad := range []string{"", "unknown", "PHASE_ORDER", "token:ci", "github-actions", "run-42"} {
				if err := fam.record(reg, bad); !errors.Is(err, ErrChangeMetricLabel) {
					t.Fatalf("%s(%q): want ErrChangeMetricLabel, got %v", fam.metric, bad, err)
				}
			}
			got := changeLines(t, reg)
			for i, v := range fam.values {
				want := fam.metric + `{` + fam.label + `="` + v + `"} ` + strconv.Itoa(i+1)
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in:\n%s", want, got)
				}
			}
			if n := strings.Count(got, fam.metric+"{"); n != len(fam.values) {
				t.Fatalf("expected exactly %d series of %s, found %d in:\n%s", len(fam.values), fam.metric, n, got)
			}
			if !strings.Contains(got, "# TYPE "+fam.metric+" counter") {
				t.Fatalf("missing TYPE line for %s in:\n%s", fam.metric, got)
			}
			for _, other := range families {
				if other.metric != fam.metric && strings.Contains(got, other.metric) {
					t.Fatalf("recording %s moved %s:\n%s", fam.metric, other.metric, got)
				}
			}
		})
	}
}

// D7: the correlation pair as the outbox worker drives it — links by role (inserted rows only),
// failures as a plain counter that exports its ZERO once any correlation has been observed, so the
// runbook's `> 0 for 15m` rule has a series to watch from the first healthy incident. A role
// outside the two, or a non-positive count, is dropped (the interface returns no error).
func TestChangeCorrelationFamiliesFromTheOutboxSide(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	reg.RecordChangeCorrelations("own_service", 2)
	reg.RecordChangeCorrelations("upstream", 1)
	reg.RecordChangeCorrelations("own_service", 1)
	reg.RecordChangeCorrelations("downstream", 5)  // not a role: dropped
	reg.RecordChangeCorrelations("own_service", 0) // nothing inserted: dropped
	reg.RecordChangeCorrelations("checkout", 1)    // a service-shaped value: dropped
	got := changeLines(t, reg)
	for _, w := range []string{
		`cerbix_change_correlations_total{role="own_service"} 3`,
		`cerbix_change_correlations_total{role="upstream"} 1`,
		"# TYPE cerbix_change_correlation_errors_total counter",
		"cerbix_change_correlation_errors_total 0",
	} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	if n := strings.Count(got, "cerbix_change_correlations_total{"); n != 2 {
		t.Fatalf("expected exactly 2 correlation series, found %d in:\n%s", n, got)
	}
	if strings.Contains(got, "downstream") || strings.Contains(got, "checkout") {
		t.Fatalf("a value outside the closed role set was exported:\n%s", got)
	}

	// An error alone speaks too — the incident opened, the correlation did not.
	reg = New(buildinfo.Info{}, "api")
	reg.RecordChangeCorrelationError()
	reg.RecordChangeCorrelationError()
	got = changeLines(t, reg)
	if !strings.Contains(got, "cerbix_change_correlation_errors_total 2") {
		t.Fatalf("missing the error counter in:\n%s", got)
	}
	if strings.Contains(got, "cerbix_change_correlations_total{") {
		t.Fatalf("an error must not invent a links series:\n%s", got)
	}
}

// D9: the retained gauge is absent until the retention pass samples it, follows the latest
// sample, and disappears when a deposed leader clears it — the counters stay.
func TestChangesRetainedGaugeSetThenCleared(t *testing.T) {
	reg := New(buildinfo.Info{}, "scheduler")
	if got := changeLines(t, reg); strings.Contains(got, "cerbix_changes_retained") {
		t.Fatalf("gauge must be absent before the first pass:\n%s", got)
	}
	reg.SetChangesRetained(1234)
	got := changeLines(t, reg)
	for _, w := range []string{"# TYPE cerbix_changes_retained gauge", "cerbix_changes_retained 1234"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	reg.SetChangesRetained(7)
	if got := changeLines(t, reg); !strings.Contains(got, "cerbix_changes_retained 7") || strings.Contains(got, "1234") {
		t.Fatalf("gauge must follow the latest sample:\n%s", got)
	}
	if err := reg.RecordChangeRecordRejected("phase_order"); err != nil {
		t.Fatal(err)
	}
	reg.ClearChangesRetained()
	got = changeLines(t, reg)
	if strings.Contains(got, "cerbix_changes_retained") {
		t.Fatalf("a cleared gauge must be absent:\n%s", got)
	}
	if !strings.Contains(got, `cerbix_change_record_rejected_total{reason="phase_order"} 1`) {
		t.Fatalf("clearing the gauge must leave the counters:\n%s", got)
	}
}

// Invariant 20 pinned as text: the exposition names exactly the D15 families and no label of
// the surface is service_id, source or external_id.
func TestChangeExpositionNamesOnlyTheD15FamiliesAndLabels(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	_ = reg.RecordChangeRecorded("deploy", "succeeded", false)
	_ = reg.RecordChangeRecordRejected("phase_order")
	reg.RecordChangeCorrelations("own_service", 1)
	reg.RecordChangeCorrelationError()
	_ = reg.RecordChangeCompare("figure")
	reg.SetChangesRetained(1)
	got := changeLines(t, reg)
	for _, name := range []string{
		"cerbix_changes_recorded_total", "cerbix_change_record_rejected_total", "cerbix_change_correlations_total",
		"cerbix_change_correlation_errors_total", "cerbix_change_compare_total", "cerbix_changes_retained",
	} {
		if !strings.Contains(got, "# TYPE "+name+" ") {
			t.Fatalf("missing family %s in:\n%s", name, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, forbidden := range []string{"service_id=", "source=", "external_id=", "service=", "identity="} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("forbidden label %q on %q", forbidden, line)
			}
		}
	}
	// Two scrapes with no new observation are byte-identical.
	if again := changeLines(t, reg); again != got {
		t.Fatalf("exposition is not deterministic\n--- first ---\n%s\n--- second ---\n%s", got, again)
	}
}

// The HELP text of `cerbix_change_compare_total` is read by operators on /metrics, and it is the
// one place a stale sentence is not merely a comment. It said `pending` meant "after not yet
// sealed", which stopped being true at D-0211: EITHER side may reach past `sealed_through`, and
// the handler has counted it that way since. Nothing asserted the wording, so the drift was
// invisible until review [11] of the close-out party read the file. This pins it.
func TestChangeCompareHelpDescribesPendingOnEitherSide(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	if err := reg.RecordChangeCompare("pending"); err != nil {
		t.Fatal(err)
	}
	got := changeLines(t, reg)
	var help string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "# HELP cerbix_change_compare_total") {
			help = line
		}
	}
	if help == "" {
		t.Fatalf("no HELP line for cerbix_change_compare_total in:\n%s", got)
	}
	if !strings.Contains(help, "pending (either side not yet sealed)") {
		t.Fatalf("HELP does not describe pending side-neutrally (D-0211):\n%s", help)
	}
	if strings.Contains(help, "after not yet sealed") {
		t.Fatalf("HELP still says pending is about the AFTER side only:\n%s", help)
	}
}
