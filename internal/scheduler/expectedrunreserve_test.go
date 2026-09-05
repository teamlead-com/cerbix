package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-032 phase F (§7.4, §17.14) — the publish boundary, at the leader.
//
// The tick is RESERVE → PUBLISH → CONFIRM. These cases pin the ORDER and what happens when the
// first step fails, because the defect they exist for was invisible to every unit suite: the old
// tick published first, and when the record of that publish failed the payload was discarded, the
// next tick republished a stale identity, and a later gap asserted that no run had happened at an
// instant one had.

// errDeadlock is what the store returns when the ledger's two writers take their rows in opposite
// order — the failure measured six times in seven minutes on a live instance, and the one the old
// tick answered by discarding its payload.
var errDeadlock = errors.New("store: reserve expectations: ERROR: deadlock detected (SQLSTATE 40P01)")

// waitFor polls until cond holds or the budget runs out. A fixed sleep would be either flaky or
// slow, and every case here has a real completion condition.
func waitFor(t *testing.T, budget time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition did not hold within %s", budget)
}

// reserveOrderFrom returns the shared-counter positions at which the store recorded windows.
func reserveOrderFrom(fs *fakeStore) []int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]int64(nil), fs.reserveAt...)
}

// orderedDispatcher records the sequence in which jobs reach a transport, so a case can compare it
// against the sequence in which the ledger recorded them.
type orderedDispatcher struct {
	seq  *int64
	mu   sync.Mutex
	sent []publishedJob
}

type publishedJob struct {
	monitorID string
	jobID     string
	dueAt     time.Time
	at        int64
}

func (d *orderedDispatcher) PublishJob(_ context.Context, job dispatch.CheckJob) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, publishedJob{
		monitorID: job.Monitor.ID, jobID: job.JobID, dueAt: job.DueAt,
		at: atomic.AddInt64(d.seq, 1),
	})
	return nil
}
func (d *orderedDispatcher) Jobs() <-chan dispatch.DeliveredJob                    { return nil }
func (d *orderedDispatcher) PublishResult(context.Context, domain.Heartbeat) error { return nil }
func (d *orderedDispatcher) Results() <-chan domain.Heartbeat                      { return nil }
func (d *orderedDispatcher) Close() error                                          { return nil }

func (d *orderedDispatcher) published() []publishedJob {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]publishedJob(nil), d.sent...)
}

// Invariant 27 — no job is published before its window is recorded.
//
// Asserted as an ORDER over one shared counter rather than as "the store was called": a fake that
// merely counts calls passes with the two steps in either sequence, which is exactly how the
// shipped order looked correct for four phases.
func TestNoJobIsPublishedBeforeItsWindowIsRecorded(t *testing.T) {
	var seq int64
	due := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	m := domain.Monitor{
		ID: "ordered", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 1,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m},
		expectations: map[string]store.DueExpectation{
			m.ID: ledgerExpectation(m.ID, due, m.ExecutionRevision)}}
	fs.reserveSeq = &seq
	d := &orderedDispatcher{seq: &seq}
	s := New(fs, d, testLogger()).WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return len(d.published()) > 0 })

	sent := d.published()
	reserved := reserveOrderFrom(fs)
	if len(reserved) == 0 {
		t.Fatal("nothing was reserved, so the order this case is about does not exist")
	}
	if reserved[0] >= sent[0].at {
		t.Fatalf("the job reached the transport at step %d and its window was recorded at step %d. "+
			"A publish that outruns its own record is the whole defect: when the record then fails, "+
			"the run is in flight and nothing knows it", sent[0].at, reserved[0])
	}
	// And the job carries the identity the ledger recorded, not one minted beside it.
	adv := advancesFrom(fs)
	if len(adv) == 0 || adv[0].JobID != sent[0].jobID {
		t.Fatalf("the published job is %q and the reserved window names %v: one object, two "+
			"identities", sent[0].jobID, adv)
	}
	if adv[0].ReservedAt.IsZero() || !adv[0].ReservedAt.Equal(sent[0].dueAt.Add(time.Second)) {
		t.Errorf("the reserved instant %s is not the one the job carries on the wire", adv[0].ReservedAt)
	}
}

// Invariant 27b — a failed reserve defers the dispatch and moves nothing.
//
// Nothing is published, no counter moves, and the SAME payload is re-submitted on the next tick.
// The mutation is the shipped behaviour: publish anyway and log the failure.
func TestAFailedReserveDefersTheDispatchAndMovesNothing(t *testing.T) {
	due := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	m := domain.Monitor{
		ID: "deferred", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 1,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m},
		expectations: map[string]store.DueExpectation{
			m.ID: ledgerExpectation(m.ID, due, m.ExecutionRevision)}}
	fs.advanceErr = errDeadlock
	// The store mints a fresh identity on every read, exactly as `gen_random_uuid()` does. Without
	// this the held payload and a re-minted one are indistinguishable, and the mutation that drops
	// the hold survives the case — which it did until this line existed.
	fs.mintFresh = true
	var seq int64
	d := &orderedDispatcher{seq: &seq}
	s := New(fs, d, testLogger()).WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	// Wait for the RETRY rather than for a fixed span: the absence assertion below needs several
	// ticks to be given the chance to fail, and "the payload came back" is the real condition.
	waitFor(t, 5*time.Second, func() bool { return len(advancesFrom(fs)) >= 2 })

	if sent := d.published(); len(sent) != 0 {
		t.Fatalf("%d jobs were published while every reserve failed: %+v. A run whose window "+
			"could not be recorded is a run nothing can ever answer for", len(sent), sent)
	}
	// The retry re-submits the SAME payload. A second identity for an instant the schedule has not
	// moved past is the stale-identity defect arriving from the other side.
	adv := advancesFrom(fs)
	if len(adv) < 2 {
		t.Fatalf("the failed reserve was submitted %d times; it must be retried, not dropped", len(adv))
	}
	for i, a := range adv {
		if a.JobID != adv[0].JobID || !a.ExpectedDue.Equal(adv[0].ExpectedDue) || !a.ReservedAt.Equal(adv[0].ReservedAt) {
			t.Fatalf("retry %d carries a different payload: %+v vs %+v", i, a, adv[0])
		}
	}
	if len(confirmsFrom(fs)) != 0 {
		t.Errorf("something was confirmed although nothing was published")
	}
}

// Invariant 27f — a new leader never republishes a reserved window, and never mints a second
// identity for it.
//
// The mechanism is the reserve itself: it moves `next_due_at` in the same statement that writes the
// window, so any later leader reads the moved instant and has no way back to the reserved one. This
// case is that leader: a fresh scheduler, a store whose expectation has already advanced past a
// reserved window, and the assertion that nothing it publishes carries the old instant.
func TestANewLeaderNeverRepublishesAReservedWindow(t *testing.T) {
	reservedDue := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	standing := reservedDue.Add(time.Minute)
	m := domain.Monitor{
		ID: "handover", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 1,
	}
	// The store a NEW leader inherits: the previous one reserved `reservedDue` and the expectation
	// moved with it, which is the atomicity phase F rests on.
	exp := ledgerExpectation(m.ID, standing, m.ExecutionRevision)
	exp.JobID = "44444444-4444-4444-8444-444444444444"
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m},
		expectations: map[string]store.DueExpectation{m.ID: exp}}
	var seq int64
	d := &orderedDispatcher{seq: &seq}
	s := New(fs, d, testLogger()).WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return len(d.published()) > 0 })
	time.Sleep(500 * time.Millisecond) // let it tick again, so "never" has something to be wrong about

	for _, job := range d.published() {
		if job.dueAt.Equal(reservedDue) {
			t.Fatalf("a new leader published job %s for %s — an instant a previous leader had "+
				"already reserved. Two identities for one window is the state §8.1 must then "+
				"refuse, and the refusal is silent", job.jobID, job.dueAt)
		}
	}
	for _, a := range advancesFrom(fs) {
		if a.ExpectedDue.Equal(reservedDue) {
			t.Fatalf("a new leader reserved %s again: %+v", reservedDue, a)
		}
	}
}

// Invariant 27, on the path that LOOKS like success: a partially reserved batch.
//
// `ReserveExpectations` returns without an error and writes fewer windows than it was given — the
// fence refused an item whose configuration changed between the decision and the statement. The
// first version of phase F treated that as success and published the whole batch, so the fenced
// item's job left the process with no window behind it: the ordering invariant broken on the one
// path nobody looks at, found by the reviewer at party [28] and not by any test here.
//
// The mutation is exactly that: aggregate the result — publish on `err == nil` and merely log the
// short count.
func TestAPartiallyReservedBatchPublishesOnlyWhatItReserved(t *testing.T) {
	due := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	first := domain.Monitor{
		ID: "reserved-one", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 1,
	}
	second := first
	second.ID = "fenced-two"
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{first, second},
		expectations: map[string]store.DueExpectation{
			first.ID:  ledgerExpectation(first.ID, due, first.ExecutionRevision),
			second.ID: ledgerExpectation(second.ID, due, second.ExecutionRevision),
		}}
	// The store commits exactly one of the two reservations, every tick.
	fs.reserveShort = 1
	var seq int64
	d := &orderedDispatcher{seq: &seq}
	s := New(fs, d, testLogger()).WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, 5*time.Second, func() bool { return len(d.published()) > 0 })
	// Give it several more ticks: the assertion about the fenced monitor is an ABSENCE.
	time.Sleep(1500 * time.Millisecond)

	// Which monitor the fake fenced is decided by the batch's order, so the case reads it from the
	// submissions rather than assuming — a test that assumed would pass on a reordering while the
	// property it names had stopped holding.
	adv := advancesFrom(fs)
	if len(adv) < 2 {
		t.Fatalf("only %d advances were submitted; this case needs a batch of two", len(adv))
	}
	fenced := adv[1].MonitorID // the fake drops the LAST item of each batch
	kept := adv[0].MonitorID
	if fenced == kept {
		t.Fatalf("both submissions name %s, so the batch was not two monitors", kept)
	}

	published := map[string]int{}
	for _, job := range d.published() {
		published[job.monitorID]++
	}
	if published[fenced] != 0 {
		t.Fatalf("%s was published %d times although its window was never reserved. A job whose "+
			"window is not durable is a run nothing can ever answer for — and a batch-level "+
			"'no error' is not evidence about this item", fenced, published[fenced])
	}
	if published[kept] == 0 {
		t.Fatalf("%s was reserved and never published: a fenced sibling must not hold back the "+
			"dispatch whose own window IS durable", kept)
	}
	// And nothing was confirmed for the fenced monitor either: a confirm names a window that was
	// written, and none was.
	for _, c := range confirmsFrom(fs) {
		if c.MonitorID == fenced {
			t.Errorf("a confirm was submitted for %s, whose window was never reserved: %+v", fenced, c)
		}
	}
}

// Invariant 27j — a FENCED payload is dropped, and the monitor is decided again from its current
// configuration.
//
// 27i proves the negative half: a job whose window was not reserved never leaves. This is the
// positive half, and the reviewer asked for it at party [30] because without it the distinction
// between the two failure shapes can be quietly turned back into loss — or into a hold that never
// terminates, since the fence refuses the same stale revision on every tick, forever.
//
// The fixture is a configuration write crossing a dispatch, which is the state §13.2's fence exists
// for: the payload names revision 1, the monitor is now at revision 2, and the reserve admits no
// item that names a stale one. Nothing was published for that payload, so the instant is provably
// undispatched and a fresh identity for it is not a second identity for anything.
func TestAFencedPayloadIsDroppedAndTheMonitorIsDecidedAgain(t *testing.T) {
	due := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	m := domain.Monitor{
		ID: "revision-crossed", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 1,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{m},
		expectations: map[string]store.DueExpectation{
			m.ID: ledgerExpectation(m.ID, due, m.ExecutionRevision)}}
	fs.mintFresh = true       // as `gen_random_uuid()` does, so a NEW identity is visible as new
	fs.fenceBelowRevision = 2 // the configuration write already landed; revision 1 is stale
	var seq int64
	d := &orderedDispatcher{seq: &seq}
	// The configuration signal is the production path for exactly this event: a write lands and the
	// leader reloads on the next tick instead of waiting out its 15-second refresh. Using it here
	// makes the fixture the real sequence rather than a faster imitation of it.
	configured := make(chan struct{}, 1)
	s := New(fs, d, testLogger()).WithLedgerCarrier(true).
		WithLocalLedgerRegions(domain.DefaultRegion).WithConfigSignals(configured)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// While the payload is stale it is refused, and nothing is published — several ticks, because
	// this half is an absence.
	waitFor(t, 5*time.Second, func() bool { return len(advancesFrom(fs)) >= 2 })
	if sent := d.published(); len(sent) != 0 {
		t.Fatalf("%d jobs were published against a fenced revision: %+v", len(sent), sent)
	}
	stale := advancesFrom(fs)
	for _, a := range stale {
		if a.ExpectedRevision != 1 {
			t.Fatalf("the stale submissions name revision %d, so the fixture is not the one this "+
				"case is about: %+v", a.ExpectedRevision, a)
		}
	}

	// The leader now sees the monitor at its CURRENT revision. Nothing else changed: the schedule
	// never moved, so the standing due instant is the same one.
	current := m
	current.ExecutionRevision = 2
	fs.setMonitors(current)
	configured <- struct{}{}

	waitFor(t, 6*time.Second, func() bool { return len(d.published()) > 0 })
	time.Sleep(500 * time.Millisecond) // and it must publish ONCE, not once per tick

	sent := d.published()
	if len(sent) != 1 {
		t.Fatalf("%d jobs were published after the revision moved, want exactly 1: %+v", len(sent), sent)
	}
	if !sent[0].dueAt.Equal(due) {
		t.Errorf("the new decision published %s, want the standing due instant %s — the schedule "+
			"never moved, so the window is the same one", sent[0].dueAt, due)
	}
	fresh := advancesFrom(fs)
	reserved := fresh[len(fresh)-1]
	if reserved.ExpectedRevision != 2 {
		t.Fatalf("the reserved payload names revision %d, want the current 2 — a HELD payload would "+
			"keep naming the stale one and be refused forever", reserved.ExpectedRevision)
	}
	if reserved.JobID == stale[0].JobID {
		t.Error("the new decision reused the fenced identity: dropping a stale payload means " +
			"deciding again, not re-sending what the fence already refused")
	}
	if sent[0].jobID != reserved.JobID {
		t.Errorf("the published job %s is not the identity that was reserved %s", sent[0].jobID, reserved.JobID)
	}
}
