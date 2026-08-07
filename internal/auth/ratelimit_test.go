package auth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mustCIDRs parses CIDR strings into networks for tests, failing on a bad entry.
func mustCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("bad test CIDR %q: %v", c, err)
		}
		nets = append(nets, n)
	}
	return nets
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter(3)
	now := time.Unix(1_700_000_000, 0)
	// First 3 allowed, 4th denied within the window.
	for i := 0; i < 3; i++ {
		if !l.allow("ip1", now) {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.allow("ip1", now) {
		t.Fatal("4th attempt should be denied")
	}
	// A different key is independent.
	if !l.allow("ip2", now) {
		t.Fatal("other key should be allowed")
	}
	// After the window slides past, attempts are allowed again.
	if !l.allow("ip1", now.Add(61*time.Second)) {
		t.Fatal("attempt after window should be allowed")
	}
}

func TestLoginLimiterEvictsIdleKeys(t *testing.T) {
	l := newLoginLimiter(3)
	now := time.Unix(1_700_000_000, 0)
	l.allow("stale-ip", now)
	if len(l.hits) != 1 {
		t.Fatalf("expected 1 tracked key, got %d", len(l.hits))
	}
	// Well past the window AND the sweep interval: a call for a different key triggers
	// the opportunistic sweep, which must drop the now-idle stale key (no unbounded map).
	later := now.Add(limiterSweepInterval + 2*time.Minute)
	l.allow("fresh-ip", later)
	if _, ok := l.hits["stale-ip"]; ok {
		t.Fatal("idle key should have been evicted by the sweep")
	}
	if len(l.hits) != 1 {
		t.Fatalf("only the fresh key should remain, got %d keys", len(l.hits))
	}
}

func TestLoginLimiterDisabled(t *testing.T) {
	l := newLoginLimiter(0)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 100; i++ {
		if !l.allow("ip1", now) {
			t.Fatal("disabled limiter should always allow")
		}
	}
}

func TestClientIP(t *testing.T) {
	req := func(xff string) *http.Request {
		r := httptest.NewRequest("POST", "/auth/local/login", nil)
		r.RemoteAddr = "10.0.0.5:54321"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// 0 trusted hops: X-Forwarded-For is ignored (client-controlled), peer is used —
	// a spoofed XFF cannot mint a fresh limiter bucket.
	if got := clientIP(req(""), 0, nil); got != "10.0.0.5" {
		t.Fatalf("no-proxy clientIP = %q, want 10.0.0.5", got)
	}
	if got := clientIP(req("203.0.113.7, 1.2.3.4"), 0, nil); got != "10.0.0.5" {
		t.Fatalf("0 hops must ignore XFF, got %q", got)
	}

	// 1 trusted hop (e.g. Traefik). Proxy appends the peer it saw, so the real client
	// is the entry just left of the peer — even when the client injected a fake XFF.
	if got := clientIP(req("203.0.113.7"), 1, nil); got != "203.0.113.7" {
		t.Fatalf("1 hop clientIP = %q, want 203.0.113.7", got)
	}
	if got := clientIP(req("1.2.3.4, 203.0.113.7"), 1, nil); got != "203.0.113.7" {
		t.Fatalf("1 hop must skip a spoofed leading XFF, got %q (want the real 203.0.113.7)", got)
	}

	// 2 trusted hops (Cloudflare → Traefik): client is 2 from the right of the chain.
	if got := clientIP(req("203.0.113.7, 172.16.0.9"), 2, nil); got != "203.0.113.7" {
		t.Fatalf("2 hops clientIP = %q, want 203.0.113.7", got)
	}
	// Fewer hops present than configured → clamp to the leftmost, never past the client.
	if got := clientIP(req("203.0.113.7"), 5, nil); got != "203.0.113.7" {
		t.Fatalf("short chain clamp = %q, want 203.0.113.7", got)
	}
}

func TestClientIPByCIDR(t *testing.T) {
	req := func(peer, xff string) *http.Request {
		r := httptest.NewRequest("POST", "/auth/local/login", nil)
		r.RemoteAddr = peer
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	// Our proxies live in 10.0.0.0/8; count is deliberately wrong (ignored under CIDR mode).
	nets := mustCIDRs(t, "10.0.0.0/8")

	// Peer is a trusted proxy: walk the chain, skip trusted hops, take the first
	// untrusted address — the real client — even with a spoofed leading XFF entry.
	if got := clientIP(req("10.0.0.5:54321", "203.0.113.7"), 0, nets); got != "203.0.113.7" {
		t.Fatalf("trusted peer clientIP = %q, want 203.0.113.7", got)
	}
	if got := clientIP(req("10.0.0.5:54321", "1.1.1.1, 203.0.113.7, 10.0.0.9"), 0, nets); got != "203.0.113.7" {
		t.Fatalf("chain walk = %q, want 203.0.113.7 (skip trailing trusted proxy 10.0.0.9)", got)
	}

	// Direct hit bypassing the proxy: peer is NOT trusted → XFF is ignored entirely,
	// so a forged XFF cannot mint a fresh bucket. This is the #14 dual-path fix.
	if got := clientIP(req("198.51.100.4:5555", "203.0.113.7"), 0, nets); got != "198.51.100.4" {
		t.Fatalf("untrusted peer must ignore XFF, got %q (want 198.51.100.4)", got)
	}

	// No XFF but a trusted peer: nothing left of the peer → the peer itself.
	if got := clientIP(req("10.0.0.5:54321", ""), 0, nets); got != "10.0.0.5" {
		t.Fatalf("trusted peer, no XFF = %q, want 10.0.0.5", got)
	}
	// Entire chain is trusted infrastructure → the leftmost known address.
	if got := clientIP(req("10.0.0.5:54321", "10.0.0.1, 10.0.0.2"), 0, nets); got != "10.0.0.1" {
		t.Fatalf("all-trusted chain = %q, want leftmost 10.0.0.1", got)
	}
}
