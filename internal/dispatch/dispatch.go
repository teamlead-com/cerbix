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
	JobID    string    `json:"job_id,omitempty"`
	IssuedAt time.Time `json:"issued_at,omitempty"`
	// DueAt is the WINDOW this dispatch answers, and it is what carrier generation 4 exists to
	// carry (FR-032 §13.1). All three transports carry CheckJob verbatim — AMQP as the JSON body,
	// pull as the JSON payload of a `pull_jobs` row, in-process as the struct itself — so a new
	// field propagates by construction, which is the reason no per-transport work is needed for
	// it. An executor older than generation 4 drops the unknown field and the result returns with
	// DueAt zero, treated as NO correlation.
	//
	// A generation-4 delivery MISSING it is a protocol violation rather than a rolling-upgrade
	// case, and is dead-lettered rather than probed (invariant 10i). See RequireLedgerFields.
	DueAt              time.Time           `json:"due_at,omitempty"`
	CredentialEnvelope *CredentialEnvelope `json:"credential_envelope,omitempty"`
}

// RequireLedgerFields reports whether a delivery on the given carrier must carry job identity.
//
// It takes the CARRIER and not the payload, and that distinction is invariant 10i's whole point:
// the tolerant branch that accepts absent identity on an older generation is one `if` away from
// covering generation 4 too, and the payload's own `ProtocolVersion` is body content an executor
// must not trust for this decision. `DeliveredJob.CarrierGeneration` is the queue a message was
// consumed from or the generation the server stamped on a claimed row — a fact about the wire.
func RequireLedgerFields(carrierGeneration int) bool { return carrierGeneration >= ProtocolV4 }

// LedgerFieldsMissing names the identity field a generation-4 delivery lacks, or "" when it
// carries all three. It returns the NAME so a dead-letter is diagnosable: "a v4 job was rejected"
// sends the reader after the wrong bug.
func (j CheckJob) LedgerFieldsMissing() string {
	switch {
	case j.JobID == "":
		return "job_id"
	case j.IssuedAt.IsZero():
		return "issued_at"
	case j.DueAt.IsZero():
		return "due_at"
	default:
		return ""
	}
}

// CarrierFor picks the carrier generation a job rides, and it is the ONE owner of that decision.
//
// Three paths choose a carrier — the scheduler's plain branch, the store's credential
// materialization, and the pull enqueue that follows either — and the generations are NOT a
// straight ladder a `min` can walk. Writing the rule out at each site is how the publisher came to
// refuse a generation-4 job with no envelope in B1's first draft: each site was right about the
// case in front of it.
//
// The rules, in the order they apply:
//
//  1. An async canary rides its OWN capability-named queue and there is no generation-4 canary
//     carrier in this phase, so it is clamped to 3 first. Its windows therefore read `unknown`,
//     which is honest — a stated limitation rather than a silent one (spec §18).
//  2. Generation 4 is DEFINED by carrying the window a run answers. A job with no window must not
//     ride it, because a v4 consumer is entitled to dead-letter a delivery missing its defining
//     field (invariant 10i).
//  3. Generations 2 and 3 EXIST to carry a credential envelope, and the publisher refuses one
//     without it. So a job with an envelope rides the highest of them its region proved, and a job
//     WITHOUT one rides generation 1 — those two are the only carriers a secretless monitor has
//     ever had, and generation 4 is the third. That is not a gap in the numbering: it is what "one
//     generation per capability" means for a monitor that needs one capability and not the other.
func CarrierFor(regionGeneration int, hasEnvelope, hasWindow bool, monitorType domain.MonitorType) int {
	if monitorType == domain.MonitorAsyncCanary && regionGeneration > ProtocolV3 {
		regionGeneration = ProtocolV3
	}
	if hasWindow && regionGeneration >= ProtocolV4 {
		return ProtocolV4
	}
	if !hasEnvelope {
		return ProtocolV1
	}
	if regionGeneration > ProtocolV3 {
		return ProtocolV3
	}
	if regionGeneration < ProtocolV2 {
		return ProtocolV2
	}
	return regionGeneration
}

// StampResult copies a job's identity onto the result that answers it. One owner, because three
// executors publish results (the AMQP worker pool, the pull agent's batch, and the probe-error path)
// and a stamp that each of them applied separately would drift the first time one of them was edited.
func StampResult(hb domain.Heartbeat, job CheckJob) domain.Heartbeat {
	hb.JobID = job.JobID
	hb.JobIssuedAt = job.IssuedAt
	// The window this result answers travels with the identity, from the same single owner. A
	// second copier is how "and its result twin" became a specification nobody implemented —
	// invariant 25f exists because that phrase was accepted once.
	hb.DueAt = job.DueAt
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
