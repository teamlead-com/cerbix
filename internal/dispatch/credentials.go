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
	message := err.Error()
	switch {
	case strings.Contains(message, "unsupported credential envelope version"):
		return domain.ProbeErrorUnsupportedVersion
	case strings.Contains(message, "unknown credential key id"):
		return domain.ProbeErrorUnknownKeyID
	default:
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

// MaterializeForProbe opens an envelope into an ephemeral monitor copy. cleanup removes
// the injected strings and wipes the byte buffers best-effort after the probe.
func (r *CredentialKeyring) MaterializeForProbe(job CheckJob) (domain.Monitor, func(), error) {
	if job.CredentialEnvelope == nil {
		return job.Monitor, func() {}, nil
	}
	fields, err := r.Open(job)
	if err != nil {
		return domain.Monitor{}, func() {}, err
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
