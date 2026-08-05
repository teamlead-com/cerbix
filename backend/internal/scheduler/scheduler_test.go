package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/dispatch"
	"git.example.com/monitoring/cerbix/internal/domain"
	"git.example.com/monitoring/cerbix/internal/metrics"
)

type fakeStore struct {
	monitors  []domain.Monitor
	stalePush []domain.Monitor
	leader    bool
	elections int32
	ensured   int32
	purged    int32
}

func (f *fakeStore) ListEnabledMonitors(context.Context) ([]domain.Monitor, error) {
	return f.monitors, nil
}

func (f *fakeStore) StalePushMonitors(context.Context) ([]domain.Monitor, error) {
	return f.stalePush, nil
}

func (f *fakeStore) TryBecomeLeader(_ context.Context, _ int64) (func(), bool, error) {
	atomic.AddInt32(&f.elections, 1)
	if !f.leader {
		return nil, false, nil
	}
	return func() {}, true, nil
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
func (f *fakeStore) AdvanceEscalations(context.Context) (int, error)           { return 0, nil }
func (f *fakeStore) EnqueuePullJob(context.Context, string, []byte, int) error { return nil }
func (f *fakeStore) PurgeExpiredPullJobs(context.Context) (int, error)         { return 0, nil }
func (f *fakeStore) PurgeExpiredPullTests(context.Context) (int, error)        { return 0, nil }
func (f *fakeStore) PurgeStaleAgentHeartbeats(context.Context, time.Duration) (int, error) {
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
	case job := <-disp.Jobs():
		if job.Monitor.ID != "m1" {
			t.Fatalf("expected active monitor m1, got %q", job.Monitor.ID)
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
		if atomic.LoadInt32(&fs.ensured) >= 1 && atomic.LoadInt32(&fs.purged) >= 1 {
			return // leader ran partition maintenance + retention on becoming leader
		}
		select {
		case <-deadline:
			t.Fatalf("leader did not run partition maintenance: ensured=%d purged=%d",
				atomic.LoadInt32(&fs.ensured), atomic.LoadInt32(&fs.purged))
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
	// The batched staleness query returns a tripped push monitor; the leader
	// publishes a down result for it (which selection is stale is the store's job,
	// covered by the DB-gated StalePushMonitors test).
	fs := &fakeStore{
		leader: true,
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
	case hb := <-disp.Results():
		if hb.MonitorID != "stale" || hb.Up {
			t.Fatalf("expected a down result for the stale monitor, got %+v", hb)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not mark the stale push monitor down")
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
	case job := <-disp.Jobs():
		if job.Monitor.ID != "m1" {
			t.Fatalf("unexpected job %q", job.Monitor.ID)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("confirm signal did not accelerate the next probe")
	}
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
