package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestProbeErrorIsDiagnosticOnlyAndRevisionFenced(t *testing.T) {
	st, ctx := outboxTestStore(t)
	_, projectID := secretsFixture(t, st, ctx, "probe-error-org", "probe-error-project")
	monitor, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: "probe-error", Type: domain.MonitorHTTP,
		Target: "https://example.com/health", Method: "GET", IntervalSeconds: 60,
		TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := st.RecordProbeError(ctx, monitor.ID, monitor.ExecutionRevision, domain.ProbeError{
		Reason: domain.ProbeErrorDecryptAuthFailed, JobID: "job-1",
	})
	if err != nil || !result.Recorded {
		t.Fatalf("record probe error: result=%+v err=%v", result, err)
	}
	assertProbeDiagnosticOnly(t, st, ctx, monitor.ID, domain.ProbeErrorDecryptAuthFailed, "job-1", domain.StatusPending, 0)

	stale, err := st.RecordProbeError(ctx, monitor.ID, monitor.ExecutionRevision+1, domain.ProbeError{
		Reason: domain.ProbeErrorUnknownKeyID, JobID: "stale-job",
	})
	if err != nil || stale.Recorded || stale.Reason != ReasonStaleRevision {
		t.Fatalf("stale outcome=%+v err=%v", stale, err)
	}
	assertProbeDiagnosticOnly(t, st, ctx, monitor.ID, domain.ProbeErrorDecryptAuthFailed, "job-1", domain.StatusPending, 0)
}

func TestProbeErrorClearsOnlyOnLiveAppliedNormalResult(t *testing.T) {
	st, ctx := outboxTestStore(t)
	_, projectID := secretsFixture(t, st, ctx, "probe-clear-org", "probe-clear-project")
	monitor, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: "probe-clear", Type: domain.MonitorHTTP,
		Target: "https://example.com/health", Method: "GET", IntervalSeconds: 60,
		TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	if outcome, err := st.RecordScheduledResult(ctx, domain.Heartbeat{MonitorID: monitor.ID, ExecutionRevision: monitor.ExecutionRevision, Ts: base, Up: true}); err != nil || !outcome.Applied {
		t.Fatalf("seed heartbeat: outcome=%+v err=%v", outcome, err)
	}
	if _, err := st.RecordProbeError(ctx, monitor.ID, monitor.ExecutionRevision, domain.ProbeError{Reason: domain.ProbeErrorUnknownKeyID, JobID: "job-new"}); err != nil {
		t.Fatal(err)
	}

	// An older SLA-only result is valid but not live-applied and must not clear the error.
	old := base.Add(-time.Second)
	if outcome, err := st.RecordScheduledResult(ctx, domain.Heartbeat{MonitorID: monitor.ID, ExecutionRevision: monitor.ExecutionRevision, Ts: old, Up: false}); err != nil || outcome.Reason != ReasonOutOfOrder {
		t.Fatalf("out-of-order heartbeat: outcome=%+v err=%v", outcome, err)
	}
	assertProbeDiagnosticOnly(t, st, ctx, monitor.ID, domain.ProbeErrorUnknownKeyID, "job-new", domain.StatusUp, 2)

	// A revision-valid, live-applied UP or DOWN clears atomically at ingest.
	fresh := time.Now().UTC()
	if outcome, err := st.RecordScheduledResult(ctx, domain.Heartbeat{MonitorID: monitor.ID, ExecutionRevision: monitor.ExecutionRevision, Ts: fresh, Up: false}); err != nil || !outcome.Applied {
		t.Fatalf("fresh heartbeat: outcome=%+v err=%v", outcome, err)
	}
	got, err := st.GetMonitor(ctx, monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastProbeErrorReason != "" || got.LastProbeErrorAt != nil || got.LastProbeErrorJobID != "" {
		t.Fatalf("probe diagnostic not cleared: %+v", got)
	}
}

func assertProbeDiagnosticOnly(t *testing.T, st *Store, ctx context.Context, monitorID, reason, jobID string, status domain.MonitorStatus, heartbeatCount int) {
	t.Helper()
	got, err := st.GetMonitor(ctx, monitorID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastProbeErrorReason != reason || got.LastProbeErrorAt == nil || got.LastProbeErrorJobID != jobID {
		t.Fatalf("diagnostic = reason=%q at=%v job=%q", got.LastProbeErrorReason, got.LastProbeErrorAt, got.LastProbeErrorJobID)
	}
	if got.Status != status || got.ConsecutiveFailures != 0 {
		t.Fatalf("probe error mutated liveness: status=%s failures=%d", got.Status, got.ConsecutiveFailures)
	}
	var gotHeartbeatCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id=$1`, monitorID).Scan(&gotHeartbeatCount); err != nil {
		t.Fatal(err)
	}
	if gotHeartbeatCount != heartbeatCount {
		t.Fatalf("heartbeat count = %d, want unchanged %d", gotHeartbeatCount, heartbeatCount)
	}
}
