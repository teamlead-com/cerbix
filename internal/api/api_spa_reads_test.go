package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a revision 7, §11 row "API — what the SPA draws", through the handlers: the recipient's name
// and `only_route_for` on every channel answer, the four new fields of a delivery, the instance's
// voice language on GET /api/v1/me, and POST /api/v1/me/phone/verify/send. The fake models the
// store's contract; the store's own tests prove it against PostgreSQL.

const meVerifySendPath = "/api/v1/me/phone/verify/send"

// PhoneChannelsOnlyRouteFor answers the scripted list of each phone channel among ids, an empty one
// for a phone channel with none scripted, and nothing for the four other types.
func (f *fakeStore) PhoneChannelsOnlyRouteFor(_ context.Context, ids []string) (map[string]domain.OnlyRouteFor, error) {
	if f.onlyRouteErr != nil {
		return nil, f.onlyRouteErr
	}
	out := map[string]domain.OnlyRouteFor{}
	for _, id := range ids {
		ch, ok := f.channels[id]
		if !ok || !domain.IsPhoneChannelType(ch.Type) {
			continue
		}
		o := f.onlyRoute[id]
		if o.Services == nil {
			o.Services = []domain.ServiceWithoutRoute{}
		}
		out[id] = o
	}
	return out, nil
}

func (f *fakeStore) SendUserPhoneCode(_ context.Context, userID string) (string, string, error) {
	if f.phoneSendErr != nil {
		return "", "", f.phoneSendErr
	}
	if f.telephonyNoKey {
		return "", "", domain.ErrPhoneKeyMissing
	}
	p := f.phones[userID]
	switch {
	case p == nil:
		return "", "", domain.ErrPhoneNoNumber
	case p.verified:
		return "", "", domain.ErrPhoneAlreadyVerified
	}
	code := f.phoneCode
	if code == "" {
		code = "482193"
	}
	p.code = code
	return p.number, code, nil
}

func (f *fakeStore) TelephonyVoiceLanguage(context.Context) (string, error) {
	if f.voiceLanguageErr != nil {
		return "", f.voiceLanguageErr
	}
	if f.voiceLanguage == "" {
		return "en-US", nil
	}
	return f.voiceLanguage, nil
}

// TelephonyCanCall reports the scripted "can place calls" (FR-041a revision 9, §4.2).
func (f *fakeStore) TelephonyCanCall(context.Context) (bool, error) {
	if f.canCallErr != nil {
		return false, f.canCallErr
	}
	return f.canCall, nil
}

func decodeObjects(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(body, &list); err == nil {
		return list
	}
	var one map[string]json.RawMessage
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return []map[string]json.RawMessage{one}
}

func byChannelID(t *testing.T, body []byte) map[string]map[string]json.RawMessage {
	t.Helper()
	out := map[string]map[string]json.RawMessage{}
	for _, o := range decodeObjects(t, body) {
		var id string
		_ = json.Unmarshal(o["id"], &id)
		out[id] = o
	}
	return out
}

// §4.3: every channel answer names a `user` channel's recipient — to a viewer too, never by email —
// and carries `only_route_for` with its total on every phone channel; the four other types carry
// neither (P14, TestSlackChannelResponseUnchanged).
func TestPhoneChannelAnswersCarryRecipientNameAndOnlyRouteFor(t *testing.T) {
	fs, _, h := phoneChannelFixture()
	seedPhoneChannel(fs, "ch-bob", domain.ChannelSMS, domain.PhoneRecipientUser, "u-bob", "", false)
	seedPhoneChannel(fs, "ch-num", domain.ChannelVoice, domain.PhoneRecipientNumber, "", myNumber, true)
	seedPhoneChannel(fs, "ch-gone", domain.ChannelSMS, domain.PhoneRecipientUser, "", "", false)
	checkout := domain.ServiceWithoutRoute{Project: "API", Service: "Checkout", Slug: "checkout"}
	fs.onlyRoute = map[string]domain.OnlyRouteFor{"ch-bob": {Services: []domain.ServiceWithoutRoute{checkout}, Total: 3}}

	rec := do(h, pcViewer, http.MethodGet, projectChannelsPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bob@x") {
		t.Fatalf("a viewer's channel list carries an email: %s", rec.Body.String())
	}
	assertNoNumber(t, rec.Body.String())
	got := byChannelID(t, rec.Body.Bytes())
	if string(got["ch-bob"]["recipient_display_name"]) != `"Bob"` {
		t.Fatalf("user channel: recipient_display_name = %s", got["ch-bob"]["recipient_display_name"])
	}
	if string(got["ch-bob"]["only_route_for"]) != `[{"project":"API","service":"Checkout","slug":"checkout"}]` ||
		string(got["ch-bob"]["only_route_for_total"]) != "3" {
		t.Fatalf("user channel: only_route_for = %s, total %s", got["ch-bob"]["only_route_for"], got["ch-bob"]["only_route_for_total"])
	}
	if string(got["ch-num"]["only_route_for"]) != "[]" || string(got["ch-num"]["only_route_for_total"]) != "0" {
		t.Fatalf("number channel: only_route_for = %s, total %s", got["ch-num"]["only_route_for"], got["ch-num"]["only_route_for_total"])
	}
	if _, ok := got["ch-num"]["recipient_display_name"]; ok {
		t.Fatalf("number channel carries recipient_display_name: %v", got["ch-num"])
	}
	if raw, ok := got["ch-gone"]["recipient_display_name"]; !ok || string(raw) != "null" {
		t.Fatalf("a deleted recipient's channel: recipient_display_name = %s (present %v); want null", raw, ok)
	}
	for _, k := range []string{"only_route_for", "only_route_for_total", "recipient_display_name"} {
		if _, ok := got["nc1"][k]; ok {
			t.Fatalf("webhook channel carries %q", k)
		}
	}

	// The monitor's channel list, the create, update and verify answers carry the same.
	fs.monLinks["mon1"] = []string{"ch-bob", "nc1"}
	rec = do(h, pcViewer, http.MethodGet, "/api/v1/monitors/mon1/notifications", "")
	if mon := byChannelID(t, rec.Body.Bytes()); string(mon["ch-bob"]["only_route_for_total"]) != "3" ||
		string(mon["ch-bob"]["recipient_display_name"]) != `"Bob"` {
		t.Fatalf("monitor channels = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, pcProjAdmin, http.MethodPost, projectChannelsPath, userChannelBody("voice", "u-ed"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	created := decodeObjects(t, rec.Body.Bytes())[0]
	if string(created["recipient_display_name"]) != `"Ed"` || string(created["only_route_for"]) != "[]" ||
		string(created["only_route_for_total"]) != "0" {
		t.Fatalf("create answer = %s", rec.Body.String())
	}
	rec = do(h, pcProjAdmin, http.MethodPatch, "/api/v1/notification-channels/ch-bob", `{"name":"bob's pager"}`)
	if upd := decodeObjects(t, rec.Body.Bytes())[0]; rec.Code != http.StatusOK || string(upd["only_route_for_total"]) != "3" ||
		string(upd["recipient_display_name"]) != `"Bob"` {
		t.Fatalf("update answer = %d %s", rec.Code, rec.Body.String())
	}
	seedPhoneChannel(fs, "ch-new", domain.ChannelSMS, domain.PhoneRecipientNumber, "", "+447700900199", false)
	if rec := do(h, pcProjAdmin, http.MethodPost, "/api/v1/notification-channels/ch-new/verify/send", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("verify/send = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, pcProjAdmin, http.MethodPost, "/api/v1/notification-channels/ch-new/verify", `{"code":"482193"}`)
	if v := decodeObjects(t, rec.Body.Bytes())[0]; rec.Code != http.StatusOK || string(v["only_route_for"]) != "[]" {
		t.Fatalf("verify answer = %d %s", rec.Code, rec.Body.String())
	}

	// A failed only_route_for read fails the answer rather than claim a channel guards nothing.
	fs.onlyRouteErr = errors.New("boom")
	if rec := do(h, pcViewer, http.MethodGet, projectChannelsPath, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("list with a failing only_route_for read = %d", rec.Code)
	}
}

// §4.5: a delivery in both reads carries subject, service_state, escalation_step and
// recipient_display_name — null when there is none.
func TestPhoneDeliveriesCarryWhatTheyWereAbout(t *testing.T) {
	fs := phoneReadStore()
	step, state, name := 2, "down", "Bob"
	r := fs.phoneReads()
	r.deliveries[0].Subject = &domain.PhoneDeliverySubject{Kind: domain.PhoneDeliverySubjectService, Name: "Checkout"}
	r.deliveries[0].ServiceState, r.deliveries[0].EscalationStep, r.deliveries[0].RecipientDisplayName = &state, &step, &name
	h := newHandler(fs)
	for _, path := range []string{"/api/v1/incidents/inc1/phone-activity", "/api/v1/notification-channels/nc-sms/phone-deliveries"} {
		rec := do(h, p1Viewer, http.MethodGet, path, "")
		var body struct {
			Deliveries []map[string]json.RawMessage `json:"deliveries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || len(body.Deliveries) == 0 {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		d := body.Deliveries[0]
		if string(d["subject"]) != `{"kind":"service","name":"Checkout"}` || string(d["service_state"]) != `"down"` ||
			string(d["escalation_step"]) != "2" || string(d["recipient_display_name"]) != `"Bob"` {
			t.Fatalf("%s: delivery = %s", path, rec.Body.String())
		}
		for _, other := range body.Deliveries[1:] {
			for _, k := range []string{"subject", "service_state", "escalation_step", "recipient_display_name"} {
				if raw, ok := other[k]; !ok || string(raw) != "null" {
					t.Fatalf("%s: a delivery with no %s answers %s (present %v); want null", path, k, raw, ok)
				}
			}
		}
	}
}

// §4.2: GET /api/v1/me names the instance's voice language beside telephony_enabled.
func TestMeCarriesTelephonyVoiceLanguage(t *testing.T) {
	fs, _, h := phoneFixture()
	fs.voiceLanguage = "tr-TR"
	rec := do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
	var me map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
	}
	if string(me["telephony_voice_language"]) != `"tr-TR"` {
		t.Fatalf("telephony_voice_language = %s", me["telephony_voice_language"])
	}
	// Telephony off does not hide it: it is what "instance default" means.
	fs.telephonyOn = false
	rec = do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if string(me["telephony_voice_language"]) != `"tr-TR"` {
		t.Fatalf("telephony off: telephony_voice_language = %s", me["telephony_voice_language"])
	}
}

// §4.2: POST /api/v1/me/phone/verify/send sends a new code to the STORED unverified number, under the
// limits of the first send; 409 without a number or once verified; 403 for a token or a machine.
func TestSendMyPhoneCodeAgain(t *testing.T) {
	fs, src, h := phoneFixture()
	assertReason(t, "no number", do(h, phonePerson, http.MethodPost, meVerifySendPath, ""), http.StatusConflict, "no_number")
	if n := len(src.p.Calls()); n != 0 {
		t.Fatalf("a refused resend sent %d messages", n)
	}
	if rec := do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody); rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	before := len(fs.audit)
	fs.phoneCode = "555111"
	rec := do(h, phonePerson, http.MethodPost, meVerifySendPath, "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("resend = %d %q", rec.Code, rec.Body.String())
	}
	calls := src.p.Calls()
	if len(calls) != 2 || calls[1].Kind != "sms" || calls[1].To != myNumber || calls[1].Body != telephony.VerificationCodeText("555111") {
		t.Fatalf("provider calls = %+v; want the new code to the stored number", calls)
	}
	if len(fs.audit) != before {
		t.Fatalf("a resend wrote an audit row: %+v", fs.audit[:len(fs.audit)-before])
	}
	if rec := do(h, phonePerson, http.MethodPost, mePhoneVerifyPath, `{"code":"555111"}`); rec.Code != http.StatusOK {
		t.Fatalf("verify the resent code = %d %s", rec.Code, rec.Body.String())
	}
	assertReason(t, "verified", do(h, phonePerson, http.MethodPost, meVerifySendPath, ""), http.StatusConflict, "number_already_verified")

	// The refusals of the first send.
	fs.phones["u-phone"].verified = false
	fs.phoneSendErr = domain.ErrPhoneSendLimited
	assertReason(t, "limited", do(h, phonePerson, http.MethodPost, meVerifySendPath, ""), http.StatusTooManyRequests, "send_limited")
	fs.phoneSendErr = nil
	fs.telephonyOn = false
	assertReason(t, "telephony off", do(h, phonePerson, http.MethodPost, meVerifySendPath, ""), http.StatusConflict, "telephony_disabled")
	fs.telephonyOn = true
	fs.telephonyNoKey = true
	assertReason(t, "no key", do(h, phonePerson, http.MethodPost, meVerifySendPath, ""), http.StatusConflict, "encryption_key_missing")
	fs.telephonyNoKey = false
	src.err = telephony.ErrSecretsUnreadable
	if rec := do(h, phonePerson, http.MethodPost, meVerifySendPath, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreadable secrets = %d", rec.Code)
	}
	src.err = nil
	src.p.ScriptSMS(telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"})
	assertReason(t, "rejected", do(h, phonePerson, http.MethodPost, meVerifySendPath, ""), http.StatusBadGateway, "provider_rejected")
	src.p.ScriptSMS(telephony.Result{Outcome: telephony.Unreachable})
	assertReason(t, "unreachable", do(h, phonePerson, http.MethodPost, meVerifySendPath, ""), http.StatusBadGateway, "provider_unreachable")

	unwired := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).Router()
	if rec := do(unwired, phonePerson, http.MethodPost, meVerifySendPath, ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("without telephony wiring = %d", rec.Code)
	}
	if rec := do(h, tokenEditor, http.MethodPost, meVerifySendPath, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("api token = %d", rec.Code)
	}
	if rec := do(h, phoneMachine, http.MethodPost, meVerifySendPath, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("machine user = %d", rec.Code)
	}
}

// The voice-language read degrades like its siblings: telephony_voice_language null, phone null,
// telephony off, 200 and one log line naming the read — never the error's text.
func TestMeDegradesWhenTheVoiceLanguageReadFails(t *testing.T) {
	fs, src, _ := phoneFixture()
	var logs bytes.Buffer
	h := api.New(fs, slog.New(slog.NewTextHandler(&logs, nil)), 8).WithTelephony(src).Router()
	if rec := do(h, phonePerson, http.MethodPut, mePhonePath, myNumberBody); rec.Code != http.StatusOK {
		t.Fatalf("put = %d", rec.Code)
	}
	logs.Reset()
	fs.voiceLanguageErr = errors.New("store: read failed for " + myNumber)
	rec := do(h, phonePerson, http.MethodGet, "/api/v1/me", "")
	var me map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
	}
	if string(me["telephony_voice_language"]) != "null" || string(me["phone"]) != "null" || string(me["telephony_enabled"]) != "false" {
		t.Fatalf("degraded me = %s", rec.Body.String())
	}
	line := strings.TrimSpace(logs.String())
	if strings.Count(line, "\n") != 0 || !strings.Contains(line, "op=telephony_voice_language") {
		t.Fatalf("want one log line naming the read, got %q", logs.String())
	}
	assertNoNumber(t, line)
}
