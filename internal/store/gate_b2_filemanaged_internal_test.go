package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

// FR-024 discharge row 13 (invariant 13; §7 *Policy writes*: "file-managed service: policy write
// 200 while a paging write is still 409"): the store half.
//
// The service is made file-managed the way the product does it — a format-2 bundle applied through
// `ApplyFileManagedBundle`, which writes the `managed_services` pin — not by a bare INSERT. On that
// service the gate policy PUT and DELETE succeed and audit, while the two §16.6a paging writes on
// the SAME service answer `ErrServiceManagedByFile` (the sentinel the API maps to 409) and move
// nothing. Then the bundle-hash half: the stored last-known-good `content_hash` equals
// `bundleContentHash` over the decoded bundle before the policy exists and after it does, a
// re-apply after the gate write leaves both the hash and the policy where they were, and the
// reflection test below shows the hash's input type has no field a gate policy could travel in.
func TestGatePolicyOnAFileManagedServiceIsWritableWhilePagingIsNot(t *testing.T) {
	st, ctx := gateStore(t)
	_, projID := seedTenant(t, st, ctx)
	dp, err := fileprovider.Decode([]byte(svcBundle), config.ProviderScopeConfig{Type: "instance"})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	applyServiceBundle(t, st, ctx, svcBundle)

	var svcID, provider string
	if err := st.pool.QueryRow(ctx, `
		SELECT s.id, ms.provider_id FROM services s JOIN managed_services ms ON ms.service_id = s.id
		 WHERE s.project_id = $1 AND s.slug = 'checkout'`, projID).Scan(&svcID, &provider); err != nil {
		t.Fatalf("the bundle did not pin the service: %v", err)
	}
	if provider != "payments-bundle" {
		t.Fatalf("managed_by = %q", provider)
	}
	// The gate policy needs a service-scoped target for its window (D2); the bundle declares none.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1, $2, 50, true, $3::jsonb)`, svcID, gateWindow, "["+gatePageRule+"]"); err != nil {
		t.Fatalf("target: %v", err)
	}
	f := gateFixture{reportFixture: reportFixture{declFixture: declFixture{projectID: projID, serviceID: svcID}}}
	hashBefore := storedBundleHash(t, st, ctx)
	if hashBefore == "" || hashBefore != bundleContentHash(dp) {
		t.Fatalf("stored content_hash %q, bundleContentHash(dp) %q", hashBefore, bundleContentHash(dp))
	}
	audits := gateAuditCount(t, st, ctx)

	// The clause, first half: the policy write on a file-managed service succeeds.
	rev, changed, err := st.PutGatePolicy(ctx, projID, svcID, nil, gateDoc(nil), gateActorToken)
	if err != nil || !changed || rev != 1 {
		t.Fatalf("PutGatePolicy on a file-managed service: rev=%d changed=%v err=%v, want 1,true,nil", rev, changed, err)
	}
	p, err := st.GetGatePolicy(ctx, projID, svcID)
	if err != nil || !p.Live() || p.Revision != 1 || p.UpdatedBy != "token:ci" {
		t.Fatalf("policy after the write: %+v %v", p, err)
	}
	if n := gateAuditCount(t, st, ctx); n != audits+1 {
		t.Errorf("gate audit rows %d, want %d", n, audits+1)
	}
	// An edit and a delete are equally the operator's — no file pin is consulted anywhere on the
	// gate write path (the file cannot express a gate policy, so nothing would restate it).
	rev2 := gatePut(t, st, ctx, f, int64p(rev), gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseServiceIncidentOpen: domain.ClauseAssignBlock}))
	if rev2 != 2 {
		t.Fatalf("edit revision = %d, want 2", rev2)
	}

	// The clause, second half: on the SAME service the paging writes are refused with the
	// sentinel the API maps to 409, and nothing moves.
	var genBefore int64
	if err := st.pool.QueryRow(ctx, `SELECT alert_config_generation FROM services WHERE id = $1`, svcID).Scan(&genBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateServiceAlertPolicy(ctx, projID, svcID, FullServiceAlertPolicyPatch(pagingPolicy(domain.ServiceAlertDegraded)), AlertActor{}); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("paging PATCH on a file-managed service = %v, want ErrServiceManagedByFile", err)
	}
	if err := st.SetServiceBurnAlerting(ctx, projID, svcID, gateWindow, false, oneBurnRuleSet(), AlertActor{}); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("burn PUT on a file-managed service = %v, want ErrServiceManagedByFile", err)
	}
	var genAfter int64
	var owns bool
	if err := st.pool.QueryRow(ctx, `SELECT alert_config_generation, owns_paging FROM services WHERE id = $1`, svcID).Scan(&genAfter, &owns); err != nil {
		t.Fatal(err)
	}
	if genAfter != genBefore || owns {
		t.Errorf("a refused paging write moved the service: generation %d→%d owns_paging=%v", genBefore, genAfter, owns)
	}

	// The bundle hash: unchanged by the gate policy, and a re-apply of the SAME bundle after the
	// gate write neither restates nor removes the policy.
	if h := storedBundleHash(t, st, ctx); h != hashBefore {
		t.Errorf("content_hash moved with a gate policy write: %q → %q", hashBefore, h)
	}
	applyServiceBundle(t, st, ctx, svcBundle)
	if h := storedBundleHash(t, st, ctx); h != hashBefore || h != bundleContentHash(dp) {
		t.Errorf("content_hash after re-apply = %q, want %q (= bundleContentHash)", h, hashBefore)
	}
	p, err = st.GetGatePolicy(ctx, projID, svcID)
	if err != nil || p.Revision != rev2 || p.Clauses[domain.ClauseServiceIncidentOpen] != domain.ClauseAssignBlock {
		t.Errorf("the re-apply touched the gate policy: %+v %v", p, err)
	}
	// And the delete is the operator's too.
	if err := st.DeleteGatePolicy(ctx, projID, svcID, rev2, gateActorToken); err != nil {
		t.Errorf("DeleteGatePolicy on a file-managed service = %v, want nil", err)
	}
}

// storedBundleHash is the provider's last-known-good content_hash, the value `bundleContentHash`
// wrote for the fixture bundle.
func storedBundleHash(t *testing.T, st *Store, ctx context.Context) string {
	t.Helper()
	var h string
	if err := st.pool.QueryRow(ctx, `SELECT content_hash FROM file_provider_bundles WHERE provider_id = 'payments-bundle'`).Scan(&h); err != nil {
		t.Fatalf("bundle row: %v", err)
	}
	return h
}

// FR-024 discharge row 13, the "absent from the bundle hash" half by construction: the ONLY input
// of `bundleContentHash` is a `fileprovider.DesiredProject`, and nothing reachable from that type —
// fields, element types, map values, pointer targets, recursively — is or names a gate policy. A
// bundle format that grows a `gate:` block would add such a field and fail here, which is the
// moment the hash's exclusion has to be decided on purpose rather than inherited.
func TestBundleContentHashHasNoGatePolicyInItsInputs(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var offenders []string
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			if rt.Kind() == reflect.Map {
				walk(rt.Key(), path+"[key]")
			}
			rt = rt.Elem()
		}
		if strings.Contains(strings.ToLower(rt.Name()), "gate") {
			offenders = append(offenders, path+" : "+rt.String())
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			fld := rt.Field(i)
			name := path + "." + fld.Name
			if strings.Contains(strings.ToLower(fld.Name), "gate") || strings.Contains(strings.ToLower(fld.Tag.Get("json")), "gate") {
				offenders = append(offenders, name+" : "+fld.Type.String())
			}
			walk(fld.Type, name)
		}
	}
	walk(reflect.TypeOf(fileprovider.DesiredProject{}), "DesiredProject")
	if len(offenders) != 0 {
		t.Fatalf("the bundle hash's input can carry a gate policy through: %v", offenders)
	}
	// The domain's gate types are the ones we are excluding — the walk must have been able to see
	// them had they been there (a sanity check on the detector, not on the bundle).
	seen = map[reflect.Type]bool{}
	offenders = nil
	walk(reflect.TypeOf(struct{ P domain.GatePolicy }{}), "probe")
	if len(offenders) == 0 {
		t.Fatal("the detector did not flag domain.GatePolicy; the assertion above is vacuous")
	}
	// And the hash is a pure function of that type: two decodes of the same bundle hash alike, a
	// bundle with a different monitor set does not.
	a, err := fileprovider.Decode([]byte(svcBundle), config.ProviderScopeConfig{Type: "instance"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := fileprovider.Decode([]byte(svcBundle), config.ProviderScopeConfig{Type: "instance"})
	if err != nil {
		t.Fatal(err)
	}
	if bundleContentHash(a) != bundleContentHash(b) || bundleContentHash(a) == "" {
		t.Errorf("bundleContentHash is not a stable function of the bundle: %q vs %q", bundleContentHash(a), bundleContentHash(b))
	}
	one := strings.Replace(svcBundle, "  checkout-db:\n    name: Checkout DB\n    type: tcp\n    target: db.internal:5432\n    interval: 60s\n", "", 1)
	one = strings.Replace(one, "monitors: [checkout-http, checkout-db]", "monitors: [checkout-http]", 1)
	c, err := fileprovider.Decode([]byte(one), config.ProviderScopeConfig{Type: "instance"})
	if err != nil {
		t.Fatal(err)
	}
	if bundleContentHash(c) == bundleContentHash(a) {
		t.Error("a bundle with one monitor fewer hashes the same; the detector above is looking at the wrong function")
	}
}
