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
// trustedProxies is how many reverse-proxy hops sit in front of cerbix; the client
// is that many entries from the right of the (X-Forwarded-For ++ peer) chain. With
// 0 trusted hops, X-Forwarded-For is ignored entirely (it is client-controlled) and
// the direct peer address is used, so a client cannot forge a fresh limiter bucket.
func clientIP(r *http.Request, trustedProxies int) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if trustedProxies <= 0 {
		return peer
	}
	// Full left-to-right chain: XFF entries (each appended by a hop) then the peer.
	chain := []string{}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			if p := strings.TrimSpace(part); p != "" {
				chain = append(chain, p)
			}
		}
	}
	chain = append(chain, peer)
	// The rightmost trustedProxies entries are our own proxies; the one just before
	// them is the real client. Clamp to the leftmost entry if the chain is short
	// (fewer hops present than configured — never index past the client).
	idx := len(chain) - 1 - trustedProxies
	if idx < 0 {
		idx = 0
	}
	return chain[idx]
}
