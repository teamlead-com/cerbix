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
	"strings"
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
	secure, host, addr, path := wsTarget(m.Target)

	conn, err := p.dial(ctx, "tcp", addr)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	if secure {
		tconn := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true}) //nolint:gosec // liveness handshake; internal/self-signed endpoints must stay monitorable
		if err := tconn.HandshakeContext(ctx); err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "tls: " + err.Error()}
		}
		conn = tconn
	}

	key, err := wsKey()
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "read handshake: " + err.Error()}
	}
	defer resp.Body.Close()
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

// wsTarget parses a WebSocket target into (secure, hostname, host:port, path),
// defaulting to port 80 (ws) / 443 (wss) and path "/".
func wsTarget(target string) (secure bool, host, addr, path string) {
	target = strings.TrimSpace(target)
	rest := target
	switch {
	case strings.HasPrefix(rest, "wss://"):
		secure, rest = true, rest[len("wss://"):]
	case strings.HasPrefix(rest, "https://"):
		secure, rest = true, rest[len("https://"):]
	case strings.HasPrefix(rest, "ws://"):
		rest = rest[len("ws://"):]
	case strings.HasPrefix(rest, "http://"):
		rest = rest[len("http://"):]
	}
	hostPortPart := rest
	path = "/"
	if i := strings.IndexAny(rest, "/"); i >= 0 {
		hostPortPart = rest[:i]
		path = rest[i:]
	}
	host = hostPortPart
	if h, _, err := net.SplitHostPort(hostPortPart); err == nil {
		host = h
		addr = hostPortPart
	} else if secure {
		addr = net.JoinHostPort(hostPortPart, "443")
	} else {
		addr = net.JoinHostPort(hostPortPart, "80")
	}
	return secure, host, addr, path
}
