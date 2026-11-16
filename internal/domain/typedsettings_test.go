package domain

import (
	"errors"
	"testing"
)

// sec-hardening-2 H-2 fix 3: `tls_skip_verify` is a PLAIN typed setting of the websocket type — no
// credential schema, so no credential envelope is demanded for every WebSocket monitor.
func TestPrepareTypedSettingsWebSocket(t *testing.T) {
	if !HasTypedSettings(MonitorWebSocket) || CredentialedType(MonitorWebSocket) || RequiresExecutionEnvelope(Monitor{RecoveryThreshold: 1, Type: MonitorWebSocket, Config: map[string]string{"tls_skip_verify": "true"}}) {
		t.Fatal("websocket must have plain typed settings, no credential schema, and no envelope")
	}
	got, err := PrepareTypedSettings(MonitorWebSocket, map[string]string{"tls_skip_verify": "true"}, SurfaceAPI)
	if err != nil || got["tls_skip_verify"] != "true" || len(got) != 1 {
		t.Fatalf("true: %v (%v)", got, err)
	}
	if got, err := PrepareTypedSettings(MonitorWebSocket, map[string]string{"tls_skip_verify": "false"}, SurfaceFile); err != nil || got["tls_skip_verify"] != "false" {
		t.Fatalf("false: %v (%v)", got, err)
	}
	if _, err := PrepareTypedSettings(MonitorWebSocket, map[string]string{"tls_skip_verify": "yes"}, SurfaceAPI); err == nil {
		t.Fatal(`"yes" accepted`)
	}
	if _, err := PrepareTypedSettings(MonitorWebSocket, map[string]string{"other": "x"}, SurfaceAPI); !errors.Is(err, ErrUnknownSetting) {
		t.Fatalf("unknown key: %v, want ErrUnknownSetting", err)
	}
	if got, err := PrepareTypedSettings(MonitorWebSocket, nil, SurfaceAPI); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v (%v)", got, err)
	}
	if HasTypedSettings(MonitorComposite) || HasTypedSettings(MonitorSynthetic) {
		t.Fatal("composite and synthetic keep their own config paths")
	}
}

// D-0215: promql's credential is OPTIONAL, which the tri-state requirement cannot express in
// one variant — so the schema is a discriminator with a DEFAULT, and these cases pin what that
// buys: a monitor written before the schema existed keeps working untouched, and the
// authenticated shape obeys the same credential rules as every other credentialed type.
func TestPromQLCredentialSchema(t *testing.T) {
	// The default variant: a monitor that predates D-0215 carries only `query`, resolves to
	// `none`, and must keep working — at validation AND at the executor's gate.
	got, err := PrepareTypedSettings(MonitorPromQL, map[string]string{"query": `up{job="api"}`}, SurfaceAPI)
	if err != nil {
		t.Fatalf("an unauthenticated promql monitor must stay valid: %v", err)
	}
	if _, ok := got["auth_mode"]; ok {
		t.Fatalf("the discriminator default must NOT be materialized (it would rewrite every canonical hash): %v", got)
	}
	req, err := ResolveCredentialRequirement(MonitorPromQL, got)
	if err != nil || req != CredentialForbidden {
		t.Fatalf("requirement = (%v, %v), want CredentialForbidden with no error", req, err)
	}
	if fields, _ := ExpectedCredentialFields(MonitorPromQL, got); len(fields) != 0 {
		t.Fatalf("the forbidden variant expects no envelope field, got %v", fields)
	}

	// `basic` demands a username and a credential slot; the file surface demands the ref form.
	if _, err := PrepareTypedSettings(MonitorPromQL,
		map[string]string{"auth_mode": "basic", "query": "up"}, SurfaceAPI); err == nil {
		t.Fatal("auth_mode: basic without a username must be refused")
	}
	if _, err := PrepareTypedSettings(MonitorPromQL,
		map[string]string{"auth_mode": "basic", "query": "up", "username": "scanner"}, SurfaceAPI); err == nil {
		t.Fatal("auth_mode: basic without a credential must be refused")
	}
	if _, err := PrepareTypedSettings(MonitorPromQL,
		map[string]string{"auth_mode": "basic", "query": "up", "username": "scanner", "password": "literal"}, SurfaceFile); err == nil {
		t.Fatal("a literal credential must be refused on the file surface")
	}
	basic, err := PrepareTypedSettings(MonitorPromQL,
		map[string]string{"auth_mode": "basic", "query": "up", "username": "scanner", "password_ref": "prom-scanner"}, SurfaceFile)
	if err != nil {
		t.Fatalf("auth_mode: basic with a ref must be accepted on the file surface: %v", err)
	}
	if req, err := ResolveCredentialRequirement(MonitorPromQL, basic); err != nil || req != CredentialRequired {
		t.Fatalf("requirement for auth_mode: basic = (%v, %v), want CredentialRequired", req, err)
	}
	if fields, _ := ExpectedCredentialFields(MonitorPromQL, basic); len(fields) != 1 || fields[0] != "password" {
		t.Fatalf("expected envelope fields = %v, want [password]", fields)
	}

	// A credential on the unauthenticated variant is a contradiction, not a spare key.
	if _, err := PrepareTypedSettings(MonitorPromQL,
		map[string]string{"query": "up", "username": "scanner", "password_ref": "x"}, SurfaceAPI); err == nil {
		t.Fatal("credentials without auth_mode: basic must be refused")
	}
	if _, err := PrepareTypedSettings(MonitorPromQL, map[string]string{"auth_mode": "bearer", "query": "up"}, SurfaceAPI); err == nil {
		t.Fatal("bearer is not a supported scheme and must be refused by name")
	}

	// The query is still the check: missing, blank, over-long and unknown keys all refuse.
	for name, in := range map[string]map[string]string{
		"no query":    {},
		"empty query": {"query": ""},
		"blank query": {"query": "   "},
		"unknown key": {"query": "up", "step": "30s"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareTypedSettings(MonitorPromQL, in, SurfaceAPI); err == nil {
				t.Fatal("expected a rejection")
			}
		})
	}
	long := make([]byte, maxQueryLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := PrepareTypedSettings(MonitorPromQL, map[string]string{"query": string(long)}, SurfaceAPI); err == nil {
		t.Fatal("an over-long query must be refused")
	}
}

// A type with no schema keeps the old answer, and a credentialed one still goes through the
// credential registry — the new entry point routes, it does not loosen.
func TestPrepareTypedSettingsRouting(t *testing.T) {
	if _, err := PrepareTypedSettings(MonitorHTTP, map[string]string{"anything": "x"}, SurfaceFile); err == nil {
		t.Fatal("a type without a schema must refuse every settings key")
	}
	if got, err := PrepareTypedSettings(MonitorHTTP, nil, SurfaceFile); err != nil || got != nil {
		t.Fatalf("no settings for a schema-less type = (nil, nil), got (%v, %v)", got, err)
	}
	// postgres requires username/database and a credential reference at the file surface:
	// the point here is only that the call still reaches that validator.
	if _, err := PrepareTypedSettings(MonitorPostgres, map[string]string{"username": "u"}, SurfaceFile); err == nil {
		t.Fatal("a credentialed type must still be validated by the credential registry")
	}
}

// D-0145 addendum, the second half: a target may not carry userinfo on ANY surface. Go turns
// such a URL into an Authorization header by itself, and the password would then sit in
// plaintext in a column that Redacted() does not touch.
func TestMonitorTargetRejectsURLUserinfo(t *testing.T) {
	base := func(target string) Monitor {
		return Monitor{RecoveryThreshold: 1, ProjectID: "p1", Name: "m", Type: MonitorHTTP, Target: target,
			IntervalSeconds: 60, TimeoutSeconds: 5}
	}
	// The credential in each fixture is a distinctive literal, so the assertion below tests
	// the property that matters — the message never echoes what it just refused — instead of
	// colliding with the generic advice the message gives.
	for name, target := range map[string]string{
		"user and password": "https://scanner:s3cr3t-v4lue@prom.example:9090",
		"password only":     "https://:s3cr3t-v4lue@prom.example:9090",
		"user only":         "https://scanner@prom.example:9090",
	} {
		t.Run(name, func(t *testing.T) {
			err := base(target).Validate()
			if err == nil {
				t.Fatal("a target with userinfo must be refused")
			}
			if got := err.Error(); contains(got, "s3cr3t-v4lue") || contains(got, "prom.example") {
				t.Fatalf("the message echoes the target it refused: %q", got)
			}
		})
	}

	// The non-URL targets are a tcp monitor's: an http target needs an http(s) scheme since the
	// external review of iter-0210 (P2-2) — net/http never probed a scheme-less one — and the point
	// here is only that url.Parse does not read a userinfo into host:port or a bare host.
	for name, m := range map[string]Monitor{
		"plain https":   base("https://prom.example:9090"),
		"host and port": {RecoveryThreshold: 1, ProjectID: "p1", Name: "m", Type: MonitorTCP, Target: "prom.example:9090", IntervalSeconds: 60, TimeoutSeconds: 5},
		"bare host":     {RecoveryThreshold: 1, ProjectID: "p1", Name: "m", Type: MonitorICMP, Target: "prom.example", IntervalSeconds: 60, TimeoutSeconds: 5},
		"path with @":   base("https://prom.example/api/v1/query?query=up@rate"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(); err != nil {
				t.Fatalf("legitimate target rejected: %v", err)
			}
		})
	}
}
