package auth

// sec-hardening-2 H-9b: POST /auth/logout answers 200 {"redirect_to": …} instead of a 302 — the SPA
// calls it with fetch, which would follow a redirect to an external post_logout_url and be blocked
// by `connect-src 'self'`, and then navigates with window.location.assign. That makes the value a
// DOM sink, so it is checked when SERVED, whatever its source (the config file, or a row saved
// before the write check existed): a local path (`/…`, not `//…`, no backslash, no control byte)
// or http(s) with a host; anything else is served as "/".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
)

func logoutOnce(t *testing.T, a *Authenticator, fs *fakeStore, token string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	u, _ := fs.upsertHuman(context.Background(), "https://idp.test", "kc-"+token, token+"@x", "U")
	_, _ = fs.CreateSession(context.Background(), u.ID, token, time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "cerbix_session", Value: token})
	rec := httptest.NewRecorder()
	a.LogoutHandler(rec, req)
	var out struct {
		RedirectTo *string `json:"redirect_to"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.RedirectTo == nil {
		t.Fatalf("logout body %q is not {\"redirect_to\": …} (%d)", rec.Body.String(), rec.Code)
	}
	return rec, *out.RedirectTo
}

var postLogoutCases = []struct{ stored, want string }{
	{"", "/"},
	{"https://idp.example/realms/x/protocol/openid-connect/logout?post_logout_redirect_uri=https%3A%2F%2Fcerbix.example%2F", "https://idp.example/realms/x/protocol/openid-connect/logout?post_logout_redirect_uri=https%3A%2F%2Fcerbix.example%2F"},
	{"http://intranet.example/bye", "http://intranet.example/bye"},
	{"/signed-out", "/signed-out"},
	{"javascript:alert(document.cookie)", "/"},
	{"JavaScript:alert(1)", "/"},
	{"/\\evil.example", "/"},
	{"/\\/evil.example", "/"},
	{"//evil.example/x", "/"},
	{"data:text/html,<script>alert(1)</script>", "/"},
	{"https://", "/"},
	{"https:evil.example", "/"},
	{"/a\tb", "/"},
	{"/a\x00b", "/"},
	{"evil.example", "/"},
}

// A row in oidc_settings seeded with any value — the write check did not exist when it was saved.
func TestLogoutServesOnlyASafePostLogoutURLFromTheStoredRow(t *testing.T) {
	mock := newMockOIDC(t, "cerbix")
	for i, c := range postLogoutCases {
		fs := newFakeStore()
		a := testAuthenticator(t, fs, mock)
		fs.oidc = &domain.OIDCSettings{Enabled: true, Issuer: mock.server.URL, ClientID: mock.clientID, ClientSecret: "secret",
			RedirectURL: "http://cerbix.test/auth/callback", Scopes: []string{"openid"}, PostLogoutURL: c.stored}
		if err := a.SyncOIDC(context.Background()); err != nil {
			t.Fatalf("sync: %v", err)
		}
		tok := "tok-row-" + itoa(i)
		rec, got := logoutOnce(t, a, fs, tok)
		if rec.Code != http.StatusOK {
			t.Errorf("stored %q: logout = %d, want 200", c.stored, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("stored %q: Content-Type %q", c.stored, ct)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("stored %q: a Location header %q — logout no longer redirects", c.stored, loc)
		}
		if got != c.want {
			t.Errorf("stored %q: redirect_to = %q, want %q", c.stored, got, c.want)
		}
		if _, err := fs.SessionByToken(context.Background(), tok); err == nil {
			t.Errorf("stored %q: the session survived logout", c.stored)
		}
	}
}

// The config-file bootstrap is served through the same check.
func TestLogoutServesOnlyASafePostLogoutURLFromTheConfigFile(t *testing.T) {
	mock := newMockOIDC(t, "cerbix")
	for i, c := range postLogoutCases {
		fs := newFakeStore()
		a := testAuthenticatorWith(t, fs, mock, func(cfg *config.Config) { cfg.OIDC.PostLogoutRedirectURL = c.stored })
		_, got := logoutOnce(t, a, fs, "tok-cfg-"+itoa(i))
		if got != c.want {
			t.Errorf("config %q: redirect_to = %q, want %q", c.stored, got, c.want)
		}
	}
}
