package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// oidcSettingsView is the redacted read model for the settings API. The client
// secret is never emitted; only whether one is stored.
type oidcSettingsView struct {
	Configured      bool     `json:"configured"` // a DB override row exists (config file no longer in effect)
	Active          bool     `json:"active"`     // the provider is currently built and serving
	Enabled         bool     `json:"enabled"`
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	ClientSecretSet bool     `json:"client_secret_set"`
	RedirectURL     string   `json:"redirect_url"`
	Scopes          []string `json:"scopes"`
	PostLogoutURL   string   `json:"post_logout_url"`
	ButtonLabel     string   `json:"button_label"`
	BootstrapAdmins []string `json:"bootstrap_admins"`
	ReloadError     string   `json:"reload_error,omitempty"` // set when a save could not build the provider (e.g. IdP unreachable)
}

// getOIDCSettings returns the instance-wide OIDC override (global-admin only), with
// the client secret redacted. When no override is saved, Configured is false and the
// config-file bootstrap (if any) is what's in effect.
func (h *Handler) getOIDCSettings(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	view := oidcSettingsView{Active: h.oidc != nil && h.oidc.OIDCActive(), Scopes: []string{}, BootstrapAdmins: []string{}}
	s, err := h.store.GetOIDCSettings(r.Context())
	if err == nil {
		view.Configured = true
		view.Enabled = s.Enabled
		view.Issuer = s.Issuer
		view.ClientID = s.ClientID
		view.ClientSecretSet = s.ClientSecret != ""
		view.RedirectURL = s.RedirectURL
		view.Scopes = nonNilStrings(s.Scopes)
		view.PostLogoutURL = s.PostLogoutURL
		view.ButtonLabel = s.ButtonLabel
		view.BootstrapAdmins = nonNilStrings(s.BootstrapAdmins)
	} else if !errors.Is(err, store.ErrNotFound) {
		h.serverError(w, "get_oidc_settings", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// setOIDCSettings persists the OIDC override and rebuilds the live provider
// (global-admin only). Once saved, the DB override is authoritative and the YAML
// oidc: block is ignored. A blank client secret preserves the stored one. If the
// provider can't be built (e.g. the IdP is unreachable), the settings are still
// saved and Active is reported false — the background reloader keeps retrying.
func (h *Handler) setOIDCSettings(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	if h.oidc == nil {
		writeError(w, http.StatusNotImplemented, "oidc is not configurable in this deployment")
		return
	}
	var body struct {
		Enabled         bool     `json:"enabled"`
		Issuer          string   `json:"issuer"`
		ClientID        string   `json:"client_id"`
		ClientSecret    *string  `json:"client_secret"` // pointer: omitted/null → keep stored secret
		RedirectURL     string   `json:"redirect_url"`
		Scopes          []string `json:"scopes"`
		PostLogoutURL   string   `json:"post_logout_url"`
		ButtonLabel     string   `json:"button_label"`
		BootstrapAdmins []string `json:"bootstrap_admins"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Issuer = strings.TrimSpace(body.Issuer)
	body.ClientID = strings.TrimSpace(body.ClientID)
	body.RedirectURL = strings.TrimSpace(body.RedirectURL)
	if body.Enabled {
		if body.Issuer == "" || body.ClientID == "" || body.RedirectURL == "" {
			writeError(w, http.StatusBadRequest, "issuer, client_id and redirect_url are required when SSO is enabled")
			return
		}
	}

	// Preserve the stored secret when the client sends an empty/omitted one.
	secret := ""
	if body.ClientSecret != nil {
		secret = *body.ClientSecret
	}
	if strings.TrimSpace(secret) == "" {
		if cur, err := h.store.GetOIDCSettings(r.Context()); err == nil {
			secret = cur.ClientSecret
		} else if !errors.Is(err, store.ErrNotFound) {
			h.serverError(w, "get_oidc_settings", err)
			return
		}
	}

	settings := domain.OIDCSettings{
		Enabled:         body.Enabled,
		Issuer:          body.Issuer,
		ClientID:        body.ClientID,
		ClientSecret:    secret,
		RedirectURL:     body.RedirectURL,
		Scopes:          nonNilStrings(body.Scopes),
		PostLogoutURL:   strings.TrimSpace(body.PostLogoutURL),
		ButtonLabel:     strings.TrimSpace(body.ButtonLabel),
		BootstrapAdmins: nonNilStrings(body.BootstrapAdmins),
	}
	if err := h.store.UpsertOIDCSettings(r.Context(), settings); err != nil {
		h.serverError(w, "upsert_oidc_settings", err)
		return
	}

	// Rebuild the live provider from the just-saved settings. A build failure is not
	// fatal to the save — report it so the admin knows SSO isn't active yet.
	view := oidcSettingsView{
		Configured: true, Enabled: settings.Enabled, Issuer: settings.Issuer, ClientID: settings.ClientID,
		ClientSecretSet: settings.ClientSecret != "", RedirectURL: settings.RedirectURL,
		Scopes: settings.Scopes, PostLogoutURL: settings.PostLogoutURL, ButtonLabel: settings.ButtonLabel,
		BootstrapAdmins: settings.BootstrapAdmins,
	}
	if err := h.oidc.SyncOIDC(r.Context()); err != nil {
		view.ReloadError = err.Error()
	}
	view.Active = h.oidc.OIDCActive()
	writeJSON(w, http.StatusOK, view)
}

// nonNilStrings guarantees a JSON array (not null) for optional string slices.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
