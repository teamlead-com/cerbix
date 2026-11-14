package config

import (
	"strings"
	"testing"
)

// sec-hardening-2 H-11: server.ops_listen moves /metrics and /readyz to their own port, so it may
// not be the application's port. Review of iter-0212, P2-5: "the same port" is judged on parsed
// host:port pairs, not strings — the same port collides when the hosts are equal or either is a
// wildcard ("", "0.0.0.0", "::"); a value with edge whitespace, all whitespace or no port is refused.
func TestOpsListenMayNotBeTheApplicationPort(t *testing.T) {
	cases := []struct {
		listen, ops string
		ok          bool
	}{
		{":8080", "", true},
		{":8080", "127.0.0.1:9091", true},
		{":8080", ":9091", true},
		{"127.0.0.1:8080", "10.0.0.5:8080", true}, // two specific addresses: two sockets
		{":8080", ":8080", false},
		{":8080", "0.0.0.0:8080", false},
		{":8080", "[::]:8080", false},
		{":8080", "127.0.0.1:8080", false},
		{"0.0.0.0:8080", "127.0.0.1:8080", false},
		{"127.0.0.1:8080", "127.0.0.1:8080", false},
		{"127.0.0.1:8080", ":8080", false},
		{":8080", "   ", false},
		{":8080", " :9091", false},
		{":8080", ":9091 ", false},
		{":8080", "9091", false},
		{":8080", "127.0.0.1:", false},
		{"8080", ":9091", false}, // listen too must be host:port
		// Second pass, P3-1: hosts compare as addresses when they parse as IPs (unmapped, wildcard by
		// IsUnspecified), as case-folded names otherwise; ports compare as numbers 1..65535.
		{"[::1]:8080", "[::]:8080", false},
		{"127.0.0.1:8080", "[0:0:0:0:0:0:0:0]:8080", false},
		{"[::1]:8080", "[0:0:0:0:0:0:0:1]:8080", false},
		{"127.0.0.1:8080", "[::ffff:127.0.0.1]:8080", false},
		{"[::1]:8080", "127.0.0.1:8080", true}, // two loopback addresses of two families: two sockets
		{"Localhost:8080", "localhost:8080", false},
		{":8080", ":08080", false},
		{":8080", ":99999", false},
		{":8080", ":-1", false},
		{":8080", ":0", false},
		{":8080", ":http", false},
		{":8080", "[not-an-ip]:8080", false},
		{":8080", "[not-an-ip]:9091", false},
		{":99999", ":9091", false},
		{":-1", ":9091", false},
		{"[not-an-ip]:8080", ":9091", false},
	}
	for _, c := range cases {
		cfg := defaults()
		cfg.Server.Listen, cfg.Server.OpsListen = c.listen, c.ops
		err := cfg.Validate()
		if c.ok && err != nil {
			t.Errorf("listen %q, ops_listen %q: %v", c.listen, c.ops, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "server.")) {
			t.Errorf("listen %q, ops_listen %q: Validate() = %v, want a server.* refusal", c.listen, c.ops, err)
		}
	}
}
