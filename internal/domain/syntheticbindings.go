package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FR-028 stage 2: a credential inside a scenario is a NAMED BINDING, never a literal.
//
// The scenario carries `{{secret:<binding>}}` where a credential belongs; the inventory
// secret's NAME lives in an ordinary flat config key, `scenario_secret_<binding>_ref`. The
// flat key is the whole reason this shape was chosen over putting the ref inside the JSON:
// `repointSecretRefs` looks a ref key up in the flat config, so a rename keeps working with
// no scenario-aware repoint, `monitor_secret_refs` keeps its tenant-safe foreign key with no
// scoped-key convention, and delete and rotation keep their existing paths.
//
// A binding NAME is not a secret and stays visible — that is what keeps a scenario readable
// to the operator who owns it.

const (
	// scenarioSecretPrefix and scenarioSecretSuffix bracket the flat config key that holds
	// the inventory secret NAME for one binding.
	scenarioSecretPrefix = "scenario_secret_"
	scenarioSecretSuffix = "_ref"
	maxScenarioBindings  = 16
)

// bindingNameRe is the grammar: lower-case, digits, dash and underscore, 1..40 characters,
// starting with a letter. Deliberately narrow — the name appears in a config KEY, in a
// template placeholder and in an error message, and a permissive grammar would make those
// three disagree about what one binding is.
var bindingNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

// secretPlaceholderRe matches exactly one placeholder and captures its binding name.
var secretPlaceholderRe = regexp.MustCompile(`\{\{secret:([^}]*)\}\}`)

// SecretCapableHeaderNames are the header names whose VALUE must be a placeholder rather
// than a literal (D7, reading 1). It is the finite set the file provider already refuses as
// credential-bearing, and it is the only rule that can be ENFORCED: a literal secret is not
// detectable by shape, so a credential pasted into a header nobody would call a credential
// header, or into a body, is not caught. The specification says so plainly instead of
// implying a guarantee this set cannot give.
var SecretCapableHeaderNames = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true,
	"x-api-key": true, "api-key": true, "x-auth-token": true, "auth-token": true,
	"x-access-token": true, "access-token": true, "private-token": true,
}

// ScenarioSecretRefKey renders the flat config key that holds one binding's secret name.
func ScenarioSecretRefKey(binding string) string {
	return scenarioSecretPrefix + binding + scenarioSecretSuffix
}

// ScenarioBindingFromRefKey returns the binding name a flat ref key carries, and whether the
// key is one of ours at all.
func ScenarioBindingFromRefKey(key string) (string, bool) {
	if !strings.HasPrefix(key, scenarioSecretPrefix) || !strings.HasSuffix(key, scenarioSecretSuffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, scenarioSecretPrefix), scenarioSecretSuffix)
	if !bindingNameRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// ScenarioSecretRefKeys returns the ref keys a config carries, sorted. The store uses it to
// normalize `monitor_secret_refs` without knowing anything about scenarios.
func ScenarioSecretRefKeys(config map[string]string) []string {
	var out []string
	for k := range config {
		if _, ok := ScenarioBindingFromRefKey(k); ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ScenarioBindingField is the envelope field name that carries one binding's value. It is
// derived from the binding name so the expected set the executor checks and the set the
// materializer builds cannot disagree — they call this function rather than agreeing on a
// convention.
func ScenarioBindingField(binding string) string { return scenarioSecretPrefix + binding }

// ScenarioBindingFromField is the inverse, for the executor's substitution step.
func ScenarioBindingFromField(field string) (string, bool) {
	if !strings.HasPrefix(field, scenarioSecretPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(field, scenarioSecretPrefix)
	if !bindingNameRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// ScenarioBindingUse is one place a scenario references a binding. The LOCATION is part of
// the identity on purpose: an envelope that stays valid while a placeholder moves into a
// request of the attacker's choosing is a worse attack than an altered field set, so the
// location is what the execution digest must cover.
type ScenarioBindingUse struct {
	Binding string
	Step    int    // 0-based index into the scenario's steps
	Field   string // "header:<canonical-name>" or "body"
}

// ScenarioBindings validates a scenario's use of bindings against the config's ref keys and
// returns every use, ordered. It enforces, in this order:
//
//   - a placeholder names a binding that matches the grammar and has a ref key;
//   - a ref key has at least one use — an unused reference is a credential the monitor asks
//     for and never sends, which is a permission nobody needs;
//   - a secret-capable header carries EXACTLY one placeholder and nothing else;
//   - a placeholder never appears in a URL (D7): a URL reaches proxy logs, access logs and
//     error text, and stage 0 exists because that text used to carry it;
//   - duplicate header keys are refused case-insensitively, because two keys differing only
//     in case are one location and the digest could not tell them apart.
func ScenarioBindings(sc Scenario, config map[string]string) ([]ScenarioBindingUse, error) {
	declared := map[string]bool{}
	for _, key := range ScenarioSecretRefKeys(config) {
		name, _ := ScenarioBindingFromRefKey(key)
		if strings.TrimSpace(config[key]) == "" {
			return nil, fmt.Errorf("scenario: binding %q has an empty secret reference", name)
		}
		declared[name] = true
	}
	if len(declared) > maxScenarioBindings {
		return nil, fmt.Errorf("scenario: at most %d secret bindings are allowed", maxScenarioBindings)
	}

	var uses []ScenarioBindingUse
	used := map[string]bool{}
	for i, st := range sc.Steps {
		if names := placeholderNames(st.URL); len(names) > 0 {
			return nil, fmt.Errorf("scenario: step %d must not reference a secret in its URL", i+1)
		}
		seen := map[string]string{}
		for name, value := range st.Headers {
			canonical := strings.ToLower(strings.TrimSpace(name))
			if prev, dup := seen[canonical]; dup {
				return nil, fmt.Errorf("scenario: step %d declares header %q twice (as %q and %q); one header is one location",
					i+1, canonical, prev, name)
			}
			seen[canonical] = name

			names := placeholderNames(value)
			if SecretCapableHeaderNames[canonical] {
				if len(names) != 1 || strings.TrimSpace(value) != "{{secret:"+names[0]+"}}" {
					return nil, fmt.Errorf("scenario: step %d header %q must be exactly {{secret:<binding>}} — a credential is never a literal here",
						i+1, canonical)
				}
			}
			for _, name := range names {
				if !bindingNameRe.MatchString(name) {
					return nil, fmt.Errorf("scenario: step %d header %q references invalid binding %q", i+1, canonical, name)
				}
				if !declared[name] {
					return nil, fmt.Errorf("scenario: step %d header %q references binding %q, which no scenario_secret_<binding>_ref declares", i+1, canonical, name)
				}
				used[name] = true
				uses = append(uses, ScenarioBindingUse{Binding: name, Step: i, Field: "header:" + canonical})
			}
		}
		for _, name := range placeholderNames(st.Body) {
			if !bindingNameRe.MatchString(name) {
				return nil, fmt.Errorf("scenario: step %d body references invalid binding %q", i+1, name)
			}
			if !declared[name] {
				return nil, fmt.Errorf("scenario: step %d body references binding %q, which no scenario_secret_<binding>_ref declares", i+1, name)
			}
			used[name] = true
			uses = append(uses, ScenarioBindingUse{Binding: name, Step: i, Field: "body"})
		}
	}
	for name := range declared {
		if !used[name] {
			return nil, fmt.Errorf("scenario: binding %q is declared and never used", name)
		}
	}
	sort.Slice(uses, func(a, b int) bool {
		if uses[a].Step != uses[b].Step {
			return uses[a].Step < uses[b].Step
		}
		if uses[a].Field != uses[b].Field {
			return uses[a].Field < uses[b].Field
		}
		return uses[a].Binding < uses[b].Binding
	})
	return uses, nil
}

// placeholderNames returns the binding names a string references, in order of appearance.
func placeholderNames(s string) []string {
	if !strings.Contains(s, "{{secret:") {
		return nil
	}
	var out []string
	for _, m := range secretPlaceholderRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}
