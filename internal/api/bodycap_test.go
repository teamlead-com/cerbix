package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/auth"
)

// sec-hardening-2 H-6: the session/bearer API router caps every body at 1 MiB by default, and the
// self-service password change refuses a new password over 1024 bytes before hashing it.

func TestAPIRouterCapsBodiesAtOneMiB(t *testing.T) {
	fs := seededStore()
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithOIDC(&fakeOIDC{active: true}).APIRouter()
	// A syntactically valid JSON object one byte over 1 MiB.
	pad := (1 << 20) + 1 - len(`{"issuer":""}`)
	big := `{"issuer":"` + strings.Repeat("a", pad) + `"}`
	rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/oidc", big)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON body") {
		t.Fatalf("1 MiB + 1 body = %d %s, want 400 invalid JSON body", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
	if fs.oidcSettings != nil {
		t.Fatal("an oversized body was persisted")
	}
	// The same request under the cap is read.
	rec = do(h, globalAdmin, http.MethodPut, "/api/v1/settings/oidc", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("small body = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// A grouped Alertmanager webhook (max_alerts: 0 sends a whole group) can exceed 1 MiB; the receiver
// is the one authenticated route with its own, larger cap — the agent ingest's 16 MiB (owner ruling
// 2026-11-13 on the code review of iter-0209, a stated exception to sec-hardening-2 H-6 fix 2).
func TestAPIRouterAlertmanagerGetsItsOwnCap(t *testing.T) {
	fs := seededStore()
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).APIRouter()
	payload := func(size int) string {
		// Alerts with neither fingerprint nor alertname are ignored — this measures the body cap only.
		one := `{"status":"firing","labels":{"pad":"` + strings.Repeat("x", 4000) + `"}}`
		var b strings.Builder
		b.WriteString(`{"alerts":[`)
		for i := 0; b.Len() < size; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(one)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager", payload(2<<20))
	if rec.Code != http.StatusOK {
		t.Fatalf("2 MiB grouped webhook = %d %s, want 200", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
	rec = do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager", payload((16<<20)+1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("16 MiB + webhook = %d, want 400", rec.Code)
	}
	// Every other route keeps the 1 MiB default.
	rec = do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/incidents", payload(2<<20))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("2 MiB to another route = %d, want 400", rec.Code)
	}
}

func TestChangePasswordTooLongIs400(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	old, _ := auth.HashPassword("oldpass12")
	fs.passwords["u1"] = old
	body := `{"current_password":"oldpass12","new_password":"` + strings.Repeat("p", auth.MaxPasswordBytes+1) + `"}`
	rec := do(h, o1Viewer, http.MethodPost, "/api/v1/me/password", body)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "new password too long") {
		t.Fatalf("1025-byte new password = %d %s, want 400 new password too long", rec.Code, rec.Body.String())
	}
	if fs.passwords["u1"] != old {
		t.Fatal("the password changed")
	}
}
