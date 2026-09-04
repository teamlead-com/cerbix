package dispatch_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 invariant 10g, the AMQP half, against a LIVE broker.
//
// The mapping test beside this one proves the queue NAMES differ. That is not the invariant: a
// name is a fact about a function, and 10g is a fact about a wire. What must hold is that an
// executor of an older generation CANNOT RECEIVE a generation-4 delivery — because it is not
// subscribed to that queue, not because a capability check refused it after the fact.
//
// Opt-in on CERBIX_TEST_RABBITMQ_URL, exactly as `amqp_test.go` is, so `go test ./...` stays
// hermetic where no broker exists.
func TestAV3ConsumerCannotReceiveAGenerationFourDelivery(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the live generation-4 isolation test")
	}
	const region = "ledgerbarrier"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// The old executor: capable of envelope v2, so it binds v1, v2 and v3 — and nothing else.
	// It declares NO ledger capability, which is the whole point.
	v3only, err := dispatch.NewAMQP(url, logger)
	if err != nil {
		t.Fatalf("connect v3 consumer: %v", err)
	}
	v3only.WithJobRegion(region).WithCredentialCapability(dispatch.EnvelopeV2)
	t.Cleanup(func() {
		_ = v3only.Close()
		for _, q := range []string{
			"checks.jobs." + region, "checks.jobs.v2." + region,
			"checks.jobs.v3." + region, "checks.jobs.v4." + region,
		} {
			deleteTestQueue(t, url, q)
		}
	})
	oldJobs := v3only.Jobs()

	publisher, err := dispatch.NewAMQP(url, logger)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	v4Job := dispatch.CheckJob{
		Monitor:         domain.Monitor{ID: "00000000-0000-4000-8000-0000000000f4", Region: region},
		ProtocolVersion: dispatch.ProtocolV4,
	}
	if err := publisher.PublishJob(ctx, v4Job); err != nil {
		t.Fatalf("publish generation-4 job: %v", err)
	}

	// Bounded wait. The v3 consumer must stay silent: it is not subscribed to the v4 queue, so
	// there is no delivery for it to decline.
	select {
	case got := <-oldJobs:
		t.Fatalf("a generation-4 delivery reached a v3 consumer (monitor %s, carrier %d). Carrier "+
			"isolation is physical or it is nothing: a capability check does not stop a consumer "+
			"from consuming", got.Job.Monitor.ID, got.CarrierGeneration)
	case <-time.After(3 * time.Second):
	}

	// The converse, without which an unroutable message or a broken publisher would pass the
	// assertion above: an executor that DOES declare the ledger capability binds the v4 queue and
	// receives the next one.
	v4able, err := dispatch.NewAMQP(url, logger)
	if err != nil {
		t.Fatalf("connect v4 consumer: %v", err)
	}
	v4able.WithJobRegion(region).WithCredentialCapability(dispatch.EnvelopeV2).WithLedgerCapability(1)
	t.Cleanup(func() { _ = v4able.Close() })
	newJobs := v4able.Jobs()

	want := "00000000-0000-4000-8000-0000000000f5"
	second := dispatch.CheckJob{
		Monitor:         domain.Monitor{ID: want, Region: region},
		ProtocolVersion: dispatch.ProtocolV4,
	}
	if err := publisher.PublishJob(ctx, second); err != nil {
		t.Fatalf("publish second generation-4 job: %v", err)
	}
	if got := awaitJob(t, newJobs, want); got != want {
		t.Fatalf("a ledger-capable consumer did not receive the generation-4 job: got %q", got)
	}

	// And the old consumer STILL has nothing, with both bound at once — which is the assertion
	// that distinguishes isolation from a race won by whoever subscribed first.
	select {
	case got := <-oldJobs:
		t.Fatalf("the v3 consumer received %s once a v4 consumer existed; the two queues are not "+
			"separate", got.Job.Monitor.ID)
	case <-time.After(time.Second):
	}
}
