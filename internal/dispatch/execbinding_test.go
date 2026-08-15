package dispatch

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// bindingMonitor is a fully normalized postgres monitor: every schema default is present
// explicitly, which is the shape the materializer produces.
func bindingMonitor() domain.Monitor {
	return domain.Monitor{
		ID: "44444444-4444-4444-4444-444444444444", Region: "geo1", ExecutionRevision: 9,
		Type: domain.MonitorPostgres, Target: "db.internal:5432",
		TimeoutSeconds: 10, Retries: 2,
		Conditions: []string{"status==0", "latency<500"},
		Config: map[string]string{
			"username": "ro", "database": "app", "sslmode": "require",
			"query": "SELECT 1", "password_ref": "app-db",
		},
	}
}

// TestExecutionBodyDigestGoldenVector pins the canonical encoding to a fixed byte string.
// Only a golden vector catches an encoder that drifts: two implementations can each be
// self-consistent and mutually incompatible, and the failure mode is a fleet that cannot
// open anything rather than an obvious crash. If this test fails after an intentional
// format change, the DTO version must be bumped — that is what it is for.
func TestExecutionBodyDigestGoldenVector(t *testing.T) {
	digest, err := ExecutionBodyDigest(bindingMonitor())
	if err != nil {
		t.Fatal(err)
	}
	const want = "072978c37bc0be7c54a71050d8228bf175d4fd3fb8dd2128eb8f33c14b4a8327"
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("canonical encoding drifted:\n got %s\nwant %s\n(if the format changed on purpose, bump executionDTOVersion)", got, want)
	}
}

// TestExecutionBodyDigestIsStableAcrossEquivalentInputs covers the equivalences the
// encoding must preserve, each of which a naive encoder gets wrong.
func TestExecutionBodyDigestIsStableAcrossEquivalentInputs(t *testing.T) {
	base, err := ExecutionBodyDigest(bindingMonitor())
	if err != nil {
		t.Fatal(err)
	}
	same := func(name string, mutate func(*domain.Monitor)) {
		t.Run(name, func(t *testing.T) {
			m := bindingMonitor()
			mutate(&m)
			got, err := ExecutionBodyDigest(m)
			if err != nil {
				t.Fatal(err)
			}
			if got != base {
				t.Fatalf("digest changed for an equivalent body")
			}
		})
	}
	// Map insertion order must not matter: the keys are emitted byte-wise sorted.
	same("shuffled config insertion order", func(m *domain.Monitor) {
		shuffled := map[string]string{}
		for _, k := range []string{"query", "password_ref", "sslmode", "database", "username"} {
			shuffled[k] = m.Config[k]
		}
		m.Config = shuffled
	})
	// Fields the DTO deliberately excludes: state, display, cadence, delivery policy.
	same("excluded fields mutate freely", func(m *domain.Monitor) {
		m.Name = "renamed"
		m.Tags = []string{"prod"}
		m.IntervalSeconds = 999
		m.FailureThreshold = 7
		m.Enabled = !m.Enabled
	})
	// The ref NAME is materialization metadata: the executor reads only the injected value.
	same("password_ref renamed", func(m *domain.Monitor) { m.Config["password_ref"] = "app-db-rotated" })
	// A credential value never belongs in the body, and must not perturb the digest if a
	// caller ever leaves one there.
	same("stray password value in config", func(m *domain.Monitor) { m.Config["password"] = "leaked" })
	// nil and empty conditions are the same absence.
	nilConds := bindingMonitor()
	nilConds.Conditions = nil
	emptyConds := bindingMonitor()
	emptyConds.Conditions = []string{}
	a, _ := ExecutionBodyDigest(nilConds)
	b, _ := ExecutionBodyDigest(emptyConds)
	if a != b {
		t.Fatal("nil and empty condition lists must encode identically")
	}
}

// TestExecutionBodyDigestChangesForEveryBoundMember is the other half: every DTO member
// must actually move the digest, or binding it is a claim the code does not keep.
func TestExecutionBodyDigestChangesForEveryBoundMember(t *testing.T) {
	base, err := ExecutionBodyDigest(bindingMonitor())
	if err != nil {
		t.Fatal(err)
	}
	differs := func(name string, mutate func(*domain.Monitor)) {
		t.Run(name, func(t *testing.T) {
			m := bindingMonitor()
			mutate(&m)
			got, err := ExecutionBodyDigest(m)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatal("digest did not change for a bound member — the binding is not real")
			}
		})
	}
	differs("target", func(m *domain.Monitor) { m.Target = "attacker.example:5432" })
	differs("type", func(m *domain.Monitor) { m.Type = domain.MonitorRedis })
	differs("timeout", func(m *domain.Monitor) { m.TimeoutSeconds = 11 })
	differs("retries", func(m *domain.Monitor) { m.Retries = 3 })
	differs("condition value", func(m *domain.Monitor) { m.Conditions = []string{"status==0", "latency<9999"} })
	differs("condition order", func(m *domain.Monitor) { m.Conditions = []string{"latency<500", "status==0"} })
	differs("condition count", func(m *domain.Monitor) { m.Conditions = append(m.Conditions, "extra") })
	differs("sslmode", func(m *domain.Monitor) { m.Config["sslmode"] = "disable" })
	differs("username", func(m *domain.Monitor) { m.Config["username"] = "admin" })
	differs("database", func(m *domain.Monitor) { m.Config["database"] = "other" })
	differs("query", func(m *domain.Monitor) { m.Config["query"] = "SELECT pg_sleep(60)" })
}

// A key/value split must be unambiguous: two settings maps that concatenate to the same
// byte run must still encode differently. The length-prefixed framing is what guarantees it.
func TestExecutionBodyDigestFramingIsUnambiguous(t *testing.T) {
	a := bindingMonitor()
	a.Config["username"] = "ab"
	a.Config["database"] = "c"
	b := bindingMonitor()
	b.Config["username"] = "a"
	b.Config["database"] = "bc"
	da, _ := ExecutionBodyDigest(a)
	db, _ := ExecutionBodyDigest(b)
	if da == db {
		t.Fatal(`("ab","c") and ("a","bc") collided — the encoding is not unambiguous`)
	}
}

// TestFieldSetDigestGoldenVector pins the field-set primitive, including the synthetic
// multi-field case §4.2 does not define yet: the contract is fixed ahead of the first real
// multi-field schema so that schema does not have to relitigate it.
func TestFieldSetDigestGoldenVector(t *testing.T) {
	single := FieldSetDigest([]string{"password"})
	const wantSingle = "5fc1d476ff9d6d36e116982d22fd50f333716eddb278bd2ccdba7a75d5110307"
	if got := hex.EncodeToString(single[:]); got != wantSingle {
		t.Fatalf("single-field digest drifted:\n got %s\nwant %s", got, wantSingle)
	}
	pair := FieldSetDigest([]string{"client_cert", "password"})
	if pair == single {
		t.Fatal("a two-field set digests the same as a one-field set")
	}
	truncated := FieldSetDigest([]string{"password"})
	if truncated == pair {
		t.Fatal("truncating a multi-field set left the digest unchanged")
	}
}

// TestEnvelopeV2BindsBodyAndFieldSet is the end-to-end statement of the r7 fix: a valid
// generation-2 envelope replayed against an edited body fails authentication, while
// generation 1 — kept openable for drain — is deliberately blind to the same edit.
func TestEnvelopeV2BindsBodyAndFieldSet(t *testing.T) {
	ring := credentialTestRing(t)
	monitor := bindingMonitor()
	seal := func(version int) CheckJob {
		envelope, err := ring.Seal(SealContext{
			EnvelopeVersion: version, Region: monitor.Region, JobID: "job-bind",
			MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor,
		}, map[string][]byte{"password": []byte("s3cret")})
		if err != nil {
			t.Fatal(err)
		}
		return CheckJob{Monitor: monitor, ProtocolVersion: ProtocolV2, CredentialEnvelope: envelope}
	}

	v2 := seal(EnvelopeV2)
	if _, err := ring.Open(v2); err != nil {
		t.Fatalf("generation-2 round trip: %v", err)
	}
	retargeted := v2
	retargeted.Monitor.Target = "attacker.example:5432"
	if _, err := ring.Open(retargeted); err == nil {
		t.Fatal("generation 2 opened a credential against a swapped target")
	}
	retyped := v2
	retyped.Monitor.Type = domain.MonitorRedis
	if _, err := ring.Open(retyped); err == nil {
		t.Fatal("generation 2 opened a credential against a swapped monitor type")
	}
	relaxed := v2
	relaxed.Monitor.Config = map[string]string{}
	for k, v := range v2.Monitor.Config {
		relaxed.Monitor.Config[k] = v
	}
	relaxed.Monitor.Config["sslmode"] = "disable"
	if _, err := ring.Open(relaxed); err == nil {
		t.Fatal("generation 2 opened a credential against a downgraded sslmode")
	}

	// Generation 1 stays openable (drain) and is honestly blind to the body edit — which
	// is exactly why the generation exists rather than a redefinition of v1.
	v1 := seal(EnvelopeV1)
	if _, err := ring.Open(v1); err != nil {
		t.Fatalf("generation-1 round trip: %v", err)
	}
	v1Retargeted := v1
	v1Retargeted.Monitor.Target = "attacker.example:5432"
	if _, err := ring.Open(v1Retargeted); err != nil {
		t.Fatalf("generation 1 unexpectedly rejected a body edit: %v", err)
	}

	// Cross-generation transplant: the version is inside the AAD, so a v1 envelope
	// relabelled as v2 (or vice versa) authenticates as neither.
	relabelled := seal(EnvelopeV1)
	clone := *relabelled.CredentialEnvelope
	clone.V = EnvelopeV2
	relabelled.CredentialEnvelope = &clone
	if _, err := ring.Open(relabelled); err == nil {
		t.Fatal("a generation-1 envelope relabelled as generation 2 opened")
	}
}

// The digest is over the EFFECTIVE schema, so a body whose type has no credential schema
// cannot produce one at all — the caller has to have already resolved that it is dealing
// with a credentialed monitor.
func TestExecutionBodyDigestRefusesNonCredentialedTypes(t *testing.T) {
	m := domain.Monitor{Type: domain.MonitorHTTP, Target: "https://example.test"}
	if _, err := ExecutionBodyDigest(m); err == nil {
		t.Fatal("built an execution binding for a type with no credential schema")
	}
}

func TestCanonicalPartsAreLengthPrefixed(t *testing.T) {
	parts, err := executionDTOParts(bindingMonitor())
	if err != nil {
		t.Fatal(err)
	}
	if parts[0] != executionDTOVersion {
		t.Fatalf("first part = %q, want the DTO version", parts[0])
	}
	if !bytes.Contains([]byte(parts[1]), []byte("postgres")) {
		t.Fatalf("second part = %q, want the monitor type", parts[1])
	}
}
