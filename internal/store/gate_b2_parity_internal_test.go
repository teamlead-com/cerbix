package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// FR-024 discharge row 1 (invariant 1; func-reliability-gate §7 *Owners and parity*, first case):
// "the badge says `unroutable` and `coverage_state` says `unroutable` — the same string from the
// same snapshot".
//
// The badge is `ServiceAlertingState`; the decision's `coverage_state` is what
// `serviceAlertingStateOn(ctx, tx, …)` produced INSIDE the decision transaction. Both are asserted
// against the owner's constant AND against each other, per signal, for four coverage states the
// fixtures can produce — so a decision path that renamed, remapped or swapped a reason fails here
// even when the badge alone still reads correctly. The last case proves the SNAPSHOT half: the
// route is restored by another connection between the policy read and the coverage read, the
// decision still says `unroutable` (the pre-mutation world), and a badge read afterwards says armed.
func TestGateCoverageStateIsTheBadgesReasonFromOneSnapshot(t *testing.T) {
	addRoute := func(t *testing.T, st *Store, ctx context.Context, f gateFixture) {
		t.Helper()
		exec(t, st, ctx, `
			INSERT INTO notification_channels (project_id, type, name, config, enabled)
			VALUES ($1,'webhook','ops','{"url":"https://hook.example/x"}',true)`, f.projectID)
	}
	cases := []struct {
		name               string
		arrange            func(t *testing.T, st *Store, ctx context.Context, f gateFixture)
		wantLive, wantBurn string // "" = armed
	}{
		// The fixture's project has no channel and no on-call schedule: nothing resolves.
		{"unroutable", func(*testing.T, *Store, context.Context, gateFixture) {}, AlertReasonUnroutable, AlertReasonUnroutable},
		{"armed", addRoute, "", ""},
		{"stale live lease", func(t *testing.T, st *Store, ctx context.Context, f gateFixture) {
			addRoute(t, st, ctx, f)
			gateArmCoverage(t, st, ctx, f, -time.Second)
		}, AlertReasonStaleLease, ""},
		{"burn held", func(t *testing.T, st *Store, ctx context.Context, f gateFixture) {
			addRoute(t, st, ctx, f)
			exec(t, st, ctx, `UPDATE service_burn_alert_state SET last_verdict = 'hold' WHERE service_id = $1`, f.serviceID)
		}, "", AlertReasonHeld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := gateStore(t)
			f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
			// Ownership first: the paging-column trigger bumps alert_config_generation, and the
			// latches and the live evaluation stamp the generation they were written under.
			exec(t, st, ctx, `UPDATE services SET owns_paging = true WHERE id = $1`, f.serviceID)
			gateFreshLatches(t, st, ctx, f)
			gateArmCoverage(t, st, ctx, f, 90*time.Second)
			gatePut(t, st, ctx, f, nil, gateDoc(nil))
			tc.arrange(t, st, ctx, f)

			badge, err := st.ServiceAlertingState(ctx, f.projectID, f.serviceID)
			if err != nil {
				t.Fatalf("badge: %v", err)
			}
			// The badge itself must be in the state the case names, or the parity below is vacuous.
			if badge.Live.Reason != tc.wantLive || badge.Live.Armed != (tc.wantLive == "") {
				t.Fatalf("badge live = armed:%v %q, want %q", badge.Live.Armed, badge.Live.Reason, tc.wantLive)
			}
			if badge.Burn.Reason != tc.wantBurn || badge.Burn.Armed != (tc.wantBurn == "") {
				t.Fatalf("badge burn = armed:%v %q, want %q", badge.Burn.Armed, badge.Burn.Reason, tc.wantBurn)
			}

			dec := gateDecide(t, st, ctx, f)
			if dec.CoverageState == nil {
				t.Fatalf("coverage_state absent on an evaluated service (reasons %v)", reasonCodes(dec))
			}
			// The clause: the SAME string, per signal, and the same armed bit.
			if got := dec.CoverageState.Live.Reason; got != badge.Live.Reason || got != tc.wantLive {
				t.Errorf("coverage_state.live.reason = %q, badge says %q, the owner's constant is %q", got, badge.Live.Reason, tc.wantLive)
			}
			if got := dec.CoverageState.Burn.Reason; got != badge.Burn.Reason || got != tc.wantBurn {
				t.Errorf("coverage_state.burn.reason = %q, badge says %q, the owner's constant is %q", got, badge.Burn.Reason, tc.wantBurn)
			}
			if dec.CoverageState.Live.Armed != badge.Live.Armed || dec.CoverageState.Burn.Armed != badge.Burn.Armed {
				t.Errorf("coverage_state armed = live:%v burn:%v, badge = live:%v burn:%v",
					dec.CoverageState.Live.Armed, dec.CoverageState.Burn.Armed, badge.Live.Armed, badge.Burn.Armed)
			}
			// The ledger row carries the same evidence: read back by id, the reasons are identical.
			got, err := st.GetGateDecision(ctx, f.projectID, dec.DecisionID)
			if err != nil || got.CoverageState == nil || *got.CoverageState != *dec.CoverageState {
				t.Errorf("ledger coverage_state = %+v %v, decision said %+v", got.CoverageState, err, dec.CoverageState)
			}
		})
	}

	// The same snapshot: the route comes back on another connection AFTER the decision's policy
	// read and BEFORE its coverage read. Coverage is read inside the REPEATABLE READ transaction,
	// so the decision reports the world as of evaluated_at — `unroutable` — while a badge read
	// after the commit already says armed, and so does the next decision.
	t.Run("coverage is read inside the decision snapshot", func(t *testing.T) {
		st, ctx := gateStore(t)
		f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
		exec(t, st, ctx, `UPDATE services SET owns_paging = true WHERE id = $1`, f.serviceID)
		gateFreshLatches(t, st, ctx, f)
		gateArmCoverage(t, st, ctx, f, 90*time.Second)
		gatePut(t, st, ctx, f, nil, gateDoc(nil))

		restored := false
		gateDecisionHook = func(hctx context.Context, _ int, phase string, _ pgx.Tx) error {
			if phase != gatePhasePolicyRead || restored {
				return nil
			}
			restored = true
			addRoute(t, st, hctx, f)
			return nil
		}
		t.Cleanup(func() { gateDecisionHook = nil })

		dec := gateDecide(t, st, ctx, f)
		if dec.CoverageState == nil || dec.CoverageState.Live.Reason != AlertReasonUnroutable || dec.CoverageState.Burn.Reason != AlertReasonUnroutable {
			t.Fatalf("coverage_state = %+v, want both signals %q as of evaluated_at", dec.CoverageState, AlertReasonUnroutable)
		}
		badge, err := st.ServiceAlertingState(ctx, f.projectID, f.serviceID)
		if err != nil || !badge.Live.Armed || !badge.Burn.Armed {
			t.Fatalf("badge after the route was restored = %+v %v, want armed", badge, err)
		}
		next := gateDecide(t, st, ctx, f)
		if next.CoverageState == nil || !next.CoverageState.Live.Armed || !next.CoverageState.Burn.Armed ||
			next.CoverageState.Live.Reason != badge.Live.Reason || next.CoverageState.Burn.Reason != badge.Burn.Reason {
			t.Errorf("the next decision's coverage_state = %+v, badge = %+v", next.CoverageState, badge)
		}
	})
}
