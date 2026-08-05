package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/config"
	"git.example.com/monitoring/cerbix/internal/store"
	"git.example.com/monitoring/cerbix/internal/totp"
)

// localAuthenticator builds an OIDC-disabled, local-login-enabled Authenticator.
func localAuthenticator(t *testing.T, fs *fakeStore, bootEmail, bootPass string) *Authenticator {
	t.Helper()
	cfg := &config.Config{
		Local: config.LocalAuthConfig{
			Enabled: true, MinPasswordLength: 8,
		},
		Security: config.SecurityConfig{AdminEmail: bootEmail, AdminPassword: bootPass},
		Session:  config.SessionConfig{CookieName: "cerbix_session", TTL: config.Duration(time.Hour), Secure: false},
	}
	a, err := New(context.Background(), cfg, fs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("auth.New (local): %v", err)
	}
	return a
}

func TestLocalLogin(t *testing.T) {
	fs := newFakeStore()
	a := localAuthenticator(t, fs, "", "")
	hash, _ := HashPassword("pw12345678")
	if _, err := fs.CreateLocalUser(context.Background(), "admin@x", "Admin", hash, true); err != nil {
		t.Fatalf("seed local user: %v", err)
	}

	// Success.
	rec := httptest.NewRecorder()
	a.LocalLoginHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/login",
		strings.NewReader(`{"username":"admin@x","password":"pw12345678"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login code = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !hasCookie(rec, "cerbix_session") {
		t.Fatal("no session cookie set on success")
	}
	if len(fs.sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(fs.sessions))
	}

	// Wrong password → 401.
	rec = httptest.NewRecorder()
	a.LocalLoginHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/login",
		strings.NewReader(`{"username":"admin@x","password":"wrong"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password code = %d, want 401", rec.Code)
	}

	// Unknown user → 401 (uniform).
	rec = httptest.NewRecorder()
	a.LocalLoginHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/login",
		strings.NewReader(`{"username":"ghost@x","password":"whatever"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user code = %d, want 401", rec.Code)
	}
}

func TestLocalLoginWithTOTP(t *testing.T) {
	fs := newFakeStore()
	a := localAuthenticator(t, fs, "", "")
	hash, _ := HashPassword("pw12345678")
	u, err := fs.CreateLocalUser(context.Background(), "admin@x", "Admin", hash, true)
	if err != nil {
		t.Fatalf("seed local user: %v", err)
	}
	// Enable 2FA on the seeded credential with a known secret + one recovery code.
	secret, err := totp.GenerateSecret()
	if err != nil {
		t.Fatalf("gen secret: %v", err)
	}
	fs.mu.Lock()
	cred := fs.localCreds["admin@x"]
	cred.TOTPEnabled = true
	cred.TOTPSecret = secret
	fs.localCreds["admin@x"] = cred
	if fs.recovery == nil {
		fs.recovery = map[string]bool{}
	}
	fs.recovery[u.ID+"|"+store.HashToken("rescue-code")] = true
	fs.mu.Unlock()

	login := func(pw, code string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		body := `{"username":"admin@x","password":"` + pw + `"`
		if code != "" {
			body += `,"totp":"` + code + `"`
		}
		body += `}`
		a.LocalLoginHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(body)))
		return rec
	}

	// Password OK but no second factor → 401 with totp_required.
	rec := login("pw12345678", "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "totp_required") {
		t.Fatalf("missing 2FA: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Correct current TOTP → 200 + session.
	code, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("totp code: %v", err)
	}
	rec = login("pw12345678", code)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid TOTP login code=%d body=%s", rec.Code, rec.Body.String())
	}

	// A recovery code works once, then is consumed.
	rec = login("pw12345678", "rescue-code")
	if rec.Code != http.StatusOK {
		t.Fatalf("recovery login code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = login("pw12345678", "rescue-code")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused recovery code should fail: code=%d", rec.Code)
	}

	// Wrong password never reaches 2FA (uniform 401, no totp_required hint).
	rec = login("wrong", code)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "totp_required") {
		t.Fatalf("wrong password should be uniform 401: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEnsureBootstrapAdmin(t *testing.T) {
	// With email+password on an empty system → creates a global admin.
	fs := newFakeStore()
	a := localAuthenticator(t, fs, "root@x", "bootpass123")
	if err := a.EnsureBootstrapAdmin(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cred, err := fs.LocalCredentialByEmail(context.Background(), "root@x")
	if err != nil || !cred.IsGlobalAdmin {
		t.Fatalf("bootstrap admin not created: %+v err=%v", cred, err)
	}
	// Idempotent: existing users → no second account.
	before := len(fs.usersByID)
	if err := a.EnsureBootstrapAdmin(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if len(fs.usersByID) != before {
		t.Fatal("bootstrap should be a no-op when users exist")
	}
}

func TestEnsureBootstrapAdminSkipsWithoutPassword(t *testing.T) {
	fs := newFakeStore()
	a := localAuthenticator(t, fs, "root@x", "") // email but no password
	if err := a.EnsureBootstrapAdmin(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(fs.usersByID) != 0 {
		t.Fatal("no admin should be created without a bootstrap password")
	}
}

func TestLocalRoutesRegistered(t *testing.T) {
	fs := newFakeStore()
	a := localAuthenticator(t, fs, "", "")
	mux := http.NewServeMux()
	a.Routes(mux)
	// OIDC disabled → /auth/login must NOT be routed; /auth/local/login must be.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(`{}`)))
	if rec.Code == http.StatusNotFound {
		t.Fatal("/auth/local/login should be registered when local is enabled")
	}
}
