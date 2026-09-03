package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-029 D9 / D9a at the dispatch decision. What matters here is not that the scheduler skips a
// saturated run — it is that the run becomes an ORDINARY monitor outcome: one DOWN heartbeat with a
// bounded reason, so confirmations, alerting and the SLI treat it like any other failure.
func canaryScheduleMonitor() domain.Monitor {
	w := domain.CanaryWorkflow{
		Kind:    domain.CanaryWorkflowKind,
		Secrets: map[string]string{},
		Submit: domain.CanarySubmit{
			Kind: domain.CanarySubmitHTTPJSON, Method: "POST",
			URL: "https://files.example.com/upload", SubmitTimeout: 10,
			AcceptedStatus: []int{202},
			Body:           map[string]domain.CanaryValue{"tenant": {Kind: domain.CanaryValueString, Str: "canary"}},
		},
		Correlate: domain.CanaryCorrelate{Source: domain.CanaryCorrelateResponseJSON, Path: "task_id"},
		Completion: domain.CanaryCompletion{
			Kind: domain.CanaryCompletionPollJSON, URL: "https://files.example.com/t/{{ correlation_id }}",
			Timeout: 60,
			Poll: &domain.CanaryPoll{Interval: 5, MaxAttempts: 10,
				Success: domain.CanaryPollMatch{Path: "status", Value: "completed"}},
		},
		Result:  domain.CanaryResult{MaxLatency: 60, RequiredJSONFields: []string{"s3_path"}, LifecyclePath: "s3_path"},
		Cleanup: domain.CanaryCleanup{Kind: domain.CanaryCleanupNone, Acknowledged: true},
	}
	cfg, _ := domain.CanaryConfig(w)
	cfg[domain.CanaryRunKey] = "5961333"
	return domain.Monitor{
		ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProjectID: "p1", Name: "canary",
		Type: domain.MonitorAsyncCanary, Region: "core", IntervalSeconds: 300, TimeoutSeconds: 300,
		Enabled: true, ExecutionRevision: 3, Config: cfg,
	}
}

func TestASaturatedCanaryRunReportsDownRatherThanQueueing(t *testing.T) {
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{canaryScheduleMonitor()}}
	fs.canaryClaimErr = store.ErrCanaryRegionSaturated
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// The refusal is what this asserts, so wait for the heartbeat rather than for a job that must
	// never arrive.
	deadline := time.After(3 * time.Second)
	for {
		fs.mu.Lock()
		got := len(fs.canaryHeartbeats)
		fs.mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no DOWN heartbeat was written for the refused run")
		case <-time.After(20 * time.Millisecond):
		}
	}

	fs.mu.Lock()
	claims, beats := len(fs.canaryClaims), append([]domain.Heartbeat(nil), fs.canaryHeartbeats...)
	fs.mu.Unlock()

	if claims == 0 {
		t.Fatal("the scheduler must ask for the in-flight slot before dispatching a canary")
	}
	select {
	case delivered := <-disp.Jobs():
		t.Fatalf("a saturated region must not receive the job, got %s", delivered.Job.Monitor.ID)
	default:
	}
	if len(beats) != 1 {
		t.Fatalf("heartbeats = %d, want exactly one DOWN for the refused run", len(beats))
	}
	if beats[0].Up || !strings.Contains(beats[0].Msg, "region_saturated") {
		t.Fatalf("heartbeat = %+v, want DOWN with the bounded reason", beats[0])
	}
	if beats[0].ExecutionRevision != 3 {
		t.Fatalf("the heartbeat must carry the monitor's revision, got %d", beats[0].ExecutionRevision)
	}
}

func TestAnUnsaturatedCanaryRunIsDispatchedNormally(t *testing.T) {
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{canaryScheduleMonitor()}}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case delivered := <-disp.Jobs():
		if delivered.Job.Monitor.Type != domain.MonitorAsyncCanary {
			t.Fatalf("published %s, want the canary", delivered.Job.Monitor.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the canary was never dispatched")
	}

	fs.mu.Lock()
	claims := append([]string(nil), fs.canaryClaims...)
	beats := len(fs.canaryHeartbeats)
	fs.mu.Unlock()

	if len(claims) == 0 || !strings.Contains(claims[0], "core|5961333") {
		t.Fatalf("claims = %v, want one carrying the region and the RUN key", claims)
	}
	if beats != 0 {
		t.Fatalf("a healthy dispatch must not fabricate a heartbeat, got %d", beats)
	}
}
