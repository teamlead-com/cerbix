package prober

import (
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// TestEchoReplyMatches guards the id/seq/payload matching that stops a concurrent
// ping or a stray reply on a shared raw socket from being counted as this probe's
// success (a false "up").
func TestEchoReplyMatches(t *testing.T) {
	seq, id := 4242, 1234
	payload := []byte("cerbix-abcdef0123456789")
	reply := func(body *icmp.Echo) *icmp.Message {
		return &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: body}
	}

	// Exact match on the raw socket (matchID=true) and the udp socket (matchID=false).
	ours := reply(&icmp.Echo{ID: id, Seq: seq, Data: payload})
	if !echoReplyMatches(ours, seq, payload, id, true) {
		t.Fatal("our own reply must match on the raw socket")
	}
	if !echoReplyMatches(ours, seq, payload, id, false) {
		t.Fatal("our own reply must match on the udp socket")
	}

	// A different ping's reply (wrong seq/payload) is rejected.
	if echoReplyMatches(reply(&icmp.Echo{ID: id, Seq: 9, Data: payload}), seq, payload, id, true) {
		t.Fatal("wrong seq must not match")
	}
	if echoReplyMatches(reply(&icmp.Echo{ID: id, Seq: seq, Data: []byte("other")}), seq, payload, id, true) {
		t.Fatal("wrong payload must not match")
	}
	// On the raw socket a foreign ID is rejected; on the udp socket the kernel owns
	// the ID so it is not matched (seq+payload still gate it).
	foreignID := reply(&icmp.Echo{ID: 999, Seq: seq, Data: payload})
	if echoReplyMatches(foreignID, seq, payload, id, true) {
		t.Fatal("foreign ID must not match on the raw socket")
	}
	if !echoReplyMatches(foreignID, seq, payload, id, false) {
		t.Fatal("ID is not matched on the udp socket (kernel-owned)")
	}

	// A non-echo-reply type (e.g. our own echo looped back) is rejected.
	if echoReplyMatches(&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: ours.Body}, seq, payload, id, true) {
		t.Fatal("a non-reply type must not match")
	}
}
