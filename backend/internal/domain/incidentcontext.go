package domain

import (
	"fmt"
	"strings"
)

// IncidentContextMarker prefixes the one system-authored context update attached
// to an auto-incident; its presence is the idempotency check on re-delivery.
const IncidentContextMarker = "⚡ Context:"

// SuppressionMarker prefixes the one system-authored timeline note explaining
// that a child incident's alerts were muted by a down dependency ancestor.
const SuppressionMarker = "⏸ Suppressed:"

// Probe error classes for the incident-context heuristic. Coarse buckets that a
// human on call actually reasons in; classification is substring-based over the
// raw prober error message plus the HTTP-ish status code.
const (
	ErrClassTimeout    = "timeout"
	ErrClassDNS        = "dns"
	ErrClassRefused    = "connection refused"
	ErrClassTLS        = "tls"
	ErrClassHTTP5xx    = "http-5xx"
	ErrClassHTTP4xx    = "http-4xx"
	ErrClassAssertBody = "condition/assert"
	ErrClassOther      = "other"
)

// ClassifyProbeError buckets one failed heartbeat into an error class. Message
// patterns win over the status code (a TLS failure may still carry code 0), and
// codes catch the plain "server answered badly" case.
func ClassifyProbeError(msg string, code int) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "context deadline exceeded"),
		strings.Contains(m, "timeout"),
		strings.Contains(m, "timed out"):
		return ErrClassTimeout
	case strings.Contains(m, "no such host"),
		strings.Contains(m, "dns"),
		strings.Contains(m, "server misbehaving"):
		return ErrClassDNS
	case strings.Contains(m, "connection refused"),
		strings.Contains(m, "connection reset"),
		strings.Contains(m, "no route to host"),
		strings.Contains(m, "broken pipe"):
		return ErrClassRefused
	case strings.Contains(m, "tls"),
		strings.Contains(m, "certificate"),
		strings.Contains(m, "x509"):
		return ErrClassTLS
	case strings.Contains(m, "assert"),
		strings.Contains(m, "condition"),
		strings.Contains(m, "expected"):
		return ErrClassAssertBody
	case code >= 500:
		return ErrClassHTTP5xx
	case code >= 400:
		return ErrClassHTTP4xx
	default:
		return ErrClassOther
	}
}

// IncidentContext is the heuristic RCA summary attached to an auto-incident:
// what else failed around the same time, how it failed, and where.
type IncidentContext struct {
	// CoFailures: names of OTHER monitors of the project that had failing
	// heartbeats inside the window (capped by the store query).
	CoFailures []string
	// CoFailureTotal is the full distinct count (CoFailures may be truncated).
	CoFailureTotal int
	// DominantClass is the most frequent probe-error class over all failing
	// heartbeats in the window (incident's own monitor included).
	DominantClass string
	// Region is set when every failing monitor sits in the same region.
	Region string
	// RootCause names a down dependency ancestor (filled once the monitor
	// dependency graph lands — iter-0040); empty until then.
	RootCause string
	// WindowMinutes documents the half-window the summary was computed over.
	WindowMinutes int
}

// Empty reports whether the context carries nothing worth posting.
func (c IncidentContext) Empty() bool {
	return c.CoFailureTotal == 0 && c.DominantClass == "" && c.RootCause == ""
}

// Render formats the context as the single system timeline entry. Always
// prefixed with IncidentContextMarker (the idempotency key).
func (c IncidentContext) Render() string {
	var b strings.Builder
	b.WriteString(IncidentContextMarker)
	if c.CoFailureTotal > 0 {
		names := strings.Join(c.CoFailures, ", ")
		if c.CoFailureTotal > len(c.CoFailures) {
			names += ", …"
		}
		fmt.Fprintf(&b, " %d other monitor(s) of this project failed within ±%dm (%s);",
			c.CoFailureTotal, c.WindowMinutes, names)
	} else {
		fmt.Fprintf(&b, " no other monitors of this project failed within ±%dm;", c.WindowMinutes)
	}
	if c.DominantClass != "" {
		fmt.Fprintf(&b, " dominant error: %s;", c.DominantClass)
	}
	if c.Region != "" {
		fmt.Fprintf(&b, " all failures in region %q;", c.Region)
	}
	if c.RootCause != "" {
		fmt.Fprintf(&b, " likely root cause: %s (dependency, down);", c.RootCause)
	}
	return strings.TrimSuffix(b.String(), ";") + "."
}
