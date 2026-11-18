package telephony

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/teamlead-com/cerbix/internal/deliveryfail"
	"github.com/teamlead-com/cerbix/internal/prober"
)

// Outcome is how a provider answered one request (§4.5). Its values are the delivery reasons they
// become on the phone_deliveries row.
type Outcome string

const (
	// Sent: the provider accepted the request (HTTP 2xx).
	Sent Outcome = "sent"
	// Unreachable: no usable answer — 408, 429, 5xx, a transport error or the context's deadline.
	// The outbox retries.
	Unreachable Outcome = "provider_unreachable"
	// Rejected: the provider answered no — any other status, a 3xx included (D14). Final.
	Rejected Outcome = "provider_rejected"
)

// Result is everything cerbix keeps of a provider's answer (D34): the status, a short code, the
// provider's reference of the message or call, and — for a request that got no answer — the closed
// transport class of notify.DeliveryFailure. Never a response body, never library error text: both
// can quote the destination number or the request URL.
type Result struct {
	Outcome    Outcome
	HTTPStatus int // 0 when there was no HTTP answer
	// Code matches ^[A-Za-z0-9_.-]{1,32}$ or is empty: a provider's error code, or one of the Code*
	// constants for a refusal decided without a request.
	Code string
	// Ref is the provider's id of the accepted message or call (Twilio's sid), or empty.
	Ref string
	// TransportClass is deliveryfail.Class of a request that got no HTTP answer, else empty.
	TransportClass string
}

// Provider sends phone alerts (D3). Implementations never return an error: every failure is a
// Result, classified per §4.5. The caller bounds each call with ctx (the D33 budget).
type Provider interface {
	SendSMS(ctx context.Context, to, text string) Result
	PlaceCall(ctx context.Context, to, twiml string) Result
	SupportsVoice() bool
	Name() string
}

// Codes of refusals decided before any request is made.
const (
	// CodeVoiceUnsupported: the provider cannot place calls (generic_http, D3).
	CodeVoiceUnsupported = "voice_unsupported"
	// CodeVoiceNeedsFromNumber: Twilio's `from` is a Messaging Service SID, which can send SMS but
	// not place a call (R14).
	CodeVoiceNeedsFromNumber = "voice_needs_from_number"
)

// maxResponseBody bounds what is read of a provider's answer to find its code or reference.
const maxResponseBody = 64 << 10

var (
	codePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	// refPattern bounds a provider reference the same way, with room for a 34-character Twilio sid.
	refPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// classifyStatus is §4.5's table for an HTTP answer.
func classifyStatus(status int) Outcome {
	switch {
	case status >= 200 && status <= 299:
		return Sent
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests,
		status >= 500 && status <= 599:
		return Unreachable
	default:
		return Rejected
	}
}

// validCode reports whether s may be kept as a provider code (D34).
func validCode(s string) bool { return codePattern.MatchString(s) }

func validRef(s string) bool { return refPattern.MatchString(s) }

// defaultClient is the fallback for a nil client: it refuses redirects exactly as the egress guard's
// client does (sec-hardening-3 S17), so a caller that passes nil never gets a laxer client than
// production — notify.New's fallback, repeated.
func defaultClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: prober.RefuseRedirects}
}

// do sends req and returns the classified answer with a bounded copy of its body for the caller to
// pick a code or a reference from. The body never leaves the provider.
//
// With the egress client a 3xx is not followed (prober.RefuseRedirects answers
// http.ErrUseLastResponse), so it arrives here as the response it is and is Rejected with its status.
func do(client *http.Client, req *http.Request) (Result, []byte) {
	resp, err := client.Do(req)
	if err != nil {
		return Result{Outcome: Unreachable, TransportClass: deliveryfail.Class(err)}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return Result{Outcome: classifyStatus(resp.StatusCode), HTTPStatus: resp.StatusCode}, body
}
