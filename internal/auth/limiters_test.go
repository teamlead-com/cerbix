package auth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/mailer"
)

// sec-hardening-2 H-6 fix 3 (invariant S6): three limiters — per IP before the body is read, per
// (IP, normalized email) after decode, per email on failures only at 5x the per-IP threshold — so
// noise from elsewhere cannot lock a real user out of a clean address.

const rightPassword = "right-password-1"

// limitedAuthenticator is a local-login authenticator with perMinute = L and one local user
// u@x.com. Password checks are stubbed (Argon2id is not what these tests measure).
func limitedAuthenticator(t *testing.T, perMinute int) (*Authenticator, *fakeStore) {
	t.Helper()
	cfg := &config.Config{
		Local:   config.LocalAuthConfig{Enabled: true, MinPasswordLength: 8, LoginRateLimitPerMinute: perMinute},
		Session: config.SessionConfig{CookieName: "cerbix_session", TTL: config.Duration(time.Hour)},
	}
	fs := newFakeStore()
	a, err := New(context.Background(), cfg, fs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if _, err := fs.CreateLocalUser(context.Background(), "u@x.com", "U", "stub-hash", false); err != nil {
		t.Fatalf("user: %v", err)
	}
	prev := verifyPasswordFn
	verifyPasswordFn = func(_, password string) (bool, error) { return password == rightPassword, nil }
	t.Cleanup(func() { verifyPasswordFn = prev })
	return a, fs
}

func login(a *Authenticator, ip, email, password string) int {
	req := jsonPost("/auth/local/login",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, email, password)))
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	a.LocalLoginHandler(rec, req)
	return rec.Code
}

func TestEmailFailuresFromOneAddressDoNotLockOutAnother(t *testing.T) {
	a, _ := limitedAuthenticator(t, 3)
	limited := 0
	for i := 0; i < 9; i++ {
		if login(a, "10.0.0.1", "u@x.com", "wrong") == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("address A was never limited")
	}
	if code := login(a, "10.0.0.2", "u@x.com", rightPassword); code != http.StatusOK {
		t.Fatalf("correct login from a clean address = %d, want 200", code)
	}
}

func TestEmailFailureLimiterTripsAcrossAddresses(t *testing.T) {
	a, _ := limitedAuthenticator(t, 3)
	for i := 0; i < 15; i++ { // 5 x L failures, each from its own address
		if code := login(a, fmt.Sprintf("10.1.0.%d", i+1), "u@x.com", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i, code)
		}
	}
	if code := login(a, "10.2.0.1", "u@x.com", rightPassword); code != http.StatusTooManyRequests {
		t.Fatalf("after 5L distributed failures = %d, want 429", code)
	}
}

func TestIPEmailLimiter(t *testing.T) {
	a, _ := limitedAuthenticator(t, 4) // per-(IP, email) = (4+1)/2 = 2
	for i := 0; i < 2; i++ {
		if code := login(a, "10.0.0.1", "a@x.com", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	if code := login(a, "10.0.0.1", "a@x.com", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("third attempt for one email from one IP = %d, want 429", code)
	}
	if code := login(a, "10.0.0.1", "b@x.com", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("another email from the same IP = %d, want 401 (not limited)", code)
	}
}

// Review Focus 4: the limiter key is the normalized email, the lookup is not.
func TestLimiterEmailNormalization(t *testing.T) {
	a, _ := limitedAuthenticator(t, 4)
	login(a, "10.0.0.1", " U@X.com", "wrong")
	login(a, "10.0.0.1", "u@x.com", "wrong")
	if code := login(a, "10.0.0.1", "u@X.COM ", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("case/space variants share the per-(IP, email) bucket: third = %d, want 429", code)
	}
	b, _ := limitedAuthenticator(t, 4)
	for i := 0; i < 20; i++ { // 5 x L failures across addresses and spellings
		email := []string{"U@x.com", " u@X.COM", "u@x.com "}[i%3]
		login(b, fmt.Sprintf("10.3.0.%d", i+1), email, "wrong")
	}
	if code := login(b, "10.4.0.1", "u@x.com", rightPassword); code != http.StatusTooManyRequests {
		t.Fatalf("spellings share the per-email failure bucket: = %d, want 429", code)
	}
}

// Review of iter-0209: at L = 2 a TOTP user's two-step sign-in (password, then the code) must fit
// in the per-(IP, email) bucket — it used to be (L+1)/2 = 1, which answered the second step 429.
func TestIPEmailLimiterFitsTheTwoStepTOTPLogin(t *testing.T) {
	for _, tc := range []struct{ perMinute, want int }{{1, 1}, {2, 2}, {3, 2}, {4, 2}, {10, 5}, {11, 6}} {
		if got := newAuthLimiters(tc.perMinute).perIPEmail.limit; got != tc.want {
			t.Errorf("L=%d: per-(IP, email) limit %d, want %d", tc.perMinute, got, tc.want)
		}
	}
	a, _ := limitedAuthenticator(t, 2)
	if code := login(a, "10.0.0.1", "u@x.com", rightPassword); code != http.StatusOK {
		t.Fatalf("first step = %d", code)
	}
	if code := login(a, "10.0.0.1", "u@x.com", rightPassword); code == http.StatusTooManyRequests {
		t.Fatal("the second step of a two-step login was limited at L=2")
	}
}

// Review of iter-0209: login and reset share the per-(IP, email) limiter, and a client IP taken
// from X-Forwarded-For is not validated — so the login key must carry its own namespace, or a
// crafted "IP" of `reset|<addr>` lands login attempts in the victim's reset bucket.
func TestLoginIPEmailKeyCannotCollideWithReset(t *testing.T) {
	a, _ := limitedAuthenticator(t, 4) // per-(IP, email) = 2
	a.trustedProxies = 1
	forged := func() int {
		req := jsonPost("/auth/local/login", strings.NewReader(`{"username":"u@x.com","password":"wrong"}`))
		req.RemoteAddr = "198.51.100.9:40000"
		req.Header.Set("X-Forwarded-For", "reset|203.0.113.7")
		rec := httptest.NewRecorder()
		a.LocalLoginHandler(rec, req)
		return rec.Code
	}
	forged()
	forged()
	req := jsonPost("/auth/local/reset/request", strings.NewReader(`{"email":"u@x.com"}`))
	req.RemoteAddr = "203.0.113.7:40000"
	rec := httptest.NewRecorder()
	a.ResetRequestHandler(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("forged login attempts filled the victim's reset bucket")
	}
}

func TestLimitersDisabledWhenZero(t *testing.T) {
	a, _ := limitedAuthenticator(t, 0)
	for i := 0; i < 30; i++ {
		if code := login(a, "10.0.0.1", "u@x.com", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d with limits off = %d, want 401", i, code)
		}
	}
	if code := login(a, "10.0.0.1", "u@x.com", rightPassword); code != http.StatusOK {
		t.Fatalf("correct login with limits off = %d, want 200", code)
	}
}

// readGuard fails the test if anything reads the body: the per-IP limiter must answer first.
type readGuard struct{ t *testing.T }

func (g readGuard) Read([]byte) (int, error) {
	g.t.Error("the body was read before the per-IP limiter refused the request")
	return 0, io.EOF
}

func TestResetConfirmLimitedBeforeBodyRead(t *testing.T) {
	a, _ := limitedAuthenticator(t, 1)
	first := jsonPost("/auth/local/reset/confirm", strings.NewReader(`{"token":"x","new_password":"longenough1"}`))
	first.RemoteAddr = "10.0.0.1:40000"
	a.ResetConfirmHandler(httptest.NewRecorder(), first)

	second := jsonPost("/auth/local/reset/confirm", readGuard{t})
	second.RemoteAddr = "10.0.0.1:40000"
	rec := httptest.NewRecorder()
	a.ResetConfirmHandler(rec, second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second confirm from one IP = %d, want 429", rec.Code)
	}
}

func TestResetRequestPerIPAndPerIPEmail(t *testing.T) {
	request := func(a *Authenticator, ip, email string) int {
		req := jsonPost("/auth/local/reset/request", strings.NewReader(fmt.Sprintf(`{"email":%q}`, email)))
		req.RemoteAddr = ip + ":40000"
		rec := httptest.NewRecorder()
		a.ResetRequestHandler(rec, req)
		return rec.Code
	}
	withMail := func(a *Authenticator) *Authenticator {
		return a.WithMailer(mailer.New("127.0.0.1", 1, "", "", "noreply@x", "http://localhost:8080"))
	}

	a, _ := limitedAuthenticator(t, 4)
	withMail(a)
	for i := 0; i < 4; i++ {
		if code := request(a, "10.0.0.1", fmt.Sprintf("n%d@x.com", i)); code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, code)
		}
	}
	if code := request(a, "10.0.0.1", "n9@x.com"); code != http.StatusTooManyRequests {
		t.Fatalf("fifth request from one IP = %d, want 429", code)
	}

	b, fs := limitedAuthenticator(t, 4)
	withMail(b)
	for i := 0; i < 2; i++ {
		if code := request(b, "10.0.0.1", "u@x.com"); code != http.StatusOK {
			t.Fatalf("request %d for u@x.com = %d, want 200", i, code)
		}
	}
	if code := request(b, "10.0.0.1", "U@x.com"); code != http.StatusTooManyRequests {
		t.Fatalf("third request for one email from one IP = %d, want 429", code)
	}
	fs.mu.Lock()
	issued := len(fs.resetTokens)
	fs.mu.Unlock()
	if issued != 2 {
		t.Fatalf("%d reset tokens issued, want 2 (the limited request issues none)", issued)
	}
}

func TestLimiterBlockedRecordsNothing(t *testing.T) {
	l := newLoginLimiter(2)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if l.blocked("k", now) {
			t.Fatalf("blocked() before any record (call %d)", i)
		}
	}
	l.record("k", now)
	if l.blocked("k", now) {
		t.Fatal("blocked after one of two")
	}
	l.record("k", now)
	if !l.blocked("k", now) {
		t.Fatal("not blocked at the limit")
	}
	if l.blocked("k", now.Add(2*time.Minute)) {
		t.Fatal("still blocked after the window")
	}
}
