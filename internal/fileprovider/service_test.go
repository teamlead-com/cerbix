package fileprovider

import (
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
)

func projectScope() config.ProviderScopeConfig {
	return config.ProviderScopeConfig{Type: "instance"}
}

const format2Bundle = `format: 2
organization: acme
project: payments
monitors:
  checkout-http:
    name: Checkout HTTP
    type: http
    target: https://checkout.example.com/healthz
    interval: 30s
  checkout-db:
    name: Checkout DB
    type: tcp
    target: db.internal:5432
    interval: 60s
services:
  checkout:
    name: Checkout
    owner:
      escalation_policy: payments-oncall
    monitors: [checkout-http, checkout-db]
    sli: [checkout-http]
    aggregation: { mode: quorum, degraded_min: 1, healthy_min: 1 }
    region: { mode: per_region, degraded_min_regions: 1, healthy_min_regions: 1 }
    missing_data: unknown
    maintenance: exclude
    freshness: { active_multiplier: 3, active_floor: 90s }
`

func decodeOK(t *testing.T, y string) *DesiredProject {
	t.Helper()
	dp, err := Decode([]byte(y), projectScope())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return dp
}

func decodeErr(t *testing.T, y string) *BundleError {
	t.Helper()
	_, err := Decode([]byte(y), projectScope())
	if err == nil {
		t.Fatal("Decode succeeded, want a rejection")
	}
	var be *BundleError
	if !errors.As(err, &be) {
		t.Fatalf("error is %T, want *BundleError: %v", err, err)
	}
	return be
}

func TestFormat2DecodesServices(t *testing.T) {
	dp := decodeOK(t, format2Bundle)
	if len(dp.Services) != 1 {
		t.Fatalf("%d services, want 1", len(dp.Services))
	}
	svc := dp.Services["checkout"]
	if svc.Name != "Checkout" || svc.EscalationPolicy != "payments-oncall" {
		t.Errorf("service decoded as %+v", svc)
	}
	if len(svc.Monitors) != 2 || len(svc.SLI) != 1 {
		t.Errorf("monitors=%v sli=%v", svc.Monitors, svc.SLI)
	}
	if svc.Hash == "" {
		t.Error("no canonical hash; the no-op rule has nothing to compare")
	}
}

// A format-1 bundle stays exactly what it was, and a resource map from a later format is
// REFUSED rather than silently ignored — a bundle whose services were quietly dropped would
// look applied and change nothing.
func TestFormat1RejectsServicesAndSlugs(t *testing.T) {
	be := decodeErr(t, "format: 1\norganization: acme\nproject: payments\nmonitors: {}\nservices: {}\n")
	if be.Reason != ReasonInvalidFormat {
		t.Errorf("reason = %s, want invalid_format", be.Reason)
	}
	be = decodeErr(t, "format: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: A\n    type: http\n    target: https://x\n    slug: api\n")
	if be.Reason != ReasonInvalidFormat {
		t.Errorf("reason = %s, want invalid_format for a format-2 monitor field", be.Reason)
	}
}

// A format-1 bundle yields an EMPTY map, never nil, so callers need no version branch.
func TestFormat1YieldsAnEmptyServiceMap(t *testing.T) {
	dp := decodeOK(t, "format: 1\norganization: acme\nproject: payments\nmonitors: {}\n")
	if dp.Services == nil {
		t.Fatal("Services is nil; every caller would need a version branch")
	}
	if len(dp.Services) != 0 {
		t.Errorf("%d services in a format-1 bundle", len(dp.Services))
	}
}

// An omitted monitor slug defaults to the map key, which IS the provider source uid — so the
// same Git-tracked bundle resolves to the same slug on every installation.
func TestMonitorSlugDefaultsToTheSourceUID(t *testing.T) {
	dp := decodeOK(t, format2Bundle)
	if got := dp.Monitors["checkout-http"].Monitor.Slug; got != "checkout-http" {
		t.Errorf("slug = %q, want the map key", got)
	}
}

func TestExplicitMonitorSlugIsUsed(t *testing.T) {
	y := strings.Replace(format2Bundle, "  checkout-http:\n    name: Checkout HTTP",
		"  checkout-http:\n    slug: checkout-api\n    name: Checkout HTTP", 1)
	y = strings.Replace(y, "monitors: [checkout-http, checkout-db]", "monitors: [checkout-api, checkout-db]", 1)
	y = strings.Replace(y, "sli: [checkout-http]", "sli: [checkout-api]", 1)
	dp := decodeOK(t, y)
	if got := dp.Monitors["checkout-http"].Monitor.Slug; got != "checkout-api" {
		t.Errorf("slug = %q, want the declared one", got)
	}
}

func TestDuplicateMonitorSlugIsRejected(t *testing.T) {
	y := strings.Replace(format2Bundle, "  checkout-db:\n    name: Checkout DB",
		"  checkout-db:\n    slug: checkout-http\n    name: Checkout DB", 1)
	be := decodeErr(t, y)
	if be.Reason != ReasonDomainInvalid || !strings.Contains(be.Msg, "duplicate monitor slug") {
		t.Errorf("got %s / %s, want a duplicate-slug rejection", be.Reason, be.Msg)
	}
}

// An SLI member outside `monitors` would be a number with no visible source.
func TestSLIMustBeWithinMonitors(t *testing.T) {
	y := strings.Replace(format2Bundle, "sli: [checkout-http]", "sli: [checkout-http, ghost]", 1)
	be := decodeErr(t, y)
	if !strings.Contains(be.Msg, "not in `monitors`") {
		t.Errorf("got %q, want the sli-subset rejection", be.Msg)
	}
}

// The canonical hash ignores what YAML says about presentation and keeps what the
// declaration means. This is the whole basis of the no-op rule.
func TestCanonicalHashIgnoresFormattingAndOrder(t *testing.T) {
	base := decodeOK(t, format2Bundle).Services["checkout"].Hash

	// Comments, blank lines and indentation style.
	commented := strings.Replace(format2Bundle, "services:", "# a comment nobody should hash\n\nservices:", 1)
	if got := decodeOK(t, commented).Services["checkout"].Hash; got != base {
		t.Error("a comment moved the canonical hash")
	}

	// Member ORDER: both lists are sets, so reordering two lines must not look like a
	// redefinition of what availability means.
	reordered := strings.Replace(format2Bundle,
		"monitors: [checkout-http, checkout-db]", "monitors: [checkout-db, checkout-http]", 1)
	if got := decodeOK(t, reordered).Services["checkout"].Hash; got != base {
		t.Error("reordering `monitors` moved the canonical hash")
	}

	// A repeated member is the same set.
	duped := strings.Replace(format2Bundle,
		"monitors: [checkout-http, checkout-db]", "monitors: [checkout-http, checkout-db, checkout-http]", 1)
	if got := decodeOK(t, duped).Services["checkout"].Hash; got != base {
		t.Error("a duplicated member moved the canonical hash")
	}
}

// ...and it DOES move for everything that changes the meaning of the number.
func TestCanonicalHashMovesOnMeaning(t *testing.T) {
	base := decodeOK(t, format2Bundle).Services["checkout"].Hash

	cases := map[string]string{
		"sli membership": strings.Replace(format2Bundle, "sli: [checkout-http]", "sli: [checkout-http, checkout-db]", 1),
		"context membership": strings.Replace(format2Bundle,
			"monitors: [checkout-http, checkout-db]", "monitors: [checkout-http]", 1),
		"aggregation": strings.Replace(format2Bundle,
			"aggregation: { mode: quorum, degraded_min: 1, healthy_min: 1 }",
			"aggregation: { mode: all }", 1),
		"missing data": strings.Replace(format2Bundle, "missing_data: unknown", "missing_data: bad", 1),
		"owner":        strings.Replace(format2Bundle, "escalation_policy: payments-oncall", "escalation_policy: platform-oncall", 1),
		"name":         strings.Replace(format2Bundle, "name: Checkout\n", "name: Checkout v2\n", 1),
	}
	for what, y := range cases {
		// "context membership" also drops an sli member's context, so fix that variant up.
		if what == "context membership" {
			y = strings.Replace(y, "monitors: [checkout-http]", "monitors: [checkout-http]", 1)
		}
		dp, err := Decode([]byte(y), projectScope())
		if err != nil {
			t.Fatalf("%s: Decode: %v", what, err)
		}
		if got := dp.Services["checkout"].Hash; got == base {
			t.Errorf("changing %s left the canonical hash unchanged; the no-op rule would skip a real redefinition", what)
		}
	}
}

// sec-hardening-2 H-2 (invariant S2): a bundle carrying a hostile probe target is refused at decode
// as domain_invalid — the MaC surface of the three.
func TestBundleRefusesHostileProbeTargets(t *testing.T) {
	// Every fixture is otherwise valid — credentialed types carry their settings — and the message
	// must name the rule, so a refusal for a missing setting cannot pass for this one (external
	// review of iter-0210, P3-1: the rabbit row used to lack its username).
	for name, tc := range map[string]struct{ monitor, names string }{
		"websocket without a scheme": {"    type: websocket\n    target: chat.example.com/ws\n", "target"},
		"websocket with a space":     {"    type: websocket\n    target: \"wss://chat.example.com/a b\"\n", "target"},
		"redis with a bad port":      {"    type: redis\n    target: redis.example.com:abc\n    settings:\n      password_ref: redis\n", "target"},
		"ssh with CRLF":              {"    type: ssh\n    target: \"bastion.example.com\\r\\nX-Evil: 1\"\n", "target"},
		"ssh with a bad port":        {"    type: ssh\n    target: bastion.example.com:70000\n", "target"},
		"rabbit amqp with a space":   {"    type: rabbitmq\n    target: \"rabbit.example.com 5672\"\n    settings:\n      mode: amqp\n", "target"},
		"rabbit amqp with a path":    {"    type: rabbitmq\n    target: rabbit.example.com:5672/vhost\n    settings:\n      mode: amqp\n", "target"},
		"rabbit path authority": {"    type: rabbitmq\n    target: rabbit.example.com:15672\n    settings:\n      mode: management\n" +
			"      username: monitor\n      password_ref: rabbit\n      path: \"@10.0.0.9:8080/x\"\n", "path"},
	} {
		y := "format: 2\norganization: acme\nproject: api\nmonitors:\n  subject:\n    name: Subject\n" + tc.monitor
		be := decodeErr(t, y)
		if be.Reason != ReasonDomainInvalid || !strings.Contains(be.Msg, tc.names) {
			t.Errorf("%s: reason = %s, message %q; want domain_invalid naming the %s", name, be.Reason, be.Msg, tc.names)
		}
		if strings.Contains(be.Msg, "X-Evil") || strings.Contains(be.Msg, "10.0.0.9") {
			t.Errorf("%s: the message echoes the input: %q", name, be.Msg)
		}
	}
}

// External review of iter-0210 (P2-1): a bundle whose RabbitMQ management path is relative is
// refused with a message that names the rule, not applied with a silently added "/". The database
// rows got their "/" from migration 00114; the bundle in Git did not, and must be edited.
func TestBundleRefusesRelativeRabbitMgmtPath(t *testing.T) {
	y := "format: 2\norganization: acme\nproject: api\nmonitors:\n  subject:\n    name: Subject\n" +
		"    type: rabbitmq\n    target: rabbit.example.com\n    settings:\n      mode: management\n      username: monitor\n      password_ref: rabbit\n      path: api/overview\n"
	be := decodeErr(t, y)
	if be.Reason != ReasonDomainInvalid || be.UID != "subject" {
		t.Fatalf("reason = %s, uid = %q; want domain_invalid on subject", be.Reason, be.UID)
	}
	if !strings.Contains(be.Msg, "path must start with a single /") {
		t.Fatalf("message %q does not name the rule", be.Msg)
	}
	ok := strings.Replace(y, "path: api/overview", "path: /api/overview", 1)
	if got := decodeOK(t, ok).Monitors["subject"].Monitor.Config["path"]; got != "/api/overview" {
		t.Fatalf("absolute path decoded as %q", got)
	}
}

// sec-hardening-2 H-2 fix 3: a bundle may declare `tls_skip_verify` for a websocket monitor, it
// reaches the monitor's config, and a non-boolean is refused.
func TestFileProviderWebSocketTLSSkipVerifyRoundTrip(t *testing.T) {
	bundle := func(v string) string {
		return "format: 2\norganization: acme\nproject: api\nmonitors:\n  subject:\n    name: Subject\n" +
			"    type: websocket\n    target: wss://chat.example.com/ws\n    settings:\n      tls_skip_verify: \"" + v + "\"\n"
	}
	dp := decodeOK(t, bundle("true"))
	if got := dp.Monitors["subject"].Monitor.Config["tls_skip_verify"]; got != "true" {
		t.Fatalf("decoded tls_skip_verify = %q, want true", got)
	}
	if be := decodeErr(t, bundle("maybe")); be.Reason != ReasonDomainInvalid {
		t.Fatalf("maybe: reason = %s, want domain_invalid", be.Reason)
	}
}

func TestUnknownPolicyValuesAreRejected(t *testing.T) {
	for _, tc := range []struct{ what, y string }{
		{"missing_data", strings.Replace(format2Bundle, "missing_data: unknown", "missing_data: whatever", 1)},
		{"maintenance", strings.Replace(format2Bundle, "maintenance: exclude", "maintenance: ignore", 1)},
		{"aggregation.mode", strings.Replace(format2Bundle, "mode: quorum", "mode: majority", 1)},
		{"region.mode", strings.Replace(format2Bundle, "mode: per_region", "mode: whatever", 1)},
	} {
		be := decodeErr(t, tc.y)
		if be.Reason != ReasonDomainInvalid {
			t.Errorf("%s: reason = %s, want domain_invalid", tc.what, be.Reason)
		}
	}
}

// Unknown keys are refused by the strict decoder — there is no field for them to land in,
// which is what stops a typo from silently meaning nothing.
func TestUnknownServiceFieldIsRejected(t *testing.T) {
	y := strings.Replace(format2Bundle, "    missing_data: unknown", "    missing_data: unknown\n    bogus_key: 1", 1)
	be := decodeErr(t, y)
	if be.Reason != ReasonUnknownField {
		t.Errorf("reason = %s, want unknown_field", be.Reason)
	}
}

// Policy defaults and validation are the DOMAIN's, shared with the API, so the file provider
// cannot drift into a second answer for the same question.
func TestDecodedPoliciesSatisfyTheSharedValidator(t *testing.T) {
	svc := decodeOK(t, format2Bundle).Services["checkout"]
	declared := map[string]int{"core": 1}
	p := domain.ApplyServicePolicyDefaults(svc.Policies, declared, 1)
	if err := domain.ValidateServicePolicies(p, declared, 1); err != nil {
		t.Fatalf("the decoded policies fail the shared validator: %v", err)
	}
}
