package prober

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sshProber checks an SSH server by connecting through the SSRF guard and reading
// the identification banner the server sends on connect (RFC 4253 §4.2, e.g.
// "SSH-2.0-OpenSSH_8.9"). No key exchange or auth is attempted. Success (no
// conditions) = a line starting with "SSH-". The banner is exposed as [BODY] so
// `[BODY] contains OpenSSH` can assert the server software/version.
type sshProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// The bounds on what a prober reads from a probed peer (sec-hardening-2 H-8, S8).
const (
	// peerLineReadLimit bounds a line-protocol exchange (the ssh banner and the comment lines
	// before it — RFC 4253 caps each at 255 bytes; the redis AUTH and PING replies).
	peerLineReadLimit = 4 << 10
	// wsHandshakeReadLimit bounds the WebSocket handshake response: status line and headers.
	wsHandshakeReadLimit = 16 << 10
	// dbConnReadLimit bounds everything the postgres and mysql probers read from one connection:
	// TLS, authentication and the probe query's answer (review of iter-0212, P2-1). The drivers'
	// own framing does not — pgx accepts a ~1 GiB message, the MySQL driver chains 16 MiB packets.
	dbConnReadLimit = 1 << 20
)

func (p sshProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	// sec-hardening-2 H-2 (S2): the target is checked again before anything is dialed.
	if err := domain.ValidateSocketTarget(m.Target); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad target: " + err.Error()}
	}
	host, port := hostPort(m.Target, 22)
	conn, err := p.dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer func() { _ = conn.Close() }()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	// The server may send SSH-layer comment lines before the identification line;
	// the identification is the first line beginning with "SSH-".
	// Every byte read from the peer is bounded (sec-hardening-2 H-8, S8): a peer that streams
	// without a newline ends the probe at the limit, not at its deadline.
	r := bufio.NewReader(io.LimitReader(conn, peerLineReadLimit))
	// Only a COMPLETE line counts (RFC 4253 §4.2 ends the identification with CR LF): a line the
	// read bound cut short, or one the peer abandoned by closing, is no banner whatever it starts
	// with — otherwise a flood beginning "SSH-" would turn the bound into an UP (review of
	// iter-0212, P1-2).
	for i := 0; i < 32; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			if line == "" {
				return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no banner: " + err.Error()}
			}
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no SSH identification banner: incomplete line"}
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "SSH-") {
			return Result{Connected: true, LatencyMS: elapsedMS(start), Body: line, Msg: line}
		}
	}
	return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no SSH identification banner"}
}
