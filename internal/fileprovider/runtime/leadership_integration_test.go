package runtime

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/store"
)

// TestProviderLeadershipFailover proves §12 anti-split-brain for a provider's advisory lock:
// at most one holder at a time, and after the holder releases another candidate acquires
// (failover). DB-gated: skips without CERBIX_TEST_DATABASE_DSN.
func TestProviderLeadershipFailover(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run leadership integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	key := leaderKeyFor("failover-probe")

	// Candidate A wins.
	sA, okA, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !okA {
		t.Fatalf("candidate A must acquire: ok=%v err=%v", okA, err)
	}
	// Candidate B is excluded while A holds.
	sB, okB, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil {
		t.Fatalf("candidate B error: %v", err)
	}
	if okB {
		if sB != nil {
			sB.Release()
		}
		t.Fatal("two providers held leadership simultaneously (split brain)")
	}
	// A releases → a candidate can fail over and acquire.
	sA.Release()
	acquired := false
	for i := 0; i < 50; i++ {
		s2, ok, _ := st.TryBecomeLeaderSession(ctx, key)
		if ok {
			acquired = true
			s2.Release()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !acquired {
		t.Fatal("failover did not occur after the leader released")
	}
}

// TestTwoLiveReplicasSingleLeaderAndFailover runs two real Provider.Run loops against the same
// provider name + directory on real Postgres and proves, end-to-end (not just at the session
// layer), that: (a) at no observed moment do BOTH replicas report leadership, and (b) killing
// the current leader lets the surviving replica take over. Uses an EMPTY directory so the test
// isolates leadership/failover from apply/tenant concerns. DB-gated.
func TestTwoLiveReplicasSingleLeaderAndFailover(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run leadership integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	dir := t.TempDir() // empty: no bundles to apply, leadership is the only concern
	const name = "two-live-replica-probe"

	// Two independent replicas: each its own context (so we can kill one) and status registry.
	newReplica := func() (*Provider, *StatusRegistry) {
		reg := NewStatusRegistry()
		p := testProvider(dir, NewStoreApplier(st))
		p.name = name
		p.leaderKey = leaderKeyFor(name)
		p.leaderCheckEvery = 80 * time.Millisecond
		p.pollEvery = 40 * time.Millisecond
		p.resync = 30 * time.Second
		p.WithStatus(reg)
		return p, reg
	}
	pA, regA := newReplica()
	pB, regB := newReplica()

	ctxA, cancelA := context.WithCancel(ctx)
	ctxB, cancelB := context.WithCancel(ctx)
	defer cancelB()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pA.Run(ctxA) }()
	go func() { defer wg.Done(); pB.Run(ctxB) }()

	isLeader := func(reg *StatusRegistry) bool {
		snap := reg.Snapshot()
		return len(snap) == 1 && snap[0].Leader
	}
	leaderCount := func() int {
		n := 0
		if isLeader(regA) {
			n++
		}
		if isLeader(regB) {
			n++
		}
		return n
	}

	// Wait for exactly one replica to acquire leadership, sampling for split-brain throughout.
	deadline := time.Now().Add(6 * time.Second)
	gotLeader := false
	for time.Now().Before(deadline) {
		lc := leaderCount()
		if lc > 1 {
			cancelA()
			t.Fatal("split brain: both replicas reported leadership simultaneously")
		}
		if lc == 1 {
			gotLeader = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotLeader {
		cancelA()
		t.Fatal("neither replica acquired leadership")
	}

	// Kill whichever replica currently leads; the survivor must take over.
	var survivor *StatusRegistry
	if isLeader(regA) {
		cancelA()
		survivor = regB
	} else {
		cancelB()
		survivor = regA
	}

	failoverDeadline := time.Now().Add(10 * time.Second)
	failedOver := false
	for time.Now().Before(failoverDeadline) {
		if isLeader(survivor) {
			failedOver = true
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	// Stop both replicas before asserting so goroutines exit cleanly.
	cancelA()
	cancelB()
	wg.Wait()
	if !failedOver {
		t.Fatal("survivor did not take over leadership after the leader was killed")
	}
}
