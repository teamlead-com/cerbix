package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

const (
	ProtocolV1 = 1
	ProtocolV2 = 2
	// ProtocolV3 is the carrier generation that carries EnvelopeV2. Envelope and carrier
	// are numbered separately on purpose: generation 2 already means envelope v1 to every
	// deployed executor, so binding the execution body had to arrive on a new carrier.
	ProtocolV3 = 3
	EnvelopeV1 = 1
	// EnvelopeV2 binds the execution body and the field set in addition to identity (r7).
	// It is a NEW generation rather than a redefinition of v1: silently changing what v1
	// binds would break a rolling upgrade in both directions — an old executor would fail
	// every new job, and a new one could not open queued old envelopes.
	EnvelopeV2 = 2
)

// CredentialEnvelope is the ciphertext-only v2 transport DTO. It is never persisted
// inside Monitor.Config and every field ciphertext is bound to the full dispatch context.
type CredentialEnvelope struct {
	V      int               `json:"v"`
	Region string            `json:"region"`
	KeyID  string            `json:"key_id"`
	JobID  string            `json:"job_id"`
	Fields map[string]string `json:"fields"`
}

// CredentialKeyMaterial is one dispatch key selected by a stable id.
type CredentialKeyMaterial struct {
	ID  string
	Key []byte
}

// CredentialKeyring seals with Primary and opens by exact key id using Primary or
// Previous. It never tries unrelated keys: key_id is authenticated in the AAD.
type CredentialKeyring struct {
	primaryID string
	byID      map[string]*secret.Cipher
}

func NewCredentialKeyring(primary CredentialKeyMaterial, previous []CredentialKeyMaterial) (*CredentialKeyring, error) {
	if primary.ID == "" {
		return nil, errors.New("dispatch: primary credential key id is required")
	}
	entries := append([]CredentialKeyMaterial{primary}, previous...)
	r := &CredentialKeyring{primaryID: primary.ID, byID: make(map[string]*secret.Cipher, len(entries))}
	for _, entry := range entries {
		if entry.ID == "" {
			return nil, errors.New("dispatch: credential key id is required")
		}
		if _, exists := r.byID[entry.ID]; exists {
			return nil, fmt.Errorf("dispatch: duplicate credential key id %q", entry.ID)
		}
		cipher, err := secret.New(entry.Key)
		if err != nil {
			return nil, fmt.Errorf("dispatch: credential key %q: %w", entry.ID, err)
		}
		r.byID[entry.ID] = cipher
	}
	return r, nil
}

// envelopeAAD builds the additional authenticated data for one field.
//
// Generation 1 binds identity only: (v, region, key_id, monitor_id, execution_revision,
// field, job_id). Generation 2 appends the two digests that bind the credential to WHAT
// the job asks it to do and to which field set it belongs — the r7 amendment. The digests
// enter as RAW 32-byte SHA-256 output, never hex and never base64: one encoding, chosen
// here, so the two sides cannot each be self-consistent and mutually incompatible.
//
// v1 remains openable for as long as v1 emission is permitted, so draining queued and
// dead-lettered payloads is not a flag day.
func envelopeAAD(e CredentialEnvelope, monitorID string, revision int64, field string, binding executionBinding) []byte {
	parts := []string{
		strconv.Itoa(e.V), e.Region, e.KeyID, monitorID,
		strconv.FormatInt(revision, 10), field, e.JobID,
	}
	if e.V >= EnvelopeV2 {
		parts = append(parts, string(binding.fieldSet[:]), string(binding.body[:]))
	}
	return secret.CanonicalAAD(parts...)
}

// executionBinding carries the generation-2 digests. It is computed once per envelope and
// is never transmitted: both sides derive it from what they hold, which is the whole point.
type executionBinding struct {
	fieldSet [32]byte
	body     [32]byte
}

// SealContext is the dispatch context a credential is bound to. EnvelopeVersion is an
// explicit decision at each emit site rather than a package default: which generation a
// region is emitting is a rollout choice, and a silent default is how a wire change
// becomes a surprise.
type SealContext struct {
	EnvelopeVersion int
	Region          string
	JobID           string
	MonitorID       string
	Revision        int64
	// Body is the execution body that will be transmitted alongside this envelope. From
	// generation 2 its digest is bound into every field's AAD, so the credential cannot be
	// replayed against a different target, type or transport.
	Body domain.Monitor
}

// Seal binds each plaintext field to the dispatch context. The caller owns and wipes the
// input byte buffers after Seal returns.
func (r *CredentialKeyring) Seal(sc SealContext, fields map[string][]byte) (*CredentialEnvelope, error) {
	if r == nil {
		return nil, errors.New("dispatch: no credential keyring")
	}
	if sc.Region == "" || sc.JobID == "" || sc.MonitorID == "" || sc.Revision < 1 {
		return nil, errors.New("dispatch: incomplete credential envelope context")
	}
	if sc.EnvelopeVersion != EnvelopeV1 && sc.EnvelopeVersion != EnvelopeV2 {
		return nil, fmt.Errorf("dispatch: unsupported credential envelope version %d", sc.EnvelopeVersion)
	}
	e := &CredentialEnvelope{
		V: sc.EnvelopeVersion, Region: sc.Region, KeyID: r.primaryID, JobID: sc.JobID,
		Fields: make(map[string]string, len(fields)),
	}
	keys := make([]string, 0, len(fields))
	for field := range fields {
		if field == "" {
			return nil, errors.New("dispatch: empty credential field name")
		}
		keys = append(keys, field)
	}
	sort.Strings(keys)
	binding, err := bindingFor(e.V, sc.Body, keys)
	if err != nil {
		return nil, err
	}
	for _, field := range keys {
		ciphertext, err := r.byID[r.primaryID].EncryptBytes(fields[field], envelopeAAD(*e, sc.MonitorID, sc.Revision, field, binding))
		if err != nil {
			return nil, fmt.Errorf("dispatch: seal credential field %q: %w", field, err)
		}
		e.Fields[field] = ciphertext
	}
	return e, nil
}

// bindingFor computes the generation-2 digests, or nothing for generation 1.
func bindingFor(version int, body domain.Monitor, sortedFields []string) (executionBinding, error) {
	if version < EnvelopeV2 {
		return executionBinding{}, nil
	}
	digest, err := ExecutionBodyDigest(body)
	if err != nil {
		return executionBinding{}, fmt.Errorf("dispatch: execution binding: %w", err)
	}
	return executionBinding{fieldSet: FieldSetDigest(sortedFields), body: digest}, nil
}

// Open authenticates and decrypts a v2 job. It returns caller-owned byte buffers; callers
// should wipe them after copying into the prober's ephemeral monitor value.
func (r *CredentialKeyring) Open(job CheckJob) (map[string][]byte, error) {
	e := job.CredentialEnvelope
	if e == nil {
		return nil, errors.New("dispatch: missing credential envelope")
	}
	if e.V != EnvelopeV1 && e.V != EnvelopeV2 {
		return nil, fmt.Errorf("dispatch: unsupported credential envelope version %d", e.V)
	}
	if job.ProtocolVersion < ProtocolV2 {
		return nil, errors.New("dispatch: credential envelope requires an envelope-bearing protocol version")
	}
	if e.Region == "" || e.Region != job.Monitor.Region || e.JobID == "" {
		return nil, errors.New("dispatch: credential envelope context mismatch")
	}
	cipher := r.byID[e.KeyID]
	if cipher == nil {
		return nil, fmt.Errorf("dispatch: unknown credential key id %q", e.KeyID)
	}
	// The field-name set is derived from what ARRIVED, so truncating a multi-field envelope
	// changes every remaining field's AAD and fails authentication — not merely policy.
	names := make([]string, 0, len(e.Fields))
	for field := range e.Fields {
		names = append(names, field)
	}
	sort.Strings(names)
	binding, err := bindingFor(e.V, job.Monitor, names)
	if err != nil {
		return nil, err
	}
	fields := make(map[string][]byte, len(e.Fields))
	for _, field := range names {
		ciphertext := e.Fields[field]
		plain, err := cipher.DecryptBytes(ciphertext, envelopeAAD(*e, job.Monitor.ID, job.Monitor.ExecutionRevision, field, binding))
		if err != nil {
			WipeCredentialFields(fields)
			return nil, fmt.Errorf("dispatch: decrypt credential field %q: %w", field, err)
		}
		fields[field] = plain
	}
	return fields, nil
}

// CredentialProbeErrorReason maps an envelope-open failure to the deliberately bounded
// executor result taxonomy. Detailed errors remain local logs only; the wire never names
// a key id or distinguishes corrupt ciphertext from failed GCM authentication.
func CredentialProbeErrorReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errNoDispatchKey) {
		return domain.ProbeErrorNoDispatchKey
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "unsupported credential envelope version"):
		return domain.ProbeErrorUnsupportedVersion
	case strings.Contains(message, "unknown credential key id"):
		return domain.ProbeErrorUnknownKeyID
	default:
		// Every structural rejection — missing envelope, an envelope on a schema that
		// forbids one, a wrong field set, an empty value, a carrier mismatch — lands here
		// deliberately. The taxonomy stays non-oracular: it never tells a prober which
		// specific way its forgery was wrong.
		return domain.ProbeErrorDecryptAuthFailed
	}
}

// ProbeErrorHeartbeat builds the typed non-liveness wire member for a rejected job.
func ProbeErrorHeartbeat(job CheckJob, reason string) domain.Heartbeat {
	// The envelope's id is the AAD-bound one and exists only for credentialed jobs; the job's own id
	// exists for every job. Prefer the job's, fall back to the envelope's, so a v1 payload from a
	// fleet that predates this field still names something.
	jobID := job.JobID
	if jobID == "" && job.CredentialEnvelope != nil {
		jobID = job.CredentialEnvelope.JobID
	}
	return StampResult(domain.Heartbeat{
		MonitorID:         job.Monitor.ID,
		ExecutionRevision: job.Monitor.ExecutionRevision,
		ProbeError:        &domain.ProbeError{Reason: reason, JobID: jobID},
	}, job)
}

// errNoDispatchKey is the one structural failure that keeps its own bounded reason: an
// executor holding no keyring at all is an operator/config fact, not a payload anomaly.
var errNoDispatchKey = errors.New("dispatch: no dispatch key")

// Materialized is the result of the executor gate: the monitor to probe, the cleanup that
// wipes any injected credential, and whether a credential was actually materialized —
// which is what readiness reporting keys off, so an ordinary job never flips credential
// health either way.
type Materialized struct {
	Monitor        domain.Monitor
	Cleanup        func()
	UsedCredential bool
}

// ValidateAndMaterialize is the ONE gate every executor path crosses before a prober sees
// a job — AMQP jobs, AMQP test-RPC, pull jobs, pull tests alike (func-secret-inventory
// §4.7, D-0160).
//
// It is a package function taking the keyring, not a method, and it is called
// UNCONDITIONALLY. That shape is the fix, not a style choice: the severe r7 defect was not
// which fields an envelope carries but who decides whether one is required at all. When the
// check lived inside the open path, a job whose `credential_envelope` had simply been
// DELETED never reached it — the executor ran an ordinary job, a `redis` monitor skipped
// AUTH and PINGed, and an auth-less target answered Up. One deleted JSON member turned an
// authenticated check into an anonymous one, with no key and no forgery. A rule placed
// behind the branch it protects cannot catch that, and a rule each new call site must
// remember to invoke will eventually be forgotten.
//
// Order is fixed so behaviour is deterministic: carrier consistency, then the tri-state
// requirement resolved from the EFFECTIVE schema, then the exact field set, then AEAD.
// Every rejection happens before any connection to the target.
func ValidateAndMaterialize(ring *CredentialKeyring, delivered DeliveredJob) (Materialized, error) {
	job := delivered.Job
	plain := Materialized{Monitor: job.Monitor, Cleanup: func() {}}
	envelope := job.CredentialEnvelope

	// 1. Carrier consistency. The carrier is the executor's only authenticated signal
	// about which contract applies, so it is normative: an envelope on a legacy-generation
	// carrier is a mismatch, never a job to execute.
	if envelope != nil && delivered.CarrierGeneration < ProtocolV2 {
		return Materialized{}, fmt.Errorf("dispatch: credential envelope on carrier generation %d", delivered.CarrierGeneration)
	}

	// 2. Tri-state credential requirement. A type with no credential schema at all is an
	// ordinary monitor: legal, and forbidden to carry an envelope.
	requirement := domain.CredentialForbidden
	if domain.CredentialedType(job.Monitor.Type) {
		resolved, err := domain.ResolveCredentialRequirement(job.Monitor.Type, job.Monitor.Config)
		if err != nil {
			return Materialized{}, fmt.Errorf("dispatch: unresolved credential schema: %w", err)
		}
		requirement = resolved
	}
	// FR-028 stage 2: a scenario binding is a credential requirement even for a type with no
	// credential SCHEMA. Without this the envelope built for a synthetic monitor would be
	// rejected as "on a schema that forbids one", which is how a security mechanism turns
	// into an outage.
	scenarioBindings := domain.ScenarioSecretRefKeys(job.Monitor.Config)
	if len(scenarioBindings) > 0 {
		requirement = domain.CredentialRequired
	}
	if requirement != domain.CredentialRequired {
		if envelope != nil {
			return Materialized{}, errors.New("dispatch: credential envelope on a schema that forbids one")
		}
		return plain, nil
	}

	// The carrier generation is the POLICY boundary, which is what makes the feature switch
	// honest in both directions (§4.1: with `secrets.enabled: false`, nothing else changes).
	// A generation-1 carrier is the pre-FR-020 wire: the credential travels inline in the
	// monitor config and there is no envelope to require. Demanding one here would stop
	// every existing postgres/mysql/redis/rabbitmq monitor the moment the feature is OFF —
	// the exact opposite of the contract. Generation 2 and above never consult the inline
	// value, so this is not a fallback the stripping attack can reach: on those carriers an
	// absent envelope stays fatal.
	if delivered.CarrierGeneration <= ProtocolV1 {
		// A scenario binding has no legacy form: there is no inline value to honour, and a
		// placeholder left unsubstituted would be SENT to the target as the literal text
		// `{{secret:name}}`. Fail closed rather than probe with a broken credential.
		if len(scenarioBindings) > 0 {
			return Materialized{}, errors.New("dispatch: a scenario secret binding requires a generation-2 carrier")
		}
		if err := checkInlineCredential(job.Monitor); err != nil {
			return Materialized{}, err
		}
		return plain, nil
	}

	if envelope == nil {
		return Materialized{}, errors.New("dispatch: credential required by schema but no envelope present")
	}
	// A scenario binding requires an envelope that BINDS THE BODY. Only EnvelopeV2 does, and
	// the body digest is what makes a relocated placeholder fail: without it an attacker with
	// a valid envelope rewrites the step's URL and the credential is delivered to a target of
	// their choosing. A test asserts exactly that relocation, and it passed — wrongly — until
	// this refusal existed (FR-028 D8a).
	if len(scenarioBindings) > 0 && envelope.V < EnvelopeV2 {
		return Materialized{}, errors.New("dispatch: a scenario secret binding requires a body-bound envelope")
	}
	if ring == nil {
		return Materialized{}, errNoDispatchKey
	}

	// 3. Exact expected field set, from the schema — never from what the payload carries.
	expected, err := domain.ExpectedCredentialFields(job.Monitor.Type, job.Monitor.Config)
	if err != nil {
		return Materialized{}, fmt.Errorf("dispatch: unresolved credential schema: %w", err)
	}
	if err := checkFieldSet(envelope.Fields, expected); err != nil {
		return Materialized{}, err
	}

	// 4. AEAD, and only now.
	m, cleanup, err := ring.materialize(job)
	if err != nil {
		return Materialized{}, err
	}
	return Materialized{Monitor: m, Cleanup: cleanup, UsedCredential: true}, nil
}

// checkInlineCredential is the generation-1 half of NFR-015's fail-closed rule: the legacy
// carrier honours an inline credential, but a REQUIRED credential that is absent or empty
// must still refuse rather than run. Without this, a stripped inline password would turn a
// redis check into an anonymous PING that reports Up — the same lie the envelope rules
// exist to prevent, reached on the older wire.
func checkInlineCredential(m domain.Monitor) error {
	expected, err := domain.ExpectedCredentialFields(m.Type, m.Config)
	if err != nil {
		return fmt.Errorf("dispatch: unresolved credential schema: %w", err)
	}
	for _, name := range expected {
		if m.Config[name] == "" {
			return fmt.Errorf("dispatch: legacy job requires an inline %q credential", name)
		}
	}
	return nil
}

// checkFieldSet requires EXACTLY the expected field names: a missing one, an unknown extra
// one, or an empty ciphertext is a typed failure. Iterating whatever the payload happens to
// carry is what made truncation authenticate nothing at all.
func checkFieldSet(fields map[string]string, expected []string) error {
	if len(fields) != len(expected) {
		return fmt.Errorf("dispatch: envelope carries %d fields, schema expects %d", len(fields), len(expected))
	}
	for _, name := range expected {
		ciphertext, ok := fields[name]
		if !ok {
			return fmt.Errorf("dispatch: envelope is missing the %q field", name)
		}
		if ciphertext == "" {
			return fmt.Errorf("dispatch: envelope field %q is empty", name)
		}
	}
	return nil
}

// materialize opens an envelope into an ephemeral monitor copy. cleanup removes
// the injected strings and wipes the byte buffers best-effort after the probe.
// It is unexported: ValidateAndMaterialize is the only way in.
func (r *CredentialKeyring) materialize(job CheckJob) (domain.Monitor, func(), error) {
	fields, err := r.Open(job)
	if err != nil {
		return domain.Monitor{}, func() {}, err
	}
	// A credential that decrypts to a zero-length value counts as missing: otherwise the
	// field-set rule is bypassable by content instead of by shape (NFR-015).
	for name, value := range fields {
		if len(value) == 0 {
			WipeCredentialFields(fields)
			return domain.Monitor{}, func() {}, fmt.Errorf("dispatch: credential field %q decrypted to an empty value", name)
		}
	}
	m := job.Monitor
	cfg := make(map[string]string, len(m.Config)+len(fields))
	for k, v := range m.Config {
		cfg[k] = v
	}
	// A scenario binding is substituted INTO the scenario and never left in the config as a
	// key of its own: the prober reads the scenario, and a value sitting beside it under a
	// `scenario_secret_*` key would be a second copy nobody wipes. Everything else is
	// injected as before, under the field name the schema expects (FR-028 stage 2 D8).
	substituted := 0
	for field, value := range fields {
		if binding, ok := domain.ScenarioBindingFromField(field); ok {
			next, n := replaceScenarioBinding(cfg[domain.SyntheticScenarioKey], binding, string(value))
			if n == 0 {
				WipeCredentialFields(fields)
				return domain.Monitor{}, func() {}, fmt.Errorf("dispatch: scenario has no placeholder for binding %q", binding)
			}
			cfg[domain.SyntheticScenarioKey] = next
			substituted += n
			continue
		}
		cfg[field] = string(value)
	}
	m.Config = cfg
	cleanup := func() {
		for field := range fields {
			delete(cfg, field)
		}
		// The substituted scenario is a string, so it cannot be zeroed in place: replace it
		// with the placeholder form so the materialized copy stops carrying the credential
		// once the probe is done, and wipe the byte buffers the values came from.
		if substituted > 0 {
			delete(cfg, domain.SyntheticScenarioKey)
		}
		WipeCredentialFields(fields)
	}
	return m, cleanup, nil
}

// replaceScenarioBinding substitutes every `{{secret:<binding>}}` occurrence with the value,
// JSON-escaping it because the scenario is a JSON document and a credential may contain a
// quote or a backslash. Returns the new document and how many occurrences were replaced —
// zero means the envelope carried a field the scenario does not use, which is a mismatch
// rather than something to ignore.
func replaceScenarioBinding(scenario, binding, value string) (string, int) {
	placeholder := "{{secret:" + binding + "}}"
	n := strings.Count(scenario, placeholder)
	if n == 0 {
		return scenario, 0
	}
	escaped, err := json.Marshal(value)
	if err != nil {
		return scenario, 0
	}
	// json.Marshal wraps a string in quotes; the placeholder already sits inside a JSON
	// string literal, so the surrounding quotes are stripped.
	return strings.ReplaceAll(scenario, placeholder, string(escaped[1:len(escaped)-1])), n
}

func WipeCredentialFields(fields map[string][]byte) {
	for _, value := range fields {
		for i := range value {
			value[i] = 0
		}
	}
}

// CredentialKeyrings selects an explicit per-region ring or a configured default.
type CredentialKeyrings struct {
	Regions map[string]*CredentialKeyring
	Default *CredentialKeyring
}

func (r CredentialKeyrings) ForRegion(region string) (*CredentialKeyring, bool) {
	if ring := r.Regions[region]; ring != nil {
		return ring, true
	}
	return r.Default, r.Default != nil
}
