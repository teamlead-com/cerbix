package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"git.example.com/monitoring/cerbix/internal/api"
)

type fakeOIDC struct {
	active  bool
	syncErr error
	syncs   int
}

func (f *fakeOIDC) SyncOIDC(context.Context) error {
	f.syncs++
	if f.syncErr != nil {
		f.active = false
		return f.syncErr
	}
	return nil
}
func (f *fakeOIDC) OIDCActive() bool { return f.active }

func newOIDCHandler(fs *fakeStore, o api.OIDCController) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithOIDC(o).Router()
}

func TestOIDCSettingsReadWrite(t *testing.T) {
	fs := seededStore()
	ctrl := &fakeOIDC{active: true}
	h := newOIDCHandler(fs, ctrl)

	// Non-global-admin cannot read or write.
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/settings/oidc", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("org-admin read = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/settings/oidc", `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("org-admin write = %d, want 403", rec.Code)
	}

	// No override yet → configured false.
	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/settings/oidc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read = %d, want 200", rec.Code)
	}
	var v struct {
		Configured, Active, Enabled, ClientSecretSet bool
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Configured || !v.Active {
		t.Fatalf("initial view = %+v (want configured false, active true)", v)
	}

	// Enabling without required fields → 400.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/oidc", `{"enabled":true,"issuer":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without issuer = %d, want 400", rec.Code)
	}

	// Valid save → 200, persisted, provider re-synced.
	body := `{"enabled":true,"issuer":"https://idp.example/realms/x","client_id":"cerbix","client_secret":"s3cr3t","redirect_url":"https://cerbix/auth/callback","scopes":["openid","email"],"button_label":"Continue with Keycloak"}`
	rec = do(h, globalAdmin, http.MethodPut, "/api/v1/settings/oidc", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ctrl.syncs != 1 {
		t.Fatalf("SyncOIDC called %d times, want 1", ctrl.syncs)
	}
	if fs.oidcSettings == nil || fs.oidcSettings.ClientSecret != "s3cr3t" || fs.oidcSettings.ButtonLabel != "Continue with Keycloak" {
		t.Fatalf("persisted settings = %+v", fs.oidcSettings)
	}

	// Read back: secret redacted, only client_secret_set true, never the value.
	rec = do(h, globalAdmin, http.MethodGet, "/api/v1/settings/oidc", "")
	if body := rec.Body.String(); !strings.Contains(body, `"client_secret_set":true`) || strings.Contains(body, "s3cr3t") {
		t.Fatalf("secret leaked or not flagged: %s", body)
	}
}

func TestOIDCSettingsSecretPreserveAndReloadError(t *testing.T) {
	fs := seededStore()
	ctrl := &fakeOIDC{active: true}
	h := newOIDCHandler(fs, ctrl)

	full := `{"enabled":true,"issuer":"https://idp/x","client_id":"cerbix","client_secret":"keepme","redirect_url":"https://c/auth/callback"}`
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/oidc", full); rec.Code != http.StatusOK {
		t.Fatalf("first save = %d", rec.Code)
	}
	// A save that omits client_secret preserves the stored one.
	noSecret := `{"enabled":true,"issuer":"https://idp/x","client_id":"cerbix","redirect_url":"https://c/auth/callback","button_label":"SSO"}`
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/oidc", noSecret); rec.Code != http.StatusOK {
		t.Fatalf("second save = %d", rec.Code)
	}
	if fs.oidcSettings.ClientSecret != "keepme" {
		t.Fatalf("secret not preserved on blank submit: %q", fs.oidcSettings.ClientSecret)
	}

	// A provider build failure is reported but the save still succeeds (200 + reload_error).
	ctrl.syncErr = errors.New("oidc discovery: connection refused")
	rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/oidc", full)
	if rec.Code != http.StatusOK {
		t.Fatalf("save with unreachable idp = %d, want 200", rec.Code)
	}
	var v struct {
		Active      bool   `json:"active"`
		ReloadError string `json:"reload_error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Active || v.ReloadError == "" {
		t.Fatalf("expected inactive + reload_error, got %+v", v)
	}
}
