package store

import "testing"

// TestRequiredMaxConns proves the pool is always sized to fit one pinned leader
// connection per configured file provider PLUS reconcile and app headroom, and never
// drops below the floor — so no provider starves on pool.Acquire and reconcile queries
// still get a connection.
func TestRequiredMaxConns(t *testing.T) {
	const reconcile = 4 // matches cli.maxConcurrentReconciles
	cases := []struct {
		name      string
		providers int
	}{
		{"none", 0},
		{"one", 1},
		{"floor-boundary", poolMaxConnsFloor},
		{"max-configured", 64}, // config.maxConfiguredFileProviders
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := int(RequiredMaxConns(tc.providers, reconcile))
			// Must fit every leader pin plus the reconcile-query headroom.
			if want := tc.providers + reconcile; got < want {
				t.Errorf("RequiredMaxConns(%d,%d)=%d, want >= providers+headroom=%d",
					tc.providers, reconcile, got, want)
			}
			// Must never regress below the floor.
			if got < poolMaxConnsFloor {
				t.Errorf("RequiredMaxConns(%d,%d)=%d, want >= floor %d",
					tc.providers, reconcile, got, poolMaxConnsFloor)
			}
		})
	}
}

// TestRequiredMaxConnsFitsMaxProviders is the concrete regression for the deadlock: at the
// configured maximum of 64 file providers, every leader can pin a conn AND reconcile
// queries + the pinned-app baseline + query headroom still have connections left.
func TestRequiredMaxConnsFitsMaxProviders(t *testing.T) {
	const (
		maxProviders = 64 // config.maxConfiguredFileProviders
		reconcile    = 4  // cli.maxConcurrentReconciles
	)
	got := int(RequiredMaxConns(maxProviders, reconcile))
	if leftAfterPins := got - maxProviders; leftAfterPins < reconcile+poolPinnedAppBaseline+poolQueryHeadroom {
		t.Fatalf("RequiredMaxConns=%d leaves only %d conns after %d leader pins; need >= reconcile(%d)+pinnedBaseline(%d)+queryHeadroom(%d)",
			got, leftAfterPins, maxProviders, reconcile, poolPinnedAppBaseline, poolQueryHeadroom)
	}
}

// TestRequiredMaxConnsClampsNegatives guards the arithmetic against nonsense inputs.
func TestRequiredMaxConnsClampsNegatives(t *testing.T) {
	if got := RequiredMaxConns(-5, -5); int(got) != poolMaxConnsFloor {
		t.Fatalf("RequiredMaxConns(-5,-5)=%d, want floor %d", got, poolMaxConnsFloor)
	}
}

// TestDSNSetsMaxConns checks the STRUCTURAL operator-cap detection: an explicit
// pool_max_conns key (URL query or keyword form) must be found, but the same literal
// appearing inside a username, password, or dbname must NOT be mistaken for a cap — a
// substring scan would false-positive there and make Open falsely reject a valid DSN.
func TestDSNSetsMaxConns(t *testing.T) {
	positives := []struct {
		name, dsn string
	}{
		{"url query", "postgres://u:p@h:5432/db?pool_max_conns=8"},
		{"url query with other opts", "postgresql://u:p@h:5432/db?sslmode=require&pool_max_conns=20"},
		{"keyword form", "host=h user=u dbname=db pool_max_conns=8"},
		{"keyword form reordered", "pool_max_conns=8 host=h user=u dbname=db"},
		// libpq grammar a hand-rolled scan gets wrong — must still detect the cap:
		{"keyword spaces around equals", "host=h user=u dbname=db pool_max_conns = 8"},
		{"keyword scheme in quoted value", `host=h user=u password='p://q' dbname=db pool_max_conns=5`},
	}
	for _, tc := range positives {
		t.Run("positive/"+tc.name, func(t *testing.T) {
			if !dsnSetsMaxConns(tc.dsn) {
				t.Errorf("dsnSetsMaxConns(%q)=false, want true", tc.dsn)
			}
		})
	}

	negatives := []struct {
		name, dsn string
	}{
		{"url no cap", "postgres://u:p@h:5432/db"},
		{"keyword no cap", "host=h user=u dbname=db sslmode=disable"},
		{"substring in username", "postgres://pool_max_conns:p@h:5432/db"},
		{"substring in password", "postgres://u:pool_max_conns@h:5432/db"},
		{"substring in dbname url", "postgres://u:p@h:5432/pool_max_conns"},
		{"substring in dbname keyword", "host=h user=u dbname=pool_max_conns"},
		{"substring in password keyword", "host=h user=u password=pool_max_conns dbname=db"},
		// literal inside a single-quoted value with spaces is a value, not a cap key:
		{"literal in quoted password value", `host=h user=u password='a b pool_max_conns=8' dbname=db`},
	}
	for _, tc := range negatives {
		t.Run("negative/"+tc.name, func(t *testing.T) {
			if dsnSetsMaxConns(tc.dsn) {
				t.Errorf("dsnSetsMaxConns(%q)=true, want false", tc.dsn)
			}
		})
	}
}
