package store

import (
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 D9 / D9a. The two rules the in-flight table exists for, and the recovery that keeps a
// crashed executor from parking a monitor forever.
func TestCanaryInflightHoldsOneRunPerMonitorAndBoundsTheRegion(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	mk := func(name string) string {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: name, Type: domain.MonitorHTTP, Target: "https://x",
			IntervalSeconds: 300, TimeoutSeconds: 60, Enabled: true, Region: "eu",
		})
		if err != nil {
			t.Fatalf("create monitor %s: %v", name, err)
		}
		return m.ID
	}

	first := mk("canary-1")
	if err := st.ClaimCanaryInflight(ctx, first, "eu", "run-1", time.Minute); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The same monitor again is the case that would submit a SECOND external transaction.
	if err := st.ClaimCanaryInflight(ctx, first, "eu", "run-2", time.Minute); !errors.Is(err, ErrCanaryMonitorInFlight) {
		t.Fatalf("second claim of the same monitor = %v, want ErrCanaryMonitorInFlight", err)
	}

	// Fill the region to its cap with other monitors, then the next one is refused as saturated —
	// not queued, because a queue of long journeys is how a region stops answering.
	ids := []string{first}
	for i := 1; i < CanaryRegionLimit; i++ {
		id := mk("canary-fill-" + string(rune('a'+i)))
		ids = append(ids, id)
		if err := st.ClaimCanaryInflight(ctx, id, "eu", "run", time.Minute); err != nil {
			t.Fatalf("fill claim %d: %v", i, err)
		}
	}
	overflow := mk("canary-overflow")
	if err := st.ClaimCanaryInflight(ctx, overflow, "eu", "run", time.Minute); !errors.Is(err, ErrCanaryRegionSaturated) {
		t.Fatalf("claim past the cap = %v, want ErrCanaryRegionSaturated", err)
	}
	// A DIFFERENT region is unaffected: the cap is per region and not global. It needs its own
	// monitor, because the per-monitor lease is global — one monitor is one running journey no
	// matter which region it belongs to, which is what the first version of this test got wrong.
	elsewhere := mk("canary-us")
	if err := st.ClaimCanaryInflight(ctx, elsewhere, "us", "run", time.Minute); err != nil {
		t.Fatalf("another region must have its own capacity: %v", err)
	}

	byRegion, err := st.CanaryInflightRegions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if byRegion["eu"] != CanaryRegionLimit || byRegion["us"] != 1 {
		t.Fatalf("in-flight by region = %v", byRegion)
	}

	// Releasing returns the slot, and releasing again is a no-op rather than an error: the row may
	// have expired while a slow executor was finishing, and failing there would turn a slow probe
	// into an alert about cerbix.
	if err := st.ReleaseCanaryInflight(ctx, first); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.ReleaseCanaryInflight(ctx, first); err != nil {
		t.Fatalf("second release must be a no-op: %v", err)
	}
	if err := st.ClaimCanaryInflight(ctx, overflow, "eu", "run", time.Minute); err != nil {
		t.Fatalf("the freed slot must be claimable: %v", err)
	}
}

func TestCanaryInflightExpiresSoACrashedExecutorDoesNotParkAMonitor(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "canary", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 300, TimeoutSeconds: 60, Enabled: true, Region: "eu",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A claim whose executor never came back: the TTL is what makes the slot recoverable without
	// an operator, and the runbook states the number rather than leaving it to arithmetic.
	if err := st.ClaimCanaryInflight(ctx, m.ID, "eu", "run-1", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimCanaryInflight(ctx, m.ID, "eu", "run-1", time.Minute); !errors.Is(err, ErrCanaryMonitorInFlight) {
		t.Fatalf("while the lease holds, a second claim must be refused: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := st.ClaimCanaryInflight(ctx, m.ID, "eu", "run-2", time.Minute); err != nil {
		t.Fatalf("after the lease expires the monitor must run again: %v", err)
	}
	row, ok, err := st.canaryInflightByMonitor(ctx, m.ID)
	if err != nil || !ok {
		t.Fatalf("row = %v, %v", ok, err)
	}
	if row.RunKey != "run-2" {
		t.Fatalf("run key = %q, want the new run", row.RunKey)
	}
}
