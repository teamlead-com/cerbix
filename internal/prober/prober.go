// Package prober executes monitor checks. Each monitor type has a Prober; the
// Runner resolves the prober, applies timeout and retries, evaluates the
// monitor's conditions, and produces a Heartbeat. Probers are stateless.
package prober

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const maxBodyBytes = 1 << 20 // 1 MiB cap on read body for condition evaluation

// Result is the raw outcome of a single probe attempt.
type Result struct {
	Connected      bool
	Code           int
	Body           string
	LatencyMS      int64
	CertExpiryDays int64   // TLS: days until the leaf certificate expires (may be negative)
	Value          float64 // PromQL: the scalar result value, for [RESULT]
	Msg            string
}

// Prober executes one probe attempt for a monitor.
type Prober interface {
	Probe(ctx context.Context, m domain.Monitor) Result
}

// ChildStatusFunc returns the current status of the given monitor ids. Injected so
// the composite prober can read child statuses without the prober package
// depending on the store.
type ChildStatusFunc func(ctx context.Context, ids []string) (map[string]domain.MonitorStatus, error)

// Runner owns the prober registry and the check execution policy.
type Runner struct {
	registry    map[domain.MonitorType]Prober
	clock       func() time.Time
	childStatus ChildStatusFunc
}

// WithChildStatus wires the lookup composite monitors use to read child statuses.
func (r *Runner) WithChildStatus(fn ChildStatusFunc) *Runner {
	r.childStatus = fn
	return r
}

// NewRunner builds a Runner with the default SSRF guard (private allowed,
// metadata/link-local blocked) — the safe default for this internal tool.
func NewRunner() *Runner {
	return NewRunnerWithGuard(defaultGuard())
}

// NewRunnerWithGuard builds a Runner whose probers enforce the given SSRF policy.
// The HTTP client dials through the guard (so every redirect hop is checked); the
// TCP and ICMP probers validate the resolved target IP before connecting.
func NewRunnerWithGuard(g Guard) *Runner {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = g.dialContext(&net.Dialer{})
	transport.Proxy = nil // never route probes through a proxy; it would bypass the IP guard
	r := &Runner{clock: time.Now}
	r.registry = map[domain.MonitorType]Prober{
		domain.MonitorHTTP:      httpProber{client: &http.Client{Transport: transport}},
		domain.MonitorTCP:       tcpProber{dial: g.dialContext(&net.Dialer{})},
		domain.MonitorICMP:      icmpProber{guard: g},
		domain.MonitorDNS:       dnsProber{resolver: net.DefaultResolver},
		domain.MonitorTLS:       tlsProber{dial: g.dialContext(&net.Dialer{}), clock: time.Now},
		domain.MonitorGRPC:      grpcProber{dial: g.dialContext(&net.Dialer{})},
		domain.MonitorComposite: compositeProber{r: r},
		domain.MonitorPostgres:  pgProber{dial: g.dialContext(&net.Dialer{})},
		domain.MonitorMySQL:     mysqlProber{},
		domain.MonitorRedis:     redisProber{dial: g.dialContext(&net.Dialer{})},
		domain.MonitorPromQL:    promqlProber{client: &http.Client{Transport: transport}},
		domain.MonitorRabbitMQ:  rabbitmqProber{dial: g.dialContext(&net.Dialer{}), client: &http.Client{Transport: transport}},
		domain.MonitorWebSocket: websocketProber{dial: g.dialContext(&net.Dialer{})},
		domain.MonitorSSH:       sshProber{dial: g.dialContext(&net.Dialer{})},
		domain.MonitorSynthetic: syntheticProber{client: &http.Client{Transport: transport}},
	}
	// MySQL's driver dialer registry is global; point it at this runner's guard.
	dial := g.dialContext(&net.Dialer{})
	setMySQLGuardDial(func(ctx context.Context, addr string) (net.Conn, error) {
		return dial(ctx, "tcp", addr)
	})
	return r
}

// Run executes a monitor check (timeout + retries + conditions) and returns a
// heartbeat. A check succeeds when the probe connects and all conditions pass;
// with no conditions the default is a 2xx status (HTTP) or a successful connect.
func (r *Runner) Run(ctx context.Context, m domain.Monitor) domain.Heartbeat {
	prober, ok := r.registry[m.Type]
	if !ok {
		return domain.Heartbeat{MonitorID: m.ID, Ts: r.clock(), ExecutionRevision: m.ExecutionRevision, Up: false, Msg: fmt.Sprintf("unsupported monitor type %q", m.Type)}
	}

	attempts := m.Retries + 1
	var res Result
	for i := 0; i < attempts; i++ {
		cctx, cancel := context.WithTimeout(ctx, m.Timeout())
		res = prober.Probe(cctx, m)
		cancel()

		up, msg := r.judge(m, res)
		if up {
			return domain.Heartbeat{MonitorID: m.ID, Ts: r.clock(), ExecutionRevision: m.ExecutionRevision, Up: true, LatencyMS: res.LatencyMS, Code: res.Code, Msg: msg}
		}
		// Retry only makes sense if the context is still alive.
		if ctx.Err() != nil {
			break
		}
	}
	_, msg := r.judge(m, res)
	if msg == "" {
		msg = res.Msg
	}
	return domain.Heartbeat{MonitorID: m.ID, Ts: r.clock(), ExecutionRevision: m.ExecutionRevision, Up: false, LatencyMS: res.LatencyMS, Code: res.Code, Msg: msg}
}

// judge decides up/down for a probe result and returns an explanatory message.
func (r *Runner) judge(m domain.Monitor, res Result) (bool, string) {
	if !res.Connected {
		if res.Msg != "" {
			return false, res.Msg
		}
		return false, "connection failed"
	}
	if len(m.Conditions) == 0 {
		if m.Type == domain.MonitorHTTP {
			if res.Code >= 200 && res.Code < 300 {
				return true, ""
			}
			return false, fmt.Sprintf("unexpected status %d", res.Code)
		}
		return true, ""
	}
	pass, failed, err := EvaluateAll(m.Conditions, Values{
		Status:         res.Code,
		ResponseTimeMS: res.LatencyMS,
		Body:           res.Body,
		Connected:      res.Connected,
		CertExpiryDays: res.CertExpiryDays,
		Result:         res.Value,
	})
	if err != nil {
		return false, "condition error: " + err.Error()
	}
	if !pass {
		return false, "failed condition: " + failed
	}
	return true, ""
}

// httpProber probes an HTTP(S) endpoint.
type httpProber struct{ client *http.Client }

func (p httpProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	method := m.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, m.Target, nil)
	if err != nil {
		return Result{Connected: false, Msg: "bad request: " + err.Error()}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	return Result{
		Connected: true,
		Code:      resp.StatusCode,
		Body:      string(body),
		LatencyMS: elapsedMS(start),
	}
}

// tcpProber checks that a TCP address accepts a connection. It dials through the
// SSRF guard's DialContext so the resolved target IP is policy-checked.
type tcpProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p tcpProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	conn, err := p.dial(ctx, "tcp", m.Target)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	_ = conn.Close()
	return Result{Connected: true, LatencyMS: elapsedMS(start)}
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
