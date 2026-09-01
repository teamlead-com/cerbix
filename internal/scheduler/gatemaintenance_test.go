package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// gateSinkSpy records the gate loop's metric calls in order.
type gateSinkSpy struct {
	mu     sync.Mutex
	events []string
	gauges []store.GateLedgerGauges
}

func (s *gateSinkSpy) RecordGateMaintenanceError(kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "error:"+kind)
	return nil
}

func (s *gateSinkSpy) SetGateLedgerGauges(pendingDrop int, oldestAgeSeconds, writableHorizonSeconds float64, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "gauges")
	s.gauges = append(s.gauges, store.GateLedgerGauges{PendingDrop: pendingDrop, OldestAgeSeconds: oldestAgeSeconds,
		WritableHorizonSeconds: writableHorizonSeconds, Bytes: bytes})
}

func (s *gateSinkSpy) ClearGateLedgerGauges() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "clear")
}

func (s *gateSinkSpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

var testGateCfg = store.GateMaintenanceConfig{
	RetentionDays: 90, LeadDays: 7, CreateMax: 3, PurgeEvery: time.Hour, PurgeMaxPartitions: 8,
}

// Invariant 18 — ledger maintenance never holds dispatch: the pass runs in its own goroutine, so
// a pass blocked for longer than many ticks (here: until cancellation, standing in for a DETACH
// or ATTACH waiting behind a lock) leaves the dispatch tick firing on cadence. The oracle is the
// dispatcher: a monitor due every second keeps being published while the pass is provably still
// in flight.
func TestGateMaintenanceBlockedPassKeepsDispatchTicking(t *testing.T) {
	inFlight := make(chan struct{}, 1)
	var released int32
	fs := &fakeStore{
		leader: true,
		monitors: []domain.Monitor{
			{ID: "m1", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 1, TimeoutSeconds: 1, Enabled: true},
		},
	}
	fs.gatePassFn = func(ctx context.Context, _ time.Time, _ store.GateMaintenanceConfig) (store.GateMaintenanceReport, bool, error) {
		select {
		case inFlight <- struct{}{}:
		default:
		}
		<-ctx.Done() // the pass ends only with the leadership
		atomic.StoreInt32(&released, 1)
		return store.GateMaintenanceReport{}, true, ctx.Err()
	}
	disp := dispatch.NewInProc(64)
	sink := &gateSinkSpy{}
	s := New(fs, disp, testLogger()).WithGateMaintenance(testGateCfg, sink)
	s.tick = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	select {
	case <-inFlight:
	case <-time.After(3 * time.Second):
		t.Fatal("the gate pass never started")
	}
	// While the pass is blocked: dispatch keeps publishing the due monitor on its 1 s cadence.
	published := 0
	deadline := time.After(3500 * time.Millisecond)
	for published < 3 {
		select {
		case <-disp.Jobs():
			published++
		case <-deadline:
			t.Fatalf("dispatch published only %d jobs in 3.5 s while the gate pass was blocked; the tick is being held", published)
		}
	}
	if atomic.LoadInt32(&released) != 0 {
		t.Fatal("the pass was not blocked for the whole observation window; the test proves nothing")
	}
	if got := atomic.LoadInt32(&fs.gatePasses); got != 1 {
		t.Fatalf("gate passes = %d, want exactly one in flight", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel: the gate loop was not joined")
	}
	// The blocked pass returned nothing publishable, and the step-down cleared the gauges.
	ev := sink.snapshot()
	if len(ev) == 0 || ev[len(ev)-1] != "clear" {
		t.Fatalf("the loop's exit did not clear the ledger gauges: %v", ev)
	}
	for _, e := range ev {
		if e == "gauges" {
			t.Fatalf("a pass that never produced gauges published some: %v", ev)
		}
	}
}

// The gauges are set from a pass's report while the leadership is live, and the step-down —
// here the watchdog losing the lock — clears them BEFORE leader=false, because lead() joins the
// loop before it returns.
func TestGateMaintenanceGaugesFollowTheReportAndClearOnStepDown(t *testing.T) {
	var held int32 = 1
	passed := make(chan struct{}, 1)
	fs := &fakeStore{
		leader:    true,
		checkHeld: func() (bool, error) { return atomic.LoadInt32(&held) == 1, nil },
	}
	fs.gatePassFn = func(context.Context, time.Time, store.GateMaintenanceConfig) (store.GateMaintenanceReport, bool, error) {
		select {
		case passed <- struct{}{}:
		default:
		}
		return store.GateMaintenanceReport{
			GaugesValid: true,
			Gauges:      store.GateLedgerGauges{PendingDrop: 2, OldestAgeSeconds: 3.5, WritableHorizonSeconds: 600, Bytes: 4096},
		}, true, nil
	}
	sink := &gateSinkSpy{}
	leaderSpy := &leaderStateSpy{}
	s := New(fs, dispatch.NewInProc(1), testLogger()).WithGateMaintenance(testGateCfg, sink).WithLeaderState(leaderSpy)
	s.tick = 15 * time.Millisecond
	s.retry = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case <-passed:
	case <-time.After(3 * time.Second):
		t.Fatal("no gate pass ran")
	}
	deadline := time.After(3 * time.Second)
	for {
		ev := sink.snapshot()
		if len(ev) > 0 && ev[len(ev)-1] == "gauges" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("gauges never published: %v", ev)
		case <-time.After(5 * time.Millisecond):
		}
	}
	sink.mu.Lock()
	g := sink.gauges[0]
	sink.mu.Unlock()
	if g.PendingDrop != 2 || g.OldestAgeSeconds != 3.5 || g.WritableHorizonSeconds != 600 || g.Bytes != 4096 {
		t.Fatalf("gauges published %+v, want the report's values", g)
	}

	atomic.StoreInt32(&held, 0) // the watchdog deposes the leader
	deadline = time.After(5 * time.Second)
	for {
		ev := sink.snapshot()
		if len(ev) >= 2 && ev[0] == "gauges" && ev[1] == "clear" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("step-down never cleared the ledger gauges: %v", ev)
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The deposed leader re-contends and (the fake still says leader) starts a fresh loop, so
	// the tail may hold further gauges/clear pairs; the leader gauge went true then false at
	// least once, and every leadership's gate loop ends with its clear.
	//
	// This WAITS for the gauge instead of reading it once. The loop above waits on the SINK —
	// the gate pass's gauges/clear — while the leader gauge is published by the leadership
	// loop itself (`setLeaderState(false)` when the lock is lost). Nothing orders the two, so
	// under load the clear can land first and a single read sees `[false true]`: the standby
	// false and the acquisition true, with the step-down false still in flight. That is the
	// flake this loop removes, and it removes it without weakening the assertion — the same
	// true-then-false is still required, just given a deadline to arrive in.
	deadline = time.After(5 * time.Second)
	var states []bool
	for {
		states = leaderSpy.snapshot()
		var sawTrue, sawFalseAfterTrue bool
		for _, v := range states {
			if v {
				sawTrue = true
			} else if sawTrue {
				sawFalseAfterTrue = true
			}
		}
		if sawTrue && sawFalseAfterTrue {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("leader gauge must go true then false on step-down, got %v", states)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	// A re-contended leadership may be deposed before its first pass published, so only the
	// shape of the FIRST leadership (gauges then clear) and the final clear are asserted.
	ev := sink.snapshot()
	if ev[len(ev)-1] != "clear" {
		t.Fatalf("the final event after shutdown is not the clear: %v", ev)
	}
}

// Without WithGateMaintenance the loop does not exist: a scheduler for a deployment that has no
// gate never calls the store's pass.
func TestGateMaintenanceNotStartedWithoutConfig(t *testing.T) {
	fs := &fakeStore{leader: true}
	s := New(fs, dispatch.NewInProc(1), testLogger())
	s.tick = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&fs.gatePasses); got != 0 {
		t.Fatalf("gate passes = %d without configuration, want 0", got)
	}
}

// D10: the whole pass lifecycle fits subCadenceTimeout — the store's constant and the
// scheduler's are the same number, asserted rather than assumed.
func TestGatePassLifecycleEqualsSubCadenceTimeout(t *testing.T) {
	if store.GatePassLifecycle != subCadenceTimeout {
		t.Fatalf("store.GatePassLifecycle = %s, subCadenceTimeout = %s; the pass timeline must fit one sub-cadence",
			store.GatePassLifecycle, subCadenceTimeout)
	}
}

// The three "cerbix" + slot advisory keys are pairwise distinct. The scheduler's key is
// unexported here, the migration key is unexported in store (asserted against the gate key
// there, see store's TestGateAdvisoryKeysPairwiseDistinct); its literal value is repeated here on
// purpose so this package proves the pairs it can see: (scheduler, gate) and (scheduler, migrate).
func TestGateAdvisoryKeysDistinctFromSchedulerLeadership(t *testing.T) {
	const migrateLockKeyLiteral int64 = 0x6365726269780002 // internal/store/migrate.go migrateLockKey
	if advisoryLockKey == store.GateMaintenanceLockKey {
		t.Fatalf("scheduler leadership and gate maintenance share advisory key %#x", advisoryLockKey)
	}
	if advisoryLockKey == migrateLockKeyLiteral {
		t.Fatalf("scheduler leadership and migrations share advisory key %#x", advisoryLockKey)
	}
	if store.GateMaintenanceLockKey == migrateLockKeyLiteral {
		t.Fatalf("gate maintenance and migrations share advisory key %#x", store.GateMaintenanceLockKey)
	}
	if store.GateMaintenanceLockKey != 0x6365726269780003 {
		t.Fatalf("gate maintenance key %#x is not slot 3 of the cerbix namespace", store.GateMaintenanceLockKey)
	}
}
