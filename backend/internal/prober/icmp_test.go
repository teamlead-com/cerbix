package prober

import (
	"context"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// TestICMPProbeLoopback pings 127.0.0.1. ICMP needs a permitted socket
// (CAP_NET_RAW or net.ipv4.ping_group_range); the test skips where neither is
// available (e.g. a locked-down CI runner) rather than failing.
func TestICMPProbeLoopback(t *testing.T) {
	conn, _, err := listenICMP()
	if err != nil {
		t.Skipf("ICMP not permitted in this environment: %v", err)
	}
	_ = conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := icmpProber{guard: defaultGuard()}.Probe(ctx, domain.Monitor{ID: "m", Type: domain.MonitorICMP, Target: "127.0.0.1", TimeoutSeconds: 2})
	if !res.Connected {
		t.Fatalf("loopback ping should connect: %s", res.Msg)
	}
	if res.LatencyMS < 0 {
		t.Fatalf("latency should be non-negative, got %d", res.LatencyMS)
	}
}

func TestICMPProbeBadTarget(t *testing.T) {
	if _, _, err := listenICMP(); err != nil {
		t.Skipf("ICMP not permitted in this environment: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res := icmpProber{guard: defaultGuard()}.Probe(ctx, domain.Monitor{ID: "m", Type: domain.MonitorICMP, Target: "definitely.not.a.host.invalid", TimeoutSeconds: 1})
	if res.Connected {
		t.Fatal("an unresolvable host should not connect")
	}
}
