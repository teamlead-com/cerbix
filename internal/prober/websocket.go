package prober

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for the Sec-WebSocket-Accept handshake
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// wsAcceptMagic is the RFC 6455 §1.3 GUID appended to the client key to derive
// the expected Sec-WebSocket-Accept value.
const wsAcceptMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// websocketProber performs the WebSocket opening handshake (RFC 6455) over a
// connection dialed through the SSRF guard. Success (no conditions) = the server
// returns 101 Switching Protocols with a Sec-WebSocket-Accept that matches the
// key we sent. Targets may be ws://, wss://, http:// or https:// URLs; wss/https
// runs the handshake over TLS. [STATUS] exposes the HTTP status code.
type websocketProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p websocketProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	// sec-hardening-2 H-2 (S2): the target is parsed again here, before anything is dialed, so a
	// value that bypassed write-time validation never reaches the socket.
	u, err := domain.ParseWebSocketTarget(m.Target)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad target: " + err.Error()}
	}
	secure := u.Scheme == "wss" || u.Scheme == "https"
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if secure {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}

	conn, err := p.dial(ctx, "tcp", addr)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer func() { _ = conn.Close() }()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	if secure {
		// Verified unless the operator opted out with tls_skip_verify (it used to be skipped always).
		tconn := tls.Client(conn, &tls.Config{
			ServerName:         u.Hostname(),
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: m.Config["tls_skip_verify"] == "true", //nolint:gosec // explicit operator opt-in
		})
		if err := tconn.HandshakeContext(ctx); err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "tls: " + err.Error()}
		}
		conn = tconn
	}

	key, err := wsKey()
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	// The handshake is built and serialized by net/http, never by concatenation.
	reqURL := *u
	reqURL.Scheme = "http"
	if secure {
		reqURL.Scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad target: request could not be built"}
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if err := req.Write(conn); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "read handshake: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	lat := elapsedMS(start)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return Result{Connected: false, LatencyMS: lat, Code: resp.StatusCode, Msg: fmt.Sprintf("handshake status %d", resp.StatusCode)}
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wsAccept(key) {
		return Result{Connected: false, LatencyMS: lat, Code: resp.StatusCode, Msg: "bad Sec-WebSocket-Accept"}
	}
	return Result{Connected: true, LatencyMS: lat, Code: resp.StatusCode}
}

// wsKey returns a fresh base64-encoded 16-byte client nonce (Sec-WebSocket-Key).
func wsKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// wsAccept computes the expected Sec-WebSocket-Accept for a given client key.
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsAcceptMagic)) //nolint:gosec // RFC 6455 handshake
	return base64.StdEncoding.EncodeToString(h[:])
}
