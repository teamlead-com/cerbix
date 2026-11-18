package telephony

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// TwilioBaseURL is the API root a new Twilio provider talks to. Tests point a provider at an
// httptest server instead; production never changes it.
var TwilioBaseURL = "https://api.twilio.com"

// TwilioConfig is the instance's twilio block (§4.1). From is an E.164 number or a Messaging Service
// SID (`MG…`, SMS only — R14).
type TwilioConfig struct {
	AccountSID string
	AuthToken  string
	From       string
}

// Twilio sends SMS through the Messages resource and places calls through the Calls resource with
// the TwiML inline (§5.1). Part (a): no StatusCallback, no Gather.
type Twilio struct {
	cfg     TwilioConfig
	client  *http.Client
	baseURL string
}

// NewTwilio builds the provider. client is the notification egress guard's client in production; nil
// gets a fallback that refuses redirects the same way.
func NewTwilio(cfg TwilioConfig, client *http.Client) *Twilio {
	return &Twilio{cfg: cfg, client: defaultClient(client), baseURL: TwilioBaseURL}
}

func (t *Twilio) Name() string { return "twilio" }

// SupportsVoice is false while `from` is a Messaging Service SID, which sends messages only (D3): the
// provider-side face of the database's telephony_can_place_calls.
func (t *Twilio) SupportsVoice() bool { return !t.fromIsMessagingService() }

func (t *Twilio) fromIsMessagingService() bool { return strings.HasPrefix(t.cfg.From, "MG") }

// SendSMS posts To, Body and either From or MessagingServiceSid.
func (t *Twilio) SendSMS(ctx context.Context, to, text string) Result {
	form := url.Values{"To": {to}, "Body": {text}}
	if t.fromIsMessagingService() {
		form.Set("MessagingServiceSid", t.cfg.From)
	} else {
		form.Set("From", t.cfg.From)
	}
	return t.post(ctx, "Messages.json", form)
}

// PlaceCall posts To, From and the TwiML inline. A Messaging Service SID cannot call: rejected
// without a request (R14).
func (t *Twilio) PlaceCall(ctx context.Context, to, twiml string) Result {
	if t.fromIsMessagingService() {
		return Result{Outcome: Rejected, Code: CodeVoiceNeedsFromNumber}
	}
	return t.post(ctx, "Calls.json", url.Values{"To": {to}, "From": {t.cfg.From}, "Twiml": {twiml}})
}

// twilioAnswer is the part of a Twilio answer that is read: the resource's sid on success, the
// numeric error code on a refusal. message and more_info are never decoded.
type twilioAnswer struct {
	SID  string          `json:"sid"`
	Code json.RawMessage `json:"code"`
}

func (t *Twilio) post(ctx context.Context, resource string, form url.Values) Result {
	endpoint := strings.TrimRight(t.baseURL, "/") + "/2010-04-01/Accounts/" +
		url.PathEscape(t.cfg.AccountSID) + "/" + resource
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		// Only a malformed base URL gets here; its text is not kept.
		return Result{Outcome: Rejected}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(t.cfg.AccountSID, t.cfg.AuthToken)

	res, body := do(t.client, req)
	if res.HTTPStatus == 0 {
		return res
	}
	var ans twilioAnswer
	if json.Unmarshal(body, &ans) != nil {
		return res
	}
	if res.Outcome == Sent {
		if validRef(ans.SID) {
			res.Ref = ans.SID
		}
		return res
	}
	res.Code = numericCode(ans.Code)
	return res
}

// numericCode keeps a JSON integer as written, or nothing.
func numericCode(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return ""
		}
	}
	if s := string(raw); validCode(s) {
		return s
	}
	return ""
}
