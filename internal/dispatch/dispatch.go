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
	Monitor            domain.Monitor      `json:"monitor"`
	ProtocolVersion    int                 `json:"protocol_version,omitempty"`
	CredentialEnvelope *CredentialEnvelope `json:"credential_envelope,omitempty"`
}

// DeliveredJob is a CheckJob together with the carrier generation it actually arrived on.
//
// The generation is stamped by the TRANSPORT ADAPTER — the queue an AMQP message was
// consumed from, or the generation the server selected for a claimed pull row — and is
// never read from the payload's own `ProtocolVersion`, which is body content an attacker
// can edit. Treating the carrier as normative while sourcing it from the payload would
// make the structural gate self-referential (func-secret-inventory §4.7, D-0160). The
// separate type is the point: it cannot be confused with the job body at a call site.
type DeliveredJob struct {
	Job               CheckJob
	CarrierGeneration int
}

// Dispatcher moves jobs and results between components.
type Dispatcher interface {
	PublishJob(ctx context.Context, job CheckJob) error
	Jobs() <-chan DeliveredJob
	PublishResult(ctx context.Context, hb domain.Heartbeat) error
	Results() <-chan domain.Heartbeat
	Close() error
}
