package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
)

func TestClientCredentialsBearer(t *testing.T) {
	fs := newFakeStore()
	mock := newMockOIDC(t, "cerbix")
	a := testAuthenticator(t, fs, mock)

	// An OIDC service account: a subject, no email/nonce.
	mock.sub, mock.email, mock.name, mock.nn = "service-account-ci", "", "", ""
	token := mock.mintIDToken(t)

	var got authz.Principal
	var reached bool
	guarded := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	// A valid client-credentials JWT authenticates and JIT-provisions the identity.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("client-creds JWT code = %d, want 204", rec.Code)
	}
	if !reached || !got.ViaToken || got.UserID == "" {
		t.Fatalf("principal not built from JWT: reached=%v %+v", reached, got)
	}
	if u, ok := fs.usersBySub["service-account-ci"]; !ok || u.Email != "service-account-ci@clients" {
		t.Fatalf("service account not provisioned: %+v (ok=%v)", u, ok)
	}
	// A service account with no memberships has none (authorization is membership-gated).
	if len(got.Memberships) != 0 {
		t.Fatalf("unexpected memberships: %+v", got.Memberships)
	}

	// A non-JWT / invalid bearer falls through to 401.
	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	bad.Header.Set("Authorization", "Bearer not.a.valid.jwt")
	guarded.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer code = %d, want 401", rec.Code)
	}
}
