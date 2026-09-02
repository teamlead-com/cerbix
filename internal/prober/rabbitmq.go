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
	u, err := rabbitMgmtURL(m.Target, m.Config["path"], m.Config["tls"] == "true")
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad request: " + err.Error()}
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
		client = &http.Client{Transport: transport, Timeout: p.client.Timeout}
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
func rabbitMgmtURL(target, path string, tlsEnabled bool) (string, error) {
	target = strings.TrimSpace(target)
	if path == "" {
		path = "/api/overview"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		if err != nil {
			return "", err
		}
		if tlsEnabled && u.Scheme != "https" {
			return "", fmt.Errorf("rabbitmq management target must use https when tls is enabled")
		}
		return strings.TrimRight(target, "/") + path, nil
	}
	defaultPort := uint16(15672)
	scheme := "http"
	if tlsEnabled {
		defaultPort = 15671
		scheme = "https"
	}
	host, port := hostPort(target, defaultPort)
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(int(port))) + path, nil
}
