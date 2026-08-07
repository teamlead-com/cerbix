package prober

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Guard is an SSRF policy for probe targets. cerbix exists to monitor internal
// services, so private/RFC1918 targets are allowed by default; the dangerous case
// is link-local (169.254.0.0/16, fe80::/10 — cloud instance metadata lives at
// 169.254.169.254), which is blocked by default. Non-routable addresses
// (unspecified, multicast, other non-global-unicast) are always blocked.
//
// The guard checks the *resolved* connect IP (see dialContext), so it holds
// across HTTP redirects and defeats DNS-rebinding: the checked IP is the one
// dialed, not the hostname re-resolved later.
type Guard struct {
	allowPrivate  bool // loopback + RFC1918 + ULA (fc00::/7)
	allowMetadata bool // link-local / cloud instance metadata
}

// NewGuard builds a Guard. allowPrivate defaults on for this internal tool;
// allowMetadata defaults off (block cloud-metadata/link-local).
func NewGuard(allowPrivate, allowMetadata bool) Guard {
	return Guard{allowPrivate: allowPrivate, allowMetadata: allowMetadata}
}

// defaultGuard is the safe default used by NewRunner: private allowed (the
// product monitors internal apps), metadata/link-local blocked.
func defaultGuard() Guard { return Guard{allowPrivate: true, allowMetadata: false} }

// blockedIPError signals a target IP rejected by policy; it carries no network
// side effect — the probe never dialed.
type blockedIPError struct {
	ip     net.IP
	reason string
}

func (e *blockedIPError) Error() string {
	return fmt.Sprintf("blocked target IP %s (%s)", e.ip, e.reason)
}

// metadataIPs are cloud instance-metadata endpoints that must be gated by
// allow_metadata_ips regardless of which range they fall in. 169.254.169.254 is
// also covered by the link-local check, but AWS's IPv6 IMDS (fd00:ec2::254) sits
// in the ULA "private" range, so without an explicit entry allow_private_ips=true
// would let it through.
var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS/GCP/Azure IMDS (IPv4)
	net.ParseIP("fd00:ec2::254"),   // AWS IMDS (IPv6)
}

func isMetadataIP(ip net.IP) bool {
	for _, m := range metadataIPs {
		if m != nil && m.Equal(ip) {
			return true
		}
	}
	return false
}

// cgnatNet is the RFC 6598 carrier-grade-NAT range 100.64.0.0/10. Go's
// net.IP.IsPrivate() does NOT include it and IsGlobalUnicast() reports true, so
// without an explicit check it would be treated as public — yet some clouds expose
// internal/metadata services there. Gated by allow_private_ips like RFC1918.
var _, cgnatNet, _ = net.ParseCIDR("100.64.0.0/10")

func isCGNAT(ip net.IP) bool {
	return cgnatNet != nil && cgnatNet.Contains(ip)
}

// checkIP returns a blockedIPError if the address is disallowed, else nil.
// Order matters: non-routable (unspecified/multicast) is rejected outright, then
// cloud-metadata and loopback/link-local are tested before the broader private
// bucket (so an IPv6 metadata address in ULA space isn't waved through as private).
func (g Guard) checkIP(ip net.IP) error {
	switch {
	case ip.IsUnspecified() || ip.IsMulticast():
		return &blockedIPError{ip, "non-routable"}
	case isMetadataIP(ip):
		if g.allowMetadata {
			return nil
		}
		return &blockedIPError{ip, "cloud-metadata"}
	case ip.IsLoopback():
		if g.allowPrivate {
			return nil
		}
		return &blockedIPError{ip, "loopback"}
	case ip.IsLinkLocalUnicast(): // 169.254.0.0/16, fe80::/10
		if g.allowMetadata {
			return nil
		}
		return &blockedIPError{ip, "link-local/metadata"}
	case ip.IsPrivate(): // RFC1918 + ULA fc00::/7
		if g.allowPrivate {
			return nil
		}
		return &blockedIPError{ip, "private"}
	case isCGNAT(ip): // 100.64.0.0/10 (RFC 6598) — not covered by IsPrivate
		if g.allowPrivate {
			return nil
		}
		return &blockedIPError{ip, "carrier-grade-nat"}
	case !ip.IsGlobalUnicast():
		return &blockedIPError{ip, "non-routable"}
	default:
		return nil // global unicast (public)
	}
}

// dialContext returns a DialContext that resolves the host, rejects it unless at
// least one candidate IP passes the policy, and dials that *checked* IP directly
// (pinned) so no re-resolution can slip a blocked address past the check. Used as
// the HTTP transport dialer (so every redirect hop is guarded) and by the TCP
// prober. Literal-IP targets are checked without a DNS lookup.
func (g Guard) dialContext(base *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip != nil {
			if err := g.checkIP(ip); err != nil {
				return nil, err
			}
			return base.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ipa := range ips {
			if err := g.checkIP(ipa.IP); err != nil {
				lastErr = err
				continue
			}
			conn, err := base.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if err != nil {
				lastErr = err
				continue
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no addresses for %s", host)
		}
		return nil, lastErr
	}
}

// HTTPClient returns an http.Client that applies this guard's egress policy to
// OUTBOUND delivery (webhook/Slack notifications): every connection — including
// redirect targets, since each hop re-dials through the same guarded dialer — is
// resolved, policy-checked, and pinned to the checked IP, so redirect-to-internal
// and DNS-rebinding can't reach loopback/link-local/metadata/(disallowed) private
// hosts. Proxy is disabled (a proxy would bypass the IP guard). Redirect chains are
// capped. The DIALER mechanism is shared with probe targets, but the POLICY is the
// caller's: alert delivery constructs this from the deny-private notification_egress
// policy, while probes use the allow-private prober policy — the two must not be
// conflated (see cli.notificationEgressGuard and D-0141).
func (g Guard) HTTPClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = g.dialContext(&net.Dialer{})
	tr.Proxy = nil
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
}

// EgressDialContext exposes the guarded dialer (resolve → policy check → pinned
// dial) for non-HTTP egress such as SMTP, which has no redirect/Transport hook.
func (g Guard) EgressDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.dialContext(&net.Dialer{})
}

// resolveChecked resolves an ICMP target host to a single allowed IPv4 address,
// or returns a blockedIPError / resolution error. ICMP has no dial step to hook,
// so the prober calls this before sending the echo.
func (g Guard) resolveChecked(ctx context.Context, host string) (*net.IPAddr, error) {
	dst, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		return nil, err
	}
	if err := g.checkIP(dst.IP); err != nil {
		return nil, err
	}
	return dst, nil
}
