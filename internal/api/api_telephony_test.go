package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const telephonyPath = "/api/v1/settings/telephony"

const telephonySaveBody = `{"enabled":true,"provider":"twilio",
 "twilio":{"account_sid":"AC0123456789abcdef0123456789abcdef","auth_token":"tok-secret","from":"+15005550006"},
 "generic_http":{"method":"POST","url":"https://sms.example/send","headers":{"X-Api-Key":"hdr-secret"},
   "content_type":"application/json","body_template":"{\"to\":\"{{to}}\",\"text\":\"{{text}}\"}"},
 "public_base_url":"https://cerbix.example","voice_language":"tr-TR","sms_encoding":"unicode",
 "sms_max_segments":3,"storm":{"per_incident_per_hour":4,"per_number_per_hour":10}}`

type telephonyViewJSON struct {
	Configured              bool   `json:"configured"`
	EncryptionKeyConfigured bool   `json:"encryption_key_configured"`
	Enabled                 bool   `json:"enabled"`
	Provider                string `json:"provider"`
	Twilio                  struct {
		AccountSID          string `json:"account_sid"`
		AuthTokenSet        bool   `json:"auth_token_set"`
		AuthTokenUnreadable bool   `json:"auth_token_unreadable"`
		From                string `json:"from"`
	} `json:"twilio"`
	GenericHTTP struct {
		Method            string `json:"method"`
		URL               string `json:"url"`
		HeadersSet        bool   `json:"headers_set"`
		HeadersUnreadable bool   `json:"headers_unreadable"`
		ContentType       string `json:"content_type"`
		BodyTemplate      string `json:"body_template"`
	} `json:"generic_http"`
	PublicBaseURL  string `json:"public_base_url"`
	VoiceLanguage  string `json:"voice_language"`
	SMSEncoding    string `json:"sms_encoding"`
	SMSMaxSegments int    `json:"sms_max_segments"`
	Storm          struct {
		PerIncidentPerHour int `json:"per_incident_per_hour"`
		PerNumberPerHour   int `json:"per_number_per_hour"`
	} `json:"storm"`
}

func decodeTelephonyView(t *testing.T, body string) telephonyViewJSON {
	t.Helper()
	var v telephonyViewJSON
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode view %s: %v", body, err)
	}
	return v
}

func assertNoTelephonySecret(t *testing.T, body string) {
	t.Helper()
	for _, s := range []string{"tok-secret", "hdr-secret", "X-Api-Key", "auth_token\"", "\"headers\""} {
		if strings.Contains(body, s) {
			t.Fatalf("response carries %q: %s", s, body)
		}
	}
}

func TestTelephonySettingsNeedGlobalAdmin(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if method == http.MethodPut {
			body = telephonySaveBody
		}
		if rec := do(h, o1Admin, method, telephonyPath, body); rec.Code != http.StatusForbidden {
			t.Fatalf("org admin %s = %d, want 403", method, rec.Code)
		}
	}
	if len(fs.telephonySaves) != 0 {
		t.Fatal("a forbidden write reached the store")
	}
}

func TestTelephonySettingsReadDefaultsAndKeyFlag(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)
	rec := do(h, globalAdmin, http.MethodGet, telephonyPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeTelephonyView(t, rec.Body.String())
	if v.Configured || v.Enabled || v.Provider != "" || !v.EncryptionKeyConfigured || v.VoiceLanguage != "en-US" ||
		v.SMSEncoding != "transliterate" || v.SMSMaxSegments != 2 || v.Storm.PerIncidentPerHour != 5 ||
		v.Storm.PerNumberPerHour != 20 || v.Twilio.AuthTokenSet || v.GenericHTTP.HeadersSet {
		t.Fatalf("default view = %+v", v)
	}

	fs.telephonyNoKey = true
	v = decodeTelephonyView(t, do(h, globalAdmin, http.MethodGet, telephonyPath, "").Body.String())
	if v.EncryptionKeyConfigured {
		t.Fatal("encryption_key_configured stays true without a key")
	}
}

func TestTelephonySettingsSaveNeverEchoesSecretsAndIsAudited(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)

	rec := do(h, globalAdmin, http.MethodPut, telephonyPath, telephonySaveBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	assertNoTelephonySecret(t, rec.Body.String())
	v := decodeTelephonyView(t, rec.Body.String())
	if !v.Configured || !v.Enabled || v.Provider != "twilio" || !v.Twilio.AuthTokenSet || !v.GenericHTTP.HeadersSet ||
		v.Twilio.AccountSID != "AC0123456789abcdef0123456789abcdef" || v.VoiceLanguage != "tr-TR" ||
		v.SMSEncoding != "unicode" || v.SMSMaxSegments != 3 || v.Storm.PerIncidentPerHour != 4 ||
		v.PublicBaseURL != "https://cerbix.example" || v.GenericHTTP.BodyTemplate == "" {
		t.Fatalf("saved view = %+v", v)
	}
	if fs.instanceSettings.Telephony.Twilio.AuthToken != "tok-secret" ||
		fs.instanceSettings.Telephony.GenericHTTP.Headers["X-Api-Key"] != "hdr-secret" {
		t.Fatalf("secrets not handed to the store: %+v", fs.instanceSettings.Telephony)
	}
	get := do(h, globalAdmin, http.MethodGet, telephonyPath, "")
	assertNoTelephonySecret(t, get.Body.String())

	// One instance-level audit row, naming the group and no secret.
	if len(fs.audit) != 1 {
		t.Fatalf("audit rows = %d", len(fs.audit))
	}
	a := fs.audit[0]
	if a.Action != "settings.telephony.update" || a.Target != "telephony" || a.OrgID != "" || a.ActorUserID != "ga" {
		t.Fatalf("audit = %+v", a)
	}

	// A blank write keeps both secrets.
	blank := `{"enabled":true,"provider":"twilio","twilio":{"account_sid":"AC0123456789abcdef0123456789abcdef","from":"+15005550006"}}`
	rec = do(h, globalAdmin, http.MethodPut, telephonyPath, blank)
	if rec.Code != http.StatusOK {
		t.Fatalf("blank save = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeTelephonyView(t, rec.Body.String())
	if !v.Twilio.AuthTokenSet || !v.GenericHTTP.HeadersSet {
		t.Fatalf("blank write dropped a secret: %+v", v)
	}

	// headers_clear removes the stored map.
	clr := `{"enabled":true,"provider":"twilio","twilio":{"account_sid":"AC0123456789abcdef0123456789abcdef","from":"+15005550006"},"generic_http":{"headers_clear":true}}`
	rec = do(h, globalAdmin, http.MethodPut, telephonyPath, clr)
	if rec.Code != http.StatusOK || decodeTelephonyView(t, rec.Body.String()).GenericHTTP.HeadersSet {
		t.Fatalf("headers_clear = %d %s", rec.Code, rec.Body.String())
	}
	if !fs.telephonySaves[len(fs.telephonySaves)-1].HeadersClear {
		t.Fatal("headers_clear not passed to the store")
	}
}

func reasonOf(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error == "" {
		t.Fatalf("error body %s: %v", body, err)
	}
	return e.Reason
}

func TestTelephonySettingsRefusals(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)

	cases := []struct {
		name   string
		body   string
		status int
		reason string
	}{
		{"headers conflict", `{"enabled":false,"generic_http":{"headers":{"X-A":"b"},"headers_clear":true}}`,
			http.StatusBadRequest, "headers_conflict"},
		{"template", `{"enabled":true,"provider":"generic_http","generic_http":{"method":"POST","url":"https://sms.example/send",
		  "content_type":"application/json","body_template":"{\"to\":\"{{to}}\"}"}}`,
			http.StatusBadRequest, "template_invalid"},
		{"segments", `{"enabled":false,"sms_max_segments":9}`, http.StatusBadRequest, ""},
		{"enabled without provider", `{"enabled":true}`, http.StatusBadRequest, ""},
		{"unknown field", `{"enabled":false,"quiet_hours":"22-07"}`, http.StatusBadRequest, ""},
		{"no enabled", `{"provider":"twilio"}`, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, globalAdmin, http.MethodPut, telephonyPath, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if got := reasonOf(t, rec.Body.String()); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
		})
	}

	// D29: refused while no key is configured, before anything else is judged.
	fs.telephonyNoKey = true
	rec := do(h, globalAdmin, http.MethodPut, telephonyPath, telephonySaveBody)
	if rec.Code != http.StatusConflict || reasonOf(t, rec.Body.String()) != "encryption_key_missing" {
		t.Fatalf("no key = %d %s", rec.Code, rec.Body.String())
	}
	if fs.instanceSettings.Telephony.Configured || len(fs.audit) != 0 {
		t.Fatal("a refused write was saved or audited")
	}
}

// R10: the warning is the save's own answer.
func TestTelephonySettingsServicesWithoutRoute(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)
	fs.telephonyRefusal = &domain.ServicesWithoutRouteError{
		Services: []domain.ServiceWithoutRoute{
			{Project: "Alpha", Service: "Pager", Slug: "alpha-pager"},
			{Project: "Delta", Service: "Voice", Slug: "delta-voice"},
		},
		Total: 3,
	}

	rec := do(h, globalAdmin, http.MethodPut, telephonyPath, telephonySaveBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unconfirmed = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error                     string                       `json:"error"`
		Reason                    string                       `json:"reason"`
		ServicesWithoutRoute      []domain.ServiceWithoutRoute `json:"services_without_route"`
		ServicesWithoutRouteTotal int                          `json:"services_without_route_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error == "" || body.Reason != "services_without_route" || body.ServicesWithoutRouteTotal != 3 ||
		!reflect.DeepEqual(body.ServicesWithoutRoute, fs.telephonyRefusal.Services) {
		t.Fatalf("409 body = %s", rec.Body.String())
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	var first map[string]any
	_ = json.Unmarshal(mustArray(t, raw["services_without_route"])[0], &first)
	if len(first) != 3 || first["project"] != "Alpha" || first["service"] != "Pager" || first["slug"] != "alpha-pager" {
		t.Fatalf("entry shape = %v", first)
	}
	if fs.instanceSettings.Telephony.Configured || len(fs.audit) != 0 {
		t.Fatal("the 409 saved or audited")
	}

	// The same request, confirmed, is saved.
	confirmed := strings.Replace(telephonySaveBody, `{"enabled":true,`, `{"enabled":true,"confirm_unroutable":true,`, 1)
	rec = do(h, globalAdmin, http.MethodPut, telephonyPath, confirmed)
	if rec.Code != http.StatusOK || !fs.instanceSettings.Telephony.Configured || len(fs.audit) != 1 {
		t.Fatalf("confirmed = %d %s", rec.Code, rec.Body.String())
	}
	if !fs.telephonySaves[len(fs.telephonySaves)-1].ConfirmUnroutable {
		t.Fatal("confirm_unroutable not passed to the store")
	}
}

func mustArray(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		t.Fatalf("not a non-empty array: %s", raw)
	}
	return out
}

// A secret the configured keys cannot open is still stored: the view says so, per secret, so the form
// can ask for it to be re-entered or the key restored — and a blank write keeps it (review of Task 6).
func TestTelephonySettingsViewReportsUnreadableSecrets(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)
	fs.instanceSettings.Telephony = domain.TelephonySettings{
		Configured: true, AuthTokenUnreadable: true,
		GenericHTTP: domain.TelephonyGenericHTTP{Headers: map[string]string{"X-Api-Key": "hdr-secret"}},
	}
	rec := do(h, globalAdmin, http.MethodPut, telephonyPath, `{"enabled":false,"provider":"twilio"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("blank save = %d %s", rec.Code, rec.Body.String())
	}
	assertNoTelephonySecret(t, rec.Body.String())
	v := decodeTelephonyView(t, rec.Body.String())
	if !v.Twilio.AuthTokenSet || !v.Twilio.AuthTokenUnreadable || !v.GenericHTTP.HeadersSet || v.GenericHTTP.HeadersUnreadable {
		t.Fatalf("view = %+v", v)
	}
}

// FR-041a owner answer 2 (review M-6): with telephony disabled, a draft generic_http template — one
// without a content type, an incomplete one — is saved as typed under generic_http and under no
// provider; it is judged only while generic_http is enabled and selected.
func TestTelephonySettingsDisabledGenericHTTPDraftTemplateIsSaved(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)
	for _, provider := range []string{`"provider":"generic_http",`, ``} {
		for _, g := range []string{
			`{"method":"POST","url":"https://sms.example/send","body_template":"{\"to\":\"{{to}}\",\"text\":\"{{text}}\"}"}`,
			`{"content_type":"application/json","body_template":"{\"to\":\"{{to}}\"}"}`,
		} {
			body := `{"enabled":false,` + provider + `"generic_http":` + g + `}`
			rec := do(h, globalAdmin, http.MethodPut, telephonyPath, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("disabled draft %s = %d %s, want 200", body, rec.Code, rec.Body.String())
			}
		}
	}
}

// FR-041a review P3-1: a draft generic_http template — incomplete, or without a content type — does
// not block saving another provider; it is kept as typed.
func TestTelephonySettingsDraftTemplateDoesNotBlockAnotherProvider(t *testing.T) {
	fs := seededStore()
	h := newSettingsHandler(fs)
	for _, g := range []string{
		`{"content_type":"application/json","body_template":"{\"to\":\"{{to}}\"}"}`,
		`{"body_template":"{\"to\":\"{{to}}\",\"text\":\"{{text}}\"}"}`,
	} {
		body := `{"enabled":false,"provider":"twilio","twilio":{"account_sid":"AC0123456789abcdef0123456789abcdef",
		  "from":"+15005550006"},"generic_http":` + g + `}`
		rec := do(h, globalAdmin, http.MethodPut, telephonyPath, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("twilio with a draft generic_http block %s = %d %s, want 200", g, rec.Code, rec.Body.String())
		}
	}
}
