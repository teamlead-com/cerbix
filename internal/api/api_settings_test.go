package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/settings"
)

// newSettingsRouters builds one settings-backed handler and returns both routers so
// the public branding view shares the same live snapshot as the admin writes.
func newSettingsRouters(fs *fakeStore) (admin, public http.Handler) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := settings.New(fs, settings.Bootstrap{MinPasswordLen: 8, SessionTTLSeconds: 3600}, logger)
	h := api.New(fs, logger, 8).WithSettings(svc)
	return h.Router(), h.PublicRouter()
}

func newSettingsHandler(fs *fakeStore) http.Handler {
	admin, _ := newSettingsRouters(fs)
	return admin
}

func TestBrandingSettings(t *testing.T) {
	fs := seededStore()
	h, pub := newSettingsRouters(fs)

	// Non-global-admin is forbidden.
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/settings/branding", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("org-admin branding = %d, want 403", rec.Code)
	}
	// Global admin sees the default.
	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/settings/branding", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"product_name":"cerbix"`) {
		t.Fatalf("default branding = %d %s", rec.Code, rec.Body.String())
	}
	// Invalid accent → 400.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/branding", `{"accent_color":"blue"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad accent = %d, want 400", rec.Code)
	}
	// Valid save persists and reads back.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/branding", `{"product_name":"Example Status","accent_color":"#3b5bdb","announcement":{"enabled":true,"text":"Migration tonight","level":"warning"}}`); rec.Code != http.StatusOK {
		t.Fatalf("save branding = %d", rec.Code)
	}
	if fs.instanceSettings.Branding.ProductName != "Example Status" || !fs.instanceSettings.Branding.Configured {
		t.Fatalf("branding not persisted: %+v", fs.instanceSettings.Branding)
	}

	// Public branding is unauthenticated and reflects the saved values.
	prec := do(pub, o1Viewer, http.MethodGet, "/api/v1/public/branding", "")
	if prec.Code != http.StatusOK || !strings.Contains(prec.Body.String(), "Example Status") || !strings.Contains(prec.Body.String(), "Migration tonight") {
		t.Fatalf("public branding = %d %s", prec.Code, prec.Body.String())
	}
}

func TestAuthPolicyAndAlertingAndDefaults(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)

	// Auth policy: too-short min password rejected; valid saved.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/auth-policy", `{"min_password_len":3,"session_ttl_seconds":3600}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad auth policy = %d, want 400", rec.Code)
	}
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/auth-policy", `{"min_password_len":16,"session_ttl_seconds":1800,"require_totp":"admins","allowed_email_domains":["Example.com"]}`); rec.Code != http.StatusOK {
		t.Fatalf("save auth policy = %d", rec.Code)
	}
	if fs.instanceSettings.AuthPolicy.MinPasswordLen != 16 || fs.instanceSettings.AuthPolicy.AllowedEmailDomains[0] != "example.com" {
		t.Fatalf("auth policy not normalized/persisted: %+v", fs.instanceSettings.AuthPolicy)
	}

	// Alerting: enable global silence.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/alerting", `{"global_silence":{"enabled":true}}`); rec.Code != http.StatusOK {
		t.Fatalf("save alerting = %d", rec.Code)
	}
	if !fs.instanceSettings.Alerting.GlobalSilence.Enabled {
		t.Fatal("silence not persisted")
	}

	// Monitor defaults: below-minimum interval rejected; valid saved and applied on create.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/monitor-defaults", `{"interval_seconds":1,"timeout_seconds":10,"failure_threshold":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad defaults = %d, want 400", rec.Code)
	}
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/monitor-defaults", `{"interval_seconds":120,"timeout_seconds":7,"retries":2,"failure_threshold":3,"renotify_seconds":600,"auto_incident":false}`); rec.Code != http.StatusOK {
		t.Fatalf("save defaults = %d", rec.Code)
	}
	// Creating a monitor with omitted fields inherits the defaults.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", `{"name":"d","type":"http","target":"https://x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with defaults = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"interval_seconds":120`) || !strings.Contains(body, `"failure_threshold":3`) || !strings.Contains(body, `"auto_incident":false`) {
		t.Fatalf("defaults not applied on create: %s", body)
	}
}

func TestMailSettings(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)

	// Non-global-admin forbidden.
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/settings/mail", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("org-admin mail = %d, want 403", rec.Code)
	}
	// Default: not deliverable.
	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/settings/mail", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deliverable":false`) {
		t.Fatalf("default mail = %d %s", rec.Code, rec.Body.String())
	}
	// Enabling without host → 400.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/mail", `{"enabled":true,"from":"a@x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without host = %d, want 400", rec.Code)
	}
	// Valid save with password → 200, persisted, deliverable.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/mail", `{"enabled":true,"smtp_host":"smtp.example","smtp_port":587,"smtp_username":"u","smtp_password":"s3cr3t","from":"status@example","public_base_url":"https://c"}`); rec.Code != http.StatusOK {
		t.Fatalf("save mail = %d", rec.Code)
	}
	if fs.instanceSettings.Mail.SMTPPassword != "s3cr3t" || !fs.instanceSettings.Mail.Configured {
		t.Fatalf("mail not persisted: %+v", fs.instanceSettings.Mail)
	}
	// Read back: password redacted, only smtp_password_set true.
	rec = do(h, globalAdmin, http.MethodGet, "/api/v1/settings/mail", "")
	if b := rec.Body.String(); !strings.Contains(b, `"smtp_password_set":true`) || strings.Contains(b, "s3cr3t") || !strings.Contains(b, `"deliverable":true`) {
		t.Fatalf("mail read leaked/missing flags: %s", b)
	}
	// A save omitting the password preserves the stored one.
	if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/mail", `{"enabled":true,"smtp_host":"smtp2.example","from":"status@example"}`); rec.Code != http.StatusOK {
		t.Fatalf("resave mail = %d", rec.Code)
	}
	if fs.instanceSettings.Mail.SMTPPassword != "s3cr3t" || fs.instanceSettings.Mail.SMTPHost != "smtp2.example" {
		t.Fatalf("password not preserved / host not updated: %+v", fs.instanceSettings.Mail)
	}
}
