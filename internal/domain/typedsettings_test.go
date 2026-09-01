package domain

import "testing"

// D-0145 addendum: `promql` is a type with a schema and NO credential. The rule the file
// provider used to apply — "settings iff credentialed" — would have rejected it, so these
// cases pin the one it applies now: a type has settings iff it has a schema.
func TestPrepareTypedSettingsPromQL(t *testing.T) {
	got, err := PrepareTypedSettings(MonitorPromQL, map[string]string{"query": "  up{job=\"api\"}  "}, SurfaceFile)
	if err != nil {
		t.Fatalf("valid promql settings rejected: %v", err)
	}
	if got["query"] != `up{job="api"}` {
		t.Fatalf("query = %q, want it trimmed", got["query"])
	}

	for name, in := range map[string]map[string]string{
		"no settings at all": nil,
		"empty query":        {"query": ""},
		"whitespace query":   {"query": "   "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareTypedSettings(MonitorPromQL, in, SurfaceFile); err == nil {
				t.Fatal("expected a rejection: a promql monitor without a query probes nothing")
			}
		})
	}

	if _, err := PrepareTypedSettings(MonitorPromQL, map[string]string{"query": "up", "step": "30s"}, SurfaceFile); err == nil {
		t.Fatal("an unknown promql setting must be refused by name, not ignored")
	}

	long := make([]byte, maxQueryLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := PrepareTypedSettings(MonitorPromQL, map[string]string{"query": string(long)}, SurfaceFile); err == nil {
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
		return Monitor{ProjectID: "p1", Name: "m", Type: MonitorHTTP, Target: target,
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

	for name, target := range map[string]string{
		"plain https":   "https://prom.example:9090",
		"host and port": "prom.example:9090",
		"bare host":     "prom.example",
		"path with @":   "https://prom.example/api/v1/query?query=up@rate",
	} {
		t.Run(name, func(t *testing.T) {
			if err := base(target).Validate(); err != nil {
				t.Fatalf("legitimate target rejected: %v", err)
			}
		})
	}
}
