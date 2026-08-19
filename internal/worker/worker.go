// Package worker runs a pool of stateless check executors. Each worker pulls a
// CheckJob from the dispatcher, runs the prober, and publishes the resulting
// heartbeat back through the dispatcher.
package worker

import (
	"context"
	"log/slog"
	"sync"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// Runner executes a single monitor check.
type Runner interface {
	Run(ctx context.Context, m domain.Monitor) domain.Heartbeat
}

type CredentialReadiness interface {
	SetCredentialReady(ready bool, reason string)
	RecordExecutorProbeError(reason string)
}

// Pool is a fixed-size worker pool.
type Pool struct {
	// credentialTracker is shared with this process's test-RPC callback so both paths
	// answer "is this executor credential-ready" with one streak, not two.
	credentialTracker   *dispatch.CredentialHealth
	dispatcher          dispatch.Dispatcher
	runner              Runner
	logger              *slog.Logger
	size                int
	credentials         *dispatch.CredentialKeyring
	credentialReadiness CredentialReadiness
}

func (p *Pool) WithCredentialKeyring(ring *dispatch.CredentialKeyring) *Pool {
	p.credentials = ring
	return p
}

func (p *Pool) WithCredentialReadiness(sink CredentialReadiness) *Pool {
	p.credentialReadiness = sink
	if sink != nil {
		reason := ""
		if p.credentials == nil {
			reason = domain.ProbeErrorNoDispatchKey
		}
		sink.SetCredentialReady(p.credentials != nil, reason)
	}
	return p
}

// New builds a worker pool of the given size (minimum 1).
func New(dispatcher dispatch.Dispatcher, runner Runner, size int, logger *slog.Logger) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{dispatcher: dispatcher, runner: runner, logger: logger, size: size, credentialTracker: &dispatch.CredentialHealth{}}
}

// WithCredentialHealthTracker shares one failure streak with this process's test-RPC
// callback: a test and a scheduled job failing for the same reason are one executor being
// unhealthy, not two independent signals.
func (p *Pool) WithCredentialHealthTracker(t *dispatch.CredentialHealth) *Pool {
	if t != nil {
		p.credentialTracker = t
	}
	return p
}

// Run starts the workers and blocks until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(p.size)
	for i := 0; i < p.size; i++ {
		go func() {
			defer wg.Done()
			p.loop(ctx)
		}()
	}
	wg.Wait()
}

func (p *Pool) loop(ctx context.Context) {
	jobs := p.dispatcher.Jobs()
	for {
		select {
		case <-ctx.Done():
			return
		case delivered, ok := <-jobs:
			if !ok {
				return
			}
			// Every job crosses the gate, unconditionally: whether a credential is
			// REQUIRED is decided by the schema, never by whether the payload happens to
			// carry an envelope (dispatch.ValidateAndMaterialize, §4.7/D-0160).
			job := delivered.Job
			materialized, err := dispatch.ValidateAndMaterialize(p.credentials, delivered)
			if err != nil {
				reason := dispatch.CredentialProbeErrorReason(err)
				p.logger.Error("credential_job_rejected", "monitor_id", job.Monitor.ID, "reason", reason)
				p.publishProbeError(ctx, job, reason)
				// Persistent, not first (dispatch.CredentialHealth): a single corrupt or
				// retired-key payload is a per-job diagnostic, and readiness routes work to
				// the whole worker.
				if p.credentialTracker.Failure(reason) {
					p.setCredentialReady(false, reason)
				}
				continue
			}
			if materialized.UsedCredential {
				p.credentialTracker.Success()
				p.setCredentialReady(true, "")
			}
			// The result carries the job it answers (func-result-protocol §9): the core stamped the
			// id and the issue instant from its own clock, and copying them here is what lets the
			// core compare `observed_at` against `job_issued_at` instead of against itself.
			hb := dispatch.StampResult(p.runner.Run(ctx, materialized.Monitor), job)
			materialized.Cleanup()
			if err := p.dispatcher.PublishResult(ctx, hb); err != nil {
				if ctx.Err() != nil {
					return
				}
				p.logger.Error("publish_result_failed", "monitor_id", job.Monitor.ID, "error", err.Error())
			}
		}
	}
}

func (p *Pool) setCredentialReady(ready bool, reason string) {
	if p.credentialReadiness != nil {
		p.credentialReadiness.SetCredentialReady(ready, reason)
	}
}

func (p *Pool) publishProbeError(ctx context.Context, job dispatch.CheckJob, reason string) {
	if p.credentialReadiness != nil {
		p.credentialReadiness.RecordExecutorProbeError(reason)
	}
	if err := p.dispatcher.PublishResult(ctx, dispatch.ProbeErrorHeartbeat(job, reason)); err != nil && ctx.Err() == nil {
		p.logger.Error("publish_probe_error_failed", "monitor_id", job.Monitor.ID, "reason", reason, "error", err.Error())
	}
}
