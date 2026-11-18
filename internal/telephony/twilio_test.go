package telephony

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSID   = "AC0123456789abcdef0123456789abcdef"
	testToken = "tok3n-secret"
	testTo    = "+12025550147"
	testFrom  = "+12025550100"
	testMG    = "MG0123456789abcdef0123456789abcdef"
	testSMSID = "SM0123456789abcdef0123456789abcdef"
	testCAID  = "CA0123456789abcdef0123456789abcdef"
)

// recorded is one request a test server saw.
type recorded struct {
	method      string
	path        string
	contentType string
	user, pass  string
	authOK      bool
	header      http.Header
	body        string
}

// recorder is an httptest server that records each request and answers with a fixed status/body.
type recorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []recorded
	status int
	body   string
	header map[string]string
}

func newRecorder(t *testing.T, status int, body string) *recorder {
	t.Helper()
	r := &recorder{status: status, body: body}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		u, p, ok := req.BasicAuth()
		r.mu.Lock()
		r.reqs = append(r.reqs, recorded{
			method: req.Method, path: req.URL.EscapedPath(), contentType: req.Header.Get("Content-Type"),
			user: u, pass: p, authOK: ok, header: req.Header.Clone(), body: string(b),
		})
		for k, v := range r.header {
			w.Header().Set(k, v)
		}
		r.mu.Unlock()
		w.WriteHeader(r.status)
		_, _ = io.WriteString(w, r.body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) requests() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func (r *recorder) only(t *testing.T) recorded {
	t.Helper()
	reqs := r.requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	return reqs[0]
}

func twilioAt(base, from string) *Twilio {
	p := NewTwilio(TwilioConfig{AccountSID: testSID, AuthToken: testToken, From: from}, deliveryClient())
	p.baseURL = base
	return p
}

func parseForm(t *testing.T, body string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("body is not a form: %v (%q)", err, body)
	}
	return v
}

func wantForm(t *testing.T, got url.Values, want map[string]string, absent ...string) {
	t.Helper()
	for k, v := range want {
		if len(got[k]) != 1 || got[k][0] != v {
			t.Errorf("form %s = %q, want exactly [%q]", k, got[k], v)
		}
	}
	for _, k := range absent {
		if _, ok := got[k]; ok {
			t.Errorf("form carries %s = %q, want it absent", k, got[k])
		}
	}
}

func TestTwilioDefaultBaseURLAndIdentity(t *testing.T) {
	p := NewTwilio(TwilioConfig{AccountSID: testSID, AuthToken: testToken, From: testFrom}, nil)
	if p.baseURL != "https://api.twilio.com" {
		t.Errorf("base URL = %q", p.baseURL)
	}
	if p.Name() != "twilio" || !p.SupportsVoice() {
		t.Errorf("Name/SupportsVoice = %q/%v, want twilio/true", p.Name(), p.SupportsVoice())
	}
	var _ Provider = p
}

func TestTwilioSMSWithANumberFrom(t *testing.T) {
	rec := newRecorder(t, 201, `{"sid":"`+testSMSID+`","status":"queued","to":"`+testTo+`"}`)
	r := twilioAt(rec.srv.URL, testFrom).SendSMS(context.Background(), testTo, "[cerbix] DOWN api & co")

	got := rec.only(t)
	if got.method != http.MethodPost {
		t.Errorf("method = %s", got.method)
	}
	if want := "/2010-04-01/Accounts/" + testSID + "/Messages.json"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if !got.authOK || got.user != testSID || got.pass != testToken {
		t.Errorf("basic auth = %v %q:%q", got.authOK, got.user, got.pass)
	}
	if !strings.HasPrefix(got.contentType, "application/x-www-form-urlencoded") {
		t.Errorf("content type = %q", got.contentType)
	}
	wantForm(t, parseForm(t, got.body),
		map[string]string{"To": testTo, "Body": "[cerbix] DOWN api & co", "From": testFrom},
		"MessagingServiceSid", "StatusCallback")

	if r != (Result{Outcome: Sent, HTTPStatus: 201, Ref: testSMSID}) {
		t.Errorf("result = %+v", r)
	}
}

func TestTwilioSMSWithAMessagingServiceFrom(t *testing.T) {
	rec := newRecorder(t, 201, `{"sid":"`+testSMSID+`"}`)
	r := twilioAt(rec.srv.URL, testMG).SendSMS(context.Background(), testTo, "hello")
	got := rec.only(t)
	wantForm(t, parseForm(t, got.body),
		map[string]string{"To": testTo, "Body": "hello", "MessagingServiceSid": testMG},
		"From", "StatusCallback")
	if r.Outcome != Sent || r.Ref != testSMSID {
		t.Errorf("result = %+v", r)
	}
}

func TestTwilioCallPassesTheTwiMLInline(t *testing.T) {
	twiml := `<Response><Say language="en-US">cerbix alert.</Say><Say language="en-US">cerbix alert.</Say></Response>`
	rec := newRecorder(t, 201, `{"sid":"`+testCAID+`","status":"queued"}`)
	r := twilioAt(rec.srv.URL, testFrom).PlaceCall(context.Background(), testTo, twiml)
	got := rec.only(t)
	if want := "/2010-04-01/Accounts/" + testSID + "/Calls.json"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if got.method != http.MethodPost || got.user != testSID || got.pass != testToken {
		t.Errorf("method/auth = %s %q:%q", got.method, got.user, got.pass)
	}
	wantForm(t, parseForm(t, got.body),
		map[string]string{"To": testTo, "From": testFrom, "Twiml": twiml},
		"Url", "StatusCallback", "MessagingServiceSid", "Body")
	if r != (Result{Outcome: Sent, HTTPStatus: 201, Ref: testCAID}) {
		t.Errorf("result = %+v", r)
	}
}

// R14: a Messaging Service SID cannot place a call; refused before any request.
func TestTwilioCallFromAMessagingServiceIsRejectedWithoutARequest(t *testing.T) {
	rec := newRecorder(t, 201, `{"sid":"`+testCAID+`"}`)
	r := twilioAt(rec.srv.URL, testMG).PlaceCall(context.Background(), testTo, "<Response/>")
	if n := len(rec.requests()); n != 0 {
		t.Fatalf("a request was sent (%d)", n)
	}
	if r != (Result{Outcome: Rejected, Code: "voice_needs_from_number"}) {
		t.Errorf("result = %+v", r)
	}
}

// FR-041a review P1-1 (D3): a provider whose `from` is a Messaging Service SID cannot place calls, and
// says so — the dispatcher then treats a replica whose snapshot lags the database as not ready, not as
// a provider refusal.
func TestTwilioWithAMessagingServiceFromDoesNotSupportVoice(t *testing.T) {
	if NewTwilio(TwilioConfig{AccountSID: testSID, AuthToken: testToken, From: testMG}, nil).SupportsVoice() {
		t.Fatal("SupportsVoice() = true with a Messaging Service SID as from")
	}
	if !NewTwilio(TwilioConfig{AccountSID: testSID, AuthToken: testToken, From: testFrom}, nil).SupportsVoice() {
		t.Fatal("SupportsVoice() = false with a number as from")
	}
}

// D34: the error answer's numeric code is kept, its text is not.
func TestTwilioErrorAnswerIsRejectedWithItsCode(t *testing.T) {
	body := `{"code": 21211, "message": "The 'To' number ` + testTo + ` is not a valid phone number.", "more_info": "https://www.twilio.com/docs/errors/21211", "status": 400}`
	rec := newRecorder(t, 400, body)
	r := twilioAt(rec.srv.URL, testFrom).SendSMS(context.Background(), testTo, "x")
	if r != (Result{Outcome: Rejected, HTTPStatus: 400, Code: "21211"}) {
		t.Errorf("result = %+v", r)
	}
	assertNoText(t, r, testTo, "valid phone", "more_info", "docs/errors")
}

func TestTwilioErrorCodeOutsideThePatternIsDropped(t *testing.T) {
	for _, body := range []string{
		`{"code": "call +12025550147 now", "status": 400}`,
		`{"code": 1.5e400}`,
		`{"code": {"x": 1}}`,
		`not json at all ` + testTo,
		``,
	} {
		rec := newRecorder(t, 400, body)
		r := twilioAt(rec.srv.URL, testFrom).SendSMS(context.Background(), testTo, "x")
		if r != (Result{Outcome: Rejected, HTTPStatus: 400}) {
			t.Errorf("body %q: result = %+v", body, r)
		}
	}
}

func TestTwilioRefOutsideThePatternIsDropped(t *testing.T) {
	rec := newRecorder(t, 201, `{"sid":"SM <script> `+testTo+`"}`)
	r := twilioAt(rec.srv.URL, testFrom).SendSMS(context.Background(), testTo, "x")
	if r != (Result{Outcome: Sent, HTTPStatus: 201}) {
		t.Errorf("result = %+v", r)
	}
}

func TestTwilioUnreachableStatuses(t *testing.T) {
	for _, status := range []int{408, 429, 500, 503} {
		rec := newRecorder(t, status, `{"code": 20500, "message": "Internal Server Error"}`)
		r := twilioAt(rec.srv.URL, testFrom).SendSMS(context.Background(), testTo, "x")
		if r.Outcome != Unreachable || r.HTTPStatus != status || r.TransportClass != "" {
			t.Errorf("status %d: result = %+v", status, r)
		}
	}
}

func TestTwilioTimeoutIsUnreachable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := twilioAt(srv.URL, testFrom).SendSMS(ctx, testTo, "x")
	if r != (Result{Outcome: Unreachable, TransportClass: "timeout"}) {
		t.Errorf("result = %+v", r)
	}
}

// The transport class is the closed set of notify.DeliveryFailure, never the client's error text
// (which quotes the URL, account SID included).
func TestTwilioTransportErrorKeepsOnlyTheClass(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	r := twilioAt("http://"+addr, testFrom).SendSMS(context.Background(), testTo, "x")
	if r != (Result{Outcome: Unreachable, TransportClass: "connection refused"}) {
		t.Errorf("result = %+v", r)
	}
	assertNoText(t, r, testSID, addr, "Accounts")
}

// D14: the egress client refuses redirects; the 3xx is the provider's answer — rejected with its
// status — and not a transport error. The Location target is never contacted.
func TestTwilioRedirectIsRejectedWithItsStatus(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"sid":"`+testSMSID+`"}`)
	}))
	defer target.Close()
	rec := newRecorder(t, 302, "")
	rec.header = map[string]string{"Location": target.URL + "/2010-04-01/Accounts/" + testSID + "/Messages.json"}

	r := twilioAt(rec.srv.URL, testFrom).SendSMS(context.Background(), testTo, "x")
	if r != (Result{Outcome: Rejected, HTTPStatus: 302}) {
		t.Errorf("result = %+v", r)
	}
	if followed.Load() != 0 {
		t.Errorf("the redirect was followed")
	}
}

// The account SID is a path segment, escaped as one.
func TestTwilioAccountSIDIsEscapedInThePath(t *testing.T) {
	rec := newRecorder(t, 201, `{"sid":"`+testSMSID+`"}`)
	p := NewTwilio(TwilioConfig{AccountSID: "AC1/../../x?y", AuthToken: testToken, From: testFrom}, deliveryClient())
	p.baseURL = rec.srv.URL
	p.SendSMS(context.Background(), testTo, "x")
	got := rec.only(t)
	if want := "/2010-04-01/Accounts/AC1%2F..%2F..%2Fx%3Fy/Messages.json"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
}
