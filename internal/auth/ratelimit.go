package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiterSweepInterval is how often allow() opportunistically drops keys with no
// in-window hits, so the map can't grow unbounded from (spoofable or churning)
// client IPs — a memory-DoS vector otherwise.
const limiterSweepInterval = 5 * time.Minute

// loginLimiter is a per-key sliding-window rate limiter (brute-force mitigation
// for local login). It keeps recent attempt timestamps per key in memory and
// periodically evicts idle keys.
type loginLimiter struct {
	mu        sync.Mutex
	hits      map[string][]time.Time
	limit     int
	window    time.Duration
	lastSweep time.Time
}

// newLoginLimiter builds a limiter of at most perMinute attempts per key per
// minute. perMinute <= 0 disables limiting.
func newLoginLimiter(perMinute int) *loginLimiter {
	return &loginLimiter{
		hits:   map[string][]time.Time{},
		limit:  perMinute,
		window: time.Minute,
	}
}

// allow records an attempt for key at now and reports whether it is within the
// limit. Disabled limiters always allow.
func (l *loginLimiter) allow(key string, now time.Time) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// sweep drops keys whose newest attempt is older than the window, bounding the
// map to keys seen within the last window. Runs at most every sweep interval;
// caller holds the lock.
func (l *loginLimiter) sweep(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < limiterSweepInterval {
		return
	}
	l.lastSweep = now
	cutoff := now.Add(-l.window)
	for k, ts := range l.hits {
		if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
			delete(l.hits, k)
		}
	}
}

// clientIP extracts the caller's IP for rate-limiting, resistant to spoofing.
//
// When trustedNets is non-empty (server.trusted_proxy_cidrs is set) the CIDR-trust
// model is used and it SUPERSEDES trustedProxies: X-Forwarded-For is honored only
// when the direct peer is inside a trusted network, then the (XFF ++ peer) chain is
// walked right-to-left skipping addresses that are themselves trusted proxies — the
// first untrusted address is the client. A request whose direct peer is not trusted
// (e.g. one reaching the origin directly, bypassing the proxy) has its XFF ignored
// entirely, so it can't forge a fresh limiter bucket.
//
// When trustedNets is empty the legacy hop-count model applies: trustedProxies is how
// many reverse-proxy hops sit in front of cerbix; the client is that many entries from
// the right of the chain. With 0 trusted hops, X-Forwarded-For is ignored entirely.
func clientIP(r *http.Request, trustedProxies int, trustedNets []*net.IPNet) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if len(trustedNets) > 0 {
		return clientIPByCIDR(r, peer, trustedNets)
	}
	if trustedProxies <= 0 {
		return peer
	}
	// Full left-to-right chain: XFF entries (each appended by a hop) then the peer.
	chain := forwardChain(r, peer)
	// The rightmost trustedProxies entries are our own proxies; the one just before
	// them is the real client. Clamp to the leftmost entry if the chain is short
	// (fewer hops present than configured — never index past the client).
	idx := len(chain) - 1 - trustedProxies
	if idx < 0 {
		idx = 0
	}
	return chain[idx]
}

// clientIPByCIDR resolves the client under the CIDR-trust model (see clientIP).
func clientIPByCIDR(r *http.Request, peer string, trustedNets []*net.IPNet) string {
	if !ipInAny(peer, trustedNets) {
		// Direct peer is not one of our proxies — do not trust any XFF it carries.
		return peer
	}
	chain := forwardChain(r, peer)
	// Walk right-to-left: skip our own proxies, stop at the first untrusted address.
	for i := len(chain) - 1; i >= 0; i-- {
		if !ipInAny(chain[i], trustedNets) {
			return chain[i]
		}
	}
	// Whole chain is trusted infrastructure — the leftmost entry is the closest to
	// the (unknown) real client we have.
	return chain[0]
}

// forwardChain builds the left-to-right address chain: X-Forwarded-For entries
// (each appended by a hop, client-first) followed by the direct peer.
func forwardChain(r *http.Request, peer string) []string {
	chain := []string{}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			if p := strings.TrimSpace(part); p != "" {
				chain = append(chain, p)
			}
		}
	}
	return append(chain, peer)
}

// ipInAny reports whether ip (a bare host string) parses and falls inside any of
// the given networks. A malformed address is never "in" a trusted network, so it
// is treated as an untrusted client — the safe default.
func ipInAny(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}
