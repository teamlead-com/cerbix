package httpsrv

// S12 (sec-hardening-2 H-12): one predicate feeds Ready(), LastError() and the cerbix_ready gauge,
// and it includes the broker: with the broker down an AMQP-role process answers 503
// broker_unreachable and exports cerbix_ready 0. The registry is fed exactly as the AMQP role feeds
// it — cli wires dispatch.AMQP.WithBrokerState(registry.SetBrokerUp), and the dispatcher's
// supervisor calls it with false on broker loss (TestBrokerStateHook).
//
// S11's last clause lives here too: no 503 body carries driver or infrastructure error text — the
// body is a stable reason code; the raw reason goes to the log.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

func opsServer(reg *metrics.Registry) *Server {
	return New(config.ServerConfig{Listen: ":0", HealthzPath: "/healthz", ReadyzPath: "/readyz", MetricsPath: "/metrics"}, reg, nil)
}

type readyzAnswer struct {
	code   int
	reason string
	body   string
	gauge  string
}

func scrape(t *testing.T, s *Server) readyzAnswer {
	t.Helper()
	rec := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	m := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(m, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	gauge := ""
	for _, line := range strings.Split(m.Body.String(), "\n") {
		if strings.HasPrefix(line, "cerbix_ready ") {
			gauge = strings.TrimPrefix(line, "cerbix_ready ")
		}
	}
	return readyzAnswer{code: rec.Code, reason: body.Error, body: rec.Body.String(), gauge: gauge}
}

func TestABrokerOutageFailsReadinessAndTheGauge(t *testing.T) {
	reg := metrics.New(buildinfo.Info{Version: "test"}, "worker")
	s := opsServer(reg)
	reg.SetReady(true, "")
	reg.SetBrokerUp(true) // WithBrokerState reports up on wiring

	if a := scrape(t, s); a.code != http.StatusOK || a.gauge != "1" {
		t.Fatalf("broker up: readyz %d, cerbix_ready %s — want 200 and 1", a.code, a.gauge)
	}
	reg.SetBrokerUp(false) // the supervisor's broker_lost
	a := scrape(t, s)
	if a.code != http.StatusServiceUnavailable || a.reason != "broker_unreachable" {
		t.Errorf("broker down: readyz %d %s, want 503 broker_unreachable", a.code, a.body)
	}
	if a.gauge != "0" {
		t.Errorf("broker down: cerbix_ready %s, want 0", a.gauge)
	}
	if reg.Ready() || reg.LastError() != "broker_unreachable" {
		t.Errorf("broker down: Ready() = %v, LastError() = %q", reg.Ready(), reg.LastError())
	}
	reg.SetBrokerUp(true) // broker_reconnected
	if a := scrape(t, s); a.code != http.StatusOK || a.gauge != "1" {
		t.Errorf("broker back: readyz %d, cerbix_ready %s — want 200 and 1", a.code, a.gauge)
	}
}

// Every not-ready state: Ready(), LastError(), the gauge and /readyz give one answer, and the 503
// body is a code from the closed vocabulary — never the raw reason a component recorded.
func TestOnePredicateFeedsReadyLastErrorAndTheGauge(t *testing.T) {
	const raw = "dial tcp 10.20.30.40:5432: connect: connection refused (pq: password authentication failed for user \"cerbix\")"
	cases := []struct {
		name string
		set  func(*metrics.Registry)
		want string // "" = ready
	}{
		{"ready", func(r *metrics.Registry) { r.SetReady(true, "") }, ""},
		{"starting", func(*metrics.Registry) {}, "starting"},
		{"shutting down", func(r *metrics.Registry) { r.SetReady(true, ""); r.SetReady(false, "shutting down") }, "shutting_down"},
		{"database", func(r *metrics.Registry) { r.SetReady(true, ""); r.SetDatabaseUp(false) }, "database_unreachable"},
		{"broker", func(r *metrics.Registry) { r.SetReady(true, ""); r.SetBrokerUp(false) }, "broker_unreachable"},
		{"credential", func(r *metrics.Registry) { r.SetReady(true, ""); r.SetCredentialReady(false, raw) }, "credential_unavailable"},
		{"service wedged", func(r *metrics.Registry) { r.SetReady(true, ""); r.SetServiceWedged(true, raw) }, "service_wedged"},
		{"alert stalled", func(r *metrics.Registry) { r.SetReady(true, ""); r.SetServiceAlertStalled("burn", true, raw) }, "alert_evaluation_stalled"},
		{"database back", func(r *metrics.Registry) { r.SetReady(true, ""); r.SetDatabaseUp(false); r.SetDatabaseUp(true) }, ""},
		{"credential back", func(r *metrics.Registry) {
			r.SetReady(true, "")
			r.SetCredentialReady(false, raw)
			r.SetCredentialReady(true, "")
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := metrics.New(buildinfo.Info{Version: "test"}, "all")
			c.set(reg)
			s := opsServer(reg)
			a := scrape(t, s)
			if c.want == "" {
				if !reg.Ready() || reg.LastError() != "" || a.code != http.StatusOK || a.gauge != "1" {
					t.Errorf("Ready() %v, LastError() %q, readyz %d, cerbix_ready %s — want ready everywhere", reg.Ready(), reg.LastError(), a.code, a.gauge)
				}
				return
			}
			if reg.Ready() || reg.LastError() != c.want || a.code != http.StatusServiceUnavailable || a.reason != c.want || a.gauge != "0" {
				t.Errorf("Ready() %v, LastError() %q, readyz %d %s, cerbix_ready %s — want not ready, %q everywhere",
					reg.Ready(), reg.LastError(), a.code, a.body, a.gauge, c.want)
			}
			if strings.Contains(a.body, "dial tcp") || strings.Contains(a.body, "10.20.30.40") {
				t.Errorf("the 503 body carries the raw reason: %s", a.body)
			}
		})
	}
}

// Review of iter-0212, P3-2: /readyz logs each change of the readiness CODE once — not_ready with
// the code and the raw detail when it starts, readiness_restored when it ends — and nothing while
// the code holds, even if the detail ticks ("lagging 300s", "lagging 310s", …). Deleting the
// logging turns this test red: it is the only record of the raw reason.
func TestReadinessChangesAreLoggedOncePerCode(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.WriteString(string(p))
	}), nil))
	reg := metrics.New(buildinfo.Info{Version: "test"}, "scheduler")
	reg.SetReady(true, "")
	s := opsServer(reg).WithLogger(logger)
	lines := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}

	scrape(t, s) // ready: nothing to say
	if got := lines(); len(got) != 0 {
		t.Fatalf("a ready probe logged %v", got)
	}
	for _, lag := range []string{"300s", "310s", "320s"} {
		reg.SetServiceAlertStalled("burn", true, "service burn alert evaluation lagging "+lag+" (bound 180s)")
		scrape(t, s)
	}
	got := lines()
	if len(got) != 1 || !strings.Contains(got[0], "msg=not_ready") || !strings.Contains(got[0], "reason=alert_evaluation_stalled") || !strings.Contains(got[0], "lagging 300s") {
		t.Fatalf("three probes of one stall (ticking detail) logged %d lines, want one not_ready with the first detail: %v", len(got), got)
	}
	reg.SetServiceAlertStalled("burn", false, "")
	scrape(t, s)
	scrape(t, s)
	got = lines()
	if len(got) != 2 || !strings.Contains(got[1], "msg=readiness_restored") {
		t.Fatalf("recovery logged %v, want one readiness_restored after the not_ready", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
