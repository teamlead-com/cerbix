package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/store"
)

// sec-hardening-2 H-6 (invariant S6): no endpoint hashes a password longer than 1024 bytes, and no
// unauthenticated endpoint reads more than 4 KiB.

func TestPasswordByteCap(t *testing.T) {
	exact := strings.Repeat("a", MaxPasswordBytes)
	h, err := HashPassword(exact)
	if err != nil {
		t.Fatalf("1024 bytes: %v", err)
	}
	if ok, err := VerifyPassword(h, exact); err != nil || !ok {
		t.Fatalf("verify 1024 bytes: ok=%v err=%v", ok, err)
	}
	over := exact + "a"
	if _, err := HashPassword(over); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("hash 1025 bytes: err %v, want ErrPasswordTooLong", err)
	}
	if ok, err := VerifyPassword(h, over); ok || !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("verify 1025 bytes: ok=%v err=%v, want ErrPasswordTooLong", ok, err)
	}
	// Bytes, not characters: 342 three-byte runes are 1026 bytes.
	runes := strings.Repeat("€", 342)
	if _, err := HashPassword(runes); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("hash 342 x €: err %v, want ErrPasswordTooLong", err)
	}
}

// localRoutes serves the auth routes of a local-login authenticator.
func localRoutes(t *testing.T, a *Authenticator) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	a.Routes(mux)
	return mux
}

func TestLocalAuthBodyCap(t *testing.T) {
	a := resetAuthenticator(t, newFakeStore(), true)
	h := localRoutes(t, a)
	for _, path := range []string{"/auth/local/login", "/auth/local/reset/request", "/auth/local/reset/confirm"} {
		// A syntactically valid JSON object one byte over the cap.
		pad := 4097 - len(`{"x":""}`)
		big := `{"x":"` + strings.Repeat("a", pad) + `"}`
		if len(big) != 4097 {
			t.Fatalf("fixture is %d bytes", len(big))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(big)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s with 4097 bytes = %d, want 400", path, rec.Code)
		}
		// Under the cap, the body is read normally (whatever the handler then answers).
		small := `{"x":"` + strings.Repeat("a", 4000-len(`{"x":""}`)) + `"}`
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(small)))
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "invalid JSON") {
			t.Fatalf("%s with 4000 bytes refused as unreadable: %s", path, rec.Body.String())
		}
	}
}

func TestResetConfirmTooLongIs400(t *testing.T) {
	fs := newFakeStore()
	oldHash, _ := HashPassword("oldpassword")
	u, _ := fs.CreateLocalUser(context.Background(), "user@x", "User", oldHash, false)
	a := resetAuthenticator(t, fs, true)
	fs.mu.Lock()
	fs.resetTokens = map[string]resetTok{store.HashToken("tok"): {userID: u.ID, expires: time.Now().Add(time.Hour)}}
	fs.mu.Unlock()

	body := `{"token":"tok","new_password":"` + strings.Repeat("p", MaxPasswordBytes+1) + `"}`
	rec := httptest.NewRecorder()
	a.ResetConfirmHandler(rec, httptest.NewRequest(http.MethodPost, "/auth/local/reset/confirm", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "new password too long") {
		t.Fatalf("1025-byte password = %d %s, want 400 new password too long", rec.Code, rec.Body.String())
	}
	if fs.resetTokens[store.HashToken("tok")].used {
		t.Fatal("a refused confirm consumed the token")
	}
}
