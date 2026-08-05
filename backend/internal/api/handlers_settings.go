package api

import (
	"net/http"
	"strings"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// settingsReady guards the instance-settings endpoints when the service isn't wired.
func (h *Handler) settingsReady(w http.ResponseWriter) bool {
	if h.settings == nil {
		writeError(w, http.StatusNotImplemented, "instance settings are not configurable in this deployment")
		return false
	}
	return true
}

// ── Branding (global-admin) ───────────────────────────────────────────────

func (h *Handler) getBranding(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, h.settings.Branding())
}

func (h *Handler) putBranding(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	var b domain.Branding
	if !decodeJSON(w, r, &b) {
		return
	}
	if err := b.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.settings.SaveBranding(r.Context(), b); err != nil {
		h.serverError(w, "save_branding", err)
		return
	}
	writeJSON(w, http.StatusOK, h.settings.Branding())
}

// publicBranding exposes branding to unauthenticated clients (login page, public
// status pages). No secrets live in branding; the configured flag is omitted.
func (h *Handler) publicBranding(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		writeJSON(w, http.StatusOK, map[string]any{"product_name": "cerbix"})
		return
	}
	b := h.settings.Branding()
	writeJSON(w, http.StatusOK, map[string]any{
		"product_name": b.ProductName,
		"accent_color": b.AccentColor,
		"logo_url":     b.LogoURL,
		"footer_text":  b.FooterText,
		"support_url":  b.SupportURL,
		"announcement": b.Announcement,
	})
}

// ── Auth policy (global-admin) ────────────────────────────────────────────

func (h *Handler) getAuthPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, h.settings.AuthPolicy())
}

func (h *Handler) putAuthPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	var p domain.AuthPolicy
	if !decodeJSON(w, r, &p) {
		return
	}
	p.Normalize()
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.settings.SaveAuthPolicy(r.Context(), p); err != nil {
		h.serverError(w, "save_auth_policy", err)
		return
	}
	writeJSON(w, http.StatusOK, h.settings.AuthPolicy())
}

// ── Alerting (global-admin) ───────────────────────────────────────────────

func (h *Handler) getAlerting(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, h.settings.Alerting())
}

func (h *Handler) putAlerting(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	var a domain.Alerting
	if !decodeJSON(w, r, &a) {
		return
	}
	if err := a.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.settings.SaveAlerting(r.Context(), a); err != nil {
		h.serverError(w, "save_alerting", err)
		return
	}
	writeJSON(w, http.StatusOK, h.settings.Alerting())
}

// ── Monitor defaults (global-admin) ───────────────────────────────────────

func (h *Handler) getMonitorDefaults(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, h.settings.MonitorDefaults())
}

func (h *Handler) putMonitorDefaults(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	var d domain.MonitorDefaults
	if !decodeJSON(w, r, &d) {
		return
	}
	if err := d.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.settings.SaveMonitorDefaults(r.Context(), d); err != nil {
		h.serverError(w, "save_monitor_defaults", err)
		return
	}
	writeJSON(w, http.StatusOK, h.settings.MonitorDefaults())
}

// ── Mail / SMTP (global-admin) ────────────────────────────────────────────

// mailSettingsView is the redacted read model: the SMTP password is never emitted,
// only whether one is stored.
type mailSettingsView struct {
	Configured    bool   `json:"configured"`
	Enabled       bool   `json:"enabled"`
	SMTPHost      string `json:"smtp_host"`
	SMTPPort      int    `json:"smtp_port"`
	SMTPUsername  string `json:"smtp_username"`
	PasswordSet   bool   `json:"smtp_password_set"`
	From          string `json:"from"`
	PublicBaseURL string `json:"public_base_url"`
	Deliverable   bool   `json:"deliverable"`
}

func mailView(m domain.MailSettings) mailSettingsView {
	return mailSettingsView{
		Configured: m.Configured, Enabled: m.Enabled, SMTPHost: m.SMTPHost, SMTPPort: m.SMTPPort,
		SMTPUsername: m.SMTPUsername, PasswordSet: m.SMTPPassword != "", From: m.From,
		PublicBaseURL: m.PublicBaseURL, Deliverable: m.Deliverable(),
	}
}

func (h *Handler) getMail(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, mailView(h.settings.Mail()))
}

func (h *Handler) putMail(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	var body struct {
		Enabled       bool    `json:"enabled"`
		SMTPHost      string  `json:"smtp_host"`
		SMTPPort      int     `json:"smtp_port"`
		SMTPUsername  string  `json:"smtp_username"`
		SMTPPassword  *string `json:"smtp_password"` // omitted/null → keep stored password
		From          string  `json:"from"`
		PublicBaseURL string  `json:"public_base_url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// Preserve the stored password on a blank/omitted submit.
	password := ""
	if body.SMTPPassword != nil {
		password = *body.SMTPPassword
	}
	if strings.TrimSpace(password) == "" {
		password = h.settings.Mail().SMTPPassword
	}
	m := domain.MailSettings{
		Enabled: body.Enabled, SMTPHost: strings.TrimSpace(body.SMTPHost), SMTPPort: body.SMTPPort,
		SMTPUsername: body.SMTPUsername, SMTPPassword: password, From: strings.TrimSpace(body.From),
		PublicBaseURL: strings.TrimSpace(body.PublicBaseURL),
	}
	if err := m.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.settings.SaveMail(r.Context(), m); err != nil {
		h.serverError(w, "save_mail", err)
		return
	}
	writeJSON(w, http.StatusOK, mailView(h.settings.Mail()))
}
