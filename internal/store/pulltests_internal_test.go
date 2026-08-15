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
	if _, _, _, ok, _ := st.ClaimPullTest(ctx, "other"); ok {
		t.Fatal("claim in wrong region returned a job")
	}
	gotID, payload, _, ok, err := st.ClaimPullTest(ctx, "geo2")
	if err != nil || !ok || gotID != id || len(payload) == 0 {
		t.Fatalf("claim: id=%q ok=%v err=%v", gotID, ok, err)
	}
	if _, _, _, ok, _ := st.ClaimPullTest(ctx, "geo2"); ok {
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
	if _, _, _, ok, _ := st.ClaimPullTest(ctx, "geo2"); ok {
		t.Fatal("expired test claimed")
	}
	if n, err := st.PurgeExpiredPullTests(ctx); err != nil || n != 1 {
		t.Fatalf("purge = %d err=%v (want 1)", n, err)
	}
}

func TestPullTestProtocolClaimsAreSeparated(t *testing.T) {
	st, ctx := outboxTestStore(t)
	id, err := st.EnqueuePullTestV2(ctx, "secure", []byte(`{"protocol":2}`), 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := st.ClaimPullTest(ctx, "secure"); err != nil || ok {
		t.Fatalf("v1 claim saw v2 test: ok=%v err=%v", ok, err)
	}
	gotID, _, _, ok, err := st.ClaimPullTestV2(ctx, "secure")
	if err != nil || !ok || gotID != id {
		t.Fatalf("v2 claim: id=%q ok=%v err=%v", gotID, ok, err)
	}
}

// The test-RPC mirror of the jobs barrier (D-0160): a jobs-only fix would leave
// test-connection broken for ordinary monitors in exactly the same way.
func TestCapableTestClaimLeasesEveryGenerationAtOrBelowCapability(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if _, err := st.EnqueuePullTest(ctx, "secure", []byte(`{"protocol":1}`), 20); err != nil {
		t.Fatal(err)
	}
	id, payload, generation, ok, err := st.ClaimPullTestV2(ctx, "secure")
	if err != nil || !ok {
		t.Fatalf("capable test claim did not see the generation-1 row: ok=%v err=%v", ok, err)
	}
	if id == "" || len(payload) == 0 {
		t.Fatalf("capable test claim returned an empty row: id=%q payload=%q", id, payload)
	}
	if generation != 1 {
		t.Fatalf("server-stamped generation = %d, want 1", generation)
	}
}

// And the incapable direction still holds for tests too.
func TestGeneration1TestClaimNeverSeesNewerGeneration(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if _, err := st.EnqueuePullTestV2(ctx, "secure", []byte(`{"protocol":2}`), 20); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := st.ClaimPullTest(ctx, "secure"); err != nil || ok {
		t.Fatalf("legacy test claim saw a newer generation: ok=%v err=%v", ok, err)
	}
}

// Mixed generations on the test queue behave like the jobs queue: a capability claim takes
// them oldest-first across generations over repeated claims, and the legacy endpoint never
// sees the newer one. Claiming twice is the point — a single claim cannot show ordering.
func TestCapableTestClaimTakesBothGenerationsOldestFirst(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if _, err := st.EnqueuePullTestV2(ctx, "secure", []byte(`{"protocol":2}`), 20); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueuePullTest(ctx, "secure", []byte(`{"protocol":1}`), 20); err != nil {
		t.Fatal(err)
	}
	// Make the order deterministic regardless of insert-timestamp resolution.
	if _, err := st.pool.Exec(ctx,
		`UPDATE pull_tests SET created_at = now() - interval '1 minute' WHERE protocol_version = 2`); err != nil {
		t.Fatal(err)
	}
	var seen []int
	for i := 0; i < 2; i++ {
		_, _, generation, ok, err := st.ClaimPullTestV2(ctx, "secure")
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		seen = append(seen, generation)
	}
	if len(seen) != 2 || seen[0] != 2 || seen[1] != 1 {
		t.Fatalf("claimed generations = %v, want [2 1] (oldest first, across generations)", seen)
	}
	if _, _, _, ok, _ := st.ClaimPullTestV2(ctx, "secure"); ok {
		t.Fatal("a third claim returned a row that does not exist")
	}
}

// The legacy test endpoint never sees a newer generation even when one is older and would
// otherwise win the FIFO.
func TestGeneration1TestClaimSkipsOlderNewerGenerationRow(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if _, err := st.EnqueuePullTestV2(ctx, "secure", []byte(`{"protocol":2}`), 20); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueuePullTest(ctx, "secure", []byte(`{"protocol":1}`), 20); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE pull_tests SET created_at = now() - interval '1 minute' WHERE protocol_version = 2`); err != nil {
		t.Fatal(err)
	}
	_, _, generation, ok, err := st.ClaimPullTest(ctx, "secure")
	if err != nil || !ok {
		t.Fatalf("legacy claim: ok=%v err=%v", ok, err)
	}
	if generation != 1 {
		t.Fatalf("legacy claim took generation %d", generation)
	}
}
