package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/fileprovider"
	"github.com/teamlead-com/cerbix/internal/store"
)

// blockingSession blocks the FIRST ApplyFileManagedBundle until the test releases it, so the
// test can mutate the directory WHILE a reconcile is in flight. It also signals when a bundle
// for a given project is applied, over channels (no shared-slice race with the loop goroutine).
type blockingSession struct {
	fakeSession
	started chan struct{} // closed when the first apply is reached
	proceed chan struct{} // test closes this to release the first apply
	once    sync.Once

	billing  chan struct{} // closed when a "billing" bundle is applied
	billOnce sync.Once
}

func (s *blockingSession) ApplyFileManagedBundle(ctx context.Context, name string, dp *fileprovider.DesiredProject, path string, grace time.Duration, max int, allow bool) (store.ApplyResult, error) {
	s.once.Do(func() {
		close(s.started)
		<-s.proceed
	})
	if dp.Project == "billing" {
		s.billOnce.Do(func() { close(s.billing) })
	}
	return s.fakeSession.ApplyFileManagedBundle(ctx, name, dp, path, grace, max, allow)
}

// TestLeaderLoopReconcilesChangeDuringReconcile is the #3 regression: a file written WHILE a
// reconcile is running must trigger another reconcile, not be absorbed until the periodic
// resync. Pre-fix, lastFP was sampled AFTER reconcile, folding the mid-reconcile change into
// it so the next poll saw no diff.
func TestLeaderLoopReconcilesChangeDuringReconcile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))

	bs := &blockingSession{
		started: make(chan struct{}),
		proceed: make(chan struct{}),
		billing: make(chan struct{}),
	}
	fa := &fakeApplier{} // owned empty → no orphan work to interfere
	p := testProvider(dir, fa)
	// Fast poll/debounce; resync far away so the ONLY thing that can trigger reconcile #2 is
	// the poll noticing the mid-reconcile change.
	p.pollEvery = 5 * time.Millisecond
	p.debounce = time.Millisecond
	p.resync = time.Hour
	p.leaderCheckEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.leaderLoop(ctx, bs); close(done) }()

	// Reconcile #1 is now blocked inside the payments apply. Add billing.yaml while it runs.
	select {
	case <-bs.started:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile #1 never reached apply")
	}
	write(t, dir, "b.yaml", bundle("acme", "billing"))
	close(bs.proceed) // let reconcile #1 finish

	// A subsequent reconcile must pick up billing.yaml well before the 1h resync.
	select {
	case <-bs.billing:
	case <-time.After(3 * time.Second):
		t.Fatal("mid-reconcile change was absorbed: billing bundle never applied before resync")
	}
	cancel()
	<-done
}
