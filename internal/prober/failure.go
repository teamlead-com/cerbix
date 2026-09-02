package prober

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// probeFailure renders a transport failure for `heartbeat.msg` WITHOUT echoing the request
// URL. It exists because `net/http` embeds the full URL in every error it returns, so the
// obvious `Msg: err.Error()` publishes whatever the target's query string carries — a
// `?token=…` lands in a stored, UI-rendered field and in the incident body derived from it.
// Reproduced on an http, a promql and a synthetic monitor before this helper existed.
//
// The rule is structural, not a guess about what looks secret: the message is COMPOSED from
// a classification of the error and a host this code took from the URL itself, and any
// unclassified text that still contains a scheme separator is dropped rather than trimmed.
// A pattern that tried to spot credentials inside prose would be both bypassable and the
// kind of guessing this repository refuses.
//
// `requested` is the URL the prober asked for; only its HOST reaches the message. A host is
// not a credential — it is already visible in the monitor's own target to anyone who can
// read the monitor — and without it a failure is much harder to diagnose.
func probeFailure(err error, requested string) string {
	class := failureClass(err)
	if host := hostOf(requested); host != "" {
		return class + " (" + host + ")"
	}
	return class
}

// failureClass names the cause in bounded vocabulary. Unwraps `*url.Error` first: its own
// Error() is the URL-bearing one, and the cause underneath is what an operator needs.
func failureClass(err error) string {
	if err == nil {
		return "unknown error"
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		if ue.Timeout() {
			return "timeout"
		}
		err = ue.Err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return "dns: no such host"
		}
		if dns.IsTimeout {
			return "dns: timeout"
		}
		return "dns: lookup failed"
	}
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return "tls: certificate not verified"
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return "tls: certificate does not match host"
	}
	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return "tls: unknown certificate authority"
	}
	var expired x509.CertificateInvalidError
	if errors.As(err, &expired) {
		return "tls: certificate invalid"
	}
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &recordHeader) {
		return "tls: not a TLS server"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connect: connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connect: connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "connect: host unreachable"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed by peer"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	// Unclassified. The text may still name a URL — a redirect chain reports the NEXT one, a
	// proxy failure its own — so anything carrying a scheme separator is refused outright
	// instead of edited: an invariant enforced beats a pattern trusted.
	text := strings.TrimSpace(err.Error())
	if text == "" || strings.Contains(text, "://") {
		return "request failed"
	}
	return text
}

// hostOf returns the host[:port] of a URL, or "" when there is nothing parseable. Only the
// authority is taken: never the path, the query or the userinfo.
func hostOf(requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ""
	}
	u, err := url.Parse(requested)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
