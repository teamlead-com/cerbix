package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
	if got := clientIP(req(""), 0); got != "10.0.0.5" {
		t.Fatalf("no-proxy clientIP = %q, want 10.0.0.5", got)
	}
	if got := clientIP(req("203.0.113.7, 1.2.3.4"), 0); got != "10.0.0.5" {
		t.Fatalf("0 hops must ignore XFF, got %q", got)
	}

	// 1 trusted hop (e.g. Traefik). Proxy appends the peer it saw, so the real client
	// is the entry just left of the peer — even when the client injected a fake XFF.
	if got := clientIP(req("203.0.113.7"), 1); got != "203.0.113.7" {
		t.Fatalf("1 hop clientIP = %q, want 203.0.113.7", got)
	}
	if got := clientIP(req("1.2.3.4, 203.0.113.7"), 1); got != "203.0.113.7" {
		t.Fatalf("1 hop must skip a spoofed leading XFF, got %q (want the real 203.0.113.7)", got)
	}

	// 2 trusted hops (Cloudflare → Traefik): client is 2 from the right of the chain.
	if got := clientIP(req("203.0.113.7, 172.16.0.9"), 2); got != "203.0.113.7" {
		t.Fatalf("2 hops clientIP = %q, want 203.0.113.7", got)
	}
	// Fewer hops present than configured → clamp to the leftmost, never past the client.
	if got := clientIP(req("203.0.113.7"), 5); got != "203.0.113.7" {
		t.Fatalf("short chain clamp = %q, want 203.0.113.7", got)
	}
}
