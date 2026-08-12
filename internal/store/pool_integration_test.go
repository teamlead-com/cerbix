package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/store"
)

// TestPoolSizingFitsFileProviderLeaders proves the P0 fix end to end against a real
// Postgres (gated on CERBIX_TEST_DATABASE_DSN): with the pool opened via
// WithFileProviderPool(N, R) for N above the old floor, N distinct leader sessions can
// each pin their connection AND a pooled query still makes progress. Pre-fix this
// deadlocked — the leader pins exhausted the floor-sized pool so pool.Query blocked
// forever. Bounded by context timeouts so it fails fast instead of hanging.
func TestPoolSizingFitsFileProviderLeaders(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run pool sizing integration test")
	}
	const (
		providers = 15 // clearly above poolMaxConnsFloor (12) — would deadlock pre-fix
		reconcile = 4  // matches cli.maxConcurrentReconciles
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.Open(ctx, dsn, store.WithFileProviderPool(providers, reconcile))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// The pool must actually be sized to the computed requirement.
	want := store.RequiredMaxConns(providers, reconcile)
	if got := st.PoolMaxConns(); got < want {
		t.Fatalf("pool MaxConns=%d, want >= RequiredMaxConns=%d", got, want)
	}

	// Pin N distinct leader sessions, each holding its own connection for life (fencing).
	sessions := make([]*store.LeaderSession, 0, providers)
	defer func() {
		for _, s := range sessions {
			s.Release()
		}
	}()
	base := int64(918273645000)
	for i := 0; i < providers; i++ {
		ls, ok, err := st.TryBecomeLeaderSession(ctx, base+int64(i))
		if err != nil {
			t.Fatalf("leader %d acquire error: %v", i, err)
		}
		if !ok {
			t.Fatalf("leader %d not acquired (distinct key should always win)", i)
		}
		sessions = append(sessions, ls)
	}

	// Progress property: with all N leader conns pinned, a pooled query must still get a
	// connection and complete under a short deadline. This is exactly what deadlocked pre-fix.
	qctx, qcancel := context.WithTimeout(ctx, 5*time.Second)
	defer qcancel()
	if _, err := st.FileProviderProjects(qctx, "sizing-probe"); err != nil {
		t.Fatalf("pooled query did not make progress with %d leaders pinned: %v", providers, err)
	}
}

// TestOpenRejectsTooSmallOperatorCap proves the fail-fast: an EXPLICIT pool_max_conns below
// the requirement makes Open error at startup rather than silently deadlocking.
func TestOpenRejectsTooSmallOperatorCap(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run pool sizing integration test")
	}
	const (
		providers = 15
		reconcile = 4
	)
	capped := withMaxConns(dsn, 8) // 8 is far below RequiredMaxConns(15,4)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.Open(ctx, capped, store.WithFileProviderPool(providers, reconcile))
	if err == nil {
		st.Close()
		t.Fatalf("Open must reject pool_max_conns=8 for %d providers, got nil error", providers)
	}
	if !strings.Contains(err.Error(), "pool_max_conns") {
		t.Fatalf("error should mention the too-small cap, got: %v", err)
	}
}

// withMaxConns appends an explicit pool_max_conns cap to either a URL or a keyword DSN.
func withMaxConns(dsn string, n int) string {
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return fmt.Sprintf("%s%spool_max_conns=%d", dsn, sep, n)
	}
	return fmt.Sprintf("%s pool_max_conns=%d", dsn, n)
}
