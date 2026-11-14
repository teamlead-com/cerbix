package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// S4 (sec-hardening-2 H-4 fix 1): the predicate is the SCHEME. A request whose Authorization names
// Bearer is authenticated by that one credential or not at all — its cookie is never read, so a
// junk, empty or duplicated Bearer beside a live session is 401, never the session (which the
// forgery check, exempting bearer requests, would otherwise let through). A request with no
// Authorization, or a proxy's Basic/Negotiate, is authenticated by the cookie alone.
func TestRequireAuthNeverReadsTheCookieOfABearerRequest(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	fs.usersByID["u-human"] = domain.User{ID: "u-human", Email: "ada@x.test"}
	if _, err := fs.CreateSession(context.Background(), "u-human", "live-session", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fs.apiTokens[store.HashToken("cbx_good")] = domain.ApiToken{ID: "tk1", OrgID: "o1", Role: domain.RoleEditor, Name: "ci"}

	var got authz.Principal
	guarded := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	cases := []struct {
		name      string
		cookie    bool
		authz     []string
		want      int
		wantToken bool // the principal is the token's, not the session's
	}{
		{"cookie alone", true, nil, 204, false},
		{"cookie + Basic (a proxy's)", true, []string{"Basic dXNlcjpwYXNz"}, 204, false},
		{"cookie + Negotiate", true, []string{"Negotiate YII="}, 204, false},
		{"cookie + Bearer junk", true, []string{"Bearer junk"}, 401, false},
		{"cookie + empty Bearer", true, []string{"Bearer "}, 401, false},
		{"cookie + bare scheme", true, []string{"bearer"}, 401, false},
		{"cookie + BEARER junk (case)", true, []string{"BEARER junk"}, 401, false},
		{"cookie + two good Bearers", true, []string{"Bearer cbx_good", "Bearer cbx_good"}, 401, false},
		{"cookie + Basic and Bearer junk", true, []string{"Basic dXNlcjpwYXNz", "Bearer junk"}, 401, false},
		{"cookie + good Bearer", true, []string{"Bearer cbx_good"}, 204, true},
		{"good Bearer alone", false, []string{"Bearer cbx_good"}, 204, true},
		{"Basic alone", false, []string{"Basic dXNlcjpwYXNz"}, 401, false},
		{"nothing", false, nil, 401, false},
	}
	for _, c := range cases {
		got = authz.Principal{}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/incidents", nil)
		if c.cookie {
			req.AddCookie(&http.Cookie{Name: "cerbix_session", Value: "live-session"})
		}
		for _, v := range c.authz {
			req.Header.Add("Authorization", v)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
			continue
		}
		if c.want == 204 && got.ViaToken != c.wantToken {
			t.Errorf("%s: authenticated as %q (via token %v), want via token %v", c.name, got.UserID, got.ViaToken, c.wantToken)
		}
	}
}
