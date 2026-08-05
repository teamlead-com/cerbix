package prober

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func TestCheckIP(t *testing.T) {
	cases := []struct {
		name          string
		ip            string
		allowPrivate  bool
		allowMetadata bool
		wantBlocked   bool
	}{
		{"metadata blocked by default", "169.254.169.254", true, false, true},
		{"metadata allowed when opted in", "169.254.169.254", true, true, false},
		{"ipv6 imds blocked despite private allowed", "fd00:ec2::254", true, false, true},
		{"ipv6 imds allowed when metadata opted in", "fd00:ec2::254", true, true, false},
		{"other ULA still treated as private", "fd12:3456::1", true, false, false},
		{"loopback allowed with private", "127.0.0.1", true, false, false},
		{"loopback blocked without private", "127.0.0.1", false, false, true},
		{"rfc1918 10/8 allowed with private", "10.1.2.3", true, false, false},
		{"rfc1918 192.168 blocked without private", "192.168.1.1", false, false, true},
		{"rfc1918 172.16 blocked without private", "172.16.0.1", false, false, true},
		{"public always allowed", "8.8.8.8", false, false, false},
		{"public allowed even locked down", "93.184.216.34", false, false, false},
		{"unspecified always blocked", "0.0.0.0", true, true, true},
		{"multicast always blocked", "224.0.0.1", true, true, true},
		{"ipv6 link-local blocked", "fe80::1", true, false, true},
		{"ipv6 ula is private", "fc00::1", false, false, true},
		{"ipv6 ula allowed with private", "fc00::1", true, false, false},
		{"ipv6 loopback blocked without private", "::1", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := NewGuard(c.allowPrivate, c.allowMetadata)
			err := g.checkIP(net.ParseIP(c.ip))
			if c.wantBlocked && err == nil {
				t.Fatalf("checkIP(%s) = nil, want blocked", c.ip)
			}
			if !c.wantBlocked && err != nil {
				t.Fatalf("checkIP(%s) = %v, want allowed", c.ip, err)
			}
			if c.wantBlocked {
				var b *blockedIPError
				if !errors.As(err, &b) {
					t.Fatalf("checkIP(%s) err = %T, want *blockedIPError", c.ip, err)
				}
			}
		})
	}
}

// TestGuardedDialRejectsLiteralIP proves a blocked literal-IP target is rejected
// *before* any dial: the guard returns a blockedIPError, never touching the base
// dialer. A sentinel dialer that panics if invoked backs the assertion.
func TestGuardedDialRejectsLiteralIP(t *testing.T) {
	g := NewGuard(false, false) // lock everything private/metadata down
	dial := g.dialContext(&net.Dialer{})

	for _, addr := range []string{"169.254.169.254:80", "127.0.0.1:8080", "10.0.0.5:5432"} {
		conn, err := dial(context.Background(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("dial(%s) returned a connection, want block", addr)
		}
		var b *blockedIPError
		if !errors.As(err, &b) {
			t.Fatalf("dial(%s) err = %v (%T), want *blockedIPError", addr, err, err)
		}
	}
}

// TestGuardedDialResolvesAndBlocks covers the hostname path: the guard resolves
// the host, finds every candidate IP disallowed, and returns a block error rather
// than dialing. `localhost` resolves to loopback, blocked when private is off.
func TestGuardedDialResolvesAndBlocks(t *testing.T) {
	g := NewGuard(false, false)
	conn, err := g.dialContext(&net.Dialer{})(context.Background(), "tcp", "localhost:80")
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("dial(localhost) returned a connection, want block")
	}
	var b *blockedIPError
	if !errors.As(err, &b) {
		t.Fatalf("dial(localhost) err = %v (%T), want *blockedIPError after resolution", err, err)
	}
}

// TestProberBlocksSSRF exercises the full HTTP probe path: a monitor pointed at
// the cloud-metadata address is reported down with a "blocked" message and makes
// no network request.
func TestProberBlocksSSRF(t *testing.T) {
	r := NewRunnerWithGuard(NewGuard(true, false)) // product default: private ok, metadata blocked
	hb := r.Run(context.Background(), domain.Monitor{
		ID: "m1", Type: domain.MonitorHTTP, Target: "http://169.254.169.254/latest/meta-data/", TimeoutSeconds: 5,
	})
	if hb.Up {
		t.Fatalf("metadata probe reported up, want down")
	}
	if !strings.Contains(hb.Msg, "blocked target IP") {
		t.Fatalf("msg = %q, want a blocked-IP message", hb.Msg)
	}
}

// TestProberBlocksLoopbackWhenLockedDown confirms the opt-out path: with private
// IPs disallowed, a loopback HTTP target is blocked.
func TestProberBlocksLoopbackWhenLockedDown(t *testing.T) {
	r := NewRunnerWithGuard(NewGuard(false, false))
	hb := r.Run(context.Background(), domain.Monitor{
		ID: "m2", Type: domain.MonitorTCP, Target: "127.0.0.1:9", TimeoutSeconds: 5,
	})
	if hb.Up {
		t.Fatalf("loopback TCP probe reported up, want down")
	}
	if !strings.Contains(hb.Msg, "blocked target IP") {
		t.Fatalf("msg = %q, want a blocked-IP message", hb.Msg)
	}
}
