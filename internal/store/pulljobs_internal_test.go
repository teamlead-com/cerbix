package store

import (
	"testing"
	"time"
)

func TestPullJobsClaimOnceAndTTL(t *testing.T) {
	st, ctx := outboxTestStore(t)

	// Enqueue three jobs for geo3 and one for core.
	for i := 0; i < 3; i++ {
		if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{"n":`+string(rune('0'+i))+`}`), 60); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if err := st.EnqueuePullJob(ctx, "core", []byte(`{"c":1}`), 60); err != nil {
		t.Fatalf("enqueue core: %v", err)
	}

	// Claim only geo3 jobs; core's is untouched.
	got, err := st.ClaimPullJobs(ctx, "geo3", 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("claim geo3 = %d jobs err=%v (want 3)", len(got), err)
	}
	// A second claim returns nothing (delivered exactly once).
	if again, _ := st.ClaimPullJobs(ctx, "geo3", 10); len(again) != 0 {
		t.Fatalf("re-claim returned %d, want 0 (delivered once)", len(again))
	}
	if core, _ := st.ClaimPullJobs(ctx, "core", 10); len(core) != 1 {
		t.Fatalf("core claim = %d, want 1 (region isolation)", len(core))
	}

	// An expired job is never claimed.
	if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{"old":1}`), 60); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE pull_jobs SET expires_at = now() - interval '1 minute' WHERE region='geo3'`); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if exp, _ := st.ClaimPullJobs(ctx, "geo3", 10); len(exp) != 0 {
		t.Fatalf("expired job claimed = %d, want 0", len(exp))
	}
	if n, err := st.PurgeExpiredPullJobs(ctx); err != nil || n != 1 {
		t.Fatalf("purge expired = %d err=%v (want 1)", n, err)
	}
}

func TestPullQueueStats(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{}`), 60); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.EnqueuePullJob(ctx, "geo3", []byte(`{}`), 60); err != nil {
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
