// Credentialed-monitor settings schemas (spec func-secret-inventory §4.2, FR-020).
// The domain is the SINGLE owner of these per-type rules: the API/UI and the
// Monitoring-as-Code file provider both validate through this file — the file
// provider owns only YAML shape/strictness, never per-type semantics (one-owner
// invariant). NOT yet wired into Monitor.Validate: acceptance of `*_ref` monitors
// switches on together with the dispatch envelope (iteration 2), so there is no
// window where a ref monitor is accepted but cannot dispatch.
package domain

import (
	"fmt"
	"regexp"
)

// CredentialSurface tells the validator which write surface the settings came from —
// the credential-field policy differs (spec §4.2):
//
//   - SurfaceFile (Monitoring-as-Code): a literal credential value is forbidden
//     anywhere (D-0152) and the `_ref` form is REQUIRED;
//   - SurfaceAPI (UI/API monitor writes): exactly-one-of value | ref.
type CredentialSurface int

const (
	SurfaceFile CredentialSurface = iota
	SurfaceAPI
)

// secretRefRe bounds `*_ref` values: they are inventory secret NAMES (slugs).
var secretRefRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidSecretName reports whether s is a valid inventory secret name (spec
// func-secret-inventory §4.1). The domain is the single owner of the slug rule:
// `*_ref` settings values and the store's name validation both resolve to this
// one regexp, so they can never drift apart.
func ValidSecretName(s string) bool { return secretRefRe.MatchString(s) }

// SecretNamePattern exposes the slug rule's source for error messages.
func SecretNamePattern() string { return secretRefRe.String() }

// Field bounds (spec §4.2).
const (
	maxCredFieldLen = 256  // username, database
	maxQueryLen     = 1024 // postgres/mysql query
	maxPathLen      = 512  // rabbitmq management path
)

// sslModes is the postgres allowlist — exactly the modes the prober accepts. `require`
// is encrypted-but-unverified (stated plainly in the spec); `disable` is the explicit
// insecure opt-in (§4.8).
var sslModes = map[string]bool{"disable": true, "require": true, "verify-ca": true, "verify-full": true}

// credentialedTypes marks the monitor types whose settings this file owns.
var credentialedTypes = map[MonitorType]bool{
	MonitorPostgres: true, MonitorMySQL: true, MonitorRedis: true, MonitorRabbitMQ: true,
}

// CredentialedType reports whether typ carries credentials governed by §4.2.
func CredentialedType(typ MonitorType) bool { return credentialedTypes[typ] }

// ValidateCredentialSettings validates the typed settings of a credentialed monitor
// for the given surface. Pure and side-effect free; unknown keys reject — there is no
// generic config escape hatch (§3.1 of func-monitoring-as-code holds). Errors name
// keys and rules only — never a submitted value.
func ValidateCredentialSettings(typ MonitorType, settings map[string]string, surface CredentialSurface) error {
	switch typ {
	case MonitorPostgres:
		if err := allowKeys(settings, "username", "database", "sslmode", "query", "password", "password_ref"); err != nil {
			return err
		}
		if err := requireBounded(settings, "username", maxCredFieldLen); err != nil {
			return err
		}
		if err := requireBounded(settings, "database", maxCredFieldLen); err != nil {
			return err
		}
		if v, ok := settings["sslmode"]; ok && !sslModes[v] {
			return fmt.Errorf("settings: sslmode must be one of disable|require|verify-ca|verify-full")
		}
		if err := boundedOpt(settings, "query", maxQueryLen); err != nil {
			return err
		}
		return credentialSlot(settings, surface)
	case MonitorMySQL:
		if err := allowKeys(settings, "username", "database", "tls", "tls_skip_verify", "query", "password", "password_ref"); err != nil {
			return err
		}
		if err := requireBounded(settings, "username", maxCredFieldLen); err != nil {
			return err
		}
		if err := requireBounded(settings, "database", maxCredFieldLen); err != nil {
			return err
		}
		if err := tlsPair(settings); err != nil {
			return err
		}
		if err := boundedOpt(settings, "query", maxQueryLen); err != nil {
			return err
		}
		return credentialSlot(settings, surface)
	case MonitorRedis:
		if err := allowKeys(settings, "username", "tls", "tls_skip_verify", "password", "password_ref"); err != nil {
			return err
		}
		if err := boundedOpt(settings, "username", maxCredFieldLen); err != nil {
			return err
		}
		if err := tlsPair(settings); err != nil {
			return err
		}
		return credentialSlot(settings, surface)
	case MonitorRabbitMQ:
		// Conditional schema (§4.2): mode=amqp is a protocol-header handshake ONLY —
		// credential fields are forbidden, not silently ignored; mode=management is an
		// authenticated HTTP check and requires them.
		mode, ok := settings["mode"]
		if !ok {
			return fmt.Errorf("settings: rabbitmq requires `mode` (amqp|management)")
		}
		switch mode {
		case "amqp":
			return allowKeys(settings, "mode")
		case "management":
			if err := allowKeys(settings, "mode", "username", "path", "tls", "tls_skip_verify", "password", "password_ref"); err != nil {
				return err
			}
			if err := requireBounded(settings, "username", maxCredFieldLen); err != nil {
				return err
			}
			if err := boundedOpt(settings, "path", maxPathLen); err != nil {
				return err
			}
			if err := tlsPair(settings); err != nil {
				return err
			}
			return credentialSlot(settings, surface)
		default:
			return fmt.Errorf("settings: rabbitmq mode must be amqp|management")
		}
	default:
		return fmt.Errorf("settings: monitor type %q has no credential settings schema", typ)
	}
}

// credentialSlot enforces the per-surface password | password_ref policy and the ref
// slug shape.
func credentialSlot(settings map[string]string, surface CredentialSurface) error {
	_, hasValue := settings["password"]
	ref, hasRef := settings["password_ref"]
	switch surface {
	case SurfaceFile:
		if hasValue {
			return fmt.Errorf("settings: inline `password` is forbidden in bundles; use password_ref")
		}
		if !hasRef {
			return fmt.Errorf("settings: `password_ref` is required for this type in a bundle")
		}
	case SurfaceAPI:
		if hasValue == hasRef { // both or neither
			return fmt.Errorf("settings: exactly one of `password` or `password_ref` is required")
		}
	default:
		return fmt.Errorf("settings: unknown credential surface")
	}
	if hasRef && !secretRefRe.MatchString(ref) {
		return fmt.Errorf("settings: password_ref must be a secret name matching %s", secretRefRe.String())
	}
	return nil
}

// tlsPair validates the tls / tls_skip_verify booleans: string "true"/"false" only, and
// skip-verify is meaningful only with TLS on — a skip-verify without TLS is rejected
// rather than silently ignored (§4.8: no silent skip-verify).
func tlsPair(settings map[string]string) error {
	for _, k := range []string{"tls", "tls_skip_verify"} {
		if v, ok := settings[k]; ok && v != "true" && v != "false" {
			return fmt.Errorf("settings: %s must be \"true\" or \"false\"", k)
		}
	}
	if settings["tls_skip_verify"] == "true" && settings["tls"] == "false" {
		return fmt.Errorf("settings: tls_skip_verify requires tls: true")
	}
	return nil
}

// allowKeys rejects any settings key outside the type's schema.
func allowKeys(settings map[string]string, allowed ...string) error {
	ok := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range settings {
		if !ok[k] {
			return fmt.Errorf("settings: unknown key %q for this monitor type", k)
		}
	}
	return nil
}

// requireBounded requires a non-empty value of bounded byte length.
func requireBounded(settings map[string]string, key string, maxLen int) error {
	v, ok := settings[key]
	if !ok || v == "" {
		return fmt.Errorf("settings: `%s` is required", key)
	}
	if len(v) > maxLen {
		return fmt.Errorf("settings: `%s` exceeds %d bytes", key, maxLen)
	}
	return nil
}

// boundedOpt bounds an optional value's byte length.
func boundedOpt(settings map[string]string, key string, maxLen int) error {
	if v, ok := settings[key]; ok && len(v) > maxLen {
		return fmt.Errorf("settings: `%s` exceeds %d bytes", key, maxLen)
	}
	return nil
}
