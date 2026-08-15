package dispatch

import (
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
	EnvelopeV1 = 1
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

func envelopeAAD(e CredentialEnvelope, monitorID string, revision int64, field string) []byte {
	return secret.CanonicalAAD(
		strconv.Itoa(e.V), e.Region, e.KeyID, monitorID,
		strconv.FormatInt(revision, 10), field, e.JobID,
	)
}

// Seal binds each plaintext field to the envelope/monitor/job context. The caller owns
// and wipes the input byte buffers after Seal returns.
func (r *CredentialKeyring) Seal(region, jobID, monitorID string, revision int64, fields map[string][]byte) (*CredentialEnvelope, error) {
	if r == nil {
		return nil, errors.New("dispatch: no credential keyring")
	}
	if region == "" || jobID == "" || monitorID == "" || revision < 1 {
		return nil, errors.New("dispatch: incomplete credential envelope context")
	}
	e := &CredentialEnvelope{V: EnvelopeV1, Region: region, KeyID: r.primaryID, JobID: jobID, Fields: make(map[string]string, len(fields))}
	keys := make([]string, 0, len(fields))
	for field := range fields {
		keys = append(keys, field)
	}
	sort.Strings(keys)
	for _, field := range keys {
		if field == "" {
			return nil, errors.New("dispatch: empty credential field name")
		}
		ciphertext, err := r.byID[r.primaryID].EncryptBytes(fields[field], envelopeAAD(*e, monitorID, revision, field))
		if err != nil {
			return nil, fmt.Errorf("dispatch: seal credential field %q: %w", field, err)
		}
		e.Fields[field] = ciphertext
	}
	return e, nil
}

// Open authenticates and decrypts a v2 job. It returns caller-owned byte buffers; callers
// should wipe them after copying into the prober's ephemeral monitor value.
func (r *CredentialKeyring) Open(job CheckJob) (map[string][]byte, error) {
	e := job.CredentialEnvelope
	if e == nil {
		return nil, errors.New("dispatch: missing credential envelope")
	}
	if e.V != EnvelopeV1 {
		return nil, fmt.Errorf("dispatch: unsupported credential envelope version %d", e.V)
	}
	if job.ProtocolVersion != ProtocolV2 {
		return nil, errors.New("dispatch: credential envelope requires protocol_version 2")
	}
	if e.Region == "" || e.Region != job.Monitor.Region || e.JobID == "" {
		return nil, errors.New("dispatch: credential envelope context mismatch")
	}
	cipher := r.byID[e.KeyID]
	if cipher == nil {
		return nil, fmt.Errorf("dispatch: unknown credential key id %q", e.KeyID)
	}
	fields := make(map[string][]byte, len(e.Fields))
	for field, ciphertext := range e.Fields {
		plain, err := cipher.DecryptBytes(ciphertext, envelopeAAD(*e, job.Monitor.ID, job.Monitor.ExecutionRevision, field))
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
	jobID := ""
	if job.CredentialEnvelope != nil {
		jobID = job.CredentialEnvelope.JobID
	}
	return domain.Heartbeat{
		MonitorID:         job.Monitor.ID,
		ExecutionRevision: job.Monitor.ExecutionRevision,
		ProbeError:        &domain.ProbeError{Reason: reason, JobID: jobID},
	}
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
	if requirement != domain.CredentialRequired {
		if envelope != nil {
			return Materialized{}, errors.New("dispatch: credential envelope on a schema that forbids one")
		}
		return plain, nil
	}
	if envelope == nil {
		return Materialized{}, errors.New("dispatch: credential required by schema but no envelope present")
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
	for field, value := range fields {
		cfg[field] = string(value)
	}
	m.Config = cfg
	cleanup := func() {
		for field := range fields {
			delete(cfg, field)
		}
		WipeCredentialFields(fields)
	}
	return m, cleanup, nil
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
