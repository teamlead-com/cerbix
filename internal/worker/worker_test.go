package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, m domain.Monitor) domain.Heartbeat {
	return domain.Heartbeat{MonitorID: m.ID, Up: true, Code: 200}
}

type credentialReadinessCall struct {
	ready  bool
	reason string
}

type fakeCredentialReadiness struct {
	mu          sync.Mutex
	calls       []credentialReadinessCall
	probeErrors []string
}

func (f *fakeCredentialReadiness) RecordExecutorProbeError(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeErrors = append(f.probeErrors, reason)
}

func (f *fakeCredentialReadiness) SetCredentialReady(ready bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, credentialReadinessCall{ready: ready, reason: reason})
}

func (f *fakeCredentialReadiness) last() credentialReadinessCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func TestPoolProcessesJobs(t *testing.T) {
	disp := dispatch.NewInProc(8)
	p := New(disp, fakeRunner{}, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if err := disp.PublishJob(ctx, dispatch.CheckJob{Monitor: domain.Monitor{ID: "m1", Type: domain.MonitorHTTP}}); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	select {
	case hb := <-disp.Results():
		if hb.MonitorID != "m1" || !hb.Up {
			t.Fatalf("result = %+v", hb)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not produce a result")
	}
}

func TestNewClampsSize(t *testing.T) {
	p := New(dispatch.NewInProc(1), fakeRunner{}, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if p.size != 1 {
		t.Fatalf("size = %d, want clamped to 1", p.size)
	}
}

func TestCredentialFailurePublishesTypedProbeError(t *testing.T) {
	workerRing, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "worker", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "other", Key: bytes.Repeat([]byte{2}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m-secret", Type: domain.MonitorPostgres, Region: "core", ExecutionRevision: 7}
	envelope, err := sealer.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV1, Region: "core", JobID: "job-1", MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	disp := dispatch.NewInProc(2)
	readiness := &fakeCredentialReadiness{}
	pool := New(disp, fakeRunner{}, 1, slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialKeyring(workerRing).WithCredentialReadiness(readiness)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)
	if err := disp.PublishJob(ctx, dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope}); err != nil {
		t.Fatal(err)
	}
	select {
	case hb := <-disp.Results():
		if hb.ProbeError == nil || hb.ProbeError.Reason != domain.ProbeErrorUnknownKeyID || hb.ProbeError.JobID != "job-1" || hb.ExecutionRevision != 7 {
			t.Fatalf("typed result = %+v", hb)
		}
		if !hb.Ts.IsZero() {
			t.Fatalf("probe_error must not masquerade as heartbeat: ts=%v", hb.Ts)
		}
		// One mismatch is a per-job diagnostic, not an unhealthy executor: readiness
		// degrades on a PERSISTENT key mismatch (§4.7), and during a dispatch-key rotation
		// an envelope under a retired key id is expected traffic.
		if got := readiness.last(); !got.ready {
			t.Fatalf("a single key mismatch degraded worker readiness: %+v", got)
		}
		readiness.mu.Lock()
		if len(readiness.probeErrors) != 1 || readiness.probeErrors[0] != domain.ProbeErrorUnknownKeyID {
			t.Fatalf("executor probe error metrics = %v", readiness.probeErrors)
		}
		readiness.mu.Unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not publish probe_error")
	}

	// A second mismatch makes it persistent, and the worker goes unready.
	if err := disp.PublishJob(ctx, dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-disp.Results():
		if got := readiness.last(); got.ready || got.reason != domain.ProbeErrorUnknownKeyID {
			t.Fatalf("readiness after a persistent key mismatch = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not publish the second probe_error")
	}
}

func TestCredentialReadinessRecoversAfterSuccessfulDecrypt(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "worker", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m-secret", Type: domain.MonitorPostgres, Region: "core", ExecutionRevision: 7}
	envelope, err := ring.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV1, Region: "core", JobID: "job-2", MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	disp := dispatch.NewInProc(2)
	readiness := &fakeCredentialReadiness{}
	pool := New(disp, fakeRunner{}, 1, slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialKeyring(ring).WithCredentialReadiness(readiness)
	if got := readiness.last(); !got.ready || got.reason != "" {
		t.Fatalf("initial readiness with keyring = %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)
	if err := disp.PublishJob(ctx, dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope}); err != nil {
		t.Fatal(err)
	}
	select {
	case hb := <-disp.Results():
		if hb.ProbeError != nil || !hb.Up {
			t.Fatalf("result = %+v", hb)
		}
		if got := readiness.last(); !got.ready || got.reason != "" {
			t.Fatalf("readiness after successful decrypt = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not publish successful result")
	}
}

// TestFeatureOffCredentialJobReachesTheProber is the executor-level half of the r7-review
// P0. With `secrets.enabled: false` the scheduler publishes credentialed monitors on the
// legacy carrier with the credential inline and no envelope, and the worker holds no
// keyring at all. The gate runs on every job, so it has to let this through — otherwise
// turning the feature OFF stops every existing postgres/mysql/redis/rabbitmq check.
func TestFeatureOffCredentialJobReachesTheProber(t *testing.T) {
	monitor := domain.Monitor{
		ID: "m-legacy", Type: domain.MonitorPostgres, Region: "core", ExecutionRevision: 3,
		Target: "db.internal:5432",
		Config: map[string]string{"username": "ro", "database": "app", "sslmode": "require", "password": "inline"},
	}
	disp := dispatch.NewInProc(2)
	readiness := &fakeCredentialReadiness{}
	// No keyring: exactly the feature-off worker profile.
	pool := New(disp, fakeRunner{}, 1, slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialReadiness(readiness)
	readiness.mu.Lock()
	callsAtStartup := len(readiness.calls) // wiring already reports "no keyring"; that is not the job's doing
	readiness.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)
	if err := disp.PublishJob(ctx, dispatch.CheckJob{Monitor: monitor}); err != nil {
		t.Fatal(err)
	}
	select {
	case hb := <-disp.Results():
		if hb.ProbeError != nil {
			t.Fatalf("legacy credentialed job rejected with the feature off: %+v", hb.ProbeError)
		}
		if hb.MonitorID != "m-legacy" || !hb.Up {
			t.Fatalf("unexpected result %+v", hb)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("legacy credentialed job never reached the prober")
	}
	// Readiness is a dispatch-credential concept: a legacy job must not move it.
	// Readiness is a dispatch-credential concept: running a legacy job must not move it.
	readiness.mu.Lock()
	defer readiness.mu.Unlock()
	if len(readiness.calls) != callsAtStartup {
		t.Fatalf("legacy job moved credential readiness: %+v", readiness.calls[callsAtStartup:])
	}
}

// The result an executor publishes carries the job it answers (func-result-protocol §9, iter-0155).
// Found by mutation rather than by design: removing the stamp from the worker left every test green,
// which means the correlation the whole feature exists for was resting on nothing.
func TestPublishedResultCarriesTheJobItAnswers(t *testing.T) {
	disp := dispatch.NewInProc(8)
	p := New(disp, fakeRunner{}, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	issued := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Millisecond)
	job := dispatch.CheckJob{
		Monitor:  domain.Monitor{ID: "m1", Type: domain.MonitorHTTP},
		JobID:    "job-42",
		IssuedAt: issued,
	}
	if err := disp.PublishJob(ctx, job); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	select {
	case hb := <-disp.Results():
		if hb.JobID != "job-42" {
			t.Fatalf("result job id = %q, want job-42 — without it the core cannot tell which dispatch "+
				"this result answers, which is the whole point of the id", hb.JobID)
		}
		if !hb.JobIssuedAt.Equal(issued) {
			t.Fatalf("result issue instant = %s, want the core's %s — the ordering check compares an "+
				"executor's clock against THIS instant, so a dropped stamp disables the check silently",
				hb.JobIssuedAt, issued)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not produce a result")
	}
}
