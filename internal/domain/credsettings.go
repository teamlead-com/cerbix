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
	// SurfaceAPIUpdate is a PARTIAL update, where omitting the credential slot entirely
	// means "keep what is stored". A write-only value is invisible to the client that
	// reads a monitor back, so demanding exactly-one-of on every PATCH made it impossible
	// to change any other setting without inventing a placeholder — callers were pushed
	// into sending `"password": ""`, which is worse than the rule it works around. A slot
	// that IS present still obeys exactly-one-of.
	SurfaceAPIUpdate
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

	required bool
	maxLen   int             // 0 = unbounded
	enum     map[string]bool // nil = free-form
	enumMsg  string          // error text for an enum miss (never echoes the value)
	boolean  bool            // "true"/"false" only
	// def is the field's CANONICAL value when the key is absent. It is used by two
	// consumers with different needs, which is why writeDefault exists: normalization
	// materializes only the defaults the stored config should actually carry, while the
	// execution binding always resolves an absent key through def — otherwise "absent" and
	// "explicitly set to the default" would produce different digests for the same
	// effective config, which §4.7 forbids.
	def          string
	writeDefault bool // normalization materializes def into the map
	defIfBlank   bool // a present-but-blank value is also replaced by def
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
	// discriminatorDefault is the variant chosen when the discriminator key is ABSENT.
	// Empty means the key is mandatory (rabbitmq: a monitor must say which mode it is).
	// It exists for a type whose credential is OPTIONAL — promql, where the common case is
	// an unauthenticated Prometheus and demanding `auth: none` from every existing monitor
	// would both break them at the executor gate and rewrite their canonical hash. The
	// default is RESOLVED and never materialized, exactly as tls_skip_verify is.
	discriminatorDefault string
	missingErr           string
	invalidErr           string
	variants             map[string]credVariant
}

// tlsFields are the shared TLS booleans; skip-verify is an explicit, visible opt-in and
// is never silent (§4.8).
func tlsFields() []credField {
	return []credField{
		{key: "tls", binding: bindingExecution, boolean: true, def: "true", writeDefault: true},
		// Absent means "do not skip verification", so its canonical value is "false". It is
		// NOT materialized into the stored config: writing it would change the canonical
		// hash of every existing monitor for no behavioural gain. Declaring it here is what
		// makes absent and an explicit "false" digest identically.
		{key: "tls_skip_verify", binding: bindingExecution, boolean: true, def: "false"},
	}
}

// promqlQueryNotBlank closes the gap `required` leaves: presence checks reject "" but accept
// "   ", and a whitespace-only expression makes the prober report "no query configured" on
// every run — a monitor that is configured, scheduled, and permanently meaningless. The
// registry does not trim values (a stored setting is what the operator wrote, and trimming
// would change the canonical hash of existing monitors), so the rule is stated instead.
func promqlQueryNotBlank(settings map[string]string) error {
	if q, ok := settings["query"]; ok && strings.TrimSpace(q) == "" {
		return fmt.Errorf("settings: promql `query` must not be blank")
	}
	return nil
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
			{key: "sslmode", binding: bindingExecution, enum: sslModes, def: "require", writeDefault: true,
				enumMsg: "settings: sslmode must be one of disable|require|verify-ca|verify-full"},
			{key: "query", binding: bindingExecution, maxLen: maxQueryLen, def: "SELECT 1", writeDefault: true, defIfBlank: true},
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
			credField{key: "query", binding: bindingExecution, maxLen: maxQueryLen, def: "SELECT 1", writeDefault: true, defIfBlank: true},
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
	// PromQL: the query is the check, and basic auth is OPTIONAL — an unauthenticated
	// Prometheus on a private network is the common case and must keep working untouched
	// (D-0215, 2026-09-01). The `auth` discriminator defaults to `none`, so a monitor that
	// predates this schema resolves to the forbidden variant and keeps probing.
	//
	// Bearer tokens are deliberately absent: one authentication scheme, chosen because it
	// is what Prometheus itself implements natively. A proxy that wants a bearer token is
	// the unsupported case, and the spec says so rather than half-supporting it.
	//
	// No TLS fields: the target is a full URL, so the scheme decides, and the prober has no
	// skip-verify to offer. Declaring the keys would claim a capability that does not exist.
	//
	// The discriminator is `auth_mode` and not `auth` because the file provider's inline-secret
	// guard treats a settings key literally named `auth` as credential-bearing — `auth: Bearer …`
	// is exactly the shape it exists to catch. Renaming the key was the correct side to give
	// way: a discriminator can be called anything, and weakening that guard for every type to
	// fit one schema's spelling would be a poor trade.
	MonitorPromQL: {
		discriminator:        "auth_mode",
		discriminatorDefault: "none",
		invalidErr:           "settings: promql `auth_mode` must be `none` or `basic`",
		variants: map[string]credVariant{
			"none": {
				requirement: CredentialForbidden,
				crossChecks: []func(map[string]string) error{promqlQueryNotBlank},
				fields: []credField{
					{key: "auth_mode", binding: bindingExecution, def: "none"},
					{key: "query", binding: bindingExecution, required: true, maxLen: maxQueryLen},
				},
			},
			"basic": {
				requirement: CredentialRequired,
				crossChecks: []func(map[string]string) error{promqlQueryNotBlank},
				fields: []credField{
					{key: "auth_mode", binding: bindingExecution, def: "none"},
					{key: "query", binding: bindingExecution, required: true, maxLen: maxQueryLen},
					{key: "username", binding: bindingExecution, required: true, maxLen: maxCredFieldLen},
					{key: "password", binding: bindingSecretValue},
					{key: "password_ref", binding: bindingSecretRef},
				},
			},
		},
	},
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
					{key: "path", binding: bindingExecution, maxLen: maxPathLen, def: "/api/overview", writeDefault: true, defIfBlank: true},
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
		if schema.discriminatorDefault == "" {
			return credVariant{}, fmt.Errorf("%s", schema.missingErr)
		}
		value = schema.discriminatorDefault
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
	// A scenario's bindings are envelope fields too (FR-028 stage 2). They are derived from
	// the config's ref keys rather than from a static schema, because the SET is per monitor:
	// two synthetic monitors legitimately carry different bindings. The executor's structural
	// gate compares against this, so the set the materializer builds and the set the gate
	// expects come from one function.
	scenario := scenarioExpectedFields(typ, settings)
	if !CredentialedType(typ) {
		if len(scenario) > 0 {
			return scenario, nil
		}
		// Unchanged contract: asking a type with no credential schema and no bindings is a
		// caller error, and the message is the schema resolver's own.
		_, err := resolveVariant(typ, settings)
		return nil, err
	}
	typed, err := credentialExpectedFields(typ, settings)
	if err != nil {
		return nil, err
	}
	if len(scenario) == 0 {
		return typed, nil
	}
	out := append(typed, scenario...)
	sort.Strings(out)
	return out, nil
}

// scenarioExpectedFields names one envelope field per declared scenario binding — for a
// SYNTHETIC monitor and no other type. The type gate is the fix for a defect the review
// caught in the shipped stage 2: keyed on the prefix alone, a `scenario_secret_x_ref` key
// on an http monitor made this function demand an envelope field that nothing would ever
// substitute.
func scenarioExpectedFields(typ MonitorType, settings map[string]string) []string {
	if typ != MonitorSynthetic {
		return nil
	}
	var out []string
	for _, key := range ScenarioSecretRefKeys(settings) {
		if strings.TrimSpace(settings[key]) == "" {
			continue
		}
		binding, _ := ScenarioBindingFromRefKey(key)
		out = append(out, ScenarioBindingField(binding))
	}
	sort.Strings(out)
	return out
}

func credentialExpectedFields(typ MonitorType, settings map[string]string) ([]string, error) {
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
	// FR-028 stage 2: for a monitor whose credential is a scenario binding, the SCENARIO is
	// the execution binding — it names the target of every step and the exact place each
	// credential lands. Without it in the digest an attacker with a valid envelope can move
	// a placeholder into a request of their choosing and the AEAD still opens; a test asserts
	// exactly that relocation, and it failed before this line existed. The ref keys join too,
	// so renaming which inventory secret fills a binding is a different execution.
	scenarioKeys := scenarioExecutionKeys(typ, settings)
	if !CredentialedType(typ) {
		if len(scenarioKeys) > 0 {
			return scenarioKeys, nil
		}
		_, err := resolveVariant(typ, settings)
		return nil, err
	}
	variant, err := resolveVariant(typ, settings)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), scenarioKeys...)
	for _, f := range variant.fields {
		if f.binding == bindingExecution {
			out = append(out, f.key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// scenarioExecutionKeys is the scenario plus its ref keys, when the monitor declares any
// binding. A monitor with no binding keeps the digest it had.
func scenarioExecutionKeys(typ MonitorType, settings map[string]string) []string {
	if typ != MonitorSynthetic {
		return nil
	}
	refs := ScenarioSecretRefKeys(settings)
	if len(refs) == 0 {
		return nil
	}
	out := append([]string{SyntheticScenarioKey}, refs...)
	sort.Strings(out)
	return out
}

// CanonicalSettingValue resolves one non-secret settings key to the value the EXECUTION
// sees: what the map carries, or the field's declared canonical value when the key is
// absent. The execution binding uses it so that a config which omits a key and one that
// states its default explicitly produce the same digest — they describe the same probe,
// and a digest that disagreed would reject legitimate jobs at random.
func CanonicalSettingValue(typ MonitorType, settings map[string]string, key string) (string, error) {
	// A scenario key has no schema field and no canonical default: it is what the operator
	// wrote, byte for byte, which is exactly what the digest must bind.
	if key == SyntheticScenarioKey {
		return settings[key], nil
	}
	if _, ok := ScenarioBindingFromRefKey(key); ok {
		return settings[key], nil
	}
	variant, err := resolveVariant(typ, settings)
	if err != nil {
		return "", err
	}
	if v, ok := settings[key]; ok {
		return v, nil
	}
	for _, f := range variant.fields {
		if f.key == key {
			return f.def, nil
		}
	}
	return "", fmt.Errorf("settings: key %q is not part of this schema", key)
}

// CredentialUpdateOmitsSlot reports whether a prepared PARTIAL update left the credential
// slot out, i.e. the stored credential must be preserved rather than replaced. It is the
// one place that question is answered, so the API and the store cannot disagree about what
// an omitted slot means.
func CredentialUpdateOmitsSlot(typ MonitorType, settings map[string]string) bool {
	requirement, err := ResolveCredentialRequirement(typ, settings)
	if err != nil || requirement != CredentialRequired {
		return false
	}
	_, hasValue := settings["password"]
	_, hasRef := settings["password_ref"]
	return !hasValue && !hasRef
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
		if f.def == "" || !f.writeDefault {
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
			return fmt.Errorf("settings: unknown key %q for this monitor type: %w", k, ErrUnknownSetting)
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
	case SurfaceAPIUpdate:
		if hasValue && hasRef {
			return fmt.Errorf("settings: exactly one of `password` or `password_ref` is required")
		}
		if !hasValue && !hasRef {
			return nil // omitted: the stored credential is kept (store preserves the ciphertext)
		}
	default:
		return fmt.Errorf("settings: unknown credential surface")
	}
	if hasRef && !secretRefRe.MatchString(ref) {
		return fmt.Errorf("settings: password_ref must be a secret name matching %s", secretRefRe.String())
	}
	return nil
}
