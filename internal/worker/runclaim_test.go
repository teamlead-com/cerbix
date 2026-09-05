package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase C — the AMQP worker emits its claim AFTER taking the job off the transport and
// BEFORE the first attempt (§8.4).
//
// The ordering is the whole point and it is what the runner below asserts: a claim sent after the
// probe would never exist for a run that crashed mid-probe, which is the state invariant 6 is
// about. So the fake runner does not merely record that it ran — it records whether the claim had
// already been published when it started.

// orderingRunner reports what the dispatcher had seen at the moment the probe began.
type orderingRunner struct {
	mu        sync.Mutex
	disp      *dispatch.InProc
	seenAtRun []domain.Heartbeat
	ran       chan struct{}
}

func (r *orderingRunner) Run(_ context.Context, m domain.Monitor) domain.Heartbeat {
	r.mu.Lock()
	// Drain whatever is already queued, without blocking: this is the state of the world at the
	// instant the probe starts.
	for {
		select {
		case hb := <-r.disp.Results():
			r.seenAtRun = append(r.seenAtRun, hb)
			continue
		default:
		}
		break
	}
	r.mu.Unlock()
	select {
	case r.ran <- struct{}{}:
	default:
	}
	return domain.Heartbeat{MonitorID: m.ID, Up: true, Code: 200}
}

func (r *orderingRunner) claimsBeforeTheProbe() []domain.Heartbeat {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Heartbeat
	for _, hb := range r.seenAtRun {
		if hb.Claim != nil {
			out = append(out, hb)
		}
	}
	return out
}

func ledgerJob() dispatch.CheckJob {
	at := time.Now().UTC()
	return dispatch.CheckJob{
		Monitor:         domain.Monitor{ID: "m1", Type: domain.MonitorHTTP, ExecutionRevision: 4},
		ProtocolVersion: dispatch.ProtocolV4,
		JobID:           "11111111-1111-4111-8111-111111111111",
		IssuedAt:        at,
		DueAt:           at.Add(-time.Minute),
	}
}

func TestTheWorkerClaimsBeforeItProbes(t *testing.T) {
	disp := dispatch.NewInProc(8)
	runner := &orderingRunner{disp: disp, ran: make(chan struct{}, 1)}
	p := New(disp, runner, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	job := ledgerJob()
	if err := disp.PublishJob(ctx, job); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	select {
	case <-runner.ran:
	case <-time.After(3 * time.Second):
		t.Fatal("the worker never probed")
	}

	claims := runner.claimsBeforeTheProbe()
	if len(claims) != 1 {
		t.Fatalf("%d claims had been published when the probe began, want 1: a claim sent AFTER "+
			"the probe would never exist for a run that crashed mid-probe, which is the state "+
			"invariant 6 is about", len(claims))
	}
	claim := claims[0]
	if claim.JobID != job.JobID || !claim.DueAt.Equal(job.DueAt) {
		t.Errorf("the claim does not correlate to the job it answers: %+v", claim)
	}
	if claim.Claim.At.IsZero() {
		t.Error("the claim carries no instant")
	}
	if !claim.Ts.IsZero() || claim.Up {
		t.Errorf("the claim looks like a result: %+v", claim)
	}
}

// A job carrying no window produces NO claim, so a fleet running below the ledger carrier pays
// nothing for a feature it cannot feed. The mutation that must kill this: claim unconditionally.
func TestTheWorkerSendsNoClaimForAJobWithNoWindow(t *testing.T) {
	disp := dispatch.NewInProc(8)
	runner := &orderingRunner{disp: disp, ran: make(chan struct{}, 1)}
	p := New(disp, runner, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// The ordinary pre-ledger job: no identity at all.
	if err := disp.PublishJob(ctx, dispatch.CheckJob{
		Monitor: domain.Monitor{ID: "m1", Type: domain.MonitorHTTP},
	}); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	select {
	case <-runner.ran:
	case <-time.After(3 * time.Second):
		t.Fatal("the worker never probed")
	}
	if got := runner.claimsBeforeTheProbe(); len(got) != 0 {
		t.Fatalf("%d claims were published for a job with no window: the message can correlate to "+
			"nothing and is pure cost", len(got))
	}
}

// A job the credential gate REFUSES is never claimed: it was not claimed for execution, it produced
// a probe_error, and a probe_error is a terminal outcome of its own. Claiming it would say a run
// started where the executor refused to start one.
func TestARefusedJobIsNeverClaimed(t *testing.T) {
	disp := dispatch.NewInProc(8)
	runner := &orderingRunner{disp: disp, ran: make(chan struct{}, 1)}
	p := New(disp, runner, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// A credentialed monitor with no envelope and no keyring: the gate refuses it before any probe.
	job := ledgerJob()
	job.Monitor.Type = domain.MonitorPostgres
	job.Monitor.Target = "db:5432"
	if err := disp.PublishJob(ctx, job); err != nil {
		t.Fatalf("publish job: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case hb := <-disp.Results():
			if hb.Claim != nil {
				t.Fatalf("a job the gate refused was CLAIMED: %+v", hb)
			}
			if hb.ProbeError != nil {
				return // the refusal reported itself, and no claim preceded it
			}
		case <-deadline:
			t.Fatal("the refused job produced neither a probe error nor a claim")
		}
	}
}
