package prober

// S8 (sec-hardening-2 H-8): every read from a probed peer is bounded. The ssh and redis probers read
// lines through io.LimitReader(conn, 4 KiB), the WebSocket prober its handshake response through
// io.LimitReader(conn, 16 KiB). A peer that streams without ever sending a newline therefore ends
// the probe at the limit — DOWN, at once — instead of growing a buffer until the probe's deadline.

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// floodPeer accepts connections and writes prefix followed by an endless run of 'A' with no
// newline, until the client goes away.
func floodPeer(t *testing.T, prefix string) string { t.Helper(); return floodPeerWith(t, prefix, "A") }

// floodPeerWith floods with fill instead of 'A' (a JSON body needs a filler its decoder accepts).
func floodPeerWith(t *testing.T, prefix, fill string) string {
	t.Helper()
	chunk := []byte(strings.Repeat(fill, 1024))
	return acceptOnce(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		if prefix != "" {
			if _, err := c.Write([]byte(prefix)); err != nil {
				return
			}
		}
		for {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	})
}

func TestProbedPeersAreReadWithABound(t *testing.T) {
	const timeout = 10 // seconds: what an unbounded read would wait for
	cases := []struct {
		name   string
		typ    domain.MonitorType
		target func(addr string) string
		prefix string
		fill   string
		config map[string]string
	}{
		// The flood starts like a banner: a bounded read must not turn its truncation into one.
		{"ssh banner", domain.MonitorSSH, func(a string) string { return a }, "SSH-2.0-", "", nil},
		{"redis reply", domain.MonitorRedis, func(a string) string { return a }, "+", "", nil},
		{"websocket handshake", domain.MonitorWebSocket, func(a string) string { return "ws://" + a + "/" },
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nX-Flood: ", "", nil},
		// A Prometheus answer whose JSON never ends: whitespace inside the result array, which a
		// JSON decoder buffers until the value completes (review of iter-0212, P2-1).
		{"promql body", domain.MonitorPromQL, func(a string) string { return "http://" + a }, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" +
			`{"status":"success","data":{"resultType":"vector","result":[`, " ", map[string]string{"query": "up"}},
	}
	r := NewRunner()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fill := c.fill
			if fill == "" {
				fill = "A"
			}
			addr := floodPeerWith(t, c.prefix, fill)
			m := domain.Monitor{ID: "b", ProjectID: "p", Name: c.name, Type: c.typ, Target: c.target(addr),
				IntervalSeconds: 60, TimeoutSeconds: timeout, Config: c.config}
			start := time.Now()
			hb := r.Run(context.Background(), m)
			took := time.Since(start)
			if hb.Up {
				t.Fatalf("a peer that never sends a newline is up: %+v", hb)
			}
			if took > 3*time.Second {
				t.Errorf("the probe read until its deadline (%s of %ds): the read is unbounded (msg %q)", took.Round(100*time.Millisecond), timeout, hb.Msg)
			}
		})
	}
}

// Review of iter-0212, P1-2: the identification is a COMPLETE line (RFC 4253 §4.2 ends it with CR
// LF). A line cut short — by the read bound, or by the peer closing — is no banner, whatever it
// starts with.
func TestSSHBannerCountsOnlyAsACompleteLine(t *testing.T) {
	r := NewRunner()
	for _, c := range []struct{ name, send string }{
		{"prefix then close", "SSH-"},
		{"version without a line end, then close", "SSH-2.0-OpenSSH_9.6"},
	} {
		t.Run(c.name, func(t *testing.T) {
			addr := acceptOnce(t, func(conn net.Conn) {
				_, _ = conn.Write([]byte(c.send))
				_ = conn.Close()
			})
			m := domain.Monitor{ID: "s", ProjectID: "p", Name: "ssh", Type: domain.MonitorSSH, Target: addr, IntervalSeconds: 60, TimeoutSeconds: 5}
			if hb := r.Run(context.Background(), m); hb.Up {
				t.Errorf("peer sent %q and closed: up, want DOWN (%+v)", c.send, hb)
			}
		})
	}
	// A complete banner line is still UP.
	addr := acceptOnce(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
		_ = conn.Close()
	})
	m := domain.Monitor{ID: "s", ProjectID: "p", Name: "ssh", Type: domain.MonitorSSH, Target: addr, IntervalSeconds: 60, TimeoutSeconds: 5}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Errorf("a complete banner line: down (%+v)", hb)
	}
}

// Review of iter-0212, P2-1 ("every read"): the database probers read their peer through drivers
// whose framing alone bounds nothing a hostile peer cannot exceed — pgx accepts a message body of
// up to ~1 GiB, and the MySQL driver chains 16 MiB packets without a total. Both connections are
// read through a total bound (dbConnReadLimit). Over loopback a gigabyte arrives inside the
// deadline, so time is no oracle here: the peer counts what it managed to write before the probe
// hung up, which a bounded reader keeps to the bound plus the sockets' buffers.
func TestDatabasePeersAreReadWithABound(t *testing.T) {
	const ceiling = 64 << 20 // far above 1 MiB + socket buffers, far below what an unbounded read takes
	r := NewRunner()
	chunk := []byte(strings.Repeat("A", 64<<10))
	cases := []struct {
		name   string
		typ    domain.MonitorType
		config map[string]string
		frame  func(seq int) []byte // the header before each 16 MiB of filler
		once   bool                 // the header is sent only before the first run of filler
	}{
		// An authentication message declaring a ~1 GiB body, then the body.
		{"postgres message", domain.MonitorPostgres, map[string]string{"sslmode": "disable", "username": "u"},
			func(int) []byte { return []byte{'R', 0x3f, 0xff, 0xff, 0x00} }, true},
		// Maximum-size packets (length 0xffffff means "more follows"), chained.
		{"mysql packets", domain.MonitorMySQL, map[string]string{"username": "u"},
			func(seq int) []byte { return []byte{0xff, 0xff, 0xff, byte(seq)} }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			written := make(chan int64, 1)
			addr := acceptOnce(t, func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				var n int64
				defer func() { written <- n }()
				for seq := 0; seq < 1<<12; seq++ { // at most 64 GiB: the probe hangs up long before
					if seq == 0 || !c.once {
						k, err := conn.Write(c.frame(seq))
						n += int64(k)
						if err != nil {
							return
						}
					}
					for left := 1<<24 - 1; left > 0; {
						m := len(chunk)
						if m > left {
							m = left
						}
						k, err := conn.Write(chunk[:m])
						n += int64(k)
						if err != nil {
							return
						}
						left -= m
					}
				}
			})
			m := domain.Monitor{ID: "db", ProjectID: "p", Name: c.name, Type: c.typ, Target: addr,
				IntervalSeconds: 60, TimeoutSeconds: 10, Config: c.config}
			if hb := r.Run(context.Background(), m); hb.Up {
				t.Fatalf("up: %+v", hb)
			}
			select {
			case n := <-written:
				if n > ceiling {
					t.Errorf("the peer wrote %d MiB before the probe hung up: the read is unbounded", n>>20)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the peer never saw the probe hang up")
			}
		})
	}
}
