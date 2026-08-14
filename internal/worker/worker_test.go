package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, m domain.Monitor) domain.Heartbeat {
	return domain.Heartbeat{MonitorID: m.ID, Up: true, Code: 200}
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
	envelope, err := sealer.Seal("core", "job-1", monitor.ID, monitor.ExecutionRevision, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	disp := dispatch.NewInProc(2)
	pool := New(disp, fakeRunner{}, 1, slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialKeyring(workerRing)
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
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not publish probe_error")
	}
}
