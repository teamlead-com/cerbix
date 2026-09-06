package dispatch_test

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestEveryTestCarrierAnswersOnItsOwnQueue publishes one Test Connection on EVERY test
// carrier this executor serves and requires each to come back.
//
// It exists because generation 3 did not. `serveEnvelopeTestsOnce` serves both the v2 and
// the v3 test queue, and its admission check compared the delivery's own ProtocolVersion
// against a hardcoded ProtocolV2 — so a generation-3 Test Connection was dead-lettered by
// the consumer bound to its own queue. The caller is answered only on success, so it waited
// out its timeout and reported `no worker responded in region "core"`: the same message an
// operator gets when the region is genuinely empty.
//
// Nothing caught it. The v2 carrier had a test and passed; the v3 carrier had none; the
// inproc dev stack never takes this path at all; and `make dev-test-distributed`, the one
// gate that does, was red for an unrelated wiring reason that masked it.
//
// So the assertion is over the SET of carriers, not over the one that broke:
// TestTheTestCarrierTableCoversEveryServeEntryPoint below fails if a fourth is added and
// left out of this table.
func TestEveryTestCarrierAnswersOnItsOwnQueue(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the AMQP test-carrier test")
	}
	const region = "testcarrier"
	d, err := dispatch.NewAMQP(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = d.Close()
		deleteTestQueue(t, url, "checks.tests."+region)
		deleteTestQueue(t, url, "checks.tests.v2."+region)
		deleteTestQueue(t, url, "checks.tests.v3."+region)
	})
	d.WithJobRegion(region).WithCredentialCapability(dispatch.EnvelopeV2)
	ctx := context.Background()

	keyring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{
		ID:  "testcarrier-key",
		Key: bytes.Repeat([]byte{0x33}, 32),
	}, nil)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}

	// ONE runner behind every carrier, exactly as `cli.go` wires it. It reports the
	// generation the CONSUMER stamped, which is the property under test: a reply proves the
	// delivery was admitted, and the stamp proves it was admitted by the right consumer.
	runner := func(_ context.Context, delivered dispatch.DeliveredJob) (domain.Heartbeat, error) {
		return domain.Heartbeat{
			MonitorID: delivered.Job.Monitor.ID,
			Up:        true,
			Code:      200 + delivered.CarrierGeneration,
		}, nil
	}
	if err := d.ServeTests(runner); err != nil {
		t.Fatalf("start v1 test consumer: %v", err)
	}
	if err := d.ServeTestsV2(runner); err != nil {
		t.Fatalf("start v2 test consumer: %v", err)
	}
	if err := d.ServeTestsV3(runner); err != nil {
		t.Fatalf("start v3 test consumer: %v", err)
	}

	for _, tc := range testCarrierTable {
		t.Run(tc.name, func(t *testing.T) {
			// A real type and target, not a zero monitor: from envelope v2 the AAD binds a
			// digest of the execution body, and sealing consults the type's credential
			// settings schema. A typeless monitor cannot be sealed at all, which is a
			// property of the binding rather than of this test.
			m := domain.Monitor{
				ID: tc.monitorID, Region: region, Type: "postgres", Target: "postgres:5432",
				ExecutionRevision: 7, TimeoutSeconds: 2,
			}
			job := dispatch.CheckJob{Monitor: m, ProtocolVersion: tc.generation}
			if tc.envelopeVersion != 0 {
				env, sealErr := keyring.Seal(dispatch.SealContext{
					EnvelopeVersion: tc.envelopeVersion,
					Region:          region,
					JobID:           "test-" + tc.name,
					MonitorID:       m.ID,
					Revision:        m.ExecutionRevision,
					Body:            m,
				}, map[string][]byte{"password": []byte("carrier-secret")})
				if sealErr != nil {
					t.Fatalf("seal: %v", sealErr)
				}
				job.CredentialEnvelope = env
			}
			hb, err := d.RunJobTest(ctx, job)
			if err != nil {
				t.Fatalf("test RPC on carrier %d: %v (a timeout here means the consumer bound to this queue refused the delivery)", tc.generation, err)
			}
			if !hb.Up || hb.Code != 200+tc.generation {
				t.Fatalf("carrier %d answered %+v, want up with code %d — a reply stamped with another generation means the wrong consumer took it", tc.generation, hb, 200+tc.generation)
			}
		})
	}
}

// TestAMismatchedEnvelopeIsDeadLetteredOnBothAMQPPaths proves the carrier->envelope rule where the
// TRANSPORT enforces it, not only at the materializer gate: a generation-1 envelope published onto
// a generation-3 queue must never be handed to a runner, on the tests path or the jobs path.
//
// The unit test `TestADowngradedEnvelopeNeverReachesTheRunner` covers the gate every executor
// crosses. This covers the two consumers that dispose of a poison body themselves — one predicate,
// two dispositions, the same split `RequireLedgerFields` already uses.
func TestAMismatchedEnvelopeIsDeadLetteredOnBothAMQPPaths(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the AMQP carrier-mismatch test")
	}
	// A run TOKEN, not a constant. Two of this test running at once on one broker would
	// otherwise share a region, share queue names, and — worse — each could take the other's
	// dead letter as its own proof. Reviewer P1 on the WIP.
	token := runToken(t)
	region := "carriermismatch-" + token
	mismatchJobID := "job-mismatch-" + token
	d, err := dispatch.NewAMQP(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = d.Close()
		for _, q := range []string{
			"checks.tests.v3." + region, "checks.jobs.v3." + region,
			"checks.jobs.v2." + region, "checks.jobs.v4." + region, "checks.jobs." + region,
		} {
			deleteTestQueue(t, url, q)
		}
	})
	d.WithJobRegion(region).WithCredentialCapability(dispatch.EnvelopeV2)
	ctx := context.Background()

	keyring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{
		ID: "carriermismatch-key", Key: bytes.Repeat([]byte{0x55}, 32),
	}, nil)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	m := domain.Monitor{
		ID: "00000000-0000-4000-8000-0000000001b1", Region: region, Type: "postgres",
		Target: "postgres:5432", ExecutionRevision: 9, TimeoutSeconds: 1,
	}
	// Envelope v1 — legitimately sealed, and the WRONG generation for a v3 carrier.
	downgraded, err := keyring.Seal(dispatch.SealContext{
		EnvelopeVersion: dispatch.EnvelopeV1, Region: region, JobID: "job-mismatch",
		MonitorID: m.ID, Revision: m.ExecutionRevision, Body: m,
	}, map[string][]byte{"password": []byte("carrier-secret")})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	downgraded.JobID = mismatchJobID

	// The dead letters this run produces are identified by a token unique to THIS run, so a
	// message from another run or another test cannot be mistaken for one of ours and ours
	// cannot be mistaken for silence.
	dead := consumeDeadLetters(t, url)

	// ── the tests path ──────────────────────────────────────────────────────────────────────
	var ran atomic.Bool
	if err := d.ServeTestsV3(func(_ context.Context, _ dispatch.DeliveredJob) (domain.Heartbeat, error) {
		ran.Store(true)
		return domain.Heartbeat{Up: true, Code: 200}, nil
	}); err != nil {
		t.Fatalf("start v3 test consumer: %v", err)
	}
	refusedAt := time.Now()
	_, err = d.RunJobTest(ctx, dispatch.CheckJob{
		Monitor: m, ProtocolVersion: dispatch.ProtocolV3, CredentialEnvelope: downgraded,
	})
	if err == nil {
		t.Fatal("the v3 test carrier ANSWERED a generation-1 envelope — it reached the runner")
	}
	// A REFUSAL IS ANSWERED, not waited out. It used to be silence: the consumer dead-lettered the
	// body and published nothing, so this call spent its whole RPC timeout and came back with
	// `no worker responded in region …` — the sentence an EMPTY region gives. An operator could
	// not tell a refused delivery from a region with no worker in it at all, and that cost this
	// arc a debugging cycle. Owner's decision (2026-09-06) on the item the reviewer fenced.
	if elapsed := time.Since(refusedAt); elapsed > time.Second {
		t.Fatalf("the refusal took %s — that is the RPC timeout being waited out, not an answer", elapsed)
	}
	if strings.Contains(err.Error(), "no worker responded") {
		t.Fatalf("a refused delivery reported %q, which is what an EMPTY region reports", err)
	}
	// The exact REASON, not merely the shape: a prefix check passes for an empty reason, and an
	// empty reason is a typed error carrying no information. A known envelope on the wrong carrier
	// stays in the non-oracular bucket — the executor never tells a prober which way its forgery
	// was wrong — and that is the same answer the unit test pins at the gate, so this proves the
	// WIRE carries it too rather than that something arrived.
	if want := "probe_error: " + domain.ProbeErrorDecryptAuthFailed; err.Error() != want {
		t.Fatalf("a refused delivery reported %q, want %q", err, want)
	}
	if ran.Load() {
		t.Fatal("the test runner was invoked with a generation-1 envelope on a generation-3 carrier")
	}
	// The runner not being called is only HALF of "one predicate, two dispositions": with the
	// consumer's own check removed, the materializer gate would still stop the probe, and the
	// difference would be invisible here. So the poison body must ALSO be durable, tagged with the
	// carrier it was dropped from — which is what an operator reads when a Test Connection times
	// out (see `docs/runbook.md`).
	if src := dead.await(t, mismatchJobID); src != "tests.v3" {
		t.Fatalf("the mismatched TEST delivery was dead-lettered as %q, want \"tests.v3\" — the poison body must survive for inspection under the carrier that dropped it", src)
	}

	// ── the jobs path ───────────────────────────────────────────────────────────────────────
	jobs := d.Jobs()
	if err := d.PublishJob(ctx, dispatch.CheckJob{
		Monitor: m, ProtocolVersion: dispatch.ProtocolV3, CredentialEnvelope: downgraded,
	}); err != nil {
		t.Fatalf("publish mismatched job: %v", err)
	}
	// A well-formed job on the same carrier IS delivered, so this is a wait for a real event
	// rather than a bet on a timeout: if the mismatched one were admitted it would arrive first.
	wellFormed, err := keyring.Seal(dispatch.SealContext{
		EnvelopeVersion: dispatch.EnvelopeV2, Region: region, JobID: "job-ok",
		MonitorID: m.ID, Revision: m.ExecutionRevision, Body: m,
	}, map[string][]byte{"password": []byte("carrier-secret")})
	if err != nil {
		t.Fatalf("seal well-formed: %v", err)
	}
	if err := d.PublishJob(ctx, dispatch.CheckJob{
		Monitor: m, ProtocolVersion: dispatch.ProtocolV3, CredentialEnvelope: wellFormed,
	}); err != nil {
		t.Fatalf("publish well-formed job: %v", err)
	}
	select {
	case delivered := <-jobs:
		if got := delivered.Job.CredentialEnvelope; got == nil || got.V != dispatch.EnvelopeV2 {
			t.Fatalf("the first job delivered carried envelope %+v, want v2 — the downgraded one was admitted", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no job arrived at all; the jobs consumer is not running and this test proves nothing")
	}
	if src := dead.await(t, mismatchJobID); src != "jobs.v3" {
		t.Fatalf("the mismatched JOB was dead-lettered as %q, want \"jobs.v3\"", src)
	}
}

// deadLetterTap consumes the shared `checks.dead` queue for the duration of a test and hands back
// the `x-cerbix-source` of the first message whose body names a given job id. Messages from other
// runs are read and ignored rather than matched, which is why the caller uses a unique id.
type deadLetterTap struct {
	msgs <-chan amqp.Delivery
	seen map[string]deadLetter
}

type deadLetter struct {
	body string
	ack  func() error
}

// consumeDeadLetters reads `checks.dead` WITHOUT consuming what it does not own.
//
// The first version used `autoAck`, which permanently ate every operational dead letter that
// happened to be on the shared queue — a test destroying the very evidence the runbook tells an
// operator to read (reviewer P1 on the WIP). Acknowledgement is manual now: a delivery carrying
// this run's token is acked, because it is this test's own litter; everything else is left
// UNACKED and is requeued by the broker when the channel closes at cleanup. Unacked deliveries
// are not redelivered while this consumer holds them, so there is no hot loop either.
func consumeDeadLetters(t *testing.T, url string) *deadLetterTap {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial for dead letters: %v", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("dead-letter channel: %v", err)
	}
	if _, err := ch.QueueDeclare("checks.dead", true, false, false, false, nil); err != nil {
		t.Fatalf("declare checks.dead: %v", err)
	}
	msgs, err := ch.Consume("checks.dead", "", false, false, false, false, nil)
	if err != nil {
		t.Fatalf("consume checks.dead: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close(); _ = conn.Close() })
	return &deadLetterTap{msgs: msgs, seen: map[string]deadLetter{}}
}

// runToken is a value no concurrent run of this test can share: the process id and a nanosecond
// clock reading, which differ across processes and across two runs in one process.
func runToken(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// await returns the source header of the dead letter carrying jobID, waiting for it to arrive.
// A previously seen one is returned from the cache, because the tests path and the jobs path both
// ask and the deliveries do not arrive in a guaranteed order.
func (d *deadLetterTap) await(t *testing.T, jobID string) string {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		for src, dl := range d.seen {
			if strings.Contains(dl.body, jobID) {
				delete(d.seen, src)
				// Ours, and only ours, is removed from the queue.
				if err := dl.ack(); err != nil {
					t.Fatalf("ack our own dead letter: %v", err)
				}
				return src
			}
		}
		select {
		case m, ok := <-d.msgs:
			if !ok {
				t.Fatal("the dead-letter consumer closed before the poison body arrived")
			}
			src, _ := m.Headers["x-cerbix-source"].(string)
			// Everything else is held UNACKED and requeued when the channel closes.
			d.seen[src] = deadLetter{body: string(m.Body), ack: func() error { return m.Ack(false) }}
		case <-deadline:
			t.Fatalf("no dead letter carrying %q arrived — the poison body did not survive", jobID)
		}
	}
}

// testCarrierTable is the enumerated set of test carriers, and the thing the source scan
// below holds the product to. `serveMethod` is the exported entry point that starts the
// consumer for this generation.
var testCarrierTable = []struct {
	name            string
	serveMethod     string
	generation      int
	envelopeVersion int // 0 = this carrier takes no credential envelope
	monitorID       string
}{
	{name: "v1_plain", serveMethod: "ServeTests", generation: dispatch.ProtocolV1, monitorID: "00000000-0000-4000-8000-0000000001a1"},
	{name: "v2_envelope_v1", serveMethod: "ServeTestsV2", generation: dispatch.ProtocolV2, envelopeVersion: dispatch.EnvelopeV1, monitorID: "00000000-0000-4000-8000-0000000001a2"},
	{name: "v3_envelope_v2", serveMethod: "ServeTestsV3", generation: dispatch.ProtocolV3, envelopeVersion: dispatch.EnvelopeV2, monitorID: "00000000-0000-4000-8000-0000000001a3"},
}

// TestTheTestCarrierTableCoversEveryServeEntryPoint reads the product source for every
// exported `ServeTests…` method on *AMQP and requires the table above to name it.
//
// The count is not the guard — the SET is. A fourth test carrier added and wired in
// `cli.go` fails this by name here, before it can repeat generation 3's history of being
// declared, consumed, and reachable by nothing. This runs with no broker.
func TestTheTestCarrierTableCoversEveryServeEntryPoint(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "amqp.go", nil, 0)
	if err != nil {
		t.Fatalf("parse amqp.go: %v", err)
	}
	var inSource []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !fn.Name.IsExported() {
			return true
		}
		if len(fn.Name.Name) < len("ServeTests") || fn.Name.Name[:len("ServeTests")] != "ServeTests" {
			return true
		}
		inSource = append(inSource, fn.Name.Name)
		return true
	})
	if len(inSource) == 0 {
		t.Fatal("found no ServeTests entry point in amqp.go — the scan is looking in the wrong place")
	}
	covered := map[string]bool{}
	for _, tc := range testCarrierTable {
		covered[tc.serveMethod] = true
	}
	var missing []string
	for _, name := range inSource {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("test carriers with no row in testCarrierTable: %v — every one of them is a queue an executor consumes and no test ever publishes to", missing)
	}
}
