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
	// A release keyed by the WRONG run frees nothing — which is the whole of P0-3 in one line.
	if err := st.ReleaseCanaryInflight(ctx, first, "run-2"); err != nil {
		t.Fatalf("a mismatched release must not error: %v", err)
	}
	if byRegion, _ := st.CanaryInflightRegions(ctx); byRegion["eu"] != CanaryRegionLimit {
		t.Fatalf("a release for a run that does not hold the row freed it anyway: %v", byRegion)
	}
	if err := st.ReleaseCanaryInflight(ctx, first, "run-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.ReleaseCanaryInflight(ctx, first, "run-1"); err != nil {
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

// P0-3, end to end against the real table: a LATE result from a run whose lease already expired
// must not release the slot a NEWER run is holding. Keyed by monitor alone — which is what the first
// version did — the stale release deletes run 2's row and the next tick starts run 3 beside it,
// which is exactly the concurrency the lease exists to forbid.
func TestAStaleCanaryResultCannotReleaseANewerRunsSlot(t *testing.T) {
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

	// Run 1 claims, then its lease expires the way a crashed executor's would.
	if err := st.ClaimCanaryInflight(ctx, m.ID, "eu", "run-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE canary_inflight SET expires_at = now() - interval '1 second' WHERE monitor_id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	// Run 2 claims the freed slot.
	if err := st.ClaimCanaryInflight(ctx, m.ID, "eu", "run-2", time.Minute); err != nil {
		t.Fatalf("run 2 must be able to claim after run 1's lease lapsed: %v", err)
	}

	// Run 1 finally answers. It must release NOTHING.
	if err := st.ReleaseCanaryInflight(ctx, m.ID, "run-1"); err != nil {
		t.Fatal(err)
	}
	var runKey string
	if err := st.pool.QueryRow(ctx,
		`SELECT run_key FROM canary_inflight WHERE monitor_id = $1`, m.ID).Scan(&runKey); err != nil {
		t.Fatalf("run 2's slot was deleted by run 1's late result: %v", err)
	}
	if runKey != "run-2" {
		t.Fatalf("slot holds run %q, want run-2", runKey)
	}
	// And run 3 is still refused, because run 2 is genuinely in flight.
	if err := st.ClaimCanaryInflight(ctx, m.ID, "eu", "run-3", time.Minute); !errors.Is(err, ErrCanaryMonitorInFlight) {
		t.Fatalf("run 3 = %v, want ErrCanaryMonitorInFlight", err)
	}
}

// A slot keyed by nothing could never be released by key, so it would park its monitor for the whole
// TTL on every run. The claim refuses it loudly instead.
func TestACanaryClaimWithoutARunKeyIsRefused(t *testing.T) {
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
	for _, key := range []string{"", "   "} {
		if err := st.ClaimCanaryInflight(ctx, m.ID, "eu", key, time.Minute); err == nil {
			t.Fatalf("a claim with run key %q was accepted", key)
		}
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM canary_inflight WHERE monitor_id = $1`, m.ID); n != 0 {
		t.Fatalf("a refused claim left %d rows", n)
	}
}
