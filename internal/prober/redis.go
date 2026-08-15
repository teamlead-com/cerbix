package prober

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// redisProber checks a Redis server by AUTH-ing (when a password is set) and
// PING-ing, over a connection dialed through the SSRF guard. Success = PONG.
// Config keys: password (encrypted at rest), username (optional, for ACL AUTH).
type redisProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p redisProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	host, port := hostPort(m.Target, 6379)
	conn, err := p.dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer func() { _ = conn.Close() }()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	if m.Config["tls"] == "true" {
		tlsConn := tls.Client(conn, &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         host,
			InsecureSkipVerify: m.Config["tls_skip_verify"] == "true", //nolint:gosec // explicit operator opt-in
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
		}
		conn = tlsConn
	}
	r := bufio.NewReader(conn)

	if pw := m.Config["password"]; pw != "" {
		args := []string{"AUTH", pw}
		if user := m.Config["username"]; user != "" {
			args = []string{"AUTH", user, pw}
		}
		if _, err := conn.Write(respCmd(args...)); err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
		}
		if !strings.HasPrefix(line, "+") {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "auth failed: " + strings.TrimSpace(line)}
		}
	}

	if _, err := conn.Write(respCmd("PING")); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	if !strings.HasPrefix(line, "+PONG") {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "unexpected PING reply: " + strings.TrimSpace(line)}
	}
	return Result{Connected: true, LatencyMS: elapsedMS(start)}
}

// respCmd encodes a Redis command as a RESP array of bulk strings (handles any
// argument bytes, unlike inline commands).
func respCmd(args ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}
