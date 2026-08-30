package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D9 against live writes (func-change-intelligence §7 *Retention*, invariant 13;
// iter-0165 task 2, Agent B; reviewer [42]): the purge takes each selected identity's lock — the
// same one RecordChangePhase takes — and re-evaluates the group's age under it, so a group is
// never split whichever side arrives first.

type purgeOutcome struct {
	groups, rows int
	err          error
}

func purgeAsync(st *Store, ctx context.Context, cutoff time.Time, batch int) <-chan purgeOutcome {
	out := make(chan purgeOutcome, 1)
	go func() {
		g, r, err := st.PurgeChangeGroups(ctx, cutoff, batch)
		out <- purgeOutcome{g, r, err}
	}()
	return out
}

// (a) A held identity lock parks the purge ON THE ADVISORY LOCK, the whole batch waits (one
// transaction), the wait is bounded only by the caller's context — a cancelled purge returns an
// error having deleted nothing, not even the other selected groups — and once the holder
// releases, an unbounded purge completes and removes every selected group whole.
func TestChangeRetentionWaitsForAHeldIdentityLockBoundedByTheCallerAndDeletesNothingWhenCancelled(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	day := 24 * time.Hour
	cutoff := now.Add(-400 * day)
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g1", domain.ChangeKindDeploy, domain.ChangePhaseStarted, now.Add(-500*day))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g1", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-499*day))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g2", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-480*day))

	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT `+changeIdentityLockSQL("$1", "$2", "$3"), f.serviceID, "ci", "g1"); err != nil {
		t.Fatal(err)
	}

	// Bounded by the caller: a one-second context.
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	_, _, err = st.PurgeChangeGroups(bounded, cutoff, 10)
	cancel()
	if err == nil {
		t.Fatal("the purge returned while g1's identity lock was held by another transaction")
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE service_id = $1`, f.serviceID); n != 3 {
		t.Fatalf("%d rows survive a cancelled purge, want all 3 (the batch is one transaction)", n)
	}

	// Unbounded: parked on the advisory lock, not on a row; released → completes whole.
	done := purgeAsync(st, ctx, cutoff, 10)
	blocked := waitForBlockedBackends(t, st, ctx, 1)
	if blocked[0].waitEvent != "advisory" || !strings.Contains(blocked[0].query, "pg_advisory_xact_lock") {
		t.Fatalf("the purge is parked at %+v, want the identity's advisory lock", blocked[0])
	}
	select {
	case o := <-done:
		t.Fatalf("the purge returned while the lock was held: %+v", o)
	case <-time.After(300 * time.Millisecond):
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.err != nil || o.groups != 2 || o.rows != 3 {
			t.Fatalf("after the holder released: %+v, want 2 groups / 3 rows", o)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the purge never completed after the holder released")
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE service_id = $1`, f.serviceID); n != 0 {
		t.Fatalf("%d rows survive the purge", n)
	}
}

// (b) The writer arrives first: a REAL RecordChangePhase of a young terminal is mid-flight
// (holding the identity lock, parked at its INSERT by a planted conflicting row) when the purge
// selects the group by the only row it can see. The purge must park on the identity lock; when
// the writer commits, the re-evaluation sees the young terminal and the group stays ENTIRELY
// intact — old started and young terminal — while an unrelated old group is removed.
func TestChangeRetentionKeepsAGroupWholeWhenAYoungTerminalCommitsBetweenSelectionAndDelete(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	day := 24 * time.Hour
	cutoff := now.Add(-400 * day)
	startedID := plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "slow", domain.ChangeKindDeploy, domain.ChangePhaseStarted, now.Add(-500*day))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "old", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-450*day))

	plant, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer plant.Rollback(ctx) //nolint:errcheck
	plantUncommittedPhase(t, plant, ctx, f.projectID, f.serviceID, "ci", "slow", "succeeded", now.Add(-time.Minute))
	writer := make(chan error, 1)
	go func() {
		in := changeInput(f, "slow", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
		in.Source = "ci"
		_, _, err := st.RecordChangePhase(ctx, in)
		writer <- err
	}()
	blocked := waitForBlockedBackends(t, st, ctx, 1)
	if !strings.Contains(blocked[0].query, "INSERT INTO service_changes") {
		t.Fatalf("the writer is parked at %+v, want its INSERT", blocked[0])
	}

	done := purgeAsync(st, ctx, cutoff, 10)
	blocked = waitForBlockedBackends(t, st, ctx, 2)
	onLock := 0
	for _, b := range blocked {
		if b.waitEvent == "advisory" && strings.Contains(b.query, "pg_advisory_xact_lock") {
			onLock++
		}
	}
	if onLock != 1 {
		t.Fatalf("parked backends = %+v: the purge must wait on the writer's identity lock", blocked)
	}
	select {
	case o := <-done:
		t.Fatalf("the purge returned while the writer held the identity: %+v", o)
	case <-time.After(300 * time.Millisecond):
	}

	if err := plant.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err, ok := completedWithin(writer, 10*time.Second); !ok || err != nil {
		t.Fatalf("writer: ok=%v err=%v", ok, err)
	}
	var o purgeOutcome
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the purge never completed")
	}
	if o.err != nil || o.groups != 1 || o.rows != 1 {
		t.Fatalf("purge = %+v, want the unrelated old group only (1 group / 1 row)", o)
	}
	rows, err := changeIdentityRowsTx(ctx, st.pool, f.projectID, f.serviceID, "ci", "slow")
	if err != nil || len(rows) != 2 || rows[0].ID != startedID || rows[1].Phase != domain.ChangePhaseSucceeded {
		t.Fatalf("slow = %+v (%v), want the old started AND the young terminal", rows, err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE external_id = 'old'`); n != 0 {
		t.Fatal("the unrelated old group survived")
	}
}

// (c) The purge arrives first: parked at its DELETE (a FOR UPDATE on the old row), it holds the
// identity lock, so a writer for that identity must WAIT rather than commit beside the delete.
// When the delete completes the whole group is gone and the writer's terminal is recorded as a
// terminal-alone group (D3) — the order, not the row count, is what makes the outcome
// consistent: no instant ever showed a young terminal with its started removed underneath it.
func TestChangeRetentionSerializesAWriterBehindThePurgeOfItsIdentity(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	day := 24 * time.Hour
	cutoff := now.Add(-400 * day)
	startedID := plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "late", domain.ChangeKindDeploy, domain.ChangePhaseStarted, now.Add(-500*day))

	park, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer park.Rollback(ctx) //nolint:errcheck
	if _, err := park.Exec(ctx, `SELECT 1 FROM service_changes WHERE id = $1 FOR UPDATE`, startedID); err != nil {
		t.Fatal(err)
	}
	done := purgeAsync(st, ctx, cutoff, 10)
	blocked := waitForBlockedBackends(t, st, ctx, 1)
	if !strings.Contains(blocked[0].query, "DELETE FROM service_changes") {
		t.Fatalf("the purge is parked at %+v, want its DELETE", blocked[0])
	}

	writer := make(chan error, 1)
	go func() {
		in := changeInput(f, "late", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
		in.Source = "ci"
		_, _, err := st.RecordChangePhase(ctx, in)
		writer <- err
	}()
	blocked = waitForBlockedBackends(t, st, ctx, 2)
	onLock := 0
	for _, b := range blocked {
		if b.waitEvent == "advisory" {
			onLock++
		}
	}
	if onLock != 1 {
		t.Fatalf("parked backends = %+v: the writer must wait on the purge's identity lock", blocked)
	}
	if _, ok := completedWithin(writer, 300*time.Millisecond); ok {
		t.Fatal("the writer committed beside the purge of its identity")
	}

	if err := park.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var o purgeOutcome
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the purge never completed")
	}
	if o.err != nil || o.groups != 1 || o.rows != 1 {
		t.Fatalf("purge = %+v, want the whole (one-row) group", o)
	}
	if err, ok := completedWithin(writer, 10*time.Second); !ok || err != nil {
		t.Fatalf("writer after the purge: ok=%v err=%v", ok, err)
	}
	rows, err := changeIdentityRowsTx(ctx, st.pool, f.projectID, f.serviceID, "ci", "late")
	if err != nil || len(rows) != 1 || rows[0].Phase != domain.ChangePhaseSucceeded || rows[0].ID == startedID {
		t.Fatalf("late = %+v (%v), want exactly the writer's terminal-alone row", rows, err)
	}
}

// The ordinary path is unchanged by the locks: a batch that selects nothing is a no-op that
// commits; the counts are of groups and rows actually removed; a zero bound is refused.
func TestChangeRetentionEmptyBatchIsANoOpAndCountsAreOfRowsRemoved(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	day := 24 * time.Hour
	if g, r, err := st.PurgeChangeGroups(ctx, now.Add(-400*day), 10); err != nil || g != 0 || r != 0 {
		t.Fatalf("empty = %d/%d %v", g, r, err)
	}
	for i := 0; i < 3; i++ {
		plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g"+string(rune('a'+i)), domain.ChangeKindDeploy, domain.ChangePhaseStarted, now.Add(-502*day))
		plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g"+string(rune('a'+i)), domain.ChangeKindDeploy, domain.ChangePhaseFailed, now.Add(-501*day))
	}
	if g, r, err := st.PurgeChangeGroups(ctx, now.Add(-400*day), 2); err != nil || g != 2 || r != 4 {
		t.Fatalf("batch of 2 = %d/%d %v, want 2 groups / 4 rows", g, r, err)
	}
	if g, r, err := st.PurgeChangeGroups(ctx, now.Add(-400*day), 2); err != nil || g != 1 || r != 2 {
		t.Fatalf("short batch = %d/%d %v, want 1 group / 2 rows", g, r, err)
	}
	if _, _, err := st.PurgeChangeGroups(ctx, now.Add(-400*day), 0); err == nil {
		t.Fatal("a zero batch bound was accepted")
	}
}
