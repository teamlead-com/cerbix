package telephony

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

const jsonTemplate = `{"to":"{{to}}","from":"{{from}}","msg":"{{text}}","kind":"alert"}`

func genericAt(t *testing.T, cfg GenericHTTPConfig) *GenericHTTP {
	t.Helper()
	if cfg.Method == "" {
		cfg.Method = http.MethodPost
	}
	if cfg.ContentType == "" {
		cfg.ContentType = ContentTypeJSON
	}
	if cfg.BodyTemplate == "" {
		cfg.BodyTemplate = jsonTemplate
	}
	if cfg.From == "" {
		cfg.From = "cerbix"
	}
	g, err := NewGenericHTTP(cfg, deliveryClient())
	if err != nil {
		t.Fatalf("NewGenericHTTP: %v", err)
	}
	return g
}

func decodeJSONObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body is not a JSON object: %v (%q)", err, body)
	}
	return m
}

// Each placeholder is substituted as a JSON string value: escaped, no quotes added, the rest of the
// literal around it kept.
func TestGenericJSONSubstitutesEachPlaceholderAsAValue(t *testing.T) {
	text := "DOWN \"api\" \\ path\nline\ttab <b>&amp; ünï 😀  "
	from := `Ops "desk" \ 1`
	rec := newRecorder(t, 200, `{"ok":true}`)
	g := genericAt(t, GenericHTTPConfig{
		URL:          rec.srv.URL + "/send",
		BodyTemplate: `{"to":"{{to}}","from":"{{from}}","msg":"Alert: {{text}} !","kind":"alert"}`,
		From:         from,
	})
	r := g.SendSMS(context.Background(), testTo, text)
	got := rec.only(t)
	if got.method != http.MethodPost || got.path != "/send" {
		t.Errorf("method/path = %s %s", got.method, got.path)
	}
	if got.contentType != ContentTypeJSON {
		t.Errorf("content type = %q", got.contentType)
	}
	m := decodeJSONObject(t, got.body)
	want := map[string]any{"to": testTo, "from": from, "msg": "Alert: " + text + " !", "kind": "alert"}
	if len(m) != len(want) {
		t.Errorf("body keys = %v", m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("body %s = %q, want %q", k, m[k], v)
		}
	}
	if r != (Result{Outcome: Sent, HTTPStatus: 200}) {
		t.Errorf("result = %+v", r)
	}
}

// P1: a name built to close the string and add a recipient stays one string value, and the request
// goes to the verified number only.
func TestGenericJSONInjectedRecipientStaysOneString(t *testing.T) {
	injected := `x","to":"+12025550199","x":"`
	text := "[cerbix] DOWN " + injected + " (prod)"
	rec := newRecorder(t, 200, "")
	g := genericAt(t, GenericHTTPConfig{URL: rec.srv.URL})
	g.SendSMS(context.Background(), testTo, text)
	body := rec.only(t).body

	dec := json.NewDecoder(strings.NewReader(body))
	keys := map[string]int{}
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	for k := range m {
		keys[k]++
	}
	if len(m) != 4 {
		t.Errorf("body has keys %v, want to/from/msg/kind", keys)
	}
	if strings.Count(body, `"to":`) != 1 {
		t.Errorf("body names a recipient more than once: %s", body)
	}
	obj := decodeJSONObject(t, body)
	if obj["to"] != testTo {
		t.Errorf("recipient = %q, want %q", obj["to"], testTo)
	}
	if obj["msg"] != text {
		t.Errorf("msg = %q, want %q", obj["msg"], text)
	}
}

func TestGenericFormSubstitutesEachPlaceholderAsAValue(t *testing.T) {
	text := "DOWN a&b=c+d 100% sure/? é\n"
	from := "Ops & Co=1"
	rec := newRecorder(t, 202, "")
	g := genericAt(t, GenericHTTPConfig{
		Method:       http.MethodPut,
		URL:          rec.srv.URL + "/sms",
		ContentType:  ContentTypeForm,
		BodyTemplate: "To={{to}}&From={{from}}&Body=Alert:{{text}}&Type=alert",
		From:         from,
	})
	r := g.SendSMS(context.Background(), testTo, text)
	got := rec.only(t)
	if got.method != http.MethodPut || got.contentType != ContentTypeForm {
		t.Errorf("method/content type = %s %q", got.method, got.contentType)
	}
	wantForm(t, parseForm(t, got.body),
		map[string]string{"To": testTo, "From": from, "Body": "Alert:" + text, "Type": "alert"})
	if r != (Result{Outcome: Sent, HTTPStatus: 202}) {
		t.Errorf("result = %+v", r)
	}
}

func TestGenericFormInjectedRecipientStaysOneValue(t *testing.T) {
	text := "x&To=+12025550199&Body=y"
	rec := newRecorder(t, 200, "")
	g := genericAt(t, GenericHTTPConfig{
		URL: rec.srv.URL, ContentType: ContentTypeForm, BodyTemplate: "To={{to}}&Body={{text}}",
	})
	g.SendSMS(context.Background(), testTo, text)
	wantForm(t, parseForm(t, rec.only(t).body), map[string]string{"To": testTo, "Body": text})
}

// A value that itself spells a placeholder is not substituted again.
func TestGenericSubstitutionIsOnePass(t *testing.T) {
	rec := newRecorder(t, 200, "")
	g := genericAt(t, GenericHTTPConfig{URL: rec.srv.URL, From: "{{to}}"})
	g.SendSMS(context.Background(), testTo, "{{from}} {{to}}")
	m := decodeJSONObject(t, rec.only(t).body)
	if m["from"] != "{{to}}" || m["msg"] != "{{from}} {{to}}" || m["to"] != testTo {
		t.Errorf("body = %v", m)
	}
}

func TestGenericHeadersAreSentAndTheContentTypeIsTheConfiguredOne(t *testing.T) {
	rec := newRecorder(t, 200, "")
	g := genericAt(t, GenericHTTPConfig{
		URL: rec.srv.URL,
		Headers: map[string]string{
			"Authorization": "Bearer gw-secret",
			"X-Account":     "42",
			"Content-Type":  "text/plain",
		},
	})
	g.SendSMS(context.Background(), testTo, "x")
	got := rec.only(t)
	if got.header.Get("Authorization") != "Bearer gw-secret" || got.header.Get("X-Account") != "42" {
		t.Errorf("headers = %v", got.header)
	}
	if got.contentType != ContentTypeJSON {
		t.Errorf("content type = %q, want the configured %q", got.contentType, ContentTypeJSON)
	}
}

func TestGenericIdentityAndVoice(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	g := genericAt(t, GenericHTTPConfig{URL: srv.URL})
	var _ Provider = g
	if g.Name() != "generic_http" || g.SupportsVoice() {
		t.Errorf("Name/SupportsVoice = %q/%v, want generic_http/false", g.Name(), g.SupportsVoice())
	}
	r := g.PlaceCall(context.Background(), testTo, "<Response/>")
	if r != (Result{Outcome: Rejected, Code: "voice_unsupported"}) {
		t.Errorf("result = %+v", r)
	}
	if hits.Load() != 0 {
		t.Errorf("PlaceCall sent a request")
	}
}

func TestGenericClassification(t *testing.T) {
	cases := []struct {
		status int
		want   Outcome
	}{{200, Sent}, {204, Sent}, {400, Rejected}, {401, Rejected}, {408, Unreachable}, {429, Unreachable}, {503, Unreachable}}
	for _, c := range cases {
		rec := newRecorder(t, c.status, `{"error":"no balance for `+testTo+`"}`)
		r := genericAt(t, GenericHTTPConfig{URL: rec.srv.URL}).SendSMS(context.Background(), testTo, "x")
		if r != (Result{Outcome: c.want, HTTPStatus: c.status}) {
			t.Errorf("status %d: result = %+v", c.status, r)
		}
		assertNoText(t, r, testTo, "balance")
	}
}

func TestGenericRedirectIsRejectedWithItsStatus(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	for _, status := range []int{301, 302, 307, 308} {
		rec := newRecorder(t, status, "")
		rec.header = map[string]string{"Location": target.URL + "/elsewhere"}
		r := genericAt(t, GenericHTTPConfig{URL: rec.srv.URL}).SendSMS(context.Background(), testTo, "x")
		if r != (Result{Outcome: Rejected, HTTPStatus: status}) {
			t.Errorf("status %d: result = %+v", status, r)
		}
	}
	if followed.Load() != 0 {
		t.Errorf("a redirect was followed %d times", followed.Load())
	}
}

func TestGenericResponseCodeField(t *testing.T) {
	cases := []struct {
		name  string
		field string
		body  string
		want  string
	}{
		{"string", "error_code", `{"error_code":"E_BALANCE","message":"no money for ` + testTo + `"}`, "E_BALANCE"},
		{"number", "err", `{"err":4031,"message":"x"}`, "4031"},
		{"big number kept as written", "err", `{"err":12345678901234567890}`, "12345678901234567890"},
		{"invalid pattern dropped", "err", `{"err":"no money for ` + testTo + `"}`, ""},
		{"too long dropped", "err", `{"err":"` + strings.Repeat("A", 33) + `"}`, ""},
		{"object dropped", "err", `{"err":{"code":"X"}}`, ""},
		{"bool dropped", "err", `{"err":true}`, ""},
		{"nested key not read", "code", `{"error":{"code":"X"}}`, ""},
		{"absent key", "err", `{"other":"X"}`, ""},
		{"not JSON", "err", `err=X`, ""},
		{"no field configured", "", `{"err":"X","code":"Y","error_code":"Z"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := newRecorder(t, 400, c.body)
			g := genericAt(t, GenericHTTPConfig{URL: rec.srv.URL, ResponseCodeField: c.field})
			r := g.SendSMS(context.Background(), testTo, "x")
			if r != (Result{Outcome: Rejected, HTTPStatus: 400, Code: c.want}) {
				t.Errorf("result = %+v, want code %q", r, c.want)
			}
			assertNoText(t, r, testTo, "money")
		})
	}
}

func TestValidateTemplateAccepts(t *testing.T) {
	ok := []struct{ ct, tmpl string }{
		{ContentTypeJSON, jsonTemplate},
		{ContentTypeJSON, `{"to":"{{to}}","text":"{{text}}"}`},
		{ContentTypeJSON, `{"messages":[{"dest":["{{to}}"],"body":"[{{from}}] {{text}}","n":1,"f":null,"b":true}]}`},
		{ContentTypeJSON, `["{{to}}","{{text}}"]`},
		{ContentTypeJSON, "{\n  \"to\": \"{{to}}\",\n  \"text\": \"{{text}} {{text}}\"\n}"},
		{ContentTypeJSON, `{"to":"{{to}}","text":"{{text}}","note":"a \"quoted\" \\ word"}`},
		{ContentTypeForm, "To={{to}}&Body={{text}}"},
		{ContentTypeForm, "To={{to}}&From={{from}}&Body=Alert%3A+{{text}}&flag="},
		{ContentTypeForm, "api_key=abc&to=%2B{{to}}&msg={{text}}"},
	}
	for _, c := range ok {
		if err := ValidateTemplate(c.ct, c.tmpl); err != nil {
			t.Errorf("ValidateTemplate(%s, %q) = %v, want nil", c.ct, c.tmpl, err)
		}
	}
}

// Every form template ValidateTemplate accepts renders a form for any number and text: the check is
// about what will be sent, not about the stand-in word it substitutes.
func TestAcceptedFormTemplatesRenderAForm(t *testing.T) {
	for _, tmpl := range []string{
		"To={{to}}&Body={{text}}",
		"To={{to}}&From={{from}}&Body=Alert%3A+{{text}}&flag=",
		"api_key=abc&to=%2B{{to}}&msg={{text}}",
	} {
		if err := ValidateTemplate(ContentTypeForm, tmpl); err != nil {
			t.Fatalf("ValidateTemplate(%q) = %v", tmpl, err)
		}
		g := &GenericHTTP{cfg: GenericHTTPConfig{ContentType: ContentTypeForm, BodyTemplate: tmpl, From: "cerbix"}}
		body := g.render("+905551236869", "[cerbix] DOWN api 100% & more")
		if _, err := url.ParseQuery(body); err != nil {
			t.Errorf("template %q rendered %q: not a form: %v", tmpl, body, err)
		}
	}
}

func TestValidateTemplateRefuses(t *testing.T) {
	bad := []struct{ name, ct, tmpl string }{
		{"json placeholder bare", ContentTypeJSON, `{"to":{{to}},"text":"{{text}}"}`},
		{"json placeholder bare in array", ContentTypeJSON, `["{{to}}",{{text}}]`},
		{"json placeholder in a key", ContentTypeJSON, `{"{{to}}":"x","text":"{{text}}"}`},
		{"json placeholder inside a key", ContentTypeJSON, `{"to":"{{to}}","k{{from}}":"{{text}}"}`},
		{"json unknown placeholder", ContentTypeJSON, `{"to":"{{to}}","text":"{{text}}","n":"{{name}}"}`},
		{"json unknown placeholder spacing", ContentTypeJSON, `{"to":"{{ to }}","text":"{{text}}"}`},
		{"json unclosed placeholder", ContentTypeJSON, `{"to":"{{to}}","text":"{{text"}`},
		{"json invalid", ContentTypeJSON, `{"to":"{{to}}","text":"{{text}}"`},
		{"json trailing data", ContentTypeJSON, `{"to":"{{to}}","text":"{{text}}"} {}`},
		{"json escape eats placeholder", ContentTypeJSON, `{"to":"\{{to}}","text":"{{text}}"}`},
		{"json no recipient", ContentTypeJSON, `{"text":"{{text}}"}`},
		{"json no text", ContentTypeJSON, `{"to":"{{to}}"}`},
		{"json empty", ContentTypeJSON, ``},
		{"form placeholder as a name", ContentTypeForm, "{{to}}=x&Body={{text}}"},
		{"form placeholder inside a name", ContentTypeForm, "To={{to}}&B{{from}}=x&Body={{text}}"},
		{"form unknown placeholder", ContentTypeForm, "To={{to}}&Body={{text}}&X={{name}}"},
		{"form pair without value", ContentTypeForm, "To={{to}}&Body={{text}}&flag"},
		{"form empty name", ContentTypeForm, "To={{to}}&Body={{text}}&=x"},
		{"form empty pair", ContentTypeForm, "To={{to}}&&Body={{text}}"},
		{"form bad escape", ContentTypeForm, "To={{to}}&Body={{text}}%zz"},
		// A `%` right before a placeholder renders as `%` + an escaped value (`%%2B…`), which is no
		// form. The stand-in word must not complete the escape, whatever hex its letters spell.
		{"form percent before a placeholder", ContentTypeForm, "To=%{{to}}&Body={{text}}"},
		{"form half escape before a placeholder", ContentTypeForm, "To=%c{{to}}&Body={{text}}"},
		{"form percent before the text", ContentTypeForm, "To={{to}}&Body=%{{text}}"},
		{"form semicolon", ContentTypeForm, "To={{to}};Body={{text}}"},
		{"form no recipient", ContentTypeForm, "Body={{text}}"},
		{"form no text", ContentTypeForm, "To={{to}}"},
		{"unknown content type", "text/plain", "{{to}} {{text}}"},
	}
	for _, c := range bad {
		if err := ValidateTemplate(c.ct, c.tmpl); err == nil {
			t.Errorf("%s: ValidateTemplate(%s, %q) = nil, want an error", c.name, c.ct, c.tmpl)
		}
	}
}

func TestNewGenericHTTPRefusesABadConfig(t *testing.T) {
	good := GenericHTTPConfig{Method: http.MethodPost, URL: "https://gw.example/send", ContentType: ContentTypeJSON, BodyTemplate: jsonTemplate}
	if _, err := NewGenericHTTP(good, nil); err != nil {
		t.Fatalf("good config refused: %v", err)
	}
	mut := []func(*GenericHTTPConfig){
		func(c *GenericHTTPConfig) { c.Method = http.MethodGet },
		func(c *GenericHTTPConfig) { c.Method = "post" },
		func(c *GenericHTTPConfig) { c.ContentType = "text/plain" },
		func(c *GenericHTTPConfig) { c.BodyTemplate = `{"to":{{to}}}` },
		func(c *GenericHTTPConfig) { c.URL = "" },
		func(c *GenericHTTPConfig) { c.URL = "ftp://gw.example/send" },
		func(c *GenericHTTPConfig) { c.URL = "https://gw.example/%zz?key=SECRET1" },
		func(c *GenericHTTPConfig) { c.URL = "https:///nohost" },
	}
	for i, m := range mut {
		c := good
		m(&c)
		_, err := NewGenericHTTP(c, nil)
		if err == nil {
			t.Errorf("mutation %d: config accepted: %+v", i, c)
			continue
		}
		if strings.Contains(err.Error(), "SECRET1") {
			t.Errorf("mutation %d: error quotes the URL: %v", i, err)
		}
	}
}
