package metrics

import (
	"bytes"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

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
}
