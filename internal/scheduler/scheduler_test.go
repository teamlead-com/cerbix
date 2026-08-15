package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/store"
)

type fakeStore struct {
	monitors      []domain.Monitor
	stalePush     []domain.Monitor
	deadmanCh     chan string // receives monitorID on each RecordDeadmanResult call
	leader        bool
	elections     int32
	ensured       int32
	purged        int32
	sessionPurges int32
	flowPurges    int32
	listCalls     int32 // ListEnabledMonitors invocations (snapshot reloads)
	// checkHeld, when set, is what the leadership watchdog check() returns; nil
	// means "still leader" (true, nil).
	checkHeld   func() (bool, error)
	materialize func([]string) ([]store.MaterializedExecution, error)
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

func (f *fakeStore) MaterializeExecutionConfigs(_ context.Context, ids []string, _ int) ([]store.MaterializedExecution, error) {
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

func (f *fakeStore) TryBecomeLeader(_ context.Context, _ int64) (func(), func(context.Context) (bool, error), bool, error) {
	atomic.AddInt32(&f.elections, 1)
	if !f.leader {
		return nil, nil, false, nil
	}
	check := func(context.Context) (bool, error) {
		if f.checkHeld != nil {
			return f.checkHeld()
		}
		return true, nil
	}
	return func() {}, check, true, nil
}

func (f *fakeStore) RollupDailyAvailability(_ context.Context, _, _ time.Time) error { return nil }

func (f *fakeStore) EnsureHeartbeatPartitions(_ context.Context, _ int) error {
	atomic.AddInt32(&f.ensured, 1)
	return nil
}

func (f *fakeStore) PurgeOldHeartbeats(_ context.Context, _ time.Time) (int, error) {
	atomic.AddInt32(&f.purged, 1)
	return 0, nil
}

func (f *fakeStore) EnqueueRenotifyReminders(context.Context) (int, error) { return 0, nil }
func (f *fakeStore) EvaluateBurnAlerts(context.Context) (int, int, error)  { return 0, 0, nil }
func (f *fakeStore) EnqueueDueSLAReports(context.Context) (int, error)     { return 0, nil }
func (f *fakeStore) EvaluateRegionWorkerAlerts(context.Context, map[string]bool, int) (int, int, error) {
	return 0, 0, nil
}
func (f *fakeStore) AdvanceEscalations(context.Context) (int, error)             { return 0, nil }
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
		if atomic.LoadInt32(&fs.ensured) >= 1 && atomic.LoadInt32(&fs.purged) >= 1 &&
			atomic.LoadInt32(&fs.sessionPurges) >= 1 && atomic.LoadInt32(&fs.flowPurges) >= 1 {
			return // leader ran partition maintenance + retention + auth housekeeping
		}
		select {
		case <-deadline:
			t.Fatalf("leader did not run maintenance: ensured=%d purged=%d sessions=%d flows=%d",
				atomic.LoadInt32(&fs.ensured), atomic.LoadInt32(&fs.purged),
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

// failingDispatcher refuses every publish, standing in for a broker outage.
type failingDispatcher struct {
	mu       sync.Mutex
	attempts int
}

func (d *failingDispatcher) PublishJob(context.Context, dispatch.CheckJob) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attempts++
	return errors.New("broker unreachable")
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
