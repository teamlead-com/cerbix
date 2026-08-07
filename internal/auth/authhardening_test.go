package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// TestCallbackAllowlistFailsClosed proves an enforced email allowlist rejects a
// login with a missing or unverified email (the enumeration/bypass fix), and
// accepts only a verified, permitted one.
func TestCallbackAllowlistFailsClosed(t *testing.T) {
	run := func(t *testing.T, email string, verified bool, wantCode int) {
		t.Helper()
		fs := newFakeStore()
		mock := newMockOIDC(t, "cerbix")
		a := testAuthenticator(t, fs, mock)
		a.WithSettings(fakePolicy{p: domain.AuthPolicy{AllowedEmailDomains: []string{"x.com"}}})

		_ = fs.CreateAuthFlow(context.Background(), store.AuthFlow{
			State: "s1", Nonce: "n1", PKCEVerifier: "verifier-123456789012345678901234567890123456789012",
			RedirectTo: "/dash", ExpiresAt: time.Now().Add(time.Minute),
		})
		mock.sub, mock.email, mock.name, mock.nn = "kc-sub", email, "N", "n1"
		mock.emailVerified = verified

		rec := httptest.NewRecorder()
		a.CallbackHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&code=abc", nil))
		if rec.Code != wantCode {
			t.Fatalf("email=%q verified=%v: code = %d, want %d (%s)", email, verified, rec.Code, wantCode, rec.Body.String())
		}
	}
	// Allowlist enforced:
	run(t, "", true, http.StatusForbidden)               // no email → blocked (was the bypass)
	run(t, "admin@x.com", false, http.StatusForbidden)   // unverified → blocked
	run(t, "admin@evil.com", true, http.StatusForbidden) // wrong domain → blocked
	run(t, "admin@x.com", true, http.StatusFound)        // verified + permitted → allowed
}

// TestLoginTOTPPolicyFailsClosed proves that when TOTP is mandatory but the user
// record can't be loaded to evaluate the policy, the login is refused (500) rather
// than issuing an MFA-less session.
func TestLoginTOTPPolicyFailsClosed(t *testing.T) {
	fs := newFakeStore()
	a := localAuthenticator(t, fs, "", "")
	a.WithSettings(fakePolicy{p: domain.AuthPolicy{MinPasswordLen: 8, RequireTOTP: domain.TOTPAll}})
	hash, _ := HashPassword("pw12345678")
	u, err := fs.CreateLocalUser(context.Background(), "u@x", "U", hash, false)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Simulate the user row being unreadable when the TOTP policy is evaluated.
	delete(fs.usersByID, u.ID)

	rec := httptest.NewRecorder()
	a.LocalLoginHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/login",
		strings.NewReader(`{"username":"u@x","password":"pw12345678"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (fail-closed, no MFA-less session)", rec.Code)
	}
	if len(fs.sessions) != 0 {
		t.Fatalf("no session must be issued on the fail-closed path, got %d", len(fs.sessions))
	}
}
