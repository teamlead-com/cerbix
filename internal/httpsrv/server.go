// Package httpsrv serves the cerbix operational endpoints: health, readiness,
// and Prometheus metrics. Application routes are mounted by higher layers.
package httpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

// Server wraps the HTTP server of the port the process listens on and, when server.ops_listen is
// set, a second server for the operational endpoints.
type Server struct {
	server  *http.Server
	ops     *http.Server // nil unless server.ops_listen is set
	metrics *metrics.Registry
	logger  *slog.Logger

	notReadyMu     sync.Mutex
	notReadyLogged string // the readiness code last logged ("" = ready), so a probe every few seconds logs a change once
}

// WithLogger sets where /readyz writes the raw reason behind a not-ready answer.
func (s *Server) WithLogger(l *slog.Logger) *Server {
	if l != nil {
		s.logger = l
	}
	return s
}

// New builds the server from config and a metrics registry. If app is non-nil it is mounted as
// the catch-all handler (application routes such as /api and /auth); the operational endpoints
// always take precedence.
//
// Without server.ops_listen one port serves /healthz, /readyz, /metrics and the application.
// With it (sec-hardening-2 H-11) /readyz and /metrics move to the ops listener and the application
// port answers them with an explicit 404 — registered as http.NotFoundHandler, not merely left
// out, because the application's SPA catch-all would otherwise answer /readyz 200 with index.html
// while the database is down. /healthz reveals nothing and stays on both.
func New(cfg config.ServerConfig, registry *metrics.Registry, app http.Handler) *Server {
	mux := http.NewServeMux()
	s := &Server{
		server:  newHTTPServer(cfg.Listen, mux),
		metrics: registry,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		// notReadyLogged starts at "" (ready): the first ready probe logs nothing.
	}
	mux.HandleFunc(cfg.HealthzPath, s.healthz)
	if cfg.OpsListen == "" {
		mux.HandleFunc(cfg.ReadyzPath, s.readyz)
		mux.HandleFunc(cfg.MetricsPath, s.metricsHandler)
	} else {
		mux.Handle(cfg.ReadyzPath, http.NotFoundHandler())
		mux.Handle(cfg.MetricsPath, http.NotFoundHandler())
		ops := http.NewServeMux()
		ops.HandleFunc(cfg.HealthzPath, s.healthz)
		ops.HandleFunc(cfg.ReadyzPath, s.readyz)
		ops.HandleFunc(cfg.MetricsPath, s.metricsHandler)
		ops.Handle("/", http.NotFoundHandler())
		s.ops = newHTTPServer(cfg.OpsListen, ops)
	}
	if app != nil {
		mux.Handle("/", app)
	}
	return s
}

// The server's time bounds (sec-hardening-2 H-8). ReadHeaderTimeout drops a client that never
// finishes its headers; IdleTimeout closes a keep-alive connection that sends no next request
// (without it the idle bound would fall back to ReadTimeout). There is deliberately NO
// WriteTimeout, and none of the three cuts a response still being written: the SSE stream
// (/api/v1/events) and the agents' long-poll outlive them all (TestLongLivedResponsesOutliveTheServerTimeouts).
const (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 60 * time.Second
)

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadTimeout:       config.HTTPReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// ListenAndServe blocks serving requests until the server is closed. With an ops listener both
// serve; the first to fail closes the other and its error is returned. After Shutdown it returns
// nil.
func (s *Server) ListenAndServe() error {
	servers := []*http.Server{s.server}
	if s.ops != nil {
		servers = append(servers, s.ops)
	}
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			err := srv.ListenAndServe()
			if err == http.ErrServerClosed {
				err = nil
			}
			errc <- err
		}()
	}
	var first error
	for range servers {
		if err := <-errc; err != nil && first == nil {
			first = err
			for _, srv := range servers {
				_ = srv.Close()
			}
		}
	}
	return first
}

// Shutdown gracefully stops the server: both listeners at once, so the ops port does not keep
// accepting while the application port drains, and both errors are reported (review of iter-0212,
// P3-1).
func (s *Server) Shutdown(ctx context.Context) error {
	if s.ops == nil {
		return s.server.Shutdown(ctx)
	}
	opsErr := make(chan error, 1)
	go func() { opsErr <- s.ops.Shutdown(ctx) }()
	appErr := s.server.Shutdown(ctx)
	return errors.Join(appErr, <-opsErr)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz answers from the registry's one readiness predicate (sec-hardening-2 H-12). A 503 body
// carries a stable reason code (metrics.NotReady*) and nothing else; the raw reason a component
// recorded — possibly a driver message or an address — goes to the log (H-11).
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	code := s.observeReadiness()
	if code != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"error":  code,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// observeReadiness reads the registry and logs a change of the readiness CODE, under one lock
// (review of iter-0212, P3-2): two concurrent probes cannot log the old state after the new one,
// and a detail that ticks while the code holds ("lagging 300s", "lagging 310s") logs nothing more.
// The first detail of a not-ready spell is the one logged.
func (s *Server) observeReadiness() string {
	s.notReadyMu.Lock()
	defer s.notReadyMu.Unlock()
	code, detail := s.metrics.Readiness()
	if code == s.notReadyLogged {
		return code
	}
	s.notReadyLogged = code
	if code == "" {
		s.logger.Info("readiness_restored")
	} else {
		s.logger.Warn("not_ready", "reason", code, "detail", detail)
	}
	return code
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.metrics.WritePrometheus(w)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
