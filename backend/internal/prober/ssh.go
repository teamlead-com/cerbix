package prober

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// sshProber checks an SSH server by connecting through the SSRF guard and reading
// the identification banner the server sends on connect (RFC 4253 §4.2, e.g.
// "SSH-2.0-OpenSSH_8.9"). No key exchange or auth is attempted. Success (no
// conditions) = a line starting with "SSH-". The banner is exposed as [BODY] so
// `[BODY] contains OpenSSH` can assert the server software/version.
type sshProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p sshProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	host, port := hostPort(m.Target, 22)
	conn, err := p.dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	// The server may send SSH-layer comment lines before the identification line;
	// the identification is the first line beginning with "SSH-".
	r := bufio.NewReader(conn)
	for i := 0; i < 32; i++ {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no banner: " + err.Error()}
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "SSH-") {
			return Result{Connected: true, LatencyMS: elapsedMS(start), Body: line, Msg: line}
		}
		if err != nil {
			break
		}
	}
	return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no SSH identification banner"}
}
