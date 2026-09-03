package scheduler

import (
	"context"
	"os"
	"strconv"
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

// The scheduler's copy of the agent endpoint's default must not drift from the endpoint's own: the
// two live in packages that deliberately do not import each other, so the agreement is asserted
// rather than assumed.
func TestThePullLeaseDefaultMatchesTheAgentEndpoint(t *testing.T) {
	src, err := os.ReadFile("../api/handlers_agent.go")
	if err != nil {
		t.Fatalf("read the agent endpoint: %v", err)
	}
	want := "pullJobLeaseSeconds = " + strconv.Itoa(pullLeaseDefaultSeconds)
	if !strings.Contains(string(src), want) {
		t.Fatalf("the agent endpoint no longer declares %q — the scheduler's copy has drifted", want)
	}
}

// A monitor whose probe fits the default asks for no lease of its own; one that outlives it asks for
// its own, and that is written against the TIMEOUT rather than the type because the defect predates
// the canary.
func TestPullLeaseIsAskedForOnlyWhenTheProbeOutlivesTheDefault(t *testing.T) {
	short := domain.Monitor{Type: domain.MonitorHTTP, TimeoutSeconds: 10}
	if got := pullLeaseFor(short); got != 0 {
		t.Fatalf("a 10s probe asked for a %ds lease, want the endpoint's default", got)
	}
	long := domain.Monitor{Type: domain.MonitorHTTP, TimeoutSeconds: 120}
	if got := pullLeaseFor(long); got <= 120 {
		t.Fatalf("a 120s probe asked for %ds, want its own budget plus slack", got)
	}
	canary := domain.Monitor{Type: domain.MonitorAsyncCanary, TimeoutSeconds: 300}
	if got := pullLeaseFor(canary); got < 300 {
		t.Fatalf("a canary asked for %ds, want at least its journey", got)
	}
}

// P0-2: a claim that is NOT followed by a delivery must give the slot back.
//
// Every branch between the claim and a successful publish is a branch where no external journey
// started, and none of them used to release. Four transient publish failures therefore consumed a
// region's whole cap until `timeout + 60s`, and every canary there reported a false
// `region_saturated` DOWN for a broker fault that had nothing to do with saturation.
func TestACanaryDispatchThatNeverPublishedGivesTheSlotBack(t *testing.T) {
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{canaryScheduleMonitor()}}
	disp := &failingDispatcher{} // never heals: every publish fails
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		fs.mu.Lock()
		released := len(fs.canaryReleases)
		fs.mu.Unlock()
		if released > 0 {
			break
		}
		select {
		case <-deadline:
			fs.mu.Lock()
			claims := len(fs.canaryClaims)
			fs.mu.Unlock()
			t.Fatalf("the slot was claimed %d time(s) and never released after a failed publish", claims)
		case <-time.After(20 * time.Millisecond):
		}
	}

	fs.mu.Lock()
	claims := append([]string(nil), fs.canaryClaims...)
	releases := append([]string(nil), fs.canaryReleases...)
	beats := len(fs.canaryHeartbeats)
	fs.mu.Unlock()

	if len(claims) == 0 {
		t.Fatal("no claim was made at all")
	}
	// Released for the SAME run that claimed it: a release keyed by anything else frees nothing.
	wantRun := canaryScheduleMonitor().Config[domain.CanaryRunKey]
	if !strings.HasSuffix(releases[0], "|"+wantRun) {
		t.Fatalf("release = %q, want it keyed by run %q", releases[0], wantRun)
	}
	if !strings.HasSuffix(claims[0], "|"+wantRun) {
		t.Fatalf("claim = %q, want it keyed by run %q", claims[0], wantRun)
	}
	// A failed publish is a transport fault, not a saturation report: no DOWN heartbeat is written
	// for it, because the monitor is not down — cerbix could not ask.
	if beats != 0 {
		t.Fatalf("a failed publish wrote %d shortage heartbeat(s); that reason belongs to a REFUSED claim", beats)
	}
}

// The plain dispatch path — a canary with no bindings — never reaches the credential materializer,
// which is where the run key used to be stamped. It therefore claimed its slot with an EMPTY run
// key, and a slot keyed by nothing can never be released by key.
func TestABindinglessCanaryStillCarriesItsRun(t *testing.T) {
	m := canaryScheduleMonitor()
	delete(m.Config, domain.CanaryRunKey) // as the snapshot delivers it on the plain path
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m}}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case delivered := <-disp.Jobs():
		if got := delivered.Job.Monitor.Config[domain.CanaryRunKey]; got == "" {
			t.Fatal("the dispatched job carries no run key, so its result could never release the slot")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the canary was never dispatched")
	}

	fs.mu.Lock()
	claims := append([]string(nil), fs.canaryClaims...)
	fs.mu.Unlock()
	if len(claims) == 0 {
		t.Fatal("no claim was made")
	}
	if strings.HasSuffix(claims[0], "|") {
		t.Fatalf("claim = %q — the slot was taken with an empty run key", claims[0])
	}
	// And the snapshot's own map was not written into: it is shared with every other reader.
	if _, leaked := m.Config[domain.CanaryRunKey]; leaked {
		t.Fatal("the run key was written into the SHARED snapshot config instead of a copy")
	}
}
