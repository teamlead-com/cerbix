package metrics

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

// gateLines returns only the FR-024 lines of a scrape — HELP, TYPE and samples — in order.
func gateLines(t *testing.T, reg *Registry) string {
	t.Helper()
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	var kept []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "cerbix_gate_") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestGateFamiliesAbsentUntilObserved(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	if got := gateLines(t, reg); got != "" {
		t.Fatalf("a fresh registry must export no gate series, got:\n%s", got)
	}
}

// D9: the state label is the observed truth and the override shows only on the action; every
// recorded series is exported and nothing else is.
func TestGateDecisionCounterRecordsExactSeries(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	records := []struct {
		state, action string
		overridden    bool
	}{
		{"ALLOW", "ALLOW", false},
		{"ALLOW", "ALLOW", false},
		{"BLOCK", "BLOCK", false},
		{"BLOCK", "ALLOW", true},
		{"UNKNOWN", "ALLOW", true},
		{"WARN", "WARN", false},
		{"NOT_CONFIGURED", "", false},
		{"NOT_CONFIGURED", "none", false},
	}
	for _, rec := range records {
		if err := reg.RecordGateDecision(rec.state, rec.action, rec.overridden); err != nil {
			t.Fatalf("RecordGateDecision(%q, %q, %v): %v", rec.state, rec.action, rec.overridden, err)
		}
	}
	got := gateLines(t, reg)
	want := []string{
		`cerbix_gate_decisions_total{state="ALLOW",action="ALLOW",overridden="false"} 2`,
		`cerbix_gate_decisions_total{state="BLOCK",action="ALLOW",overridden="true"} 1`,
		`cerbix_gate_decisions_total{state="BLOCK",action="BLOCK",overridden="false"} 1`,
		`cerbix_gate_decisions_total{state="NOT_CONFIGURED",action="none",overridden="false"} 2`,
		`cerbix_gate_decisions_total{state="UNKNOWN",action="ALLOW",overridden="true"} 1`,
		`cerbix_gate_decisions_total{state="WARN",action="WARN",overridden="false"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	if n := strings.Count(got, "cerbix_gate_decisions_total{"); n != len(want) {
		t.Fatalf("expected exactly %d decision series, found %d in:\n%s", len(want), n, got)
	}
	for _, other := range []string{
		"cerbix_gate_evaluate_rejected_total", "cerbix_gate_evaluate_errors_total",
		"cerbix_gate_maintenance_errors_total", "cerbix_gate_decision_duration_seconds",
		"cerbix_gate_decisions_partitions_pending_drop",
	} {
		if strings.Contains(got, other) {
			t.Fatalf("recording a decision moved an unrelated family %s:\n%s", other, got)
		}
	}
}

func TestGateDecisionRefusesLabelsOutsideClosedSet(t *testing.T) {
	cases := []struct {
		name, state, action string
		overridden          bool
	}{
		{"unknown state", "allow", "ALLOW", false},
		{"empty state", "", "ALLOW", false},
		{"unknown action", "ALLOW", "PERMIT", false},
		{"empty action with a configured state", "ALLOW", "", false},
		{"none with a configured state", "BLOCK", "none", false},
		{"real action with NOT_CONFIGURED", "NOT_CONFIGURED", "ALLOW", false},
		{"NOT_CONFIGURED overridden", "NOT_CONFIGURED", "", true},
		{"principal-shaped value", "ALLOW", "token:ci-deployer", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := New(buildinfo.Info{}, "api")
			err := reg.RecordGateDecision(tc.state, tc.action, tc.overridden)
			if !errors.Is(err, ErrGateMetricLabel) {
				t.Fatalf("want ErrGateMetricLabel, got %v", err)
			}
			if got := gateLines(t, reg); got != "" {
				t.Fatalf("a refused decision must record nothing, got:\n%s", got)
			}
		})
	}
}

// One test per closed single-label family: every allowed value lands on its own series, an
// unknown value is refused and leaves the family untouched.
func TestGateSingleLabelFamiliesAreClosed(t *testing.T) {
	families := []struct {
		metric, label string
		values        []string
		record        func(*Registry, string) error
	}{
		{
			metric: "cerbix_gate_evaluate_rejected_total", label: "reason",
			values: []string{"process_inflight", "principal_inflight", "process_rate", "principal_rate"},
			record: (*Registry).RecordGateEvaluateRejected,
		},
		{
			metric: "cerbix_gate_evaluate_errors_total", label: "kind",
			values: []string{"snapshot_conflict", "timeout", "ledger_unwritable", "error"},
			record: (*Registry).RecordGateEvaluateError,
		},
		{
			metric: "cerbix_gate_maintenance_errors_total", label: "kind",
			values: []string{"lock_timeout", "statement_timeout", "partition_identity", "error"},
			record: (*Registry).RecordGateMaintenanceError,
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
			for _, bad := range []string{"", "unknown", "LOCK_TIMEOUT", "user:alice"} {
				if err := fam.record(reg, bad); !errors.Is(err, ErrGateMetricLabel) {
					t.Fatalf("%s(%q): want ErrGateMetricLabel, got %v", fam.metric, bad, err)
				}
			}
			got := gateLines(t, reg)
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

// The first histogram in the repo: cumulative buckets, an inclusive `le`, an observation above
// every bound landing only in +Inf, and exact _sum/_count.
func TestGateDecisionDurationHistogram(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")
	for _, d := range []time.Duration{
		30 * time.Millisecond,  // below the first bound
		50 * time.Millisecond,  // ON the first bound: le is <=, so it belongs to le="0.05"
		700 * time.Millisecond, // between 0.5 and 1
		31 * time.Second,       // above 30: only +Inf
	} {
		reg.ObserveGateDecisionDuration(d)
	}
	got := gateLines(t, reg)
	want := strings.Join([]string{
		"# HELP cerbix_gate_decision_duration_seconds Wall time of one admitted gate evaluation, request to decision, in seconds over fixed buckets 0.05 s to 30 s (FR-024 §5a).",
		"# TYPE cerbix_gate_decision_duration_seconds histogram",
		`cerbix_gate_decision_duration_seconds_bucket{le="0.05"} 2`,
		`cerbix_gate_decision_duration_seconds_bucket{le="0.1"} 2`,
		`cerbix_gate_decision_duration_seconds_bucket{le="0.25"} 2`,
		`cerbix_gate_decision_duration_seconds_bucket{le="0.5"} 2`,
		`cerbix_gate_decision_duration_seconds_bucket{le="1"} 3`,
		`cerbix_gate_decision_duration_seconds_bucket{le="2.5"} 3`,
		`cerbix_gate_decision_duration_seconds_bucket{le="5"} 3`,
		`cerbix_gate_decision_duration_seconds_bucket{le="10"} 3`,
		`cerbix_gate_decision_duration_seconds_bucket{le="30"} 3`,
		`cerbix_gate_decision_duration_seconds_bucket{le="+Inf"} 4`,
		"cerbix_gate_decision_duration_seconds_sum 31.78",
		"cerbix_gate_decision_duration_seconds_count 4",
	}, "\n")
	if got != want {
		t.Fatalf("histogram exposition mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A histogram's +Inf bucket must always equal its count, whatever the observations.
func TestHistogramInfBucketEqualsCount(t *testing.T) {
	h := newHistogram([]float64{1, 2})
	for _, v := range []float64{0, 1, 1.5, 2, 2.0001, 1e9} {
		h.observe(v)
	}
	var buf bytes.Buffer
	h.write(&prometheusWriter{w: &buf}, "h", "help")
	s := buf.String()
	for _, want := range []string{
		`h_bucket{le="1"} 2`, `h_bucket{le="2"} 4`, `h_bucket{le="+Inf"} 6`, "h_count 6",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// clone must not share the counts slice with the original.
	cp := h.clone()
	h.observe(0)
	if cp.counts[0] == h.counts[0] {
		t.Fatal("clone shares its bucket slice with the live histogram")
	}
}

func TestGateLedgerGaugesSetThenCleared(t *testing.T) {
	reg := New(buildinfo.Info{}, "scheduler")
	gauges := []string{
		"cerbix_gate_decisions_partitions_pending_drop",
		"cerbix_gate_decisions_oldest_partition_age_seconds",
		"cerbix_gate_decisions_writable_horizon_seconds",
		"cerbix_gate_decisions_bytes",
	}
	if got := gateLines(t, reg); got != "" {
		t.Fatalf("ledger gauges must be absent before a pass sampled them:\n%s", got)
	}
	reg.SetGateLedgerGauges(2, 7776000.5, 604800, 136314880)
	got := gateLines(t, reg)
	for _, want := range []string{
		"# TYPE cerbix_gate_decisions_partitions_pending_drop gauge",
		"cerbix_gate_decisions_partitions_pending_drop 2",
		"# TYPE cerbix_gate_decisions_oldest_partition_age_seconds gauge",
		"cerbix_gate_decisions_oldest_partition_age_seconds 7776000.500",
		"# TYPE cerbix_gate_decisions_writable_horizon_seconds gauge",
		"cerbix_gate_decisions_writable_horizon_seconds 604800.000",
		"# TYPE cerbix_gate_decisions_bytes gauge",
		"cerbix_gate_decisions_bytes 136314880",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// A later pass REPLACES the sample; zero is a value, not an absence.
	reg.SetGateLedgerGauges(0, 0, 0, 0)
	got = gateLines(t, reg)
	if !strings.Contains(got, "cerbix_gate_decisions_partitions_pending_drop 0\n") ||
		!strings.Contains(got, "cerbix_gate_decisions_bytes 0") {
		t.Fatalf("a zero sample must still be exported:\n%s", got)
	}
	// Step-down: a deposed pass does not speak — absence, not zero.
	reg.RecordGateMaintenanceError("lock_timeout") // a counter, which must SURVIVE the clear
	reg.ClearGateLedgerGauges()
	got = gateLines(t, reg)
	for _, g := range gauges {
		if strings.Contains(got, g) {
			t.Fatalf("%s still exported after ClearGateLedgerGauges:\n%s", g, got)
		}
	}
	if !strings.Contains(got, `cerbix_gate_maintenance_errors_total{kind="lock_timeout"} 1`) {
		t.Fatalf("clearing the gauges must not erase the process's counters:\n%s", got)
	}
	// Clearing twice, or before any set, is harmless.
	reg.ClearGateLedgerGauges()
	New(buildinfo.Info{}, "scheduler").ClearGateLedgerGauges()
}

// Golden exposition for a small fixture: two scrapes with no new observation are byte-identical,
// and the whole family renders exactly this — HELP, TYPE, sorted labels, fixed order.
func TestGateFamilyExpositionIsByteStable(t *testing.T) {
	reg := New(buildinfo.Info{}, "all")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(reg.RecordGateDecision("BLOCK", "ALLOW", true))
	must(reg.RecordGateDecision("ALLOW", "ALLOW", false))
	must(reg.RecordGateDecision("NOT_CONFIGURED", "", false))
	must(reg.RecordGateEvaluateRejected("principal_rate"))
	must(reg.RecordGateEvaluateRejected("process_inflight"))
	must(reg.RecordGateEvaluateError("ledger_unwritable"))
	must(reg.RecordGateMaintenanceError("partition_identity"))
	must(reg.RecordGateMaintenanceError("lock_timeout"))
	must(reg.RecordGateMaintenanceError("lock_timeout"))
	reg.ObserveGateDecisionDuration(250 * time.Millisecond)
	reg.ObserveGateDecisionDuration(3 * time.Second)
	reg.SetGateLedgerGauges(1, 86400, 518400, 4096)

	first := gateLines(t, reg)
	second := gateLines(t, reg)
	if first != second {
		t.Fatalf("two scrapes with no new observation differ\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	golden := strings.Join([]string{
		`# HELP cerbix_gate_decisions_total Gate decisions by observed state, effective action and whether an active override changed the action (FR-024 D9); a NOT_CONFIGURED decision has no action and carries action="none".`,
		"# TYPE cerbix_gate_decisions_total counter",
		`cerbix_gate_decisions_total{state="ALLOW",action="ALLOW",overridden="false"} 1`,
		`cerbix_gate_decisions_total{state="BLOCK",action="ALLOW",overridden="true"} 1`,
		`cerbix_gate_decisions_total{state="NOT_CONFIGURED",action="none",overridden="false"} 1`,
		"# HELP cerbix_gate_evaluate_rejected_total Gate evaluations refused by a process-local bound before evaluation, by reason (FR-024 §5a).",
		"# TYPE cerbix_gate_evaluate_rejected_total counter",
		`cerbix_gate_evaluate_rejected_total{reason="principal_rate"} 1`,
		`cerbix_gate_evaluate_rejected_total{reason="process_inflight"} 1`,
		"# HELP cerbix_gate_evaluate_errors_total Admitted gate evaluations that failed, by kind — evaluation errors only, never maintenance (FR-024 §5a).",
		"# TYPE cerbix_gate_evaluate_errors_total counter",
		`cerbix_gate_evaluate_errors_total{kind="ledger_unwritable"} 1`,
		"# HELP cerbix_gate_maintenance_errors_total Decision-ledger maintenance statements refused or failed, by kind; retried next pass, never escalated to a longer wait (FR-024 D10).",
		"# TYPE cerbix_gate_maintenance_errors_total counter",
		`cerbix_gate_maintenance_errors_total{kind="lock_timeout"} 2`,
		`cerbix_gate_maintenance_errors_total{kind="partition_identity"} 1`,
		"# HELP cerbix_gate_decision_duration_seconds Wall time of one admitted gate evaluation, request to decision, in seconds over fixed buckets 0.05 s to 30 s (FR-024 §5a).",
		"# TYPE cerbix_gate_decision_duration_seconds histogram",
		`cerbix_gate_decision_duration_seconds_bucket{le="0.05"} 0`,
		`cerbix_gate_decision_duration_seconds_bucket{le="0.1"} 0`,
		`cerbix_gate_decision_duration_seconds_bucket{le="0.25"} 1`,
		`cerbix_gate_decision_duration_seconds_bucket{le="0.5"} 1`,
		`cerbix_gate_decision_duration_seconds_bucket{le="1"} 1`,
		`cerbix_gate_decision_duration_seconds_bucket{le="2.5"} 1`,
		`cerbix_gate_decision_duration_seconds_bucket{le="5"} 2`,
		`cerbix_gate_decision_duration_seconds_bucket{le="10"} 2`,
		`cerbix_gate_decision_duration_seconds_bucket{le="30"} 2`,
		`cerbix_gate_decision_duration_seconds_bucket{le="+Inf"} 2`,
		"cerbix_gate_decision_duration_seconds_sum 3.25",
		"cerbix_gate_decision_duration_seconds_count 2",
		"# HELP cerbix_gate_decisions_partitions_pending_drop Decision-ledger partitions attached past the retention cutoff plus detached but not yet dropped (FR-024 D10).",
		"# TYPE cerbix_gate_decisions_partitions_pending_drop gauge",
		"cerbix_gate_decisions_partitions_pending_drop 1",
		"# HELP cerbix_gate_decisions_oldest_partition_age_seconds Age of the oldest attached decision partition's upper bound, in seconds; 0 when none is past the cutoff (FR-024 D10).",
		"# TYPE cerbix_gate_decisions_oldest_partition_age_seconds gauge",
		"cerbix_gate_decisions_oldest_partition_age_seconds 86400.000",
		"# HELP cerbix_gate_decisions_writable_horizon_seconds Seconds until the upper bound of the newest attached decision partition, from the registry and catalog; the ledger stops accepting decisions at 0 (FR-024 D10).",
		"# TYPE cerbix_gate_decisions_writable_horizon_seconds gauge",
		"cerbix_gate_decisions_writable_horizon_seconds 518400.000",
		"# HELP cerbix_gate_decisions_bytes Sum of pg_total_relation_size over decision-ledger partitions not yet dropped, in bytes (FR-024 D10).",
		"# TYPE cerbix_gate_decisions_bytes gauge",
		"cerbix_gate_decisions_bytes 4096",
	}, "\n")
	if first != golden {
		t.Fatalf("gate exposition drifted from golden\n--- got ---\n%s\n--- want ---\n%s", first, golden)
	}
}

// §5a: "all low-cardinality and none carrying a principal". The label vocabulary of the whole
// family is fixed here; a new label name is a spec change, not a code change.
func TestGateFamiliesCarryOnlyClosedLabels(t *testing.T) {
	reg := New(buildinfo.Info{}, "all")
	_ = reg.RecordGateDecision("WARN", "WARN", false)
	_ = reg.RecordGateEvaluateRejected("process_rate")
	_ = reg.RecordGateEvaluateError("timeout")
	_ = reg.RecordGateMaintenanceError("error")
	reg.ObserveGateDecisionDuration(time.Second)
	reg.SetGateLedgerGauges(0, 0, 0, 0)

	allowed := map[string]map[string]bool{
		"cerbix_gate_decisions_total":                        {"state": true, "action": true, "overridden": true},
		"cerbix_gate_evaluate_rejected_total":                {"reason": true},
		"cerbix_gate_evaluate_errors_total":                  {"kind": true},
		"cerbix_gate_maintenance_errors_total":               {"kind": true},
		"cerbix_gate_decision_duration_seconds_bucket":       {"le": true},
		"cerbix_gate_decision_duration_seconds_sum":          {},
		"cerbix_gate_decision_duration_seconds_count":        {},
		"cerbix_gate_decisions_partitions_pending_drop":      {},
		"cerbix_gate_decisions_oldest_partition_age_seconds": {},
		"cerbix_gate_decisions_writable_horizon_seconds":     {},
		"cerbix_gate_decisions_bytes":                        {},
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(gateLines(t, reg), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		head, _, _ := strings.Cut(line, " ")
		name, rest, hasLabels := strings.Cut(head, "{")
		labels, ok := allowed[name]
		if !ok {
			t.Fatalf("unexpected gate series %q — the §5a surface is fixed", name)
		}
		seen[name] = true
		if !hasLabels {
			if len(labels) != 0 {
				t.Fatalf("%s rendered without its labels: %s", name, line)
			}
			continue
		}
		labelSet, _, _ := strings.Cut(rest, "}")
		for _, pair := range strings.Split(labelSet, ",") {
			key, _, _ := strings.Cut(pair, "=")
			if !labels[key] {
				t.Fatalf("gate series %s carries label %q — no principal, service or tenant label may exist here (line: %s)", name, key, line)
			}
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Fatalf("gate series %s was never emitted", name)
		}
	}
}

// Recorders and the scrape share one mutex; -race is the oracle.
func TestGateRecordersAreConcurrencySafe(t *testing.T) {
	reg := New(buildinfo.Info{}, "all")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				_ = reg.RecordGateDecision("ALLOW", "ALLOW", false)
				_ = reg.RecordGateEvaluateRejected("process_rate")
				_ = reg.RecordGateEvaluateError("error")
				_ = reg.RecordGateMaintenanceError("error")
				reg.ObserveGateDecisionDuration(time.Duration(n) * time.Millisecond)
				if i%2 == 0 {
					reg.SetGateLedgerGauges(n, float64(n), float64(n), int64(n))
				} else {
					reg.ClearGateLedgerGauges()
				}
				var sink bytes.Buffer
				reg.WritePrometheus(&sink)
			}
		}(i)
	}
	wg.Wait()
	got := gateLines(t, reg)
	if !strings.Contains(got, `cerbix_gate_decisions_total{state="ALLOW",action="ALLOW",overridden="false"} 1600`) ||
		!strings.Contains(got, "cerbix_gate_decision_duration_seconds_count 1600") {
		t.Fatalf("lost observations under concurrency:\n%s", got)
	}
}
