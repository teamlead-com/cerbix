package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"syscall"

	"github.com/teamlead-com/cerbix/internal/prober"
)

// ErrInvalidURL is a delivery URL the request could not be built from. It names no part of the URL:
// url.Parse's own error quotes the input, secret path and query included (sec-hardening-3 T-4).
var ErrInvalidURL = errors.New("invalid URL")

// DeliveryFailure turns a failed request — one that got no HTTP response — into text that holds no
// part of the delivery URL (sec-hardening-3 T-4, S21). The client's *url.Error quotes the whole URL,
// and that text reached the outbox's last_error, the log and the dead-letter API: a Telegram bot
// token, a Slack webhook path, a tokened webhook URL, all of them write-only in the API.
//
// The reason is a closed set and no library text passes through, so the property holds by
// construction rather than by scrubbing. Both deliverers (notify and webhook) call this one copy; the
// `channel <id>` / `webhook <id>` prefix they add is what tells an operator which one failed.
func DeliveryFailure(err error) error {
	return errors.New("post failed: " + failureReason(err))
}

// failureReason classifies a failed request, in this order on the error chain. Each branch was
// checked against the real chain (sec-hardening-3 §3.6): a TLS alert over TCP is a *net.OpError with
// Op "remote error"/"local error" (tls.AlertError is wrapped only for QUIC), and tls.RecordHeaderError
// is returned by VALUE, so only a value target matches it.
func failureReason(err error) string {
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var dns *net.DNSError
	switch {
	case prober.IsEgressRefusal(err):
		return "destination refused by the delivery egress policy"
	case errors.As(err, &certificate):
		return "certificate not accepted"
	case errors.Is(err, http.ErrSchemeMismatch):
		return "the server does not speak TLS"
	case tlsAlert(err) || errors.As(err, &record):
		return "TLS handshake failed"
	case isTimeout(err):
		return "timeout"
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	default:
		return "connection failed"
	}
}

// tlsAlert reports a TLS alert sent or received over TCP, anywhere in the chain.
func tlsAlert(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if op, ok := e.(*net.OpError); ok && (op.Op == "remote error" || op.Op == "local error") {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
