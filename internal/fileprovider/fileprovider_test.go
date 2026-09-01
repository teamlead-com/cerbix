package fileprovider

import (
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
)

func instanceScope() config.ProviderScopeConfig {
	return config.ProviderScopeConfig{Type: config.ProviderScopeInstance}
}

// mustDecode decodes under instance scope and fails the test on error.
func mustDecode(t *testing.T, y string) *DesiredProject {
	t.Helper()
	dp, err := Decode([]byte(y), instanceScope())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return dp
}

// wantReason decodes and asserts a specific BundleError reason.
func wantReason(t *testing.T, y string, scope config.ProviderScopeConfig, r Reason) {
	t.Helper()
	_, err := Decode([]byte(y), scope)
	var be *BundleError
	if !errors.As(err, &be) {
		t.Fatalf("want *BundleError(%s), got %v", r, err)
	}
	if be.Reason != r {
		t.Fatalf("reason = %s, want %s (%s)", be.Reason, r, be.Msg)
	}
}

const validHTTP = `
format: 1
organization: acme
project: payments
monitors:
  api:
    name: Payments API
    type: http
    target: https://payments.internal/health
    interval: 30s
    timeout: 5s
`

func TestDecodeValidAndDefaults(t *testing.T) {
	dp := mustDecode(t, validHTTP)
	if dp.Organization != "acme" || dp.Project != "payments" {
		t.Fatalf("tenant = %s/%s", dp.Organization, dp.Project)
	}
	m := dp.Monitors["api"].Monitor
	if m.Name != "Payments API" || m.Type != "http" || m.IntervalSeconds != 30 || m.TimeoutSeconds != 5 {
		t.Fatalf("monitor = %+v", m)
	}
	// Format-1 contract defaults.
	if m.Method != "GET" || m.Region != "core" || !m.Enabled || !m.AutoIncident || m.FailureThreshold != 1 {
		t.Fatalf("defaults not applied: %+v", m)
	}
	if dp.Monitors["api"].Hash == "" {
		t.Fatal("canonical hash not computed")
	}
}

func TestDecodeStrictRejections(t *testing.T) {
	base := "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://x\n"
	cases := []struct {
		name string
		y    string
		r    Reason
	}{
		{"missing format", "organization: acme\nproject: payments\nmonitors: {}\n", ReasonInvalidFormat},
		{"unsupported format", "format: 3\norganization: acme\nproject: payments\nmonitors: {}\n", ReasonInvalidFormat},
		{"services need format 2", "format: 1\norganization: acme\nproject: payments\nmonitors: {}\nservices: {}\n", ReasonInvalidFormat},
		{"unknown root field", "format: 1\norganization: acme\nproject: payments\nbogus: x\nmonitors: {}\n", ReasonUnknownField},
		{"server-owned field", base + "    id: 123\n", ReasonUnknownField},
		{"duplicate key", "format: 1\norganization: acme\norganization: beta\nproject: p\nmonitors: {}\n", ReasonDuplicateKey},
		{"bad uid", "format: 1\norganization: acme\nproject: p\nmonitors:\n  Bad_UID:\n    name: A\n    type: http\n    target: https://x\n", ReasonInvalidUID},
		{"fractional duration", base + "    interval: 1500ms\n", ReasonInvalidDuration},
		{"unknown type", "format: 1\norganization: acme\nproject: p\nmonitors:\n  x:\n    name: A\n    type: bogus\n    target: t\n", ReasonUnsupportedType},
		{"credentialed type missing settings", "format: 1\norganization: acme\nproject: p\nmonitors:\n  db:\n    name: DB\n    type: postgres\n    target: pg:5432\n", ReasonDomainInvalid},
		{"unsupported settings on http", base + "    settings:\n      foo: bar\n", ReasonUnsupportedField},
		{"inline secret", base + "    settings:\n      password: hunter2\n", ReasonInlineSecret},
		{"target userinfo user:pass", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://u:pw@h/\n", ReasonInlineSecret},
		{"target userinfo user only", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://u@h/\n", ReasonInlineSecret},
		{"target dsn userinfo", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: tcp\n    target: postgres://u:pw@h/db\n", ReasonInlineSecret},
		// P1 bypass: a URL-shaped target that FAILS to parse (invalid %-escape after the
		// userinfo) must still be rejected — url.Parse errors, so the userinfo guard alone
		// would let the raw credential through.
		{"target malformed url with creds", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://u:pw@h/%zz\n", ReasonInlineSecret},
		// password-only userinfo (empty username) still parses and must be rejected.
		{"target userinfo password only", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://:pw@h/\n", ReasonInlineSecret},
		// A known secret-bearing query key in the target is an inline secret too (§2.9/§14).
		{"target query token", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://h/?token=cleartext\n", ReasonInlineSecret},
		{"target query mixed-case api_key", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://h/?API_KEY=x\n", ReasonInlineSecret},
		{"target query percent-encoded key", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://h/?tok%65n=x\n", ReasonInlineSecret},
		{"target query duplicate secret key", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://h/?token=a&token=b\n", ReasonInlineSecret},
		{"target query malformed encoding", "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://h/?token=%zz\n", ReasonInlineSecret},
		{"empty name", "format: 1\norganization: acme\nproject: p\nmonitors:\n  x:\n    type: http\n    target: https://x\n", ReasonDomainInvalid},
		// D-0145 addendum: promql is admitted, and its ONE field is required and closed.
		{"promql without a query", "format: 1\norganization: acme\nproject: p\nmonitors:\n  q:\n    name: Q\n    type: promql\n    target: http://prom:9090\n", ReasonDomainInvalid},
		{"promql with an empty query", "format: 1\norganization: acme\nproject: p\nmonitors:\n  q:\n    name: Q\n    type: promql\n    target: http://prom:9090\n    settings:\n      query: \"   \"\n", ReasonDomainInvalid},
		{"promql with an unknown setting", "format: 1\norganization: acme\nproject: p\nmonitors:\n  q:\n    name: Q\n    type: promql\n    target: http://prom:9090\n    settings:\n      query: up\n      step: 30s\n", ReasonUnsupportedField},
		// The two types that stay out keep their reason: a schema problem, not a missing field.
		{"composite still unsupported", "format: 1\norganization: acme\nproject: p\nmonitors:\n  c:\n    name: C\n    type: composite\n    target: x\n", ReasonUnsupportedType},
		{"synthetic still unsupported", "format: 1\norganization: acme\nproject: p\nmonitors:\n  s:\n    name: S\n    type: synthetic\n    target: x\n", ReasonUnsupportedType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantReason(t, c.y, instanceScope(), c.r) })
	}
}

// TestDecodePromQLBundle is the positive half of the D-0145 addendum: a promql monitor is
// expressible in a bundle, its query survives decoding trimmed, and the canonical hash covers
// it — a settings change must be a semantic change, or a query edit would never be applied.
func TestDecodePromQLBundle(t *testing.T) {
	y := func(query string) []byte {
		return []byte("format: 1\norganization: acme\nproject: payments\nmonitors:\n  budget:\n    name: Error budget\n    type: promql\n    target: http://prom.internal:9090\n    settings:\n      query: " + query + "\n")
	}
	dp, err := Decode(y("'up{job=\"api\"}'"), instanceScope())
	if err != nil {
		t.Fatalf("a promql bundle must decode: %v", err)
	}
	m := dp.Monitors["budget"].Monitor
	if m.Type != "promql" {
		t.Fatalf("type = %q", m.Type)
	}
	if m.Config["query"] != `up{job="api"}` {
		t.Fatalf("query = %q, want the expression", m.Config["query"])
	}
	if dp.Monitors["budget"].Hash == "" {
		t.Fatal("canonical hash not computed for a promql monitor")
	}

	other, err := Decode(y("'up{job=\"web\"}'"), instanceScope())
	if err != nil {
		t.Fatalf("second bundle: %v", err)
	}
	if other.Monitors["budget"].Hash == dp.Monitors["budget"].Hash {
		t.Fatal("two different queries produced one hash: a query edit would never be applied")
	}
}

// TestDecodeTargetNoUserinfoAccepted guards against false positives: the target-userinfo
// secret check must reject only genuine URL credentials, never legitimate non-URL targets
// (bare host:port, hostnames) or clean URLs with query strings.
func TestDecodeTargetNoUserinfoAccepted(t *testing.T) {
	cases := []struct {
		name, typ, target string
	}{
		{"http query no userinfo", "http", "https://h/path?x=1"},
		{"tcp host:port", "tcp", "localhost:5432"},
		{"icmp hostname", "icmp", "db.internal"},
		{"ssh host:port", "ssh", "host:22"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := "format: 1\norganization: acme\nproject: payments\nmonitors:\n  m:\n    name: A\n    type: " + c.typ + "\n    target: " + c.target + "\n"
			if _, err := Decode([]byte(y), instanceScope()); err != nil {
				t.Fatalf("target %q (%s) should be accepted, got %v", c.target, c.typ, err)
			}
		})
	}
}

// TestDecodeTargetRejectionDoesNotLeakSecret asserts the credential in a rejected target
// is never echoed back through the (loggable) error, for both the well-formed userinfo
// path and the malformed-URL path.
func TestDecodeTargetRejectionDoesNotLeakSecret(t *testing.T) {
	const (
		user = "secretuser"
		pass = "secretpass"
	)
	cases := []struct {
		name, target string
	}{
		{"well-formed userinfo", "https://" + user + ":" + pass + "@h/"},
		{"malformed url after userinfo", "https://" + user + ":" + pass + "@h/%zz"},
		{"secret query value", "https://h/?token=" + pass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: " + c.target + "\n"
			var be *BundleError
			_, err := Decode([]byte(y), instanceScope())
			if !errors.As(err, &be) || be.Reason != ReasonInlineSecret {
				t.Fatalf("want *BundleError(%s), got %v", ReasonInlineSecret, err)
			}
			if msg := be.Error(); strings.Contains(msg, user) || strings.Contains(msg, pass) {
				t.Fatalf("rejection error leaks the credential: %q", msg)
			}
		})
	}
}

// TestBuildMonitorControlCharTargetRejected covers the promised control-character case for
// the malformed-URL guard directly (a raw control char in a target cannot survive a YAML
// scalar, so it is exercised at the buildMonitor boundary).
func TestBuildMonitorControlCharTargetRejected(t *testing.T) {
	rm := rawMonitor{Name: "A", Type: "http", Target: "https://h/\x01path"}
	_, err := buildMonitor("api", rm)
	var be *BundleError
	if !errors.As(err, &be) || be.Reason != ReasonInlineSecret {
		t.Fatalf("control-char URL target: want *BundleError(%s), got %v", ReasonInlineSecret, err)
	}
}

func TestScopeMatrix(t *testing.T) {
	proj := config.ProviderScopeConfig{Type: config.ProviderScopeProject, Organization: "acme", Project: "payments"}
	org := config.ProviderScopeConfig{Type: config.ProviderScopeOrganization, Organization: "acme"}
	body := "monitors:\n  api:\n    name: A\n    type: http\n    target: https://x\n"

	// project scope: org/project forbidden in the bundle.
	if _, err := Decode([]byte("format: 1\n"+body), proj); err != nil {
		t.Fatalf("project scope, no tenant fields: %v", err)
	}
	wantReason(t, "format: 1\norganization: acme\n"+body, proj, ReasonScopeMismatch)

	// organization scope: org forbidden, project required.
	if _, err := Decode([]byte("format: 1\nproject: payments\n"+body), org); err != nil {
		t.Fatalf("org scope with project: %v", err)
	}
	wantReason(t, "format: 1\n"+body, org, ReasonScopeMismatch)                                 // missing project
	wantReason(t, "format: 1\norganization: acme\nproject: p\n"+body, org, ReasonScopeMismatch) // org present

	// instance scope: both required.
	wantReason(t, "format: 1\nproject: p\n"+body, instanceScope(), ReasonScopeMismatch)
	wantReason(t, "format: 1\norganization: acme\n"+body, instanceScope(), ReasonScopeMismatch)

	// resolved pair comes from scope for project-scope.
	dp, _ := Decode([]byte("format: 1\n"+body), proj)
	if dp.Organization != "acme" || dp.Project != "payments" {
		t.Fatalf("resolved tenant = %s/%s, want acme/payments", dp.Organization, dp.Project)
	}
}

func TestCanonicalHashSemantics(t *testing.T) {
	a := mustDecode(t, validHTTP).Monitors["api"].Hash

	// Comments, key reorder, and added tags in different order → same semantic hash.
	reordered := `
format: 1
project: payments   # trailing comment
organization: acme
monitors:
  api:
    # a comment
    type: http
    target: https://payments.internal/health
    timeout: 5s
    name: Payments API
    interval: 30s
    tags: [b, a, a]
`
	withTagsAB := mustDecode(t, reordered).Monitors["api"].Hash

	sameTagsBA := `
format: 1
organization: acme
project: payments
monitors:
  api:
    name: Payments API
    type: http
    target: https://payments.internal/health
    interval: 30s
    timeout: 5s
    tags: [a, b]
`
	withTagsBA := mustDecode(t, sameTagsBA).Monitors["api"].Hash
	if withTagsAB != withTagsBA {
		t.Fatal("tag order/dup must not change the canonical hash")
	}
	if a == withTagsAB {
		t.Fatal("adding tags SHOULD change the hash")
	}

	// Name change IS semantic (§6.2).
	renamed := mustDecode(t, `
format: 1
organization: acme
project: payments
monitors:
  api:
    name: Renamed API
    type: http
    target: https://payments.internal/health
    interval: 30s
    timeout: 5s
`).Monitors["api"].Hash
	if renamed == a {
		t.Fatal("name change must change the hash")
	}

	// Condition order IS significant.
	cond := func(order string) string {
		return mustDecode(t, "format: 1\norganization: acme\nproject: p\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://x\n    conditions:\n"+order).Monitors["api"].Hash
	}
	h1 := cond("      - status == 200\n      - latency < 500ms\n")
	h2 := cond("      - latency < 500ms\n      - status == 200\n")
	if h1 == h2 {
		t.Fatal("condition order must be significant in the hash")
	}
}

func TestDependencyDAG(t *testing.T) {
	dag := func(deps string) error {
		_, err := Decode([]byte("format: 1\norganization: acme\nproject: p\nmonitors:\n"+
			"  a:\n    name: A\n    type: http\n    target: https://a\n"+deps+
			"  b:\n    name: B\n    type: http\n    target: https://b\n"), instanceScope())
		return err
	}
	if err := dag("    depends_on: [b]\n"); err != nil {
		t.Fatalf("valid DAG rejected: %v", err)
	}
	wantReason(t, "format: 1\norganization: acme\nproject: p\nmonitors:\n  a:\n    name: A\n    type: http\n    target: https://a\n    depends_on: [a]\n", instanceScope(), ReasonDependencyInvalid)
	wantReason(t, "format: 1\norganization: acme\nproject: p\nmonitors:\n  a:\n    name: A\n    type: http\n    target: https://a\n    depends_on: [ghost]\n", instanceScope(), ReasonDependencyInvalid)
	// Cycle a→b→a.
	wantReason(t, "format: 1\norganization: acme\nproject: p\nmonitors:\n"+
		"  a:\n    name: A\n    type: http\n    target: https://a\n    depends_on: [b]\n"+
		"  b:\n    name: B\n    type: http\n    target: https://b\n    depends_on: [a]\n", instanceScope(), ReasonDependencyCycle)
}

func TestDecodeYAMLPolicy(t *testing.T) {
	// Custom tag rejected.
	wantReason(t, "format: 1\norganization: acme\nproject: p\nmonitors:\n  a:\n    name: !!binary aGk=\n    type: http\n    target: https://x\n", instanceScope(), ReasonInvalidFormat)
	// A custom application tag rejected.
	wantReason(t, "format: 1\norganization: acme\nproject: p\nmonitors:\n  a: !Custom\n    name: A\n    type: http\n    target: https://x\n", instanceScope(), ReasonInvalidFormat)
	// Excessive nesting depth rejected (build a deeply nested seq under a field the decoder
	// would otherwise reject — but the policy walk runs first, so depth wins).
	deep := "format: 1\norganization: acme\nproject: p\nmonitors:\n  a:\n    name: A\n    type: http\n    target: "
	deep += strings.Repeat("[", maxYAMLDepth+3)
	wantReason(t, deep+"\n", instanceScope(), ReasonInvalidFormat)
	// Alias bomb: an anchored 10-item seq aliased ~1500× expands (via the alias-following walk)
	// past maxYAMLNodes and is rejected — before any struct decode.
	bomb := "format: 1\nx: &a [a,a,a,a,a,a,a,a,a,a]\ny: [" + strings.Repeat("*a,", 1500) + "*a]\n"
	wantReason(t, bomb, instanceScope(), ReasonInvalidFormat)
}
