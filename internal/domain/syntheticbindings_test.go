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
