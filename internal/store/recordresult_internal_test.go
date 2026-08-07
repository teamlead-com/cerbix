package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestRecordScheduledResultPipeline exercises the ordered ingest pipeline
// (spec func-result-protocol §4) end-to-end over the real DB: missing timestamp,
// fresh apply, duplicate dedup, future-beyond-skew quarantine, outside-retention ignore,
// out-of-order SLA-only, and a strictly-newer fresh recovery — asserting both the
// ResultOutcome and the persisted heartbeat rows.
func TestRecordScheduledResultPipeline(t *testing.T) {
	st, ctx := outboxTestStore(t)
	st.WithResultPolicy(5*time.Minute, 30*time.Minute)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "r", Type: domain.MonitorTCP, Target: "10.0.0.1:80",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	hbCount := func() int {
		t.Helper()
		var n int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id = $1`, mon.ID).Scan(&n); err != nil {
			t.Fatalf("count heartbeats: %v", err)
		}
		return n
	}
	rec := func(hb domain.Heartbeat) ResultOutcome {
		t.Helper()
		// Stamp the monitor's current config generation so the revision gate passes; this
		// test exercises the timestamp pipeline, not the revision gate (see TestRevisionGate).
		if !hb.Ts.IsZero() {
			hb.ExecutionRevision = mon.ExecutionRevision
		}
		o, err := st.RecordScheduledResult(ctx, hb)
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		return o
	}

	now := time.Now().UTC().Truncate(time.Millisecond)

	// 0) Missing timestamp → fail-closed reject, no insert.
	if o := rec(domain.Heartbeat{MonitorID: mon.ID, Up: true}); o.Reason != ReasonMissingTimestamp || o.Inserted || o.Applied {
		t.Fatalf("missing ts: %+v, want reject/no-insert", o)
	}
	if hbCount() != 0 {
		t.Fatalf("missing ts must not insert: hb=%d", hbCount())
	}

	// 1) Fresh DOWN at now → applied, flips down (threshold 1); observed_at == ts.
	if o := rec(domain.Heartbeat{MonitorID: mon.ID, Ts: now, Up: false, Code: 500}); !o.Applied || o.Cur != domain.StatusDown || !o.Inserted {
		t.Fatalf("fresh down: %+v", o)
	}
	var obs *time.Time
	_ = st.pool.QueryRow(ctx, `SELECT observed_at FROM heartbeats WHERE monitor_id=$1 AND ts=$2`, mon.ID, now).Scan(&obs)
	if obs == nil || !obs.Equal(now) {
		t.Fatalf("observed_at should equal ts for scheduled: got %v", obs)
	}

	// 2) Duplicate of now → no heartbeat, not applied, counter untouched.
	if o := rec(domain.Heartbeat{MonitorID: mon.ID, Ts: now, Up: false, Code: 500}); o.Reason != ReasonDuplicate || o.Applied || o.Inserted {
		t.Fatalf("duplicate: %+v", o)
	}
	if got, _ := st.GetMonitor(ctx, mon.ID); got.ConsecutiveFailures != 1 {
		t.Fatalf("duplicate must not re-bump failures: %d", got.ConsecutiveFailures)
	}

	// 3) Future beyond skew (now+10m, skew 5m) → quarantine, no insert.
	if o := rec(domain.Heartbeat{MonitorID: mon.ID, Ts: now.Add(10 * time.Minute), Up: true}); o.Reason != ReasonFutureTimestamp || o.Inserted {
		t.Fatalf("future: %+v, want quarantine/no-insert", o)
	}

	// 4) Outside retention (now-1h, retention 30m) → ignore, no insert.
	if o := rec(domain.Heartbeat{MonitorID: mon.ID, Ts: now.Add(-time.Hour), Up: true}); o.Reason != ReasonOutsideRetention || o.Inserted {
		t.Fatalf("outside retention: %+v, want ignore/no-insert", o)
	}
	if hbCount() != 1 {
		t.Fatalf("dup/future/outside must not insert: hb=%d, want 1", hbCount())
	}

	// 5) Out-of-order within retention (now-1m, older than watermark) → SLA-only.
	if o := rec(domain.Heartbeat{MonitorID: mon.ID, Ts: now.Add(-time.Minute), Up: true, Code: 200}); o.Reason != ReasonOutOfOrder || o.Applied || !o.Inserted {
		t.Fatalf("out-of-order: %+v, want SLA-only insert", o)
	}
	if hbCount() != 2 {
		t.Fatalf("out-of-order must insert for SLA: hb=%d, want 2", hbCount())
	}
	if got, _ := st.GetMonitor(ctx, mon.ID); got.Status != domain.StatusDown {
		t.Fatalf("out-of-order up must not override live state: %v", got.Status)
	}

	// 6) Fresh UP at now+1m (within skew, newer than watermark) → recovers.
	if o := rec(domain.Heartbeat{MonitorID: mon.ID, Ts: now.Add(time.Minute), Up: true, Code: 200}); !o.Applied || o.Prev != domain.StatusDown || o.Cur != domain.StatusUp {
		t.Fatalf("fresh up: %+v", o)
	}
	if hbCount() != 3 {
		t.Fatalf("after fresh up: hb=%d, want 3", hbCount())
	}
}

// TestRecordPushResult proves the push entrypoint (spec §4): ordering by the ingress
// received_at, observed_at NULL (no client ts), last_result_ts advanced, and the
// current-state re-check dropping a ping to a now-disabled monitor.
func TestRecordPushResult(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "cron", Type: domain.MonitorPush,
		IntervalSeconds: 3600, FailureThreshold: 1, Enabled: true, PushToken: "cbxp_x",
	})
	if err != nil {
		t.Fatalf("create push monitor: %v", err)
	}
	recv := time.Now().UTC().Truncate(time.Millisecond)

	// A down ping applies (threshold 1), ts = received_at, observed_at NULL.
	o, err := st.RecordPushResult(ctx, mon.ID, false, "timeout", recv, time.Time{})
	if err != nil || !o.Applied || o.Cur != domain.StatusDown || !o.Inserted {
		t.Fatalf("push down: %+v err=%v", o, err)
	}
	var gotTs time.Time
	var obs *time.Time
	_ = st.pool.QueryRow(ctx, `SELECT ts, observed_at FROM heartbeats WHERE monitor_id=$1`, mon.ID).Scan(&gotTs, &obs)
	if !gotTs.Equal(recv) {
		t.Fatalf("push ts = %v, want received_at %v", gotTs, recv)
	}
	if obs != nil {
		t.Fatalf("observed_at must be NULL when no client timestamp: got %v", obs)
	}
	// last_result_ts advanced to received_at (so the dead-man CAS sees fresh pings).
	var last *time.Time
	_ = st.pool.QueryRow(ctx, `SELECT last_result_ts FROM monitors WHERE id=$1`, mon.ID).Scan(&last)
	if last == nil || !last.Equal(recv) {
		t.Fatalf("last_result_ts = %v, want received_at %v", last, recv)
	}

	// Disable the monitor → a later ping is dropped by the current-state re-check.
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET enabled = false WHERE id=$1`, mon.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	o, err = st.RecordPushResult(ctx, mon.ID, true, "ok", recv.Add(time.Minute), time.Time{})
	if err != nil || o.Applied || o.Inserted || o.Reason != "" {
		t.Fatalf("push to disabled monitor must be dropped: %+v err=%v", o, err)
	}

	// A ping to a nonexistent monitor is ErrNotFound.
	if _, err := st.RecordPushResult(ctx, "00000000-0000-0000-0000-000000000000", true, "", recv, time.Time{}); err != ErrNotFound {
		t.Fatalf("push to missing monitor err = %v, want ErrNotFound", err)
	}
}

// TestRevisionGate proves the execution_revision gate (spec §3): a matching revision
// applies; a mismatched one is always rejected (stale_revision, no insert); a missing one
// is rejected under enforce but tolerated+counted under observe; UpdateMonitor bumps the
// revision and resets the watermark.
func TestRevisionGate(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "r", Type: domain.MonitorTCP, Target: "10.0.0.1:80",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if mon.ExecutionRevision != 1 {
		t.Fatalf("new monitor execution_revision = %d, want 1", mon.ExecutionRevision)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	hb := func(rev int64, ts time.Time) domain.Heartbeat {
		return domain.Heartbeat{MonitorID: mon.ID, Ts: ts, Up: true, ExecutionRevision: rev}
	}

	// enforce (default): mismatched revision → reject, no insert.
	if o, _ := st.RecordScheduledResult(ctx, hb(2, now)); o.Reason != ReasonStaleRevision || o.Inserted {
		t.Fatalf("mismatched revision: %+v, want stale_revision/no-insert", o)
	}
	// enforce: missing revision (0) → reject.
	if o, _ := st.RecordScheduledResult(ctx, hb(0, now.Add(time.Second))); o.Reason != ReasonMissingRevision || o.Inserted {
		t.Fatalf("missing revision (enforce): %+v, want missing_revision/no-insert", o)
	}
	// matching revision → applied.
	if o, _ := st.RecordScheduledResult(ctx, hb(1, now.Add(2*time.Second))); !o.Applied {
		t.Fatalf("matching revision: %+v, want applied", o)
	}

	// observe mode: missing revision is tolerated (applied) and flagged.
	st.WithResultRevisionMode("observe")
	if o, _ := st.RecordScheduledResult(ctx, hb(0, now.Add(3*time.Second))); !o.Applied || !o.MissingRevisionObserved {
		t.Fatalf("missing revision (observe): %+v, want applied + MissingRevisionObserved", o)
	}
	// A PRESENT mismatch is still rejected even under observe.
	if o, _ := st.RecordScheduledResult(ctx, hb(99, now.Add(4*time.Second))); o.Reason != ReasonStaleRevision {
		t.Fatalf("observe present-mismatch: %+v, want stale_revision", o)
	}
	st.WithResultRevisionMode("enforce")

	// UpdateMonitor bumps the revision and resets the watermark.
	mon.Name = "renamed"
	up, err := st.UpdateMonitor(ctx, mon)
	if err != nil || up.ExecutionRevision != 2 {
		t.Fatalf("after update, execution_revision = %d err=%v, want 2", up.ExecutionRevision, err)
	}
	var last *time.Time
	_ = st.pool.QueryRow(ctx, `SELECT last_result_ts FROM monitors WHERE id=$1`, mon.ID).Scan(&last)
	if last != nil {
		t.Fatalf("update must reset last_result_ts, got %v", last)
	}
	// Old-generation result now rejected; new-generation applies.
	if o, _ := st.RecordScheduledResult(ctx, hb(1, now.Add(5*time.Second))); o.Reason != ReasonStaleRevision {
		t.Fatalf("old-gen result after bump: %+v, want stale_revision", o)
	}
	if o, _ := st.RecordScheduledResult(ctx, hb(2, now.Add(6*time.Second))); !o.Applied {
		t.Fatalf("new-gen result after bump: %+v, want applied", o)
	}
}
