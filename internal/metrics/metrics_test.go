package metrics

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

type failAfterWriter struct {
	calls  int
	failAt int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls >= w.failAt {
		return 0, errors.New("injected writer failure")
	}
	return len(p), nil
}

func TestWritePrometheusEmitsCoreSeries(t *testing.T) {
	reg := New(buildinfo.Info{Version: "v1", Commit: "abc", GoVersion: "go1.24"}, "api")
	reg.SetReady(true, "")

	var out bytes.Buffer
	reg.WritePrometheus(&out)
	got := out.String()

	for _, want := range []string{
		`cerbix_build_info{version="v1",commit="abc",go_version="go1.24",role="api"} 1`,
		"cerbix_up 1",
		"cerbix_ready 1",
		"cerbix_uptime_seconds",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, got)
		}
	}
}

func TestWritePrometheusStopsAfterWriterFailure(t *testing.T) {
	reg := New(buildinfo.Info{Version: "v1", Commit: "abc", GoVersion: "go1.24"}, "api")
	w := &failAfterWriter{failAt: 3}
	reg.WritePrometheus(w)
	if w.calls != w.failAt {
		t.Fatalf("writer called %d times after failure, want exactly %d", w.calls, w.failAt)
	}
}

func TestPullStatsGauges(t *testing.T) {
	reg := New(buildinfo.Info{}, "scheduler")
	var before bytes.Buffer
	reg.WritePrometheus(&before)
	if strings.Contains(before.String(), "cerbix_pull_jobs_pending") {
		t.Fatal("pull gauges should be absent until sampled")
	}
	reg.SetPullStats([]PullStat{{Region: "geo3", Pending: 5, LagSeconds: 42.5}})
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	s := out.String()
	if !strings.Contains(s, `cerbix_pull_jobs_pending{region="geo3"} 5`) {
		t.Fatalf("missing pending gauge:\n%s", s)
	}
	if !strings.Contains(s, `cerbix_pull_agent_lag_seconds{region="geo3"} 42.500`) {
		t.Fatalf("missing lag gauge:\n%s", s)
	}
}

func TestSchedulerLeaderGauge(t *testing.T) {
	reg := New(buildinfo.Info{}, "scheduler")
	var before bytes.Buffer
	reg.WritePrometheus(&before)
	if strings.Contains(before.String(), "cerbix_scheduler_leader") {
		t.Fatal("leader gauge should be absent until a scheduler tracks it")
	}
	reg.SetSchedulerLeader(false)
	var standby bytes.Buffer
	reg.WritePrometheus(&standby)
	if !strings.Contains(standby.String(), "cerbix_scheduler_leader 0") {
		t.Fatalf("expected scheduler_leader 0, got:\n%s", standby.String())
	}
	reg.SetSchedulerLeader(true)
	var lead bytes.Buffer
	reg.WritePrometheus(&lead)
	if !strings.Contains(lead.String(), "cerbix_scheduler_leader 1") {
		t.Fatalf("expected scheduler_leader 1, got:\n%s", lead.String())
	}
}

func TestBrokerUpGauge(t *testing.T) {
	reg := New(buildinfo.Info{}, "worker")
	var before bytes.Buffer
	reg.WritePrometheus(&before)
	if strings.Contains(before.String(), "cerbix_broker_up") {
		t.Fatal("broker gauge should be absent until the AMQP transport tracks it")
	}
	reg.SetBrokerUp(true)
	var up bytes.Buffer
	reg.WritePrometheus(&up)
	if !strings.Contains(up.String(), "cerbix_broker_up 1") {
		t.Fatalf("expected broker_up 1, got:\n%s", up.String())
	}
	reg.SetBrokerUp(false)
	var down bytes.Buffer
	reg.WritePrometheus(&down)
	if !strings.Contains(down.String(), "cerbix_broker_up 0") {
		t.Fatalf("expected broker_up 0, got:\n%s", down.String())
	}
}

func TestDatabaseUpOnlyExportedWhenEnabled(t *testing.T) {
	reg := New(buildinfo.Info{}, "api")

	var before bytes.Buffer
	reg.WritePrometheus(&before)
	if strings.Contains(before.String(), "cerbix_database_up") {
		t.Fatal("database_up should not be exported before a DB is configured")
	}

	reg.SetDatabaseUp(false)
	var down bytes.Buffer
	reg.WritePrometheus(&down)
	if !strings.Contains(down.String(), "cerbix_database_up 0") {
		t.Fatalf("expected database_up 0, got:\n%s", down.String())
	}

	reg.SetDatabaseUp(true)
	var up bytes.Buffer
	reg.WritePrometheus(&up)
	if !strings.Contains(up.String(), "cerbix_database_up 1") {
		t.Fatalf("expected database_up 1, got:\n%s", up.String())
	}
}

func TestReadinessTransitions(t *testing.T) {
	reg := New(buildinfo.Info{}, "worker")
	if reg.Ready() {
		t.Fatal("registry should start not-ready")
	}
	reg.SetReady(false, "db unavailable")
	if reg.Ready() || reg.LastError() != "db unavailable" {
		t.Fatalf("ready=%v err=%q", reg.Ready(), reg.LastError())
	}
	reg.SetReady(true, "")
	if !reg.Ready() {
		t.Fatal("registry should be ready")
	}
	reg.SetCredentialReady(false, "decrypt_auth_failed")
	if reg.Ready() || !strings.Contains(reg.LastError(), "credential envelope") {
		t.Fatalf("credential failure did not degrade readiness: ready=%v err=%q", reg.Ready(), reg.LastError())
	}
	// A generic health transition must not erase the component failure.
	reg.SetReady(true, "")
	if reg.Ready() {
		t.Fatal("generic SetReady(true) erased credential degradation")
	}
	reg.SetCredentialReady(true, "")
	if !reg.Ready() {
		t.Fatal("successful envelope decrypt did not restore readiness")
	}
}

func TestDispatchSharedTrustMetric(t *testing.T) {
	reg := New(buildinfo.Info{}, "scheduler")
	reg.SetDispatchSharedTrust(true)
	var out strings.Builder
	reg.WritePrometheus(&out)
	if !strings.Contains(out.String(), "cerbix_dispatch_shared_trust 1") {
		t.Fatalf("shared-trust posture metric missing: %s", out.String())
	}
}

// TestResultOutcomeMetrics proves the result-ingest outcome counters render with their
// low-cardinality labels and that empty families are omitted.
func TestResultOutcomeMetrics(t *testing.T) {
	reg := New(buildinfo.Info{}, "all")
	reg.RecordResultQuarantined("future_timestamp")
	reg.RecordResultIgnored("out_of_order")
	reg.RecordResultRejected("missing_timestamp")
	reg.RecordResultClockSkew("push", "future")
	reg.RecordResultMissingRevision()

	var b bytes.Buffer
	reg.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`cerbix_result_quarantined_total{reason="future_timestamp"} 1`,
		`cerbix_result_ignored_total{reason="out_of_order"} 1`,
		`cerbix_result_rejected_total{reason="missing_timestamp"} 1`,
		`cerbix_result_clock_skew_total{origin="push",reason="future"} 1`,
		`cerbix_result_missing_revision_total 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics output missing %q\n%s", want, out)
		}
	}
	// A fresh registry emits none of the result families (empty → omitted).
	var empty bytes.Buffer
	New(buildinfo.Info{}, "all").WritePrometheus(&empty)
	if strings.Contains(empty.String(), "cerbix_result_") {
		t.Fatalf("empty registry should not emit result_* series:\n%s", empty.String())
	}
}

func TestFileProviderMetrics(t *testing.T) {
	r := New(buildinfo.Info{Version: "test"}, "all")
	r.SetFileProviderLeader("platform", true)
	r.RecordFileProviderReconcile("platform", "applied")
	r.RecordFileProviderReconcile("platform", "applied")
	r.RecordFileProviderReconcile("platform", "noop")
	r.SetFileProviderStatus("platform", 0.125, 1700000000, 7, 2, 1)

	var b strings.Builder
	r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`cerbix_file_provider_leader{provider="platform"} 1`,
		`cerbix_file_provider_reconcile_total{provider="platform",outcome="applied"} 2`,
		`cerbix_file_provider_reconcile_total{provider="platform",outcome="noop"} 1`,
		`cerbix_file_provider_reconcile_duration_seconds{provider="platform"} 0.125`,
		`cerbix_file_provider_last_success_timestamp_seconds{provider="platform"} 1700000000`,
		`cerbix_file_provider_managed_monitors{provider="platform"} 7`,
		`cerbix_file_provider_orphaned_monitors{provider="platform"} 2`,
		`cerbix_file_provider_bundle_errors{provider="platform"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q\n%s", want, out)
		}
	}
	// No provider configured → no file-provider series at all.
	var empty strings.Builder
	New(buildinfo.Info{}, "all").WritePrometheus(&empty)
	if strings.Contains(empty.String(), "cerbix_file_provider_") {
		t.Fatal("file-provider metrics must be absent when no provider is configured")
	}
}

// TestFileProviderLeaderZeroRegisteredBeforeElection covers P1#4: a provider registered with
// leader=false (as cli does at startup, before election) already exports a zero leader gauge, so
// the NoLeader alert's `max by(provider)(…leader) == 0` matches a real 0 — not an empty vector —
// after the sole leader disappears.
func TestFileProviderLeaderZeroRegisteredBeforeElection(t *testing.T) {
	r := New(buildinfo.Info{Version: "test"}, "all")
	r.SetFileProviderLeader("platform", false) // pre-election registration, never won leadership
	var b strings.Builder
	r.WritePrometheus(&b)
	if !strings.Contains(b.String(), `cerbix_file_provider_leader{provider="platform"} 0`) {
		t.Fatalf("a configured-but-never-leader provider must export a zero leader gauge\n%s", b.String())
	}
}

// TestSetFileProviderReconcileStatsPreservesCounts covers P1#3: updating duration/last-success/
// errors when counts are unknown must NOT clobber the last-known managed/orphaned gauges.
func TestSetFileProviderReconcileStatsPreservesCounts(t *testing.T) {
	r := New(buildinfo.Info{Version: "test"}, "all")
	r.SetFileProviderStatus("platform", 0.1, 1700000000, 7, 2, 0) // counts known: 7 managed, 2 orphaned
	r.SetFileProviderReconcileStats("platform", 0.2, 0, 3)        // counts unknown later
	var b strings.Builder
	r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`cerbix_file_provider_managed_monitors{provider="platform"} 7`,  // preserved
		`cerbix_file_provider_orphaned_monitors{provider="platform"} 2`, // preserved
		`cerbix_file_provider_reconcile_duration_seconds{provider="platform"} 0.2`,
		`cerbix_file_provider_bundle_errors{provider="platform"} 3`,
		`cerbix_file_provider_last_success_timestamp_seconds{provider="platform"} 1700000000`, // not reset by 0
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q\n%s", want, out)
		}
	}
}

func TestServiceWedgedGatesReadinessAndClearForgetsIt(t *testing.T) {
	reg := New(buildinfo.Info{}, "scheduler")
	reg.SetReady(true, "")
	if !reg.Ready() {
		t.Fatal("baseline not ready")
	}
	reg.SetServiceWedged(true, "service repair ranges parked in error")
	if reg.Ready() {
		t.Fatal("§21: readiness reported healthy while service work is wedged")
	}
	if reg.LastError() != "service repair ranges parked in error" {
		t.Fatalf("wedge reason not surfaced: %q", reg.LastError())
	}
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	if !strings.Contains(out.String(), "cerbix_service_wedged 1") {
		t.Fatalf("wedged gauge missing:\n%s", out.String())
	}
	// /readyz and the exported gauge answer ONE question: a wedged scheduler must not fail
	// its probe while telling Prometheus cerbix_ready 1.
	if !strings.Contains(out.String(), "cerbix_ready 0") {
		t.Fatalf("cerbix_ready disagrees with Ready() while wedged:\n%s", out.String())
	}
	reg.SetServiceWedged(false, "")
	if !reg.Ready() {
		t.Fatal("readiness did not recover after the wedge cleared")
	}

	// Leadership loss forgets BOTH the gauges and the wedge: a deposed scheduler exporting
	// the old leader's verdict makes two scrapes disagree, and a stale wedged=true would
	// hold a standby's /readyz down.
	reg.SetServiceReliabilityStats(ServiceReliabilityStat{RepairPending: 7})
	reg.SetServiceWedged(true, "stale")
	reg.SetServiceFactMaintenance(false, 12345)
	reg.ClearServiceReliabilityStats()
	if !reg.Ready() {
		t.Fatal("a cleared registry still reports the old leader's wedge")
	}
	out.Reset()
	reg.WritePrometheus(&out)
	if strings.Contains(out.String(), "cerbix_service_repair_ranges") || strings.Contains(out.String(), "cerbix_service_wedged") ||
		strings.Contains(out.String(), "cerbix_service_fact_maintenance") {
		t.Fatalf("cleared registry still exports the old leader's service gauges:\n%s", out.String())
	}
}

func TestFactMaintenanceStuckSignal(t *testing.T) {
	// INITIAL failure: the tracking start is the last-success floor, so the age-based alert
	// cannot fire on the first startup blip (last_success=0 would read as "stuck since the
	// epoch").
	first := New(buildinfo.Info{}, "scheduler")
	first.SetServiceFactMaintenance(false, 5000)
	var out0 bytes.Buffer
	first.WritePrometheus(&out0)
	if !strings.Contains(out0.String(), "cerbix_service_fact_maintenance_last_success_timestamp_seconds 5000") {
		t.Fatalf("an initial failure must floor last-success at tracking start, got:\n%s", out0.String())
	}

	reg := New(buildinfo.Info{}, "scheduler")
	reg.SetServiceFactMaintenance(true, 1000)
	reg.SetServiceFactMaintenance(false, 2000) // failure must NOT advance last-success
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	s := out.String()
	if !strings.Contains(s, "cerbix_service_fact_maintenance_failing 1") {
		t.Fatalf("failing gauge missing:\n%s", s)
	}
	if !strings.Contains(s, "cerbix_service_fact_maintenance_last_success_timestamp_seconds 1000") {
		t.Fatalf("last-success must hold the last SUCCESS, not the failure time:\n%s", s)
	}
}

func TestServiceReliabilityGauges(t *testing.T) {
	reg := New(buildinfo.Info{}, "scheduler")
	var before bytes.Buffer
	reg.WritePrometheus(&before)
	if strings.Contains(before.String(), "cerbix_service_repair_ranges") {
		t.Fatal("service gauges should be absent until sampled")
	}
	reg.SetServiceReliabilityStats(ServiceReliabilityStat{
		RepairPending: 3, RepairRunning: 1, RepairErrored: 2, WatermarkLagSeconds: 90.25,
		EpochFanoutTotal: 2, LateArrivalsTotal: 3, LateOverflowTotal: 1,
	})
	reg.RecordServiceSlice("worked")
	reg.RecordServiceSlice("worked")
	reg.RecordServiceSlice("error")
	reg.RecordServiceRepairOutcome("complete", "maintenance")
	reg.RecordServiceRepairOutcome("unrecomputable", "admin")
	reg.RecordServiceUnrecomputableRejection()
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	s := out.String()
	for _, want := range []string{
		`cerbix_service_repair_ranges{state="pending"} 3`,
		`cerbix_service_repair_ranges{state="running"} 1`,
		`cerbix_service_repair_ranges{state="error"} 2`,
		`cerbix_service_watermark_lag_seconds 90.250`,
		`cerbix_service_slices_total{outcome="worked"} 2`,
		`cerbix_service_slices_total{outcome="error"} 1`,
		`cerbix_service_epoch_fanout_total 2`,
		`cerbix_service_repair_outcomes_total{outcome="complete",reason="maintenance"} 1`,
		`cerbix_service_repair_outcomes_total{outcome="unrecomputable",reason="admin"} 1`,
		`cerbix_service_unrecomputable_rejections_total 1`,
		`cerbix_service_late_arrivals_total 3`,
		`cerbix_service_late_arrival_overflow_total 1`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}
