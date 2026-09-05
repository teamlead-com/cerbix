package dispatch_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

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

	// The identity is on the fixture because B2 made it MANDATORY: a generation-4 delivery
	// missing `DueAt` is a protocol violation and its consumer dead-letters it, so a fixture
	// without it stopped being a v4 job and started being the poison message the test below is
	// about. B1's fixture carried none — the field did not exist — and adding it here is the
	// cross-phase consequence of 10i becoming enforceable, not a change to what 10g asserts.
	v4Job := dispatch.CheckJob{
		Monitor:         domain.Monitor{ID: "00000000-0000-4000-8000-0000000000f4", Region: region},
		ProtocolVersion: dispatch.ProtocolV4,
		JobID:           "11111111-1111-4111-8111-1111111111f4",
		IssuedAt:        time.Now().UTC(),
		DueAt:           time.Now().UTC().Add(-time.Minute),
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
		JobID:           "11111111-1111-4111-8111-1111111111f5",
		IssuedAt:        time.Now().UTC(),
		DueAt:           time.Now().UTC().Add(-time.Minute),
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

// FR-032 invariant 10i, against a LIVE broker — a generation-4 delivery missing its identity is
// DEAD-LETTERED, not probed and not dropped.
//
// §17.4 reserved this for B2's gate and named both mutations before B1 shipped, so the analysis is
// inherited rather than re-derived:
//
//   - Treat the missing field as a rolling-upgrade case and probe anyway. That is the plausible
//     mistake, because the code already tolerates absent identity on OLDER carriers and the
//     tolerant branch is one `if` away from covering generation 4 too. The test must therefore
//     distinguish CARRIER from payload, which is why the same body is published to the v1 queue
//     below and must arrive there normally.
//   - Drop the delivery silently instead of dead-lettering. That passes any assertion which only
//     checks "no probe ran", which is exactly why the dead-letter ARRIVAL is asserted separately.
//
// A B2 core never publishes such a job — it stamps the identity in the same statement that selects
// the carrier — so the publish here is hand-crafted, and that is what the case is: a poison
// message, from a corrupted body or a B1-era binary with the gate forced on. A dead-letter queue
// exists for exactly the deliveries production cannot make.
func TestAGenerationFourDeliveryMissingItsIdentityIsDeadLettered(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the live generation-4 identity test")
	}
	const region = "ledgeridentity"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// A ledger-capable executor: it binds the v4 queue, so a delivery there is one it CAN receive
	// and chooses to refuse. That is the distinction being tested — refusal, not unreachability,
	// which 10g already covers.
	consumer, err := dispatch.NewAMQP(url, logger)
	if err != nil {
		t.Fatalf("connect ledger consumer: %v", err)
	}
	consumer.WithJobRegion(region).WithCredentialCapability(dispatch.EnvelopeV2).WithLedgerCapability(1)
	t.Cleanup(func() {
		_ = consumer.Close()
		for _, q := range []string{
			"checks.jobs." + region, "checks.jobs.v2." + region,
			"checks.jobs.v3." + region, "checks.jobs.v4." + region,
		} {
			deleteTestQueue(t, url, q)
		}
	})
	jobs := consumer.Jobs()

	publisher, err := dispatch.NewAMQP(url, logger)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	// Drain whatever an earlier run left in the shared dead-letter queue, so the arrival assertion
	// below is about THIS delivery. `checks.dead` has no consumer in production — it exists so a
	// poison body survives for inspection — which is what makes draining it safe here.
	drainDeadLetters(t, url)

	const poisoned = "00000000-0000-4000-8000-00000000d0ea"
	// DueAt is absent: `omitempty` drops the zero value, so the body reaching the v4 queue does
	// not carry the field that DEFINES the generation it rode.
	missing := dispatch.CheckJob{
		Monitor:         domain.Monitor{ID: poisoned, Region: region},
		ProtocolVersion: dispatch.ProtocolV4,
		JobID:           "11111111-1111-4111-8111-111111111111",
		IssuedAt:        time.Now().UTC(),
	}
	if err := publisher.PublishJob(ctx, missing); err != nil {
		t.Fatalf("publish a generation-4 job with no window: %v", err)
	}

	// It must never reach the executor.
	select {
	case got := <-jobs:
		if got.Job.Monitor.ID == poisoned {
			t.Fatalf("a generation-4 delivery with no window was handed to the executor on carrier "+
				"%d. The absence is ordinary on an OLDER carrier and a protocol violation on this "+
				"one, and the tolerant branch is one `if` away from covering both",
				got.CarrierGeneration)
		}
	case <-time.After(3 * time.Second):
	}

	// And it must have SURVIVED, in the durable queue, tagged with the carrier it came from. A
	// silent drop passes the assertion above and fails this one, which is the whole reason the two
	// are separate.
	body, source := awaitDeadLetter(t, url, poisoned)
	if body == "" {
		t.Fatal("the poisoned delivery was dropped silently instead of dead-lettered: nothing " +
			"survives for an operator to inspect, and the assertion that no probe ran cannot tell " +
			"the two apart")
	}
	if source != "jobs.v4" {
		t.Errorf("the dead-lettered body is tagged source %q, want \"jobs.v4\" — a dead letter that "+
			"does not say which carrier it came from sends its reader after the wrong bug", source)
	}

	// The carrier decides, not the payload: the SAME body on the v1 queue is the ordinary
	// rolling-upgrade case and is delivered normally, its window simply not ledger-eligible.
	onOldCarrier := missing
	onOldCarrier.Monitor.ID = "00000000-0000-4000-8000-00000000d0eb"
	onOldCarrier.ProtocolVersion = dispatch.ProtocolV1
	if err := publisher.PublishJob(ctx, onOldCarrier); err != nil {
		t.Fatalf("publish the same body on generation 1: %v", err)
	}
	delivered := awaitDelivered(t, jobs, onOldCarrier.Monitor.ID)
	if delivered.CarrierGeneration != dispatch.ProtocolV1 {
		t.Errorf("the ordinary delivery arrived on carrier %d, want 1", delivered.CarrierGeneration)
	}
	if !delivered.Job.DueAt.IsZero() {
		t.Errorf("the ordinary delivery carries a window it never had: %s", delivered.Job.DueAt)
	}
}

// drainDeadLetters empties the shared dead-letter queue.
func drainDeadLetters(t *testing.T, url string) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial to drain dead letters: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open drain channel: %v", err)
	}
	defer func() { _ = ch.Close() }()
	if _, err := ch.QueueDeclare("checks.dead", true, false, false, false, nil); err != nil {
		t.Fatalf("declare checks.dead: %v", err)
	}
	if _, err := ch.QueuePurge("checks.dead", false); err != nil {
		t.Fatalf("purge checks.dead: %v", err)
	}
}

// awaitDeadLetter waits for a body containing want and returns it with its source tag.
func awaitDeadLetter(t *testing.T, url, want string) (string, string) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial to read dead letters: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open dead-letter channel: %v", err)
	}
	defer func() { _ = ch.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok, err := ch.Get("checks.dead", true)
		if err != nil {
			t.Fatalf("read checks.dead: %v", err)
		}
		if !ok {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if strings.Contains(string(msg.Body), want) {
			source, _ := msg.Headers["x-cerbix-source"].(string)
			return string(msg.Body), source
		}
	}
	return "", ""
}
