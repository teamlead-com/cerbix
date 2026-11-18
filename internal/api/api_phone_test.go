package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/deliveryfail"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

const (
	mePhonePath       = "/api/v1/me/phone"
	mePhoneVerifyPath = "/api/v1/me/phone/verify"
	mePhonePrefsPath  = "/api/v1/me/phone/preferences"
	testSMSPath       = "/api/v1/settings/telephony/test-sms"
	myNumber          = "+905321116869"
	myNumberBody      = `{"number":"+90 532 111 68 69"}`
)

var (
	phonePerson  = authz.Principal{UserID: "u-phone", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleEditor}}}
	phoneMachine = authz.Principal{UserID: "m-phone", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleEditor}}}
)

// fakeProviderSource stands in for telephony.Service: the provider in force, or the refusal.
type fakeProviderSource struct {
	p   *telephony.FakeProvider
	err error
	// override, when set, is the provider in force instead of p — a real provider built as
	// production builds it (FR-041a revision 9: the providers that cannot place calls).
	override telephony.Provider
}

func (f *fakeProviderSource) CurrentProvider(context.Context) (telephony.Provider, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.override != nil {
		return f.override, nil
	}
	return f.p, nil
}

func phoneFixture() (*fakeStore, *fakeProviderSource, http.Handler) {
	fs := seededStore()
	fs.users["u-phone"] = domain.User{ID: "u-phone", Email: "person@x", Kind: domain.UserKindHuman}
	fs.users["u-other"] = domain.User{ID: "u-other", Email: "other@x", Kind: domain.UserKindHuman}
	fs.users["m-phone"] = domain.User{ID: "m-phone", Email: "machine@x", Kind: domain.UserKindMachine}
	fs.users["ga"] = domain.User{ID: "ga", Email: "ga@x", Kind: domain.UserKindHuman, IsGlobalAdmin: true}
	fs.telephonyOn = true
	src := &fakeProviderSource{p: telephony.NewFakeProvider()}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTelephony(src).Router()
	return fs, src, h
}

// assertNoNumber fails when body carries the number's digits in any form a mask does not: the
// national part, its first six digits, the whole number (P3).
func assertNoNumber(t *testing.T, body string) {
	t.Helper()
	for _, s := range []string{"905321116869", "5321116869", "532111", "1116869"} {
		if strings.Contains(body, s) {
			t.Fatalf("response carries the number (%q): %s", s, body)
		}
	}
}

func assertReason(t *testing.T, label string, rec *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	body := rec.Body.String()
	if got := rec.Code; got != status {
		t.Fatalf("%s: status = %d, want %d (%s)", label, got, status, body)
	}
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Reason != reason || e.Error == "" {
		t.Fatalf("%s: body = %s, want reason %q and a sentence", label, body, reason)
	}
	assertNoNumber(t, body)
}

func decodePhone(t *testing.T, body string) domain.UserPhone {
	t.Helper()
	var p domain.UserPhone
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode phone %s: %v", body, err)
	}
	return p
}

func phoneAudits(fs *fakeStore, action string) []domain.AuditEntry {
	var out []domain.AuditEntry
	for _, e := range fs.audit {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestPhoneRoutesRefuseTokensAndMachines(t *testing.T) {
	_, src, h := phoneFixture()
	routes := []struct{ method, path, body string }{
		{http.MethodPut, mePhonePath, myNumberBody},
		{http.MethodPost, mePhoneVerifyPath, `{"code":"482193"}`},
		{http.MethodPost, mePhoneVerifyPath + "/send", ""},
		{http.MethodDelete, mePhonePath, ""},
		{http.MethodPatch, mePhonePrefsPath, `{"language":"tr-TR"}`},
	}
	for _, r := range routes {
		for name, p := range map[string]authz.Principal{"api token": tokenEditor, "machine user": phoneMachine} {
			if rec := do(h, p, r.method, r.path, r.body); rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s %s = %d, want 403", name, r.method, r.path, rec.Code)
			}
		}
	}
	if n := len(src.p.Calls()); n != 0 {
		t.Fatalf("a refused principal caused %d sends", n)
	}
	// The test SMS is the caller's own number: a token has none.
	tokenAdmin := authz.Principal{UserID: "apitoken:ga", ViaToken: true, IsGlobalAdmin: true}
	if rec := do(h, tokenAdmin, http.MethodPost, testSMSPath, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("token test-sms = %d, want 403", rec.Code)
	}
}

func TestMeCarriesPhoneAndTelephonyEnabled(t *testing.T) {
	fs, _, h := phoneFixture()
	fs.telephonyOn = false
	rec := do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d", rec.Code)
	}
	var me map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if string(me["phone"]) != "null" || string(me["telephony_enabled"]) != "false" {
		t.Fatalf("me without a phone, telephony off: phone=%s telephony_enabled=%s", me["phone"], me["telephony_enabled"])
	}
	for _, k := range []string{"user", "memberships", "totp_enabled"} {
		if _, ok := me[k]; !ok {
			t.Fatalf("me lost %q: %s", k, rec.Body.String())
		}
	}

	fs.telephonyOn = true
	if rec := do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody); rec.Code != http.StatusOK {
		t.Fatalf("put phone = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
	assertNoNumber(t, rec.Body.String())
	var got struct {
		Phone            map[string]json.RawMessage `json:"phone"`
		TelephonyEnabled bool                       `json:"telephony_enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.TelephonyEnabled || len(got.Phone) != 3 || string(got.Phone["number_masked"]) != `"+90•••••6869"` ||
		string(got.Phone["verified"]) != "false" || string(got.Phone["language"]) != "null" {
		t.Fatalf("me with a phone: %s", rec.Body.String())
	}
}

// GET /api/v1/me never fails because of its phone part: a failed phone read or telephony read
// answers 200 with BOTH fields degraded — phone null, telephony off — and one log line with the
// user's id and the error's class. Never its message, which could carry a number (P3): the injected
// errors below carry the number to prove it.
func TestMeDegradesWhenAPhoneReadFails(t *testing.T) {
	cases := map[string]func(fs *fakeStore, err error){
		"get_user_phone":    func(fs *fakeStore, err error) { fs.phoneGetErr = err },
		"telephony_enabled": func(fs *fakeStore, err error) { fs.telephonyErr = err },
	}
	for op, inject := range cases {
		t.Run(op, func(t *testing.T) {
			fs, src, _ := phoneFixture()
			var logs bytes.Buffer
			h := api.New(fs, slog.New(slog.NewTextHandler(&logs, nil)), 8).WithTelephony(src).Router()
			if rec := do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody); rec.Code != http.StatusOK {
				t.Fatalf("put phone = %d %s", rec.Code, rec.Body.String())
			}
			logs.Reset()
			inject(fs, errors.New("store: read failed for "+myNumber+" (5321116869)"))

			rec := do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("me with a failing %s read = %d %s, want 200", op, rec.Code, rec.Body.String())
			}
			var me map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
				t.Fatal(err)
			}
			if string(me["phone"]) != "null" || string(me["telephony_enabled"]) != "false" {
				t.Fatalf("me with a failing %s read: phone=%s telephony_enabled=%s; want both degraded",
					op, me["phone"], me["telephony_enabled"])
			}
			for _, k := range []string{"user", "memberships", "totp_enabled"} {
				if _, ok := me[k]; !ok {
					t.Fatalf("me lost %q: %s", k, rec.Body.String())
				}
			}
			assertNoNumber(t, rec.Body.String())

			line := strings.TrimSpace(logs.String())
			if strings.Count(line, "\n") != 0 || line == "" {
				t.Fatalf("want exactly one log line, got %q", logs.String())
			}
			if !strings.Contains(line, "user_id="+phonePerson.UserID) || !strings.Contains(line, "op="+op) ||
				!strings.Contains(line, "error_class=") {
				t.Fatalf("log line lacks the user id, the read or the error class: %s", line)
			}
			assertNoNumber(t, line)
		})
	}
}

func TestPutMyPhoneSendsOnlyTheCode(t *testing.T) {
	fs, src, h := phoneFixture()
	rec := do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	assertNoNumber(t, rec.Body.String())
	if p := decodePhone(t, rec.Body.String()); p.Verified || p.NumberMasked != "+90•••••6869" {
		t.Fatalf("phone = %+v", p)
	}
	calls := src.p.Calls()
	if len(calls) != 1 || calls[0].Kind != "sms" || calls[0].To != myNumber ||
		calls[0].Body != telephony.VerificationCodeText("482193") {
		t.Fatalf("provider calls = %+v; want exactly the code SMS to the normalized number (P1)", calls)
	}
	audits := phoneAudits(fs, "user.phone.set")
	if len(audits) != 1 || audits[0].Target != "user u-phone" || audits[0].OrgID != "" || audits[0].ActorUserID != "u-phone" {
		t.Fatalf("audit = %+v", audits)
	}
}

func TestPutMyPhoneRefusals(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(fs *fakeStore, src *fakeProviderSource)
		body    string
		status  int
		reason  string
		stored  bool // the number is stored although the send failed
		audited bool
	}{
		{name: "telephony off", setup: func(fs *fakeStore, _ *fakeProviderSource) { fs.telephonyOn = false },
			body: myNumberBody, status: http.StatusConflict, reason: "telephony_disabled"},
		{name: "provider snapshot off", setup: func(_ *fakeStore, src *fakeProviderSource) { src.err = telephony.ErrTelephonyDisabled },
			body: myNumberBody, status: http.StatusConflict, reason: "telephony_disabled"},
		{name: "no key", setup: func(fs *fakeStore, _ *fakeProviderSource) { fs.telephonyNoKey = true },
			body: myNumberBody, status: http.StatusConflict, reason: "encryption_key_missing"},
		{name: "invalid", body: `{"number":"0532 111 68 69"}`, status: http.StatusBadRequest, reason: "number_invalid"},
		{name: "missing", body: `{}`, status: http.StatusBadRequest, reason: "number_invalid"},
		{name: "in use", setup: func(fs *fakeStore, _ *fakeProviderSource) {
			fs.phones = map[string]*fakePhone{"u-other": {number: myNumber, verified: true}}
		}, body: myNumberBody, status: http.StatusConflict, reason: "number_in_use"},
		{name: "limited", setup: func(fs *fakeStore, _ *fakeProviderSource) { fs.phoneSetErr = domain.ErrPhoneSendLimited },
			body: myNumberBody, status: http.StatusTooManyRequests, reason: "send_limited"},
		{name: "provider rejected", setup: func(_ *fakeStore, src *fakeProviderSource) {
			src.p.ScriptSMS(telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"})
		}, body: myNumberBody, status: http.StatusBadGateway, reason: "provider_rejected", stored: true, audited: true},
		{name: "provider unreachable", setup: func(_ *fakeStore, src *fakeProviderSource) {
			src.p.ScriptSMS(telephony.Result{Outcome: telephony.Unreachable, HTTPStatus: 503})
		}, body: myNumberBody, status: http.StatusBadGateway, reason: "provider_unreachable", stored: true, audited: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, src, h := phoneFixture()
			if tc.setup != nil {
				tc.setup(fs, src)
			}
			rec := do(h, phonePerson, http.MethodPut, mePhonePath, tc.body)
			assertReason(t, tc.name, rec, tc.status, tc.reason)
			if stored := fs.phones["u-phone"] != nil; stored != tc.stored {
				t.Fatalf("stored = %v, want %v", stored, tc.stored)
			}
			if audited := len(phoneAudits(fs, "user.phone.set")) == 1; audited != tc.audited {
				t.Fatalf("audited = %v, want %v", audited, tc.audited)
			}
			if !tc.stored && len(src.p.Calls()) != 0 {
				t.Fatalf("a refused write sent %+v", src.p.Calls())
			}
		})
	}
}

func TestPhoneRoutesWithoutTelephonyWiring(t *testing.T) {
	fs, _, _ := phoneFixture()
	h := newHandler(fs)
	if rec := do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody); rec.Code != http.StatusNotImplemented {
		t.Fatalf("put without a provider source = %d, want 501", rec.Code)
	}
	if fs.phones["u-phone"] != nil {
		t.Fatal("a number was stored that no code could be sent to")
	}
}

func TestPutSameVerifiedNumberIsNoOp(t *testing.T) {
	fs, src, h := phoneFixture()
	do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody)
	if rec := do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"482193"}`); rec.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body.String())
	}
	rec := do(h, phonePerson, http.MethodPut, mePhonePath, `{"number":"+905321116869"}`)
	if rec.Code != http.StatusOK || !decodePhone(t, rec.Body.String()).Verified {
		t.Fatalf("re-put = %d %s", rec.Code, rec.Body.String())
	}
	if n := len(src.p.Calls()); n != 1 {
		t.Fatalf("sends = %d, want no code for the no-op", n)
	}
	if n := len(phoneAudits(fs, "user.phone.set")); n != 1 {
		t.Fatalf("set audits = %d, want none for the no-op", n)
	}
}

func TestVerifyMyPhone(t *testing.T) {
	fs, _, h := phoneFixture()
	if rec := do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"482193"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("verify without a phone = %d, want 404", rec.Code)
	}
	do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody)

	rec := do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"111111"}`)
	assertReason(t, "incorrect", rec, http.StatusBadRequest, "code_incorrect")
	rec = do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"12 345"}`)
	assertReason(t, "malformed", rec, http.StatusBadRequest, "code_incorrect")
	fs.phoneVerifyErr = domain.ErrPhoneCodeExpired
	rec = do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"482193"}`)
	assertReason(t, "expired", rec, http.StatusGone, "code_expired")
	fs.phoneVerifyErr = domain.ErrPhoneCodeAttemptsExhausted
	rec = do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"482193"}`)
	assertReason(t, "exhausted", rec, http.StatusGone, "code_attempts_exhausted")
	if n := len(phoneAudits(fs, "user.phone.verify")); n != 0 {
		t.Fatalf("a failed verification was audited (%d)", n)
	}

	fs.phoneVerifyErr = nil
	rec = do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"482193"}`)
	if rec.Code != http.StatusOK || !decodePhone(t, rec.Body.String()).Verified {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body.String())
	}
	assertNoNumber(t, rec.Body.String())
	if a := phoneAudits(fs, "user.phone.verify"); len(a) != 1 || a[0].Target != "user u-phone" {
		t.Fatalf("verify audit = %+v", a)
	}
}

func TestDeleteMyPhone(t *testing.T) {
	fs, _, h := phoneFixture()
	if rec := do(h, phonePerson, http.MethodDelete, mePhonePath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete without a phone = %d, want 404", rec.Code)
	}
	do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody)
	if rec := do(h, phonePerson, http.MethodDelete, mePhonePath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if a := phoneAudits(fs, "user.phone.delete"); len(a) != 1 || a[0].Target != "user u-phone" {
		t.Fatalf("delete audit = %+v", a)
	}
	rec := do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
	if !strings.Contains(rec.Body.String(), `"phone":null`) {
		t.Fatalf("me after delete: %s", rec.Body.String())
	}
}

func TestPatchMyPhonePreferences(t *testing.T) {
	_, _, h := phoneFixture()
	if rec := do(h, phonePerson, http.MethodPatch, mePhonePrefsPath, `{"language":"tr-TR"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("patch without a phone = %d, want 404", rec.Code)
	}
	do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody)

	lang := func(body string) *string {
		t.Helper()
		rec := do(h, phonePerson, http.MethodPatch, mePhonePrefsPath, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %s = %d %s", body, rec.Code, rec.Body.String())
		}
		assertNoNumber(t, rec.Body.String())
		return decodePhone(t, rec.Body.String()).Language
	}
	if l := lang(`{"language":"tr-TR"}`); l == nil || *l != "tr-TR" {
		t.Fatalf("set: %v", l)
	}
	if l := lang(`{}`); l == nil || *l != "tr-TR" {
		t.Fatalf("an absent language changed it: %v", l)
	}
	if l := lang(`{"language":null}`); l != nil {
		t.Fatalf("null did not clear it: %v", *l)
	}
	for _, body := range []string{`{"language":"english"}`, `{"language":""}`, `{"language":7}`,
		`{"quiet_hours":{"start":"22:00"}}`, `{"language":"tr-TR"}{"language":"en"}`} {
		if rec := do(h, phonePerson, http.MethodPatch, mePhonePrefsPath, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("patch %s = %d, want 400", body, rec.Code)
		}
	}
}

func TestTelephonyTestSMS(t *testing.T) {
	fs, src, h := phoneFixture()
	ga := authz.Principal{UserID: "ga", IsGlobalAdmin: true}
	if rec := do(h, o1Admin, http.MethodPost, testSMSPath, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("org admin test-sms = %d, want 403", rec.Code)
	}

	fs.telephonyOn = false
	rec := do(h, ga, http.MethodPost, testSMSPath, "")
	assertReason(t, "telephony off", rec, http.StatusConflict, "telephony_disabled")
	fs.telephonyOn = true

	rec = do(h, ga, http.MethodPost, testSMSPath, "")
	assertReason(t, "no number", rec, http.StatusConflict, "no_verified_number")
	fs.phones = map[string]*fakePhone{"ga": {number: myNumber}}
	rec = do(h, ga, http.MethodPost, testSMSPath, "")
	assertReason(t, "unverified number", rec, http.StatusConflict, "no_verified_number")
	if n := len(src.p.Calls()); n != 0 {
		t.Fatalf("a test SMS reached an unverified number (P1): %d sends", n)
	}

	fs.phones["ga"].verified = true
	src.err = telephony.ErrTelephonyDisabled
	rec = do(h, ga, http.MethodPost, testSMSPath, "")
	assertReason(t, "snapshot off", rec, http.StatusConflict, "telephony_disabled")
	src.err = nil

	src.p.ScriptSMS(
		telephony.Result{Outcome: telephony.Sent, HTTPStatus: 201, Ref: "SMfake"},
		telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"},
		telephony.Result{Outcome: telephony.Unreachable, HTTPStatus: 503},
	)
	for _, want := range []string{
		`{"outcome":"sent","http_status":201,"provider_code":""}`,
		`{"outcome":"rejected","http_status":400,"provider_code":"21211"}`,
		`{"outcome":"unreachable","http_status":503,"provider_code":""}`,
	} {
		rec := do(h, ga, http.MethodPost, testSMSPath, "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("test-sms = %d %s, want %s", rec.Code, rec.Body.String(), want)
		}
	}
	calls := src.p.Calls()
	if len(calls) != 3 || calls[0].To != myNumber || calls[0].Body != telephony.TestMessageText {
		t.Fatalf("calls = %+v", calls)
	}
}

// FR-041a review P3-3 (§4.1): when the provider was not reached, the test SMS's answer carries
// `transport_class` — the class of the failure from deliveryfail's closed set, each of them — and
// never free text: a class outside the set is left out. A provider that answered carries none.
func TestTelephonyTestSMSTransportClass(t *testing.T) {
	fs, src, h := phoneFixture()
	ga := authz.Principal{UserID: "ga", IsGlobalAdmin: true}
	fs.phones = map[string]*fakePhone{"ga": {number: myNumber, verified: true}}

	send := func(res telephony.Result) map[string]json.RawMessage {
		t.Helper()
		src.p.ScriptSMS(res)
		rec := do(h, ga, http.MethodPost, testSMSPath, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("test-sms = %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	classes := deliveryfail.Classes()
	if len(classes) != 8 {
		t.Fatalf("deliveryfail.Classes() = %q; this test expects the eight classes of the classifier", classes)
	}
	for _, class := range classes {
		out := send(telephony.Result{Outcome: telephony.Unreachable, TransportClass: class})
		want, _ := json.Marshal(class)
		if string(out["outcome"]) != `"unreachable"` || string(out["transport_class"]) != string(want) {
			t.Fatalf("unreached provider, class %q: answer %v", class, out)
		}
	}
	for _, res := range []telephony.Result{
		{Outcome: telephony.Sent, HTTPStatus: 201},
		{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"},
		{Outcome: telephony.Unreachable, HTTPStatus: 503},
		{Outcome: telephony.Unreachable, TransportClass: "dial tcp 203.0.113.9:443: free text"},
	} {
		if out := send(res); out["transport_class"] != nil {
			t.Fatalf("answer for %+v carries transport_class %s", res, out["transport_class"])
		}
	}
}
