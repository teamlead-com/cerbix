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

// authLimiters are the three brute-force limiters of the unauthenticated /auth/local/* endpoints
// (sec-hardening-2 H-6, D-0276), all derived from local.login_rate_limit_per_minute = L; L <= 0
// disables all three. A single "IP and email" key allowed either an account-lockout DoS or no
// protection, so they are split:
//   - perIP: L/min per client IP, checked BEFORE the body is read; buckets "login|<ip>" and
//     "reset|<ip>" (reset request and confirm share one);
//   - perIPEmail: min(L, max(2, (L+1)/2))/min per (IP, normalized email), checked after decode — one
//     address hammering one account is cut off sooner than a NAT signing many people in;
//   - emailFailures: 5L/min per normalized email, counting FAILED logins only — several times the
//     per-IP threshold, so failures driven from one other address (it can drive at most the
//     per-(IP, email) limit, about L/2) cannot lock a real user out of a clean address. Residual,
//     inherent to any per-email limiter: about ten addresses together (5L ÷ L/2) can keep an
//     email blocked for as long as they keep failing.
//
// The limiters are per process; a shared limiter across replicas is a non-goal.
type authLimiters struct {
	perIP         *loginLimiter
	perIPEmail    *loginLimiter
	emailFailures *loginLimiter
}

func newAuthLimiters(perMinute int) authLimiters {
	// (L+1)/2, but never below 2 — a TOTP user's sign-in is two requests (password, then the
	// code) — and never above L, which the per-IP bucket enforces anyway.
	ipEmail := max(2, (perMinute+1)/2)
	ipEmail = min(ipEmail, perMinute)
	if perMinute <= 0 {
		ipEmail = 0
	}
	return authLimiters{
		perIP:         newLoginLimiter(perMinute),
		perIPEmail:    newLoginLimiter(ipEmail),
		emailFailures: newLoginLimiter(5 * perMinute),
	}
}

// normalizeEmail is the limiter key for an email: trimmed and lower-cased. It is never used for
// the account lookup, which stays exact.
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

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

// blocked reports whether key is at or over the limit within the window at now, recording
// nothing — the read half of a failures-only limiter (record is the write half). Disabled
// limiters never block.
func (l *loginLimiter) blocked(key string, now time.Time) bool {
	if l.limit <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	n := 0
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			n++
		}
	}
	return n >= l.limit
}

// record counts one event for key at now without checking the limit (failures-only limiter).
func (l *loginLimiter) record(key string, now time.Time) {
	if l.limit <= 0 {
		return
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
	l.hits[key] = append(kept, now)
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
