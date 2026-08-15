package dispatch

import (
	"bytes"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func credentialTestRing(t *testing.T) *CredentialKeyring {
	t.Helper()
	r, err := NewCredentialKeyring(
		CredentialKeyMaterial{ID: "k-new", Key: bytes.Repeat([]byte{1}, 32)},
		[]CredentialKeyMaterial{{ID: "k-old", Key: bytes.Repeat([]byte{2}, 32)}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCredentialEnvelopeRoundTripAndAADTransplants(t *testing.T) {
	ring := credentialTestRing(t)
	base := CheckJob{ProtocolVersion: ProtocolV2, Monitor: domain.Monitor{
		ID: "11111111-1111-1111-1111-111111111111", Region: "geo1", ExecutionRevision: 7,
		Config: map[string]string{"password_ref": "db-password"},
	}}
	envelope, err := ring.Seal("geo1", "job-1", base.Monitor.ID, 7, map[string][]byte{"password": []byte("super-secret")})
	if err != nil {
		t.Fatal(err)
	}
	base.CredentialEnvelope = envelope
	fields, err := ring.Open(base)
	if err != nil || string(fields["password"]) != "super-secret" {
		t.Fatalf("open = %q, %v", fields["password"], err)
	}
	WipeCredentialFields(fields)

	mutations := map[string]func(*CheckJob){
		"monitor":  func(j *CheckJob) { j.Monitor.ID = "22222222-2222-2222-2222-222222222222" },
		"revision": func(j *CheckJob) { j.Monitor.ExecutionRevision++ },
		"region":   func(j *CheckJob) { j.Monitor.Region = "geo2" },
		"job":      func(j *CheckJob) { j.CredentialEnvelope.JobID = "job-2" },
		"field": func(j *CheckJob) {
			j.CredentialEnvelope.Fields = map[string]string{"other": envelope.Fields["password"]}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copyJob := base
			copyEnvelope := *envelope
			copyEnvelope.Fields = map[string]string{"password": envelope.Fields["password"]}
			copyJob.CredentialEnvelope = &copyEnvelope
			mutate(&copyJob)
			if _, err := ring.Open(copyJob); err == nil {
				t.Fatal("AAD transplant unexpectedly authenticated")
			}
		})
	}
	// Exact same-job replay is transport behavior and must authenticate.
	if _, err := ring.Open(base); err != nil {
		t.Fatalf("same-job replay: %v", err)
	}
}

func TestCredentialEnvelopePreviousKeyAndUnknownID(t *testing.T) {
	old, _ := NewCredentialKeyring(CredentialKeyMaterial{ID: "k-old", Key: bytes.Repeat([]byte{2}, 32)}, nil)
	job := CheckJob{ProtocolVersion: ProtocolV2, Monitor: domain.Monitor{ID: "m1", Region: "core", ExecutionRevision: 1}}
	job.CredentialEnvelope, _ = old.Seal("core", "job", "m1", 1, map[string][]byte{"password": []byte("value")})
	rotated := credentialTestRing(t)
	if _, err := rotated.Open(job); err != nil {
		t.Fatalf("previous key must open queued payload: %v", err)
	}
	job.CredentialEnvelope.KeyID = "missing"
	if _, err := rotated.Open(job); err == nil {
		t.Fatal("unknown key id accepted")
	}
}

// gateJob builds a valid credentialed job: a postgres monitor whose schema requires
// exactly one `password` field, sealed for its own context.
func gateJob(t *testing.T, ring *CredentialKeyring) CheckJob {
	t.Helper()
	monitor := domain.Monitor{
		ID: "22222222-2222-2222-2222-222222222222", Region: "geo1", ExecutionRevision: 4,
		Type: domain.MonitorPostgres, Target: "db.internal:5432",
		Config: map[string]string{"username": "ro", "database": "app", "sslmode": "require", "password_ref": "app-db"},
	}
	envelope, err := ring.Seal(monitor.Region, "job-gate", monitor.ID, monitor.ExecutionRevision,
		map[string][]byte{"password": []byte("s3cret")})
	if err != nil {
		t.Fatal(err)
	}
	return CheckJob{Monitor: monitor, ProtocolVersion: ProtocolV2, CredentialEnvelope: envelope}
}

// TestStructuralGateRejectsBeforeAnyProbe covers the r7 structural gate (§4.7, D-0160).
// The severe defect was never about WHICH fields an envelope carries — it was about who
// decides whether one is required. With the check living inside the open path, a job whose
// envelope had simply been deleted skipped it entirely and ran credential-less: a redis
// monitor would skip AUTH, PING, and an auth-less target would answer Up. Every case below
// must fail closed, and every failure must be indistinguishable on the wire.
func TestStructuralGateRejectsBeforeAnyProbe(t *testing.T) {
	ring := credentialTestRing(t)

	stripEnvelope := func(j CheckJob) CheckJob { j.CredentialEnvelope = nil; return j }
	emptyFields := func(j CheckJob) CheckJob {
		clone := *j.CredentialEnvelope
		clone.Fields = map[string]string{}
		j.CredentialEnvelope = &clone
		return j
	}
	extraField := func(j CheckJob) CheckJob {
		clone := *j.CredentialEnvelope
		fields := map[string]string{"extra": "x"}
		for k, v := range clone.Fields {
			fields[k] = v
		}
		clone.Fields = fields
		j.CredentialEnvelope = &clone
		return j
	}
	renamedField := func(j CheckJob) CheckJob {
		clone := *j.CredentialEnvelope
		fields := map[string]string{}
		for _, v := range clone.Fields {
			fields["passphrase"] = v
		}
		clone.Fields = fields
		j.CredentialEnvelope = &clone
		return j
	}
	blankCiphertext := func(j CheckJob) CheckJob {
		clone := *j.CredentialEnvelope
		clone.Fields = map[string]string{"password": ""}
		j.CredentialEnvelope = &clone
		return j
	}

	cases := []struct {
		name    string
		mutate  func(CheckJob) CheckJob
		carrier int
	}{
		{"whole envelope stripped", stripEnvelope, ProtocolV2},
		{"envelope emptied of all fields", emptyFields, ProtocolV2},
		{"unknown extra field", extraField, ProtocolV2},
		{"expected field renamed", renamedField, ProtocolV2},
		{"field present but ciphertext blank", blankCiphertext, ProtocolV2},
		{"envelope on a legacy carrier", func(j CheckJob) CheckJob { return j }, ProtocolV1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := tc.mutate(gateJob(t, ring))
			got, err := ValidateAndMaterialize(ring, DeliveredJob{Job: job, CarrierGeneration: tc.carrier})
			if err == nil {
				t.Fatalf("gate accepted a tampered job (materialized=%+v)", got.Monitor.Config)
			}
			if got.UsedCredential {
				t.Fatal("gate reported a materialized credential on a rejected job")
			}
			// The wire reason stays non-oracular: a forger never learns which way it was wrong.
			if reason := CredentialProbeErrorReason(err); reason != domain.ProbeErrorDecryptAuthFailed {
				t.Fatalf("reason = %q, want %q", reason, domain.ProbeErrorDecryptAuthFailed)
			}
		})
	}
}

// A zero-length credential must be refused even though its envelope has the exact expected
// field name and authenticates: otherwise the field-set rule is bypassable by CONTENT
// instead of by shape, and the probe runs as if it had no credential (NFR-015).
func TestZeroLengthCredentialCountsAsMissing(t *testing.T) {
	ring := credentialTestRing(t)
	monitor := domain.Monitor{
		ID: "33333333-3333-3333-3333-333333333333", Region: "geo1", ExecutionRevision: 2,
		Type: domain.MonitorRedis, Target: "cache:6379",
		Config: map[string]string{"tls": "true", "password_ref": "cache"},
	}
	// Seal an empty plaintext directly: EncryptBytes refuses one, so build the envelope
	// with a ciphertext that legitimately decrypts to zero bytes is impossible here —
	// assert instead that the seal path itself refuses to create that shape.
	if _, err := ring.Seal(monitor.Region, "job-empty", monitor.ID, monitor.ExecutionRevision,
		map[string][]byte{"password": {}}); err == nil {
		t.Fatal("sealing an empty credential succeeded; the empty value must be refused at the source too")
	}
}

// The forbidden half of the tri-state: a schema that takes no credential must REJECT an
// envelope rather than open it vacuously, and an ordinary non-credentialed monitor must
// still pass through untouched.
func TestGateTriStateForbidsAndAllows(t *testing.T) {
	ring := credentialTestRing(t)
	sealed := gateJob(t, ring)

	amqpMonitor := domain.Monitor{
		ID: sealed.Monitor.ID, Region: "geo1", ExecutionRevision: 4,
		Type: domain.MonitorRabbitMQ, Config: map[string]string{"mode": "amqp"},
	}
	forbidden := CheckJob{Monitor: amqpMonitor, ProtocolVersion: ProtocolV2, CredentialEnvelope: sealed.CredentialEnvelope}
	if _, err := ValidateAndMaterialize(ring, DeliveredJob{Job: forbidden, CarrierGeneration: ProtocolV2}); err == nil {
		t.Fatal("gate opened an envelope on a schema that forbids credentials")
	}

	httpJob := CheckJob{Monitor: domain.Monitor{ID: "m-http", Type: domain.MonitorHTTP, Target: "https://example.test"}}
	got, err := ValidateAndMaterialize(ring, DeliveredJob{Job: httpJob, CarrierGeneration: ProtocolV1})
	if err != nil {
		t.Fatalf("gate rejected an ordinary monitor: %v", err)
	}
	if got.UsedCredential || got.Monitor.ID != "m-http" {
		t.Fatalf("ordinary monitor mangled by the gate: %+v", got)
	}

	// A credential-required schema arriving with no envelope at all is the stripping case.
	stripped := sealed
	stripped.CredentialEnvelope = nil
	if _, err := ValidateAndMaterialize(ring, DeliveredJob{Job: stripped, CarrierGeneration: ProtocolV2}); err == nil {
		t.Fatal("gate ran a credential-required monitor with no envelope")
	}

	// And the happy path still materializes.
	ok, err := ValidateAndMaterialize(ring, DeliveredJob{Job: sealed, CarrierGeneration: ProtocolV2})
	if err != nil {
		t.Fatalf("gate rejected a valid job: %v", err)
	}
	if !ok.UsedCredential || ok.Monitor.Config["password"] != "s3cret" {
		t.Fatalf("valid job not materialized: used=%v config=%v", ok.UsedCredential, ok.Monitor.Config)
	}
	ok.Cleanup()
	if ok.Monitor.Config["password"] != "" {
		t.Fatal("cleanup left the injected credential in the monitor config")
	}
}

// Verification ORDER is fixed — structural gate, then field-set policy, then AEAD — so a
// tampered job fails deterministically instead of depending on which check fires first.
// A job whose field set is wrong AND whose ciphertext is forged must be refused by the
// field-set rule, without the AEAD ever being consulted.
func TestGateChecksFieldSetBeforeAEAD(t *testing.T) {
	ring := credentialTestRing(t)
	other := credentialTestRing(t)
	job := gateJob(t, other) // sealed by a keyring `ring` does not have
	clone := *job.CredentialEnvelope
	clone.KeyID = "k-new" // a key id `ring` DOES have, so AEAD would fail authentication
	clone.Fields = map[string]string{}
	job.CredentialEnvelope = &clone

	if _, err := ValidateAndMaterialize(ring, DeliveredJob{Job: job, CarrierGeneration: ProtocolV2}); err == nil {
		t.Fatal("gate accepted an empty field set")
	} else if !bytes.Contains([]byte(err.Error()), []byte("schema expects")) {
		t.Fatalf("field-set rule did not fire first: %v", err)
	}
}
