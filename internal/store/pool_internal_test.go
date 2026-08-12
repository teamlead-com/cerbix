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
// queries + app baseline still have connections left.
func TestRequiredMaxConnsFitsMaxProviders(t *testing.T) {
	const (
		maxProviders = 64 // config.maxConfiguredFileProviders
		reconcile    = 4  // cli.maxConcurrentReconciles
	)
	got := int(RequiredMaxConns(maxProviders, reconcile))
	if leftAfterPins := got - maxProviders; leftAfterPins < reconcile+poolAppBaseline {
		t.Fatalf("RequiredMaxConns=%d leaves only %d conns after %d leader pins; need >= reconcile(%d)+baseline(%d)",
			got, leftAfterPins, maxProviders, reconcile, poolAppBaseline)
	}
}

// TestRequiredMaxConnsClampsNegatives guards the arithmetic against nonsense inputs.
func TestRequiredMaxConnsClampsNegatives(t *testing.T) {
	if got := RequiredMaxConns(-5, -5); int(got) != poolMaxConnsFloor {
		t.Fatalf("RequiredMaxConns(-5,-5)=%d, want floor %d", got, poolMaxConnsFloor)
	}
}

// TestDSNSetsMaxConns checks the operator-cap detection used to fail fast on an explicit
// pool_max_conns that is too small (vs. pgx's implicit default, which we raise silently).
func TestDSNSetsMaxConns(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@h:5432/db?pool_max_conns=8",
		"host=h user=u dbname=db pool_max_conns=8",
	} {
		if !dsnSetsMaxConns(dsn) {
			t.Errorf("dsnSetsMaxConns(%q)=false, want true", dsn)
		}
	}
	for _, dsn := range []string{
		"postgres://u:p@h:5432/db",
		"host=h user=u dbname=db sslmode=disable",
	} {
		if dsnSetsMaxConns(dsn) {
			t.Errorf("dsnSetsMaxConns(%q)=true, want false", dsn)
		}
	}
}
