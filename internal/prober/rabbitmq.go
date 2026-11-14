package prober

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// amqpProtocolHeader is the AMQP 0-9-1 protocol header a client sends on connect.
// A live broker replies with a Connection.Start method frame; on a version
// mismatch it replies with its own protocol header and closes — either way it
// proves a broker is listening.
var amqpProtocolHeader = []byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}

// rabbitmqProber checks a RabbitMQ broker in one of two modes (config "mode"):
//
//   - "amqp" (default): open a TCP connection through the SSRF guard, send the
//     AMQP protocol header, and confirm the broker starts the handshake. No
//     credentials are needed. Target is host:port (default 5672).
//   - "management": GET the management HTTP API (through the guarded HTTP client)
//     with basic auth; success (no conditions) = a 2xx. Config: username,
//     password (encrypted at rest), path (default /api/overview). Target is the
//     management base URL (or host, defaulting to port 15672). [STATUS]/[BODY]
//     conditions can assert on the JSON (e.g. queue/consumer/memory checks).
type rabbitmqProber struct {
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	client *http.Client
}

func (p rabbitmqProber) Probe(ctx context.Context, m domain.Monitor) Result {
	if strings.EqualFold(m.Config["mode"], "management") {
		return p.probeManagement(ctx, m)
	}
	return p.probeAMQP(ctx, m)
}

func (p rabbitmqProber) probeAMQP(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	// sec-hardening-2 H-2 (S2): the target is checked again before anything is dialed.
	if err := domain.ValidateSocketTarget(m.Target); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad target: " + err.Error()}
	}
	host, port := hostPort(m.Target, 5672)
	conn, err := p.dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer func() { _ = conn.Close() }()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	if _, err := conn.Write(amqpProtocolHeader); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	// One byte is enough to tell a broker from a dead port: 0x01 is a METHOD frame
	// (Connection.Start), 'A' is the broker echoing its protocol header on a
	// version mismatch. Anything else is not AMQP.
	first := make([]byte, 1)
	if _, err := io.ReadFull(conn, first); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no AMQP reply: " + err.Error()}
	}
	if first[0] != 0x01 && first[0] != 'A' {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: fmt.Sprintf("unexpected AMQP reply byte 0x%02x", first[0])}
	}
	return Result{Connected: true, LatencyMS: elapsedMS(start)}
}

func (p rabbitmqProber) probeManagement(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	u, err := rabbitMgmtURL(m.Target, m.Config["path"], domain.RabbitMgmtTLS(m.Config))
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad target: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: msgRequestNotBuilt}
	}
	if user := m.Config["username"]; user != "" {
		req.SetBasicAuth(user, m.Config["password"])
	}
	client := p.client
	if m.Config["tls_skip_verify"] == "true" {
		base, ok := p.client.Transport.(*http.Transport)
		if !ok {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "TLS skip-verify requires HTTP transport"}
		}
		transport := base.Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // explicit operator opt-in
		// The redirect rule travels with the client (sec-hardening-2 H-13): this branch used to drop it.
		client = &http.Client{Transport: transport, Timeout: p.client.Timeout, CheckRedirect: p.client.CheckRedirect}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The management URL is built from the operator's target and may carry a query; the
		// invariant is that no probe result contains a request URL, whatever the type.
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: probeFailure(err, u)}
	}
	defer func() { _ = resp.Body.Close() }()
	lat := elapsedMS(start)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	res := Result{Connected: true, LatencyMS: lat, Code: resp.StatusCode, Body: string(body)}
	// Mirror HTTP semantics: with no conditions, success is a 2xx; conditions (if
	// any) get the raw status/body to assert on (e.g. [STATUS] or [BODY] checks).
	if len(m.Conditions) == 0 && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		res.Connected = false
		res.Msg = fmt.Sprintf("management status %d", resp.StatusCode)
	}
	return res
}

// rabbitMgmtURL builds the management API URL from a target (a base URL, or a
// bare host defaulting to port 15672) and a path (default /api/overview).
//
// sec-hardening-2 H-2 (S2): the path is checked by domain.ValidateRabbitMgmtPath and the URL is
// assembled as a url.URL, never by concatenation, so `path` can never become an authority that
// moves the request — and its Basic-Auth credentials — to another host. The path starts with a
// single "/"; a base path of a URL target (a reverse-proxy prefix) is kept.
//
// The target is parsed by domain.ParseRabbitMgmtTarget, the same parser Monitor.Validate runs, so
// the prober and the write surfaces give one verdict (external review of iter-0210). It gets the
// RAW target: trimming first would hide a trailing CR/LF.
//
// The path is a URL REFERENCE and its percent-encoding is part of what the operator configured:
// `%2F` is RabbitMQ's default vhost. So the URL carries both Path and RawPath, joined by
// joinURLPath, and the reference's own RawQuery. Building from the decoded Path alone re-encoded
// `%2F` as `%252F` (review of iter-0210).
func rabbitMgmtURL(target, path string, tlsEnabled bool) (string, error) {
	if path == "" {
		path = "/api/overview"
	}
	// A relative path is refused, not given a "/" (external review of iter-0210, P2-1): the rule is
	// the one the write surfaces apply, and migration 00114 wrote the "/" into existing rows.
	if err := domain.ValidateRabbitMgmtPath(path); err != nil {
		return "", err
	}
	ref, err := domain.ParseRabbitMgmtPath(path)
	if err != nil {
		return "", err
	}
	base, err := domain.ParseRabbitMgmtTarget(target, tlsEnabled)
	if err != nil {
		return "", err
	}
	out := url.URL{Scheme: base.Scheme, Host: base.Host, RawQuery: ref.RawQuery}
	out.Path, out.RawPath = joinURLPath(base, ref)
	return out.String(), nil
}
