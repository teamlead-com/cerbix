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
