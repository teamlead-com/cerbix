package store

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-043 §4.1 — the state machine, row by row, at a threshold of 3. Each step names the table row it
// exercises; `last_notified_at` is asserted on every step because §4.2 changes exactly one of its
// cases (a success that leaves the monitor down keeps it) and I7 hangs on that.
func TestRecoveryRunFollowsTheStateMachine(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-state")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-state", 3, 1)

	// Row 1 + I2: pending + success → up at once, whatever the threshold (D7).
	o := m.feed(true)
	w := m.row()
	if o.Prev != domain.StatusPending || o.Cur != domain.StatusUp || o.Recovery != nil {
		t.Fatalf("row 1 (pending+success): outcome %+v, want pending→up with no recovery snapshot", o)
	}
	if w.Status != "up" || w.Successes != 0 || w.StartedAt != nil || w.LastNotified != nil {
		t.Fatalf("row 1: %+v, want up/0/NULL/NULL", w)
	}

	// Row 2: up + success → up, nothing to count.
	o = m.feed(true)
	w = m.row()
	if o.Prev != domain.StatusUp || o.Cur != domain.StatusUp || o.Recovery != nil {
		t.Fatalf("row 2 (up+success): outcome %+v", o)
	}
	if w.Status != "up" || w.Successes != 0 || w.StartedAt != nil || w.LastNotified != nil {
		t.Fatalf("row 2: %+v, want up/0/NULL/NULL", w)
	}

	// Row 6: up + failure → as today (failure_threshold 1: the verdict, last_notified_at stamped).
	o = m.feed(false)
	w = m.row()
	if o.Cur != domain.StatusDown || w.Successes != 0 || w.StartedAt != nil || w.LastNotified == nil {
		t.Fatalf("row 6 (up+failure): outcome %+v row %+v, want down/0/NULL/stamped", o, w)
	}
	notified := w.LastNotified
	seqAtDown := w.StateSequence
	eventsAtDown := len(transitionPayloads(t, st, ctx, m.ID))

	// Row 3: down + success, run 0 → 1 < 3 → stays down, run stamped, last_notified_at KEPT.
	o = m.feed(true)
	w = m.row()
	if o.Prev != domain.StatusDown || o.Cur != domain.StatusDown || o.Recovery != nil {
		t.Fatalf("row 3 (first success of a run): outcome %+v, want down→down and no snapshot", o)
	}
	if w.Status != "down" || w.Successes != 1 || w.StartedAt == nil {
		t.Fatalf("row 3: %+v, want down/1/stamped", w)
	}
	if !sameInstant(w.LastNotified, notified) {
		t.Fatalf("row 3: last_notified_at %v, want it kept at %v (§4.2, I7)", w.LastNotified, notified)
	}
	firstStamp := w.StartedAt

	// Row 3 again: run 1 → 2, the stamp is KEPT, not re-taken.
	m.feed(true)
	w = m.row()
	if w.Status != "down" || w.Successes != 2 || !sameInstant(w.StartedAt, firstStamp) || !sameInstant(w.LastNotified, notified) {
		t.Fatalf("row 3 (second success): %+v, want down/2/stamp %v kept/last_notified %v kept", w, firstStamp, notified)
	}

	// Row 5 + I3: down + failure ends the run; last_notified_at still kept.
	o = m.feed(false)
	w = m.row()
	if o.Cur != domain.StatusDown || w.Successes != 0 || w.StartedAt != nil || !sameInstant(w.LastNotified, notified) {
		t.Fatalf("row 5 (failure mid-run): outcome %+v row %+v, want down/0/NULL/kept", o, w)
	}
	// I4's store half: a run that started and broke moved no state_sequence and enqueued nothing.
	if w.StateSequence != seqAtDown || len(transitionPayloads(t, st, ctx, m.ID)) != eventsAtDown {
		t.Fatalf("a broken run moved state_sequence %d→%d or enqueued a transition", seqAtDown, w.StateSequence)
	}

	// I3: after the break the monitor needs the full threshold again.
	m.feed(true)
	w = m.row()
	if w.Successes != 1 || w.StartedAt == nil || w.StartedAt.Before(*firstStamp) {
		t.Fatalf("restarted run: %+v, want 1 with a fresh stamp not before %v", w, firstStamp)
	}
	restartStamp := w.StartedAt
	if o := m.feed(true); o.Cur != domain.StatusDown {
		t.Fatalf("second success of the new run flipped the monitor: %+v", o)
	}

	// Row 4: run 2 → 3 ≥ 3 → up, columns reset, last_notified_at cleared, snapshot returned.
	o = m.feed(true)
	w = m.row()
	if o.Prev != domain.StatusDown || o.Cur != domain.StatusUp {
		t.Fatalf("row 4 (the M-th success): outcome %+v, want down→up", o)
	}
	if w.Status != "up" || w.Successes != 0 || w.StartedAt != nil || w.LastNotified != nil {
		t.Fatalf("row 4: %+v, want up/0/NULL/NULL", w)
	}
	if o.Recovery == nil || o.Recovery.Run != 3 || o.Recovery.Threshold != 3 || !o.Recovery.FirstOK.Equal(*restartStamp) {
		t.Fatalf("row 4 snapshot = %+v, want run 3, threshold 3, first OK %v (the pre-clear stamp)", o.Recovery, restartStamp)
	}

	// Exactly one transition for the whole episode after the down, and it carries the snapshot.
	events := transitionPayloads(t, st, ctx, m.ID)
	if len(events) != eventsAtDown+1 {
		t.Fatalf("%d transitions after the down verdict, want exactly one (the confirmed recovery)", len(events)-eventsAtDown)
	}
	tr := decodeTransition(t, events[len(events)-1])
	if tr.Prev != domain.StatusDown || tr.Cur != domain.StatusUp {
		t.Fatalf("recovery event %+v, want down→up", tr)
	}
	if snap := tr.Recovery(); snap == nil || snap.Run != 3 || snap.Threshold != 3 || !snap.FirstOK.Equal(*restartStamp) {
		t.Fatalf("recovery event snapshot = %+v, want 3/3 first OK %v (§4.6: the worker renders from the event)", snap, restartStamp)
	}
}

// Row 6 for `pending`: a fresh monitor that fails goes down exactly as today, and a later run is
// measured against its threshold — the threshold applies to down → up only (D7).
func TestRecoveryPendingFailureIsTodaysVerdict(t *testing.T) {
	st, ctx := outboxTestStore(t)
	projectID := recoveryProject(t, st, ctx, "rt-pending")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-pending", 2, 1)

	o := m.feed(false)
	w := m.row()
	if o.Prev != domain.StatusPending || o.Cur != domain.StatusDown || w.Successes != 0 || w.StartedAt != nil || w.LastNotified == nil {
		t.Fatalf("pending+failure: outcome %+v row %+v, want pending→down/0/NULL/stamped", o, w)
	}
	if o := m.feed(true); o.Cur != domain.StatusDown || o.Recovery != nil {
		t.Fatalf("first success at threshold 2: %+v, want still down", o)
	}
	if o := m.feed(true); o.Cur != domain.StatusUp || o.Recovery == nil || o.Recovery.Run != 2 || o.Recovery.Threshold != 2 {
		t.Fatalf("second success at threshold 2: %+v, want up with snapshot 2/2", o)
	}
}

// I1 — at threshold 1 every status, counter, `last_notified_at` and transition payload is what the
// code produced before FR-043. The payloads are compared key by key against the four keys a
// pre-change transition carried, so a recovery field leaking into a threshold-1 event is caught.
func TestRecoveryThresholdOneIsTodaysBehaviour(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-one")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-one", 1, 1)

	type step struct {
		up           bool
		wantPrev     domain.MonitorStatus
		wantCur      domain.MonitorStatus
		wantNotified bool // last_notified_at non-NULL after the step
	}
	// The pre-change expectations: a success is up at once and clears last_notified_at; a failure
	// at failure_threshold 1 is the verdict and stamps it only on a FRESH down.
	steps := []step{
		{true, domain.StatusPending, domain.StatusUp, false},
		{false, domain.StatusUp, domain.StatusDown, true},
		{true, domain.StatusDown, domain.StatusUp, false},
		{true, domain.StatusUp, domain.StatusUp, false},
		{false, domain.StatusUp, domain.StatusDown, true},
		{false, domain.StatusDown, domain.StatusDown, true},
		{true, domain.StatusDown, domain.StatusUp, false},
	}
	var downNotified = m.row().LastNotified
	wantEvents := 0
	for i, s := range steps {
		o := m.feed(s.up)
		w := m.row()
		if o.Prev != s.wantPrev || o.Cur != s.wantCur {
			t.Fatalf("step %d: %s→%s, want %s→%s", i, o.Prev, o.Cur, s.wantPrev, s.wantCur)
		}
		if w.Successes != 0 || w.StartedAt != nil {
			t.Fatalf("step %d: a run exists at threshold 1: %+v", i, w)
		}
		if (w.LastNotified != nil) != s.wantNotified {
			t.Fatalf("step %d: last_notified_at = %v, want non-NULL=%v", i, w.LastNotified, s.wantNotified)
		}
		if s.wantPrev == domain.StatusDown && s.wantCur == domain.StatusDown && !sameInstant(w.LastNotified, downNotified) {
			t.Fatalf("step %d: a repeated failure re-stamped last_notified_at", i)
		}
		if s.wantCur == domain.StatusDown {
			downNotified = w.LastNotified
		}
		// ResultOutcome.Recovery is new and carries every confirmed recovery, N = 1 included; the
		// first-OK of a one-check run is that check's own clock, never before the down verdict.
		confirmed := s.wantPrev == domain.StatusDown && s.wantCur == domain.StatusUp
		switch {
		case confirmed && (o.Recovery == nil || o.Recovery.Run != 1 || o.Recovery.Threshold != 1):
			t.Fatalf("step %d: snapshot %+v, want 1/1 on a confirmed recovery", i, o.Recovery)
		case confirmed && o.Recovery.FirstOK.Before(*downNotified):
			t.Fatalf("step %d: first OK %v is earlier than the down verdict %v", i, o.Recovery.FirstOK, *downNotified)
		case !confirmed && o.Recovery != nil:
			t.Fatalf("step %d: snapshot %+v on a step that is not a confirmed recovery", i, o.Recovery)
		}
		if s.wantPrev != s.wantCur {
			wantEvents++
		}
	}

	events := transitionPayloads(t, st, ctx, m.ID)
	if len(events) != wantEvents {
		t.Fatalf("%d transitions, want %d (one per status change, as before)", len(events), wantEvents)
	}
	for i, e := range events {
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if got := strings.Join(keys, ","); got != "at,cur,monitor_id,prev,seq" {
			t.Fatalf("transition %d carries keys %s, want exactly the pre-change at,cur,monitor_id,prev,seq (I1)", i, got)
		}
	}
}

// D10, I3, I11 — lowering the threshold mid-run flips nothing inside the update; the next success is
// measured against the new value, and the snapshot reads N > M: three successes at 4, lowered to 2,
// confirmed on the fourth, is 4/2.
func TestRecoveryThresholdLoweredMidRun(t *testing.T) {
	st, ctx := outboxTestStore(t)
	withoutRunGuard(t, st, ctx) // this release's own resets / the CHECK itself, not 00118's guard
	projectID := recoveryProject(t, st, ctx, "rt-lower")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-lower", 4, 1)
	m.feed(false)
	for i := 0; i < 3; i++ {
		if o := m.feed(true); o.Cur != domain.StatusDown {
			t.Fatalf("success %d at threshold 4 flipped the monitor: %+v", i+1, o)
		}
	}
	before := m.row()
	if before.Successes != 3 {
		t.Fatalf("run = %d, want 3", before.Successes)
	}
	var outboxBefore int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}

	cur, err := st.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.RecoveryThreshold = 2
	updated, err := st.UpdateMonitor(ctx, cur)
	if err != nil {
		t.Fatalf("lower the threshold: %v", err)
	}
	if updated.RecoveryThreshold != 2 {
		t.Fatalf("threshold after update = %d, want 2", updated.RecoveryThreshold)
	}
	after := m.row()
	if after.Status != "down" || after.Successes != 3 || !sameInstant(after.StartedAt, before.StartedAt) || after.StateSequence != before.StateSequence {
		t.Fatalf("the update touched the run: before %+v after %+v (D10, I11)", before, after)
	}
	var outboxAfter int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if outboxAfter != outboxBefore {
		t.Fatalf("the update enqueued %d outbox events, want none (I11)", outboxAfter-outboxBefore)
	}

	o := m.feed(true)
	if o.Prev != domain.StatusDown || o.Cur != domain.StatusUp {
		t.Fatalf("the next success after lowering: %+v, want down→up", o)
	}
	if o.Recovery == nil || o.Recovery.Run != 4 || o.Recovery.Threshold != 2 || !o.Recovery.FirstOK.Equal(*before.StartedAt) {
		t.Fatalf("snapshot = %+v, want run 4, threshold 2, first OK %v", o.Recovery, before.StartedAt)
	}
	events := transitionPayloads(t, st, ctx, m.ID)
	if snap := decodeTransition(t, events[len(events)-1]).Recovery(); snap == nil || snap.Run != 4 || snap.Threshold != 2 {
		t.Fatalf("event snapshot = %+v, want 4/2", snap)
	}
}

// §4.1 "What does not touch the run": a probe error, a duplicate, an out-of-order result and the
// refusals (future, stale revision) record no verdict, so the counter stays where it was and the
// next fresh success continues the same run.
func TestRecoveryRunIgnoresNonVerdictResults(t *testing.T) {
	st, ctx := outboxTestStore(t)
	projectID := recoveryProject(t, st, ctx, "rt-nonverdict")
	m := newRecoveryMonitor(t, st, ctx, projectID, "rt-nonverdict", 3, 1)
	m.feed(false)
	m.feed(true)
	mid := m.row()
	if mid.Status != "down" || mid.Successes != 1 {
		t.Fatalf("setup: %+v, want a run of 1 on a down monitor", mid)
	}
	unchanged := func(step string) {
		t.Helper()
		w := m.row()
		if w.Status != mid.Status || w.Successes != mid.Successes || !sameInstant(w.StartedAt, mid.StartedAt) {
			t.Fatalf("%s moved the run: %+v, want %+v", step, w, mid)
		}
	}

	if pe, err := st.RecordProbeError(ctx, domain.Heartbeat{
		MonitorID: m.ID, ExecutionRevision: m.revision(),
		ProbeError: &domain.ProbeError{Reason: domain.ProbeErrorUnknownKeyID, JobID: "rt-job"},
	}); err != nil || !pe.Recorded {
		t.Fatalf("probe error: %+v %v", pe, err)
	}
	unchanged("a probe error")

	record := func(hb domain.Heartbeat) ResultOutcome {
		t.Helper()
		hb.MonitorID = m.ID
		if hb.ExecutionRevision == 0 {
			hb.ExecutionRevision = m.revision()
		}
		o, err := st.RecordScheduledResult(ctx, hb)
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		return o
	}
	for i, up := range []bool{false, true} {
		if o := record(domain.Heartbeat{Ts: m.lastT, Up: up}); o.Reason != ReasonDuplicate {
			t.Fatalf("duplicate up=%v: %+v", up, o)
		}
		unchanged("a duplicate")
		// A distinct instant per iteration: the first one's out-of-order row is already stored, and
		// reusing its timestamp would test the duplicate path a second time.
		if o := record(domain.Heartbeat{Ts: m.lastT.Add(-time.Duration(30+10*i) * time.Second), Up: up}); o.Reason != ReasonOutOfOrder {
			t.Fatalf("out-of-order up=%v: %+v", up, o)
		}
		unchanged("an out-of-order result")
	}
	if o := record(domain.Heartbeat{Ts: m.lastT.Add(time.Hour), Up: false}); o.Reason != ReasonFutureTimestamp {
		t.Fatalf("future: %+v", o)
	}
	unchanged("a future result")
	if o := record(domain.Heartbeat{Ts: m.lastT.Add(time.Second), Up: false, ExecutionRevision: m.revision() + 7}); o.Reason != ReasonStaleRevision {
		t.Fatalf("stale revision: %+v", o)
	}
	unchanged("a stale result")

	m.feed(true)
	if w := m.row(); w.Successes != 2 || !sameInstant(w.StartedAt, mid.StartedAt) {
		t.Fatalf("the next fresh success: %+v, want the same run at 2", w)
	}
}
