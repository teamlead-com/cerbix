package fileprovider

import (
	"errors"
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
		{"wrong format", "format: 2\norganization: acme\nproject: payments\nmonitors: {}\n", ReasonInvalidFormat},
		{"unknown root field", "format: 1\norganization: acme\nproject: payments\nbogus: x\nmonitors: {}\n", ReasonUnknownField},
		{"server-owned field", base + "    id: 123\n", ReasonUnknownField},
		{"duplicate key", "format: 1\norganization: acme\norganization: beta\nproject: p\nmonitors: {}\n", ReasonDuplicateKey},
		{"bad uid", "format: 1\norganization: acme\nproject: p\nmonitors:\n  Bad_UID:\n    name: A\n    type: http\n    target: https://x\n", ReasonInvalidUID},
		{"fractional duration", base + "    interval: 1500ms\n", ReasonInvalidDuration},
		{"unknown type", "format: 1\norganization: acme\nproject: p\nmonitors:\n  x:\n    name: A\n    type: bogus\n    target: t\n", ReasonUnsupportedType},
		{"unsupported type (credentialed)", "format: 1\norganization: acme\nproject: p\nmonitors:\n  db:\n    name: DB\n    type: postgres\n    target: pg:5432\n", ReasonUnsupportedType},
		{"unsupported settings on http", base + "    settings:\n      foo: bar\n", ReasonUnsupportedField},
		{"inline secret", base + "    settings:\n      password: hunter2\n", ReasonInlineSecret},
		{"empty name", "format: 1\norganization: acme\nproject: p\nmonitors:\n  x:\n    type: http\n    target: https://x\n", ReasonDomainInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantReason(t, c.y, instanceScope(), c.r) })
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
