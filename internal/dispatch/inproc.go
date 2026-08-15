package dispatch

import (
	"context"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// InProc is an in-process Dispatcher backed by buffered channels. It is valid
// only within a single process (the --role=all dev/test mode). Lifecycle is
// context-driven; Close is a no-op so concurrent producers never send on a
// closed channel.
type InProc struct {
	jobs    chan DeliveredJob
	results chan domain.Heartbeat
}

// NewInProc creates an in-process dispatcher with the given channel buffer size.
func NewInProc(buffer int) *InProc {
	if buffer <= 0 {
		buffer = 256
	}
	return &InProc{
		jobs:    make(chan DeliveredJob, buffer),
		results: make(chan domain.Heartbeat, buffer),
	}
}

func (d *InProc) PublishJob(ctx context.Context, job CheckJob) error {
	// In-process there is no wire between publisher and consumer, so the publisher's own
	// protocol version IS the carrier generation: there is no untrusted hop that could
	// have rewritten it.
	select {
	case d.jobs <- DeliveredJob{Job: job, CarrierGeneration: job.ProtocolVersion}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *InProc) Jobs() <-chan DeliveredJob { return d.jobs }

func (d *InProc) PublishResult(ctx context.Context, hb domain.Heartbeat) error {
	select {
	case d.results <- hb:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *InProc) Results() <-chan domain.Heartbeat { return d.results }

func (d *InProc) Close() error { return nil }
