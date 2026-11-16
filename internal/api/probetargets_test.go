package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-2 H-2 (invariant S2): a hostile target or rabbit path is refused at every API write
// surface — create, update and test-connection — before anything reaches a prober.
func TestMonitorWriteSurfacesRefuseHostileTargets(t *testing.T) {
	fs := seededStore()
	fs.monitors["ws1"] = domain.Monitor{RecoveryThreshold: 1, ID: "ws1", ProjectID: "p1", Name: "ws", Type: domain.MonitorWebSocket,
		Target: "wss://chat.internal/ws", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true}
	// A tester is wired so test-connection reaches its validation; it must never be called.
	tester := &countingProbe{}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTester(tester).Router()

	// Each body is otherwise valid (the rabbit one carries its credential), and each refusal must
	// name the rule: a bare 400 also comes from a body the endpoint cannot decode, which is how the
	// test-connection rows here used to pass (external review of iter-0210, P3-1).
	hostileWS := `{"name":"e2e-ws","type":"websocket","target":"ws://h/a\r\nX-Evil: 1","interval_seconds":60,"timeout_seconds":10}`
	hostileRabbit := `{"name":"e2e-rb","type":"rabbitmq","target":"rabbit:15672","interval_seconds":60,"timeout_seconds":10,"config":{"mode":"management","username":"guest","password":"pw","path":"@10.0.0.9:8080/x"}}`
	for name, req := range map[string]struct{ method, path, body, names string }{
		"create websocket":    {http.MethodPost, "/api/v1/projects/p1/monitors", hostileWS, "target"},
		"test websocket":      {http.MethodPost, "/api/v1/projects/p1/monitors/test", testBody(hostileWS), "target"},
		"create rabbit":       {http.MethodPost, "/api/v1/projects/p1/monitors", hostileRabbit, "path"},
		"test rabbit":         {http.MethodPost, "/api/v1/projects/p1/monitors/test", testBody(hostileRabbit), "path"},
		"update websocket":    {http.MethodPatch, "/api/v1/monitors/ws1", `{"target":"ws://h/a\r\nX-Evil: 1"}`, "target"},
		"websocket no scheme": {http.MethodPost, "/api/v1/projects/p1/monitors", `{"name":"e2e-ws2","type":"websocket","target":"chat.internal/ws","interval_seconds":60,"timeout_seconds":10}`, "target"},
	} {
		rec := do(h, o1Admin, req.method, req.path, req.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), req.names) {
			t.Errorf("%s = %d %s, want 400 naming the %s", name, rec.Code, rec.Body.String(), req.names)
			continue
		}
		if strings.Contains(rec.Body.String(), "X-Evil") || strings.Contains(rec.Body.String(), "10.0.0.9") {
			t.Errorf("%s: the error echoes the hostile input: %s", name, rec.Body.String())
		}
	}
	if got := fs.monitors["ws1"].Target; got != "wss://chat.internal/ws" {
		t.Fatalf("a refused update changed the target to %q", got)
	}
	if tester.calls != 0 {
		t.Fatalf("a hostile target reached the tester %d time(s)", tester.calls)
	}
}

// External review of iter-0210 (P3-1): ssh and redis on the three API surfaces, and PATCH for
// rabbitmq (target and management path). Each fixture is otherwise valid — redis carries its
// credential — so the 400 is the target rule's, which the answer names.
func TestSocketAndRabbitWriteSurfacesRefuseHostileTargets(t *testing.T) {
	fs := seededStore()
	fs.monitors["ssh1"] = domain.Monitor{RecoveryThreshold: 1, ID: "ssh1", ProjectID: "p1", Name: "ssh", Type: domain.MonitorSSH,
		Target: "bastion.internal:22", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true}
	fs.monitors["rd1"] = domain.Monitor{RecoveryThreshold: 1, ID: "rd1", ProjectID: "p1", Name: "rd", Type: domain.MonitorRedis,
		Target: "cache.internal:6379", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
		Config: map[string]string{"password": "pw"}}
	fs.monitors["rb1"] = domain.Monitor{RecoveryThreshold: 1, ID: "rb1", ProjectID: "p1", Name: "rb", Type: domain.MonitorRabbitMQ,
		Target: "https://rabbit.internal:15671", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
		Config: map[string]string{"mode": "management", "username": "guest", "password": "pw"}}
	tester := &countingProbe{}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTester(tester).Router()

	ssh := `{"name":"e2e-ssh","type":"ssh","target":"bastion.internal\r\nX-Evil: 1","interval_seconds":60,"timeout_seconds":10}`
	redis := `{"name":"e2e-rd","type":"redis","target":"cache.internal:6379\r\nX-Evil","interval_seconds":60,"timeout_seconds":10,"config":{"password":"pw"}}`
	for name, req := range map[string]struct{ method, path, body, names string }{
		"create ssh":            {http.MethodPost, "/api/v1/projects/p1/monitors", ssh, "target"},
		"test ssh":              {http.MethodPost, "/api/v1/projects/p1/monitors/test", testBody(ssh), "target"},
		"update ssh":            {http.MethodPatch, "/api/v1/monitors/ssh1", `{"target":"bastion.internal:abc"}`, "target"},
		"create redis":          {http.MethodPost, "/api/v1/projects/p1/monitors", redis, "target"},
		"test redis":            {http.MethodPost, "/api/v1/projects/p1/monitors/test", testBody(redis), "target"},
		"update redis":          {http.MethodPatch, "/api/v1/monitors/rd1", `{"target":"cache.internal:99999"}`, "target"},
		"update rabbit":         {http.MethodPatch, "/api/v1/monitors/rb1", `{"target":"https://10.0.0.9:15671\r\n"}`, "target"},
		"update rabbit http":    {http.MethodPatch, "/api/v1/monitors/rb1", `{"target":"http://rabbit.internal:15672","config":{"mode":"management","username":"guest","tls":"true"}}`, "https"},
		"update rabbit path":    {http.MethodPatch, "/api/v1/monitors/rb1", `{"config":{"mode":"management","username":"guest","path":"@10.0.0.9:8080/x"}}`, "path"},
		"update rabbit relpath": {http.MethodPatch, "/api/v1/monitors/rb1", `{"config":{"mode":"management","username":"guest","path":"api/overview"}}`, "path"},
	} {
		rec := do(h, o1Admin, req.method, req.path, req.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), req.names) {
			t.Errorf("%s = %d %s, want 400 naming the %s", name, rec.Code, rec.Body.String(), req.names)
			continue
		}
		if strings.Contains(rec.Body.String(), "X-Evil") || strings.Contains(rec.Body.String(), "10.0.0.9") {
			t.Errorf("%s: the error echoes the hostile input: %s", name, rec.Body.String())
		}
	}
	if fs.monitors["ssh1"].Target != "bastion.internal:22" || fs.monitors["rd1"].Target != "cache.internal:6379" ||
		fs.monitors["rb1"].Target != "https://rabbit.internal:15671" || fs.monitors["rb1"].Config["path"] != "" {
		t.Fatalf("a refused update changed a monitor: %+v %+v %+v", fs.monitors["ssh1"], fs.monitors["rd1"], fs.monitors["rb1"])
	}
	if tester.calls != 0 {
		t.Fatalf("a hostile target reached the tester %d time(s)", tester.calls)
	}
}

// External review of iter-0210 (P2-2): an http or promql target that does not parse is refused on
// create, update and test-connection — before, a parse failure skipped the userinfo check, so
// `user:secret@` with a DEL byte or an invalid escape was stored, secret and all. The answer never
// echoes the secret.
func TestHTTPAndPromQLUnparsableTargetsRefused(t *testing.T) {
	fs := seededStore()
	fs.monitors["h1"] = domain.Monitor{RecoveryThreshold: 1, ID: "h1", ProjectID: "p1", Name: "h", Type: domain.MonitorHTTP,
		Target: "https://app.internal/health", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true}
	fs.monitors["q1"] = domain.Monitor{RecoveryThreshold: 1, ID: "q1", ProjectID: "p1", Name: "q", Type: domain.MonitorPromQL,
		Target: "http://prom.internal:9090", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
		Config: map[string]string{"query": "up"}}
	tester := &countingProbe{}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTester(tester).Router()

	for _, target := range []string{`http://user:secret@app.internal\u007f/`, `http://user:secret@app.internal/%zz`} {
		httpBody := `{"name":"e2e-h","type":"http","target":"` + target + `","interval_seconds":60,"timeout_seconds":10}`
		promBody := `{"name":"e2e-q","type":"promql","target":"` + target + `","interval_seconds":60,"timeout_seconds":10,"config":{"query":"up"}}`
		for name, req := range map[string]struct{ method, path, body string }{
			"create http":   {http.MethodPost, "/api/v1/projects/p1/monitors", httpBody},
			"test http":     {http.MethodPost, "/api/v1/projects/p1/monitors/test", testBody(httpBody)},
			"update http":   {http.MethodPatch, "/api/v1/monitors/h1", `{"target":"` + target + `"}`},
			"create promql": {http.MethodPost, "/api/v1/projects/p1/monitors", promBody},
			"test promql":   {http.MethodPost, "/api/v1/projects/p1/monitors/test", testBody(promBody)},
			"update promql": {http.MethodPatch, "/api/v1/monitors/q1", `{"target":"` + target + `"}`},
		} {
			rec := do(h, o1Admin, req.method, req.path, req.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "target") {
				t.Errorf("%s %s = %d %s, want 400 naming the target", name, target, rec.Code, rec.Body.String())
				continue
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("%s %s: the error echoes the secret: %s", name, target, rec.Body.String())
			}
		}
	}
	if fs.monitors["h1"].Target != "https://app.internal/health" || fs.monitors["q1"].Target != "http://prom.internal:9090" {
		t.Fatalf("a refused update changed a target: %q, %q", fs.monitors["h1"].Target, fs.monitors["q1"].Target)
	}
	if tester.calls != 0 {
		t.Fatalf("an unparsable target reached the tester %d time(s)", tester.calls)
	}
}

// sec-hardening-2 H-2 fix 3: the API validates a websocket's `tls_skip_verify` as strictly as the
// file provider does — on create, test-connection and update — while types without a typed schema
// (composite) keep their own config path.
func TestWebSocketTLSSkipVerifyOnTheAPI(t *testing.T) {
	fs := seededStore()
	tester := &countingProbe{}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTester(tester).Router()
	ws := func(v string) string {
		return `{"name":"e2e-ws","type":"websocket","target":"wss://chat.internal/ws","interval_seconds":60,"timeout_seconds":10,"config":{"tls_skip_verify":"` + v + `"}}`
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", ws("true")); rec.Code != http.StatusCreated {
		t.Fatalf("create with true = %d %s", rec.Code, rec.Body.String())
	}
	if got := fs.monitors["mon-new"].Config["tls_skip_verify"]; got != "true" {
		t.Fatalf("stored tls_skip_verify = %q, want true", got)
	}
	// The test-connection body has no `name` (the endpoint refuses unknown fields), and every
	// refusal must name the key, so a 400 for an undecodable body cannot pass for this one.
	for path, body := range map[string]string{
		"/api/v1/projects/p1/monitors":      ws("maybe"),
		"/api/v1/projects/p1/monitors/test": testBody(ws("maybe")),
	} {
		if rec := do(h, o1Admin, http.MethodPost, path, body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tls_skip_verify") {
			t.Fatalf("%s with maybe = %d %s, want 400 naming tls_skip_verify", path, rec.Code, rec.Body.String())
		}
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors/test", testBody(ws("true"))); rec.Code != http.StatusOK || tester.calls != 1 {
		t.Fatalf("test-connection with true = %d %s (tester calls %d), want 200 through the tester", rec.Code, rec.Body.String(), tester.calls)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/mon-new", `{"config":{"tls_skip_verify":"sure"}}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tls_skip_verify") {
		t.Fatalf("update with sure = %d %s, want 400 naming tls_skip_verify", rec.Code, rec.Body.String())
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"e2e-ws-x","type":"websocket","target":"wss://chat.internal/ws","interval_seconds":60,"timeout_seconds":10,"config":{"unknown":"x"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown websocket key = %d, want 400", rec.Code)
	}
}

// testBody turns a create body into a test-connection body: that endpoint has no `name` and refuses
// unknown fields, so a create body sent to it is refused as undecodable before any rule runs.
func testBody(createBody string) string {
	return testBodyName.ReplaceAllString(createBody, "")
}

var testBodyName = regexp.MustCompile(`"name":"[^"]*",`)

// countingProbe is a RegionTester that records whether anything reached it.
type countingProbe struct{ calls int }

func (c *countingProbe) RunTest(context.Context, domain.Monitor) (domain.Heartbeat, error) {
	c.calls++
	return domain.Heartbeat{Up: true}, nil
}
