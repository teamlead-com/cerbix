package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// `observed_at >= job_issued_at` (func-result-protocol §9, deferred there with "not here" and
// delivered in iter-0155). Three cases, and the middle one is the owner's decision rather than an
// implementation detail: a result observed BEFORE the job that asked for it is impossible, but a
// region whose clock trails the core's by seconds produces exactly that shape, and dropping those
// results would lose real measurements to fix a bookkeeping detail. So: reject beyond
// `result.allowed_skew`, accept and COUNT inside it, and check nothing at all when the executor
// carried no job identity — a push result, or a fleet older than the field.
func TestAResultCannotPredateTheJobItAnswers(t *testing.T) {
	st, ctx := outboxTestStore(t)
	st.WithResultPolicy(30*time.Second, 24*time.Hour) // the tolerance under test, and a retention wide enough not to interfere
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	now := time.Now().UTC()

	// (a) beyond the tolerance: the result cannot be about the job it names.
	out, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: mon.ID, Ts: now.Add(-5 * time.Minute), Up: true,
		ExecutionRevision: mon.ExecutionRevision,
		JobID:             "job-a", JobIssuedAt: now,
	})
	if err != nil {
		t.Fatalf("record (a): %v", err)
	}
	if out.Reason != ReasonObservedBeforeIssue || out.Inserted || out.Applied {
		t.Fatalf("observed 5m before its job = %+v, want %s and nothing recorded — beyond the tolerance "+
			"a result cannot be about the job it claims to answer", out, ReasonObservedBeforeIssue)
	}

	// (b) inside the tolerance: ACCEPTED, and counted so the drift is visible before it costs data.
	before := metricEventValue(t, st, ctx, metricEventObservedBeforeIssue)
	out, err = st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: mon.ID, Ts: now.Add(-5 * time.Second), Up: true,
		ExecutionRevision: mon.ExecutionRevision,
		JobID:             "job-b", JobIssuedAt: now,
	})
	if err != nil {
		t.Fatalf("record (b): %v", err)
	}
	if !out.Inserted || out.Reason != "" {
		t.Fatalf("observed 5s before its job = %+v, want it INSERTED: a region trailing the core by "+
			"seconds is ordinary, and dropping it would lose a real measurement", out)
	}
	if after := metricEventValue(t, st, ctx, metricEventObservedBeforeIssue); after != before+1 {
		t.Errorf("counter %d → %d, want +1 — an accepted out-of-order result must still be VISIBLE, "+
			"or the drift grows silently until it starts costing measurements", before, after)
	}

	// (c) no job identity: nothing is checked. A push result and any executor older than this field
	// must keep working exactly as before.
	out, err = st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: mon.ID, Ts: now.Add(-time.Hour), Up: true,
		ExecutionRevision: mon.ExecutionRevision,
	})
	if err != nil {
		t.Fatalf("record (c): %v", err)
	}
	if out.Reason == ReasonObservedBeforeIssue {
		t.Errorf("a result carrying no job identity was judged against one: %+v — zero means "+
			"'not carried', never 'issued at the epoch'", out)
	}
}

// metricEventValue reads one persisted event aggregate, so a test can assert that a counter moved
// with the transaction that counted it rather than trusting an exported gauge.
func metricEventValue(t *testing.T, st *Store, ctx context.Context, kind string) int64 {
	t.Helper()
	var v int64
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE(value, 0) FROM service_metric_events WHERE kind = $1`, kind).Scan(&v); err != nil {
		return 0
	}
	return v
}
