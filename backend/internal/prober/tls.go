package prober

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// tlsProber connects to a TLS endpoint (host:port, default 443) through the SSRF
// guard, completes the handshake, and reports the leaf certificate's days-to-expiry
// as [CERT_EXPIRY]. Success (no conditions) = handshake OK and the certificate is
// currently valid. Chain verification is intentionally skipped so internal /
// self-signed certificates are still monitorable — expiry is the signal.
type tlsProber struct {
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	clock func() time.Time
}

func (p tlsProber) Probe(ctx context.Context, m domain.Monitor) Result {
	now := p.clock()
	start := time.Now()
	host, addr := tlsTarget(m.Target)
	conn, err := p.dial(ctx, "tcp", addr)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	tconn := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true}) //nolint:gosec // cert-expiry monitor: inspect the leaf cert without chain verification
	defer tconn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = tconn.SetDeadline(d)
	}
	if err := tconn.HandshakeContext(ctx); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	certs := tconn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no peer certificate"}
	}
	notAfter := certs[0].NotAfter
	days := int64(notAfter.Sub(now).Hours() / 24)
	if now.After(notAfter) {
		return Result{Connected: false, LatencyMS: elapsedMS(start), CertExpiryDays: days, Msg: "certificate expired"}
	}
	return Result{Connected: true, LatencyMS: elapsedMS(start), CertExpiryDays: days}
}

// tlsTarget splits a target into (serverName, host:port), defaulting to port 443
// and tolerating a pasted https:// URL.
func tlsTarget(target string) (host, addr string) {
	target = strings.TrimPrefix(target, "https://")
	target = strings.TrimPrefix(target, "http://")
	target = strings.TrimSuffix(target, "/")
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h, target
	}
	return target, net.JoinHostPort(target, "443")
}
