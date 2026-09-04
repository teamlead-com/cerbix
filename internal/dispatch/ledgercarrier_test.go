package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// FR-032 invariants 10g and 11.
//
// 10g: carrier isolation is PHYSICAL on the transports that have a wire. An executor of an older
// generation cannot receive a V4 AMQP delivery because it is not subscribed to that queue — not
// because a capability check refused it after the fact.
//
// 11: the `Dispatcher` interface exposes no ack. Asserted as an exact method SET, because a name
// check would let `Nack`, `Settle` or `Confirm` past.

func TestEveryCarrierGenerationHasItsOwnQueue(t *testing.T) {
	seen := map[string]int{}
	for _, generation := range []int{ProtocolV1, ProtocolV2, ProtocolV3, ProtocolV4} {
		queue, ok := jobsQueueForGeneration("core", generation)
		if !ok {
			t.Fatalf("generation %d maps to no queue, so nothing can be published for it", generation)
		}
		if prior, clash := seen[queue]; clash {
			t.Fatalf("generations %d and %d share the queue %q: a consumer of one would receive "+
				"the other, which is a filter rather than isolation", prior, generation, queue)
		}
		seen[queue] = generation
	}
}

// The boundary must stay CLOSED by default. A generation nobody mapped must fail to route rather
// than falling back to an older queue, where an executor would receive a payload it cannot read.
func TestAnUnknownCarrierGenerationRoutesNowhere(t *testing.T) {
	for _, generation := range []int{0, 5, 99} {
		if queue, ok := jobsQueueForGeneration("core", generation); ok {
			t.Errorf("generation %d routed to %q; an unmapped generation must be refused", generation, queue)
		}
	}
}

// The v4 queue is bound only by an executor that announces the LEDGER capability — never the
// credential one. Identity applies to every monitor, so reusing the envelope number would tie an
// ordinary HTTP monitor's eligibility to a capability its dispatch never needs (§13.0).
func TestTheLedgerCapabilityIsSeparateFromTheCredentialOne(t *testing.T) {
	d := &AMQP{}
	d.WithCredentialCapability(EnvelopeV2)
	if d.ledgerCapability != 0 {
		t.Fatal("declaring the credential capability also declared the ledger one; a worker that " +
			"can open envelopes is not thereby able to read job identity")
	}
	d.WithLedgerCapability(1)
	if d.credentialCapability != EnvelopeV2 || d.ledgerCapability != 1 {
		t.Fatalf("the two capabilities are not independent: credential=%d ledger=%d",
			d.credentialCapability, d.ledgerCapability)
	}
}

// Invariant 11, second half. The SET is enumerated: a check for "contains no method called Ack"
// would pass on `Nack`, `Settle` or `Confirm`.
func TestTheDispatcherInterfaceExposesNoAck(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dispatch.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"PublishJob": true, "Jobs": true, "PublishResult": true, "Results": true, "Close": true}
	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Dispatcher" {
			return true
		}
		iface, ok := spec.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, method := range iface.Methods.List {
			for _, name := range method.Names {
				found[name.Name] = true
			}
		}
		return false
	})
	if len(found) == 0 {
		t.Fatal("the Dispatcher interface was not found, so this assertion guards nothing")
	}
	for name := range found {
		if !want[name] {
			t.Errorf("Dispatcher gained the method %q. Settlement is the transport's own business: "+
				"an executor that can ack through this interface can also decline to, and the "+
				"scheduler would then depend on executor behaviour it cannot see", name)
		}
	}
	for name := range want {
		if !found[name] {
			t.Errorf("Dispatcher no longer declares %q; this assertion's SET is stale", name)
		}
	}
}
