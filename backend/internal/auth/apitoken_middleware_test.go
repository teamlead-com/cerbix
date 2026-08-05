package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.example.com/monitoring/cerbix/internal/authz"
	"git.example.com/monitoring/cerbix/internal/domain"
	"git.example.com/monitoring/cerbix/internal/store"
)

func TestRequireAuthBearerToken(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	fs.apiTokens[store.HashToken("cbx_secret")] = domain.ApiToken{ID: "tk1", OrgID: "o1", Role: domain.RoleEditor, Name: "ci"}

	var got authz.Principal
	guarded := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	// Valid bearer token → principal built from the token, flagged ViaToken.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/incidents", nil)
	req.Header.Set("Authorization", "Bearer cbx_secret")
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid token code = %d, want 204", rec.Code)
	}
	if !got.ViaToken {
		t.Error("principal should be flagged ViaToken")
	}
	if len(got.Memberships) != 1 || got.Memberships[0].OrgID != "o1" || got.Memberships[0].Role != domain.RoleEditor {
		t.Fatalf("token membership = %+v, want one o1/editor", got.Memberships)
	}
	if len(fs.touched) != 1 || fs.touched[0] != "tk1" {
		t.Fatalf("token should be touched once, got %+v", fs.touched)
	}

	// Unknown token → 401.
	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	guarded.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token code = %d, want 401", rec.Code)
	}
}
