package domain

import (
	"errors"
	"fmt"
	"sort"
)

// ErrUnknownSetting marks "this key does not belong to this type's schema", so a caller can
// map it onto its own vocabulary without matching on message text. The file provider needs
// exactly that: an unknown key is `unsupported_field`, while a key that IS in the schema and
// fails its rule is `domain_invalid` — two different reasons for the operator reading the
// bundle's rejection.
var ErrUnknownSetting = errors.New("settings: unknown key for this monitor type")

// PrepareTypedSettings normalizes and validates a monitor's typed `settings` object for a
// write surface, and is the ONE entry point a caller needs: it routes a credentialed type to
// the credential registry and a type with a plain non-secret schema to its own rules.
//
// It exists because the file provider used to branch on "a type has settings IF AND ONLY IF
// it is credentialed", which was never what the rule said. `func-monitoring-as-code.md` §3.1
// says a type is available when every type-specific field has a strict non-secret schema —
// credentials are one way to have such a schema, not the definition of having one. `promql`
// is the type that made the difference visible: when it was admitted it carried one field,
// `query`, and no credential at all (D-0145 addendum, 2026-09-01). It has since gained
// OPTIONAL basic auth (D-0215) and resolves through the credential registry like every other
// credentialed type — routing here is what let that be a schema change and nothing else.
//
// A type with no schema at all keeps the old answer: any settings key is an error naming the
// key, because there is no generic config escape hatch.
func PrepareTypedSettings(typ MonitorType, input map[string]string, surface CredentialSurface) (map[string]string, error) {
	if CredentialedType(typ) {
		return PrepareCredentialSettings(typ, input, surface)
	}
	for _, k := range sortedKeys(input) {
		return nil, fmt.Errorf("settings: monitor type %q has no setting %q: %w", typ, k, ErrUnknownSetting)
	}
	return nil, nil
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
