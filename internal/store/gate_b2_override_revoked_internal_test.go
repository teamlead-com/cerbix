package store

import (
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-024 discharge row 9 (invariant 9): "an expired or revoked one is not applied" — the MANUAL
// revocation half, reached through a DECISION rather than through the status read alone.
//
// A BLOCK decision is overridden (action ALLOW, override named); the override is revoked by id
// through `RevokeGateOverride`; the next decision is un-overridden — action BLOCK, no `override`,
// no `override_id`, no `unoverridden_action` (raw JSON keys, not merely nil pointers), the ledger
// row's override_id NULL, and `state`/`reasons[]` identical to the never-overridden decision. Then
// the EXPIRED half on the same fixture: a fresh override whose expiry is planted in the past is
// not applied either, and a decision taken AFTER the next create closes it still names only the
// new one.
func TestGateDecisionIgnoresAManuallyRevokedOverride(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateLatch(t, st, ctx, f, gatePageKey, true, 90*time.Second)
	gateLatch(t, st, ctx, f, gateTicketKey, false, 90*time.Second)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	now := gateDBNow(t, st, ctx)

	blocked := gateDecide(t, st, ctx, f)
	wantOutcome(t, blocked, domain.GateStateBlock, domain.GateActionBlock)

	ovID, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "hotfix 1.2.3", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	applied := gateDecide(t, st, ctx, f)
	if actionOf(applied) != "ALLOW" || applied.OverrideID == nil || *applied.OverrideID != ovID || applied.UnoverriddenAction == nil {
		t.Fatalf("the override was not applied before the revoke: action=%s override_id=%v", actionOf(applied), applied.OverrideID)
	}

	// The manual revoke, by id, by a different principal than the creator.
	revoker := GateActor{ActorUserID: "", ViaToken: true, Label: "token:release-bot"}
	if err := st.RevokeGateOverride(ctx, f.projectID, f.serviceID, ovID, revoker); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, ovID)
	if err != nil || rec.Status != domain.GateOverrideRevoked || rec.RevokedReason != domain.GateRevokedManual {
		t.Fatalf("revoked row reads %+v %v, want status revoked / reason manual", rec, err)
	}

	assertUnoverridden := func(t *testing.T, dec domain.GateDecision, what string) {
		t.Helper()
		if dec.State != domain.GateStateBlock || actionOf(dec) != "BLOCK" {
			t.Errorf("%s: state=%s action=%s, want BLOCK/BLOCK", what, dec.State, actionOf(dec))
		}
		if dec.Override != nil || dec.OverrideID != nil || dec.UnoverriddenAction != nil {
			t.Errorf("%s: override=%+v override_id=%v unoverridden_action=%v, want all absent", what, dec.Override, dec.OverrideID, dec.UnoverriddenAction)
		}
		keys := gateJSONKeys(t, dec)
		for _, k := range []string{"override", "override_id", "unoverridden_action"} {
			if contains(keys, k) {
				t.Errorf("%s: raw JSON carries %q: %v", what, k, keys)
			}
		}
		if strings.Join(reasonCodes(dec), " ") != strings.Join(reasonCodes(blocked), " ") {
			t.Errorf("%s: reasons %v, the un-overridden decision said %v", what, reasonCodes(dec), reasonCodes(blocked))
		}
		var stored *string
		if err := st.pool.QueryRow(ctx, `SELECT override_id::text FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&stored); err != nil || stored != nil {
			t.Errorf("%s: ledger override_id = %v %v, want NULL", what, stored, err)
		}
	}

	// The clause: a decision taken AFTER a manual revoke does not apply the override.
	assertUnoverridden(t, gateDecide(t, st, ctx, f), "after the manual revoke")
	// Idempotent: still un-overridden on the next evaluation, and the revoked row is untouched.
	assertUnoverridden(t, gateDecide(t, st, ctx, f), "second decision after the revoke")
	if rec2, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, ovID); err != nil || rec2.Status != domain.GateOverrideRevoked ||
		rec2.RevokedByLabel == nil || *rec2.RevokedByLabel != "token:release-bot" {
		t.Errorf("a decision touched the revoked row: %+v %v", rec2, err)
	}

	// The slot was released by the revoke: a new override applies; planted expired, it does not;
	// and the decision after the next create (which closes the expired row) names only the new one.
	ov2, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "second", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatalf("the manual revoke did not release the slot: %v", err)
	}
	if dec := gateDecide(t, st, ctx, f); dec.OverrideID == nil || *dec.OverrideID != ov2 || actionOf(dec) != "ALLOW" {
		t.Fatalf("the new override was not applied: %v %s", dec.OverrideID, actionOf(dec))
	}
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_overrides SET expires_at = now() - interval '1 second' WHERE id = $1`, ov2); err != nil {
		t.Fatal(err)
	}
	assertUnoverridden(t, gateDecide(t, st, ctx, f), "expired (open row)")
	ov3, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "third", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatalf("the expired override did not release its slot: %v", err)
	}
	if got, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, ov2); err != nil || got.Status != domain.GateOverrideExpired || got.RevokedReason != domain.GateRevokedExpired {
		t.Errorf("the expired row was not closed by the create: %+v %v", got, err)
	}
	dec := gateDecide(t, st, ctx, f)
	if dec.OverrideID == nil || *dec.OverrideID != ov3 || dec.Override == nil || dec.Override.ID != ov3 {
		t.Errorf("after the expired closure the decision names %v, want %s", dec.OverrideID, ov3)
	}
}
