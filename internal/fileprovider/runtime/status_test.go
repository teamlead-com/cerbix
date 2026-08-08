package runtime

import "testing"

// TestStatusRegistryConfiguredButIdle verifies that a provider registered at New (before any
// reconcile) is already visible in the snapshot with Leader=false and zero counts — spec §15's
// "configured-but-empty providers must surface" requirement.
func TestStatusRegistryConfiguredButIdle(t *testing.T) {
	r := NewStatusRegistry()
	r.register("platform", "instance", "", "")

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	s := snap[0]
	if s.Provider != "platform" || s.ScopeType != "instance" {
		t.Fatalf("unexpected entry: %+v", s)
	}
	if s.Leader || s.LastScanUnix != 0 || s.LastSuccessUnix != 0 || s.Managed != 0 || s.LastError != "" {
		t.Fatalf("idle provider should have zero/false fields, got %+v", s)
	}
}

// TestStatusRegistryUpdatePreservesScopeAndLeader checks that update() keeps the scope and
// leadership set earlier and that LastSuccessUnix is sticky (a later failed reconcile does not
// clear the last known good success timestamp).
func TestStatusRegistryUpdatePreservesScopeAndLeader(t *testing.T) {
	r := NewStatusRegistry()
	r.register("team", "organization", "acme", "")
	r.setLeader("team", true)

	// Successful reconcile.
	r.update("team", 100, 100, "", 5, 1, 0)
	s := r.Snapshot()[0]
	if !s.Leader || s.ScopeType != "organization" || s.ScopeOrg != "acme" {
		t.Fatalf("scope/leader not preserved through update: %+v", s)
	}
	if s.LastSuccessUnix != 100 || s.Managed != 5 || s.Orphaned != 1 {
		t.Fatalf("unexpected post-success state: %+v", s)
	}

	// Later failed reconcile: LastSuccessUnix must stay 100, error surfaces, scan advances.
	r.update("team", 200, 0, "scan_failed", 5, 1, 1)
	s = r.Snapshot()[0]
	if s.LastScanUnix != 200 || s.LastSuccessUnix != 100 {
		t.Fatalf("failed reconcile must advance scan but keep last success, got scan=%d success=%d", s.LastScanUnix, s.LastSuccessUnix)
	}
	if s.LastError != "scan_failed" || s.Rejected != 1 {
		t.Fatalf("failed reconcile should record error/rejected, got %+v", s)
	}
	if !s.Leader {
		t.Fatalf("leadership must survive a failed reconcile update")
	}
}

// TestStatusRegistrySnapshotSorted confirms deterministic (name-sorted) snapshot ordering.
func TestStatusRegistrySnapshotSorted(t *testing.T) {
	r := NewStatusRegistry()
	r.register("zeta", "instance", "", "")
	r.register("alpha", "instance", "", "")
	r.register("mid", "instance", "", "")
	snap := r.Snapshot()
	if len(snap) != 3 || snap[0].Provider != "alpha" || snap[1].Provider != "mid" || snap[2].Provider != "zeta" {
		t.Fatalf("snapshot not sorted by name: %+v", snap)
	}
}
