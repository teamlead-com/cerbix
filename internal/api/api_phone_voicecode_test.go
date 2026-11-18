package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a revision 9, §4.2 "The code by a call", §11 row "Verification by a call", through the
// handlers over the fake store. The store is the same for both ways — one code, one hash, the same
// limits — so what is proved here is the handlers' half: `via` read on the three sends, the call
// placed instead of the SMS, the refusal before any code is issued, the language, the 400 for an
// unknown value, telephony_can_call on GET /api/v1/me and the log line of a failed call. The real
// store's half (one allowance for both ways, no phone_deliveries row) is
// phone_voicecode_integration_test.go.

const channelSendPath = "/api/v1/notification-channels/ch-num/verify/send"

// codeSend is one of the three sends of a code (§4.2), with what each needs before it can send.
type codeSend struct {
	name string
	p    authz.Principal
	// prepare runs before the send: a stored number for the person's verify/send, a `number` channel
	// for the channel's.
	prepare func(t *testing.T, fs *fakeStore, h http.Handler)
	method  string
	path    string
	// body is the request body for a given `via` member ("" = no member); bodyless sends answer an
	// empty body for "".
	body func(viaMember string) string
	// verify checks the code just sent, as the SMS code is checked.
	verifyPath string
	// issued reports whether the store was asked for a code.
	issued func(fs *fakeStore) bool
}

func viaBody(viaMember string) string {
	if viaMember == "" {
		return ""
	}
	return "{" + viaMember + "}"
}

func codeSends() []codeSend {
	return []codeSend{
		{
			name: "PUT /api/v1/me/phone", p: phonePerson,
			prepare: func(*testing.T, *fakeStore, http.Handler) {},
			method:  http.MethodPut, path: mePhonePath,
			body: func(viaMember string) string {
				if viaMember == "" {
					return myNumberBody
				}
				return `{"number":"+90 532 111 68 69",` + viaMember + `}`
			},
			verifyPath: mePhoneVerifyPath,
			issued:     func(fs *fakeStore) bool { return fs.phones["u-phone"] != nil },
		},
		{
			name: "POST /api/v1/me/phone/verify/send", p: phonePerson,
			prepare: func(t *testing.T, fs *fakeStore, h http.Handler) {
				t.Helper()
				fs.phones = map[string]*fakePhone{"u-phone": {number: myNumber, code: "000000"}}
			},
			method: http.MethodPost, path: meVerifySendPath, body: viaBody,
			verifyPath: mePhoneVerifyPath,
			issued:     func(fs *fakeStore) bool { return fs.phones["u-phone"].code != "000000" },
		},
		{
			name: "POST /api/v1/notification-channels/{id}/verify/send", p: pcProjAdmin,
			prepare: func(t *testing.T, fs *fakeStore, h http.Handler) {
				t.Helper()
				seedPhoneChannel(fs, "ch-num", domain.ChannelVoice, domain.PhoneRecipientNumber, "", myNumber, false)
			},
			method: http.MethodPost, path: channelSendPath, body: viaBody,
			verifyPath: "/api/v1/notification-channels/ch-num/verify",
			issued:     func(fs *fakeStore) bool { return len(fs.phoneChans().requesters) != 0 },
		},
	}
}

// §11: `via: voice` on each of the three sends places ONE call to the number and sends no SMS; the
// call speaks the code twice, digit by digit, and asks for nothing (no <Gather>).
func TestCodeByCallOnEachSend(t *testing.T) {
	for _, s := range codeSends() {
		t.Run(s.name, func(t *testing.T) {
			fs, src, h := phoneChannelFixture()
			fs.phoneCode = "730194"
			s.prepare(t, fs, h)
			rec := do(h, s.p, s.method, s.path, s.body(`"via":"voice"`))
			if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
				t.Fatalf("send by a call = %d %s", rec.Code, rec.Body.String())
			}
			assertNoNumber(t, rec.Body.String())
			calls := src.p.Calls()
			if len(calls) != 1 || calls[0].Kind != "voice" || calls[0].To != myNumber {
				t.Fatalf("provider requests = %+v; want exactly one call to the number and no SMS", calls)
			}
			if want := telephony.VerificationCodeVoice("730194", "en-US"); calls[0].Body != want {
				t.Fatalf("the call's TwiML = %s; want %s", calls[0].Body, want)
			}
			if strings.Count(calls[0].Body, "7, 3, 0, 1, 9, 4.") != 2 || strings.Contains(calls[0].Body, "<Gather") {
				t.Fatalf("the call does not speak the code twice digit by digit, or asks for a key: %s", calls[0].Body)
			}
		})
	}
}

// §11: the code a call spoke verifies exactly as an SMS code does.
func TestCodeByCallVerifiesAsAnSMSCodeDoes(t *testing.T) {
	for _, s := range codeSends() {
		t.Run(s.name, func(t *testing.T) {
			fs, _, h := phoneChannelFixture()
			fs.phoneCode = "730194"
			s.prepare(t, fs, h)
			if rec := do(h, s.p, s.method, s.path, s.body(`"via":"voice"`)); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
				t.Fatalf("send by a call = %d %s", rec.Code, rec.Body.String())
			}
			assertReason(t, "a wrong code", do(h, s.p, http.MethodPost, s.verifyPath, `{"code":"111111"}`),
				http.StatusBadRequest, "code_incorrect")
			rec := do(h, s.p, http.MethodPost, s.verifyPath, `{"code":"730194"}`)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"verified":true`) &&
				!strings.Contains(rec.Body.String(), `"number_verified":true`) {
				t.Fatalf("verify the spoken code = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// countingTransport answers every request 201 without a network and counts them: a refusal decided
// before any request leaves it at zero.
type countingTransport struct {
	mu sync.Mutex
	n  int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(`{"sid":"SMx"}`)),
		Header: http.Header{}, Request: r}, nil
}

func (c *countingTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// noCallProviders are the two providers that cannot place calls (D3), built as production builds
// them, each with its own counting transport.
func noCallProviders(t *testing.T) map[string]func() (telephony.Provider, *countingTransport) {
	t.Helper()
	return map[string]func() (telephony.Provider, *countingTransport){
		"generic_http": func() (telephony.Provider, *countingTransport) {
			tr := &countingTransport{}
			g, err := telephony.NewGenericHTTP(telephony.GenericHTTPConfig{Method: http.MethodPost,
				URL: "https://sms.example/send", ContentType: telephony.ContentTypeJSON,
				BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`}, &http.Client{Transport: tr})
			if err != nil {
				t.Fatal(err)
			}
			return g, tr
		},
		"twilio with a Messaging Service SID": func() (telephony.Provider, *countingTransport) {
			tr := &countingTransport{}
			return telephony.NewTwilio(telephony.TwilioConfig{AccountSID: "AC00000000000000000000000000000000",
				AuthToken: "tok", From: "MG00000000000000000000000000000000"}, &http.Client{Transport: tr}), tr
		},
	}
}

// §11: `via: voice` while the provider cannot place calls — generic_http, or twilio whose `from` is
// a Messaging Service SID — is refused with 400 voice_unsupported_by_provider BEFORE a code is
// issued: nothing is stored, no code, no request to the provider. An SMS code through the same
// provider is still sent, so the refusal is the call's alone.
func TestCodeByCallRefusedWhenTheProviderCannotCall(t *testing.T) {
	for pname, build := range noCallProviders(t) {
		for _, s := range codeSends() {
			t.Run(pname+"/"+s.name, func(t *testing.T) {
				fs, src, h := phoneChannelFixture()
				s.prepare(t, fs, h)
				provider, tr := build()
				src.override = provider
				assertReason(t, "voice", do(h, s.p, s.method, s.path, s.body(`"via":"voice"`)),
					http.StatusBadRequest, "voice_unsupported_by_provider")
				if s.issued(fs) {
					t.Fatal("a code was issued (or a number stored) for a call the provider cannot place")
				}
				if n := tr.count(); n != 0 {
					t.Fatalf("the refused call made %d provider requests", n)
				}
				if rec := do(h, s.p, s.method, s.path, s.body(`"via":"sms"`)); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
					t.Fatalf("the SMS code through the same provider = %d %s", rec.Code, rec.Body.String())
				}
				if n := tr.count(); n != 1 {
					t.Fatalf("the SMS code made %d provider requests, want 1", n)
				}
			})
		}
	}
}

// §11: the spoken code is in the person's language when it is for their own number and they set
// one, otherwise in the instance's voice_language; a channel's code is always in the instance's.
func TestCodeByCallLanguage(t *testing.T) {
	fs, src, h := phoneChannelFixture()
	fs.voiceLanguage = "de-DE"
	fs.phoneCode = "730194"
	lastCall := func(t *testing.T) telephony.FakeCall {
		t.Helper()
		calls := src.p.Calls()
		if len(calls) == 0 || calls[len(calls)-1].Kind != "voice" {
			t.Fatalf("no call placed: %+v", calls)
		}
		return calls[len(calls)-1]
	}

	// A first number, no language of the person's: the instance's.
	if rec := do(h, phonePerson, http.MethodPut, mePhonePath, `{"number":"+905321116869","via":"voice"}`); rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	if got := lastCall(t).Body; got != telephony.VerificationCodeVoice("730194", "de-DE") {
		t.Fatalf("PUT without a language of the person's: %s; want the instance's de-DE", got)
	}
	// The person sets one: their own number's code is spoken in it — on a resend and on a PUT that
	// replaces the number (the language is kept, §4.2).
	if rec := do(h, phonePerson, http.MethodPatch, mePhonePrefsPath, `{"language":"tr-TR"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch language = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, phonePerson, http.MethodPost, meVerifySendPath, `{"via":"voice"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("resend = %d %s", rec.Code, rec.Body.String())
	}
	if got := lastCall(t).Body; got != telephony.VerificationCodeVoice("730194", "tr-TR") {
		t.Fatalf("resend with the person's language: %s; want tr-TR", got)
	}
	if rec := do(h, phonePerson, http.MethodPut, mePhonePath, `{"number":"+905321116870","via":"voice"}`); rec.Code != http.StatusOK {
		t.Fatalf("replace = %d %s", rec.Code, rec.Body.String())
	}
	if got := lastCall(t).Body; got != telephony.VerificationCodeVoice("730194", "tr-TR") {
		t.Fatalf("replacing PUT with the person's language: %s; want tr-TR", got)
	}
	// Cleared: the instance's again.
	if rec := do(h, phonePerson, http.MethodPatch, mePhonePrefsPath, `{"language":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear language = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, phonePerson, http.MethodPost, meVerifySendPath, `{"via":"voice"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("resend = %d %s", rec.Code, rec.Body.String())
	}
	if got := lastCall(t).Body; got != telephony.VerificationCodeVoice("730194", "de-DE") {
		t.Fatalf("resend after clearing the language: %s; want the instance's de-DE", got)
	}

	// A channel's code: the instance's, even when the requester has a language of their own.
	lang := "tr-TR"
	fs.phones["u-pa"] = &fakePhone{number: "+905321116871", verified: true, language: &lang}
	seedPhoneChannel(fs, "ch-num", domain.ChannelVoice, domain.PhoneRecipientNumber, "", myNumber, false)
	if rec := do(h, pcProjAdmin, http.MethodPost, channelSendPath, `{"via":"voice"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("channel send = %d %s", rec.Code, rec.Body.String())
	}
	if got := lastCall(t).Body; got != telephony.VerificationCodeVoice("730194", "de-DE") {
		t.Fatalf("a channel's code: %s; want the instance's de-DE", got)
	}
}

// §11: an unknown `via` is 400 `via_invalid` on each of the three sends — before anything is
// stored, issued or sent. An absent `via` (no body, `{}`) and `"sms"` send the SMS code.
func TestCodeViaUnknownIsRefused(t *testing.T) {
	for _, s := range codeSends() {
		t.Run(s.name, func(t *testing.T) {
			for _, bad := range []string{`"via":"fax"`, `"via":""`, `"via":"VOICE"`, `"via":"sms "`} {
				fs, src, h := phoneChannelFixture()
				s.prepare(t, fs, h)
				assertReason(t, bad, do(h, s.p, s.method, s.path, s.body(bad)), http.StatusBadRequest, "via_invalid")
				if s.issued(fs) || len(src.p.Calls()) != 0 {
					t.Fatalf("%s: issued %v, provider requests %+v; want neither", bad, s.issued(fs), src.p.Calls())
				}
			}
			// A value that is not a string is an unreadable body, as for any field.
			fs, src, h := phoneChannelFixture()
			s.prepare(t, fs, h)
			if rec := do(h, s.p, s.method, s.path, s.body(`"via":1`)); rec.Code != http.StatusBadRequest || len(src.p.Calls()) != 0 {
				t.Fatalf("via 1 = %d %s, %d requests", rec.Code, rec.Body.String(), len(src.p.Calls()))
			}

			for _, ok := range []string{"", `"via":"sms"`, `"via":null`} {
				fs, src, h := phoneChannelFixture()
				s.prepare(t, fs, h)
				rec := do(h, s.p, s.method, s.path, s.body(ok))
				if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
					t.Fatalf("%q = %d %s", ok, rec.Code, rec.Body.String())
				}
				if calls := src.p.Calls(); len(calls) != 1 || calls[0].Kind != "sms" {
					t.Fatalf("%q: provider requests %+v; want the SMS code", ok, calls)
				}
			}
		})
	}
	// `{}` on a bodyless send is an absent `via` too.
	for _, path := range []string{meVerifySendPath, channelSendPath} {
		fs, src, h := phoneChannelFixture()
		fs.phones = map[string]*fakePhone{"u-phone": {number: myNumber}}
		seedPhoneChannel(fs, "ch-num", domain.ChannelVoice, domain.PhoneRecipientNumber, "", myNumber, false)
		p := phonePerson
		if path == channelSendPath {
			p = pcProjAdmin
		}
		if rec := do(h, p, http.MethodPost, path, `{}`); rec.Code != http.StatusNoContent {
			t.Fatalf("%s {} = %d %s", path, rec.Code, rec.Body.String())
		}
		if calls := src.p.Calls(); len(calls) != 1 || calls[0].Kind != "sms" {
			t.Fatalf("%s {}: provider requests %+v", path, calls)
		}
	}
}

// §4.2: GET /api/v1/me's telephony_can_call is the instance's "can place calls" (D3) — true only
// while telephony is on and the provider can place calls — in both states.
func TestMeCarriesTelephonyCanCall(t *testing.T) {
	fs, _, h := phoneFixture()
	for _, can := range []bool{true, false} {
		fs.canCall = can
		rec := do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
		var me map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
		}
		raw, ok := me["telephony_can_call"]
		if !ok || string(raw) != map[bool]string{true: "true", false: "false"}[can] {
			t.Fatalf("store can call %v: telephony_can_call = %s (present %v)", can, raw, ok)
		}
	}
}

// §11: telephony_can_call degrades with its siblings: a failed read of it — or of any other part of
// the phone answer — is 200 with telephony_can_call false, phone null, telephony off, voice language
// null, and one log line naming the read, never the error's text.
func TestMeDegradesWhenTheCanCallReadFails(t *testing.T) {
	cases := map[string]func(fs *fakeStore, err error){
		"get_user_phone":           func(fs *fakeStore, err error) { fs.phoneGetErr = err },
		"telephony_enabled":        func(fs *fakeStore, err error) { fs.telephonyErr = err },
		"telephony_voice_language": func(fs *fakeStore, err error) { fs.voiceLanguageErr = err },
		"telephony_can_call":       func(fs *fakeStore, err error) { fs.canCallErr = err },
	}
	for op, inject := range cases {
		t.Run(op, func(t *testing.T) {
			fs, src, _ := phoneFixture()
			fs.canCall = true
			var logs bytes.Buffer
			h := api.New(fs, slog.New(slog.NewTextHandler(&logs, nil)), 8).WithTelephony(src).Router()
			inject(fs, errors.New("store: read failed for "+myNumber))
			rec := do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
			var me map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
			}
			if string(me["telephony_can_call"]) != "false" || string(me["phone"]) != "null" ||
				string(me["telephony_enabled"]) != "false" || string(me["telephony_voice_language"]) != "null" {
				t.Fatalf("a failing %s read: %s; want every phone field degraded", op, rec.Body.String())
			}
			line := strings.TrimSpace(logs.String())
			if line == "" || strings.Contains(line, "\n") || !strings.Contains(line, "op="+op) ||
				!strings.Contains(line, "user_id="+phonePerson.UserID) {
				t.Fatalf("want one log line naming the user and %s, got %q", op, logs.String())
			}
			assertNoNumber(t, line)
		})
	}
}

// §11, P3: a call that fails — refused or unanswered — is answered 502 with the outcome as the SMS
// send reports it, and logged once with what the operator needs (the way it went, the provider's
// outcome, status and code); the number is in no log line and in no answer.
func TestFailedCodeCallNeverLogsTheNumber(t *testing.T) {
	results := map[string]struct {
		res    telephony.Result
		reason string
	}{
		"rejected":    {telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21215"}, "provider_rejected"},
		"unreachable": {telephony.Result{Outcome: telephony.Unreachable, TransportClass: "timeout"}, "provider_unreachable"},
	}
	for _, s := range codeSends() {
		for name, r := range results {
			t.Run(s.name+"/"+name, func(t *testing.T) {
				fs, src, _ := phoneChannelFixture()
				var logs bytes.Buffer
				h := api.New(fs, slog.New(slog.NewTextHandler(&logs, nil)), 8).WithTelephony(src).Router()
				s.prepare(t, fs, h)
				src.p.ScriptCall(r.res)
				assertReason(t, name, do(h, s.p, s.method, s.path, s.body(`"via":"voice"`)), http.StatusBadGateway, r.reason)
				if calls := src.p.Calls(); len(calls) != 1 || calls[0].Kind != "voice" {
					t.Fatalf("provider requests %+v; want the one call, and no SMS in its place", calls)
				}
				out := logs.String()
				if strings.Count(out, "phone_code_send_failed") != 1 || !strings.Contains(out, "via=voice") ||
					!strings.Contains(out, "outcome="+string(r.res.Outcome)) {
					t.Fatalf("log = %q; want one phone_code_send_failed line with via=voice and the outcome", out)
				}
				assertNoNumber(t, out)
			})
		}
	}
}

// §4.2 (revision 9, review): the answers about the number's own state come BEFORE the refusal of a
// call the provider cannot place — the verified number again is a no-op 200, no stored number is 409
// no_number, a verified number is 409 number_already_verified — and in every order no code is
// issued, no allowance is spent and no request is made.
func TestNumberStateAnswersComeBeforeTheCallRefusal(t *testing.T) {
	for pname, build := range noCallProviders(t) {
		type tc struct {
			name, method, path, body string
			p                        authz.Principal
			seed                     func(*fakeStore)
			status                   int
			reason                   string
		}
		verified := func(fs *fakeStore) {
			fs.phones = map[string]*fakePhone{"u-phone": {number: myNumber, code: "000000", verified: true}}
		}
		cases := []tc{
			{"re-PUT of the verified number", http.MethodPut, mePhonePath, `{"number":"+90 532 111 68 69","via":"voice"}`, phonePerson, verified, http.StatusOK, ""},
			{"verify/send without a number", http.MethodPost, meVerifySendPath, `{"via":"voice"}`, phonePerson, func(*fakeStore) {}, http.StatusConflict, "no_number"},
			{"verify/send of a verified number", http.MethodPost, meVerifySendPath, `{"via":"voice"}`, phonePerson, verified, http.StatusConflict, "number_already_verified"},
			{"channel verify/send of a verified number", http.MethodPost, "/api/v1/notification-channels/ch-ok/verify/send", `{"via":"voice"}`, pcProjAdmin,
				func(fs *fakeStore) {
					seedPhoneChannel(fs, "ch-ok", domain.ChannelVoice, domain.PhoneRecipientNumber, "", myNumber, true)
				}, http.StatusConflict, "number_already_verified"},
		}
		for _, c := range cases {
			t.Run(pname+"/"+c.name, func(t *testing.T) {
				fs, src, h := phoneChannelFixture()
				c.seed(fs)
				provider, tr := build()
				src.override = provider
				rec := do(h, c.p, c.method, c.path, c.body)
				if c.reason == "" {
					if rec.Code != c.status {
						t.Fatalf("= %d %s; want %d", rec.Code, rec.Body.String(), c.status)
					}
				} else {
					assertReason(t, c.name, rec, c.status, c.reason)
				}
				if n := tr.count(); n != 0 {
					t.Fatalf("made %d provider requests", n)
				}
				if p := fs.phones["u-phone"]; p != nil && p.code != "000000" {
					t.Fatal("a code was issued")
				}
				if got := fs.phoneChans().requesters; len(got) != 0 {
					t.Fatalf("a channel code was issued: %v", got)
				}
			})
		}
	}
}
