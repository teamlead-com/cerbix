package store

import (
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestPullJobsClaimLeaseAckReclaimAndTTL(t *testing.T) {
	st, ctx := outboxTestStore(t)

	// Enqueue three jobs for geo3 and one for core.
	for i := 0; i < 3; i++ {
		if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{"n":`+string(rune('0'+i))+`}`), 60, 0, ""); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if err := st.EnqueuePullJob(ctx, "core", []byte(`{"c":1}`), 60, 0, ""); err != nil {
		t.Fatalf("enqueue core: %v", err)
	}

	// Claim only geo3 jobs (leases them); core's is untouched.
	got, err := st.ClaimPullJobs(ctx, "geo3", 10, 30, nil)
	if err != nil || len(got) != 3 {
		t.Fatalf("claim geo3 = %d jobs err=%v (want 3)", len(got), err)
	}
	for _, j := range got {
		if j.Token == "" {
			t.Fatal("claimed job has no lease token")
		}
	}
	// A second claim returns nothing while the lease is live (not re-delivered).
	if again, _ := st.ClaimPullJobs(ctx, "geo3", 10, 30, nil); len(again) != 0 {
		t.Fatalf("re-claim under live lease returned %d, want 0", len(again))
	}
	if core, _ := st.ClaimPullJobs(ctx, "core", 10, 30, nil); len(core) != 1 {
		t.Fatalf("core claim = %d, want 1 (region isolation)", len(core))
	}

	// Ack two of the three: those are deleted, the third remains leased.
	if err := st.AckPullJobs(ctx, []string{got[0].Token, got[1].Token}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	var remaining int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM pull_jobs WHERE region='geo3'`).Scan(&remaining)
	if remaining != 1 {
		t.Fatalf("after acking 2 of 3, %d geo3 jobs remain, want 1", remaining)
	}
	// A stale token (the acked one) is a harmless no-op.
	if err := st.AckPullJobs(ctx, []string{got[0].Token}); err != nil {
		t.Fatalf("stale ack should be a no-op: %v", err)
	}

	// The un-acked job's lease lapses → it becomes claimable again (crash recovery),
	// with a fresh token distinct from the original lease.
	if _, err := st.pool.Exec(ctx, `UPDATE pull_jobs SET lease_expires_at = now() - interval '1 second' WHERE region='geo3'`); err != nil {
		t.Fatalf("lapse lease: %v", err)
	}
	reclaimed, err := st.ClaimPullJobs(ctx, "geo3", 10, 30, nil)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaim after lease lapse = %d err=%v (want 1)", len(reclaimed), err)
	}
	if reclaimed[0].Token == got[2].Token {
		t.Fatal("reclaim must mint a fresh token so the old lease's late ack cannot delete it")
	}

	// An expired (TTL) job is never claimed, and is purged.
	if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{"old":1}`), 60, 0, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE pull_jobs SET expires_at = now() - interval '1 minute' WHERE region='geo3'`); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if exp, _ := st.ClaimPullJobs(ctx, "geo3", 10, 30, nil); len(exp) != 0 {
		t.Fatalf("expired job claimed = %d, want 0", len(exp))
	}
	if n, err := st.PurgeExpiredPullJobs(ctx); err != nil || n != 2 {
		t.Fatalf("purge expired = %d err=%v (want 2)", n, err)
	}
}

func TestPullQueueStats(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{}`), 60, 0, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{}`), 60, 0, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Backdate one so lag reflects the oldest.
	if _, err := st.pool.Exec(ctx, `UPDATE pull_jobs SET created_at = now() - interval '90 seconds' WHERE region='geo3'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	stats, err := st.PullQueueStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(stats) != 1 || stats[0].Region != "geo3" || stats[0].Pending != 2 || stats[0].LagSeconds < 80 {
		t.Fatalf("stats = %#v, want geo3 pending=2 lag>=80", stats)
	}
	// Expired jobs are excluded.
	if _, err := st.pool.Exec(ctx, `UPDATE pull_jobs SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if stats, _ := st.PullQueueStats(ctx); len(stats) != 0 {
		t.Fatalf("expired jobs counted: %#v", stats)
	}
}

func TestPullProtocolClaimsArePhysicallySeparated(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.EnqueuePullJobV2(ctx, "secure", []byte(`{"protocol":2}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	if jobs, err := st.ClaimPullJobs(ctx, "secure", 10, 30, nil); err != nil || len(jobs) != 0 {
		t.Fatalf("v1 claim saw v2 row: jobs=%d err=%v", len(jobs), err)
	}
	jobs, err := st.ClaimPullJobsV2(ctx, "secure", 10, 30, nil)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("v2 claim: jobs=%d err=%v", len(jobs), err)
	}
}

func TestCredentialReadyAgentRegionIsExistential(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.RecordAgentCapabilities(ctx, "secure", "legacy", 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.LiveCredentialReadyAgentRegions(ctx, time.Minute, 1); err != nil || ready["secure"] {
		t.Fatalf("legacy-only region reported ready: %#v err=%v", ready, err)
	}
	if err := st.RecordAgentCapabilities(ctx, "secure", "v2-degraded", 1, false, nil); err != nil {
		t.Fatal(err)
	}
	if ready, _ := st.LiveCredentialReadyAgentRegions(ctx, time.Minute, 1); ready["secure"] {
		t.Fatalf("degraded v2 agent reported ready: %#v", ready)
	}
	if err := st.RecordAgentCapabilities(ctx, "secure", "v2-ready", 1, true, nil); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.LiveCredentialReadyAgentRegions(ctx, time.Minute, 1); err != nil || !ready["secure"] {
		t.Fatalf("ready v2 agent not discovered: %#v err=%v", ready, err)
	}
}

func TestAgentHeartbeatLiveRegions(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.RecordAgentHeartbeat(ctx, "geo3", "agent-a"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := st.RecordAgentHeartbeat(ctx, "geo3", "agent-a"); err != nil { // idempotent upsert
		t.Fatalf("heartbeat 2: %v", err)
	}
	live, err := st.LiveAgentRegions(ctx, time.Minute)
	if err != nil || !live["geo3"] {
		t.Fatalf("live = %#v err=%v, want geo3", live, err)
	}
	// A stale heartbeat drops out of the window.
	if _, err := st.pool.Exec(ctx, `UPDATE agent_heartbeats SET seen_at = now() - interval '10 minutes'`); err != nil {
		t.Fatalf("age: %v", err)
	}
	if live, _ := st.LiveAgentRegions(ctx, time.Minute); live["geo3"] {
		t.Fatalf("stale heartbeat still live: %#v", live)
	}

	// Housekeeping: a 10-minute-old row is purged past a 1-hour age? No — it's younger.
	if n, err := st.PurgeStaleAgentHeartbeats(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("purge <1h = %d err=%v (want 0)", n, err)
	}
	// Age it past the threshold → purged; a fresh agent row is kept.
	if err := st.RecordAgentHeartbeat(ctx, "geo3", "agent-live"); err != nil {
		t.Fatalf("fresh heartbeat: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE agent_heartbeats SET seen_at = now() - interval '2 hours' WHERE agent_id = 'agent-a'`); err != nil {
		t.Fatalf("age old: %v", err)
	}
	if n, err := st.PurgeStaleAgentHeartbeats(ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("purge stale = %d err=%v (want 1)", n, err)
	}
	if live, _ := st.LiveAgentRegions(ctx, time.Minute); !live["geo3"] {
		t.Fatalf("live agent must survive the purge: %#v", live)
	}
}

// TestCapableClaimLeasesEveryGenerationAtOrBelowCapability is the regression for the r7
// availability blocker (D-0160): the pull barrier must be ONE-directional. Non-credentialed
// monitors are enqueued as generation-1 rows, and an `enforced` region's agent necessarily
// holds a dispatch keyring — so a capable claim that returned only its own generation left
// every ordinary monitor's row to expire by TTL: no probe, no heartbeat, no DOWN, no alert.
// Enabling a security feature must never silently disable monitoring.
func TestCapableClaimLeasesEveryGenerationAtOrBelowCapability(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.EnqueuePullJob(ctx, "secure", []byte(`{"protocol":1}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueuePullJobV2(ctx, "secure", []byte(`{"protocol":2}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimPullJobsV2(ctx, "secure", 10, 30, nil)
	if err != nil {
		t.Fatalf("capable claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("capable claim leased %d rows, want both generations (the blackhole regression)", len(claimed))
	}
	// The generation is stamped by the SERVER on every row: a capable claim mixes
	// generations, and the executor must never read the generation out of the payload.
	gens := map[int]bool{}
	for _, job := range claimed {
		gens[job.ProtocolVersion] = true
	}
	if !gens[1] || !gens[2] {
		t.Fatalf("stamped generations = %#v, want both 1 and 2", gens)
	}
}

// The other direction of the same barrier: an incapable claim must still never see a newer
// generation. Widening the capable claim must not widen this one.
func TestGeneration1ClaimNeverSeesNewerGeneration(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.EnqueuePullJobV2(ctx, "secure", []byte(`{"protocol":2}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueuePullJob(ctx, "secure", []byte(`{"protocol":1}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimPullJobs(ctx, "secure", 10, 30, nil)
	if err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ProtocolVersion != 1 {
		t.Fatalf("legacy claim leased %d rows (first gen=%d), want exactly the generation-1 row",
			len(claimed), firstGeneration(claimed))
	}
}

// One claim, one max, one lease — across generations, not per generation. Two independent
// per-generation claims would each honour max separately and over-lease past the window the
// agent can actually finish in.
func TestCapableClaimSharesOneMaxAcrossGenerations(t *testing.T) {
	st, ctx := outboxTestStore(t)
	for i := 0; i < 2; i++ {
		if err := st.EnqueuePullJob(ctx, "secure", []byte(`{"protocol":1}`), 60, 0, ""); err != nil {
			t.Fatal(err)
		}
		if err := st.EnqueuePullJobV2(ctx, "secure", []byte(`{"protocol":2}`), 60, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := st.ClaimPullJobsV2(ctx, "secure", 3, 30, nil)
	if err != nil {
		t.Fatalf("capable claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("capable claim leased %d rows under max=3: the max must be shared across generations", len(claimed))
	}
}

// Fairness: rows compete on age, never on which generation they belong to, so neither
// generation can starve the other under sustained load.
func TestCapableClaimOrdersByAgeNotGeneration(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.EnqueuePullJobV2(ctx, "secure", []byte(`{"protocol":2}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueuePullJob(ctx, "secure", []byte(`{"protocol":1}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	// Make the ordering deterministic regardless of insert-timestamp resolution: the
	// generation-2 row is the older one.
	if _, err := st.pool.Exec(ctx,
		`UPDATE pull_jobs SET created_at = now() - interval '1 minute' WHERE protocol_version = 2`); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimPullJobsV2(ctx, "secure", 1, 30, nil)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("capable claim: rows=%d err=%v", len(claimed), err)
	}
	if claimed[0].ProtocolVersion != 2 {
		t.Fatalf("oldest row not served first: got generation %d, want 2", claimed[0].ProtocolVersion)
	}
}

func firstGeneration(jobs []PullJob) int {
	if len(jobs) == 0 {
		return 0
	}
	return jobs[0].ProtocolVersion
}

// FR-029 invariant 6, storage half: what a region's LIVE agents announced, unioned, and nothing
// else. The union matters because a region is as capable as its most capable live agent — the
// in-flight cap, not the announcement, is what keeps one agent from being handed everything.
func TestLiveCanaryAgentCapabilitiesUnionsOnlyLiveAgents(t *testing.T) {
	st, ctx := outboxTestStore(t)
	token := domain.CanaryCapabilityOfThisBinary()

	if err := st.RecordAgentCapabilities(ctx, "geo1", "old", 2, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAgentCapabilities(ctx, "geo1", "new", 2, true, []string{token}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAgentCapabilities(ctx, "geo2", "skewed", 2, true, []string{"async_transaction_v1@2"}); err != nil {
		t.Fatal(err)
	}
	byRegion, err := st.LiveCanaryAgentCapabilities(ctx, time.Minute)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if got := byRegion["geo1"]; len(got) != 1 || got[0] != token {
		t.Fatalf("geo1 = %#v, want exactly the announced token (the silent agent adds nothing)", got)
	}
	// A region whose only runner speaks another version is NOT the same as a region with none: the
	// caller names two different reasons from this difference.
	if got := byRegion["geo2"]; len(got) != 1 || got[0] != "async_transaction_v1@2" {
		t.Fatalf("geo2 = %#v, want the skewed token preserved", got)
	}

	// Liveness is the whole point of the window: an announcement from an agent that stopped
	// heartbeating is not evidence that anything there can run a canary NOW. Aged in the table
	// rather than by shrinking the window, because a non-positive window falls back to the
	// function's own default and would prove nothing about staleness.
	if _, err := st.pool.Exec(ctx,
		`UPDATE agent_heartbeats SET seen_at = now() - interval '10 minutes' WHERE region = 'geo1'`); err != nil {
		t.Fatalf("age the heartbeat: %v", err)
	}
	stale, err := st.LiveCanaryAgentCapabilities(ctx, time.Minute)
	if err != nil {
		t.Fatalf("stale window: %v", err)
	}
	if len(stale["geo1"]) != 0 {
		t.Fatalf("a stale agent still announced %#v", stale["geo1"])
	}
	if len(stale["geo2"]) != 1 {
		t.Fatalf("the live region stopped announcing: %#v", stale)
	}
}

// FR-029 invariant 6 on the PULL transport. The scheduler already refuses to enqueue into a region
// that announced nothing, but that is not the whole barrier: in a mixed fleet the region DOES
// announce (the new agent did), and the old agent claiming from the same table would take the canary
// and fail it. A capability check does not stop a consumer from consuming — the claim has to filter.
func TestAClaimTakesOnlyTheCapabilitiesItDeclared(t *testing.T) {
	st, ctx := outboxTestStore(t)
	token := domain.CanaryCapabilityOfThisBinary()

	if err := st.EnqueuePullJob(ctx, "geo9", []byte(`{"ordinary":1}`), 60, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueuePullJob(ctx, "geo9", []byte(`{"canary":1}`), 60, 0, token); err != nil {
		t.Fatal(err)
	}

	// The old agent: declares nothing, and must see only the job that requires nothing. It must
	// still see THAT one — a barrier that starved the ordinary path would be the worse bug.
	old, err := st.ClaimPullJobs(ctx, "geo9", 10, 30, nil)
	if err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	if len(old) != 1 || !strings.Contains(string(old[0].Payload), "ordinary") {
		t.Fatalf("legacy claim = %#v, want exactly the ordinary job", old)
	}

	// The wrong version is not a substitute for the right one.
	if skewed, _ := st.ClaimPullJobs(ctx, "geo9", 10, 30, []string{"async_transaction_v1@2"}); len(skewed) != 0 {
		t.Fatalf("a v2 agent claimed %d job(s) that require v1", len(skewed))
	}

	// The capable agent takes it.
	capable, err := st.ClaimPullJobs(ctx, "geo9", 10, 30, []string{token})
	if err != nil {
		t.Fatalf("capable claim: %v", err)
	}
	if len(capable) != 1 || !strings.Contains(string(capable[0].Payload), "canary") {
		t.Fatalf("capable claim = %#v, want the canary", capable)
	}
}
