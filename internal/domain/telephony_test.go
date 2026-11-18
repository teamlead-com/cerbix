package domain

import (
	"errors"
	"strings"
	"testing"
)

const (
	testAccountSID = "AC0123456789abcdef0123456789abcdef"
	testServiceSID = "MG0123456789abcdef0123456789abcdef"
)

func validTwilioTelephony() TelephonySettings {
	ts := TelephonySettings{
		Enabled: true, Provider: TelephonyProviderTwilio,
		Twilio: TelephonyTwilio{AccountSID: testAccountSID, AuthToken: "tok", From: "+15005550006"},
	}
	ts.Normalize()
	return ts
}

func validGenericTelephony() TelephonySettings {
	ts := TelephonySettings{
		Enabled: true, Provider: TelephonyProviderGenericHTTP,
		GenericHTTP: TelephonyGenericHTTP{
			Method: "POST", URL: "https://sms.example/send", ContentType: "application/json",
			BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`,
		},
	}
	ts.Normalize()
	return ts
}

func TestTelephonyDefaults(t *testing.T) {
	d := DefaultTelephonySettings()
	if d.Configured || d.Enabled || d.Provider != "" {
		t.Fatalf("defaults must be unconfigured and disabled: %+v", d)
	}
	if d.VoiceLanguage != "en-US" || d.SMSEncoding != TelephonySMSTransliterate || d.SMSMaxSegments != 2 ||
		d.Storm.PerIncidentPerHour != 5 || d.Storm.PerNumberPerHour != 20 {
		t.Fatalf("defaults = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("the defaults must validate: %v", err)
	}

	// Normalize fills only what is unset and trims.
	ts := TelephonySettings{Provider: " twilio ", VoiceLanguage: "", SMSMaxSegments: 3,
		GenericHTTP: TelephonyGenericHTTP{Method: "post", ContentType: " application/json "}}
	ts.Normalize()
	if ts.Provider != "twilio" || ts.VoiceLanguage != "en-US" || ts.SMSMaxSegments != 3 ||
		ts.SMSEncoding != TelephonySMSTransliterate || ts.Storm.PerIncidentPerHour != 5 ||
		ts.GenericHTTP.Method != "POST" || ts.GenericHTTP.ContentType != "application/json" {
		t.Fatalf("normalized = %+v", ts)
	}
}

func TestTelephonyValidate(t *testing.T) {
	if err := validTwilioTelephony().Validate(); err != nil {
		t.Fatalf("valid twilio: %v", err)
	}
	if err := validGenericTelephony().Validate(); err != nil {
		t.Fatalf("valid generic: %v", err)
	}
	// A Messaging Service SID is a valid twilio `from` (R14).
	ms := validTwilioTelephony()
	ms.Twilio.From = testServiceSID
	if err := ms.Validate(); err != nil {
		t.Fatalf("messaging service from: %v", err)
	}
	// Disabled: a half-filled provider block is allowed; formats still apply.
	half := TelephonySettings{Provider: TelephonyProviderTwilio}
	half.Normalize()
	if err := half.Validate(); err != nil {
		t.Fatalf("disabled half-filled: %v", err)
	}
	// Loopback may use http.
	for _, u := range []string{"http://localhost:8080", "http://127.0.0.1", "http://[::1]:9000", "https://cerbix.example",
		"https://cerbix.example/status/", "https://cerbix.example:8443/base"} {
		ts := validTwilioTelephony()
		ts.PublicBaseURL = u
		if err := ts.Validate(); err != nil {
			t.Fatalf("public_base_url %q: %v", u, err)
		}
	}
	for _, lang := range []string{"en", "tr-TR", "zh-Hant-TW", "deu"} {
		ts := validTwilioTelephony()
		ts.VoiceLanguage = lang
		if err := ts.Validate(); err != nil {
			t.Fatalf("voice_language %q: %v", lang, err)
		}
	}

	cases := []struct {
		name   string
		mutate func(*TelephonySettings)
	}{
		{"unknown provider", func(ts *TelephonySettings) { ts.Provider = "pager" }},
		{"enabled without provider", func(ts *TelephonySettings) { ts.Provider = "" }},
		{"bad encoding", func(ts *TelephonySettings) { ts.SMSEncoding = "utf8" }},
		{"segments 0", func(ts *TelephonySettings) { ts.SMSMaxSegments = 0 }},
		{"segments 5", func(ts *TelephonySettings) { ts.SMSMaxSegments = 5 }},
		{"storm incident 0", func(ts *TelephonySettings) { ts.Storm.PerIncidentPerHour = 0 }},
		{"storm number negative", func(ts *TelephonySettings) { ts.Storm.PerNumberPerHour = -1 }},
		{"voice language", func(ts *TelephonySettings) { ts.VoiceLanguage = "english" }},
		{"voice language digits", func(ts *TelephonySettings) { ts.VoiceLanguage = "e1-US" }},
		{"public url http", func(ts *TelephonySettings) { ts.PublicBaseURL = "http://cerbix.example" }},
		{"public url scheme", func(ts *TelephonySettings) { ts.PublicBaseURL = "ftp://cerbix.example" }},
		{"public url no host", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://" }},
		{"public url space", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example/ x" }},
		// The renderer appends /i/<ref> and refuses any Unicode space or control: what it refuses at send
		// is refused at save, and a query, fragment or userinfo would make a broken link.
		{"public url nbsp", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example/a\u00a0b" }},
		{"public url ideographic space", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example/a\u3000b" }},
		{"public url line separator", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example/a\u2028b" }},
		{"public url C1 control", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example/a\u0085b" }},
		{"public url query", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example?x=1" }},
		{"public url empty query", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example/?" }},
		{"public url fragment", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://cerbix.example/#frag" }},
		{"public url userinfo", func(ts *TelephonySettings) { ts.PublicBaseURL = "https://user:pw@cerbix.example" }},
		{"twilio without sid", func(ts *TelephonySettings) { ts.Twilio.AccountSID = "" }},
		{"twilio bad sid", func(ts *TelephonySettings) { ts.Twilio.AccountSID = "AC123" }},
		{"twilio without from", func(ts *TelephonySettings) { ts.Twilio.From = "" }},
		{"twilio bad from", func(ts *TelephonySettings) { ts.Twilio.From = "cerbix" }},
		{"twilio from not e164", func(ts *TelephonySettings) { ts.Twilio.From = "+0123" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := validTwilioTelephony()
			tc.mutate(&ts)
			err := ts.Validate()
			if err == nil || !errors.Is(err, ErrInvalidTelephonySettings) {
				t.Fatalf("Validate() = %v, want ErrInvalidTelephonySettings", err)
			}
		})
	}

	genericCases := []struct {
		name   string
		mutate func(*TelephonySettings)
	}{
		{"method", func(ts *TelephonySettings) { ts.GenericHTTP.Method = "GET" }},
		{"no method", func(ts *TelephonySettings) { ts.GenericHTTP.Method = "" }},
		{"no url", func(ts *TelephonySettings) { ts.GenericHTTP.URL = "" }},
		{"url scheme", func(ts *TelephonySettings) { ts.GenericHTTP.URL = "javascript:alert(1)" }},
		{"url opaque", func(ts *TelephonySettings) { ts.GenericHTTP.URL = "https:sms.example" }},
		{"content type", func(ts *TelephonySettings) { ts.GenericHTTP.ContentType = "text/plain" }},
		{"no content type", func(ts *TelephonySettings) { ts.GenericHTTP.ContentType = "" }},
		{"no template", func(ts *TelephonySettings) { ts.GenericHTTP.BodyTemplate = "" }},
		{"response code field", func(ts *TelephonySettings) { ts.GenericHTTP.ResponseCodeField = "a b" }},
	}
	for _, tc := range genericCases {
		t.Run("generic "+tc.name, func(t *testing.T) {
			ts := validGenericTelephony()
			tc.mutate(&ts)
			if err := ts.Validate(); !errors.Is(err, ErrInvalidTelephonySettings) {
				t.Fatalf("Validate() = %v, want ErrInvalidTelephonySettings", err)
			}
		})
	}
}

// The credential rule is judged after the stored secret has been merged in (blank keeps), so it is a
// separate question the store asks with what it is about to write.
func TestTelephonyValidateCredentials(t *testing.T) {
	ts := validTwilioTelephony()
	if err := ts.ValidateCredentials(true); err != nil {
		t.Fatalf("token present: %v", err)
	}
	if err := ts.ValidateCredentials(false); !errors.Is(err, ErrInvalidTelephonySettings) {
		t.Fatalf("token missing: %v", err)
	}
	ts.Enabled = false
	if err := ts.ValidateCredentials(false); err != nil {
		t.Fatalf("disabled needs no token: %v", err)
	}
	if err := validGenericTelephony().ValidateCredentials(false); err != nil {
		t.Fatalf("generic_http needs no token: %v", err)
	}
}

func TestServicesWithoutRouteError(t *testing.T) {
	e := &ServicesWithoutRouteError{
		Services: []ServiceWithoutRoute{{Project: "Payments", Service: "Checkout", Slug: "checkout"}}, Total: 3,
	}
	var err error = e
	var got *ServicesWithoutRouteError
	if !errors.As(err, &got) || got.Total != 3 {
		t.Fatalf("errors.As = %v", got)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("message should carry the total: %q", err.Error())
	}
}
