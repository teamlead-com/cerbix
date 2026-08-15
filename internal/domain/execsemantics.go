package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The evaluation-semantics projection (func-service-reliability.md §6.3).
//
// A service's evaluation epoch snapshots what the evaluator READS about each member, so
// that recomputing an old range reproduces the state in force then rather than today's.
// Deciding when a new epoch is needed is therefore a question about monitor fields, and
// this file is where that question is answered — beside the type whose fields it
// classifies, not in the service code that consumes it.
//
// `UpdateMonitor` bumps `execution_revision` on ANY write: the fence is deliberately
// coarse, because a missed bump reopens the stale-config hole while an extra one costs a
// re-probe. That coarseness is safe for the fence and useless for epochs — every rename
// would create one — so the no-op decision is this projection's canonical hash instead.
//
// The trap that has to be avoided is the one a narrower snapshot walked into: classifying
// only a handful of fields moves the allowlist inside the hash rather than removing it,
// and it fails in the dangerous direction. A `target` change bumps the revision, leaves a
// (type, region, interval, enabled) snapshot byte-identical, and would produce no epoch —
// while a target change is precisely what makes two availability numbers incomparable.
// So the classification below is EXHAUSTIVE over every field of Monitor, the zero value is
// `SemanticUnclassified`, and TestMonitorFieldsAreExhaustivelyClassified fails when a new
// field appears without an explicit answer.

// SemanticClass says whether a monitor field is part of the evaluation semantics, and when
// it is not, why not. The reason is recorded rather than implied, because "out" for four
// different reasons is four different arguments a future reader may need to re-check.
type SemanticClass uint8

const (
	// SemanticUnclassified is the zero value and is never valid. A field that reaches it
	// fails the guard test rather than defaulting into — or silently out of — the epoch.
	SemanticUnclassified SemanticClass = iota
	// SemanticEvaluation: changes what endpoint or operation produced `heartbeat.up`, or
	// when a missing observation becomes stale. IN the projection.
	SemanticEvaluation
	// SemanticPresentation: display only.
	SemanticPresentation
	// SemanticDelivery: alerting, routing and suppression. Changes who is told, never what
	// was measured.
	SemanticDelivery
	// SemanticRuntime: server-owned state and watermarks.
	SemanticRuntime
	// SemanticIdentity: identity and timestamps.
	SemanticIdentity
	// SemanticSecretMaterial: never enters a snapshot row or a hash. Credential IDENTITY
	// and GENERATION do, and they are carried separately.
	SemanticSecretMaterial
)

// monitorFieldClass classifies every field of Monitor by its Go field name.
//
// Two entries are worth their comment because they look like they belong on the other
// side: `Retries` and `TimeoutSeconds` are IN — they change cadence, and cadence decides
// how long a result is held before it decays — while `FailureThreshold` and
// `ConfirmIntervalSeconds` are OUT, because confirmation changes alerting and not measured
// state (§6.7) and no history of confirmed transitions exists to read instead.
var monitorFieldClass = map[string]SemanticClass{
	// Identity.
	"ID":        SemanticIdentity,
	"ProjectID": SemanticIdentity,
	"CreatedAt": SemanticIdentity,
	"UpdatedAt": SemanticIdentity,

	// Presentation.
	"Name": SemanticPresentation,
	"Tags": SemanticPresentation,

	// Evaluation semantics.
	"Type":            SemanticEvaluation,
	"Target":          SemanticEvaluation,
	"Method":          SemanticEvaluation,
	"IntervalSeconds": SemanticEvaluation,
	"TimeoutSeconds":  SemanticEvaluation,
	"Retries":         SemanticEvaluation,
	"GraceSeconds":    SemanticEvaluation,
	"Conditions":      SemanticEvaluation,
	"Region":          SemanticEvaluation,
	"Enabled":         SemanticEvaluation,
	// Config is IN, but only its non-secret execution keys and its credential REF names
	// reach the hash; the secret value slots are excluded by construction below.
	"Config": SemanticEvaluation,

	// Delivery: who is told, not what happened.
	"FailureThreshold":       SemanticDelivery,
	"ConfirmIntervalSeconds": SemanticDelivery,
	"RenotifySeconds":        SemanticDelivery,
	"AutoIncident":           SemanticDelivery,
	"EscalationPolicyID":     SemanticDelivery,
	"DependsOn":              SemanticDelivery,

	// Server-owned runtime.
	"Status":               SemanticRuntime,
	"ConsecutiveFailures":  SemanticRuntime,
	"ExecutionRevision":    SemanticRuntime,
	"StateSequence":        SemanticRuntime,
	"LastProbeErrorReason": SemanticRuntime,
	"LastProbeErrorAt":     SemanticRuntime,
	"LastProbeErrorJobID":  SemanticRuntime,

	// Secret material.
	"PushToken": SemanticSecretMaterial,
}

// ClassifyMonitorField returns a field's semantic class, or SemanticUnclassified when the
// field is unknown to the table.
func ClassifyMonitorField(name string) SemanticClass { return monitorFieldClass[name] }

// EvaluationSemantics is the canonical projection of one monitor: everything that changes
// what endpoint or operation produced `heartbeat.up`, and nothing else.
type EvaluationSemantics struct {
	Type            MonitorType
	Target          string
	Method          string
	Region          string
	IntervalSeconds int
	TimeoutSeconds  int
	Retries         int
	GraceSeconds    int
	Enabled         bool
	Conditions      []string
	// Config carries the non-secret execution settings, with a credentialed type's values
	// resolved through the schema's canonical defaults so that an omitted key and an
	// explicitly-stated default describe the same probe and hash the same.
	Config map[string]string
	// CredentialRefs maps a credential slot to the inventory secret NAME it points at.
	// The name is identity, never material.
	CredentialRefs map[string]string
	// CredentialGenerations maps the same slots to the referenced secret's rotation
	// generation. A rotated credential can change what the probe is authorized to see, so
	// it belongs in the projection — while the value itself never does.
	CredentialGenerations map[string]string
}

const credentialRefSuffix = "_ref"

// MonitorEvaluationSemantics projects a monitor.
//
// credentialGenerations maps a credential slot key (e.g. "password_ref") to an opaque
// generation token for the secret it references — the caller supplies it, because the
// rotation marker lives in `project_secrets` and this package does no I/O. Passing nil is
// legitimate for a monitor with no credential refs.
func MonitorEvaluationSemantics(m Monitor, credentialGenerations map[string]string) (EvaluationSemantics, error) {
	e := EvaluationSemantics{
		Type:            m.Type,
		Target:          m.Target,
		Method:          m.Method,
		Region:          m.Region,
		IntervalSeconds: m.IntervalSeconds,
		TimeoutSeconds:  m.TimeoutSeconds,
		Retries:         m.Retries,
		GraceSeconds:    m.GraceSeconds,
		Enabled:         m.Enabled,
		Conditions:      append([]string(nil), m.Conditions...),
		Config:          map[string]string{},
		CredentialRefs:  map[string]string{},
	}

	if CredentialedType(m.Type) {
		// Reuse FR-020's own registry rather than re-deriving which keys are safe: it
		// already answers "which non-secret settings does the execution see", and a second
		// answer to that question is exactly the drift this file exists to prevent.
		keys, err := ExecutionBindingKeys(m.Type, m.Config)
		if err != nil {
			return EvaluationSemantics{}, fmt.Errorf("domain: evaluation semantics: %w", err)
		}
		for _, k := range keys {
			v, err := CanonicalSettingValue(m.Type, m.Config, k)
			if err != nil {
				return EvaluationSemantics{}, fmt.Errorf("domain: evaluation semantics: %w", err)
			}
			e.Config[k] = v
		}
		for k, v := range m.Config {
			if strings.HasSuffix(k, credentialRefSuffix) {
				e.CredentialRefs[k] = v
			}
		}
	} else {
		// A non-credentialed type's config holds plain execution settings only; there is
		// no secret slot to exclude.
		for k, v := range m.Config {
			e.Config[k] = v
		}
	}

	if len(credentialGenerations) > 0 {
		e.CredentialGenerations = map[string]string{}
		for k, v := range credentialGenerations {
			e.CredentialGenerations[k] = v
		}
	}
	return e, nil
}

// Canonical returns a deterministic byte encoding of the projection.
//
// Every part is length-prefixed. Without that, ("ab","c") and ("a","bc") encode
// identically and two different monitors collide — the same reason the credential AAD is
// built that way.
func (e EvaluationSemantics) Canonical() []byte {
	var parts [][]byte
	add := func(s string) { parts = append(parts, []byte(s)) }

	add("cerbix.evalsem.v1")
	add(string(e.Type))
	add(e.Target)
	add(e.Method)
	add(e.Region)
	add(strconv.Itoa(e.IntervalSeconds))
	add(strconv.Itoa(e.TimeoutSeconds))
	add(strconv.Itoa(e.Retries))
	add(strconv.Itoa(e.GraceSeconds))
	add(strconv.FormatBool(e.Enabled))

	// Conditions are order-sensitive: they describe a sequence of assertions, and sorting
	// them would make two different probes hash the same.
	add(strconv.Itoa(len(e.Conditions)))
	for _, c := range e.Conditions {
		add(c)
	}

	for _, m := range []map[string]string{e.Config, e.CredentialRefs, e.CredentialGenerations} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		add(strconv.Itoa(len(keys)))
		for _, k := range keys {
			add(k)
			add(m[k])
		}
	}

	var buf []byte
	var n [binary.MaxVarintLen64]byte
	buf = append(buf, n[:binary.PutUvarint(n[:], uint64(len(parts)))]...)
	for _, p := range parts {
		buf = append(buf, n[:binary.PutUvarint(n[:], uint64(len(p)))]...)
		buf = append(buf, p...)
	}
	return buf
}

// Hash is the value an evaluation epoch stores and compares. An execution write whose hash
// is unchanged creates no epoch (§6.2).
func (e EvaluationSemantics) Hash() string {
	sum := sha256.Sum256(e.Canonical())
	return hex.EncodeToString(sum[:])
}
