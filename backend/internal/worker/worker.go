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

// Pool is a fixed-size worker pool.
type Pool struct {
	dispatcher dispatch.Dispatcher
	runner     Runner
	logger     *slog.Logger
	size       int
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
			hb := p.runner.Run(ctx, job.Monitor)
			if err := p.dispatcher.PublishResult(ctx, hb); err != nil {
				if ctx.Err() != nil {
					return
				}
				p.logger.Error("publish_result_failed", "monitor_id", job.Monitor.ID, "error", err.Error())
			}
		}
	}
}
