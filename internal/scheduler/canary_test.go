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
	// Same wiring role=all uses: the executor is in this process, so the region announces the
	// workflow this binary runs. Without it the capability gate refuses before any of these
	// assertions can be reached — which is itself asserted, separately, below.
	s := New(fs, disp, testLogger()).WithLocalCanaryRegions("core")

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
	// Same wiring role=all uses: the executor is in this process, so the region announces the
	// workflow this binary runs. Without it the capability gate refuses before any of these
	// assertions can be reached — which is itself asserted, separately, below.
	s := New(fs, disp, testLogger()).WithLocalCanaryRegions("core")

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
	// Same wiring role=all uses: the executor is in this process, so the region announces the
	// workflow this binary runs. Without it the capability gate refuses before any of these
	// assertions can be reached — which is itself asserted, separately, below.
	s := New(fs, disp, testLogger()).WithLocalCanaryRegions("core")

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
	// Same wiring role=all uses: the executor is in this process, so the region announces the
	// workflow this binary runs. Without it the capability gate refuses before any of these
	// assertions can be reached — which is itself asserted, separately, below.
	s := New(fs, disp, testLogger()).WithLocalCanaryRegions("core")

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

// B2 — a shortage goes through the RESULT pipeline, carries no run key, and is not a bare row.
//
// The shortage used to write through `InsertHeartbeat`, which touches no status, no confirmation
// counter, no transition outbox and no service bucket. A canary whose region had no capable
// executor therefore stayed green while nothing was probing it: no alert, no incident, no
// escalation, and the service SLI kept reporting a number computed over a monitor that had stopped
// being measured. A heartbeat ROW existed in both the broken and the fixed version, which is why
// this was invisible — so the assertion here is on the DOOR, and the flip itself is asserted in the
// store, against a database.
//
// The mutation that must kill this: send the shortage through the bare insert again. It cannot even
// compile, because `InsertHeartbeat` is no longer on the scheduler's store interface — which is the
// point of removing it rather than leaving it beside the new call.
func TestACanaryShortageIsSubmittedToTheResultPipeline(t *testing.T) {
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{canaryScheduleMonitor()}}
	disp := dispatch.NewInProc(8)
	// No local canary region and no announcement: the region has no capable runner at all, which is
	// the shortage reason the item is named for. The saturation reason is covered above; this
	// asserts the rule applies to EVERY shortage reason and not only to the one that was reported.
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

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
			t.Fatal("a region with no capable runner produced no result at all: the monitor would " +
				"stay green forever while nothing probed it")
		case <-time.After(20 * time.Millisecond):
		}
	}
	fs.mu.Lock()
	beats := append([]domain.Heartbeat(nil), fs.canaryHeartbeats...)
	claims := len(fs.canaryClaims)
	fs.mu.Unlock()

	if !strings.Contains(beats[0].Msg, "no_capable_runner") {
		t.Errorf("the result reads %q, want the bounded shortage reason", beats[0].Msg)
	}
	if beats[0].Up {
		t.Error("the shortage was recorded as UP")
	}
	if beats[0].Ts.IsZero() {
		t.Error("the result carries no timestamp; `RecordScheduledResult` fails closed on one and " +
			"the shortage would be silently dropped")
	}
	if beats[0].ExecutionRevision != 3 {
		t.Errorf("the result carries revision %d, want the monitor's 3 — the revision gate refuses "+
			"anything else, exactly as it does for an executor's result", beats[0].ExecutionRevision)
	}
	// A shortage never took an in-flight slot, so it must not carry the key that RELEASES one: the
	// pipeline deletes the lease keyed by (monitor, run), and a shortage reporting a run key would
	// delete the lease a different run is holding.
	if beats[0].CanaryRunKey != "" {
		t.Errorf("the shortage carries run key %q; it claimed no slot and must release none",
			beats[0].CanaryRunKey)
	}
	// The capability gate runs BEFORE the in-flight claim, so an incapable region never consumes a
	// slot it can never release.
	if claims != 0 {
		t.Errorf("%d in-flight slots were claimed for a region with no runner", claims)
	}
	select {
	case delivered := <-disp.Jobs():
		t.Fatalf("a region with no capable runner received the job: %s", delivered.Job.Monitor.ID)
	default:
	}
}

// B5 — `role=all` runs a canary in EVERY non-pull region, and announces exactly that.
//
// The announcement was `WithLocalCanaryRegions(core)` and nothing else, while the in-process
// dispatcher ignores the region entirely and executes whatever job it is handed. A canary declared
// in any other non-pull region was therefore refused with `no_capable_runner` on every tick,
// forever — while an ordinary monitor of that same region was probed normally by the same process.
// The region field meant one thing for canaries and another for every other type.
//
// The mutation that must kill this: announce a LIST again. A list has to be kept in step with the
// regions people create, and this test declares a region no list mentions.
func TestRoleAllRunsACanaryInAnyNonPullRegion(t *testing.T) {
	m := canaryScheduleMonitor()
	m.Region = "geo-frankfurt" // named nowhere, and not pull-served
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m}}
	disp := dispatch.NewInProc(8)
	// Exactly role=all's wiring: the default region is the only one anyone enumerated.
	s := New(fs, disp, testLogger()).WithLocalCanaryRegions("core").WithLocalCanaryAnyRegion()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case delivered := <-disp.Jobs():
		if delivered.Job.Monitor.Region != "geo-frankfurt" {
			t.Fatalf("published a job for region %q, want geo-frankfurt", delivered.Job.Monitor.Region)
		}
	case <-time.After(3 * time.Second):
		fs.mu.Lock()
		beats := append([]domain.Heartbeat(nil), fs.canaryHeartbeats...)
		fs.mu.Unlock()
		t.Fatalf("the canary was never dispatched; shortages recorded: %+v", beats)
	}
}

// And the exclusion the announcement rests on is unchanged: a PULL-served region is executed by its
// agents, never by this process, so the in-process runner is no evidence about it. Announcing it
// would enqueue a capability-bound row no legacy agent can claim and leave the monitor silent until
// the row's TTL — the indefinite pending invariant 6 forbids.
func TestRoleAllStillAnnouncesNothingForAPullServedRegion(t *testing.T) {
	m := canaryScheduleMonitor()
	m.Region = "geo-pull"
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m}}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger()).WithLocalCanaryAnyRegion().WithPullRegions([]string{"geo-pull"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

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
			t.Fatal("a pull-served region with no agent produced no shortage: the job was dispatched " +
				"into a region this process does not execute")
		case <-time.After(20 * time.Millisecond):
		}
	}
	fs.mu.Lock()
	beats := append([]domain.Heartbeat(nil), fs.canaryHeartbeats...)
	fs.mu.Unlock()
	if !strings.Contains(beats[0].Msg, "no_capable_runner") {
		t.Errorf("the shortage reads %q, want no_capable_runner for a pull region with no agent", beats[0].Msg)
	}
}
