package dispatch

import (
	"go/ast"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase C — the claim's wire shape and its ONE owner (§8.4).

// A claim is built only for a delivery that ARRIVED on the ledger carrier, and the refusal is the
// whole of the carrier rule on this side: a claim exists to fill `claimed_at` on one row identified
// by `(monitor_id, due_at)` plus the job's id, so a delivery below generation 4 has nothing to
// correlate to and the message would be pure cost.
//
// The carrier and not the payload, which is A3. The three payload fields decided this until then,
// and a generation-1 job carrying `DueAt` — the producer slip A1 closed — therefore produced a
// claim the core would correlate. Two halves of one property: the producer cannot ship the field
// below generation 4, and the consumer does not read the field to answer a carrier question.
//
// The mutation that must kill this: decide from `job.DueAt` again, or drop the guard entirely. A
// fleet running below the ledger carrier then pays a round trip per run for a message the core
// discards, and a slipped field becomes a claim.
func TestAClaimIsBuiltOnlyForADeliveryOnTheLedgerCarrier(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	full := CheckJob{
		Monitor:  domain.Monitor{ID: "m", ExecutionRevision: 7},
		JobID:    "11111111-1111-4111-8111-111111111111",
		IssuedAt: at.Add(-time.Second),
		DueAt:    at.Add(-time.Minute),
	}
	claim, ok := ClaimHeartbeat(DeliveredJob{Job: full, CarrierGeneration: ProtocolV4}, at)
	if !ok {
		t.Fatal("a generation-4 delivery carrying full identity produced no claim")
	}
	if claim.Claim == nil || !claim.Claim.At.Equal(at) {
		t.Fatalf("the claim instant is %v, want the caller's %s — taken from the caller so a batch "+
			"can share the instant its poll happened at", claim.Claim, at)
	}
	// It carries the correlation and nothing that would make it look like a result.
	if claim.JobID != full.JobID || !claim.DueAt.Equal(full.DueAt) || !claim.JobIssuedAt.Equal(full.IssuedAt) {
		t.Errorf("the claim lost its correlation: %+v", claim)
	}
	if claim.ExecutionRevision != full.Monitor.ExecutionRevision {
		t.Errorf("the claim carries revision %d, want the job's %d", claim.ExecutionRevision, full.Monitor.ExecutionRevision)
	}
	if !claim.Ts.IsZero() || claim.Up || claim.ProbeError != nil {
		t.Errorf("the claim looks like a result: %+v — a heartbeat carrying one records ONLY the "+
			"claim, and a timestamp would invite the ingest path to treat it as an observation", claim)
	}

	// The A3 case: a full identity on a carrier that is not the ledger's. This is the delivery a
	// producer slip creates, and it is the one the old payload-shaped guard admitted.
	for _, carrier := range []int{ProtocolV1, ProtocolV2, ProtocolV3} {
		if _, ok := ClaimHeartbeat(DeliveredJob{Job: full, CarrierGeneration: carrier}, at); ok {
			t.Errorf("a generation-%d delivery produced a claim because its BODY carried a window; "+
				"the carrier a message arrived on is the fact, and the body is content", carrier)
		}
	}

	// And a generation-4 delivery missing any of the three fields still produces nothing. The gate
	// dead-letters such a delivery before an executor gets here, so this is the exported
	// function's own contract rather than a path the product reaches.
	for _, tc := range []struct {
		name   string
		mutate func(j *CheckJob)
	}{
		{"no job id", func(j *CheckJob) { j.JobID = "" }},
		{"no window", func(j *CheckJob) { j.DueAt = time.Time{} }},
		{"no issue instant", func(j *CheckJob) { j.IssuedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := full
			tc.mutate(&job)
			if _, ok := ClaimHeartbeat(DeliveredJob{Job: job, CarrierGeneration: ProtocolV4}, at); ok {
				t.Fatalf("%s produced a claim; it can correlate to no window, so the message is "+
					"pure cost", tc.name)
			}
		})
	}
}

// Invariant 11 — the `Dispatcher` interface gains NO method for the claim.
//
// §4a requires that `worker` and `agent` stay DB-less executors returning idempotent events over
// the EXISTING transport, and B1 discharged the five-method set against an `Ack` mutation. Phase C
// is where that set is most tempting to grow: a claim is a new kind of message, and the obvious
// shape is a new method for it. The claim rides `PublishResult` instead, as `ProbeError` does.
//
// The mutation that must kill this: add `PublishClaim(ctx, hb) error` to the interface.
func TestTheClaimAddsNoDispatcherMethod(t *testing.T) {
	files := parseGoPackage(t, ".")
	var methods []string
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != "Dispatcher" {
				return true
			}
			iface, ok := spec.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			for _, m := range iface.Methods.List {
				for _, name := range m.Names {
					methods = append(methods, name.Name)
				}
			}
			return false
		})
	}
	sort.Strings(methods)
	want := []string{"Close", "Jobs", "PublishJob", "PublishResult", "Results"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("Dispatcher exposes %v, want exactly %v.\n"+
			"The claim rides the result path as a typed message (§8.4), which is what keeps the "+
			"transport seam free of concepts §4a rules out — and an enumerated SET is required "+
			"rather than a name check, because `PublishClaim`, `Claim` or `Started` would all slip "+
			"past one.", methods, want)
	}
}
