package domain

import (
	"fmt"
	"sort"
	"strings"
)

// PrepareTypedSettings normalizes and validates a monitor's typed `settings` object for a
// write surface, and is the ONE entry point a caller needs: it routes a credentialed type to
// the credential registry and a type with a plain non-secret schema to its own rules.
//
// It exists because the file provider used to branch on "a type has settings IF AND ONLY IF
// it is credentialed", which was never what the rule said. `func-monitoring-as-code.md` §3.1
// says a type is available when every type-specific field has a strict non-secret schema —
// credentials are one way to have such a schema, not the definition of having one. `promql`
// is the type that made the difference visible: it carries one field, `query`, and no
// credential at all (D-0145 addendum, 2026-09-01).
//
// A type with no schema at all keeps the old answer: any settings key is an error naming the
// key, because there is no generic config escape hatch.
func PrepareTypedSettings(typ MonitorType, input map[string]string, surface CredentialSurface) (map[string]string, error) {
	if CredentialedType(typ) {
		return PrepareCredentialSettings(typ, input, surface)
	}
	if typ == MonitorPromQL {
		return preparePromQLSettings(input)
	}
	for _, k := range sortedKeys(input) {
		return nil, fmt.Errorf("settings: monitor type %q has no setting %q", typ, k)
	}
	return nil, nil
}

// preparePromQLSettings validates the one field a PromQL monitor carries. The expression is
// NOT a secret and is never redacted: it is the check's definition, and an operator reading
// the monitor must see what is being asked. Length is bounded by the same limit the postgres
// query field uses, so one type cannot store a payload another type would refuse.
func preparePromQLSettings(input map[string]string) (map[string]string, error) {
	out := make(map[string]string, 1)
	for _, k := range sortedKeys(input) {
		if k != "query" {
			return nil, fmt.Errorf("settings: monitor type %q has no setting %q", MonitorPromQL, k)
		}
		out[k] = strings.TrimSpace(input[k])
	}
	query := out["query"]
	if query == "" {
		return nil, fmt.Errorf("settings: %s requires a non-empty `query`", MonitorPromQL)
	}
	if len(query) > maxQueryLen {
		return nil, fmt.Errorf("settings: %s `query` must be at most %d characters", MonitorPromQL, maxQueryLen)
	}
	return out, nil
}

// sortedKeys makes an error message deterministic: with two unknown keys, the same one is
// always named, so a bundle's rejection reason does not change between runs.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
