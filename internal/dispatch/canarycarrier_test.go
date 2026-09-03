package dispatch

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 invariant 6, publisher half. The canary rides its own queue for the reason D-0160 gave for
// generation 3: a capability CHECK does not stop a consumer from consuming, so a worker that cannot
// run an async transaction must be unable to receive one. What is asserted here is the routing
// DECISION — which queue, or why none — because that is what decides who can receive the job.

func canaryCarrierMonitor(cfg map[string]string) domain.Monitor {
	if cfg == nil {
		cfg = map[string]string{}
	}
	return domain.Monitor{
		ID: "11111111-1111-1111-1111-111111111111", Type: domain.MonitorAsyncCanary,
		Region: "core", IntervalSeconds: 300, Config: cfg,
	}
}

func TestASecretlessCanaryRidesThePlainCarrier(t *testing.T) {
	job := CheckJob{Monitor: canaryCarrierMonitor(nil)}
	queue, err := canaryCarrierFor(job, "core", ProtocolV1)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	want := "checks.canary." + domain.CanaryCapabilityOfThisBinary() + ".core"
	if queue != want {
		t.Fatalf("queue = %q, want %q", queue, want)
	}
	// A worker with no envelope capability consumes exactly this queue, so `secrets.enabled: false`
	// keeps a secretless canary working — the property §4.1 promises in both directions.
	if strings.Contains(queue, canaryV3Infix) {
		t.Fatalf("a secretless canary must not be routed to the envelope carrier: %q", queue)
	}
}

func TestABoundCanaryRidesTheEnvelopeCarrierAndOnlyThat(t *testing.T) {
	cfg := map[string]string{"canary_secret_upload_ref": "s1"}
	job := CheckJob{Monitor: canaryCarrierMonitor(cfg), CredentialEnvelope: &CredentialEnvelope{V: EnvelopeV2}}
	queue, err := canaryCarrierFor(job, "core", ProtocolV3)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	want := "checks.canary." + canaryV3Infix + domain.CanaryCapabilityOfThisBinary() + ".core"
	if queue != want {
		t.Fatalf("queue = %q, want %q", queue, want)
	}
	// Generation 2 is not a carrier a canary may take: a binding needs a BODY-bound envelope, so a
	// generation-2 canary could only ever be refused by the executor's gate. Refusing at the
	// publisher keeps the failure in the log of the process that made the decision.
	if _, err := canaryCarrierFor(job, "core", ProtocolV2); err == nil {
		t.Fatal("a bound canary was routed onto a generation-2 carrier")
	}
}

// The pairing is exact in BOTH directions, which the generic "generation 2 and up must carry an
// envelope" rule cannot express: a stripped envelope must not be published, and an envelope must not
// ride the queue a capability-0 worker consumes.
func TestTheEnvelopePairingIsRefusedInBothDirections(t *testing.T) {
	bound := CheckJob{Monitor: canaryCarrierMonitor(map[string]string{"canary_secret_upload_ref": "s1"})}
	if _, err := canaryCarrierFor(bound, "core", ProtocolV3); err == nil {
		t.Fatal("a canary with a binding was published without its envelope")
	}
	loose := CheckJob{Monitor: canaryCarrierMonitor(nil), CredentialEnvelope: &CredentialEnvelope{V: EnvelopeV2}}
	if _, err := canaryCarrierFor(loose, "core", ProtocolV1); err == nil {
		t.Fatal("a canary with no binding carried an envelope onto the plain queue")
	}
}

// The queue is named for the token the DOCUMENT needs, not for this binary's constant: a workflow of
// a kind core does not run must ask for a runner that announces that kind, and therefore land on a
// queue nothing in an un-upgraded region consumes.
func TestTheCarrierIsNamedForTheDocumentsCapability(t *testing.T) {
	cfg := map[string]string{domain.CanaryWorkflowKey: `{"kind":"async_transaction_v9"}`}
	queue, err := canaryCarrierFor(CheckJob{Monitor: canaryCarrierMonitor(cfg)}, "core", ProtocolV1)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if !strings.Contains(queue, "async_transaction_v9@") {
		t.Fatalf("queue = %q, want the document's own kind", queue)
	}
}
