package runtime

import (
	"context"
	"os"
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
	relA, _, okA, err := st.TryBecomeLeader(ctx, key)
	if err != nil || !okA {
		t.Fatalf("candidate A must acquire: ok=%v err=%v", okA, err)
	}
	// Candidate B is excluded while A holds.
	relB, _, okB, err := st.TryBecomeLeader(ctx, key)
	if err != nil {
		t.Fatalf("candidate B error: %v", err)
	}
	if okB {
		if relB != nil {
			relB()
		}
		t.Fatal("two providers held leadership simultaneously (split brain)")
	}
	// A releases → a candidate can fail over and acquire.
	relA()
	acquired := false
	for i := 0; i < 50; i++ {
		rel, _, ok, _ := st.TryBecomeLeader(ctx, key)
		if ok {
			acquired = true
			rel()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !acquired {
		t.Fatal("failover did not occur after the leader released")
	}
}
