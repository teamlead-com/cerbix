package auth

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-025 D12 (invariant 17): the token's `actions` list rides onto the Principal untouched —
// a list stays a list, NULL stays nil — so authz.Can is the only place that reads it.
func TestBearerTokenActionsRideOntoThePrincipal(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	fs.apiTokens[store.HashToken("cbx_ci")] = domain.ApiToken{ID: "tk-ci", OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor, Name: "ci",
		Actions: []string{"gate:evaluate", "change:record"}}
	fs.apiTokens[store.HashToken("cbx_plain")] = domain.ApiToken{ID: "tk-plain", OrgID: "o1", Role: domain.RoleEditor, Name: "plain"}

	var got authz.Principal
	guarded := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(secret string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/services/s1/changes", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("code = %d, want 204", rec.Code)
		}
	}

	call("cbx_ci")
	if want := []authz.Action{authz.ActionGateEvaluate, authz.ActionChangeRecord}; !reflect.DeepEqual(got.Actions, want) {
		t.Fatalf("CI principal Actions = %v, want %v", got.Actions, want)
	}
	if got.AuditLabel != "token:ci" || !got.ViaToken {
		t.Fatalf("the rest of the principal changed: %+v", got)
	}
	// The intersection holds end to end: the CI principal records changes and asks the gate,
	// nothing else.
	if !got.Can(authz.ActionChangeRecord, "o1", "p1") || !got.Can(authz.ActionGateEvaluate, "o1", "p1") ||
		got.Can(authz.ActionProjectRead, "o1", "p1") || got.Can(authz.ActionGatePolicyWrite, "o1", "p1") {
		t.Fatal("the CI principal's authority is not the D12 intersection")
	}

	call("cbx_plain")
	if got.Actions != nil {
		t.Fatalf("a token without a list must yield a nil Actions, got %v", got.Actions)
	}
	if !got.Can(authz.ActionProjectRead, "o1", "") || !got.Can(authz.ActionChangeRecord, "o1", "") {
		t.Fatal("a listless editor token lost authority it had before the list existed")
	}
}
