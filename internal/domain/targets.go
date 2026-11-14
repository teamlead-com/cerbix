package domain

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Probe-target rules (sec-hardening-2 H-2, invariant S2): no byte of a monitor's target or config
// reaches a socket or a URL without passing through that protocol's parser. These run at every
// write surface (Monitor.Validate is the one they all pass through) and again in the prober before
// it dials. The errors never echo the input.

// ParseWebSocketTarget parses a WebSocket monitor's target: a URL whose scheme is ws, wss, http or
// https (wss/https mean TLS), with a host, an optional numeric port, no userinfo, no fragment, and
// no whitespace or control byte anywhere.
func ParseWebSocketTarget(target string) (*url.URL, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return nil, errors.New("websocket target is required")
	}
	if hasControl(target) || hasSpaceOrControl(t) {
		return nil, errors.New("websocket target must not contain whitespace or control characters")
	}
	u, err := url.Parse(t)
	if err != nil {
		return nil, errors.New("websocket target is not a valid URL")
	}
	switch u.Scheme {
	case "ws", "wss", "http", "https":
	default:
		return nil, errors.New("websocket target must start with ws://, wss://, http:// or https://")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return nil, errors.New("websocket target must name a host")
	}
	if u.User != nil {
		return nil, errors.New("websocket target must not carry credentials in its URL userinfo")
	}
	if u.Fragment != "" || strings.Contains(t, "#") {
		return nil, errors.New("websocket target must not carry a fragment")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("websocket target port is empty")
	}
	if p := u.Port(); p != "" && !validPort(p) {
		return nil, errors.New("websocket target port must be a number between 1 and 65535")
	}
	return u, nil
}

// ValidateSocketTarget checks a target that is dialed as `host[:port]` (ssh, redis, rabbitmq AMQP):
// a non-empty host with no whitespace or control byte, and — when present — a numeric port in
// 1..65535. The port is optional because those types default it (owner ruling, 2026-11-14).
func ValidateSocketTarget(target string) error {
	t := strings.TrimSpace(target)
	if t == "" {
		return errors.New("target host is required")
	}
	if hasControl(target) || hasSpaceOrControl(t) {
		return errors.New("target must not contain whitespace or control characters")
	}
	if host, port, err := net.SplitHostPort(t); err == nil {
		if host == "" || strings.ContainsAny(host, "[]/@") {
			return errors.New("target must be host or host:port")
		}
		if !validPort(port) {
			return errors.New("target port must be a number between 1 and 65535")
		}
		return nil
	}
	// No port. A bare IPv6 literal is the one host that legitimately contains colons.
	if strings.Contains(t, ":") && net.ParseIP(t) == nil {
		return errors.New("target must be host or host:port")
	}
	if strings.ContainsAny(t, "[]/@") {
		return errors.New("target must be host or host:port")
	}
	return nil
}

// ValidateRabbitMgmtPath checks the RabbitMQ management `path` setting: empty (meaning
// /api/overview), or a path that starts with exactly one `/` and carries no `@`, backslash,
// whitespace or control byte — so it can never become an authority that moves the request, and
// its Basic-Auth credentials, to another host.
func ValidateRabbitMgmtPath(path string) error {
	if path == "" {
		return nil
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("rabbitmq management path must start with a single /")
	}
	if strings.ContainsAny(path, `@\`) || hasSpaceOrControl(path) {
		return errors.New("rabbitmq management path must not contain @, a backslash, whitespace or control characters")
	}
	_, err := ParseRabbitMgmtPath(path)
	return err
}

// ParseRabbitMgmtPath parses a management path as a URL reference, keeping its percent-encoding
// (`%2F` is RabbitMQ's default vhost). It is refused when it does not parse — including an invalid
// percent sequence in the path or the query — or when it names a scheme, host or userinfo: a
// path must never become an authority. The error never echoes the input.
func ParseRabbitMgmtPath(path string) (*url.URL, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return nil, errors.New("rabbitmq management path is not a valid URL path (check its percent-encoding)")
	}
	if ref.Scheme != "" || ref.Host != "" || ref.User != nil || ref.Opaque != "" {
		return nil, errors.New("rabbitmq management path must be a path, not a URL")
	}
	if _, err := url.ParseQuery(ref.RawQuery); err != nil {
		return nil, errors.New("rabbitmq management path has an invalid query (check its percent-encoding)")
	}
	return ref, nil
}

// ParseHTTPTarget parses an http monitor's target exactly as the prober will send it (the raw value,
// not a trimmed one): it must parse — a parse failure is a refusal, never a skipped check — with an
// http or https scheme (case-insensitive), a host, and a written port in 1..65535. A query and a
// fragment are the operator's. The http prober runs it before it builds a request.
//
// URL userinfo is NOT judged here: it is the D-0145 write rule in Monitor.Validate, and a row stored
// before that rule keeps probing until its next edit. The error never echoes the input.
func ParseHTTPTarget(target string) (*url.URL, error) {
	if hasControl(target) {
		return nil, errors.New("target must not contain control characters")
	}
	u, err := url.Parse(target) // lower-cases the scheme
	if err != nil {
		return nil, errors.New("target is not a valid URL (check its spacing and percent-encoding)")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("target must be an http:// or https:// URL")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return nil, errors.New("target URL must name a host")
	}
	// A written port is 1..65535. An EMPTY one ("host:") is not refused: net/http normalises it
	// to the scheme's default and probes, so such a target works today.
	if p := u.Port(); p != "" && !validPort(p) {
		return nil, errors.New("target port must be a number between 1 and 65535")
	}
	return u, nil
}

// ParsePromQLTarget parses a promql monitor's target, the Prometheus base URL the prober appends
// /api/v1/query to: ParseHTTPTarget's rules, and no query or fragment — either would swallow the
// path cerbix appends. A base path (a reverse-proxy prefix) is kept. Userinfo: see ParseHTTPTarget.
func ParsePromQLTarget(target string) (*url.URL, error) {
	u, err := ParseHTTPTarget(target)
	if err != nil {
		return nil, err
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(target, "?#") {
		return nil, errors.New("promql target must be the Prometheus base URL, without a query or a fragment")
	}
	return u, nil
}

// RabbitMgmtTLS reports whether a RabbitMQ management monitor asked for TLS — the one reading of
// config "tls" the write surfaces and the prober share (absent means no).
func RabbitMgmtTLS(config map[string]string) bool {
	return config["tls"] == "true"
}

// ParseRabbitMgmtTarget parses a management-mode target into the base URL the prober builds on. It
// is the ONE parser behind both Monitor.Validate and the prober (external review of iter-0210), so a
// target gets the same verdict on the write surfaces and before the request:
//   - a control byte anywhere in the RAW value is refused (trimming would hide a trailing CR/LF);
//   - a target without "://" is a socket target (ValidateSocketTarget); its base is
//     http://host:15672, or https://host:15671 with tlsEnabled;
//   - otherwise an http or https URL (scheme case-insensitive) with a non-empty host, no
//     whitespace, no userinfo, no query and no fragment; a port, when written, is 1..65535 — an
//     empty one ("host:") is refused; its path (a reverse-proxy prefix) is kept as escaped;
//   - tlsEnabled demands https.
//
// The error never echoes the input.
func ParseRabbitMgmtTarget(target string, tlsEnabled bool) (*url.URL, error) {
	if hasControl(target) {
		return nil, errors.New("rabbitmq management target must not contain control characters")
	}
	t := strings.TrimSpace(target)
	if t == "" {
		return nil, errors.New("rabbitmq management target is required")
	}
	if !strings.Contains(t, "://") {
		if err := ValidateSocketTarget(t); err != nil {
			return nil, err
		}
		scheme, port := "http", "15672"
		if tlsEnabled {
			scheme, port = "https", "15671"
		}
		host := t
		if h, p, err := net.SplitHostPort(t); err == nil {
			host, port = h, p
		}
		return &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}, nil
	}
	if hasSpaceOrControl(t) {
		return nil, errors.New("rabbitmq management target must not contain whitespace or control characters")
	}
	u, err := url.Parse(t) // lower-cases the scheme
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Hostname() == "" {
		return nil, errors.New("rabbitmq management target must be an http:// or https:// URL, or host[:port]")
	}
	if u.User != nil {
		return nil, errors.New("rabbitmq management target must not carry credentials in its URL userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(t, "?#") {
		return nil, errors.New("rabbitmq management target must not carry a query or a fragment; put the query in the path setting")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("rabbitmq management target port is empty")
	}
	if p := u.Port(); p != "" && !validPort(p) {
		return nil, errors.New("rabbitmq management target port must be a number between 1 and 65535")
	}
	if tlsEnabled && u.Scheme != "https" {
		return nil, errors.New("rabbitmq management target must use https when tls is enabled")
	}
	return u, nil
}

// validateProbeTarget applies the per-protocol rule to a monitor's target and the config that
// shapes a URL. Every type that probes a target is refused a control byte in the raw value first
// (Monitor.Validate); the types listed here are also parsed by their protocol's parser.
//
// The types NOT listed (tcp, icmp, dns, tls, grpc, postgres, mysql) do reach the network with
// their target, but only as an address handed to a library that parses it — net.Dial, the
// resolver, the gRPC client, the database drivers. No prober writes their target into a request
// line, a header or a URL by hand, which is what the listed parsers exist to stop (S2).
func validateProbeTarget(m Monitor) error {
	switch m.Type {
	case MonitorHTTP:
		_, err := ParseHTTPTarget(m.Target)
		return err
	case MonitorPromQL:
		_, err := ParsePromQLTarget(m.Target)
		return err
	case MonitorWebSocket:
		_, err := ParseWebSocketTarget(m.Target)
		return err
	case MonitorSSH, MonitorRedis:
		return ValidateSocketTarget(m.Target)
	case MonitorRabbitMQ:
		if strings.EqualFold(m.Config["mode"], "management") {
			if _, err := ParseRabbitMgmtTarget(m.Target, RabbitMgmtTLS(m.Config)); err != nil {
				return err
			}
			return ValidateRabbitMgmtPath(m.Config["path"])
		}
		return ValidateSocketTarget(m.Target)
	}
	return nil
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == p
}

// hasControl reports a control byte anywhere — checked on the RAW value, because trimming would
// silently drop a trailing CR/LF that an attacker put there.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func hasSpaceOrControl(s string) bool {
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
