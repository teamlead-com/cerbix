package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"git.example.com/monitoring/cerbix/internal/config"
	"git.example.com/monitoring/cerbix/internal/store"
)

// mockOIDC is a minimal, hermetic OIDC provider: discovery + JWKS + token.
type mockOIDC struct {
	server               *httptest.Server
	key                  *rsa.PrivateKey
	clientID             string
	sub, email, name, nn string // claims for the next minted id_token
}

func newMockOIDC(t *testing.T, clientID string) *mockOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	m := &mockOIDC{key: key, clientID: clientID}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := m.server.URL
		writeJSONRaw(w, map[string]any{
			"issuer":                 iss,
			"authorization_endpoint": iss + "/authorize",
			"token_endpoint":         iss + "/token",
			"jwks_uri":               iss + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONRaw(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONRaw(w, map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     m.mintIDToken(t),
		})
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockOIDC) mintIDToken(t *testing.T) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	now := time.Now()
	claims := jwt.Claims{
		Issuer:   m.server.URL,
		Subject:  m.sub,
		Audience: jwt.Audience{m.clientID},
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(now),
	}
	custom := map[string]any{"nonce": m.nn, "email": m.email, "name": m.name}
	raw, err := jwt.Signed(sig).Claims(claims).Claims(custom).Serialize()
	if err != nil {
		t.Fatalf("sign id_token: %v", err)
	}
	return raw
}

func writeJSONRaw(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testAuthenticator(t *testing.T, fs *fakeStore, mock *mockOIDC, bootstrap ...string) *Authenticator {
	t.Helper()
	cfg := &config.Config{
		OIDC: config.OIDCConfig{
			Issuer:               mock.server.URL,
			ClientID:             mock.clientID,
			ClientSecret:         "secret",
			RedirectURL:          "http://cerbix.test/auth/callback",
			Scopes:               []string{"openid", "email", "profile"},
			BootstrapAdminEmails: bootstrap,
		},
		Session: config.SessionConfig{CookieName: "cerbix_session", TTL: config.Duration(time.Hour), Secure: false},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(context.Background(), cfg, fs, logger)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	// Build the OIDC provider synchronously against the mock issuer (no DB override,
	// so this resolves the config bootstrap).
	if err := a.SyncOIDC(context.Background()); err != nil {
		t.Fatalf("sync oidc: %v", err)
	}
	return a
}

func TestLoginHandlerStoresFlowAndRedirects(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))

	rec := httptest.NewRecorder()
	a.LoginHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/login?redirect=/dash", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("login code = %d, want 302", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("no state in redirect")
	}
	if loc.Query().Get("code_challenge") == "" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("PKCE challenge missing from redirect")
	}
	if _, ok := fs.flows[state]; !ok {
		t.Fatal("auth flow not stored")
	}
	if fs.flows[state].RedirectTo != "/dash" {
		t.Fatalf("redirect_to = %q, want /dash", fs.flows[state].RedirectTo)
	}
}

func TestCallbackProvisionsUserAndCreatesSession(t *testing.T) {
	fs := newFakeStore()
	mock := newMockOIDC(t, "cerbix")
	a := testAuthenticator(t, fs, mock, "admin@x.com")

	// Seed a pending flow and align the provider's minted claims to it.
	_ = fs.CreateAuthFlow(context.Background(), store.AuthFlow{
		State: "s1", Nonce: "n1", PKCEVerifier: "verifier-123456789012345678901234567890123456789012",
		RedirectTo: "/dash", ExpiresAt: time.Now().Add(time.Minute),
	})
	mock.sub, mock.email, mock.name, mock.nn = "kc-sub-1", "admin@x.com", "Admin", "n1"

	rec := httptest.NewRecorder()
	a.CallbackHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&code=abc", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("callback code = %d (%s), want 302", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "/dash" {
		t.Fatalf("redirect = %q, want /dash", rec.Header().Get("Location"))
	}
	// User provisioned + promoted to global admin via bootstrap.
	u, ok := fs.usersBySub["kc-sub-1"]
	if !ok || u.Email != "admin@x.com" {
		t.Fatalf("user not provisioned: %+v", u)
	}
	if !u.IsGlobalAdmin {
		t.Fatal("bootstrap admin email should have granted global admin")
	}
	// Session cookie set and stored.
	if len(fs.sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(fs.sessions))
	}
	if !hasCookie(rec, "cerbix_session") {
		t.Fatal("session cookie not set")
	}
	// Flow consumed (single use).
	if _, ok := fs.flows["s1"]; ok {
		t.Fatal("auth flow should be consumed")
	}
}

func TestCallbackRejectsUnknownState(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	rec := httptest.NewRecorder()
	a.CallbackHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?state=nope&code=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown state code = %d, want 400", rec.Code)
	}
}

func TestCallbackRejectsNonceMismatch(t *testing.T) {
	fs := newFakeStore()
	mock := newMockOIDC(t, "cerbix")
	a := testAuthenticator(t, fs, mock)
	_ = fs.CreateAuthFlow(context.Background(), store.AuthFlow{
		State: "s2", Nonce: "expected", PKCEVerifier: "v", RedirectTo: "/", ExpiresAt: time.Now().Add(time.Minute),
	})
	mock.sub, mock.email, mock.nn = "kc-2", "u@x", "WRONG-NONCE"
	rec := httptest.NewRecorder()
	a.CallbackHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?state=s2&code=abc", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nonce mismatch code = %d, want 401", rec.Code)
	}
}

func TestRequireAuthMiddleware(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))

	// No cookie → 401.
	rec := httptest.NewRecorder()
	guarded := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFrom(r.Context()); !ok {
			t.Error("principal missing in guarded handler")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie code = %d, want 401", rec.Code)
	}

	// Valid session → handler runs with principal.
	u, _ := fs.UpsertUserByOIDCSub(context.Background(), "kc-9", "u@x", "U")
	_, _ = fs.CreateSession(context.Background(), u.ID, "tok-9", time.Now().Add(time.Hour))
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: "cerbix_session", Value: "tok-9"})
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid session code = %d, want 204", rec.Code)
	}
}

func TestLogoutClearsSession(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	u, _ := fs.UpsertUserByOIDCSub(context.Background(), "kc-10", "u@x", "U")
	_, _ = fs.CreateSession(context.Background(), u.ID, "tok-10", time.Now().Add(time.Hour))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "cerbix_session", Value: "tok-10"})
	a.LogoutHandler(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("logout code = %d, want 302", rec.Code)
	}
	if _, err := fs.SessionByToken(context.Background(), "tok-10"); err != store.ErrNotFound {
		t.Fatal("session should be deleted after logout")
	}
}

func TestCallbackErrorAndMissingParams(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))

	// Provider returned an error.
	rec := httptest.NewRecorder()
	a.CallbackHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?error=access_denied", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("error param code = %d, want 400", rec.Code)
	}
	// Missing state/code.
	rec = httptest.NewRecorder()
	a.CallbackHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing params code = %d, want 400", rec.Code)
	}
}

func TestLoginSanitizesOpenRedirect(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	rec := httptest.NewRecorder()
	a.LoginHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/login?redirect=//evil.example/x", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	if fs.flows[state].RedirectTo != "/" {
		t.Fatalf("protocol-relative redirect should be sanitized to /, got %q", fs.flows[state].RedirectTo)
	}
}

func TestLogoutWithoutCookie(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	rec := httptest.NewRecorder()
	a.LogoutHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("logout without cookie = %d, want 302", rec.Code)
	}
}

func TestRoutesRegister(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	mux := http.NewServeMux()
	a.Routes(mux)
	// /auth/login should be routed (302 redirect to provider).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("routed login = %d, want 302", rec.Code)
	}
}

func hasCookie(rec *httptest.ResponseRecorder, name string) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
}
