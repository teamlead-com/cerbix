package fileprovider

import "testing"

func planFor(t *testing.T, y string, current []CurrentMonitor) *ProjectPlan {
	t.Helper()
	dp := mustDecode(t, y)
	p, err := Plan(dp, current)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func actionOf(p *ProjectPlan, uid string) Action {
	for _, e := range p.Entries {
		if e.UID == uid {
			return e.Action
		}
	}
	return ""
}

func TestPlanCreateNoopUpdate(t *testing.T) {
	dp := mustDecode(t, validHTTP)
	hash := dp.Monitors["api"].Hash

	// No current state → create.
	if got := actionOf(planFor(t, validHTTP, nil), "api"); got != ActionCreate {
		t.Fatalf("create: got %s", got)
	}
	// Same hash → noop.
	cur := []CurrentMonitor{{UID: "api", Type: "http", Hash: hash}}
	if got := actionOf(planFor(t, validHTTP, cur), "api"); got != ActionNoop {
		t.Fatalf("noop: got %s", got)
	}
	// Different hash → update (semantic).
	stale := []CurrentMonitor{{UID: "api", Type: "http", Hash: "stale"}}
	p := planFor(t, validHTTP, stale)
	if got := actionOf(p, "api"); got != ActionUpdate {
		t.Fatalf("update: got %s", got)
	}
	for _, e := range p.Entries {
		if e.UID == "api" && !e.SemanticChange {
			t.Fatal("update must flag SemanticChange")
		}
	}
}

func TestPlanDependencyOnlyUpdate(t *testing.T) {
	y := "format: 1\norganization: acme\nproject: p\nmonitors:\n" +
		"  a:\n    name: A\n    type: http\n    target: https://a\n    depends_on: [b]\n" +
		"  b:\n    name: B\n    type: http\n    target: https://b\n"
	dp := mustDecode(t, y)
	// Current state matches the semantic hash of 'a' but with NO dependency → dependency_update.
	cur := []CurrentMonitor{
		{UID: "a", Type: "http", Hash: dp.Monitors["a"].Hash, DependsOn: nil},
		{UID: "b", Type: "http", Hash: dp.Monitors["b"].Hash},
	}
	p, err := Plan(dp, cur)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got := actionOf(p, "a"); got != ActionDependencyUpdate {
		t.Fatalf("a: got %s, want dependency_update", got)
	}
	for _, e := range p.Entries {
		if e.UID == "a" && e.SemanticChange {
			t.Fatal("dependency_update must NOT flag SemanticChange (no revision bump)")
		}
	}
	if got := actionOf(p, "b"); got != ActionNoop {
		t.Fatalf("b: got %s, want noop", got)
	}
}

func TestPlanOrphanRestore(t *testing.T) {
	// Owned UID absent from an empty bundle → orphan.
	empty := "format: 1\norganization: acme\nproject: payments\nmonitors: {}\n"
	cur := []CurrentMonitor{{UID: "api", Type: "http", Hash: "h"}}
	if got := actionOf(planFor(t, empty, cur), "api"); got != ActionOrphan {
		t.Fatalf("orphan: got %s", got)
	}
	// Already-orphaned absent UID → not re-planned.
	curOrph := []CurrentMonitor{{UID: "api", Type: "http", Hash: "h", Orphaned: true}}
	if p := planFor(t, empty, curOrph); len(p.Entries) != 0 {
		t.Fatalf("already-orphaned should produce no entry, got %+v", p.Entries)
	}
	// Reappearance of an orphaned UID → restore (same hash → pure restore, no semantic change).
	dp := mustDecode(t, validHTTP)
	restore := []CurrentMonitor{{UID: "api", Type: "http", Hash: dp.Monitors["api"].Hash, Orphaned: true}}
	p := planFor(t, validHTTP, restore)
	if got := actionOf(p, "api"); got != ActionRestore {
		t.Fatalf("restore: got %s", got)
	}
	for _, e := range p.Entries {
		if e.UID == "api" && e.SemanticChange {
			t.Fatal("pure restore (unchanged hash) must not flag SemanticChange")
		}
	}
}

func TestPlanTypeChangeRejected(t *testing.T) {
	// Existing UID owned as tcp, bundle declares http → rejected.
	cur := []CurrentMonitor{{UID: "api", Type: "tcp", Hash: "h"}}
	_, err := Plan(mustDecode(t, validHTTP), cur)
	var be *BundleError
	if err == nil || !asBundle(err, &be) || be.Reason != ReasonTypeChange {
		t.Fatalf("want type_change, got %v", err)
	}
}

// asBundle is a tiny errors.As shim to avoid importing errors in every test.
func asBundle(err error, target **BundleError) bool {
	if e, ok := err.(*BundleError); ok {
		*target = e
		return true
	}
	return false
}
