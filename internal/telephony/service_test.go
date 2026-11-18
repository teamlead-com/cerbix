package telephony

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type staticSettings struct {
	mu sync.Mutex
	ts domain.TelephonySettings
}

func (s *staticSettings) Telephony() domain.TelephonySettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ts
}

// CurrentTelephony is the fixed value: a test's source is current by construction.
func (s *staticSettings) CurrentTelephony(context.Context) (domain.TelephonySettings, error) {
	return s.Telephony(), nil
}

func (s *staticSettings) set(ts domain.TelephonySettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ts = ts
}

func genericSettings(url string) domain.TelephonySettings {
	ts := domain.TelephonySettings{
		Configured: true, Enabled: true, Provider: domain.TelephonyProviderGenericHTTP,
		GenericHTTP: domain.TelephonyGenericHTTP{
			Method: "POST", URL: url, ContentType: ContentTypeJSON,
			BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`,
			Headers:      map[string]string{"X-Api-Key": "k"},
		},
	}
	ts.Normalize()
	return ts
}

func twilioSettings() domain.TelephonySettings {
	ts := domain.TelephonySettings{
		Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio,
		Twilio: domain.TelephonyTwilio{AccountSID: "AC0123456789abcdef0123456789abcdef", AuthToken: "tok", From: "+15005550006"},
	}
	ts.Normalize()
	return ts
}

// countingTransport proves which client a provider uses.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestServiceRefusesWhileDisabled(t *testing.T) {
	src := &staticSettings{ts: domain.DefaultTelephonySettings()}
	svc := NewService(src, nil)
	if _, err := svc.Provider(); !errors.Is(err, ErrTelephonyDisabled) {
		t.Fatalf("absent settings: err = %v, want ErrTelephonyDisabled", err)
	}
	ts := twilioSettings()
	ts.Enabled = false
	src.set(ts)
	if _, err := svc.Provider(); !errors.Is(err, ErrTelephonyDisabled) {
		t.Fatalf("disabled: err = %v, want ErrTelephonyDisabled", err)
	}
}

// Only the secret of the provider in force matters: an unreadable headers map does not stop Twilio, and
// an unreadable token does not stop generic_http.
func TestServiceRefusesOnlyTheUnreadableSecretInForce(t *testing.T) {
	cases := []struct {
		name    string
		ts      domain.TelephonySettings
		refused bool
	}{
		{"twilio, token unreadable", func() domain.TelephonySettings {
			ts := twilioSettings()
			ts.Twilio.AuthToken, ts.AuthTokenUnreadable = "", true
			return ts
		}(), true},
		{"twilio, headers unreadable", func() domain.TelephonySettings {
			ts := twilioSettings()
			ts.HeadersUnreadable = true
			return ts
		}(), false},
		{"generic, headers unreadable", func() domain.TelephonySettings {
			ts := genericSettings("https://sms.example/send")
			ts.GenericHTTP.Headers, ts.HeadersUnreadable = nil, true
			return ts
		}(), true},
		{"generic, token unreadable", func() domain.TelephonySettings {
			ts := genericSettings("https://sms.example/send")
			ts.AuthTokenUnreadable = true
			return ts
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewService(&staticSettings{ts: tc.ts}, nil).Provider()
			if tc.refused {
				if !errors.Is(err, ErrSecretsUnreadable) {
					t.Fatalf("err = %v, want ErrSecretsUnreadable", err)
				}
				return
			}
			if err != nil || p == nil {
				t.Fatalf("provider = %v, %v", p, err)
			}
		})
	}
}

// The provider is built from the snapshot on each use, so a save is in force for the next send.
func TestServiceBuildsTheCurrentProviderWithTheInjectedClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("header not sent")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	transport := &countingTransport{}
	client := &http.Client{Transport: transport}
	src := &staticSettings{ts: genericSettings(srv.URL)}
	svc := NewService(src, client)

	p, err := svc.Provider()
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if p.Name() != "generic_http" || p.SupportsVoice() {
		t.Fatalf("provider = %s voice=%v", p.Name(), p.SupportsVoice())
	}
	if res := p.SendSMS(context.Background(), "+15005550006", "hi"); res.Outcome != Sent {
		t.Fatalf("send = %+v", res)
	}
	if hits.Load() != 1 || transport.n.Load() != 1 {
		t.Fatalf("hits=%d via injected client=%d", hits.Load(), transport.n.Load())
	}

	src.set(twilioSettings())
	p, err = svc.Provider()
	if err != nil {
		t.Fatalf("twilio provider: %v", err)
	}
	if p.Name() != "twilio" || !p.SupportsVoice() {
		t.Fatalf("provider after the switch = %s", p.Name())
	}
	if svc.Settings().Provider != domain.TelephonyProviderTwilio {
		t.Fatalf("Settings() is not the current snapshot")
	}
}

func TestValidateSettings(t *testing.T) {
	if err := ValidateSettings(genericSettings("https://sms.example/send")); err != nil {
		t.Fatalf("valid generic: %v", err)
	}
	if err := ValidateSettings(twilioSettings()); err != nil {
		t.Fatalf("valid twilio: %v", err)
	}
	// The domain rules come first.
	bad := twilioSettings()
	bad.SMSMaxSegments = 9
	if err := ValidateSettings(bad); !errors.Is(err, domain.ErrInvalidTelephonySettings) {
		t.Fatalf("domain rule: %v", err)
	}
	// The template is checked at save (carried from Task 4's review) when generic_http is enabled and
	// selected.
	tmpl := genericSettings("https://sms.example/send")
	tmpl.GenericHTTP.BodyTemplate = `{"to":"{{to}}"}`
	if err := ValidateSettings(tmpl); !errors.Is(err, ErrTemplate) {
		t.Fatalf("template without {{text}}: %v", err)
	}
	// FR-041a review P3-1: under another provider the generic_http template is a draft and does not
	// block the save — not an incomplete one, not one without a content type, enabled or not. It is
	// judged when generic_http is enabled and the provider saved: every save sends the template again.
	for _, enabled := range []bool{true, false} {
		for _, g := range []domain.TelephonyGenericHTTP{
			{ContentType: ContentTypeJSON, BodyTemplate: `{"to":"{{to}}"}`},
			{BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`},
		} {
			draft := twilioSettings()
			draft.Enabled = enabled
			draft.GenericHTTP = g
			if err := ValidateSettings(draft); err != nil {
				t.Fatalf("twilio (enabled %v) with a draft generic_http template %+v: %v", enabled, g, err)
			}
		}
	}
	// Header names and values are checked when headers are sent.
	for _, h := range []map[string]string{{"Bad Name": "v"}, {"X-Ok": "a\r\nb"}, {"": "v"}} {
		ts := genericSettings("https://sms.example/send")
		ts.GenericHTTP.Headers = h
		if err := ValidateSettings(ts); !errors.Is(err, domain.ErrInvalidTelephonySettings) {
			t.Fatalf("headers %q: %v", h, err)
		}
	}
}

// FR-041a owner answer 2 (review M-6): the generic_http template is judged only when generic_http is
// BOTH enabled AND the selected provider. A disabled group keeps a draft as typed — one without a
// content type, an incomplete one — whichever provider is saved, none included; the save that
// enables generic_http sends the template again and judges it.
func TestValidateSettingsJudgesTheTemplateOnlyWhenGenericHTTPIsEnabledAndSelected(t *testing.T) {
	drafts := []domain.TelephonyGenericHTTP{
		{Method: "POST", URL: "https://sms.example/send", BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`},
		{Method: "POST", URL: "https://sms.example/send", ContentType: ContentTypeJSON, BodyTemplate: `{"to":"{{to}}"}`},
	}
	for _, provider := range []string{domain.TelephonyProviderGenericHTTP, ""} {
		for _, g := range drafts {
			ts := genericSettings("https://sms.example/send")
			ts.Enabled = false
			ts.Provider = provider
			ts.GenericHTTP = g
			if err := ValidateSettings(ts); err != nil {
				t.Fatalf("disabled, provider %q, draft %+v: %v; want saved", provider, g, err)
			}
		}
	}
	// Enabled and selected, the same incomplete template is refused as before.
	ts := genericSettings("https://sms.example/send")
	ts.GenericHTTP = drafts[1]
	if err := ValidateSettings(ts); !errors.Is(err, ErrTemplate) {
		t.Fatalf("enabled generic_http with an incomplete template: %v; want ErrTemplate", err)
	}
}

// snapshotTrap is a source whose snapshot must never be read by CurrentProvider.
type snapshotTrap struct {
	t       *testing.T
	current domain.TelephonySettings
	err     error
}

func (s snapshotTrap) Telephony() domain.TelephonySettings {
	s.t.Error("CurrentProvider read the snapshot")
	return domain.TelephonySettings{}
}

func (s snapshotTrap) CurrentTelephony(context.Context) (domain.TelephonySettings, error) {
	return s.current, s.err
}

// Group-A review M-5: CurrentProvider builds from the settings as stored now and from nothing else —
// the source must offer CurrentTelephony (a compile-time requirement of SettingsSource), so no wrapper
// can make it fall back to a snapshot — and a failed read is an error.
func TestCurrentProviderReadsOnlyTheCurrentSettings(t *testing.T) {
	p, err := NewService(snapshotTrap{t: t, current: twilioSettings()}, nil).CurrentProvider(context.Background())
	if err != nil || p.Name() != domain.TelephonyProviderTwilio {
		t.Fatalf("current provider = %v, %v; want twilio", p, err)
	}
	down := errors.New("database down")
	if _, err := NewService(snapshotTrap{t: t, err: down}, nil).CurrentProvider(context.Background()); !errors.Is(err, down) {
		t.Fatalf("over a failed read: %v; want the read's error", err)
	}
}
