package httpsrv

// sec-hardening-2 H-8: the server bounds how long a client may take to send its headers
// (ReadHeaderTimeout 5 s) and how long an idle keep-alive connection is held (IdleTimeout 60 s), on
// top of the existing 15 s ReadTimeout. None of the three may cut a response that is still being
// written: the SSE stream (/api/v1/events) and the agents' long-poll outlive all of them.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

func TestServerBoundsHeaderAndIdleTime(t *testing.T) {
	s := newTestServer(true)
	if got := s.server.ReadHeaderTimeout; got != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 5s", got)
	}
	if got := s.server.IdleTimeout; got != 60*time.Second {
		t.Errorf("IdleTimeout = %s, want 60s", got)
	}
	if got := s.server.ReadTimeout; got != config.HTTPReadTimeout {
		t.Errorf("ReadTimeout = %s, want %s", got, config.HTTPReadTimeout)
	}
	if got := s.server.WriteTimeout; got != 0 {
		t.Errorf("WriteTimeout = %s, want none: it would cut the SSE stream", got)
	}
}

// The same server, its three timeouts shrunk far below the life of the responses, still delivers
// a stream (SSE) and a late single answer (long-poll) in full.
func TestLongLivedResponsesOutliveTheServerTimeouts(t *testing.T) {
	app := http.NewServeMux()
	app.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 8 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
		}
	})
	app.HandleFunc("/api/v1/agent/jobs", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(1200 * time.Millisecond):
		}
		_, _ = io.WriteString(w, `{"jobs":[]}`)
	})
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	s := New(config.ServerConfig{Listen: "127.0.0.1:0", HealthzPath: "/healthz", ReadyzPath: "/readyz", MetricsPath: "/metrics"}, reg, app)
	s.server.ReadTimeout = 300 * time.Millisecond
	s.server.ReadHeaderTimeout = 200 * time.Millisecond
	s.server.IdleTimeout = 250 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.server.Serve(ln) }()
	t.Cleanup(func() { _ = s.server.Close() })
	base := "http://" + ln.Addr().String()

	resp, err := http.Get(base + "/api/v1/events")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var got []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			got = append(got, line)
		}
	}
	_ = resp.Body.Close()
	if len(got) != 8 {
		t.Errorf("SSE stream delivered %d of 8 events over 1.2 s (err %v): a timeout cut it", len(got), sc.Err())
	}

	resp, err = http.Get(base + "/api/v1/agent/jobs")
	if err != nil {
		t.Fatalf("long-poll: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != `{"jobs":[]}` {
		t.Errorf("long-poll answered %q (%v) after 1.2 s, want the full body", body, err)
	}
}

// A client that never finishes its headers is dropped at ReadHeaderTimeout.
func TestSlowHeadersAreDropped(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	s := New(config.ServerConfig{Listen: "127.0.0.1:0", HealthzPath: "/healthz", ReadyzPath: "/readyz", MetricsPath: "/metrics"}, reg, nil)
	s.server.ReadHeaderTimeout = 200 * time.Millisecond
	s.server.ReadTimeout = 0 // only the header bound is under test
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.server.Serve(ln) }()
	t.Cleanup(func() { _ = s.server.Close() })
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n") // never the blank line
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, _ = io.ReadAll(c) // returns when the server closes the connection
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a client that never finished its headers was held %s", took.Round(100*time.Millisecond))
	}
}
