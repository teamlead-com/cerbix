package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/mailer"
	"github.com/teamlead-com/cerbix/internal/store"
)

func resetAuthenticator(t *testing.T, fs *fakeStore, withMail bool) *Authenticator {
	t.Helper()
	a := localAuthenticator(t, fs, "", "")
	if withMail {
		// Unreachable SMTP: Send fails fast, but the token is created before sending.
		a.WithMailer(mailer.New("127.0.0.1", 1, "", "", "noreply@x", "http://localhost:8080"))
	}
	return a
}

func TestResetRequestCreatesToken(t *testing.T) {
	fs := newFakeStore()
	hash, _ := HashPassword("pw12345678")
	if _, err := fs.CreateLocalUser(context.Background(), "user@x", "User", hash, false); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	a := resetAuthenticator(t, fs, true)

	// Known local email → 200 and a reset token is created.
	rec := httptest.NewRecorder()
	a.ResetRequestHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/request",
		strings.NewReader(`{"email":"user@x"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("request code = %d, want 200", rec.Code)
	}
	if len(fs.resetTokens) != 1 {
		t.Fatalf("expected 1 reset token, got %d", len(fs.resetTokens))
	}

	// Unknown email → still 200 (no enumeration) and no new token.
	rec = httptest.NewRecorder()
	a.ResetRequestHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/request",
		strings.NewReader(`{"email":"ghost@x"}`)))
	if rec.Code != http.StatusOK || len(fs.resetTokens) != 1 {
		t.Fatalf("unknown email: code=%d tokens=%d, want 200/1", rec.Code, len(fs.resetTokens))
	}
}

func TestResetRequestDisabledWithoutMailer(t *testing.T) {
	fs := newFakeStore()
	hash, _ := HashPassword("pw12345678")
	_, _ = fs.CreateLocalUser(context.Background(), "user@x", "User", hash, false)
	a := resetAuthenticator(t, fs, false) // no mailer

	rec := httptest.NewRecorder()
	a.ResetRequestHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/request",
		strings.NewReader(`{"email":"user@x"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("request code = %d, want 200", rec.Code)
	}
	if len(fs.resetTokens) != 0 {
		t.Fatal("no token should be created without a mailer")
	}
}

func TestResetConfirm(t *testing.T) {
	fs := newFakeStore()
	oldHash, _ := HashPassword("oldpassword")
	u, _ := fs.CreateLocalUser(context.Background(), "user@x", "User", oldHash, false)
	a := resetAuthenticator(t, fs, true)

	seed := func(raw string, exp time.Time, used bool) {
		fs.mu.Lock()
		if fs.resetTokens == nil {
			fs.resetTokens = map[string]resetTok{}
		}
		fs.resetTokens[store.HashToken(raw)] = resetTok{userID: u.ID, expires: exp, used: used}
		fs.mu.Unlock()
	}

	// Valid token → 204 and the password is updated.
	seed("goodtok", time.Now().Add(time.Hour), false)
	rec := httptest.NewRecorder()
	a.ResetConfirmHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/confirm",
		strings.NewReader(`{"token":"goodtok","new_password":"brandnewpass"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("confirm code = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if h := fs.passwords[u.ID]; h == "" {
		t.Fatal("password not set after reset")
	} else if ok, _ := VerifyPassword(h, "brandnewpass"); !ok {
		t.Fatal("new password does not verify")
	}

	// Reusing the same token → 400 (single-use).
	rec = httptest.NewRecorder()
	a.ResetConfirmHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/confirm",
		strings.NewReader(`{"token":"goodtok","new_password":"anotherpass1"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reused token code = %d, want 400", rec.Code)
	}

	// Expired token → 400.
	seed("expiredtok", time.Now().Add(-time.Minute), false)
	rec = httptest.NewRecorder()
	a.ResetConfirmHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/confirm",
		strings.NewReader(`{"token":"expiredtok","new_password":"anotherpass1"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expired token code = %d, want 400", rec.Code)
	}

	// Unknown token → 400.
	rec = httptest.NewRecorder()
	a.ResetConfirmHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/confirm",
		strings.NewReader(`{"token":"nope","new_password":"anotherpass1"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown token code = %d, want 400", rec.Code)
	}

	// Too-short new password → 400 (before touching the token).
	seed("shortpwtok", time.Now().Add(time.Hour), false)
	rec = httptest.NewRecorder()
	a.ResetConfirmHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/confirm",
		strings.NewReader(`{"token":"shortpwtok","new_password":"short"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short password code = %d, want 400", rec.Code)
	}
}
