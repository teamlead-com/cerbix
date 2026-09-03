package store

import (
	"strings"
	"testing"
	"time"
)

// FR-029 §4.2. The claim lease used to be one hardcoded 30 seconds for the whole batch, which is
// shorter than an async canary's journey — so a second agent re-claimed a job the first was still
// running and submitted a SECOND external transaction. It is also a defect that predates the canary:
// any pull monitor with a timeout past the default is re-claimable mid-probe today.
func TestAJobCarriesItsOwnClaimLease(t *testing.T) {
	st, ctx := outboxTestStore(t)

	// A short job takes the endpoint's default (0 = "use the caller's"), a long one asks for more.
	if err := st.EnqueuePullJob(ctx, "geo9", []byte(`{"short":1}`), 300, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueuePullJob(ctx, "geo9", []byte(`{"long":1}`), 300, 360); err != nil {
		t.Fatal(err)
	}

	claimed, err := st.ClaimPullJobs(ctx, "geo9", 10, 30)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d jobs, want 2", len(claimed))
	}

	var short, long time.Time
	for _, j := range claimed {
		var expires time.Time
		if err := st.pool.QueryRow(ctx,
			`SELECT lease_expires_at FROM pull_jobs WHERE claim_token = $1`, j.Token).Scan(&expires); err != nil {
			t.Fatalf("read lease: %v", err)
		}
		// The payload comes back re-serialized by the database, so match on a KEY rather than on
		// the exact bytes — the first version of this test compared strings and matched neither.
		if strings.Contains(string(j.Payload), "short") {
			short = expires
		} else {
			long = expires
		}
	}
	if short.IsZero() || long.IsZero() {
		t.Fatal("both jobs must carry a lease")
	}
	// The long job's lease has to outlast the short one by roughly the extra it asked for; the
	// exact instant is the database's, so the assertion is on the DIFFERENCE.
	if gap := long.Sub(short); gap < 5*time.Minute {
		t.Fatalf("the long job's lease is only %s past the short one's, want its own 360s", gap)
	}
}
