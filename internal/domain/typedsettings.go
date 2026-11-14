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
	schema := plainSettingsSchemas[typ]
	var out map[string]string
	for _, k := range sortedKeys(input) {
		rule, ok := schema[k]
		if !ok {
			return nil, fmt.Errorf("settings: monitor type %q has no setting %q: %w", typ, k, ErrUnknownSetting)
		}
		v := input[k]
		if rule.boolean && v != "true" && v != "false" {
			return nil, fmt.Errorf("settings: %s must be true or false", k)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out, nil
}

// plainSetting is one key of a plain (non-secret, non-credential) typed schema.
type plainSetting struct {
	boolean bool // "true"/"false" only
}

// plainSettingsSchemas are the typed settings of types that carry no credential. They are kept out
// of the credential registry on purpose: a key there makes RequiresExecutionEnvelope demand a
// credential envelope for every monitor of the type (sec-hardening-2 H-2 fix 3). A key is emitted
// only when given; absent means the default (false).
var plainSettingsSchemas = map[MonitorType]map[string]plainSetting{
	// The WebSocket handshake over wss/https verifies the certificate unless this is "true".
	MonitorWebSocket: {"tls_skip_verify": {boolean: true}},
}

// HasTypedSettings reports whether a type has a typed `settings` schema — a credential schema or a
// plain one — and so must pass PrepareTypedSettings on every write surface. Types without one
// (composite, synthetic, async_canary, …) keep their own config paths.
func HasTypedSettings(typ MonitorType) bool {
	if CredentialedType(typ) {
		return true
	}
	_, ok := plainSettingsSchemas[typ]
	return ok
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
