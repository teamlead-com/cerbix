package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/store"
)

type fakeStore struct {
	serviceSlices int32
	monitors      []domain.Monitor
	stalePush     []domain.Monitor
	deadmanCh     chan string // receives monitorID on each RecordDeadmanResult call
	leader        bool
	elections     int32
	ensured       int32
	factEnsured   int32
	purged        int32
	sessionPurges int32
	flowPurges    int32
	listCalls     int32 // ListEnabledMonitors invocations (snapshot reloads)
	// checkHeld, when set, is what the leadership watchdog check() returns; nil
	// means "still leader" (true, nil).
	checkHeld func() (bool, error)
	// statsFn, when set, backs ServiceReliabilityStats — it must honor ctx like the real
	// query does.
	statsFn     func(context.Context) (metrics.ServiceReliabilityStat, error)
	materialize func([]string) ([]store.MaterializedExecution, error)
	// alertCadence / burnCadence record the cadence each service-alert arm was called with, and
	// the counters record whether it was called at all. Both matter: the cadence IS the freshness
	// lease's basis (§16.5a), so a leader that evaluates on one number and arms on another either
	// dis-arms a healthy evaluator or keeps coverage armed after it stopped.
	alertCalls, burnCalls   int32
	alertCadence, burnCadns int64 // nanoseconds, atomically stored
	// What each arm reports back, and the failure it reports instead. Written before Run and read
	// from the leader loop, like every other field of this fake.
	alertEval store.ServiceAlertEvaluation
	burnEval  store.ServiceBurnEvaluation
	alertErr  error
	burnErr   error
	// alertStats backs ServiceAlertStats (FR-021 §16.6b); alertStatsErr fails the sample.
	alertStats     metrics.ServiceAlertStat
	alertStatsErr  error
	escalationPass store.EscalationPass
	// gatePasses counts RunGateLedgerMaintenancePass calls; gatePassFn, when set, is its body.
	gatePasses int32
	gatePassFn func(ctx context.Context, passStart time.Time, cfg store.GateMaintenanceConfig) (store.GateMaintenanceReport, bool, error)
	// changePurges counts PurgeChangeGroups calls; changePurgeFn, when set, scripts each batch.
	changePurges  int32
	changePurgeFn func(ctx context.Context, cutoff time.Time, groupsPerBatch int) (int, int, error)
}

type staticCredentialRegions map[string]bool

func (s staticCredentialRegions) LiveCredentialV3JobRegions(context.Context) (map[string]bool, error) {
	// No generation-3 consumers by default: the emitter must stay on generation 2 unless a
	// test says otherwise.
	return map[string]bool{}, nil
}

func (s staticCredentialRegions) LiveCredentialJobRegions(context.Context) (map[string]bool, error) {
	return s, nil
}

func (f *fakeStore) ListEnabledMonitors(context.Context) ([]domain.Monitor, error) {
	atomic.AddInt32(&f.listCalls, 1)
	return f.monitors, nil
}

func (f *fakeStore) ListEnabledMonitorSnapshots(ctx context.Context) ([]domain.Monitor, error) {
	return f.ListEnabledMonitors(ctx)
}

func (f *fakeStore) MaterializeExecutionConfigs(_ context.Context, ids []string, _ map[string]int) ([]store.MaterializedExecution, error) {
	if f.materialize != nil {
		return f.materialize(ids)
	}
	byID := make(map[string]domain.Monitor, len(f.monitors))
	for _, m := range f.monitors {
		byID[m.ID] = m
	}
	out := make([]store.MaterializedExecution, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok || !m.Enabled {
			out = append(out, store.MaterializedExecution{MonitorID: id, Reason: store.MaterializeSkippedCurrentState})
			continue
		}
		out = append(out, store.MaterializedExecution{MonitorID: id, Job: dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV2}})
	}
	return out, nil
}

func TestCredentialFailureRetryHasIntervalFloorAndResyncCap(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		failures int
		want     time.Duration
	}{
		{name: "floor", interval: 2 * time.Second, failures: 1, want: 2 * time.Second},
		{name: "exponential", interval: 2 * time.Second, failures: 3, want: 8 * time.Second},
		{name: "resync cap", interval: 2 * time.Second, failures: 8, want: refreshEvery},
		{name: "slow monitor remains floor", interval: time.Minute, failures: 8, want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := credentialFailureRetry(tt.interval, tt.failures); got != tt.want {
				t.Fatalf("retry = %s, want %s", got, tt.want)
			}
		})
	}
}

func (f *fakeStore) StalePushMonitors(context.Context) ([]domain.Monitor, error) {
	return f.stalePush, nil
}
func (f *fakeStore) RecordDeadmanResult(_ context.Context, monitorID string, _ int64, _ time.Time) (store.ResultOutcome, error) {
	if f.deadmanCh != nil {
		select {
		case f.deadmanCh <- monitorID:
		default:
		}
	}
	return store.ResultOutcome{Applied: true, Prev: domain.StatusUp, Cur: domain.StatusDown}, nil
}

// fakeLeaderSession stands in for the pinned-connection session. Its RunServiceSlice
// reports "nothing to do", which is what an installation with no services looks like — and
// what the scheduler tests are about.
type fakeLeaderSession struct{ owner *fakeStore }

func (s fakeLeaderSession) Check(context.Context) (bool, error) {
	if s.owner.checkHeld != nil {
		return s.owner.checkHeld()
	}
	return true, nil
}
func (s fakeLeaderSession) Release() {}
func (s fakeLeaderSession) RunServiceSlice(context.Context, time.Time) (bool, error) {
	atomic.AddInt32(&s.owner.serviceSlices, 1)
	return false, nil
}

// The two alert arms hang off the SESSION, not the store: that is where the real ones live, so a
// scheduler that went back to calling pool-backed evaluators would not compile.
func (s fakeLeaderSession) EvaluateServiceAlerts(_ context.Context, cadence time.Duration) (store.ServiceAlertEvaluation, error) {
	atomic.AddInt32(&s.owner.alertCalls, 1)
	atomic.StoreInt64(&s.owner.alertCadence, int64(cadence))
	if s.owner.alertErr != nil {
		return store.ServiceAlertEvaluation{}, s.owner.alertErr
	}
	return s.owner.alertEval, nil
}

func (s fakeLeaderSession) EvaluateServiceBurnAlerts(_ context.Context, cadence time.Duration) (store.ServiceBurnEvaluation, error) {
	atomic.AddInt32(&s.owner.burnCalls, 1)
	atomic.StoreInt64(&s.owner.burnCadns, int64(cadence))
	if s.owner.burnErr != nil {
		return store.ServiceBurnEvaluation{}, s.owner.burnErr
	}
	return s.owner.burnEval, nil
}

func (f *fakeStore) TryBecomeLeaderSession(_ context.Context, _ int64) (LeaderSession, bool, error) {
	atomic.AddInt32(&f.elections, 1)
	if !f.leader {
		return nil, false, nil
	}
	return fakeLeaderSession{owner: f}, true, nil
}

func (f *fakeStore) RollupDailyAvailability(_ context.Context, _, _ time.Time) error { return nil }

func (f *fakeStore) EnsureHeartbeatPartitions(_ context.Context, _ int) error {
	atomic.AddInt32(&f.ensured, 1)
	return nil
}
func (f *fakeStore) EnsureServiceFactPartitions(ctx context.Context, aheadMonths int) error {
	atomic.AddInt32(&f.factEnsured, 1)
	return nil
}

// RunGateLedgerMaintenancePass is the FR-024 D10 pass. gatePassFn, when set, is the pass body
// (a test blocks it to prove dispatch keeps ticking); otherwise the pass reports "not acquired".
func (f *fakeStore) RunGateLedgerMaintenancePass(ctx context.Context, passStart time.Time, cfg store.GateMaintenanceConfig,
	clock func() time.Time, metrics store.GateMaintenanceMetrics) (store.GateMaintenanceReport, bool, error) {
	atomic.AddInt32(&f.gatePasses, 1)
	if f.gatePassFn != nil {
		return f.gatePassFn(ctx, passStart, cfg)
	}
	return store.GateMaintenanceReport{}, false, nil
}

func (f *fakeStore) ServiceReliabilityStats(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
	if f.statsFn != nil {
		return f.statsFn(ctx)
	}
	return metrics.ServiceReliabilityStat{}, nil
}

func (f *fakeStore) ServiceAlertStats(context.Context) (metrics.ServiceAlertStat, error) {
	if f.alertStatsErr != nil {
		return metrics.ServiceAlertStat{}, f.alertStatsErr
	}
	return f.alertStats, nil
}

func (f *fakeStore) PurgeOldHeartbeats(_ context.Context, _ time.Time) (int, error) {
	atomic.AddInt32(&f.purged, 1)
	return 0, nil
}

// PurgeChangeGroups is the FR-025 D9 pass. changePurgeFn, when set, scripts each batch's
// answer (a test proves the leader repeats until a batch selects fewer than the bound);
// otherwise the fake reports an empty batch.
func (f *fakeStore) PurgeChangeGroups(ctx context.Context, cutoff time.Time, groupsPerBatch int) (int, int, error) {
	atomic.AddInt32(&f.changePurges, 1)
	if f.changePurgeFn != nil {
		return f.changePurgeFn(ctx, cutoff, groupsPerBatch)
	}
	return 0, 0, nil
}

func (f *fakeStore) EnqueueRenotifyReminders(context.Context) (int, error) { return 0, nil }

func (f *fakeStore) EvaluateBurnAlerts(context.Context) (int, int, error) { return 0, 0, nil }
func (f *fakeStore) EnqueueDueSLAReports(context.Context) (int, error)    { return 0, nil }
func (f *fakeStore) EvaluateRegionWorkerAlerts(context.Context, map[string]bool, int) (int, int, error) {
	return 0, 0, nil
}

// escalationPass is what this fake's ladder "did"; a test that cares sets it.
func (f *fakeStore) AdvanceEscalations(context.Context) (store.EscalationPass, error) {
	return f.escalationPass, nil
}
func (f *fakeStore) EnqueuePullJob(context.Context, string, []byte, int) error   { return nil }
func (f *fakeStore) EnqueuePullJobV2(context.Context, string, []byte, int) error { return nil }
func (f *fakeStore) EnqueuePullJobV3(context.Context, string, []byte, int) error { return nil }
func (f *fakeStore) LiveCredentialReadyAgentRegions(context.Context, time.Duration, int) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (f *fakeStore) PurgeExpiredPullJobs(context.Context) (int, error) { return 0, nil }
func (f *fakeStore) PurgeDeliveredOutbox(context.Context, time.Duration) (int, error) {
	return 0, nil
}
func (f *fakeStore) PurgeExpiredPullTests(context.Context) (int, error) { return 0, nil }
func (f *fakeStore) PurgeStaleAgentHeartbeats(context.Context, time.Duration) (int, error) {
	return 0, nil
}
func (f *fakeStore) DeleteExpiredSessions(context.Context) (int64, error) {
	atomic.AddInt32(&f.sessionPurges, 1)
	return 0, nil
}
func (f *fakeStore) DeleteExpiredAuthFlows(context.Context) (int64, error) {
	atomic.AddInt32(&f.flowPurges, 1)
	return 0, nil
}
func (f *fakeStore) PullQueueStats(context.Context) ([]metrics.PullStat, error) {
	return nil, nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSchedulerLeaderPublishesDueJobs(t *testing.T) {
	fs := &fakeStore{
		leader: true,
		monitors: []domain.Monitor{
			{ID: "m1", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true},
			// Passive push monitor, fresh (created now) → not marked down, never a job.
			{ID: "push1", Type: domain.MonitorPush, IntervalSeconds: 3600, Enabled: true, Status: domain.StatusUp, CreatedAt: time.Now()},
		},
	}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case delivered := <-disp.Jobs():
		if delivered.Job.Monitor.ID != "m1" {
			t.Fatalf("expected active monitor m1, got %q", delivered.Job.Monitor.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not publish a job in time")
	}
}

func TestSchedulerLeaderMaintainsPartitions(t *testing.T) {
	fs := &fakeStore{leader: true}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger()).WithRetentionDays(30)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		if atomic.LoadInt32(&fs.ensured) >= 1 && atomic.LoadInt32(&fs.factEnsured) >= 1 &&
			atomic.LoadInt32(&fs.purged) >= 1 &&
			atomic.LoadInt32(&fs.sessionPurges) >= 1 && atomic.LoadInt32(&fs.flowPurges) >= 1 {
			return // leader ran partition maintenance (heartbeats AND facts) + retention + auth housekeeping
		}
		select {
		case <-deadline:
			t.Fatalf("leader did not run maintenance: ensured=%d factEnsured=%d purged=%d sessions=%d flows=%d",
				atomic.LoadInt32(&fs.ensured), atomic.LoadInt32(&fs.factEnsured), atomic.LoadInt32(&fs.purged),
				atomic.LoadInt32(&fs.sessionPurges), atomic.LoadInt32(&fs.flowPurges))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWithRetentionDaysClampsLow(t *testing.T) {
	s := New(&fakeStore{}, dispatch.NewInProc(1), testLogger()).WithRetentionDays(0)
	if s.retentionDays != 2 {
		t.Fatalf("retentionDays = %d, want clamped to 2", s.retentionDays)
	}
}

func TestSchedulerPushLiveness(t *testing.T) {
	// The batched staleness query returns a tripped push monitor; the leader applies a DOWN
	// directly via RecordDeadmanResult (the atomic staleness re-check + which selection is
	// stale are the store's job, covered by the DB-gated tests) rather than publishing a
	// synthetic result through the dispatcher.
	fs := &fakeStore{
		leader:    true,
		deadmanCh: make(chan string, 1),
		stalePush: []domain.Monitor{
			{ID: "stale", Type: domain.MonitorPush, IntervalSeconds: 1, Enabled: true, Status: domain.StatusUp, CreatedAt: time.Now().Add(-time.Hour)},
		},
	}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case id := <-fs.deadmanCh:
		if id != "stale" {
			t.Fatalf("expected dead-man DOWN for the stale monitor, got %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not apply the dead-man DOWN for the stale push monitor")
	}
}

// TestConfirmSignalAcceleratesProbe proves the confirm-phase fast path: a
// monitor on a long interval gets its next probe rescheduled to the confirm
// interval when a failure signal arrives, instead of waiting out the interval.
func TestConfirmSignalAcceleratesProbe(t *testing.T) {
	mon := domain.Monitor{
		ID: "m1", Type: domain.MonitorTCP, Target: "10.0.0.1:80", Enabled: true,
		IntervalSeconds: 3600, TimeoutSeconds: 5,
		FailureThreshold: 3, ConfirmIntervalSeconds: 1, Status: domain.StatusUp,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{mon}}
	disp := dispatch.NewInProc(8)
	ch := make(chan string, 1)
	s := New(fs, disp, testLogger()).WithConfirmSignals(ch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// First probe publishes on the normal path.
	select {
	case <-disp.Jobs():
	case <-time.After(3 * time.Second):
		t.Fatal("initial job not published")
	}
	// Without a signal the next probe is an hour away; the failure signal must
	// pull it in to ~1s (the confirm interval).
	ch <- "m1"
	select {
	case delivered := <-disp.Jobs():
		if delivered.Job.Monitor.ID != "m1" {
			t.Fatalf("unexpected job %q", delivered.Job.Monitor.ID)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("confirm signal did not accelerate the next probe")
	}
}

// TestConfigSignalForcesSnapshotReload proves the §12 config-change wake path: a signal on the
// config channel (store.ConfigNotifier, from a committed file-provider apply) forces the leader
// to reload its snapshot on the next tick, well before the slow refreshEvery fallback.
func TestConfigSignalForcesSnapshotReload(t *testing.T) {
	fs := &fakeStore{leader: true} // no monitors → no dispatch, just count reloads
	ch := make(chan struct{}, 1)
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithConfigSignals(ch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitUntil(t, 3*time.Second, "initial snapshot load", func() bool {
		return atomic.LoadInt32(&fs.listCalls) >= 1
	})
	n0 := atomic.LoadInt32(&fs.listCalls)

	// A config signal must force another reload within a couple of ticks — far under the 15s
	// refreshEvery fallback (which would otherwise be the only reload path).
	ch <- struct{}{}
	waitUntil(t, 4*time.Second, "config-signal-forced reload", func() bool {
		return atomic.LoadInt32(&fs.listCalls) > n0
	})
}

func TestCredentialDispatchRequiresV2AMQPConsumer(t *testing.T) {
	monitor := domain.Monitor{
		ID: "credential-monitor", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: "secure", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
	}
	for _, tc := range []struct {
		name    string
		ready   staticCredentialRegions
		wantJob bool
	}{
		{name: "legacy-or-absent-consumer-does-not-authorize", ready: staticCredentialRegions{}, wantJob: false},
		{name: "v2-consumer-authorizes", ready: staticCredentialRegions{"secure": true}, wantJob: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
			disp := dispatch.NewInProc(2)
			s := New(fs, disp, testLogger()).WithCredentialEnvelopes(true).WithCredentialLiveRegions(tc.ready)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go s.Run(ctx)
			select {
			case delivered := <-disp.Jobs():
				if !tc.wantJob {
					t.Fatalf("credential job reached transport without v2 consumer: %+v", delivered.Job)
				}
				if delivered.Job.ProtocolVersion != dispatch.ProtocolV2 {
					t.Fatalf("protocol=%d, want v2", delivered.Job.ProtocolVersion)
				}
				// In-process the carrier generation is the publisher's own version;
				// on a wire it comes from the queue the message was consumed from.
				if delivered.CarrierGeneration != dispatch.ProtocolV2 {
					t.Fatalf("carrier generation=%d, want v2", delivered.CarrierGeneration)
				}
			case <-time.After(1500 * time.Millisecond):
				if tc.wantJob {
					t.Fatal("credential job was not published to capable region")
				}
			}
		})
	}
}

func TestCredentialCadenceUsesAuthoritativeMaterializedInterval(t *testing.T) {
	snapshot := domain.Monitor{
		ID: "credential-monitor", Type: domain.MonitorPostgres, Target: "old:5432",
		Region: "secure", Enabled: true, IntervalSeconds: 1, TimeoutSeconds: 5,
	}
	authoritative := snapshot
	authoritative.Target = "new:5432"
	authoritative.IntervalSeconds = 60
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{snapshot}}
	fs.materialize = func(ids []string) ([]store.MaterializedExecution, error) {
		return []store.MaterializedExecution{{MonitorID: ids[0], Job: dispatch.CheckJob{Monitor: authoritative, ProtocolVersion: dispatch.ProtocolV2}}}, nil
	}
	disp := dispatch.NewInProc(4)
	s := New(fs, disp, testLogger()).WithCredentialEnvelopes(true).WithCredentialLiveRegions(staticCredentialRegions{"secure": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	select {
	case delivered := <-disp.Jobs():
		if delivered.Job.Monitor.Target != authoritative.Target || delivered.Job.Monitor.IntervalSeconds != authoritative.IntervalSeconds {
			t.Fatalf("published stale snapshot instead of authoritative row: %+v", delivered.Job.Monitor)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("credential job was not published")
	}
	select {
	case delivered := <-disp.Jobs():
		t.Fatalf("cadence advanced from stale 1s snapshot; unexpected second job: %+v", delivered.Job)
	case <-time.After(2500 * time.Millisecond):
	}
}

// waitUntil polls cond until true or the deadline elapses (then fails with msg).
func waitUntil(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// TestEnterConfirmCap proves the per-region cap: past confirmCapPerRegion the
// monitor is not accelerated (its nextRun stays untouched).
func TestEnterConfirmCap(t *testing.T) {
	s := New(&fakeStore{}, dispatch.NewInProc(1), testLogger())
	now := time.Now()
	confirmFast := map[string]time.Time{}
	byID := map[string]domain.Monitor{}
	nextRun := map[string]time.Time{}
	mk := func(id string) domain.Monitor {
		m := domain.Monitor{ID: id, Type: domain.MonitorTCP, Target: "x:1", Region: "core",
			IntervalSeconds: 3600, FailureThreshold: 3, ConfirmIntervalSeconds: 5, Status: domain.StatusUp}
		byID[id] = m
		return m
	}
	for i := 0; i < confirmCapPerRegion; i++ {
		s.enterConfirm(confirmFast, byID, mk(fmt.Sprintf("m%d", i)), now, nextRun)
	}
	if len(confirmFast) != confirmCapPerRegion {
		t.Fatalf("accelerated = %d, want %d", len(confirmFast), confirmCapPerRegion)
	}
	over := mk("overflow")
	s.enterConfirm(confirmFast, byID, over, now, nextRun)
	if _, ok := confirmFast["overflow"]; ok {
		t.Fatal("cap must reject the overflow monitor")
	}
	if _, ok := nextRun["overflow"]; ok {
		t.Fatal("capped monitor's schedule must stay untouched")
	}
	// A different region is unaffected by the full one.
	other := domain.Monitor{ID: "geo", Type: domain.MonitorTCP, Target: "x:1", Region: "geo1",
		IntervalSeconds: 3600, FailureThreshold: 3, ConfirmIntervalSeconds: 5, Status: domain.StatusUp}
	byID["geo"] = other
	s.enterConfirm(confirmFast, byID, other, now, nextRun)
	if _, ok := confirmFast["geo"]; !ok {
		t.Fatal("another region must not be capped by core")
	}
}

type leaderStateSpy struct {
	mu     sync.Mutex
	states []bool
}

func (s *leaderStateSpy) SetSchedulerLeader(leader bool) {
	s.mu.Lock()
	s.states = append(s.states, leader)
	s.mu.Unlock()
}

func (s *leaderStateSpy) snapshot() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.states...)
}

// TestLeadershipWatchdogStepsDown proves the anti-split-brain watchdog: when the
// liveness check reports the advisory lock is no longer held, the leader steps down
// (gauge → false) and re-contends instead of continuing to dispatch.
func TestLeadershipWatchdogStepsDown(t *testing.T) {
	fs := &fakeStore{leader: true, checkHeld: func() (bool, error) { return false, nil }}
	disp := dispatch.NewInProc(1)
	spy := &leaderStateSpy{}
	s := New(fs, disp, testLogger()).WithLeaderState(spy)
	s.tick = 15 * time.Millisecond
	s.retry = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// The lost lock makes it flap: acquire → first tick detects loss → step down →
	// re-contend. Expect several elections and a leader-gauge that went true then false.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&fs.elections) < 2 {
		select {
		case <-deadline:
			t.Fatalf("watchdog did not re-contend after losing the lock (elections=%d)", atomic.LoadInt32(&fs.elections))
		case <-time.After(15 * time.Millisecond):
		}
	}
	cancel()

	states := spy.snapshot()
	var sawTrue, sawFalseAfterTrue bool
	for _, v := range states {
		if v {
			sawTrue = true
		} else if sawTrue {
			sawFalseAfterTrue = true
		}
	}
	if !sawTrue || !sawFalseAfterTrue {
		t.Fatalf("leader gauge must go true then false on step-down, got %v", states)
	}
}

func TestSchedulerStandbyStopsOnCancel(t *testing.T) {
	fs := &fakeStore{leader: false}
	disp := dispatch.NewInProc(1)
	s := New(fs, disp, testLogger())
	s.retry = 20 * time.Millisecond

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { s.Run(ctx); close(done) }()

	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("standby scheduler did not stop on cancel")
	}
	if atomic.LoadInt32(&fs.elections) < 1 {
		t.Fatal("scheduler should have attempted election at least once")
	}
}

// failingDispatcher refuses every publish until healAfter attempts have been made,
// standing in for a broker outage and its recovery.
type failingDispatcher struct {
	mu        sync.Mutex
	attempts  int
	healAfter int // 0 = never heal
	published []string
}

func (d *failingDispatcher) PublishJob(_ context.Context, job dispatch.CheckJob) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attempts++
	if d.healAfter > 0 && d.attempts > d.healAfter {
		d.published = append(d.published, job.Monitor.ID)
		return nil
	}
	return errors.New("broker unreachable")
}

func (d *failingDispatcher) publishedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.published...)
}
func (d *failingDispatcher) Jobs() <-chan dispatch.DeliveredJob                    { return nil }
func (d *failingDispatcher) PublishResult(context.Context, domain.Heartbeat) error { return nil }
func (d *failingDispatcher) Results() <-chan domain.Heartbeat                      { return nil }
func (d *failingDispatcher) Close() error                                          { return nil }

func (d *failingDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

// TestPublishFailureHonoursTheBackoffFloor is the regression for the audit's remaining P1
// (§4.4.5, D-0160). "Not marked as sent" and "eligible again on the next tick" are
// different statements, and the publish path conflated them: it left nextRun untouched, so
// the monitor came due on the very next tick. A broker outage — the one fault guaranteed to
// hit every credentialed monitor at once — therefore turned each tick into a full
// authoritative-read + decrypt + seal storm.
//
// The assertion is on STATE, not on a wall-clock rate: with a 60s interval the backoff is
// at least one interval, so over a couple of seconds of ticking a monitor must be attempted
// exactly once, not once per tick.
func TestPublishFailureHonoursTheBackoffFloor(t *testing.T) {
	// A CREDENTIALED monitor: this clause governs the materialize→publish path, where a
	// retry costs an authoritative read, a decrypt and a seal. The plain snapshot path is
	// deliberately left as it is — a failed publish there costs a marshal and an enqueue
	// attempt, so retrying it promptly is cheap and helps recovery.
	monitor := domain.Monitor{
		ID: "publish-fails", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: "secure", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
	disp := &failingDispatcher{}
	s := New(fs, disp, testLogger()).WithCredentialEnvelopes(true).
		WithCredentialLiveRegions(staticCredentialRegions{"secure": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitUntil(t, 3*time.Second, "first publish attempt", func() bool { return disp.count() >= 1 })
	time.Sleep(2500 * time.Millisecond) // several scheduler ticks
	if got := disp.count(); got != 1 {
		t.Fatalf("publish attempted %d times while the broker was down; the backoff floor bounds it to one per window", got)
	}
}

// countingSecretSink records which metric FAMILY each rejection lands in.
type countingSecretSink struct {
	mu        sync.Mutex
	secret    []string
	transport []string
}

func (s *countingSecretSink) RecordSecretResolutionFailure(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secret = append(s.secret, reason)
}

func (s *countingSecretSink) RecordDispatchTransportFailure(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transport = append(s.transport, reason)
}

func (s *countingSecretSink) snapshot() (secret, transport []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.secret...), append([]string(nil), s.transport...)
}

// TestPublishFailureRecoversAndIsCountedSeparately completes the §4.4.5 acceptance the
// first regression left implicit: it is not enough that a failing publish backs off — the
// counter must RESET on success and ordinary cadence must resume, and a transport fault
// must not be counted as a secret-resolution fault. An operator seeing "secret resolution
// failed" during a broker outage would look in entirely the wrong place.
func TestPublishFailureRecoversAndIsCountedSeparately(t *testing.T) {
	monitor := domain.Monitor{
		ID: "publish-recovers", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: "secure", Enabled: true, IntervalSeconds: 1, TimeoutSeconds: 5,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
	disp := &failingDispatcher{healAfter: 1}
	sink := &countingSecretSink{}
	s := New(fs, disp, testLogger()).WithCredentialEnvelopes(true).
		WithCredentialLiveRegions(staticCredentialRegions{"secure": true}).
		WithSecretResolutionMetrics(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// The first attempt fails and backs off; the second succeeds once the backoff elapses.
	waitUntil(t, 6*time.Second, "publish recovery after the transport heals", func() bool {
		return len(disp.publishedIDs()) >= 1
	})
	// After a success the counter is reset, so ordinary cadence resumes rather than the
	// monitor staying on an ever-growing backoff.
	waitUntil(t, 6*time.Second, "ordinary cadence resumes after recovery", func() bool {
		return len(disp.publishedIDs()) >= 2
	})

	secret, transport := sink.snapshot()
	if len(transport) == 0 || transport[0] != "publish_failed" {
		t.Fatalf("transport failures = %v, want at least one publish_failed", transport)
	}
	for _, reason := range secret {
		if reason == "publish_failed" {
			t.Fatal("a transport fault was counted as a secret-resolution failure")
		}
	}
}

// TestSkippedMonitorIsNotAFailure closes the audit P2 the first pass only half-fixed: the
// metric stopped counting a skip, but the WARN, the failure counter and the backoff still
// treated it as one. A snapshot nominating a row the authoritative read finds disabled is
// ordinary reconcile churn (§4.4.3) — calling it an operational error is how a metric and a
// log both learn to cry wolf.
func TestSkippedMonitorIsNotAFailure(t *testing.T) {
	monitor := domain.Monitor{
		ID: "skipped-monitor", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: "secure", Enabled: true, IntervalSeconds: 1, TimeoutSeconds: 5,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
	// The authoritative read reports the row as no longer dispatchable.
	fs.materialize = func(ids []string) ([]store.MaterializedExecution, error) {
		out := make([]store.MaterializedExecution, 0, len(ids))
		for _, id := range ids {
			out = append(out, store.MaterializedExecution{MonitorID: id, Reason: store.MaterializeSkippedCurrentState})
		}
		return out, nil
	}
	sink := &countingSecretSink{}
	s := New(fs, dispatch.NewInProc(4), testLogger()).WithCredentialEnvelopes(true).
		WithCredentialLiveRegions(staticCredentialRegions{"secure": true}).
		WithSecretResolutionMetrics(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	time.Sleep(2500 * time.Millisecond) // several ticks, each nominating the same row

	secret, transport := sink.snapshot()
	if len(secret) != 0 {
		t.Fatalf("a skipped monitor was counted as a secret-resolution failure: %v", secret)
	}
	if len(transport) != 0 {
		t.Fatalf("a skipped monitor was counted as a transport failure: %v", transport)
	}
}

// U2 (iter-0134) — lag is NOT a wedge: a service adopting ninety days of history reports an
// enormous, shrinking lag while every slice advances normally; wedged means an operator is
// REQUIRED, which today only a terminally parked error range proves.
func TestOldBackfillLagIsNotWedged(t *testing.T) {
	if wedged, _ := serviceWedgeReason(metrics.ServiceReliabilityStat{WatermarkLagSeconds: 90 * 24 * 3600}); wedged {
		t.Fatal("a progressing 90-day backfill was declared wedged from one absolute lag sample")
	}
	wedged, reason := serviceWedgeReason(metrics.ServiceReliabilityStat{RepairErrored: 1})
	if !wedged || reason == "" {
		t.Fatalf("a terminally parked error range must wedge (wedged=%v reason=%q)", wedged, reason)
	}
}

// recordingServiceSink captures the sampler's calls in order, for lifecycle assertions.
type recordingServiceSink struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingServiceSink) log(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}
func (r *recordingServiceSink) SetServiceReliabilityStats(metrics.ServiceReliabilityStat) {
	r.log("stats")
}
func (r *recordingServiceSink) SetServiceWedged(wedged bool, reason string) {
	r.log(fmt.Sprintf("wedged=%v:%s", wedged, reason))
}
func (r *recordingServiceSink) ClearServiceReliabilityStats() { r.log("clear") }
func (r *recordingServiceSink) SetServiceFactMaintenance(ok bool, _ int64) {
	r.log(fmt.Sprintf("maint=%v", ok))
}
func (r *recordingServiceSink) RecordServiceSlice(string) {}

// The §16.6b families are deliberately NOT logged into the event sequence: these tests pin the
// reliability sampler's ORDER of verdicts, and an alerting sample riding the same loop must not
// rewrite what those assertions are reading.
func (r *recordingServiceSink) RecordServiceAlertEvaluations(string, string, int) {}
func (r *recordingServiceSink) RecordServiceAlertEmitted(string, string, int)     {}
func (r *recordingServiceSink) RecordServiceAlertWithheld(signal, reason string, n int) {
	if n > 0 {
		r.log("withheld:" + signal + ":" + reason)
	}
}
func (r *recordingServiceSink) RecordServiceIncidents(string, int)            {}
func (r *recordingServiceSink) RecordEscalationSteps(string, int)             {}
func (r *recordingServiceSink) SetServiceAlertPass(string, int64, float64)    {}
func (r *recordingServiceSink) SetServiceAlertStats(metrics.ServiceAlertStat) {}
func (r *recordingServiceSink) SetServiceAlertStalled(string, bool, string)   {}
func (r *recordingServiceSink) SetSchedulerLeader(leader bool) {
	r.log(fmt.Sprintf("leader=%v", leader))
}
func (r *recordingServiceSink) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// V (iter-0134) — the sampler FAILS CLOSED: before any sample succeeds the subsystem is
// UNKNOWN (wedged, not ready) — a terminal error range persisted before this leadership
// began must not hide behind an empty registry — and a failed sample re-enters the same
// unavailable state instead of preserving a stale healthy verdict.
func TestSamplerFailsClosedUntilASampleSucceeds(t *testing.T) {
	release := make(chan struct{})
	fs := &fakeStore{statsFn: func(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
		select {
		case <-release:
			return metrics.ServiceReliabilityStat{}, nil
		case <-ctx.Done():
			return metrics.ServiceReliabilityStat{}, ctx.Err()
		}
	}}
	sink := &recordingServiceSink{}
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.serviceStatsLoop(ctx) }()

	// While the first sample is in flight, the state is UNKNOWN — and unknown reads wedged.
	deadline := time.After(3 * time.Second)
	for {
		ev := sink.snapshot()
		if len(ev) >= 1 {
			if ev[0] != "wedged=true:service reliability state unknown (no sample yet)" {
				t.Fatalf("first event %q is not the fail-closed unknown state", ev[0])
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the sampler never declared the unknown state")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(release)
	for {
		ev := sink.snapshot()
		if len(ev) >= 3 {
			if ev[1] != "stats" || ev[2] != "wedged=false:" {
				t.Fatalf("healthy sample did not clear the unknown state: %v", ev)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the healthy sample never landed: %v", sink.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// V (iter-0134) — a stats FAILURE after a healthy sample marks the component unavailable
// (not ready) instead of silently preserving the previous verdict.
func TestSamplerFailureMarksTheComponentUnavailable(t *testing.T) {
	var calls int32
	fs := &fakeStore{statsFn: func(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return metrics.ServiceReliabilityStat{}, nil
		}
		return metrics.ServiceReliabilityStat{}, fmt.Errorf("boom")
	}}
	sink := &recordingServiceSink{}
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.serviceStatsLoop(ctx) }()
	deadline := time.After(3 * time.Second)
	for {
		ev := sink.snapshot()
		if len(ev) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("healthy sample never landed: %v", sink.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	// Drive the error path directly through one more sample cycle: a fresh loop whose only
	// sample fails must surface "unavailable", never a silent healthy carry-over.
	sink2 := &recordingServiceSink{}
	s2 := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sink2)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); s2.serviceStatsLoop(ctx2) }()
	deadline2 := time.After(3 * time.Second)
	for {
		ev := sink2.snapshot()
		if len(ev) >= 2 {
			if ev[1] != "wedged=true:service reliability stats unavailable" {
				t.Fatalf("failed sample did not mark the component unavailable: %v", ev)
			}
			break
		}
		select {
		case <-deadline2:
			t.Fatalf("failure verdict never landed: %v", sink2.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel2()
	<-done2
}

// V (iter-0134) — cancellation JOINS before the step-down clear: a sample completing
// concurrently with cancellation cannot resurrect a deposed leader's gauges after the clear.
func TestSamplerJoinPrecedesTheStepDownClear(t *testing.T) {
	inFlight := make(chan struct{}, 1)
	fs := &fakeStore{statsFn: func(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
		select {
		case inFlight <- struct{}{}:
		default:
		}
		<-ctx.Done() // a slow query the cancellation interrupts
		return metrics.ServiceReliabilityStat{}, ctx.Err()
	}}
	sink := &recordingServiceSink{}
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sink)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.serviceStatsLoop(ctx) }()
	<-inFlight // the sample is provably in flight
	cancel()
	<-done // the JOIN the lifecycle owns: nothing of the loop survives this line
	sink.ClearServiceReliabilityStats()

	ev := sink.snapshot()
	if ev[len(ev)-1] != "clear" {
		t.Fatalf("an event landed after the join+clear: %v", ev)
	}
	for _, e := range ev[1:] { // ev[0] is the fail-closed unknown verdict
		if e == "stats" {
			t.Fatalf("a cancelled sample still published stats: %v", ev)
		}
	}
}

// W (iter-0135) — UNKNOWN is published SYNCHRONOUSLY before leadership: the readiness
// endpoint can run between setLeaderState(true) and the sampler goroutine's first
// instruction, and in that window a terminal error state persisted before this leadership
// must not read as healthy. Exercised through the REAL Run acquisition path.
func TestLeadershipPublishesUnknownBeforeLeader(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	fs := &fakeStore{leader: true, statsFn: func(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return metrics.ServiceReliabilityStat{}, ctx.Err()
	}}
	sink := &recordingServiceSink{}
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sink).WithLeaderState(sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		ev := sink.snapshot()
		iUnknown, iLeader := -1, -1
		for i, e := range ev {
			if iUnknown < 0 && e == "wedged=true:service reliability state unknown (no sample yet)" {
				iUnknown = i
			}
			if iLeader < 0 && e == "leader=true" {
				iLeader = i
			}
		}
		if iLeader >= 0 {
			if iUnknown < 0 || iUnknown > iLeader {
				t.Fatalf("leadership published before the unknown verdict: %v", ev)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("leadership never published: %v", sink.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// W (iter-0135) — healthy → error inside ONE sampler loop: a failed sample after a healthy
// one marks the component unavailable in the same lifetime, no fresh loop involved.
func TestSamplerHealthyThenErrorInOneLoop(t *testing.T) {
	var calls int32
	fs := &fakeStore{statsFn: func(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return metrics.ServiceReliabilityStat{}, nil
		}
		return metrics.ServiceReliabilityStat{}, fmt.Errorf("boom")
	}}
	sink := &recordingServiceSink{}
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sink)
	s.statsEvery = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.serviceStatsLoop(ctx) }()
	deadline := time.After(3 * time.Second)
	for {
		ev := sink.snapshot()
		sawHealthy, sawUnavailable := false, false
		for _, e := range ev {
			if e == "wedged=false:" {
				sawHealthy = true
			}
			if sawHealthy && e == "wedged=true:service reliability stats unavailable" {
				sawUnavailable = true
			}
		}
		if sawUnavailable {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("healthy→error never surfaced in one loop: %v", sink.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// W (iter-0135) — the REAL lead return joins the sampler before the step-down clear: with a
// blocked sample in flight, losing leadership (watchdog says not held) must end in
// leader=false + clear as the FINAL events, nothing published after.
func TestStepDownJoinsAndClearsThroughRealLead(t *testing.T) {
	var held int32 = 1
	fs := &fakeStore{
		leader:    true,
		checkHeld: func() (bool, error) { return atomic.LoadInt32(&held) == 1, nil },
		statsFn: func(ctx context.Context) (metrics.ServiceReliabilityStat, error) {
			<-ctx.Done() // a sample that only cancellation ends
			return metrics.ServiceReliabilityStat{}, ctx.Err()
		},
	}
	sink := &recordingServiceSink{}
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sink).WithLeaderState(sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		ev := sink.snapshot()
		found := false
		for _, e := range ev {
			if e == "leader=true" {
				found = true
			}
		}
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("leadership never acquired: %v", sink.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
	atomic.StoreInt32(&held, 0) // the watchdog deposes the leader; lead must return

	deadline2 := time.After(5 * time.Second)
	for {
		ev := sink.snapshot()
		if n := len(ev); n >= 2 && ev[n-1] == "clear" && ev[n-2] == "leader=false" {
			// Settle: nothing may land after the join+clear.
			time.Sleep(50 * time.Millisecond)
			ev2 := sink.snapshot()
			if len(ev2) != n {
				t.Fatalf("events landed after the step-down clear: %v", ev2[n:])
			}
			for _, e := range ev2 {
				if e == "stats" {
					t.Fatalf("a blocked sample still published stats: %v", ev2)
				}
			}
			return
		}
		select {
		case <-deadline2:
			t.Fatalf("step-down never cleared: %v", sink.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// FR-021 §16.3/§16.4 — both service-alert arms belong to the LEADER, and each must be handed the
// cadence it is evaluated on. The cadence is not decoration: the freshness lease is a multiple of
// it (§16.5a), so a leader evaluating on one number while the store leases on another either
// dis-arms a healthy evaluator or leaves coverage armed after the evaluator stopped.
func TestLeaderEvaluatesBothServiceAlertArms(t *testing.T) {
	fs := &fakeStore{leader: true}
	s := New(fs, dispatch.NewInProc(1), testLogger())
	s.tick = 15 * time.Millisecond
	s.retry = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&fs.alertCalls) == 0 || atomic.LoadInt32(&fs.burnCalls) == 0 {
		select {
		case <-deadline:
			t.Fatalf("leader did not evaluate both arms (live=%d burn=%d)",
				atomic.LoadInt32(&fs.alertCalls), atomic.LoadInt32(&fs.burnCalls))
		case <-time.After(15 * time.Millisecond):
		}
	}
	cancel()

	if got := time.Duration(atomic.LoadInt64(&fs.alertCadence)); got != serviceAlertEvery {
		t.Fatalf("live arm evaluated with cadence %s, want the cadence it is scheduled on (%s)",
			got, serviceAlertEvery)
	}
	if got := time.Duration(atomic.LoadInt64(&fs.burnCadns)); got != serviceBurnEvery {
		t.Fatalf("burn arm evaluated with cadence %s, want %s", got, serviceBurnEvery)
	}
}

// A standby must never evaluate: an evaluation writes the arming state that silences OTHER
// monitors' alerts, so two nodes doing it would let a node that lost the lock keep deciding who
// gets paged.
func TestStandbyEvaluatesNeitherServiceAlertArm(t *testing.T) {
	fs := &fakeStore{leader: false}
	s := New(fs, dispatch.NewInProc(1), testLogger())
	s.tick = 15 * time.Millisecond
	s.retry = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()

	if a, b := atomic.LoadInt32(&fs.alertCalls), atomic.LoadInt32(&fs.burnCalls); a != 0 || b != 0 {
		t.Fatalf("a standby evaluated service alerts (live=%d burn=%d)", a, b)
	}
}

// leadUntilMetrics runs a scheduler against a REAL registry and returns the first scrape that
// satisfies `ready`, with the scheduler STILL leader — the gauges are leadership-scoped and the
// step-down clear forgets them, so a scrape taken after cancellation would read an empty page.
func leadUntilMetrics(t *testing.T, fs *fakeStore, reg *metrics.Registry, ready func(string) bool) string {
	t.Helper()
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(reg)
	s.tick = 15 * time.Millisecond
	s.retry = 15 * time.Millisecond
	s.statsEvery = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.After(5 * time.Second)
	for {
		var buf bytes.Buffer
		reg.WritePrometheus(&buf)
		if got := buf.String(); ready(got) {
			return got
		}
		select {
		case <-deadline:
			var buf bytes.Buffer
			reg.WritePrometheus(&buf)
			t.Fatalf("the expected telemetry never landed:\n%s", buf.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// FR-021 §16.6b — one pass of each arm moves the evaluator families, fed from what the evaluation
// RETURNED. Driven through the real leader loop against the real registry: the point is the wiring,
// and a hand-called observer would pass with the loop never publishing anything.
func TestLeaderPublishesServiceAlertTelemetry(t *testing.T) {
	fs := &fakeStore{
		leader: true,
		alertEval: store.ServiceAlertEvaluation{
			Evaluated: 2, Onsets: 1, Closes: 1, Errors: 1, Lag: 12500 * time.Millisecond,
			// FR-022. Deliberately NOT equal to Onsets/Closes: an onset for a service whose incident
			// is already open announces and opens nothing, so the two families must be readable
			// apart. Equal numbers here would let a wiring that published the edges twice pass.
			IncidentsOpened: 3, IncidentsResolved: 2,
		},
		// Rules 3 with 1 HOLD: two rules could speak, one evaluated successfully and could not.
		burnEval: store.ServiceBurnEvaluation{
			Targets: 2, Rules: 3, Holds: 1, Onsets: 2, Closes: 0, Errors: 1, Lag: 3 * time.Second,
		},
		alertStats: metrics.ServiceAlertStat{
			ActiveHealth: 4, ActiveBurn: 2, BacklogHealth: 7, BacklogBurn: 1,
		},
		// FR-023: the ladder now has two kinds of subject, and unequal numbers are what prove the
		// two series are fed from the split rather than from one total counted twice.
		escalationPass: store.EscalationPass{MonitorSteps: 2, ServiceSteps: 5},
	}
	reg := metrics.New(buildinfo.Info{}, "scheduler")
	reg.SetReady(true, "")

	// The wait and the assertions are built from ONE list, and that is the fix rather than a longer
	// predicate: waiting for three series while asserting sixteen made this a race — the sampled
	// families and the burn arm's HOLD arrive on their own passes, so a render that satisfied the old
	// predicate could legitimately be missing half of what follows. Two failures in forty -race runs.
	want := []string{
		// The live arm's unit is the service: Evaluated → ok, Errors → error, no skips.
		`cerbix_service_alert_evaluations_total{signal="health",outcome="ok"} 2`,
		`cerbix_service_alert_evaluations_total{signal="health",outcome="error"} 1`,
		`cerbix_service_alert_evaluations_total{signal="health",outcome="skipped"} 0`,
		// The burn arm's unit is the rule: Rules−Holds → ok, Holds → skipped, Errors (targets).
		`cerbix_service_alert_evaluations_total{signal="burn",outcome="ok"} 2`,
		`cerbix_service_alert_evaluations_total{signal="burn",outcome="skipped"} 1`,
		`cerbix_service_alert_evaluations_total{signal="burn",outcome="error"} 1`,
		`cerbix_service_alert_emitted_total{signal="health",edge="onset"} 1`,
		`cerbix_service_alert_emitted_total{signal="health",edge="close"} 1`,
		`cerbix_service_alert_emitted_total{signal="burn",edge="onset"} 2`,
		`cerbix_service_alert_emitted_total{signal="burn",edge="close"} 0`,
		// FR-022: incidents a MACHINE opened and resolved. These were computed by the evaluator and
		// consumed by nothing at all until this test — the same dead-fact shape as the member
		// snapshot: recorded, and invisible to whoever has to answer for it at 3am.
		`cerbix_service_incidents_total{action="opened"} 3`,
		`cerbix_service_incidents_total{action="resolved"} 2`,
		// The ladder's own family, split by who paged (FR-023). Before this the ladder had NO metric
		// at all — only a log line — so "12 steps fired" was not even askable, let alone by subject.
		`cerbix_escalation_steps_total{subject="monitor"} 2`,
		`cerbix_escalation_steps_total{subject="service"} 5`,
		`cerbix_service_alert_lag_seconds{signal="health"} 12.500`,
		`cerbix_service_alert_lag_seconds{signal="burn"} 3.000`,
		// The sampled half: what the slice cannot see.
		`cerbix_service_alert_active{signal="health"} 4`,
		`cerbix_service_alert_active{signal="burn"} 2`,
		`cerbix_service_alert_backlog{signal="health"} 7`,
		`cerbix_service_alert_backlog{signal="burn"} 1`,
	}
	got := leadUntilMetrics(t, fs, reg, func(s string) bool {
		for _, w := range want {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	})
	// Still asserted item by item: leadUntilMetrics fails with the whole render, and this names WHICH
	// line is missing if the two ever disagree.
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	if !strings.Contains(got, `cerbix_service_alert_last_success_seconds{signal="health"}`) {
		t.Fatalf("a successful pass did not stamp last-success:\n%s", got)
	}
	// 12.5s and 3s are both inside 3 × their cadence: a busy evaluator is not a stalled one.
	if !reg.Ready() {
		t.Fatalf("a pass inside the lease bound marked the scheduler not-ready: %q", reg.LastError())
	}
}

// An arm that FAILS counts the pass as an error and stamps no success: a slice that rolled back
// evaluated none of its units, and a family that only counts successes would render a permanently
// broken evaluator as silence.
func TestErroringServiceAlertArmCountsAnErrorOutcome(t *testing.T) {
	fs := &fakeStore{leader: true, alertErr: errors.New("boom")}
	reg := metrics.New(buildinfo.Info{}, "scheduler")
	reg.SetReady(true, "")
	// The predicate waits for BOTH facts this test asserts, not just the first one. Waiting only for
	// the health arm's error made the burn assertion a RACE: the health arm fails on its first pass
	// while the burn arm may not have run at all yet, and the test then reported "the healthy burn arm
	// published nothing" about an arm that had simply not been asked. Two failures in thirty -race
	// runs, and it is a defect of the WAIT, not of the code under test.
	got := leadUntilMetrics(t, fs, reg, func(s string) bool {
		return strings.Contains(s, `cerbix_service_alert_evaluations_total{signal="health",outcome="error"} 1`) &&
			strings.Contains(s, `cerbix_service_alert_evaluations_total{signal="burn",outcome="ok"}`)
	})
	if strings.Contains(got, `cerbix_service_alert_evaluations_total{signal="health",outcome="ok"}`) {
		t.Fatalf("a failed pass reported evaluated units:\n%s", got)
	}
	if strings.Contains(got, `cerbix_service_alert_last_success_seconds{signal="health"}`) {
		t.Fatalf("a failed pass stamped a last SUCCESS:\n%s", got)
	}
	// The burn arm is independent and still healthy — one broken arm does not blank the other.
	if !strings.Contains(got, `cerbix_service_alert_evaluations_total{signal="burn",outcome="ok"} 0`) {
		t.Fatalf("the healthy burn arm published nothing:\n%s", got)
	}
}

// §16.6b readiness — the bound is the LEASE's own multiplier, and equality is still fresh
// (`now() < lease_until` is what the store writes, so lag == bound has not expired anything).
func TestServiceAlertStallThresholdIsTheLeaseBound(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cadence time.Duration
		lag     time.Duration
		stalled bool
	}{
		{name: "well inside", cadence: serviceAlertEvery, lag: serviceAlertEvery, stalled: false},
		{name: "at the bound", cadence: serviceAlertEvery,
			lag: store.ServiceAlertLeaseMultiplier * serviceAlertEvery, stalled: false},
		{name: "past the bound", cadence: serviceAlertEvery,
			lag: store.ServiceAlertLeaseMultiplier*serviceAlertEvery + time.Second, stalled: true},
		{name: "burn cadence scales the bound", cadence: serviceBurnEvery,
			lag: store.ServiceAlertLeaseMultiplier*serviceAlertEvery + time.Second, stalled: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := metrics.New(buildinfo.Info{}, "scheduler")
			reg.SetReady(true, "")
			s := New(&fakeStore{}, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(reg)
			s.observeServiceAlertPass(serviceAlertPass{
				signal: "health", cadence: tt.cadence, lag: tt.lag,
			})
			if reg.Ready() == tt.stalled {
				t.Fatalf("lag %s against cadence %s: ready=%v, want stalled=%v (%q)",
					tt.lag, tt.cadence, reg.Ready(), tt.stalled, reg.LastError())
			}
		})
	}
}

// §16.6b — a stalled evaluator marks the SCHEDULER not-ready and NOTHING else: taking the API out
// of rotation for an alerting stall would turn a degradation into an outage. Both directions, and
// the recovery, through the real leader loop.
func TestServiceAlertStallMarksSchedulerNotReadyOnly(t *testing.T) {
	// The API's registry: a process that never runs the leader loop is never handed this sink, so
	// no evaluation verdict can reach its readiness.
	api := metrics.New(buildinfo.Info{}, "api")
	api.SetReady(true, "")

	fs := &fakeStore{leader: true, alertEval: store.ServiceAlertEvaluation{
		Lag: store.ServiceAlertLeaseMultiplier*serviceAlertEvery + time.Minute,
	}}
	sched := metrics.New(buildinfo.Info{}, "scheduler")
	sched.SetReady(true, "")
	// The predicate reads the RENDER, not the live registry: the helper hands back the snapshot
	// that satisfied it, so a condition phrased against `sched.Ready()` could return a buffer
	// captured a moment BEFORE the flip and then assert on it. That race only ever loses under
	// load, which is the worst way to find out.
	got := leadUntilMetrics(t, fs, sched, func(got string) bool {
		return strings.Contains(got, "cerbix_ready 0")
	})

	if sched.Ready() {
		t.Fatal("§16.6b: a stalled evaluator left the scheduler ready")
	}
	if !strings.Contains(sched.LastError(), "lagging") {
		t.Fatalf("the stall reason is not surfaced on /readyz: %q", sched.LastError())
	}
	if !strings.Contains(got, "cerbix_ready 0") {
		t.Fatalf("cerbix_ready disagrees with Ready() while stalled:\n%s", got)
	}
	if !api.Ready() || api.LastError() != "" {
		t.Fatalf("an alerting stall degraded the API's readiness: ready=%v err=%q",
			api.Ready(), api.LastError())
	}

	// The other direction: a pass back inside the bound clears the stall on the same registry.
	s := New(&fakeStore{}, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(sched)
	s.observeServiceAlertPass(serviceAlertPass{
		signal: "health", cadence: serviceAlertEvery, lag: time.Second,
	})
	if !sched.Ready() {
		t.Fatalf("a recovered evaluator did not restore readiness: %q", sched.LastError())
	}
}

// FR-021 invariant 91 — a stalled evaluator marks the SCHEDULER not-ready, and an arm that fails
// every cadence is the most stalled it can be.
//
// The earlier shape derived readiness from the last reported LAG, which only a pass that READ the
// state can report. So an arm erroring every cadence kept the last successful pass's lag forever and
// read as healthy — while every lease it should have refreshed expired and every service it covers
// dis-armed. The measure is the AGE of the last success.
func TestAPersistentlyFailingArmMarksTheSchedulerNotReady(t *testing.T) {
	reg := metrics.New(buildinfo.Info{}, "scheduler")
	reg.SetReady(true, "")
	s := New(&fakeStore{}, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(reg)

	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	s.startAlertBaseline(now)

	// One good pass, then failures. Inside the bound nothing is wedged: a single missed cadence is
	// not a stall, and treating it as one would flap readiness on every transient error.
	s.observeServiceAlertPass(serviceAlertPass{
		signal: "health", cadence: serviceAlertEvery, lag: time.Second,
	})
	now = now.Add(serviceAlertEvery)
	s.recordServiceAlertArmError("health", serviceAlertEvery)
	if !reg.Ready() {
		t.Fatalf("one failed cadence made the scheduler not-ready: %q", reg.LastError())
	}

	// Past the lease bound the arm has demonstrably stopped covering anything.
	now = now.Add(serviceAlertStallThreshold(serviceAlertEvery) + time.Second)
	s.recordServiceAlertArmError("health", serviceAlertEvery)
	if reg.Ready() {
		t.Fatal("an arm failing past its lease bound left the scheduler ready; every service it " +
			"covers has dis-armed and nothing says so")
	}
	if !strings.Contains(reg.LastError(), "failing") {
		t.Fatalf("the reason does not name a failing evaluator: %q", reg.LastError())
	}

	// A successful pass clears it — the stall is a statement about now, not a scar.
	s.observeServiceAlertPass(serviceAlertPass{
		signal: "health", cadence: serviceAlertEvery, lag: time.Second,
	})
	if !reg.Ready() {
		t.Fatalf("a recovered evaluator did not restore readiness: %q", reg.LastError())
	}
}

// The first-ever pass failing must NOT wedge a scheduler that has only just acquired the lock: it
// has not stalled, it has started. It must still wedge once the bound has passed with no success.
func TestAnArmThatNeverSucceedsWedgesOnlyAfterTheBound(t *testing.T) {
	reg := metrics.New(buildinfo.Info{}, "scheduler")
	reg.SetReady(true, "")
	s := New(&fakeStore{}, dispatch.NewInProc(1), testLogger()).WithServiceMetrics(reg)

	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	s.startAlertBaseline(now)

	s.recordServiceAlertArmError("burn", serviceBurnEvery)
	if !reg.Ready() {
		t.Fatalf("a first-cadence failure wedged a scheduler that had just started: %q", reg.LastError())
	}
	now = now.Add(serviceAlertStallThreshold(serviceBurnEvery) + time.Second)
	s.recordServiceAlertArmError("burn", serviceBurnEvery)
	if reg.Ready() {
		t.Fatal("an arm that has never succeeded left the scheduler ready past its bound")
	}
}
