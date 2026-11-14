package api_test

// S10 (sec-hardening-2 H-10): no support_url reaches a client unless it is http(s) with a host or
// mailto: with an address. Refused on write; a stored value that fails the rule — saved before the
// rule existed — is served as "" by the public branding read.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/settings"
)

var badSupportURLs = []string{
	"javascript:alert(1)",
	"JavaScript:alert(1)",
	" javascript:alert(1)",
	"data:text/html,<script>alert(1)</script>",
	"vbscript:x",
	"//evil.example/help",
	"/help",
	"https://",
	"http:///path",
	"https:evil.example",
	"mailto:",
	"mailto:?subject=x",
	"ftp://files.example",
	"https://help.example/\x00",
}

var goodSupportURLs = []string{
	"",
	"https://help.example.com/support",
	"http://intranet.example/help",
	"mailto:support@example.com",
	"mailto:support@example.com?subject=cerbix",
}

func TestSupportURLIsRefusedOnWriteUnlessHTTPOrMailto(t *testing.T) {
	fs := seededStore()
	h, _ := newSettingsRouters(fs)
	for _, u := range badSupportURLs {
		body, _ := json.Marshal(map[string]string{"support_url": u})
		if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/branding", string(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("support_url %q saved: %d, want 400", u, rec.Code)
		}
	}
	for _, u := range goodSupportURLs {
		body, _ := json.Marshal(map[string]string{"support_url": u})
		if rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/branding", string(body)); rec.Code != http.StatusOK {
			t.Errorf("support_url %q refused: %d %s", u, rec.Code, rec.Body.String())
		}
	}
}

func TestAStoredUnsafeSupportURLIsServedEmpty(t *testing.T) {
	for _, u := range append([]string{"javascript:alert(document.cookie)"}, badSupportURLs...) {
		fs := seededStore()
		fs.instanceSettings.Branding = domain.Branding{Configured: true, ProductName: "Seeded", SupportURL: u}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		svc := settings.New(fs, settings.Bootstrap{MinPasswordLen: 8, SessionTTLSeconds: 3600}, logger)
		if err := svc.Load(context.Background()); err != nil {
			t.Fatalf("load: %v", err)
		}
		pub := api.New(fs, logger, 8).WithSettings(svc).PublicRouter()
		rec := do(pub, o1Viewer, http.MethodGet, "/api/v1/public/branding", "")
		var out struct {
			ProductName string `json:"product_name"`
			SupportURL  string `json:"support_url"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("public branding = %d %s", rec.Code, rec.Body.String())
		}
		if out.ProductName != "Seeded" {
			t.Fatalf("the seeded row was not served: %+v", out)
		}
		if out.SupportURL != "" {
			t.Errorf("stored support_url %q served as %q, want \"\"", u, out.SupportURL)
		}
	}
	// A valid stored value is served unchanged.
	fs := seededStore()
	fs.instanceSettings.Branding = domain.Branding{Configured: true, SupportURL: "mailto:help@example.com"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := settings.New(fs, settings.Bootstrap{MinPasswordLen: 8, SessionTTLSeconds: 3600}, logger)
	_ = svc.Load(context.Background())
	rec := do(api.New(fs, logger, 8).WithSettings(svc).PublicRouter(), o1Viewer, http.MethodGet, "/api/v1/public/branding", "")
	var out struct {
		SupportURL string `json:"support_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.SupportURL != "mailto:help@example.com" {
		t.Errorf("a valid stored support_url served as %q", out.SupportURL)
	}
}

// Review of iter-0212, P2-3: putBranding trims support_url before it checks it, as setOIDCSettings
// trims post_logout_url — a value pasted with a stray space is saved clean, not refused.
func TestSupportURLIsTrimmedBeforeItIsChecked(t *testing.T) {
	fs := seededStore()
	h, _ := newSettingsRouters(fs)
	rec := do(h, globalAdmin, http.MethodPut, "/api/v1/settings/branding", `{"support_url":"  https://help.example.com/support \n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a support_url with edge whitespace = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := fs.instanceSettings.Branding.SupportURL; got != "https://help.example.com/support" {
		t.Errorf("stored support_url %q, want it trimmed", got)
	}
}
