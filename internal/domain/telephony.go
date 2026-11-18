package domain

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Telephony settings (FR-041a §4.1). The group lives in the database only (D36): there is no YAML
// seed, and an absent group means telephony is disabled. The two secret fields — Twilio's auth token
// and the generic_http headers map — are never serialized from this type (`json:"-"`); the store
// keeps them encrypted at rest (D29) and the API reports only whether each is set.

// Telephony providers (D3).
const (
	TelephonyProviderTwilio      = "twilio"
	TelephonyProviderGenericHTTP = "generic_http"
)

// SMS encodings (D25).
const (
	TelephonySMSTransliterate = "transliterate"
	TelephonySMSUnicode       = "unicode"
)

// Defaults of §4.1 and §6.2.
const (
	defaultTelephonyVoiceLanguage = "en-US"
	defaultTelephonySMSSegments   = 2
	defaultStormPerIncidentHour   = 5
	defaultStormPerNumberHour     = 20
)

// TelephonyTwilio is the twilio block. From is an E.164 number or a Messaging Service SID (R14).
type TelephonyTwilio struct {
	AccountSID string `json:"account_sid"`
	AuthToken  string `json:"-"` // secret
	From       string `json:"from"`
}

// TelephonyGenericHTTP is the operator-defined SMS gateway request (D3).
type TelephonyGenericHTTP struct {
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"-"` // secret: the whole map (§4.1)
	ContentType       string            `json:"content_type"`
	BodyTemplate      string            `json:"body_template"`
	From              string            `json:"from"`
	ResponseCodeField string            `json:"response_code_field"`
}

// TelephonyStorm is the storm guard's per-hour bounds (§6.2).
type TelephonyStorm struct {
	PerIncidentPerHour int `json:"per_incident_per_hour"`
	PerNumberPerHour   int `json:"per_number_per_hour"`
}

// TelephonySettings is the instance's telephony group.
type TelephonySettings struct {
	Configured     bool                 `json:"configured"`
	Enabled        bool                 `json:"enabled"`
	Provider       string               `json:"provider"`
	Twilio         TelephonyTwilio      `json:"twilio"`
	GenericHTTP    TelephonyGenericHTTP `json:"generic_http"`
	PublicBaseURL  string               `json:"public_base_url"`
	VoiceLanguage  string               `json:"voice_language"`
	SMSEncoding    string               `json:"sms_encoding"`
	SMSMaxSegments int                  `json:"sms_max_segments"`
	Storm          TelephonyStorm       `json:"storm"`

	// AuthTokenUnreadable and HeadersUnreadable are set by the store, each for its own secret, when a
	// stored ciphertext cannot be opened with the configured keys (a key removed before `cerbix
	// reencrypt`). That secret is then empty here but still stored: a blank write keeps it, and only the
	// provider that needs it refuses to send. The rest of the instance settings still load.
	AuthTokenUnreadable bool `json:"-"`
	HeadersUnreadable   bool `json:"-"`
}

// AuthTokenStored reports whether a Twilio auth token is stored, readable or not.
func (t TelephonySettings) AuthTokenStored() bool {
	return t.Twilio.AuthToken != "" || t.AuthTokenUnreadable
}

// HeadersStored reports whether generic_http headers are stored, readable or not.
func (t TelephonySettings) HeadersStored() bool {
	return len(t.GenericHTTP.Headers) > 0 || t.HeadersUnreadable
}

// TelephonySaveOptions are the parts of a telephony write that are not settings.
type TelephonySaveOptions struct {
	// HeadersClear removes the stored generic_http headers. Refused with new headers in the same write.
	HeadersClear bool
	// ConfirmUnroutable saves even when the write leaves a paging service without a route (§4.1).
	ConfirmUnroutable bool
}

var (
	// ErrInvalidTelephonySettings is the class of every validation refusal of the group. Messages
	// name the field, never a value.
	ErrInvalidTelephonySettings = errors.New("invalid telephony settings")
	// ErrEncryptionKeyMissing: telephony cannot be saved without the at-rest key (D29).
	ErrEncryptionKeyMissing = errors.New("telephony settings need security.encryption_key to be configured")
	// ErrTelephonyHeadersConflict: headers_clear together with new headers (§4.1).
	ErrTelephonyHeadersConflict = errors.New("generic_http.headers_clear cannot be sent together with new headers")
)

// ServiceWithoutRoute names one service a telephony write would leave with no route (R10).
type ServiceWithoutRoute struct {
	Project string `json:"project"`
	Service string `json:"service"`
	Slug    string `json:"slug"`
}

// ServicesWithoutRouteError is the 409 answer of a telephony write (§4.1, R10): the services, at most
// 50 ordered by project name then slug, and how many there are in all. Nothing was written.
type ServicesWithoutRouteError struct {
	Services []ServiceWithoutRoute
	Total    int
}

func (e *ServicesWithoutRouteError) Error() string {
	return fmt.Sprintf("this change leaves %d paging service(s) without a route; resend with confirm_unroutable to save it", e.Total)
}

// DefaultTelephonySettings is what an absent group resolves to: disabled, with §4.1's defaults.
func DefaultTelephonySettings() TelephonySettings {
	var ts TelephonySettings
	ts.Normalize()
	return ts
}

// Normalize trims the free-text fields, upper-cases the method and fills every unset field that has
// a default. A zero sms_max_segments or storm bound means "unset" here; Validate refuses negatives.
func (t *TelephonySettings) Normalize() {
	t.Provider = strings.TrimSpace(t.Provider)
	t.Twilio.AccountSID = strings.TrimSpace(t.Twilio.AccountSID)
	t.Twilio.AuthToken = strings.TrimSpace(t.Twilio.AuthToken)
	t.Twilio.From = strings.TrimSpace(t.Twilio.From)
	g := &t.GenericHTTP
	g.Method = strings.ToUpper(strings.TrimSpace(g.Method))
	g.URL = strings.TrimSpace(g.URL)
	g.ContentType = strings.ToLower(strings.TrimSpace(g.ContentType))
	g.From = strings.TrimSpace(g.From)
	g.ResponseCodeField = strings.TrimSpace(g.ResponseCodeField)
	t.PublicBaseURL = strings.TrimSpace(t.PublicBaseURL)
	t.VoiceLanguage = strings.TrimSpace(t.VoiceLanguage)
	t.SMSEncoding = strings.TrimSpace(t.SMSEncoding)
	if t.VoiceLanguage == "" {
		t.VoiceLanguage = defaultTelephonyVoiceLanguage
	}
	if t.SMSEncoding == "" {
		t.SMSEncoding = TelephonySMSTransliterate
	}
	if t.SMSMaxSegments == 0 {
		t.SMSMaxSegments = defaultTelephonySMSSegments
	}
	if t.Storm.PerIncidentPerHour == 0 {
		t.Storm.PerIncidentPerHour = defaultStormPerIncidentHour
	}
	if t.Storm.PerNumberPerHour == 0 {
		t.Storm.PerNumberPerHour = defaultStormPerNumberHour
	}
}

var (
	voiceLanguageRe     = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	twilioAccountSIDRe  = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	twilioServiceSIDRe  = regexp.MustCompile(`^MG[0-9a-fA-F]{32}$`)
	responseCodeFieldRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

func telephonyInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidTelephonySettings}, args...)...)
}

// Validate enforces the group's invariants on a normalized value. Formats are checked for every
// non-empty field; the provider's required fields only while telephony is enabled. Two checks live
// elsewhere: the body template's (telephony.ValidateSettings — the template grammar belongs to the
// provider package) and the credential's presence (ValidateCredentials, judged after the stored
// secret is merged in).
func (t TelephonySettings) Validate() error {
	switch t.Provider {
	case "", TelephonyProviderTwilio, TelephonyProviderGenericHTTP:
	default:
		return telephonyInvalid("provider must be twilio or generic_http")
	}
	if t.Enabled && t.Provider == "" {
		return telephonyInvalid("a provider is required when telephony is enabled")
	}
	switch t.SMSEncoding {
	case TelephonySMSTransliterate, TelephonySMSUnicode:
	default:
		return telephonyInvalid("sms_encoding must be transliterate or unicode")
	}
	if t.SMSMaxSegments < 1 || t.SMSMaxSegments > 4 {
		return telephonyInvalid("sms_max_segments must be 1..4")
	}
	if t.Storm.PerIncidentPerHour < 1 || t.Storm.PerNumberPerHour < 1 {
		return telephonyInvalid("storm bounds must be at least 1")
	}
	if !voiceLanguageRe.MatchString(t.VoiceLanguage) {
		return telephonyInvalid("voice_language must be a language tag such as en-US")
	}
	if t.PublicBaseURL != "" && !publicBaseURLAllowed(t.PublicBaseURL) {
		return telephonyInvalid("public_base_url must be an https URL (http only for a loopback host)")
	}
	if err := t.validateTwilio(); err != nil {
		return err
	}
	return t.validateGenericHTTP()
}

func (t TelephonySettings) validateTwilio() error {
	tw := t.Twilio
	if tw.AccountSID != "" && !twilioAccountSIDRe.MatchString(tw.AccountSID) {
		return telephonyInvalid("twilio.account_sid must be an account SID (AC followed by 32 hex digits)")
	}
	if tw.From != "" && !e164Re.MatchString(tw.From) && !twilioServiceSIDRe.MatchString(tw.From) {
		return telephonyInvalid("twilio.from must be an E.164 number or a Messaging Service SID")
	}
	if t.Enabled && t.Provider == TelephonyProviderTwilio && (tw.AccountSID == "" || tw.From == "") {
		return telephonyInvalid("twilio.account_sid and twilio.from are required when telephony is enabled")
	}
	return nil
}

func (t TelephonySettings) validateGenericHTTP() error {
	g := t.GenericHTTP
	if g.Method != "" && g.Method != "POST" && g.Method != "PUT" {
		return telephonyInvalid("generic_http.method must be POST or PUT")
	}
	if g.URL != "" && !webhookURLAllowed(g.URL) {
		return telephonyInvalid("generic_http.url must be an http(s) URL with a host")
	}
	if g.ContentType != "" && g.ContentType != "application/json" && g.ContentType != "application/x-www-form-urlencoded" {
		return telephonyInvalid("generic_http.content_type must be application/json or application/x-www-form-urlencoded")
	}
	if g.ResponseCodeField != "" && !responseCodeFieldRe.MatchString(g.ResponseCodeField) {
		return telephonyInvalid("generic_http.response_code_field must be a plain JSON key")
	}
	if len(g.From) > 64 || !cleanEdgesAndBytes(g.From) {
		return telephonyInvalid("generic_http.from must be at most 64 characters without control bytes")
	}
	if t.Enabled && t.Provider == TelephonyProviderGenericHTTP &&
		(g.Method == "" || g.URL == "" || g.ContentType == "" || strings.TrimSpace(g.BodyTemplate) == "") {
		return telephonyInvalid("generic_http.method, url, content_type and body_template are required when telephony is enabled")
	}
	return nil
}

// ValidateCredentials asks whether the provider in force has its secret: Twilio cannot send without
// an auth token. authTokenPresent is what the write will leave stored — a new token, or the kept one.
func (t TelephonySettings) ValidateCredentials(authTokenPresent bool) error {
	if t.Enabled && t.Provider == TelephonyProviderTwilio && !authTokenPresent {
		return telephonyInvalid("twilio.auth_token is required when telephony is enabled")
	}
	return nil
}

// webhookURLAllowed is the rule a webhook channel's URL follows, tightened the way every stored URL
// is (safeurl.go): http(s), a host name, no opaque part, clean edges and no control byte.
func webhookURLAllowed(s string) bool {
	if !cleanEdgesAndBytes(s) || strings.ContainsAny(s, " \t") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && httpWithHostName(u)
}

// publicBaseURLAllowed: the webhook rule, and https unless the host is loopback (§4.1). It is also a
// base the renderer appends `/i/<ref>` to, so it carries no Unicode space or control (the renderer
// refuses those at send, telephony/message.go), no query or fragment (`?` and `#` anywhere — the link
// would point elsewhere) and no userinfo.
func publicBaseURLAllowed(s string) bool {
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 ||
		strings.ContainsAny(s, "?#") || !webhookURLAllowed(s) {
		return false
	}
	u, _ := url.Parse(s)
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
