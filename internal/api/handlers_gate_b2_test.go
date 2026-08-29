package api_test

import (
	"net/http"
	"testing"
)

// FR-024 discharge row 13 (invariant 13; §7 *Policy writes*: "file-managed service: policy write
// 200 while a paging write is still 409"): the HTTP half, against the scripted fake whose
// `fileManaged` pin mirrors the real store's `managed_services` refusal on the §16.6a writes and
// — like the real store — has no pin check on the gate policy path.
//
// One principal, one service: `PUT …/gate/policy` is 200 and `DELETE …/gate/policy` is 204, while
// `PATCH …/alerting` and `PUT …/sla-target/burn-alerting` on the same service are 409
// `managed_by_file` and attempt no write.
func TestGatePolicyPutOnAFileManagedServiceIs200WhilePagingPatchIs409(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	svc := fs.serviceStore()[gateSvcID]
	svc.fileManaged = true
	svc.burnTargets = map[string]*fakeBurnTarget{"30d": {}}
	g := fs.gateState()
	g.putRevision = 1
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	base := "/api/v1/projects/p1/services/" + gateSvcID

	body := wantStatus(t, do(h, gateEditor, http.MethodPut, base+"/gate/policy", gatePolicyBody), http.StatusOK, "")
	if string(body) != "{\"revision\":1}\n" {
		t.Fatalf("policy PUT body = %q", body)
	}
	if calls := fs.gateCalls(); len(calls) != 1 || calls[0] != "PutGatePolicy" {
		t.Fatalf("store calls = %v", calls)
	}

	wantStatus(t, do(h, gateEditor, http.MethodPatch, base+"/alerting", `{"owns_paging":true}`), http.StatusConflict, "managed_by_file")
	wantStatus(t, do(h, gateEditor, http.MethodPut, base+"/sla-target/burn-alerting",
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page"}]}`),
		http.StatusConflict, "managed_by_file")
	if svc.alertWrites != 0 || svc.burnWrites != 0 || svc.alerting != nil {
		t.Fatalf("a 409 wrote something: alertWrites=%d burnWrites=%d alerting=%+v", svc.alertWrites, svc.burnWrites, svc.alerting)
	}

	wantStatus(t, do(h, gateEditor, http.MethodDelete, base+"/gate/policy?expected_revision=1", ""), http.StatusNoContent, "")
	if calls := fs.gateCalls(); len(calls) != 2 || calls[1] != "DeleteGatePolicy" {
		t.Fatalf("store calls = %v", calls)
	}
}
