// Credentialed-monitor settings schemas (spec func-secret-inventory §4.2, FR-020).
// The domain is the SINGLE owner of these per-type rules: the API/UI and the
// Monitoring-as-Code file provider both validate through this file — the file
// provider owns only YAML shape/strictness, never per-type semantics (one-owner
// invariant).
//
// The rules live in a DECLARATIVE registry (`credentialSchemas`) rather than in
// hand-written per-type code, because four separate consumers must agree about the same
// keys and an imperative allowlist drifts from them one commit at a time (D-0160):
//
//  1. validation — which keys exist, which are required, their bounds and enums;
//  2. normalization — the canonical defaults, so implicit and explicit agree;
//  3. the EXPECTED credential field set — the tri-state requirement the executor's
//     structural gate resolves from the effective schema, never from the payload (§4.7);
//  4. the non-secret EXECUTION BINDING keys — the members of the credential-execution DTO
//     covered by `body_digest` (§4.7).
//
// Adding a setting to the registry therefore adds it to all four by construction. Every
// field must declare its binding class: the zero value is `bindingUnclassified` and the
// registry guard test rejects it, so a new key cannot silently land outside the digest.
package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
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

// bindingClass records what a settings key means for the CREDENTIAL EXECUTION binding.
// It exists so the classification is a decision someone made, not a default someone got.
type bindingClass int

const (
	// bindingUnclassified is the zero value and is never valid: a field literal that
	// omits its binding fails the registry guard test rather than defaulting into
	// (or silently out of) the digest.
	bindingUnclassified bindingClass = iota
	// bindingExecution: a non-secret key that decides where the credential goes, over
	// what transport, or how many times it is transmitted. Enters `body_digest`.
	bindingExecution
	// bindingSecretValue: the credential value slot itself. Never in the digest — it
	// travels as envelope ciphertext, which GCM already covers.
	bindingSecretValue
	// bindingSecretRef: the inventory ref NAME. Materialization metadata, deliberately
	// EXCLUDED from the digest: the executor reads only the injected value, so renaming a
	// ref in an already-sealed job selects no different ciphertext and changes no remote
	// behaviour (§4.7).
	bindingSecretRef
)

// CredentialRequirement is the tri-state of §4.7, resolved from the EFFECTIVE schema and
// never from what a payload happens to carry.
type CredentialRequirement int

const (
	// CredentialInvalid: unknown type, or a variant that cannot be resolved.
	CredentialInvalid CredentialRequirement = iota
	// CredentialRequired: the type/mode takes a credential — an envelope must be present
	// and carry exactly the expected non-empty field set.
	CredentialRequired
	// CredentialForbidden: the type/mode takes none — an envelope present at all is a
	// failure, not something to open vacuously.
	CredentialForbidden
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

// credField is one settings key of a per-type schema.
type credField struct {
	key     string
	binding bindingClass // mandatory; see bindingUnclassified

	required   bool
	maxLen     int             // 0 = unbounded
	enum       map[string]bool // nil = free-form
	enumMsg    string          // error text for an enum miss (never echoes the value)
	boolean    bool            // "true"/"false" only
	def        string          // canonical default materialized by normalization
	defIfBlank bool            // a present-but-blank value is also replaced by def
}

// credVariant is one resolved shape of a type's schema (rabbitmq has two; every other
// credentialed type has one).
type credVariant struct {
	fields      []credField
	requirement CredentialRequirement
	// crossChecks are the rules that span more than one field and cannot be expressed
	// per-key. They stay explicit rather than pretending to be declarative.
	crossChecks []func(map[string]string) error
}

// credSchema is a type's schema: either a single variant, or several selected by a
// discriminator key (rabbitmq `mode`).
type credSchema struct {
	discriminator string // "" = single variant, keyed ""
	missingErr    string
	invalidErr    string
	variants      map[string]credVariant
}

// tlsFields are the shared TLS booleans; skip-verify is an explicit, visible opt-in and
// is never silent (§4.8).
func tlsFields() []credField {
	return []credField{
		{key: "tls", binding: bindingExecution, boolean: true, def: "true"},
		{key: "tls_skip_verify", binding: bindingExecution, boolean: true},
	}
}

// tlsPairCheck rejects a skip-verify without TLS rather than silently ignoring it.
func tlsPairCheck(settings map[string]string) error {
	if settings["tls_skip_verify"] == "true" && settings["tls"] == "false" {
		return fmt.Errorf("settings: tls_skip_verify requires tls: true")
	}
	return nil
}

var credentialSchemas = map[MonitorType]credSchema{
	MonitorPostgres: {variants: map[string]credVariant{"": {
		requirement: CredentialRequired,
		fields: []credField{
			{key: "username", binding: bindingExecution, required: true, maxLen: maxCredFieldLen},
			{key: "database", binding: bindingExecution, required: true, maxLen: maxCredFieldLen},
			{key: "sslmode", binding: bindingExecution, enum: sslModes, def: "require",
				enumMsg: "settings: sslmode must be one of disable|require|verify-ca|verify-full"},
			{key: "query", binding: bindingExecution, maxLen: maxQueryLen, def: "SELECT 1", defIfBlank: true},
			{key: "password", binding: bindingSecretValue},
			{key: "password_ref", binding: bindingSecretRef},
		},
	}}},
	MonitorMySQL: {variants: map[string]credVariant{"": {
		requirement: CredentialRequired,
		crossChecks: []func(map[string]string) error{tlsPairCheck},
		fields: append([]credField{
			{key: "username", binding: bindingExecution, required: true, maxLen: maxCredFieldLen},
			{key: "database", binding: bindingExecution, required: true, maxLen: maxCredFieldLen},
		}, append(tlsFields(),
			credField{key: "query", binding: bindingExecution, maxLen: maxQueryLen, def: "SELECT 1", defIfBlank: true},
			credField{key: "password", binding: bindingSecretValue},
			credField{key: "password_ref", binding: bindingSecretRef},
		)...),
	}}},
	MonitorRedis: {variants: map[string]credVariant{"": {
		requirement: CredentialRequired,
		crossChecks: []func(map[string]string) error{tlsPairCheck},
		fields: append([]credField{
			{key: "username", binding: bindingExecution, maxLen: maxCredFieldLen},
		}, append(tlsFields(),
			credField{key: "password", binding: bindingSecretValue},
			credField{key: "password_ref", binding: bindingSecretRef},
		)...),
	}}},
	// Conditional schema (§4.2): mode=amqp is a protocol-header handshake ONLY —
	// credential fields are forbidden, not silently ignored; mode=management is an
	// authenticated HTTP check and requires them. `mode` is execution-binding because it
	// alone decides credentialed HTTP versus unauthenticated AMQP.
	MonitorRabbitMQ: {
		discriminator: "mode",
		missingErr:    "settings: rabbitmq requires `mode` (amqp|management)",
		invalidErr:    "settings: rabbitmq mode must be amqp|management",
		variants: map[string]credVariant{
			"amqp": {
				requirement: CredentialForbidden,
				fields:      []credField{{key: "mode", binding: bindingExecution, required: true}},
			},
			"management": {
				requirement: CredentialRequired,
				crossChecks: []func(map[string]string) error{tlsPairCheck},
				fields: append([]credField{
					{key: "mode", binding: bindingExecution, required: true},
					{key: "username", binding: bindingExecution, required: true, maxLen: maxCredFieldLen},
					{key: "path", binding: bindingExecution, maxLen: maxPathLen, def: "/api/overview", defIfBlank: true},
				}, append(tlsFields(),
					credField{key: "password", binding: bindingSecretValue},
					credField{key: "password_ref", binding: bindingSecretRef},
				)...),
			},
		},
	},
}

// CredentialedType reports whether typ carries credentials governed by §4.2.
func CredentialedType(typ MonitorType) bool {
	_, ok := credentialSchemas[typ]
	return ok
}

// resolveVariant picks the effective variant for a type + settings. It is the single
// place the conditional schema is decided, so validation, normalization, the expected
// field set and the binding keys can never disagree about which shape applies.
func resolveVariant(typ MonitorType, settings map[string]string) (credVariant, error) {
	schema, ok := credentialSchemas[typ]
	if !ok {
		return credVariant{}, fmt.Errorf("settings: monitor type %q has no credential settings schema", typ)
	}
	if schema.discriminator == "" {
		return schema.variants[""], nil
	}
	value, ok := settings[schema.discriminator]
	if !ok {
		return credVariant{}, fmt.Errorf("%s", schema.missingErr)
	}
	variant, ok := schema.variants[value]
	if !ok {
		return credVariant{}, fmt.Errorf("%s", schema.invalidErr)
	}
	return variant, nil
}

// ResolveCredentialRequirement returns the tri-state credential requirement for the
// effective schema (§4.7). An unresolvable type or variant is CredentialInvalid with the
// reason — never a permissive default.
func ResolveCredentialRequirement(typ MonitorType, settings map[string]string) (CredentialRequirement, error) {
	variant, err := resolveVariant(typ, settings)
	if err != nil {
		return CredentialInvalid, err
	}
	return variant.requirement, nil
}

// ExpectedCredentialFields returns the EXACT envelope field set the effective schema
// expects, sorted. It is empty for a CredentialForbidden variant. The executor's
// structural gate compares against this and never against what the payload carries.
func ExpectedCredentialFields(typ MonitorType, settings map[string]string) ([]string, error) {
	variant, err := resolveVariant(typ, settings)
	if err != nil {
		return nil, err
	}
	if variant.requirement != CredentialRequired {
		return nil, nil
	}
	var out []string
	for _, f := range variant.fields {
		if f.binding == bindingSecretValue {
			out = append(out, f.key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ExecutionBindingKeys returns the non-secret settings keys of the effective schema that
// belong in the credential-execution DTO covered by `body_digest`, sorted. Keys are
// returned whether or not the map carries them: the digest is over the effective schema,
// and normalization has already materialized the canonical defaults.
func ExecutionBindingKeys(typ MonitorType, settings map[string]string) ([]string, error) {
	variant, err := resolveVariant(typ, settings)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range variant.fields {
		if f.binding == bindingExecution {
			out = append(out, f.key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PrepareCredentialSettings is the ONLY exported settings entrypoint. It returns
// a NEW, normalized map after validating it for the requested write surface.
// Keeping normalization and validation inseparable makes omission fail-closed:
// no caller can accidentally validate a raw map and let a prober supply an
// insecure historical runtime default.
func PrepareCredentialSettings(typ MonitorType, input map[string]string, surface CredentialSurface) (map[string]string, error) {
	normalized := normalizeCredentialSettings(typ, input)
	if err := validateCredentialSettings(typ, normalized, surface); err != nil {
		return nil, err
	}
	return normalized, nil
}

// normalizeCredentialSettings returns a NEW map with the registry's canonical defaults
// materialized (§4.2/§4.8), so an implicit, empty-runtime-default and explicit default
// produce the SAME effective config — and therefore the same canonical hash and the same
// `body_digest`. It is deliberately unexported; callers use PrepareCredentialSettings.
// A type or variant that cannot be resolved passes through unchanged — validation is what
// rejects it, so normalization never has to guess. The input map is never mutated.
func normalizeCredentialSettings(typ MonitorType, settings map[string]string) map[string]string {
	out := make(map[string]string, len(settings)+3)
	for k, v := range settings {
		out[k] = v
	}
	variant, err := resolveVariant(typ, settings)
	if err != nil {
		return out
	}
	for _, f := range variant.fields {
		if f.def == "" {
			continue
		}
		current, present := out[f.key]
		if !present || (f.defIfBlank && strings.TrimSpace(current) == "") {
			out[f.key] = f.def
		}
	}
	return out
}

// validateCredentialSettings validates an already-normalized settings map against the
// registry. It is deliberately unexported so every external writer must cross the prepare
// gate. Pure and side-effect free; unknown keys reject — there is no generic config escape
// hatch. Errors name keys and rules only, never a submitted value.
func validateCredentialSettings(typ MonitorType, settings map[string]string, surface CredentialSurface) error {
	variant, err := resolveVariant(typ, settings)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(variant.fields))
	for _, f := range variant.fields {
		allowed[f.key] = true
	}
	for k := range settings {
		if !allowed[k] {
			return fmt.Errorf("settings: unknown key %q for this monitor type", k)
		}
	}
	for _, f := range variant.fields {
		if err := validateField(f, settings); err != nil {
			return err
		}
	}
	for _, check := range variant.crossChecks {
		if err := check(settings); err != nil {
			return err
		}
	}
	if variant.requirement != CredentialRequired {
		return nil
	}
	return credentialSlot(settings, surface)
}

// validateField applies one field's declared rules in a fixed order: presence, bounds,
// then shape (enum or boolean).
func validateField(f credField, settings map[string]string) error {
	v, present := settings[f.key]
	if f.required && (!present || v == "") {
		return fmt.Errorf("settings: `%s` is required", f.key)
	}
	if !present {
		return nil
	}
	if f.maxLen > 0 && len(v) > f.maxLen {
		return fmt.Errorf("settings: `%s` exceeds %d bytes", f.key, f.maxLen)
	}
	if f.enum != nil && !f.enum[v] {
		return fmt.Errorf("%s", f.enumMsg)
	}
	if f.boolean && v != "true" && v != "false" {
		return fmt.Errorf("settings: %s must be \"true\" or \"false\"", f.key)
	}
	return nil
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
