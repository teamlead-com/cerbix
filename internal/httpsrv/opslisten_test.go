package httpsrv

// S11 (sec-hardening-2 H-11): with server.ops_listen set, /metrics and /readyz answer 404 on the
// application port — an explicit 404, not the SPA catch-all's 200 with index.html — and serve on
// the ops port; /healthz stays on both. Without ops_listen nothing changes: one port serves all.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/web"
)

func spa(t *testing.T) http.Handler {
	t.Helper()
	h, err := web.New()
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return h
}

func opsCfg(listen, ops string) config.ServerConfig {
	return config.ServerConfig{Listen: listen, OpsListen: ops, HealthzPath: "/healthz", ReadyzPath: "/readyz", MetricsPath: "/metrics"}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestOpsListenMovesMetricsAndReadyzOffTheApplicationPort(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	reg.SetReady(true, "")
	s := New(opsCfg(":8080", "127.0.0.1:9091"), reg, spa(t))
	if s.ops == nil {
		t.Fatal("ops_listen set, but no ops server")
	}
	app, ops := s.server.Handler, s.ops.Handler

	for _, p := range []string{"/metrics", "/readyz"} {
		rec := get(app, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("application port GET %s = %d, want 404", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<html") || strings.Contains(rec.Body.String(), "cerbix_") {
			t.Errorf("application port GET %s served %q", p, rec.Body.String())
		}
	}
	if rec := get(app, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("application port GET /healthz = %d, want 200", rec.Code)
	}
	if rec := get(app, "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("application port GET / = %d, want the SPA", rec.Code)
	}

	if rec := get(ops, "/metrics"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cerbix_ready 1") {
		t.Errorf("ops port GET /metrics = %d %.80q", rec.Code, rec.Body.String())
	}
	if rec := get(ops, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("ops port GET /readyz = %d", rec.Code)
	}
	if rec := get(ops, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("ops port GET /healthz = %d", rec.Code)
	}
	// The ops port serves nothing of the application.
	if rec := get(ops, "/"); rec.Code != http.StatusNotFound {
		t.Errorf("ops port GET / = %d, want 404", rec.Code)
	}
	if rec := get(ops, "/api/v1/version"); rec.Code != http.StatusNotFound {
		t.Errorf("ops port GET /api/v1/version = %d, want 404", rec.Code)
	}
	// The ops server has the same time bounds as the application's (H-8).
	if s.ops.ReadHeaderTimeout != readHeaderTimeout || s.ops.IdleTimeout != idleTimeout {
		t.Errorf("ops server timeouts: header %s idle %s", s.ops.ReadHeaderTimeout, s.ops.IdleTimeout)
	}
}

func TestWithoutOpsListenOnePortServesEverything(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	reg.SetReady(true, "")
	s := New(opsCfg(":8080", ""), reg, spa(t))
	if s.ops != nil {
		t.Fatal("an ops server without ops_listen")
	}
	if rec := get(s.server.Handler, "/metrics"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cerbix_ready") {
		t.Errorf("GET /metrics = %d", rec.Code)
	}
	if rec := get(s.server.Handler, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d", rec.Code)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// Both ports listen, and Shutdown stops both.
func TestBothPortsServeAndStopTogether(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	reg.SetReady(true, "")
	appAddr, opsAddr := freeAddr(t), freeAddr(t)
	s := New(opsCfg(appAddr, opsAddr), reg, spa(t))
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe() }()

	fetch := func(addr, path string) int {
		for range 50 {
			resp, err := http.Get("http://" + addr + path)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s never answered", addr)
		return 0
	}
	if got := fetch(appAddr, "/readyz"); got != http.StatusNotFound {
		t.Errorf("app port /readyz = %d, want 404", got)
	}
	if got := fetch(opsAddr, "/readyz"); got != http.StatusOK {
		t.Errorf("ops port /readyz = %d, want 200", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe after Shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
	if _, err := http.Get("http://" + opsAddr + "/healthz"); err == nil {
		t.Error("the ops port still answers after Shutdown")
	}
}

// A port that cannot be bound is an error, whichever of the two it is.
func TestAnOpsPortThatCannotBeBoundFailsTheServer(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	s := New(opsCfg(freeAddr(t), busy.Addr().String()), reg, spa(t))
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a busy ops port returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a busy ops port did not fail the server")
	}
}

// sec-hardening-2 H-9b: the operational endpoints are answered before the application handler,
// which sets the security headers; they set nosniff themselves.
func TestOperationalEndpointsAreNosniff(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	for _, ops := range []string{"", "127.0.0.1:9091"} {
		s := New(opsCfg(":8080", ops), reg, nil)
		h := s.server.Handler
		if s.ops != nil {
			h = s.ops.Handler
		}
		for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
			if got := get(h, p).Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("ops_listen %q: GET %s X-Content-Type-Options = %q", ops, p, got)
			}
		}
		// Review of iter-0212, P3-3: with ops_listen set, the application port's own answers —
		// /healthz, and the 404 for /readyz and /metrics — carry it too.
		if s.ops != nil {
			for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
				rec := get(s.server.Handler, p)
				if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
					t.Errorf("application port GET %s (%d) X-Content-Type-Options = %q", p, rec.Code, got)
				}
			}
		}
	}
}

// Review of iter-0212, P3-1: Shutdown stops both listeners at once and joins their errors. While
// the application port drains a request that outlives the deadline, the ops port must already be
// closed — sequentially it stayed open, accepting, for as long as the drain took.
func TestShutdownStopsBothListenersAtOnce(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	reg.SetReady(true, "")
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	})
	appAddr, opsAddr := freeAddr(t), freeAddr(t)
	s := New(opsCfg(appAddr, opsAddr), reg, app)
	go func() { _ = s.ListenAndServe() }()
	defer close(release)
	for range 50 {
		if c, err := net.Dial("tcp", opsAddr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	go func() { _, _ = http.Get("http://" + appAddr + "/hang") }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(ctx) }()
	time.Sleep(200 * time.Millisecond) // the app port is draining the hung request
	if c, err := net.Dial("tcp", opsAddr); err == nil {
		_ = c.Close()
		t.Error("the ops port still accepts connections while the application port drains")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("Shutdown = %v, want the application port's deadline error", err)
	}
}
