package prober

import (
	"context"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// icmpProber checks host reachability with an ICMP echo request (ping). It tries
// an unprivileged datagram socket first and falls back to a raw socket, so it
// works both where `net.ipv4.ping_group_range` is set and where the process
// holds CAP_NET_RAW.
type icmpProber struct{ guard Guard }

func (p icmpProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	dst, err := p.guard.resolveChecked(ctx, m.Target)
	if err != nil {
		return Result{Connected: false, Msg: err.Error()}
	}

	conn, useUDP, err := listenICMP()
	if err != nil {
		return Result{Connected: false, Msg: "icmp listen: " + err.Error()}
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(start.Add(5 * time.Second))
	}

	id := os.Getpid() & 0xffff
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho, Code: 0,
		Body: &icmp.Echo{ID: id, Seq: 1, Data: []byte("cerbix-ping")},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return Result{Connected: false, Msg: "icmp marshal: " + err.Error()}
	}
	var writeDst net.Addr = dst
	if useUDP {
		writeDst = &net.UDPAddr{IP: dst.IP} // datagram socket wants a UDPAddr
	}
	if _, err := conn.WriteTo(wb, writeDst); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "icmp send: " + err.Error()}
	}

	rb := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(rb)
		if err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no echo reply: " + err.Error()}
		}
		rm, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), rb[:n])
		if err != nil {
			continue
		}
		if rm.Type == ipv4.ICMPTypeEchoReply {
			return Result{Connected: true, LatencyMS: elapsedMS(start)}
		}
		// Not our reply (e.g. an echo we sent looped back on a raw socket); keep reading.
	}
}

// listenICMP opens an ICMP socket, preferring the unprivileged datagram flavor.
func listenICMP() (*icmp.PacketConn, bool, error) {
	if c, err := icmp.ListenPacket("udp4", "0.0.0.0"); err == nil {
		return c, true, nil
	}
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, false, err
	}
	return c, false, nil
}
