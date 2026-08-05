// Package dispatch is the transport seam between the scheduler and workers. The
// scheduler publishes CheckJobs and consumes nothing; workers consume jobs and
// publish result Heartbeats; the ingestion consumer reads results. The
// production implementation is RabbitMQ (added later); inproc backs local dev
// and tests without a broker.
package dispatch

import (
	"context"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// CheckJob carries a monitor snapshot to execute. The snapshot is taken at
// publish time so workers need no database access.
type CheckJob struct {
	Monitor domain.Monitor
}

// Dispatcher moves jobs and results between components.
type Dispatcher interface {
	PublishJob(ctx context.Context, job CheckJob) error
	Jobs() <-chan CheckJob
	PublishResult(ctx context.Context, hb domain.Heartbeat) error
	Results() <-chan domain.Heartbeat
	Close() error
}
