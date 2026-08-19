// Package dispatch is the transport seam between the scheduler and workers. The
// scheduler publishes CheckJobs and consumes nothing; workers consume jobs and
// publish result Heartbeats; the ingestion consumer reads results. The
// production implementation is RabbitMQ (added later); inproc backs local dev
// and tests without a broker.
package dispatch

import (
	"context"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// CheckJob carries a monitor snapshot to execute. The snapshot is taken at
// publish time so workers need no database access.
type CheckJob struct {
	Monitor         domain.Monitor `json:"monitor"`
	ProtocolVersion int            `json:"protocol_version,omitempty"`
	// JobID and IssuedAt identify this dispatch. Both are stamped by the core when the job is
	// materialized — the id and the instant come from the DATABASE in one statement — and an
	// executor copies them onto the result it returns (`StampResult`). That is what makes
	// `observed_at >= job_issued_at` a comparison between an executor's clock and the core's rather
	// than between two readings of the same unknown clock. A credentialed job also carries the id
	// inside its envelope, where it is AAD; this field exists because every job needs the identity,
	// not only the ones with a secret.
	JobID              string              `json:"job_id,omitempty"`
	IssuedAt           time.Time           `json:"issued_at,omitempty"`
	CredentialEnvelope *CredentialEnvelope `json:"credential_envelope,omitempty"`
}

// StampResult copies a job's identity onto the result that answers it. One owner, because three
// executors publish results (the AMQP worker pool, the pull agent's batch, and the probe-error path)
// and a stamp that each of them applied separately would drift the first time one of them was edited.
func StampResult(hb domain.Heartbeat, job CheckJob) domain.Heartbeat {
	hb.JobID = job.JobID
	hb.JobIssuedAt = job.IssuedAt
	return hb
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
