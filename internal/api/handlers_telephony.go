package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// Telephony settings (FR-041a §4.1). Global admin only. The two secrets — Twilio's auth token and the
// generic_http headers map — are written here and never read back: the view says only whether each
// is stored. A blank or omitted secret keeps what is stored; generic_http.headers_clear removes the
// headers.

// Machine reasons of the telephony settings refusals (R7).
const (
	reasonEncryptionKeyMissing = "encryption_key_missing"
	reasonHeadersConflict      = "headers_conflict"
	reasonTemplateInvalid      = "template_invalid"
	reasonServicesWithoutRoute = "services_without_route"
)

// setTelephonySettingsRequest is the PUT body (openapi SetTelephonySettings). A whole-group write:
// every non-secret field is what the request says.
type setTelephonySettingsRequest struct {
	Enabled           *bool                           `json:"enabled" contract:"required"`
	Provider          string                          `json:"provider"`
	Twilio            *setTelephonyTwilioRequest      `json:"twilio"`
	GenericHTTP       *setTelephonyGenericHTTPRequest `json:"generic_http"`
	PublicBaseURL     string                          `json:"public_base_url"`
	VoiceLanguage     string                          `json:"voice_language"`
	SMSEncoding       string                          `json:"sms_encoding"`
	SMSMaxSegments    int                             `json:"sms_max_segments"`
	Storm             *telephonyStormBody             `json:"storm"`
	ConfirmUnroutable bool                            `json:"confirm_unroutable"`
}

// setTelephonyTwilioRequest is the twilio block (openapi SetTelephonyTwilio).
type setTelephonyTwilioRequest struct {
	AccountSID string `json:"account_sid"`
	AuthToken  string `json:"auth_token"` // blank or omitted keeps the stored token
	From       string `json:"from"`
}

// setTelephonyGenericHTTPRequest is the generic_http block (openapi SetTelephonyGenericHTTP).
type setTelephonyGenericHTTPRequest struct {
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers"`       // empty or omitted keeps the stored map
	HeadersClear      bool              `json:"headers_clear"` // removes the stored map
	ContentType       string            `json:"content_type"`
	BodyTemplate      string            `json:"body_template"`
	From              string            `json:"from"`
	ResponseCodeField string            `json:"response_code_field"`
}

// telephonyStormBody is the storm guard's bounds, in the request and in the view (openapi
// TelephonyStorm).
type telephonyStormBody struct {
	PerIncidentPerHour int `json:"per_incident_per_hour"`
	PerNumberPerHour   int `json:"per_number_per_hour"`
}

// *_set says a secret is stored; *_unreadable that it is stored but the configured keys cannot open it
// (re-enter it, or restore the key and run `cerbix reencrypt`).
type telephonyTwilioView struct {
	AccountSID          string `json:"account_sid"`
	AuthTokenSet        bool   `json:"auth_token_set"`
	AuthTokenUnreadable bool   `json:"auth_token_unreadable"`
	From                string `json:"from"`
}

type telephonyGenericHTTPView struct {
	Method            string `json:"method"`
	URL               string `json:"url"`
	HeadersSet        bool   `json:"headers_set"`
	HeadersUnreadable bool   `json:"headers_unreadable"`
	ContentType       string `json:"content_type"`
	BodyTemplate      string `json:"body_template"`
	From              string `json:"from"`
	ResponseCodeField string `json:"response_code_field"`
}

// telephonySettingsView is the read model (openapi TelephonySettingsView): never a secret.
type telephonySettingsView struct {
	Configured              bool                     `json:"configured"`
	EncryptionKeyConfigured bool                     `json:"encryption_key_configured"`
	Enabled                 bool                     `json:"enabled"`
	Provider                string                   `json:"provider"`
	Twilio                  telephonyTwilioView      `json:"twilio"`
	GenericHTTP             telephonyGenericHTTPView `json:"generic_http"`
	PublicBaseURL           string                   `json:"public_base_url"`
	VoiceLanguage           string                   `json:"voice_language"`
	SMSEncoding             string                   `json:"sms_encoding"`
	SMSMaxSegments          int                      `json:"sms_max_segments"`
	Storm                   telephonyStormBody       `json:"storm"`
}

func telephonyView(ts domain.TelephonySettings, keyConfigured bool) telephonySettingsView {
	g := ts.GenericHTTP
	return telephonySettingsView{
		Configured: ts.Configured, EncryptionKeyConfigured: keyConfigured, Enabled: ts.Enabled, Provider: ts.Provider,
		Twilio: telephonyTwilioView{
			AccountSID: ts.Twilio.AccountSID, AuthTokenSet: ts.AuthTokenStored(),
			AuthTokenUnreadable: ts.AuthTokenUnreadable, From: ts.Twilio.From,
		},
		GenericHTTP: telephonyGenericHTTPView{
			Method: g.Method, URL: g.URL, HeadersSet: ts.HeadersStored(), HeadersUnreadable: ts.HeadersUnreadable,
			ContentType:  g.ContentType,
			BodyTemplate: g.BodyTemplate, From: g.From, ResponseCodeField: g.ResponseCodeField,
		},
		PublicBaseURL: ts.PublicBaseURL, VoiceLanguage: ts.VoiceLanguage, SMSEncoding: ts.SMSEncoding,
		SMSMaxSegments: ts.SMSMaxSegments,
		Storm: telephonyStormBody{
			PerIncidentPerHour: ts.Storm.PerIncidentPerHour, PerNumberPerHour: ts.Storm.PerNumberPerHour,
		},
	}
}

func (r setTelephonySettingsRequest) settings() (domain.TelephonySettings, domain.TelephonySaveOptions) {
	ts := domain.TelephonySettings{
		Enabled: *r.Enabled, Provider: r.Provider, PublicBaseURL: r.PublicBaseURL,
		VoiceLanguage: r.VoiceLanguage, SMSEncoding: r.SMSEncoding, SMSMaxSegments: r.SMSMaxSegments,
	}
	opts := domain.TelephonySaveOptions{ConfirmUnroutable: r.ConfirmUnroutable}
	if r.Twilio != nil {
		ts.Twilio = domain.TelephonyTwilio{AccountSID: r.Twilio.AccountSID, AuthToken: r.Twilio.AuthToken, From: r.Twilio.From}
	}
	if g := r.GenericHTTP; g != nil {
		ts.GenericHTTP = domain.TelephonyGenericHTTP{
			Method: g.Method, URL: g.URL, Headers: g.Headers, ContentType: g.ContentType,
			BodyTemplate: g.BodyTemplate, From: g.From, ResponseCodeField: g.ResponseCodeField,
		}
		opts.HeadersClear = g.HeadersClear
	}
	if r.Storm != nil {
		ts.Storm = domain.TelephonyStorm{PerIncidentPerHour: r.Storm.PerIncidentPerHour, PerNumberPerHour: r.Storm.PerNumberPerHour}
	}
	return ts, opts
}

func (h *Handler) getTelephonySettings(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, telephonyView(h.settings.Telephony(), h.settings.EncryptionKeyConfigured()))
}

func (h *Handler) putTelephonySettings(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) || !h.settingsReady(w) {
		return
	}
	var body setTelephonySettingsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	// D29: nothing is judged before the refusal the admin cannot fix in this form.
	if !h.settings.EncryptionKeyConfigured() {
		writeErrorReason(w, http.StatusConflict, reasonEncryptionKeyMissing, domain.ErrEncryptionKeyMissing.Error())
		return
	}
	ts, opts := body.settings()
	if opts.HeadersClear && len(ts.GenericHTTP.Headers) > 0 {
		writeErrorReason(w, http.StatusBadRequest, reasonHeadersConflict, domain.ErrTelephonyHeadersConflict.Error())
		return
	}
	err := h.settings.SaveTelephony(r.Context(), ts, opts)
	var unroutable *domain.ServicesWithoutRouteError
	switch {
	case err == nil:
	case errors.As(err, &unroutable):
		services := unroutable.Services
		if services == nil {
			services = []domain.ServiceWithoutRoute{}
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":                        unroutable.Error(),
			"reason":                       reasonServicesWithoutRoute,
			"services_without_route":       services,
			"services_without_route_total": unroutable.Total,
		})
		return
	case errors.Is(err, domain.ErrEncryptionKeyMissing):
		writeErrorReason(w, http.StatusConflict, reasonEncryptionKeyMissing, err.Error())
		return
	case errors.Is(err, domain.ErrTelephonyHeadersConflict):
		writeErrorReason(w, http.StatusBadRequest, reasonHeadersConflict, err.Error())
		return
	case errors.Is(err, telephony.ErrTemplate):
		writeErrorReason(w, http.StatusBadRequest, reasonTemplateInvalid, err.Error())
		return
	case errors.Is(err, domain.ErrInvalidTelephonySettings):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	default:
		h.serverError(w, "save_telephony", err)
		return
	}
	h.audit(r, "", "settings.telephony.update", "telephony")
	h.logEvent(r, "settings_saved", "group", "telephony")
	writeJSON(w, http.StatusOK, telephonyView(h.settings.Telephony(), h.settings.EncryptionKeyConfigured()))
}
