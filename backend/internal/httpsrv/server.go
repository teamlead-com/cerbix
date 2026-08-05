// Package httpsrv serves the cerbix operational endpoints: health, readiness,
// and Prometheus metrics. Application routes are mounted by higher layers.
package httpsrv

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

// Server wraps the operational HTTP server.
type Server struct {
	server  *http.Server
	metrics *metrics.Registry
}

// New builds the operational server from config and a metrics registry. If app
// is non-nil it is mounted as the catch-all handler (application routes such as
// /api and /auth); the operational endpoints always take precedence.
func New(cfg config.ServerConfig, registry *metrics.Registry, app http.Handler) *Server {
	mux := http.NewServeMux()
	s := &Server{
		server: &http.Server{
			Addr:        cfg.Listen,
			Handler:     mux,
			ReadTimeout: config.HTTPReadTimeout,
		},
		metrics: registry,
	}
	mux.HandleFunc(cfg.HealthzPath, s.healthz)
	mux.HandleFunc(cfg.ReadyzPath, s.readyz)
	mux.HandleFunc(cfg.MetricsPath, s.metricsHandler)
	if app != nil {
		mux.Handle("/", app)
	}
	return s
}

// ListenAndServe blocks serving requests until the server is closed.
func (s *Server) ListenAndServe() error {
	err := s.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.metrics.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"error":  s.metrics.LastError(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.metrics.WritePrometheus(w)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
