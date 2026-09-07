package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// B2's store half — what "an ORDINARY monitor outcome" has to mean, asserted against a database.
//
// The scheduler's shortage path used to write through `InsertHeartbeat`, whose docstring in the
// scheduler's own store interface promised that a shortage "becomes an ordinary monitor outcome"
// and that "the monitor's own failure_threshold decides whether that flips its status". The insert
// does none of that: it writes one row and touches no status, no confirmation counter, no
// transition outbox and no service bucket. A canary whose region had no capable executor stayed UP
// while nothing probed it — no alert, no incident, no escalation — and the number the service
// reported was still computed over a monitor that had stopped being measured.
//
// Both halves are asserted here, in one test, because the finding IS the difference between them:
// a heartbeat row exists either way, so a test that looked for the row would have passed on the
// broken version. What separates them is the status, the counter and the outbox.
func TestACanaryShortageResultFlipsTheMonitorAndTheBareInsertDoesNot(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	shortage := func(m domain.Monitor, at time.Time) domain.Heartbeat {
		return domain.Heartbeat{
			MonitorID:         m.ID,
			Ts:                at,
			ExecutionRevision: m.ExecutionRevision,
			Up:                false,
			Msg:               "dispatch: no_capable_runner",
		}
	}

	// The pipeline: one shortage against a threshold of one flips the monitor and enqueues the
	// transition the alerting path consumes.
	viaPipeline, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "canary-pipeline", Type: domain.MonitorHTTP, Target: "https://a",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	before := st.countOutbox(ctx, t, domain.TopicMonitorTransition, "pending")
	out, err := st.RecordScheduledResult(ctx, shortage(viaPipeline, time.Now().UTC()))
	if err != nil {
		t.Fatalf("record shortage through the pipeline: %v", err)
	}
	if !out.Applied {
		t.Fatalf("the shortage was not applied: %+v — it is an ordinary result and the pipeline "+
			"must treat it as one", out)
	}
	if out.Cur != domain.StatusDown {
		t.Errorf("the monitor is %q after a shortage at threshold 1, want down: a run that could "+
			"not be dispatched is a run that did not happen", out.Cur)
	}
	if after := st.countOutbox(ctx, t, domain.TopicMonitorTransition, "pending"); after != before+1 {
		t.Errorf("the transition outbox holds %d pending events, want %d: without one, no alert, "+
			"no incident and no escalation ever fires for a monitor nothing is probing",
			after, before+1)
	}

	// The bare insert, on the identical heartbeat. This is what the shortage used to do, and it is
	// asserted rather than described: the row lands and NOTHING else moves.
	viaInsert, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "canary-bare", Type: domain.MonitorHTTP, Target: "https://b",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	bareBefore := st.countOutbox(ctx, t, domain.TopicMonitorTransition, "pending")
	if err := st.InsertHeartbeat(ctx, shortage(viaInsert, time.Now().UTC())); err != nil {
		t.Fatalf("bare insert: %v", err)
	}
	var status string
	var failures int
	if err := st.pool.QueryRow(ctx,
		`SELECT status, consecutive_failures FROM monitors WHERE id = $1`, viaInsert.ID).
		Scan(&status, &failures); err != nil {
		t.Fatalf("read monitor after the bare insert: %v", err)
	}
	if domain.MonitorStatus(status) == domain.StatusDown || failures != 0 {
		t.Fatalf("the bare insert moved status=%s failures=%d — if it did, this test no longer "+
			"describes the difference the finding is about", status, failures)
	}
	if after := st.countOutbox(ctx, t, domain.TopicMonitorTransition, "pending"); after != bareBefore {
		t.Fatalf("the bare insert enqueued a transition; it does not, which is the whole finding")
	}
	// And the row IS there in both cases, which is why looking for the row proved nothing.
	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM heartbeats WHERE monitor_id = $1`, viaInsert.ID).Scan(&rows); err != nil {
		t.Fatalf("count heartbeats: %v", err)
	}
	if rows != 1 {
		t.Errorf("the bare insert wrote %d rows, want 1 — a row exists in both the broken and the "+
			"fixed version, and that is precisely why this was invisible", rows)
	}
}
