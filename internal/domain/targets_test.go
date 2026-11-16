package domain

import (
	"net/url"
	"strings"
	"testing"
)

// sec-hardening-2 H-2 (invariant S2): no byte of monitor.target or monitor.config reaches a socket or
// a URL without passing through that protocol's parser.

func TestParseWebSocketTarget(t *testing.T) {
	for _, ok := range []string{"ws://h/x", "wss://h:8443/a?b=1", "http://h", "https://[::1]:9/x", "  WSS://H/x  "} {
		if _, err := ParseWebSocketTarget(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"h:80/x", "ftp://h", "ws://h/a\r\nX: y", "ws:// h", "ws://", "ws://u:p@h", "ws://h:99999",
		"ws://h:0", "ws://h/#f", "ws://h/a b", "ws://h\x00/x",
	} {
		if _, err := ParseWebSocketTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	u, err := ParseWebSocketTarget("wss://[::1]:9/x?y=1")
	if err != nil || u.Hostname() != "::1" || u.Port() != "9" || u.Scheme != "wss" {
		t.Fatalf("ipv6: %+v (%v)", u, err)
	}
}

func TestValidateSocketTarget(t *testing.T) {
	for _, ok := range []string{"redis.internal", "redis.internal:6379", "[::1]:22", "10.0.0.1"} {
		if err := ValidateSocketTarget(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ":22", "h:abc", "h:0", "h:70000", "h\r\n", "h x", "h\x00", "h:", "a:b:c"} {
		if err := ValidateSocketTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateRabbitMgmtPath(t *testing.T) {
	for _, ok := range []string{"", "/api/overview", "/api/x?y=1"} {
		if err := ValidateRabbitMgmtPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"api", "//evil/x", "/@10.0.0.9:8080/x", "@10.0.0.9:8080/x", "/a\r\n", "/a b", `/a\b`} {
		if err := ValidateRabbitMgmtPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Review of iter-0210: a percent-encoded vhost (`%2F`, `%20`) is a valid management path; an
// invalid percent sequence is refused at the write surfaces, and the error never echoes the input.
func TestValidateRabbitMgmtPathPercentEncoding(t *testing.T) {
	for _, ok := range []string{"/api/aliveness-test/%2F", "/api/queues/my%20vhost/q", "/api/x?columns=a%2Cb"} {
		if err := ValidateRabbitMgmtPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"/api/%zz", "/api/aliveness-test/%2", "/api/x?y=%zz"} {
		err := ValidateRabbitMgmtPath(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "%zz") || strings.Contains(err.Error(), bad) {
			t.Errorf("%q: the error echoes the input: %v", bad, err)
		}
	}
}

func TestMonitorValidateRefusesHostileTargets(t *testing.T) {
	base := func(typ MonitorType, target string, cfg map[string]string) Monitor {
		return Monitor{RecoveryThreshold: 1, ProjectID: "p", Name: "n", Type: typ, Target: target, IntervalSeconds: 60, TimeoutSeconds: 10, Config: cfg}
	}
	cases := []Monitor{
		base(MonitorWebSocket, "ws://h/a\r\nX-Evil: 1", nil),
		base(MonitorWebSocket, "h.internal/ws", nil),
		base(MonitorSSH, "h\r\nx", nil),
		base(MonitorRedis, "h:abc", nil),
		base(MonitorRabbitMQ, "h x", nil),
		base(MonitorRabbitMQ, "rabbit:15672", map[string]string{"mode": "management", "path": "@10.0.0.9:8080/x"}),
		base(MonitorRabbitMQ, "ftp://rabbit", map[string]string{"mode": "management"}),
	}
	for _, m := range cases {
		err := m.Validate()
		if err == nil {
			t.Errorf("%s %q accepted", m.Type, m.Target)
			continue
		}
		if strings.Contains(err.Error(), m.Target) || (m.Config["path"] != "" && strings.Contains(err.Error(), m.Config["path"])) {
			t.Errorf("%s: the error echoes the input: %v", m.Type, err)
		}
	}
	for _, m := range []Monitor{
		base(MonitorWebSocket, "wss://h/ws", nil),
		base(MonitorRedis, "cache.internal", nil),
		base(MonitorRabbitMQ, "https://rabbit:15671", map[string]string{"mode": "management", "path": "/api/overview"}),
		base(MonitorRabbitMQ, "rabbit", map[string]string{"mode": "management"}),
	} {
		if err := m.Validate(); err != nil {
			t.Errorf("%s %q refused: %v", m.Type, m.Target, err)
		}
	}
}

// Second external review of iter-0210 (P2-1): ParseHTTPTarget refuses a port outside 1..65535 and a
// URL without a host; an EMPTY port ("host:") is left to net/http, which normalises it and probes.
// It does not judge userinfo: that is D-0145's write rule in Monitor.Validate, so a legacy row the
// prober re-parses keeps probing. ParsePromQLTarget inherits all of it.
func TestParseHTTPTarget(t *testing.T) {
	for _, ok := range []string{
		"http://h.example", "HTTPS://h.example/x?y=1#z", "http://h.example:/x", "http://h.example:65535",
		"http://[::1]:8080/health", "http://legacy:pw@h.example/health",
	} {
		if _, err := ParseHTTPTarget(ok); err != nil {
			t.Errorf("http %q refused: %v", ok, err)
		}
		if strings.Contains(ok, "#") || strings.Contains(ok, "?") {
			continue
		}
		if _, err := ParsePromQLTarget(ok); err != nil {
			t.Errorf("promql %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://h.example:99999/x", "http://h.example:0", "http://h.example:8o80", "http://:8080/health",
		"http://h.example/#/dashboard\r\n", "http://user:secret@h.example/%zz",
	} {
		for name, parse := range map[string]func(string) (*url.URL, error){"http": ParseHTTPTarget, "promql": ParsePromQLTarget} {
			_, err := parse(bad)
			if err == nil {
				t.Errorf("%s %q accepted", name, bad)
				continue
			}
			if strings.Contains(err.Error(), "h.example") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "://") {
				t.Errorf("%s %q: the error echoes the input: %v", name, bad, err)
			}
		}
	}
}

// External review of iter-0210 (P2-2): a control byte in the raw target is refused for every type
// that probes a target; an http or promql target must parse — a parse failure is a refusal, not a
// skipped userinfo check — with an http(s) scheme and a host; promql's base URL carries no query or
// fragment. No error echoes the input (the secret in particular).
func TestMonitorValidateHTTPAndPromQLTargets(t *testing.T) {
	base := func(typ MonitorType, target string) Monitor {
		cfg := map[string]string(nil)
		if typ == MonitorPromQL {
			cfg = map[string]string{"query": "up"}
		}
		return Monitor{RecoveryThreshold: 1, ProjectID: "p", Name: "n", Type: typ, Target: target, IntervalSeconds: 60, TimeoutSeconds: 10, Config: cfg}
	}
	for _, m := range []Monitor{
		base(MonitorHTTP, "http://user:secret@h.example\x7f/"),
		base(MonitorHTTP, "http://user:secret@h.example/%zz"),
		base(MonitorHTTP, "https://h.example/\r\nX-Evil: 1"),
		base(MonitorHTTP, "h.example/health"),
		base(MonitorHTTP, "ftp://h.example/"),
		base(MonitorHTTP, "http:///health"),
		base(MonitorHTTP, "http:h.example"),
		base(MonitorPromQL, "http://user:secret@prom.example\x7f"),
		base(MonitorPromQL, "http://user:secret@prom.example/%zz"),
		base(MonitorPromQL, "prom.example:9090"),
		base(MonitorPromQL, "http://prom.example:9090?x=1"),
		base(MonitorPromQL, "http://prom.example:9090/#f"),
		base(MonitorPromQL, "http://prom.example:9090/?"),
		base(MonitorTCP, "h.example:80\r\n"),
		base(MonitorDNS, "h.example\n"),
		base(MonitorICMP, "\th.example"),
		base(MonitorTLS, "h.example:443\x00"),
		base(MonitorGRPC, "h.example:50051\r"),
		base(MonitorPostgres, "db.example:5432\n"),
		base(MonitorMySQL, "db.example:3306\x7f"),
	} {
		err := m.Validate()
		if err == nil {
			t.Errorf("%s %q accepted", m.Type, m.Target)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "h.example") ||
			strings.Contains(err.Error(), "X-Evil") || strings.Contains(err.Error(), "%zz") {
			t.Errorf("%s: the error echoes the input: %v", m.Type, err)
		}
	}
	for _, m := range []Monitor{
		base(MonitorHTTP, "https://h.example/health?full=1#top"),
		base(MonitorHTTP, "HTTPS://h.example"),
		base(MonitorHTTP, "http://h.example:/x"),
		base(MonitorPromQL, "http://prom.example:9090"),
		base(MonitorPromQL, "https://gw.example/prometheus/"),
		base(MonitorPromQL, "https://gw.example/pre%2Ffix"),
		base(MonitorTCP, "h.example:80"),
		base(MonitorPostgres, "db.example"),
	} {
		if err := m.Validate(); err != nil {
			t.Errorf("%s %q refused: %v", m.Type, m.Target, err)
		}
	}
}

// External review of iter-0210 (P1-1): one parser for a management-mode target, the same verdict on
// the write surfaces and in the prober. The scheme is case-insensitive; control bytes are judged on
// the RAW value; a base URL carries no userinfo, query or fragment; an empty or out-of-range port is
// refused; tls "true" demands https.
func TestParseRabbitMgmtTarget(t *testing.T) {
	for _, tc := range []struct {
		target string
		tls    bool
		want   string // the base URL the prober builds on
	}{
		{"rabbit", false, "http://rabbit:15672"},
		{"rabbit", true, "https://rabbit:15671"},
		{"rabbit:8080", false, "http://rabbit:8080"},
		{"[::1]:15672", false, "http://[::1]:15672"},
		{"HTTPS://rabbit:15671", true, "https://rabbit:15671"},
		{"Http://rabbit", false, "http://rabbit"},
		{"https://rabbit:15671/base/", true, "https://rabbit:15671/base/"},
		{"http://rabbit:15672/pre%2Ffix", false, "http://rabbit:15672/pre%2Ffix"},
		{"  http://rabbit:15672  ", false, "http://rabbit:15672"},
	} {
		u, err := ParseRabbitMgmtTarget(tc.target, tc.tls)
		if err != nil {
			t.Errorf("%q (tls %v) refused: %v", tc.target, tc.tls, err)
			continue
		}
		if got := u.String(); got != tc.want {
			t.Errorf("%q (tls %v) = %q, want %q", tc.target, tc.tls, got, tc.want)
		}
	}
	for _, tc := range []struct {
		target string
		tls    bool
	}{
		{"", false}, {"   ", false},
		{"http://mq.example:15672", true},
		{"mq.example\r\n", false}, {"http://mq.example:15672\r\n", false}, {"\thttp://mq.example:15672", false},
		{"http://mq.example:15672/my base", false},
		{"http://mq.example:99999", false}, {"mq.example:99999", false}, {"http://mq.example:0", false},
		{"http://mq.example:", false}, {"mq.example:", false},
		{"http://user:secret@mq.example:15672", false},
		{"http://mq.example:15672?x=1", false}, {"http://mq.example:15672/?", false},
		{"http://mq.example:15672/#f", false}, {"http://mq.example:15672#", false},
		{"ftp://mq.example", false}, {"http://", false}, {"http://:15672", false},
		{"http://mq.example:15672/%zz", false},
	} {
		_, err := ParseRabbitMgmtTarget(tc.target, tc.tls)
		if err == nil {
			t.Errorf("%q (tls %v) accepted", tc.target, tc.tls)
			continue
		}
		if strings.Contains(err.Error(), "mq.example") || strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error echoes the input: %v", tc.target, err)
		}
	}
}
