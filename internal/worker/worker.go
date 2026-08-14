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
	return &Pool{dispatcher: dispatcher, runner: runner, logger: logger, size: size}
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
		case job, ok := <-jobs:
			if !ok {
				return
			}
			monitor := job.Monitor
			cleanup := func() {}
			if job.CredentialEnvelope != nil {
				if p.credentials == nil {
					p.logger.Error("credential_job_rejected", "monitor_id", job.Monitor.ID, "reason", "no_dispatch_key")
					p.publishProbeError(ctx, job, domain.ProbeErrorNoDispatchKey)
					p.setCredentialReady(false, domain.ProbeErrorNoDispatchKey)
					continue
				}
				var err error
				monitor, cleanup, err = p.credentials.MaterializeForProbe(job)
				if err != nil {
					reason := dispatch.CredentialProbeErrorReason(err)
					p.logger.Error("credential_job_rejected", "monitor_id", job.Monitor.ID, "reason", reason)
					p.publishProbeError(ctx, job, reason)
					if reason != domain.ProbeErrorUnsupportedVersion {
						p.setCredentialReady(false, reason)
					}
					continue
				}
				p.setCredentialReady(true, "")
			}
			hb := p.runner.Run(ctx, monitor)
			cleanup()
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
