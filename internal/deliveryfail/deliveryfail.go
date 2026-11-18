// Package deliveryfail names why an outbound delivery request got no HTTP response, from a closed set
// of reasons that quote no part of the request (sec-hardening-3 T-4, S21).
//
// It is a leaf package so that every deliverer can share one copy: notify (alert channels, webhooks
// through notify.DeliveryFailure) and telephony (phone providers, FR-041a D34), which notify itself
// imports.
package deliveryfail

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"syscall"

	"github.com/teamlead-com/cerbix/internal/prober"
)

// The classes Class returns: the whole closed set.
const (
	ClassEgressRefused   = "destination refused by the delivery egress policy"
	ClassCertificate     = "certificate not accepted"
	ClassNotTLS          = "the server does not speak TLS"
	ClassTLSHandshake    = "TLS handshake failed"
	ClassTimeout         = "timeout"
	ClassDNS             = "DNS lookup failed"
	ClassRefused         = "connection refused"
	ClassConnectionFails = "connection failed"
)

// Classes is every value Class can return, in the order Class tests them. A caller that reports a
// class (the test SMS's transport_class, FR-041a §4.1) admits only these.
func Classes() []string {
	return []string{ClassEgressRefused, ClassCertificate, ClassNotTLS, ClassTLSHandshake, ClassTimeout,
		ClassDNS, ClassRefused, ClassConnectionFails}
}

// IsClass reports whether s is one of Classes.
func IsClass(s string) bool {
	for _, c := range Classes() {
		if s == c {
			return true
		}
	}
	return false
}

// Class classifies a failed request, in this order on the error chain. Each branch was checked
// against the real chain (sec-hardening-3 §3.6): a TLS alert over TCP is a *net.OpError with Op
// "remote error"/"local error" (tls.AlertError is wrapped only for QUIC), and tls.RecordHeaderError
// is returned by VALUE, so only a value target matches it.
//
// The reason is a closed set and no library text passes through, so the property "names no URL"
// holds by construction rather than by scrubbing.
func Class(err error) string {
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var dns *net.DNSError
	switch {
	case prober.IsEgressRefusal(err):
		return ClassEgressRefused
	case errors.As(err, &certificate):
		return ClassCertificate
	case errors.Is(err, http.ErrSchemeMismatch):
		return ClassNotTLS
	case tlsAlert(err) || errors.As(err, &record):
		return ClassTLSHandshake
	case isTimeout(err):
		return ClassTimeout
	case errors.As(err, &dns):
		return ClassDNS
	case errors.Is(err, syscall.ECONNREFUSED):
		return ClassRefused
	default:
		return ClassConnectionFails
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
