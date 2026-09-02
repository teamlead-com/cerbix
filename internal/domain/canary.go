package domain

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FR-029 / NFR-024 — the typed external canary (`async_canary`, workflow kind
// `async_transaction_v1`), specified in docs/specs/func-async-canary.md revision 8 and decided in
// D-0218. This file is the CONTRACT: the types an operator writes, the rules that refuse a document
// that does not mean anything, and nothing else. Canonicalization, the semantic hash and the flat
// config projection live in canarycanonical.go; the executor lives behind the dispatch boundary and
// never re-derives a rule from here.
//
// Why a typed contract at all, in one sentence the project paid for: `synthetic` is an untyped
// document, so every rule about it has to GUESS — FR-028's D7 could not tell a credential from data
// and fell back to a rule about header NAMES. A schema does not guess: a field either takes a
// binding or it does not.

// CanaryWorkflowKind is the only workflow this type carries. It is versioned in its name because a
// second kind is a capability an executor announces, not a flag it interprets.
const CanaryWorkflowKind = "async_transaction_v1"

// The bounds of §4b, as constants rather than as adjectives. A test that says "the edge and one past
// it" needs a number to write, which is what the review round that produced this list asked for.
const (
	CanaryMaxBindings          = 8
	CanaryMaxHeadersPerRequest = 16
	CanaryMaxHeaderNameBytes   = 64
	CanaryMaxHeaderValueBytes  = 1024
	CanaryMaxBodyDepth         = 8
	CanaryMaxBodyKeysPerObject = 64
	CanaryMaxBodyListElements  = 32
	CanaryMaxBodyBytes         = 8 * 1024
	CanaryMaxStringLeafBytes   = 1024
	CanaryMaxMultipartFields   = 16
	CanaryMaxRequiredFields    = 16
	CanaryMaxFailureValues     = 8
	CanaryMinSubmitTimeout     = 1
	CanaryMaxSubmitTimeout     = 60
	CanaryMinPollInterval      = 1
	CanaryMaxPollInterval      = 60
	CanaryMinPollAttempts      = 1
	CanaryMaxPollAttempts      = 600
	CanaryMaxJSONPathDepth     = 8
	CanaryMaxJSONPathBytes     = 200
	CanaryMaxCorrelationBytes  = 256
)

// canaryJSONPath is the restricted grammar of D5: object keys and non-negative array indices, dot
// separated. It addresses ONE value. It cannot filter, iterate or compute — anything beyond that
// re-opens the decision rather than justifying an escape hatch.
var canaryJSONPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.([A-Za-z_][A-Za-z0-9_]*|[0-9]+))*$`)

// canaryHeaderName is an RFC 7230 token. A header name is NOT a JSON path — revision 3 pointed both
// at the same grammar and made `correlate.source: response_header` unusable for `task-id`, its own
// reason to exist (D5).
var canaryHeaderName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$")

// canaryBindingName is the same grammar FR-028 gives a scenario binding: the name appears in a config
// KEY, in a document position and in an error message, and a permissive grammar would make those
// three disagree about what one binding is.
var canaryBindingName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

// canaryBodyKey bounds what an operator may name in a request body.
var canaryBodyKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)

// CanaryCredentialHeaders is FROZEN HERE and not referenced from FR-028 (review round 3): a spec that
// said "the set FR-028 uses" would change meaning silently if that one changed. Ten names, compared
// case-insensitively. A header in this set takes a binding and nothing else, BY SCHEMA — the rule
// D7 reaches for from the other side, without inspecting a single value.
var CanaryCredentialHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true,
	"x-api-key": true, "api-key": true, "x-auth-token": true, "auth-token": true,
	"x-access-token": true, "access-token": true, "private-token": true,
}

// CanaryReservedHeaders are the names the RUNNER owns: it derives the idempotency key from the
// scheduled run (D8) and the multipart boundary from its own encoder, so an author-supplied value
// would make the stable-key contract ambiguous or produce a body that does not parse. A schema that
// silently overrode them would be worse than one that refuses.
var CanaryReservedHeaders = map[string]bool{
	"idempotency-key": true, "host": true, "content-length": true, "transfer-encoding": true,
}

// Closed unions. Every one of these is exhaustive on purpose: a `default:` that accepts is how a
// contract acquires an escape hatch nobody documented.
const (
	CanarySubmitHTTPJSON         = "http_json"
	CanarySubmitMultipartFixture = "multipart_fixture"

	CanaryCorrelateResponseJSON   = "response_json"
	CanaryCorrelateResponseHeader = "response_header"

	CanaryCompletionSSE      = "sse"
	CanaryCompletionPollJSON = "poll_json"

	CanaryCleanupLifecyclePrefix = "lifecycle_prefix"
	CanaryCleanupNone            = "none"
)

// CanaryValueKind tags one node of the request-body algebra (D3a).
type CanaryValueKind string

const (
	CanaryValueString CanaryValueKind = "string"
	CanaryValueNumber CanaryValueKind = "number"
	CanaryValueBool   CanaryValueKind = "bool"
	CanaryValueObject CanaryValueKind = "object"
	CanaryValueList   CanaryValueKind = "list"
	CanaryValueSecret CanaryValueKind = "secret"
)

// CanaryValue is one node of a request body. The algebra is closed: a leaf is a scalar or a
// `{secret_ref: <binding>}` node, and nothing else. What this CANNOT do is detect a credential
// pasted as an ordinary string — the D3a residual, stated in the specification in the same words as
// FR-028's D7 because it is the same undecidable thing.
type CanaryValue struct {
	Kind      CanaryValueKind
	Str       string
	Num       json.Number
	Bool      bool
	Obj       map[string]CanaryValue
	List      []CanaryValue
	SecretRef string // binding name, when Kind == CanaryValueSecret
}

// CanaryHeader is one header entry: a non-secret VALUE or a binding, never both, never neither.
type CanaryHeader struct {
	Name      string
	Value     string
	SecretRef string
}

// CanaryMultipart is the multipart_fixture shape: which field carries the file, and the flat scalar
// fields beside it.
type CanaryMultipart struct {
	FileField string
	Fields    map[string]CanaryValue
}

// CanarySubmit is the first stage: the request that creates the external transaction.
type CanarySubmit struct {
	Kind           string
	Method         string
	URL            string
	SubmitTimeout  int // seconds
	AcceptedStatus []int
	Headers        []CanaryHeader
	FixtureRef     string
	Multipart      *CanaryMultipart
	Body           map[string]CanaryValue
}

// CanaryCorrelate says where the correlation id comes from.
type CanaryCorrelate struct {
	Source     string
	Path       string // response_json
	HeaderName string // response_header
}

// CanarySSE and CanaryPoll are the two completion shapes.
type CanarySSE struct {
	SuccessEvent       string
	FailureEvents      []string
	RequiredJSONFields []string
}

type CanaryPollMatch struct {
	Path   string
	Value  string
	Values []string
}

type CanaryPoll struct {
	Interval    int // seconds
	MaxAttempts int
	Success     CanaryPollMatch
	Failure     CanaryPollMatch
}

// CanaryCompletion awaits the terminal outcome.
type CanaryCompletion struct {
	Kind    string
	URL     string
	Timeout int // seconds
	Headers []CanaryHeader
	SSE     *CanarySSE
	Poll    *CanaryPoll
}

// CanaryResult is what the terminal document must contain, and how fast the journey promised to be.
type CanaryResult struct {
	MaxLatency         int // seconds — the PROMISE; the monitor's timeout is the LIMIT (§5.5)
	RequiredJSONFields []string
	LifecyclePath      string
}

// CanaryCleanup is a VALIDATION and never a deletion (D10): cerbix has no rights on the object store
// and never removes what it did not create.
type CanaryCleanup struct {
	Kind         string
	Prefix       string
	Acknowledged bool
}

// CanaryWorkflow is the whole typed document. `Secrets` is INPUT ONLY (D3f): it is validated here and
// projected into the flat `canary_secret_<binding>_ref` keys, which are the sole persisted identity.
// The persisted document keeps binding MARKERS and no project-secret name, so a rename touches one
// place and cannot leave the stored document disagreeing with it.
type CanaryWorkflow struct {
	Kind       string
	Secrets    map[string]string
	Submit     CanarySubmit
	Correlate  CanaryCorrelate
	Completion CanaryCompletion
	Result     CanaryResult
	Cleanup    CanaryCleanup
}

// CanaryStage names the five stages a failure can be attributed to. The heartbeat carries the stage
// and a bounded class, never a URL, a body, a header, a correlation id or an object path (NFR-024).
type CanaryStage string

const (
	CanaryStageSubmit            CanaryStage = "submit"
	CanaryStageCorrelate         CanaryStage = "correlate"
	CanaryStageAwaitResult       CanaryStage = "await_result"
	CanaryStageAssertResult      CanaryStage = "assert_result"
	CanaryStageCleanupValidation CanaryStage = "cleanup_validation"
)

// ── Validation ─────────────────────────────────────────────────────────────────────────────────

// ValidateCanaryWorkflow enforces the whole contract. `monitorTimeout` is the monitor's own
// `timeout_seconds`: several bounds are expressed against it rather than against a constant, because
// the promise and the stage budgets have to fit inside the probe that carries them.
//
// Every refusal names the position and never echoes a value: a validation message is a place a
// credential leaks exactly as a log line is.
func ValidateCanaryWorkflow(w CanaryWorkflow, monitorTimeout int) error {
	if w.Kind != CanaryWorkflowKind {
		return fmt.Errorf("workflow: unknown kind %q; the only kind is %s", w.Kind, CanaryWorkflowKind)
	}

	declared, err := validateCanarySecrets(w.Secrets)
	if err != nil {
		return err
	}
	used := map[string]bool{}

	if err := validateCanarySubmit(w.Submit, declared, used, monitorTimeout); err != nil {
		return err
	}
	if err := validateCanaryCorrelate(w.Correlate); err != nil {
		return err
	}
	if err := validateCanaryCompletion(w.Completion, w.Submit, declared, used, monitorTimeout); err != nil {
		return err
	}
	if err := validateCanaryResult(w.Result, monitorTimeout); err != nil {
		return err
	}
	if err := validateCanaryCleanup(w.Cleanup); err != nil {
		return err
	}

	// A binding nobody sends is a permission the monitor holds for nothing — and it blocks the
	// deletion of a secret it never uses. The mirror rule (a position naming an undeclared binding)
	// is enforced at each position, so both directions are covered.
	for name := range declared {
		if !used[name] {
			return fmt.Errorf("workflow: binding %q is declared and never used", name)
		}
	}
	return nil
}

func validateCanarySecrets(secrets map[string]string) (map[string]bool, error) {
	declared := map[string]bool{}
	if len(secrets) > CanaryMaxBindings {
		return nil, fmt.Errorf("workflow: at most %d secret bindings are allowed", CanaryMaxBindings)
	}
	for _, name := range sortedKeys(secrets) {
		if !canaryBindingName.MatchString(name) {
			return nil, fmt.Errorf("workflow: %q is not a valid binding name (%s)", name, canaryBindingName.String())
		}
		if strings.TrimSpace(secrets[name]) == "" {
			return nil, fmt.Errorf("workflow: binding %q names no project secret", name)
		}
		declared[name] = true
	}
	return declared, nil
}

func validateCanarySubmit(s CanarySubmit, declared, used map[string]bool, monitorTimeout int) error {
	switch s.Kind {
	case CanarySubmitHTTPJSON, CanarySubmitMultipartFixture:
	default:
		return fmt.Errorf("submit: unknown kind %q; expected %s or %s",
			s.Kind, CanarySubmitHTTPJSON, CanarySubmitMultipartFixture)
	}
	if s.Method != "POST" {
		return fmt.Errorf("submit: method must be POST in v1, got %q", s.Method)
	}
	if err := validateCanaryURL("submit.url", s.URL, false); err != nil {
		return err
	}
	if s.SubmitTimeout < CanaryMinSubmitTimeout || s.SubmitTimeout > CanaryMaxSubmitTimeout {
		return fmt.Errorf("submit: submit_timeout must be between %d and %d seconds",
			CanaryMinSubmitTimeout, CanaryMaxSubmitTimeout)
	}
	if monitorTimeout > 0 && s.SubmitTimeout > monitorTimeout {
		return fmt.Errorf("submit: submit_timeout must not exceed the monitor's timeout")
	}
	if len(s.AcceptedStatus) == 0 {
		return fmt.Errorf("submit: accepted_status must name at least one status")
	}
	seenStatus := map[int]bool{}
	for _, code := range s.AcceptedStatus {
		// 2xx only, on the FINAL response after redirects: a 3xx is not an outcome when redirects
		// are followed, and a 4xx or 5xx an author wants to call "accepted" is a target contract
		// nobody should encode here.
		if code < 200 || code > 299 {
			return fmt.Errorf("submit: accepted_status must be 2xx, got %d", code)
		}
		if seenStatus[code] {
			return fmt.Errorf("submit: accepted_status lists %d twice", code)
		}
		seenStatus[code] = true
	}
	if err := validateCanaryHeaders("submit", s.Headers, declared, used, s.Kind == CanarySubmitMultipartFixture); err != nil {
		return err
	}

	switch s.Kind {
	case CanarySubmitHTTPJSON:
		if s.FixtureRef != "" || s.Multipart != nil {
			return fmt.Errorf("submit: fixture_ref and multipart belong to %s, not %s",
				CanarySubmitMultipartFixture, CanarySubmitHTTPJSON)
		}
		if len(s.Body) == 0 {
			return fmt.Errorf("submit: %s needs a body", CanarySubmitHTTPJSON)
		}
		if err := validateCanaryBody(s.Body, declared, used); err != nil {
			return err
		}
	case CanarySubmitMultipartFixture:
		if len(s.Body) > 0 {
			return fmt.Errorf("submit: body belongs to %s, not %s", CanarySubmitHTTPJSON, CanarySubmitMultipartFixture)
		}
		if !CanaryFixtureExists(s.FixtureRef) {
			return fmt.Errorf("submit: fixture_ref %q is not a registry key", s.FixtureRef)
		}
		if s.Multipart == nil || strings.TrimSpace(s.Multipart.FileField) == "" {
			return fmt.Errorf("submit: multipart.file_field is required for %s", CanarySubmitMultipartFixture)
		}
		if len(s.Multipart.Fields) > CanaryMaxMultipartFields {
			return fmt.Errorf("submit: at most %d multipart fields are allowed", CanaryMaxMultipartFields)
		}
		for _, k := range sortedValueKeys(s.Multipart.Fields) {
			if !canaryBodyKey.MatchString(k) {
				return fmt.Errorf("submit: multipart field %q is not a valid name", k)
			}
			v := s.Multipart.Fields[k]
			if v.Kind == CanaryValueObject || v.Kind == CanaryValueList {
				return fmt.Errorf("submit: multipart field %q must be a scalar or a secret_ref", k)
			}
			if err := validateCanaryValue("submit.multipart."+k, v, declared, used, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCanaryCorrelate(c CanaryCorrelate) error {
	switch c.Source {
	case CanaryCorrelateResponseJSON:
		if c.HeaderName != "" {
			return fmt.Errorf("correlate: header_name belongs to %s", CanaryCorrelateResponseHeader)
		}
		return validateCanaryJSONPath("correlate.path", c.Path)
	case CanaryCorrelateResponseHeader:
		if c.Path != "" {
			return fmt.Errorf("correlate: path belongs to %s", CanaryCorrelateResponseJSON)
		}
		if !canaryHeaderName.MatchString(c.HeaderName) {
			return fmt.Errorf("correlate: header_name %q is not a valid header name", c.HeaderName)
		}
		return nil
	default:
		return fmt.Errorf("correlate: unknown source %q; expected %s or %s",
			c.Source, CanaryCorrelateResponseJSON, CanaryCorrelateResponseHeader)
	}
}

func validateCanaryCompletion(c CanaryCompletion, s CanarySubmit, declared, used map[string]bool, monitorTimeout int) error {
	switch c.Kind {
	case CanaryCompletionSSE, CanaryCompletionPollJSON:
	default:
		return fmt.Errorf("completion: unknown kind %q; expected %s or %s",
			c.Kind, CanaryCompletionSSE, CanaryCompletionPollJSON)
	}
	if err := validateCanaryURL("completion.url", c.URL, true); err != nil {
		return err
	}
	if c.Timeout < 1 {
		return fmt.Errorf("completion: timeout must be positive")
	}
	if monitorTimeout > 0 && c.Timeout > monitorTimeout {
		return fmt.Errorf("completion: timeout must not exceed the monitor's timeout")
	}

	// Completion never inherits submit's headers (D3c): it is a different URL and in general a
	// different host, so silence here is a credential-forwarding bug waiting to be written. And a
	// binding may not cross hosts — refused at the WRITE boundary rather than at run time.
	if err := validateCanaryHeaders("completion", c.Headers, declared, used, false); err != nil {
		return err
	}
	if canaryHeadersCarryBinding(c.Headers) {
		sh, err := canaryHostOf(s.URL)
		if err != nil {
			return err
		}
		ch, err := canaryHostOf(canaryStripPlaceholder(c.URL))
		if err != nil {
			return err
		}
		if sh != ch {
			return fmt.Errorf("completion: a header carrying a binding requires the completion host to equal the submit host")
		}
	}

	switch c.Kind {
	case CanaryCompletionSSE:
		if c.Poll != nil {
			return fmt.Errorf("completion: poll_json fields belong to %s", CanaryCompletionPollJSON)
		}
		if c.SSE == nil {
			return fmt.Errorf("completion: %s needs its own block", CanaryCompletionSSE)
		}
		if strings.TrimSpace(c.SSE.SuccessEvent) == "" {
			return fmt.Errorf("completion: sse.success_event is required")
		}
		if len(c.SSE.FailureEvents) > CanaryMaxFailureValues {
			return fmt.Errorf("completion: at most %d failure_events are allowed", CanaryMaxFailureValues)
		}
		if err := validateCanaryFieldList("completion.sse.required_json_fields", c.SSE.RequiredJSONFields); err != nil {
			return err
		}
	case CanaryCompletionPollJSON:
		if c.SSE != nil {
			return fmt.Errorf("completion: sse fields belong to %s", CanaryCompletionSSE)
		}
		if c.Poll == nil {
			return fmt.Errorf("completion: %s needs its own block", CanaryCompletionPollJSON)
		}
		if c.Poll.Interval < CanaryMinPollInterval || c.Poll.Interval > CanaryMaxPollInterval {
			return fmt.Errorf("completion: poll interval must be between %d and %d seconds",
				CanaryMinPollInterval, CanaryMaxPollInterval)
		}
		if c.Poll.MaxAttempts < CanaryMinPollAttempts || c.Poll.MaxAttempts > CanaryMaxPollAttempts {
			return fmt.Errorf("completion: poll max_attempts must be between %d and %d",
				CanaryMinPollAttempts, CanaryMaxPollAttempts)
		}
		// The polling budget must fit the window it polls in, or the attempt bound is decoration.
		if c.Poll.Interval*c.Poll.MaxAttempts > c.Timeout {
			return fmt.Errorf("completion: poll interval × max_attempts must not exceed the completion timeout")
		}
		if err := validateCanaryJSONPath("completion.poll.success.path", c.Poll.Success.Path); err != nil {
			return err
		}
		if strings.TrimSpace(c.Poll.Success.Value) == "" {
			return fmt.Errorf("completion: poll success.value is required")
		}
		if len(c.Poll.Success.Values) > 0 {
			return fmt.Errorf("completion: poll success takes one value, not a list")
		}
		if c.Poll.Failure.Path != "" || len(c.Poll.Failure.Values) > 0 {
			if err := validateCanaryJSONPath("completion.poll.failure.path", c.Poll.Failure.Path); err != nil {
				return err
			}
			if len(c.Poll.Failure.Values) == 0 {
				return fmt.Errorf("completion: poll failure needs at least one value")
			}
			if len(c.Poll.Failure.Values) > CanaryMaxFailureValues {
				return fmt.Errorf("completion: at most %d poll failure values are allowed", CanaryMaxFailureValues)
			}
			if c.Poll.Failure.Value != "" {
				return fmt.Errorf("completion: poll failure takes values, not a single value")
			}
		}
	}
	return nil
}

func validateCanaryResult(r CanaryResult, monitorTimeout int) error {
	// The PROMISE, distinct from the monitor's timeout, which is the LIMIT (§5.5). Equal is legal
	// and means "the promise is the limit".
	if r.MaxLatency < 1 {
		return fmt.Errorf("result: max_latency must be positive")
	}
	if monitorTimeout > 0 && r.MaxLatency > monitorTimeout {
		return fmt.Errorf("result: max_latency must not exceed the monitor's timeout")
	}
	if len(r.RequiredJSONFields) == 0 {
		return fmt.Errorf("result: required_json_fields must name at least one field")
	}
	if err := validateCanaryFieldList("result.required_json_fields", r.RequiredJSONFields); err != nil {
		return err
	}
	if err := validateCanaryJSONPath("result.lifecycle_path", r.LifecyclePath); err != nil {
		return err
	}
	return nil
}

func validateCanaryCleanup(c CanaryCleanup) error {
	switch c.Kind {
	case CanaryCleanupLifecyclePrefix:
		if strings.TrimSpace(c.Prefix) == "" {
			return fmt.Errorf("cleanup: %s needs a prefix", CanaryCleanupLifecyclePrefix)
		}
		if len(c.Prefix) > CanaryMaxStringLeafBytes {
			return fmt.Errorf("cleanup: prefix is too long")
		}
	case CanaryCleanupNone:
		if c.Prefix != "" {
			return fmt.Errorf("cleanup: prefix belongs to %s", CanaryCleanupLifecyclePrefix)
		}
		// An operator may accept the debt of artifacts nobody sweeps, but not silently: the
		// acknowledgement is visible in the API, the UI and the audit trail (D10).
		if !c.Acknowledged {
			return fmt.Errorf("cleanup: kind %q requires acknowledged: true", CanaryCleanupNone)
		}
	default:
		return fmt.Errorf("cleanup: unknown kind %q; expected %s or %s",
			c.Kind, CanaryCleanupLifecyclePrefix, CanaryCleanupNone)
	}
	return nil
}

// validateCanaryHeaders enforces the header rules for one stage: the count and byte bounds, the
// case-insensitive duplicate rule, the reserved names the runner owns, and D7 — a credential-bearing
// name takes a binding and nothing else, by schema and never by inspecting a value.
func validateCanaryHeaders(stage string, hs []CanaryHeader, declared, used map[string]bool, multipart bool) error {
	if len(hs) > CanaryMaxHeadersPerRequest {
		return fmt.Errorf("%s: at most %d headers are allowed", stage, CanaryMaxHeadersPerRequest)
	}
	seen := map[string]bool{}
	for _, h := range hs {
		name := strings.ToLower(strings.TrimSpace(h.Name))
		if !canaryHeaderName.MatchString(name) {
			return fmt.Errorf("%s: %q is not a valid header name", stage, h.Name)
		}
		if len(name) > CanaryMaxHeaderNameBytes {
			return fmt.Errorf("%s: header name is longer than %d bytes", stage, CanaryMaxHeaderNameBytes)
		}
		// Two names differing only in case are ONE location on the wire, and a digest could not
		// tell them apart — the rule FR-028 learned the hard way, applied here at the schema level.
		if seen[name] {
			return fmt.Errorf("%s: header %q is declared twice; one header is one location", stage, name)
		}
		seen[name] = true
		if CanaryReservedHeaders[name] || (multipart && name == "content-type") {
			return fmt.Errorf("%s: header %q is set by the runner and must not be declared", stage, name)
		}
		if h.SecretRef != "" && h.Value != "" {
			return fmt.Errorf("%s: header %q takes a value or a secret_ref, not both", stage, name)
		}
		if h.SecretRef != "" {
			if !declared[h.SecretRef] {
				return fmt.Errorf("%s: header %q references binding %q, which workflow.secrets does not declare",
					stage, name, h.SecretRef)
			}
			used[h.SecretRef] = true
			continue
		}
		if CanaryCredentialHeaders[name] {
			// Never a heuristic over the value: the SCHEMA of this position has no `value`.
			return fmt.Errorf("%s: header %q is credential-bearing and takes a secret_ref, never a literal", stage, name)
		}
		if h.Value == "" {
			return fmt.Errorf("%s: header %q has no value", stage, name)
		}
		if len(h.Value) > CanaryMaxHeaderValueBytes {
			return fmt.Errorf("%s: header %q value is longer than %d bytes", stage, name, CanaryMaxHeaderValueBytes)
		}
	}
	return nil
}

func canaryHeadersCarryBinding(hs []CanaryHeader) bool {
	for _, h := range hs {
		if h.SecretRef != "" {
			return true
		}
	}
	return false
}

// validateCanaryBody walks the closed algebra of D3a and enforces its bounds. The total size is
// checked against the canonical encoding, so an operator cannot get more through by formatting.
func validateCanaryBody(body map[string]CanaryValue, declared, used map[string]bool) error {
	if len(body) > CanaryMaxBodyKeysPerObject {
		return fmt.Errorf("submit.body: at most %d keys per object", CanaryMaxBodyKeysPerObject)
	}
	for _, k := range sortedValueKeys(body) {
		if !canaryBodyKey.MatchString(k) {
			return fmt.Errorf("submit.body: %q is not a valid key", k)
		}
		if err := validateCanaryValue("submit.body."+k, body[k], declared, used, 1); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(canonicalBodyValue(CanaryValue{Kind: CanaryValueObject, Obj: body}))
	if err != nil {
		return fmt.Errorf("submit.body: cannot be encoded: %w", err)
	}
	if len(encoded) > CanaryMaxBodyBytes {
		return fmt.Errorf("submit.body: canonical encoding is longer than %d bytes", CanaryMaxBodyBytes)
	}
	return nil
}

func validateCanaryValue(pos string, v CanaryValue, declared, used map[string]bool, depth int) error {
	if depth > CanaryMaxBodyDepth {
		return fmt.Errorf("%s: nested deeper than %d", pos, CanaryMaxBodyDepth)
	}
	switch v.Kind {
	case CanaryValueString:
		if len(v.Str) > CanaryMaxStringLeafBytes {
			return fmt.Errorf("%s: string is longer than %d bytes", pos, CanaryMaxStringLeafBytes)
		}
	case CanaryValueNumber:
		if _, err := v.Num.Float64(); err != nil {
			return fmt.Errorf("%s: %q is not a number", pos, v.Num.String())
		}
	case CanaryValueBool:
	case CanaryValueSecret:
		if !declared[v.SecretRef] {
			return fmt.Errorf("%s: references binding %q, which workflow.secrets does not declare", pos, v.SecretRef)
		}
		used[v.SecretRef] = true
	case CanaryValueObject:
		if len(v.Obj) > CanaryMaxBodyKeysPerObject {
			return fmt.Errorf("%s: at most %d keys per object", pos, CanaryMaxBodyKeysPerObject)
		}
		for _, k := range sortedValueKeys(v.Obj) {
			if !canaryBodyKey.MatchString(k) {
				return fmt.Errorf("%s: %q is not a valid key", pos, k)
			}
			if err := validateCanaryValue(pos+"."+k, v.Obj[k], declared, used, depth+1); err != nil {
				return err
			}
		}
	case CanaryValueList:
		if len(v.List) > CanaryMaxBodyListElements {
			return fmt.Errorf("%s: at most %d list elements", pos, CanaryMaxBodyListElements)
		}
		for i, item := range v.List {
			if err := validateCanaryValue(fmt.Sprintf("%s[%d]", pos, i), item, declared, used, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s: unsupported value shape", pos)
	}
	return nil
}

func validateCanaryFieldList(pos string, fields []string) error {
	if len(fields) > CanaryMaxRequiredFields {
		return fmt.Errorf("%s: at most %d paths are allowed", pos, CanaryMaxRequiredFields)
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if err := validateCanaryJSONPath(pos, f); err != nil {
			return err
		}
		if seen[f] {
			return fmt.Errorf("%s: %q is listed twice", pos, f)
		}
		seen[f] = true
	}
	return nil
}

func validateCanaryJSONPath(pos, path string) error {
	if path == "" {
		return fmt.Errorf("%s: a path is required", pos)
	}
	if len(path) > CanaryMaxJSONPathBytes {
		return fmt.Errorf("%s: path is longer than %d bytes", pos, CanaryMaxJSONPathBytes)
	}
	if !canaryJSONPath.MatchString(path) {
		return fmt.Errorf("%s: %q is not a valid path (%s)", pos, path, canaryJSONPath.String())
	}
	if strings.Count(path, ".")+1 > CanaryMaxJSONPathDepth {
		return fmt.Errorf("%s: path is deeper than %d segments", pos, CanaryMaxJSONPathDepth)
	}
	return nil
}

// CanaryCorrelationPlaceholder is the ONE substitution the contract has, legal only in
// `completion.url` and only as a whole path segment (D4).
const CanaryCorrelationPlaceholder = "{{ correlation_id }}"

var canaryPlaceholderAny = regexp.MustCompile(`\{\{[^}]*\}\}`)

// validateCanaryURL enforces the URL rules that can be decided at WRITE time: https only, a parseable
// absolute URL, no userinfo, and — for the completion URL — exactly one `{{ correlation_id }}`
// occupying one whole path segment. The address-level policy (loopback, link-local, private,
// metadata, rebinding) is the executor's and is enforced after resolution, because a name's address
// is not knowable here.
func validateCanaryURL(pos, raw string, allowCorrelation bool) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s: a URL is required", pos)
	}
	placeholders := canaryPlaceholderAny.FindAllString(raw, -1)
	if !allowCorrelation && len(placeholders) > 0 {
		return fmt.Errorf("%s: template substitution is only legal in completion.url", pos)
	}
	if allowCorrelation {
		switch len(placeholders) {
		case 0:
			// A completion URL without the correlation id is legal: not every API addresses the
			// transaction by path.
		case 1:
			if placeholders[0] != CanaryCorrelationPlaceholder {
				return fmt.Errorf("%s: the only substitution is %s", pos, CanaryCorrelationPlaceholder)
			}
			if !canaryPlaceholderIsWholeSegment(raw) {
				return fmt.Errorf("%s: %s must occupy one whole path segment", pos, CanaryCorrelationPlaceholder)
			}
		default:
			return fmt.Errorf("%s: at most one %s is allowed", pos, CanaryCorrelationPlaceholder)
		}
	}

	u, err := url.Parse(canaryStripPlaceholder(raw))
	if err != nil {
		return fmt.Errorf("%s: is not a valid URL", pos)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s: must be https in v1", pos)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: must name a host", pos)
	}
	if u.User != nil {
		return fmt.Errorf("%s: must not carry credentials in its URL userinfo", pos)
	}
	return nil
}

// canaryPlaceholderIsWholeSegment reports that the placeholder stands alone between two slashes —
// never a fragment of a segment, never inside the query, never in the host. The value is
// TARGET-controlled, and a placeholder glued to other text is how `../` in a response rewrites the
// request cerbix is about to make (D4).
func canaryPlaceholderIsWholeSegment(raw string) bool {
	idx := strings.Index(raw, CanaryCorrelationPlaceholder)
	if idx < 0 {
		return false
	}
	if q := strings.IndexAny(raw, "?#"); q >= 0 && idx > q {
		return false
	}
	before := raw[:idx]
	after := raw[idx+len(CanaryCorrelationPlaceholder):]
	if !strings.HasSuffix(before, "/") {
		return false
	}
	// The scheme's "//" must not be what precedes it: that would put the placeholder in the host.
	if strings.HasSuffix(before, "://") {
		return false
	}
	return after == "" || strings.HasPrefix(after, "/") || strings.HasPrefix(after, "?") || strings.HasPrefix(after, "#")
}

func canaryStripPlaceholder(raw string) string {
	return strings.ReplaceAll(raw, CanaryCorrelationPlaceholder, "correlation-id")
}

func canaryHostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url: is not valid")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return host + ":" + port, nil
}

func sortedValueKeys(m map[string]CanaryValue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CanaryDurationSeconds parses the duration spellings the YAML accepts ("30s", "5m", "600") into
// whole seconds. Canonicalization stores seconds, so two documents that differ only in spelling are
// one document (§5.2).
func CanaryDurationSeconds(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("duration: empty")
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "ms"):
		return 0, fmt.Errorf("duration: %q is below the one-second resolution this contract uses", raw)
	case strings.HasSuffix(s, "s"):
		s, mult = strings.TrimSuffix(s, "s"), 1
	case strings.HasSuffix(s, "m"):
		s, mult = strings.TrimSuffix(s, "m"), 60
	case strings.HasSuffix(s, "h"):
		s, mult = strings.TrimSuffix(s, "h"), 3600
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("duration: %q is not a whole number of seconds", raw)
	}
	return n * mult, nil
}
