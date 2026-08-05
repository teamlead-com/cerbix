package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter is a per-key sliding-window rate limiter (brute-force mitigation
// for local login). It keeps recent attempt timestamps per key in memory.
type loginLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
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

// clientIP extracts the caller's IP, honoring a reverse proxy's
// X-Forwarded-For (first hop) before falling back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
