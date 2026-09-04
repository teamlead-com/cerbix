package store

import "testing"

// FR-032 invariant 10g, pull half, against a real database rather than a fake: the claim
// predicate is `protocol_version <= $4`, so a generation-4 row is OUTSIDE a v3 claim's result set.
// Physical unreachability, not a filter applied after selection.
func TestAV3ClaimLeavesAGenerationFourRowUnclaimed(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "carrier4claim", 101)
	defer cleanup()

	insertPullRow(t, ctx, db, "pull_jobs", 3)
	insertPullRow(t, ctx, db, "pull_jobs", 4)

	// The store under test talks to the CONTROL database, so the claim is exercised there; the
	// probe database above only proves the migration is at 101. Insert the same pair through the
	// store's own pool so the predicate runs against rows it can see.
	for _, generation := range []int{3, 4} {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO pull_jobs (region, payload, expires_at, protocol_version)
			 VALUES ('carrier4', '{}'::jsonb, now() + interval '10 minutes', $1)`, generation); err != nil {
			t.Fatalf("seed generation-%d row: %v", generation, err)
		}
	}

	claimed, err := st.ClaimPullJobsV3(ctx, "carrier4", 10, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range claimed {
		if job.ProtocolVersion >= 4 {
			t.Fatalf("ClaimPullJobsV3 returned a generation-%d row; its predicate must leave that "+
				"row outside the result set", job.ProtocolVersion)
		}
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimPullJobsV3 claimed %d rows, want the single generation-3 one", len(claimed))
	}

	// The converse, without which the assertion above cannot distinguish exclusion from an empty
	// table: a v4 claim reaches the row the v3 claim could not.
	v4, err := st.ClaimPullJobsV4(ctx, "carrier4", 10, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v4) != 1 || v4[0].ProtocolVersion != 4 {
		t.Fatalf("ClaimPullJobsV4 claimed %d rows (%v), want the single generation-4 one", len(v4), v4)
	}
}
