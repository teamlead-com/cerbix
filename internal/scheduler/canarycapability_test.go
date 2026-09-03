package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 invariant 6 at the dispatch decision: a canary reaches an executor only if that executor
// ANNOUNCED the workflow kind and version. A region with none produces `no_capable_runner`, a
// version skew produces `capability_mismatch`, and both are bounded per-monitor DOWNs — never a job
// left on a queue nobody consumes, which reports nothing at all.

// waitForCanaryHeartbeat blocks until the fake has recorded one, and returns it.
func waitForCanaryHeartbeat(t *testing.T, fs *fakeStore) domain.Heartbeat {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		fs.mu.Lock()
		got := append([]domain.Heartbeat(nil), fs.canaryHeartbeats...)
		fs.mu.Unlock()
		if len(got) > 0 {
			return got[0]
		}
		select {
		case <-deadline:
			t.Fatal("no heartbeat was written for the refused run")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestARegionThatAnnouncedNothingRefusesTheCanary(t *testing.T) {
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{canaryScheduleMonitor()}}
	disp := dispatch.NewInProc(8)
	// No WithLocalCanaryRegions, no agent announcement, no AMQP consumer: nothing anywhere says
	// this region can run an async transaction.
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	hb := waitForCanaryHeartbeat(t, fs)
	if hb.Up || !strings.Contains(hb.Msg, "no_capable_runner") {
		t.Fatalf("heartbeat = %+v, want DOWN with no_capable_runner", hb)
	}
	select {
	case delivered := <-disp.Jobs():
		t.Fatalf("an unannounced region received %s", delivered.Job.Monitor.ID)
	default:
	}
	// The check precedes the LEASE: an incapable region must not consume an in-flight slot it can
	// never release, which would then refuse every other canary there for a whole timeout window.
	fs.mu.Lock()
	claims := len(fs.canaryClaims)
	fs.mu.Unlock()
	if claims != 0 {
		t.Fatalf("the refused run took %d in-flight slot(s)", claims)
	}
}

func TestAVersionSkewIsNamedAsSuchRatherThanAsAnAbsence(t *testing.T) {
	m := canaryScheduleMonitor()
	m.Region = "geo1"
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m}}
	// A real runner is there — it just speaks a version this core does not emit. The operator's
	// fix is an upgrade, not a start, so the two must not share a reason.
	fs.canaryCapabilities = map[string][]string{"geo1": {domain.CanaryCapabilityToken(domain.CanaryWorkflowKind, domain.CanaryWorkflowVersion+1)}}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger()).WithPullRegions([]string{"geo1"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	hb := waitForCanaryHeartbeat(t, fs)
	if hb.Up || !strings.Contains(hb.Msg, "capability_mismatch") {
		t.Fatalf("heartbeat = %+v, want DOWN with capability_mismatch", hb)
	}
}

// The two announcement sources answer for different transports and must not be crossed: an agent
// heartbeat says nothing about a region served over AMQP. Crossing them would let a dead AMQP region
// look capable because an agent for an unrelated deployment is alive in the same row set.
func TestAnAgentAnnouncementDoesNotSpeakForAnAMQPRegion(t *testing.T) {
	m := canaryScheduleMonitor()
	m.Region = "amqp1"
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m}}
	fs.canaryCapabilities = map[string][]string{"amqp1": {domain.CanaryCapabilityOfThisBinary()}}
	disp := dispatch.NewInProc(8)
	// amqp1 is NOT a pull region, so the agent table is not its announcement channel.
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	hb := waitForCanaryHeartbeat(t, fs)
	if hb.Up || !strings.Contains(hb.Msg, "no_capable_runner") {
		t.Fatalf("heartbeat = %+v, want the AMQP region to read as incapable", hb)
	}
}

func TestAPullAgentAnnouncementAuthorizesItsOwnRegion(t *testing.T) {
	m := canaryScheduleMonitor()
	m.Region = "geo1"
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m}}
	fs.canaryCapabilities = map[string][]string{"geo1": {domain.CanaryCapabilityOfThisBinary()}}
	s := New(fs, dispatch.NewInProc(8), testLogger()).WithPullRegions([]string{"geo1"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		fs.mu.Lock()
		enqueued, beats := len(fs.pullLeases), len(fs.canaryHeartbeats)
		fs.mu.Unlock()
		if beats > 0 {
			t.Fatal("an announced region refused the canary")
		}
		if enqueued > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the canary was never enqueued for the announcing pull region")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A lookup that FAILS is not evidence of capability. It refuses — bounded, with a reason an operator
// can read — rather than dispatching into a region core could not verify.
func TestAFailedCapabilityLookupRefusesRatherThanAssumes(t *testing.T) {
	m := canaryScheduleMonitor()
	m.Region = "geo1"
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m}}
	fs.canaryCapabilitiesErr = errors.New("database is down")
	s := New(fs, dispatch.NewInProc(8), testLogger()).WithPullRegions([]string{"geo1"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	hb := waitForCanaryHeartbeat(t, fs)
	if hb.Up || !strings.Contains(hb.Msg, "no_capable_runner") {
		t.Fatalf("heartbeat = %+v, want a bounded refusal when capability cannot be read", hb)
	}
}

// The lookup is not paid for by an instance that has no canaries: it happens at most once per tick,
// and only when a canary is actually due.
func TestTheCapabilityLookupIsSkippedWhenNoCanaryIsDue(t *testing.T) {
	http := domain.Monitor{
		ID: "22222222-2222-2222-2222-222222222222", ProjectID: "p1", Name: "web",
		Type: domain.MonitorHTTP, Region: "core", IntervalSeconds: 1, Enabled: true,
		Config: map[string]string{"url": "https://example.com"},
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{http}}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case <-disp.Jobs():
	case <-time.After(3 * time.Second):
		t.Fatal("the ordinary monitor was never dispatched")
	}
	fs.mu.Lock()
	lookups := fs.canaryCapabilityLookups
	fs.mu.Unlock()
	if lookups != 0 {
		t.Fatalf("an instance with no canary paid for %d capability lookup(s) per tick", lookups)
	}
}

// Ensures the resolver merges the in-process announcement, which is what role=all runs on: without
// it the commonest deployment announces nothing and refuses every canary — the same omission that
// left role=all a credential generation behind until D-0160's local wiring existed.
func TestTheInProcessExecutorAnnouncesItsOwnRegion(t *testing.T) {
	s := New(&fakeStore{}, dispatch.NewInProc(1), testLogger()).WithLocalCanaryRegions(domain.DefaultRegion)
	got := s.canaryAnnouncements(context.Background())
	if !domain.CanaryCapabilityAnnounced(got[domain.DefaultRegion], domain.CanaryCapabilityOfThisBinary()) {
		t.Fatalf("role=all announced %#v, want this binary's own token", got)
	}
}
