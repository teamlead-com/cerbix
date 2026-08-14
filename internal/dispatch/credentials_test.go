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
