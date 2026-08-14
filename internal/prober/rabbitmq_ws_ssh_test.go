package prober

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// acceptOnce accepts a single connection and hands it to fn, returning the
// listener's address. The listener closes when the test ends.
func acceptOnce(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go fn(c)
		}
	}()
	return ln.Addr().String()
}

func TestSSHProber(t *testing.T) {
	// A server that greets with an SSH identification banner.
	addr := acceptOnce(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	})
	r := NewRunner()
	m := domain.Monitor{ID: "s", ProjectID: "p", Name: "ssh", Type: domain.MonitorSSH, Target: addr, IntervalSeconds: 60, TimeoutSeconds: 5}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("SSH banner should be up: %+v", hb)
	}
	// [BODY] condition can assert on the banner (exposed as the probe body).
	m.Conditions = []string{"[BODY] contains OpenSSH"}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("[BODY] contains OpenSSH should pass: %+v", hb)
	}
	m.Conditions = []string{"[BODY] contains Dropbear"}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("[BODY] contains Dropbear should fail: %+v", hb)
	}

	// A non-SSH server (no banner starting with SSH-) → down.
	bad := acceptOnce(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write([]byte("220 smtp ready\r\n"))
	})
	m.Conditions = nil
	m.Target = bad
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("non-SSH banner should be down: %+v", hb)
	}
	// Closed port → down.
	m.Target = "127.0.0.1:1"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("closed port should be down: %+v", hb)
	}
}

func TestWebSocketProber(t *testing.T) {
	// A minimal server that completes the RFC 6455 handshake with a valid accept.
	good := acceptOnce(t, func(c net.Conn) {
		defer c.Close()
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"))
	})
	r := NewRunner()
	m := domain.Monitor{ID: "w", ProjectID: "p", Name: "ws", Type: domain.MonitorWebSocket, Target: "ws://" + good + "/chat", IntervalSeconds: 60, TimeoutSeconds: 5}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("valid handshake should be up: %+v", hb)
	}

	// Server that returns 101 but a wrong accept → down.
	badAccept := acceptOnce(t, func(c net.Conn) {
		defer c.Close()
		_, _ = http.ReadRequest(bufio.NewReader(c))
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Accept: wrong\r\n\r\n"))
	})
	m.Target = "ws://" + badAccept
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("bad accept should be down: %+v", hb)
	}

	// A plain HTTP server that never upgrades (200) → down; [STATUS] sees 200.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()
	m.Target = plain.URL
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("non-upgrading server should be down: %+v", hb)
	}
	// Closed port → down.
	m.Target = "ws://127.0.0.1:1"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("closed port should be down: %+v", hb)
	}
}

func TestRabbitMQProberAMQP(t *testing.T) {
	// A broker that replies to the protocol header with a METHOD frame (type 0x01).
	broker := acceptOnce(t, func(c net.Conn) {
		defer c.Close()
		hdr := make([]byte, 8)
		if _, err := c.Read(hdr); err != nil {
			return
		}
		_, _ = c.Write([]byte{0x01, 0, 0}) // start of a Connection.Start method frame
	})
	r := NewRunner()
	m := domain.Monitor{ID: "rq", ProjectID: "p", Name: "rabbit", Type: domain.MonitorRabbitMQ, Target: broker, IntervalSeconds: 60, TimeoutSeconds: 5}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("AMQP handshake should be up: %+v", hb)
	}

	// A version-mismatch broker echoes the protocol header ('A'...) — still alive.
	echo := acceptOnce(t, func(c net.Conn) {
		defer c.Close()
		hdr := make([]byte, 8)
		if _, err := c.Read(hdr); err != nil {
			return
		}
		_, _ = c.Write(amqpProtocolHeader)
	})
	m.Target = echo
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("protocol-header reply should be up: %+v", hb)
	}

	// A non-AMQP service (HTTP-ish reply) → down.
	notAMQP := acceptOnce(t, func(c net.Conn) {
		defer c.Close()
		hdr := make([]byte, 8)
		if _, err := c.Read(hdr); err != nil {
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 400\r\n"))
	})
	m.Target = notAMQP
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("non-AMQP reply should be down: %+v", hb)
	}
	// Closed port → down.
	m.Target = "127.0.0.1:1"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("closed port should be down: %+v", hb)
	}
}

func TestRabbitMQProberManagement(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth = req.Header.Get("Authorization")
		if req.URL.Path == "/api/overview" {
			_, _ = w.Write([]byte(`{"rabbitmq_version":"3.12.0"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := NewRunner()
	m := domain.Monitor{
		ID: "rm", ProjectID: "p", Name: "rabbit-mgmt", Type: domain.MonitorRabbitMQ, Target: srv.URL,
		IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{"mode": "management", "username": "guest", "password": "guest"},
	}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("management 200 should be up: %+v", hb)
	}
	if gotAuth == "" || !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("basic auth not sent: %q", gotAuth)
	}
	// [BODY] can assert on the JSON.
	m.Conditions = []string{"[BODY] contains rabbitmq_version"}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("[BODY] contains version should pass: %+v", hb)
	}
	// A path that 404s → down (default judge requires 2xx).
	m.Conditions = nil
	m.Config["path"] = "/api/nope"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("404 management path should be down: %+v", hb)
	}
}

func TestRabbitMQManagementTLSIsExplicitAndVerifiedByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	r := NewRunner()
	m := domain.Monitor{
		ID: "rm-tls", ProjectID: "p", Name: "rabbit-mgmt-tls", Type: domain.MonitorRabbitMQ,
		Target: srv.URL, IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{"mode": "management", "username": "guest", "password": "guest", "tls": "true"},
	}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("untrusted certificate passed verified TLS: %+v", hb)
	}
	m.Config["tls_skip_verify"] = "true"
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("explicit skip-verify TLS should connect: %+v", hb)
	}
	m.Target = strings.Replace(srv.URL, "https://", "http://", 1)
	if hb := r.Run(context.Background(), m); hb.Up || !strings.Contains(hb.Msg, "must use https") {
		t.Fatalf("tls=true accepted http target: %+v", hb)
	}
}
