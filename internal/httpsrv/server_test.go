package httpsrv

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

func newTestServer(ready bool) *Server {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
	reg.SetReady(ready, "")
	return New(config.ServerConfig{
		Listen:      ":0",
		HealthzPath: "/healthz",
		ReadyzPath:  "/readyz",
		MetricsPath: "/metrics",
	}, reg, nil)
}

func TestHealthzAlwaysOK(t *testing.T) {
	s := newTestServer(false)
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", rec.Code)
	}
}

func TestReadyzReflectsRegistry(t *testing.T) {
	notReady := newTestServer(false)
	rec := httptest.NewRecorder()
	notReady.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready readyz code = %d", rec.Code)
	}

	ready := newTestServer(true)
	rec = httptest.NewRecorder()
	ready.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready readyz code = %d", rec.Code)
	}
}

func TestMetricsHandlerServesText(t *testing.T) {
	s := newTestServer(true)
	rec := httptest.NewRecorder()
	s.metricsHandler(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Fatalf("metrics content-type = %q", ct)
	}
}
