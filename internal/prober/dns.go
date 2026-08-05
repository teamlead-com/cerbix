package prober

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// dnsProber resolves a hostname's A/AAAA records. Success (no conditions) = at
// least one address resolves. The resolved addresses are exposed as the body (so
// `[BODY] contains "10.0."` can assert an expected value) and the resolve time as
// [RESPONSE_TIME].
type dnsProber struct{ resolver *net.Resolver }

func (p dnsProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	host := dnsHost(m.Target)
	addrs, err := p.resolver.LookupHost(ctx, host)
	lat := elapsedMS(start)
	if err != nil {
		return Result{Connected: false, LatencyMS: lat, Msg: err.Error()}
	}
	if len(addrs) == 0 {
		return Result{Connected: false, LatencyMS: lat, Msg: "no records"}
	}
	return Result{Connected: true, LatencyMS: lat, Body: strings.Join(addrs, ", ")}
}

// dnsHost tolerates a pasted URL by stripping a scheme and trailing slash.
func dnsHost(target string) string {
	target = strings.TrimPrefix(target, "https://")
	target = strings.TrimPrefix(target, "http://")
	return strings.TrimSuffix(target, "/")
}
