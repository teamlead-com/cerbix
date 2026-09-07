package prober

import (
	"net/url"
	"testing"
)

// B1's ORIGIN half, asserted directly (reviewer P2, party [352]).
//
// The redirect test proves the hop is never issued, which is the other half and the louder one. It
// cannot prove this one: its fixture is an httptest TLS server and an httptest plaintext server, and
// those carry DIFFERENT EXPLICIT PORTS. The comparison as it stood before B1 already included the
// port, so it classified that pair as cross-origin and stripped the binding headers — the fixture
// would stay green with the scheme removed from `canaryOrigin` entirely. A test that cannot fail for
// the reason it names is the defect this whole package is about, so the comparison is exercised here
// on its own terms.
//
// The killing case is the pair that differs ONLY in scheme, which is why the ports are written out.
func TestCanaryOriginSeparatesTwoURLsThatDifferOnlyInScheme(t *testing.T) {
	https := mustURL(t, "https://api.example.com:443/journey")
	plain := mustURL(t, "http://api.example.com:443/journey")

	if canaryOrigin(https) == canaryOrigin(plain) {
		t.Fatalf("same origin for %q and %q: an https journey would keep its binding headers "+
			"across a downgrade to the same host and port", https, plain)
	}
}

// Default ports are NORMALIZED, or the comparison would call one origin two.
//
// Without it a redirect from `https://host/a` to `https://host:443/b` — the same place, written two
// ways — reads as cross-origin and silently drops the binding headers, which turns a working journey
// into an authentication failure nobody can explain from the log.
func TestCanaryOriginNormalizesTheDefaultPortOfEachScheme(t *testing.T) {
	for _, c := range []struct{ bare, explicit string }{
		{"https://api.example.com/x", "https://api.example.com:443/x"},
		{"http://api.example.com/x", "http://api.example.com:80/x"},
	} {
		bare, explicit := mustURL(t, c.bare), mustURL(t, c.explicit)
		if canaryOrigin(bare) != canaryOrigin(explicit) {
			t.Errorf("%q and %q are the same origin written two ways, got %q and %q",
				c.bare, c.explicit, canaryOrigin(bare), canaryOrigin(explicit))
		}
	}
	// And the normalization must not erase a REAL difference: the default port of one scheme is an
	// ordinary port for the other.
	if canaryOrigin(mustURL(t, "http://api.example.com/x")) == canaryOrigin(mustURL(t, "http://api.example.com:443/x")) {
		t.Error("port 80 and port 443 on http are different origins")
	}
}

// Host case is not part of the identity; port and host are.
func TestCanaryOriginFoldsHostCaseAndKeepsEverythingElse(t *testing.T) {
	if canaryOrigin(mustURL(t, "https://API.Example.COM/x")) != canaryOrigin(mustURL(t, "https://api.example.com/x")) {
		t.Error("host case is not part of an origin; folding it is what the comparison is for")
	}
	for _, pair := range [][2]string{
		{"https://api.example.com/x", "https://other.example.com/x"},
		{"https://api.example.com:8443/x", "https://api.example.com:9443/x"},
	} {
		if canaryOrigin(mustURL(t, pair[0])) == canaryOrigin(mustURL(t, pair[1])) {
			t.Errorf("%q and %q must be different origins", pair[0], pair[1])
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
