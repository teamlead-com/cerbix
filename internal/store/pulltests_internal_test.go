package store

import "testing"

func TestPullTestLifecycle(t *testing.T) {
	st, ctx := outboxTestStore(t)

	id, err := st.EnqueuePullTest(ctx, "geo2", []byte(`{"Monitor":{"id":"m1"}}`), 20)
	if err != nil || id == "" {
		t.Fatalf("enqueue: id=%q err=%v", id, err)
	}
	// Result isn't ready yet.
	if _, ok, _ := st.GetPullTestResult(ctx, id); ok {
		t.Fatal("result should not be ready before the agent posts it")
	}
	// Agent claims it (only once) and it's scoped to the region.
	if _, _, ok, _ := st.ClaimPullTest(ctx, "other"); ok {
		t.Fatal("claim in wrong region returned a job")
	}
	gotID, payload, ok, err := st.ClaimPullTest(ctx, "geo2")
	if err != nil || !ok || gotID != id || len(payload) == 0 {
		t.Fatalf("claim: id=%q ok=%v err=%v", gotID, ok, err)
	}
	if _, _, ok, _ := st.ClaimPullTest(ctx, "geo2"); ok {
		t.Fatal("a claimed test must not be claimed again")
	}
	// A result posted for the wrong region is ignored (scoped write).
	if err := st.SavePullTestResult(ctx, id, "other", []byte(`{"up":true}`)); err != nil {
		t.Fatalf("save result wrong region: %v", err)
	}
	if _, ok, _ := st.GetPullTestResult(ctx, id); ok {
		t.Fatal("a cross-region result post must not populate the test")
	}
	// Agent posts the result for its own region; the API fetches it once (atomically removing it).
	if err := st.SavePullTestResult(ctx, id, "geo2", []byte(`{"up":true,"code":200}`)); err != nil {
		t.Fatalf("save result: %v", err)
	}
	raw, ok, err := st.GetPullTestResult(ctx, id)
	if err != nil || !ok || len(raw) == 0 {
		t.Fatalf("get result: ok=%v err=%v raw=%s", ok, err, raw)
	}
	if _, ok, _ := st.GetPullTestResult(ctx, id); ok {
		t.Fatal("result should be consumed (removed) after the first fetch")
	}

	// Expired tests are purged and never claimed.
	id2, _ := st.EnqueuePullTest(ctx, "geo2", []byte(`{}`), 20)
	if _, err := st.pool.Exec(ctx, `UPDATE pull_tests SET expires_at = now() - interval '1 minute' WHERE id=$1`, id2); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, _, ok, _ := st.ClaimPullTest(ctx, "geo2"); ok {
		t.Fatal("expired test claimed")
	}
	if n, err := st.PurgeExpiredPullTests(ctx); err != nil || n != 1 {
		t.Fatalf("purge = %d err=%v (want 1)", n, err)
	}
}
