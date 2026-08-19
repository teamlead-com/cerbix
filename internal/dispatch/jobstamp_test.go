package dispatch

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The job identity travels: core → executor → result (func-result-protocol §9, iter-0155). Three
// executors publish results — the AMQP worker pool, the pull agent's batch and the probe-error path —
// and they all go through StampResult, because a stamp applied separately in three places drifts the
// first time one of them is edited.
func TestStampResultCarriesTheJobIdentity(t *testing.T) {
	issued := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	job := CheckJob{Monitor: domain.Monitor{ID: "m1"}, JobID: "job-1", IssuedAt: issued}

	hb := StampResult(domain.Heartbeat{MonitorID: "m1", Up: true}, job)
	if hb.JobID != "job-1" || !hb.JobIssuedAt.Equal(issued) {
		t.Fatalf("stamped result = %q/%s, want job-1/%s — the core's issue instant is the only clock the "+
			"ordering check can compare an executor against", hb.JobID, hb.JobIssuedAt, issued)
	}
	// A job that carries no identity must not gain an invented one: zero means "not carried", and the
	// store's ordering check reads it as "do not judge this result" rather than "issued at the epoch".
	bare := StampResult(domain.Heartbeat{MonitorID: "m1"}, CheckJob{Monitor: domain.Monitor{ID: "m1"}})
	if bare.JobID != "" || !bare.JobIssuedAt.IsZero() {
		t.Errorf("an unidentified job produced %q/%s, want empty/zero", bare.JobID, bare.JobIssuedAt)
	}
}

// The probe-error result is a result too: a rejected credentialed job must be traceable to the job
// that was rejected. Its id has two possible sources and the precedence matters — the job's own id
// exists for every job, the envelope's only for credentialed ones, and the envelope's is AAD-bound.
func TestProbeErrorHeartbeatNamesItsJob(t *testing.T) {
	issued := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	job := CheckJob{
		Monitor: domain.Monitor{ID: "m1", ExecutionRevision: 7}, JobID: "job-1", IssuedAt: issued,
		CredentialEnvelope: &CredentialEnvelope{JobID: "envelope-job"},
	}
	hb := ProbeErrorHeartbeat(job, "decrypt_auth_failed")
	if hb.JobID != "job-1" || !hb.JobIssuedAt.Equal(issued) {
		t.Fatalf("probe-error result = %q/%s, want the JOB's identity", hb.JobID, hb.JobIssuedAt)
	}
	if hb.ProbeError == nil || hb.ProbeError.JobID != "job-1" {
		t.Fatalf("probe error names %+v, want job-1 — the job's own id, which every job has", hb.ProbeError)
	}
	if hb.ExecutionRevision != 7 {
		t.Errorf("execution revision = %d, want the job's 7", hb.ExecutionRevision)
	}

	// A payload from a fleet older than CheckJob.JobID still names something: the envelope's id.
	legacy := CheckJob{
		Monitor:            domain.Monitor{ID: "m1"},
		CredentialEnvelope: &CredentialEnvelope{JobID: "envelope-job"},
	}
	if hb := ProbeErrorHeartbeat(legacy, "unknown_key_id"); hb.ProbeError.JobID != "envelope-job" {
		t.Errorf("legacy probe error names %q, want the envelope's id as the fallback", hb.ProbeError.JobID)
	}
}
