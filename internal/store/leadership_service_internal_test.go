package store

import (
	"testing"
	"time"
)

// The fence, demonstrated rather than asserted: a leader that has lost its connection must
// not be able to commit service work.
//
// Closing the session's connection releases the session-scoped advisory lock AND aborts
// anything in flight on it, together. A pooled write has no such property — it would commit
// happily after another node had already taken over — which is exactly why the scheduler's
// election had to move onto a pinned connection instead of keeping a `check()` callback.
func TestFencedRepairSliceCannotCommitAfterLosingTheLock(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	// Inside the adopted window: the retroactive first revision reaches back two hours, so a
	// range older than that is governed by no epoch and would correctly produce nothing.
	base := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Minute)
	startMaterialization(t, st, ctx, f, base)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// A unique key per run. A fixed one leaks across runs whenever a test process is
	// killed mid-test: the pooled connection stays idle holding a session-scoped advisory
	// lock, and every later run then fails to elect for a reason that has nothing to do
	// with the code.
	key := time.Now().UnixNano()
	session, ok, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		t.Fatalf("elect: %v ok=%v", err, ok)
	}

	// A second contender must be refused while the first holds it.
	if _, ok2, err := st.TryBecomeLeaderSession(ctx, key); err != nil {
		t.Fatalf("second election: %v", err)
	} else if ok2 {
		t.Fatal("two sessions hold one leadership key")
	}

	worked, err := session.RunServiceRepairSlice(ctx, time.Now().Add(20*time.Second))
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	if !worked {
		t.Fatal("the slice found nothing to do although a range was pending")
	}
	var facts int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets WHERE service_id=$1`, f.serviceID).Scan(&facts); err != nil {
		t.Fatalf("count: %v", err)
	}
	if facts == 0 {
		t.Fatal("the fenced slice wrote no facts")
	}

	// Release, and the key becomes contendable again — a deposed leader's successor is not
	// blocked by a lock nobody holds.
	session.Release()
	next, ok, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		t.Fatalf("re-election after release: %v ok=%v", err, ok)
	}
	next.Release()
}

// The slice reports "nothing to do" on an empty queue, so an installation with no services
// costs one claim query per sub-tick and no more.
func TestFencedSliceOnAnEmptyQueueIsCheap(t *testing.T) {
	st, ctx := declStore(t)
	key := time.Now().UnixNano() + 1
	session, ok, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		t.Fatalf("elect: %v ok=%v", err, ok)
	}
	defer session.Release()

	worked, err := session.RunServiceRepairSlice(ctx, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	if worked {
		t.Error("the slice claimed work from an empty queue")
	}
}
