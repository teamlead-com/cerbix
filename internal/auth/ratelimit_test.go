package auth

import (
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
	r := httptest.NewRequest("POST", "/auth/local/login", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	if got := clientIP(r); got != "10.0.0.5" {
		t.Fatalf("clientIP = %q, want 10.0.0.5", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("clientIP with XFF = %q, want 203.0.113.7", got)
	}
}
