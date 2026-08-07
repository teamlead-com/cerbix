package prober

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
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

	// A unique per-probe id/seq/payload so a concurrent ping or a stray reply on the
	// shared raw socket can't be mistaken for THIS probe's reply (a false "up").
	id := os.Getpid() & 0xffff
	var seqBuf [2]byte
	_, _ = rand.Read(seqBuf[:])
	seq := int(binary.BigEndian.Uint16(seqBuf[:]))
	token := make([]byte, 16)
	_, _ = rand.Read(token)
	payload := append([]byte("cerbix-"), token...)

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho, Code: 0,
		Body: &icmp.Echo{ID: id, Seq: seq, Data: payload},
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
		// Match seq + payload always; match ID only on the raw socket — the
		// unprivileged datagram socket lets the kernel own/rewrite the ID.
		if echoReplyMatches(rm, seq, payload, id, !useUDP) {
			return Result{Connected: true, LatencyMS: elapsedMS(start)}
		}
		// Not our reply (another ping's, or our own echo looped back) — keep reading.
	}
}

// echoReplyMatches reports whether rm is an ICMP echo reply for THIS probe: an
// EchoReply body whose Seq and payload match what we sent (and, on a raw socket
// where we own the id, the ID too). This rejects a concurrent monitor's or a stray
// reply on a shared raw socket being counted as our success.
func echoReplyMatches(rm *icmp.Message, seq int, payload []byte, id int, matchID bool) bool {
	if rm.Type != ipv4.ICMPTypeEchoReply {
		return false
	}
	echo, ok := rm.Body.(*icmp.Echo)
	if !ok {
		return false
	}
	if echo.Seq != seq || !bytes.Equal(echo.Data, payload) {
		return false
	}
	if matchID && echo.ID != id {
		return false
	}
	return true
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
