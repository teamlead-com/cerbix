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

// A claim is built only for a job that carries a window, and the refusal is the whole of the
// carrier rule on this side: a claim exists to fill `claimed_at` on one row identified by
// `(monitor_id, due_at)` plus the job's id, so a job below generation 4 has nothing to correlate to
// and the message would be pure cost.
//
// The mutation that must kill this: drop the guard and return a claim for every job. A fleet
// running below the ledger carrier then pays a round trip per run for a message the core discards.
func TestAClaimIsBuiltOnlyForAJobThatCarriesItsWindow(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	full := CheckJob{
		Monitor:  domain.Monitor{ID: "m", ExecutionRevision: 7},
		JobID:    "11111111-1111-4111-8111-111111111111",
		IssuedAt: at.Add(-time.Second),
		DueAt:    at.Add(-time.Minute),
	}
	claim, ok := ClaimHeartbeat(full, at)
	if !ok {
		t.Fatal("a job carrying full identity produced no claim")
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
			if _, ok := ClaimHeartbeat(job, at); ok {
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
