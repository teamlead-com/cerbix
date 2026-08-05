package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SyntheticScenarioKey is the monitor-config key holding the JSON-encoded scenario
// for a synthetic monitor.
const SyntheticScenarioKey = "scenario"

// SyntheticExtract pulls a value out of a step's response into a named variable that
// later steps can interpolate with {{var}}.
//
//	From: "json"   → Path is a dot path (e.g. data.token, items.0.id)
//	From: "header" → Path is the response header name
//	From: "status" → the numeric status code
//	From: "body"   → the whole response body
type SyntheticExtract struct {
	Var  string `json:"var"`
	From string `json:"from"`
	Path string `json:"path,omitempty"`
}

// SyntheticAssert checks one property of a step's response; all asserts must hold for
// the step (and scenario) to pass.
//
//	That: "status"        → Op in {eq,ne,lt,gt}, Value numeric
//	That: "latency_ms"    → Op in {lt,gt}, Value numeric
//	That: "body_contains" → Value substring
//	That: "json"          → Path dot path, Op in {eq,ne,contains}, Value compared as string
type SyntheticAssert struct {
	That  string `json:"that"`
	Op    string `json:"op,omitempty"`
	Path  string `json:"path,omitempty"`
	Value string `json:"value,omitempty"`
}

// SyntheticStep is one HTTP request in a scenario. {{var}} placeholders in URL, header
// values and body are substituted from variables extracted by earlier steps.
type SyntheticStep struct {
	Name    string             `json:"name,omitempty"`
	Method  string             `json:"method,omitempty"`
	URL     string             `json:"url"`
	Headers map[string]string  `json:"headers,omitempty"`
	Body    string             `json:"body,omitempty"`
	Extract []SyntheticExtract `json:"extract,omitempty"`
	Assert  []SyntheticAssert  `json:"assert,omitempty"`
}

// Scenario is a synthetic monitor's ordered multi-step HTTP journey.
type Scenario struct {
	Steps []SyntheticStep `json:"steps"`
}

var (
	syntheticFromKinds   = map[string]bool{"json": true, "header": true, "status": true, "body": true}
	syntheticAssertKinds = map[string]bool{"status": true, "latency_ms": true, "body_contains": true, "json": true}
	syntheticOps         = map[string]bool{"eq": true, "ne": true, "lt": true, "gt": true, "contains": true}
)

// ParseScenario decodes and validates the scenario stored in a monitor's config.
func ParseScenario(config map[string]string) (Scenario, error) {
	raw := config[SyntheticScenarioKey]
	if strings.TrimSpace(raw) == "" {
		return Scenario{}, fmt.Errorf("synthetic: config.%s is required", SyntheticScenarioKey)
	}
	var sc Scenario
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		return Scenario{}, fmt.Errorf("synthetic: invalid scenario json: %w", err)
	}
	if err := sc.Validate(); err != nil {
		return Scenario{}, err
	}
	return sc, nil
}

// Validate enforces scenario invariants (domain-owned).
func (s Scenario) Validate() error {
	if len(s.Steps) == 0 {
		return fmt.Errorf("synthetic: scenario needs at least one step")
	}
	for i, st := range s.Steps {
		if strings.TrimSpace(st.URL) == "" {
			return fmt.Errorf("synthetic: step %d requires a url", i+1)
		}
		for _, e := range st.Extract {
			if strings.TrimSpace(e.Var) == "" {
				return fmt.Errorf("synthetic: step %d extract needs a var name", i+1)
			}
			if !syntheticFromKinds[e.From] {
				return fmt.Errorf("synthetic: step %d extract has unknown from %q", i+1, e.From)
			}
			if (e.From == "json" || e.From == "header") && strings.TrimSpace(e.Path) == "" {
				return fmt.Errorf("synthetic: step %d extract from %q needs a path", i+1, e.From)
			}
		}
		for _, a := range st.Assert {
			if !syntheticAssertKinds[a.That] {
				return fmt.Errorf("synthetic: step %d assert has unknown that %q", i+1, a.That)
			}
			if a.Op != "" && !syntheticOps[a.Op] {
				return fmt.Errorf("synthetic: step %d assert has unknown op %q", i+1, a.Op)
			}
			if a.That == "json" && strings.TrimSpace(a.Path) == "" {
				return fmt.Errorf("synthetic: step %d json assert needs a path", i+1)
			}
		}
	}
	return nil
}
