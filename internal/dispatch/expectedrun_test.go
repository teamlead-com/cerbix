package dispatch

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 phase B2 — the wire's side of the ledger: the field that DEFINES generation 4, the one
// owner of the carrier decision, and the refusal of a delivery that does not honour its own
// carrier's contract.

// `domain.LedgerMinCarrier` duplicates `ProtocolV4` by value because `dispatch` imports `domain`
// and the dependency only runs one way. A constant with no pin is the two-expressions-of-one-rule
// shape this design has been bitten by three times, so this is the pin.
func TestTheLedgerMinimumCarrierMatchesTheProtocolGeneration(t *testing.T) {
	if domain.LedgerMinCarrier != ProtocolV4 {
		t.Fatalf("domain.LedgerMinCarrier is %d and dispatch.ProtocolV4 is %d. The verdict function "+
			"reads the first and the publisher writes the second, so a divergence makes every "+
			"window on the current carrier read `unknown` — or, worse, promotes one that should not be",
			domain.LedgerMinCarrier, ProtocolV4)
	}
}

// Invariant 25f — `DueAt` has ONE copier, and `StampResult` is it.
//
// "And its result twin" is not a specification: that phrase was accepted once, for a field that
// was later deleted, and the rule outlives it. Three executors publish results — the AMQP worker
// pool, the pull agent's batch, and the probe-error path — and a stamp each of them applied
// separately would drift the first time one was edited.
//
// The mutation that must kill this: assign `hb.DueAt` anywhere else.
func TestOnlyStampResultCopiesTheWindowOntoAResult(t *testing.T) {
	var copiers []string
	for _, dir := range []string{".", "../worker", "../agent", "../prober", "../ingest"} {
		for name, file := range parseGoPackage(t, dir) {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					assign, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for _, lhs := range assign.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if ok && sel.Sel.Name == "DueAt" {
							copiers = append(copiers, dir+"/"+name+":"+fn.Name.Name)
						}
					}
					return true
				})
			}
		}
	}
	sort.Strings(copiers)
	want := []string{"./dispatch.go:StampResult"}
	if strings.Join(copiers, ",") != strings.Join(want, ",") {
		t.Fatalf("DueAt is assigned in %v, want exactly %v.\n"+
			"One owner, because three executors publish results and a stamp each applied "+
			"separately would drift the first time one was edited (invariant 25f).", copiers, want)
	}
}

// `CarrierFor` is the ONE owner of "which generation does this job ride", and the generations are
// not a ladder a `min` can walk. Every case below is a rule the three call sites would otherwise
// each have gotten right for the case in front of them — which is how B1's publisher came to
// refuse a generation-4 job that had no envelope.
func TestTheCarrierDecisionHasOneOwnerAndFourRules(t *testing.T) {
	for _, tc := range []struct {
		name        string
		region      int
		hasEnvelope bool
		hasWindow   bool
		typ         domain.MonitorType
		want        int
	}{
		{"a secretless monitor with a window in a ledger region", ProtocolV4, false, true, domain.MonitorHTTP, ProtocolV4},
		{"a secretless monitor with NO window in a ledger region", ProtocolV4, false, false, domain.MonitorHTTP, ProtocolV1},
		{"a secretless monitor in a generation-3 region", ProtocolV3, false, true, domain.MonitorHTTP, ProtocolV1},
		{"a credentialed monitor with a window in a ledger region", ProtocolV4, true, true, domain.MonitorPostgres, ProtocolV4},
		{"a credentialed monitor with NO window in a ledger region", ProtocolV4, true, false, domain.MonitorPostgres, ProtocolV3},
		{"a credentialed monitor in a generation-3 region", ProtocolV3, true, true, domain.MonitorPostgres, ProtocolV3},
		{"a credentialed monitor in a generation-2 region", ProtocolV2, true, true, domain.MonitorPostgres, ProtocolV2},
		{"a credentialed monitor in an unresolved region", 0, true, true, domain.MonitorPostgres, ProtocolV2},
		{"a canary with a window in a ledger region", ProtocolV4, false, true, domain.MonitorAsyncCanary, ProtocolV1},
		{"a credentialed canary in a ledger region", ProtocolV4, true, true, domain.MonitorAsyncCanary, ProtocolV3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CarrierFor(tc.region, tc.hasEnvelope, tc.hasWindow, tc.typ); got != tc.want {
				t.Fatalf("CarrierFor(%d, envelope=%v, window=%v, %s) = %d, want %d",
					tc.region, tc.hasEnvelope, tc.hasWindow, tc.typ, got, tc.want)
			}
		})
	}
	// Every generation the decision can return must have a queue, or the publish fails at
	// runtime for a combination nobody tried. Asserted over the SET of outputs rather than by
	// listing queues, because the failure mode is a new generation with no mapping.
	seen := map[int]bool{}
	for _, region := range []int{0, ProtocolV1, ProtocolV2, ProtocolV3, ProtocolV4} {
		for _, envelope := range []bool{false, true} {
			for _, window := range []bool{false, true} {
				for _, typ := range []domain.MonitorType{domain.MonitorHTTP, domain.MonitorPostgres, domain.MonitorAsyncCanary} {
					seen[CarrierFor(region, envelope, window, typ)] = true
				}
			}
		}
	}
	for generation := range seen {
		if _, ok := jobsQueueForGeneration("core", generation); !ok {
			t.Errorf("CarrierFor can return generation %d, which no jobs queue maps", generation)
		}
	}
	// The canary carrier is separately mapped and separately capped, so the same question is
	// asked of it: a canary must never be handed a generation its own queue mapping lacks.
	for _, envelope := range []bool{false, true} {
		generation := CarrierFor(ProtocolV4, envelope, true, domain.MonitorAsyncCanary)
		if _, ok := canaryQueueForGeneration("checkout@v1", "core", generation); !ok {
			t.Errorf("a canary was assigned generation %d, which no canary queue maps — its "+
				"windows read `unknown`, which is a stated limitation, and an unroutable job is not",
				generation)
		}
	}
}

// Invariant 10i — a generation-4 delivery missing its identity is a PROTOCOL VIOLATION, refused on
// the CARRIER and never on the payload's own claim about itself.
//
// Two mutations must fail it, and §17.4 named both before B1 shipped: treat the missing field as a
// rolling-upgrade case and probe anyway, which is the plausible mistake because the tolerant
// branch for OLDER carriers is one `if` away; and drop the delivery silently, which passes any
// assertion that only checks "no probe ran".
func TestAGenerationFourDeliveryMissingItsIdentityIsRefused(t *testing.T) {
	monitor := domain.Monitor{ID: "m", Type: domain.MonitorHTTP, Target: "https://example.com"}
	full := CheckJob{
		Monitor: monitor, ProtocolVersion: ProtocolV4,
		JobID:    "11111111-1111-4111-8111-111111111111",
		IssuedAt: time.Now(), DueAt: time.Now().Add(-time.Minute),
	}
	for _, tc := range []struct {
		name    string
		mutate  func(j *CheckJob)
		missing string
	}{
		{"no job id", func(j *CheckJob) { j.JobID = "" }, "job_id"},
		{"no issue instant", func(j *CheckJob) { j.IssuedAt = time.Time{} }, "issued_at"},
		{"no window", func(j *CheckJob) { j.DueAt = time.Time{} }, "due_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := full
			tc.mutate(&job)
			if got := job.LedgerFieldsMissing(); got != tc.missing {
				t.Fatalf("LedgerFieldsMissing is %q, want %q — a dead-letter that does not NAME "+
					"the field sends its reader after the wrong bug", got, tc.missing)
			}
			// The executor gate refuses it, and the refusal is a distinguishable sentinel rather
			// than a credential failure wearing its words: every credential rejection funnels into
			// ONE non-oracular reason so a prober is never told which way its forgery was wrong,
			// and this is not a credential rejection at all.
			_, err := ValidateAndMaterialize(nil, DeliveredJob{Job: job, CarrierGeneration: ProtocolV4})
			if !errors.Is(err, ErrLedgerIdentityMissing) {
				t.Fatalf("the gate returned %v, want ErrLedgerIdentityMissing", err)
			}
			if got := CredentialProbeErrorReason(err); got != domain.ProbeErrorUnsupportedVersion {
				t.Errorf("the diagnostic reads %q, want %q", got, domain.ProbeErrorUnsupportedVersion)
			}
			// The SAME absence on an OLDER carrier is the ordinary rolling-upgrade case: the
			// executor predates generation 4, drops the unknown field, and the window is simply
			// not ledger-eligible. This is the mutation that matters — the test must distinguish
			// carrier from payload.
			if _, err := ValidateAndMaterialize(nil, DeliveredJob{Job: job, CarrierGeneration: ProtocolV1}); err != nil {
				t.Errorf("a generation-1 delivery with no identity was refused: %v — that absence "+
					"is ordinary and makes the window unknown, not a violation", err)
			}
		})
	}
	// The converse, without which the refusal proves nothing: a complete generation-4 delivery
	// passes.
	if _, err := ValidateAndMaterialize(nil, DeliveredJob{Job: full, CarrierGeneration: ProtocolV4}); err != nil {
		t.Fatalf("a complete generation-4 delivery was refused: %v", err)
	}
	// And the predicate itself is carrier-shaped, not payload-shaped.
	if RequireLedgerFields(ProtocolV3) {
		t.Error("generation 3 is required to carry ledger identity; it predates the field")
	}
	if !RequireLedgerFields(ProtocolV4) {
		t.Error("generation 4 is not required to carry the identity that defines it")
	}
}

// StampResult copies the window with the identity, in ONE call.
func TestStampResultCarriesTheWindowWithTheIdentity(t *testing.T) {
	due := time.Now().Add(-time.Minute).Truncate(time.Second)
	job := CheckJob{
		JobID:    "11111111-1111-4111-8111-111111111111",
		IssuedAt: due.Add(time.Second), DueAt: due,
	}
	hb := StampResult(domain.Heartbeat{MonitorID: "m"}, job)
	if hb.JobID != job.JobID || !hb.JobIssuedAt.Equal(job.IssuedAt) || !hb.DueAt.Equal(job.DueAt) {
		t.Fatalf("StampResult produced %+v from %+v", hb, job)
	}
}

func parseGoPackage(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	set := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(set, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = parsed
	}
	if len(out) == 0 {
		t.Fatalf("no sources parsed from %s; the guard would report green over nothing", dir)
	}
	return out
}
