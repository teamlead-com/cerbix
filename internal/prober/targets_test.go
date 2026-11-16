package prober

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-2 H-2 (invariant S2): the prober parses its target again before it dials, so a
// value that bypassed write-time validation still never reaches a socket.

func countingDial(n *int32) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt32(n, 1)
		return nil, &net.OpError{Op: "dial", Err: errDialRefusedInTest}
	}
}

type dialRefused struct{}

func (dialRefused) Error() string { return "dial refused in test" }

var errDialRefusedInTest = dialRefused{}

func TestProbersRefuseHostileTargetsBeforeDialing(t *testing.T) {
	var dials int32
	dial := countingDial(&dials)
	var httpCalls int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&httpCalls, 1)
		return nil, errDialRefusedInTest
	})}
	cases := []struct {
		name  string
		probe func() Result
	}{
		{"websocket CRLF", func() Result {
			return websocketProber{dial: dial}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorWebSocket, Target: "ws://h/a\r\nX-Evil: 1"})
		}},
		{"websocket no scheme", func() Result {
			return websocketProber{dial: dial}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorWebSocket, Target: "h.internal/ws"})
		}},
		{"ssh", func() Result {
			return sshProber{dial: dial}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorSSH, Target: "h\r\n"})
		}},
		{"redis", func() Result {
			return redisProber{dial: dial}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRedis, Target: "h:abc"})
		}},
		{"rabbit amqp", func() Result {
			return rabbitmqProber{dial: dial, client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRabbitMQ, Target: "h x"})
		}},
		{"rabbit management path", func() Result {
			return rabbitmqProber{dial: dial, client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRabbitMQ,
				Target: "rabbit:15672", Config: map[string]string{"mode": "management", "path": "@10.0.0.9:8080/x"}})
		}},
		{"promql CRLF", func() Result {
			return promqlProber{client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorPromQL,
				Target: "http://10.0.0.9:9090\r\n", Config: map[string]string{"query": "up"}})
		}},
		{"promql query in the base", func() Result {
			return promqlProber{client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorPromQL,
				Target: "http://10.0.0.9:9090?x=1", Config: map[string]string{"query": "up"}})
		}},
		{"promql no scheme", func() Result {
			return promqlProber{client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorPromQL,
				Target: "10.0.0.9:9090", Config: map[string]string{"query": "up"}})
		}},
		{"rabbit management target CRLF", func() Result {
			return rabbitmqProber{dial: dial, client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRabbitMQ,
				Target: "http://10.0.0.9:15672\r\n", Config: map[string]string{"mode": "management"}})
		}},
		{"rabbit management target userinfo", func() Result {
			return rabbitmqProber{dial: dial, client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRabbitMQ,
				Target: "http://u:p@10.0.0.9:15672", Config: map[string]string{"mode": "management"}})
		}},
		{"rabbit management target port", func() Result {
			return rabbitmqProber{dial: dial, client: client}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRabbitMQ,
				Target: "HTTP://10.0.0.9:99999", Config: map[string]string{"mode": "management"}})
		}},
	}
	for _, tc := range cases {
		res := tc.probe()
		if res.Connected {
			t.Errorf("%s: connected", tc.name)
		}
		if strings.Contains(res.Msg, "X-Evil") || strings.Contains(res.Msg, "10.0.0.9") {
			t.Errorf("%s: the message echoes the input: %q", tc.name, res.Msg)
		}
	}
	if dials != 0 || httpCalls != 0 {
		t.Fatalf("hostile targets reached the network: %d dial(s), %d HTTP call(s)", dials, httpCalls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// wsHandshakeHandler answers a valid RFC 6455 upgrade.
func wsHandshakeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAccept(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n")
		_ = buf.Flush()
	})
}

func TestWebSocketSelfSignedIsDownByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(wsHandshakeHandler())
	defer srv.Close()
	p := websocketProber{dial: (&net.Dialer{}).DialContext}
	m := domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorWebSocket, Target: "wss://" + srv.Listener.Addr().String() + "/ws"}
	if res := p.Probe(context.Background(), m); res.Connected || !strings.HasPrefix(res.Msg, "tls:") {
		t.Fatalf("self-signed without tls_skip_verify: %+v, want DOWN with a tls: message", res)
	}
	m.Config = map[string]string{"tls_skip_verify": "true"}
	if res := p.Probe(context.Background(), m); !res.Connected {
		t.Fatalf("self-signed with tls_skip_verify: %+v, want UP", res)
	}
}

// The request bytes the prober writes: a real request line and Host header, built by net/http.
func TestWebSocketHandshakeWire(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		ln, err := net.Listen(network, map[string]string{"tcp4": "127.0.0.1:0", "tcp6": "[::1]:0"}[network])
		if err != nil {
			if network == "tcp6" {
				t.Logf("no IPv6 loopback: %v", err)
				continue
			}
			t.Fatal(err)
		}
		got := make(chan *http.Request, 1)
		go func() {
			// Closed on every exit, so the read below never hangs when Accept or the read fails
			// (external review of iter-0210, P3-1).
			defer close(got)
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			req, err := http.ReadRequest(bufio.NewReader(c))
			if err != nil {
				return
			}
			got <- req
			_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Accept: " + wsAccept(req.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"))
		}()
		addr := ln.Addr().String()
		res := websocketProber{dial: (&net.Dialer{}).DialContext}.Probe(context.Background(),
			domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorWebSocket, Target: "ws://" + addr + "/chat?x=1"})
		_ = ln.Close()
		if !res.Connected {
			t.Fatalf("%s: handshake failed: %+v", network, res)
		}
		req, ok := <-got
		if !ok || req == nil {
			t.Fatalf("%s: the fixture read no request", network)
		}
		if req.Method != http.MethodGet || req.RequestURI != "/chat?x=1" || req.Host != addr {
			t.Fatalf("%s: request line %s %s, Host %q; want GET /chat?x=1, Host %q", network, req.Method, req.RequestURI, req.Host, addr)
		}
		if req.Header.Get("Upgrade") != "websocket" || req.Header.Get("Sec-WebSocket-Version") != "13" {
			t.Fatalf("%s: upgrade headers missing: %v", network, req.Header)
		}
	}
}

func TestRabbitMgmtURLAssembled(t *testing.T) {
	for _, tc := range []struct {
		target, path string
		tls          bool
		want         string
	}{
		{"rabbit", "", false, "http://rabbit:15672/api/overview"},
		{"rabbit", "", true, "https://rabbit:15671/api/overview"},
		{"https://rabbit:15671/base", "/api/overview?columns=x", true, "https://rabbit:15671/base/api/overview?columns=x"}, // a base path (reverse-proxy prefix) is kept
		{"http://rabbit:15672", "/api/healthchecks/node", false, "http://rabbit:15672/api/healthchecks/node"},
	} {
		got, err := rabbitMgmtURL(tc.target, tc.path, tc.tls)
		if err != nil || got != tc.want {
			t.Errorf("rabbitMgmtURL(%q, %q, %v) = %q (%v), want %q", tc.target, tc.path, tc.tls, got, err, tc.want)
		}
	}
	// "api/overview": a relative path is refused, not silently given its "/" (external review of
	// iter-0210, P2-1); migration 00114 wrote the "/" into existing rows.
	for _, bad := range []string{"@10.0.0.9:8080/x", "//evil/x", "/a\r\n", "api/overview", "api/aliveness-test/%2F"} {
		if got, err := rabbitMgmtURL("rabbit", bad, false); err == nil {
			t.Errorf("path %q accepted: %q", bad, got)
		}
	}
}

// Review of iter-0210: a percent-encoded path must reach the URL byte for byte. `%2F` is RabbitMQ's
// default vhost (`/api/aliveness-test/%2F`) and `%20` a space in a vhost name; assembling the URL
// from the decoded Path alone re-encoded them as `%252F` / `%2520`, and those monitors went DOWN.
func TestRabbitMgmtURLKeepsPercentEncoding(t *testing.T) {
	for _, tc := range []struct {
		target, path string
		tls          bool
		want         string
	}{
		{"rabbit", "/api/aliveness-test/%2F", false, "http://rabbit:15672/api/aliveness-test/%2F"},
		{"rabbit:15672", "/api/queues/my%20vhost/q", false, "http://rabbit:15672/api/queues/my%20vhost/q"},
		{"rabbit", "/api/aliveness-test/%2F?columns=status", true, "https://rabbit:15671/api/aliveness-test/%2F?columns=status"},
		{"https://rabbit:15671/base", "/api/aliveness-test/%2F", true, "https://rabbit:15671/base/api/aliveness-test/%2F"},
		{"https://rabbit:15671/base/", "/api/queues/my%20vhost/q?columns=name", true, "https://rabbit:15671/base/api/queues/my%20vhost/q?columns=name"},
		{"http://rabbit:15672/pre%2Ffix", "/api/aliveness-test/%2F", false, "http://rabbit:15672/pre%2Ffix/api/aliveness-test/%2F"},
	} {
		got, err := rabbitMgmtURL(tc.target, tc.path, tc.tls)
		if err != nil || got != tc.want {
			t.Errorf("rabbitMgmtURL(%q, %q, %v) = %q (%v), want %q", tc.target, tc.path, tc.tls, got, err, tc.want)
		}
	}
}

// The same, on the wire: what the management API receives is the configured path, byte for byte.
func TestRabbitMgmtPathReachesTheWireByteForByte(t *testing.T) {
	var gotURI, gotEscaped string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotEscaped = r.RequestURI, r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := rabbitmqProber{client: srv.Client()}
	for _, tc := range []struct{ target, path, uri, escaped string }{
		{srv.URL, "/api/aliveness-test/%2F", "/api/aliveness-test/%2F", "/api/aliveness-test/%2F"},
		{srv.URL, "/api/queues/my%20vhost/q?columns=name", "/api/queues/my%20vhost/q?columns=name", "/api/queues/my%20vhost/q"},
		{srv.URL + "/rabbit", "/api/aliveness-test/%2F", "/rabbit/api/aliveness-test/%2F", "/rabbit/api/aliveness-test/%2F"},
		{strings.TrimPrefix(srv.URL, "http://"), "/api/aliveness-test/%2F", "/api/aliveness-test/%2F", "/api/aliveness-test/%2F"},
	} {
		gotURI, gotEscaped = "", ""
		res := p.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRabbitMQ, Target: tc.target,
			Config: map[string]string{"mode": "management", "path": tc.path}})
		if !res.Connected {
			t.Errorf("%s + %s: %+v", tc.target, tc.path, res)
			continue
		}
		if gotURI != tc.uri || gotEscaped != tc.escaped {
			t.Errorf("%s + %s: the server received %q (escaped path %q), want %q (%q)", tc.target, tc.path, gotURI, gotEscaped, tc.uri, tc.escaped)
		}
	}
}

// An invalid percent sequence is refused by the prober too, without echoing the input.
func TestRabbitMgmtInvalidEscapeRefused(t *testing.T) {
	for _, bad := range []string{"/api/%zz", "/api/aliveness-test/%2", "/api/x?y=%zz"} {
		got, err := rabbitMgmtURL("rabbit", bad, false)
		if err == nil {
			t.Errorf("path %q accepted: %q", bad, got)
			continue
		}
		if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), "%zz") {
			t.Errorf("path %q: the error echoes the input: %v", bad, err)
		}
	}
}

// External review of iter-0210 (P1-1): a management-mode target gets the SAME verdict from the
// write surfaces (Monitor.Validate) and from the prober (rabbitMgmtURL) — one parser,
// domain.ParseRabbitMgmtTarget, behind both.
func TestRabbitMgmtTargetSameVerdictOnWriteAndProbe(t *testing.T) {
	for _, tc := range []struct {
		target, tls string
		ok          bool
		want        string
	}{
		{"HTTPS://rabbit:15671", "true", true, "https://rabbit:15671/api/overview"},
		{"Http://rabbit:15672/base", "", true, "http://rabbit:15672/base/api/overview"},
		{"rabbit", "true", true, "https://rabbit:15671/api/overview"},
		{"http://rabbit:15672", "true", false, ""},
		{"rabbit\r\n", "", false, ""},
		{"http://rabbit:15672\r\n", "", false, ""},
		{"http://rabbit:15672/my base", "", false, ""},
		{"http://rabbit:99999", "", false, ""},
		{"rabbit:99999", "", false, ""},
		{"http://rabbit:", "", false, ""},
		{"http://rabbit:15672?x=1", "", false, ""},
		{"http://rabbit:15672/#f", "", false, ""},
	} {
		cfg := map[string]string{"mode": "management", "path": "/api/overview"}
		if tc.tls != "" {
			cfg["tls"] = tc.tls
		}
		m := domain.Monitor{RecoveryThreshold: 1, ProjectID: "p", Name: "n", Type: domain.MonitorRabbitMQ, Target: tc.target,
			IntervalSeconds: 60, TimeoutSeconds: 10, Config: cfg}
		writeErr := m.Validate()
		got, probeErr := rabbitMgmtURL(m.Target, cfg["path"], cfg["tls"] == "true")
		if tc.ok {
			if writeErr != nil || probeErr != nil || got != tc.want {
				t.Errorf("%q tls=%q: write %v, probe %q (%v); want accepted as %q", tc.target, tc.tls, writeErr, got, probeErr, tc.want)
			}
			continue
		}
		if writeErr == nil || probeErr == nil {
			t.Errorf("%q tls=%q: write %v, probe %q (%v); want refused by both", tc.target, tc.tls, writeErr, got, probeErr)
		}
	}
}

// External review of iter-0210 (P2-2): the PromQL query URL is a url.URL built on the base the
// operator configured — its reverse-proxy prefix kept, percent-encoding and all — never a string
// concatenation, and the base is parsed again before the request.
func TestPromQLQueryURLKeepsTheBasePath(t *testing.T) {
	var gotURI, gotEscaped, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotEscaped, gotQuery = r.RequestURI, r.URL.EscapedPath(), r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[0,"1"]}}`))
	}))
	defer srv.Close()
	p := promqlProber{client: srv.Client()}
	for _, tc := range []struct{ target, escaped string }{
		{srv.URL, "/api/v1/query"},
		{srv.URL + "/", "/api/v1/query"},
		{srv.URL + "/prometheus", "/prometheus/api/v1/query"},
		{srv.URL + "/prometheus//", "/prometheus/api/v1/query"},
		{srv.URL + "/pre%2Ffix", "/pre%2Ffix/api/v1/query"},
	} {
		gotURI, gotEscaped, gotQuery = "", "", ""
		res := p.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorPromQL, Target: tc.target,
			Config: map[string]string{"query": `sum(rate(x{a="b c"}[5m]))`}})
		if !res.Connected || res.Value != 1 {
			t.Errorf("%s: %+v", tc.target, res)
			continue
		}
		if gotEscaped != tc.escaped || gotQuery != `sum(rate(x{a="b c"}[5m]))` {
			t.Errorf("%s: the server received %q (escaped path %q, query %q), want path %q", tc.target, gotURI, gotEscaped, gotQuery, tc.escaped)
		}
	}
}

// D-0145: a promql row stored with URL userinfo before the write rule keeps probing — the write
// surfaces refuse it at its next edit, the prober does not turn it DOWN. Its credentials still go to
// the host they were written for, and nowhere else.
func TestPromQLLegacyUserinfoTargetKeepsProbing(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[0,"1"]}}`))
	}))
	defer srv.Close()
	target := strings.Replace(srv.URL, "http://", "http://legacy:pw@", 1)
	res := promqlProber{client: srv.Client()}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorPromQL,
		Target: target, Config: map[string]string{"query": "up"}})
	if !res.Connected || gotUser != "legacy" || gotPass != "pw" {
		t.Fatalf("legacy userinfo target: %+v, basic auth %q/%q", res, gotUser, gotPass)
	}
	if strings.Contains(res.Msg, "pw") {
		t.Fatalf("the message carries the password: %q", res.Msg)
	}
}

// Second external review of iter-0210 (P2-1): the http prober parses its target with
// domain.ParseHTTPTarget before it builds a request, so the write surfaces and the probe give one
// verdict. A refused target reaches no network, and the message carries no URL: before, the
// NewRequest error went into Msg as `parse "http://user:secret@…": …`.
func TestHTTPTargetSameVerdictOnWriteAndProbe(t *testing.T) {
	var calls int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errDialRefusedInTest
	})}
	for _, typ := range []domain.MonitorType{domain.MonitorHTTP, domain.MonitorPromQL} {
		for _, target := range []string{
			"http://h.example/#/dashboard\r\n",
			"http://:8080/health",
			"http://h.example:99999/health",
			"http://user:secret@h.example/%zz",
			"http://user:secret@h.example\x7f/",
		} {
			m := domain.Monitor{RecoveryThreshold: 1, ProjectID: "p", Name: "n", Type: typ, Target: target, IntervalSeconds: 60, TimeoutSeconds: 10,
				Config: map[string]string{"query": "up"}}
			if typ == domain.MonitorHTTP {
				m.Config = nil
			}
			if err := m.Validate(); err == nil {
				t.Errorf("%s %q: accepted on write", typ, target)
			}
			var res Result
			if typ == domain.MonitorHTTP {
				res = httpProber{client: client}.Probe(context.Background(), m)
			} else {
				res = promqlProber{client: client}.Probe(context.Background(), m)
			}
			if res.Connected || !strings.HasPrefix(res.Msg, "bad target: ") {
				t.Errorf("%s %q: probe %+v, want a bad target refusal", typ, target, res)
			}
			if strings.Contains(res.Msg, "://") || strings.Contains(res.Msg, "secret") || strings.Contains(res.Msg, "h.example") {
				t.Errorf("%s %q: the message carries the URL: %q", typ, target, res.Msg)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("refused targets reached the network %d time(s)", calls)
	}
}

// D-0145, for the http prober now that it re-parses its target: a row stored with URL userinfo
// before the write rule keeps probing (net/http sends it as Basic Auth), and the message never
// carries the password.
func TestHTTPLegacyUserinfoTargetKeepsProbing(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	target := strings.Replace(srv.URL, "http://", "http://legacy:pw@", 1) + "/health"
	res := httpProber{client: srv.Client()}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorHTTP, Target: target})
	if !res.Connected || res.Code != http.StatusNoContent || gotUser != "legacy" || gotPass != "pw" {
		t.Fatalf("legacy userinfo target: %+v, basic auth %q/%q", res, gotUser, gotPass)
	}
}

// Second external review of iter-0210 (P2-2): a base path that ENDS in an escaped slash. Trimming
// "/" from both the decoded Path ("/vhosts/") and the escaped one ("/vhosts/%2F") made
// unescape(RawPath) != Path, so net/url dropped RawPath and re-encoded Path: the %2F of the base and
// the %2F of the path were both lost. One function, joinURLPath, trims from Path exactly as many "/"
// as it trims LITERAL "/" from the escaped path, for rabbitMgmtURL and promQueryURL alike.
func TestBasePathEndingInAnEscapedSlashReachesTheWire(t *testing.T) {
	var gotEscaped string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[0,"1"]}}`))
	}))
	defer srv.Close()
	for _, tc := range []struct{ base, rabbit, prom string }{
		{"/vhosts/%2F", "/vhosts/%2F/api/aliveness-test/%2F", "/vhosts/%2F/api/v1/query"},
		{"/vhosts/%2F/", "/vhosts/%2F/api/aliveness-test/%2F", "/vhosts/%2F/api/v1/query"},
		{"/tenants/%2f", "/tenants/%2f/api/aliveness-test/%2F", "/tenants/%2f/api/v1/query"},
		{"/base//", "/base/api/aliveness-test/%2F", "/base/api/v1/query"},
	} {
		gotEscaped = ""
		res := rabbitmqProber{client: srv.Client()}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorRabbitMQ,
			Target: srv.URL + tc.base, Config: map[string]string{"mode": "management", "path": "/api/aliveness-test/%2F"}})
		if !res.Connected || gotEscaped != tc.rabbit {
			t.Errorf("rabbitmq base %q: the server received %q (%+v), want %q", tc.base, gotEscaped, res, tc.rabbit)
		}
		gotEscaped = ""
		res = promqlProber{client: srv.Client()}.Probe(context.Background(), domain.Monitor{RecoveryThreshold: 1, Type: domain.MonitorPromQL,
			Target: srv.URL + tc.base, Config: map[string]string{"query": "up"}})
		if !res.Connected || gotEscaped != tc.prom {
			t.Errorf("promql base %q: the server received %q (%+v), want %q", tc.base, gotEscaped, res, tc.prom)
		}
	}
}

// The invariant joinURLPath keeps for every base and reference: unescape(RawPath) == Path, so
// net/url keeps RawPath and sends exactly the escaped base without its trailing literal "/",
// followed by the escaped reference.
func TestJoinURLPathKeepsRawPathConsistent(t *testing.T) {
	bases := []string{"", "/", "//", "/a", "/a/", "/a//", "/a%2Fb", "/a%2F", "/a%2F/", "/a%2f//", "/%2F", "/%2F%2F/",
		"/a%20b/", "/pre%252F", "/%C3%BC/", "/a%2F%2F"}
	refs := []string{"/api/aliveness-test/%2F", "/api/v1/query", "/api/queues/my%20vhost/q", "/"}
	for _, b := range bases {
		base, err := url.Parse("http://h" + b)
		if err != nil {
			t.Fatalf("base %q: %v", b, err)
		}
		for _, r := range refs {
			ref, err := url.Parse(r)
			if err != nil {
				t.Fatalf("ref %q: %v", r, err)
			}
			path, raw := joinURLPath(base, ref)
			if un, err := url.PathUnescape(raw); err != nil || un != path {
				t.Errorf("base %q + %q: unescape(%q) = %q (%v), Path %q", b, r, raw, un, err, path)
			}
			want := strings.TrimRight(base.EscapedPath(), "/") + ref.EscapedPath()
			if got := (&url.URL{Path: path, RawPath: raw}).EscapedPath(); got != want || raw != want {
				t.Errorf("base %q + %q: escaped %q (raw %q), want %q", b, r, got, raw, want)
			}
		}
	}
}
