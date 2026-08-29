package store

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-024 discharge row 21 (invariant 21; §7 *Override*: "`GET …/overrides` after 120 rapid
// create-and-revoke rounds, 40 of them sharing one `created_at` (fixed clock), returns exactly the
// newest 50 in `created_at DESC, id DESC` order, identical across ten calls, and `EXPLAIN` on the
// analyzed fixture shows the `(service_id, created_at DESC, id DESC)` index and no Sort node").
//
// The rounds go through the real `CreateGateOverride`/`RevokeGateOverride` (one active slot, so
// each round must revoke before the next create). The forty equal timestamps are planted by SQL
// onto ranks 6..45 of the newest-first order, so the tie block sits INSIDE the returned window and
// the id tie-break is what orders it. The expected order is computed in Go from a full read of all
// 120 rows; the listing must equal it — ten times — and the plan for the listing's own statement
// must walk `service_gate_overrides_history_idx` with no Sort node.
func TestGateOverrideHistoryIsTheNewestFiftyInIndexOrderWithoutASort(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	now := gateDBNow(t, st, ctx)

	const rounds = 120
	for i := 0; i < rounds; i++ {
		id, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, fmt.Sprintf("round %d", i), now.Add(time.Hour), gateActorToken)
		if err != nil {
			t.Fatalf("round %d create: %v", i, err)
		}
		if err := st.RevokeGateOverride(ctx, f.projectID, f.serviceID, id, gateActorToken); err != nil {
			t.Fatalf("round %d revoke: %v", i, err)
		}
	}
	// Forty rows share one created_at: ranks 6..45 take rank 6's timestamp.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_gate_overrides
		   SET created_at = (SELECT created_at FROM service_gate_overrides WHERE service_id = $1
		                      ORDER BY created_at DESC, id DESC OFFSET 5 LIMIT 1)
		 WHERE id IN (SELECT id FROM service_gate_overrides WHERE service_id = $1
		               ORDER BY created_at DESC, id DESC OFFSET 5 LIMIT 40)`, f.serviceID); err != nil {
		t.Fatalf("plant equal timestamps: %v", err)
	}
	var total, tied int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), (SELECT count(*) FROM service_gate_overrides o2 WHERE o2.service_id = $1
		                   GROUP BY o2.created_at ORDER BY count(*) DESC LIMIT 1)
		  FROM service_gate_overrides WHERE service_id = $1`, f.serviceID).Scan(&total, &tied); err != nil {
		t.Fatal(err)
	}
	if total != rounds || tied != 40 {
		t.Fatalf("fixture: %d rows, largest created_at tie %d; want %d and 40", total, tied, rounds)
	}
	if _, err := st.pool.Exec(ctx, `ANALYZE service_gate_overrides`); err != nil {
		t.Fatal(err)
	}

	// The expected order, from a full read sorted in Go by (created_at DESC, id DESC). uuid text
	// compares like the database's bytewise uuid order.
	type row struct {
		id string
		at time.Time
	}
	all := []row{}
	rows, err := st.pool.Query(ctx, `SELECT id, created_at FROM service_gate_overrides WHERE service_id = $1`, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.at); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	sort.Slice(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.After(all[j].at)
		}
		return all[i].id > all[j].id
	})
	want := make([]string, 0, gateOverrideListLimit)
	for _, r := range all[:gateOverrideListLimit] {
		want = append(want, r.id)
	}
	// The tie block must be inside the window for the id tie-break to be what is tested.
	tiedInWindow := 0
	for _, r := range all[:gateOverrideListLimit] {
		if r.at.Equal(all[5].at) {
			tiedInWindow++
		}
	}
	if tiedInWindow != 40 {
		t.Fatalf("only %d of the 40 tied rows are inside the newest 50", tiedInWindow)
	}

	// The clause: exactly the newest 50, in that order, identical across ten calls.
	for call := 0; call < 10; call++ {
		hist, err := st.ListGateOverrides(ctx, f.projectID, f.serviceID)
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if len(hist) != gateOverrideListLimit {
			t.Fatalf("call %d: %d rows, want %d", call, len(hist), gateOverrideListLimit)
		}
		got := make([]string, 0, len(hist))
		for _, h := range hist {
			got = append(got, h.ID)
			if h.Status != domain.GateOverrideRevoked || h.RevokedReason != domain.GateRevokedManual {
				t.Errorf("call %d: row %s status %s/%s, want revoked/manual", call, h.ID, h.Status, h.RevokedReason)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("call %d: order differs from (created_at DESC, id DESC)\n got %v\nwant %v", call, got, want)
		}
	}

	// The plan for the listing's statement (ListGateOverrides' own text, parameters inlined): the
	// history index, and no Sort node anywhere in it.
	plan := []string{}
	prows, err := st.pool.Query(ctx, fmt.Sprintf(`EXPLAIN (COSTS OFF)
		SELECT`+gateOverrideColumns+`
		  FROM service_gate_overrides o
		 WHERE o.service_id = '%s' AND o.project_id = '%s'
		 ORDER BY o.created_at DESC, o.id DESC
		 LIMIT %d`, f.serviceID, f.projectID, gateOverrideListLimit))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for prows.Next() {
		var line string
		if err := prows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	prows.Close()
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "service_gate_overrides_history_idx") {
		t.Errorf("the plan does not use the history index:\n%s", text)
	}
	if strings.Contains(text, "Sort") {
		t.Errorf("the plan sorts:\n%s", text)
	}
}
