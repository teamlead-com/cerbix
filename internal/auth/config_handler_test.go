package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfigHandler(t *testing.T) {
	type resp struct {
		Local           bool   `json:"local"`
		Oidc            bool   `json:"oidc"`
		OidcButtonLabel string `json:"oidc_button_label"`
	}
	call := func(a *Authenticator) resp {
		rec := httptest.NewRecorder()
		a.ConfigHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		var r resp
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return r
	}

	// Local enabled, OIDC inactive (nil runtime) → oidc false, default label.
	got := call(&Authenticator{local: true})
	if !got.Local || got.Oidc || got.OidcButtonLabel != "Continue with SSO" {
		t.Fatalf("got %+v", got)
	}

	// OIDC active with a custom label → oidc true, label echoed.
	active := &Authenticator{}
	active.oidc.Store(&oidcRuntime{buttonLabel: "Continue with Okta"})
	if got := call(active); !got.Oidc || got.OidcButtonLabel != "Continue with Okta" {
		t.Fatalf("active got %+v", got)
	}

	// OIDC active with an empty label falls back to a generic default.
	blank := &Authenticator{}
	blank.oidc.Store(&oidcRuntime{})
	if got := call(blank); got.OidcButtonLabel != "Continue with SSO" {
		t.Fatalf("default label = %q, want %q", got.OidcButtonLabel, "Continue with SSO")
	}
}
