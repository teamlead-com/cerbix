package domain

import (
	"strings"
	"testing"
)

func scenarioOf(t *testing.T, steps string) Scenario {
	t.Helper()
	sc, err := ParseScenario(map[string]string{SyntheticScenarioKey: `{"steps":` + steps + `}`})
	if err != nil {
		t.Fatalf("fixture scenario is invalid: %v", err)
	}
	return sc
}

// FR-028 stage 2. A credential in a scenario is a NAMED BINDING; the enforceable half of D7
// is the key-name rule, and these cases pin it together with the two rules the review
// insisted on: no secret in a URL, and duplicate header keys refused case-insensitively.
func TestScenarioBindingsAcceptsTheDeclaredShape(t *testing.T) {
	sc := scenarioOf(t, `[{"url":"https://api.internal/login","headers":{"authorization":"{{secret:login}}"}},
	                      {"url":"https://api.internal/act","body":"token={{secret:login}}"}]`)
	cfg := map[string]string{ScenarioSecretRefKey("login"): "login-token"}
	uses, err := ScenarioBindings(sc, cfg)
	if err != nil {
		t.Fatalf("a declared binding must be accepted: %v", err)
	}
	if len(uses) != 2 {
		t.Fatalf("uses = %+v, want two (a header and a body)", uses)
	}
	// The LOCATION is part of the identity: it is what the execution digest covers, so a
	// relocated placeholder is a different execution.
	if uses[0].Step != 0 || uses[0].Field != "header:authorization" {
		t.Fatalf("first use = %+v", uses[0])
	}
	if uses[1].Step != 1 || uses[1].Field != "body" {
		t.Fatalf("second use = %+v", uses[1])
	}
}

func TestScenarioBindingsRefusals(t *testing.T) {
	ref := func(names ...string) map[string]string {
		cfg := map[string]string{}
		for _, n := range names {
			cfg[ScenarioSecretRefKey(n)] = n + "-secret"
		}
		return cfg
	}
	cases := map[string]struct {
		steps string
		cfg   map[string]string
		want  string
	}{
		"a literal in a secret-capable header": {
			`[{"url":"https://x","headers":{"authorization":"Bearer literal-token"}}]`, nil,
			"must be exactly {{secret:",
		},
		"a placeholder mixed with text in one": {
			`[{"url":"https://x","headers":{"authorization":"Bearer {{secret:login}}"}}]`, ref("login"),
			"must be exactly {{secret:",
		},
		"a secret in a URL": {
			`[{"url":"https://x/?t={{secret:login}}"}]`, ref("login"),
			"must not reference a secret in its URL",
		},
		"an undeclared binding": {
			`[{"url":"https://x","headers":{"authorization":"{{secret:ghost}}"}}]`, nil,
			"which no scenario_secret_",
		},
		"a declared binding nobody uses": {
			`[{"url":"https://x"}]`, ref("orphan"),
			"declared and never used",
		},
		"an empty secret reference": {
			`[{"url":"https://x","headers":{"authorization":"{{secret:login}}"}}]`,
			map[string]string{ScenarioSecretRefKey("login"): "   "},
			"empty secret reference",
		},
		"duplicate header keys differing in case": {
			`[{"url":"https://x","headers":{"Authorization":"{{secret:login}}","authorization":"{{secret:login}}"}}]`, ref("login"),
			"twice",
		},
		"an invalid binding name": {
			`[{"url":"https://x","body":"{{secret:Login Token}}"}]`, nil,
			"invalid binding",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ScenarioBindings(scenarioOf(t, c.steps), c.cfg)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not name the rule (%q)", err, c.want)
			}
			// A refusal must never echo the credential it refused.
			if strings.Contains(err.Error(), "literal-token") {
				t.Fatalf("the refusal echoed the literal: %q", err)
			}
		})
	}
}

// A scenario with no bindings at all stays legal — the overwhelming majority of monitors —
// and an ordinary header may still carry a literal, because a literal secret is not
// detectable and pretending otherwise would be the guessing this design refuses.
func TestScenarioWithoutBindingsIsUnaffected(t *testing.T) {
	sc := scenarioOf(t, `[{"url":"https://x","headers":{"x-request-id":"abc123","accept":"application/json"}}]`)
	uses, err := ScenarioBindings(sc, nil)
	if err != nil {
		t.Fatalf("a scenario without bindings must be accepted: %v", err)
	}
	if len(uses) != 0 {
		t.Fatalf("uses = %+v, want none", uses)
	}
}

func TestScenarioSecretRefKeyRoundTrip(t *testing.T) {
	key := ScenarioSecretRefKey("checkout")
	if key != "scenario_secret_checkout_ref" {
		t.Fatalf("key = %q", key)
	}
	name, ok := ScenarioBindingFromRefKey(key)
	if !ok || name != "checkout" {
		t.Fatalf("round trip = (%q, %v)", name, ok)
	}
	for _, notOurs := range []string{"password_ref", "scenario", "scenario_secret__ref", "scenario_secret_Checkout_ref"} {
		if _, ok := ScenarioBindingFromRefKey(notOurs); ok {
			t.Fatalf("%q must not parse as a scenario ref key", notOurs)
		}
	}
	cfg := map[string]string{"password_ref": "x", ScenarioSecretRefKey("b"): "y", ScenarioSecretRefKey("a"): "z"}
	keys := ScenarioSecretRefKeys(cfg)
	if len(keys) != 2 || keys[0] != ScenarioSecretRefKey("a") || keys[1] != ScenarioSecretRefKey("b") {
		t.Fatalf("ref keys = %v, want the two scenario ones sorted", keys)
	}
}

// A scenario binding belongs to a SYNTHETIC monitor and to no other type. Found in review
// of the shipped stage 2: every helper acted on the key PREFIX alone, so an HTTP monitor
// could save `scenario_secret_x_ref`, and from there the key changed what the store wrote
// into monitor_secret_refs, what the materializer built, and which carrier the dispatch gate
// demanded — until the executor failed on a scenario that does not exist. Nothing leaked:
// every step fails closed. What broke is the monitor, at dispatch time, for a config the
// write surface had accepted. The rule is now decided once, at the write boundary.
func TestScenarioBindingIsRefusedOnEveryNonSyntheticType(t *testing.T) {
	for _, typ := range []MonitorType{MonitorHTTP, MonitorTCP, MonitorPostgres, MonitorPromQL} {
		m := Monitor{Name: "api", ProjectID: "p", Type: typ, Target: "https://x",
			IntervalSeconds: 60, TimeoutSeconds: 10,
			Config: map[string]string{ScenarioSecretRefKey("login"): "login-token"}}
		err := m.Validate()
		if err == nil {
			t.Fatalf("%s accepted a scenario secret binding", typ)
		}
		if !strings.Contains(err.Error(), "scenario_secret_login_ref") {
			t.Fatalf("%s: refusal must name the key, got %v", typ, err)
		}
		// And the two derived sets must not treat the key as a credential either: a crafted
		// job carrying it must not be able to demand an envelope field or shift the digest.
		cfg := m.Config
		fields, ferr := ExpectedCredentialFields(typ, cfg)
		for _, f := range fields {
			if _, ok := ScenarioBindingFromField(f); ok {
				t.Fatalf("%s: expected fields carry a scenario binding %q (err=%v)", typ, f, ferr)
			}
		}
		keys, kerr := ExecutionBindingKeys(typ, cfg)
		for _, k := range keys {
			if k == SyntheticScenarioKey {
				t.Fatalf("%s: execution keys carry the scenario (err=%v)", typ, kerr)
			}
			if _, ok := ScenarioBindingFromRefKey(k); ok {
				t.Fatalf("%s: execution keys carry a ref key %q (err=%v)", typ, k, kerr)
			}
		}
	}
}

// A key that looks like a binding reference and does not parse used to be ignored in
// silence, which means the operator declares a binding, sees no error, and the credential
// is simply never wired.
func TestMalformedBindingReferenceIsRefusedByName(t *testing.T) {
	sc := scenarioOf(t, `[{"url":"https://api.internal/act","headers":{"x-api-key":"{{secret:login}}"}}]`)
	cfg := map[string]string{
		ScenarioSecretRefKey("login"): "login-token",
		"scenario_secret_Login_ref":   "login-token", // capitalised: not the grammar
	}
	if _, e := ScenarioBindings(sc, cfg); e == nil {
		t.Fatal("a malformed reference key must be refused")
	} else if !strings.Contains(e.Error(), "scenario_secret_Login_ref") {
		t.Fatalf("the refusal must name the key, got %v", e)
	}
}
