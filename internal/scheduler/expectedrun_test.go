package scheduler

import (
	"context"
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

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-032 phase B2 — the leader's side of the ledger.
//
// The four invariants the discharge audit found had NOTHING testing them are the ones that say
// what FR-032 is FOR, and invariant 1 is the OWNER's headline property: no monitor's probe instant
// changes as a result of this requirement. It is asserted here rather than in the store because
// the instant is the leader's to compute, and the ledger's whole claim is that it PERSISTS the
// instant the leader already computed rather than producing one of its own.

func ledgerExpectation(monitorID string, due time.Time, revision int64) store.DueExpectation {
	return store.DueExpectation{
		MonitorID: monitorID, DueAt: due, IntervalInForce: 60, Revision: revision,
		JobID: "11111111-1111-4111-8111-111111111111", IssuedAt: due.Add(time.Second),
	}
}

func advancesFrom(fs *fakeStore) []store.ExpectationAdvance {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]store.ExpectationAdvance(nil), fs.advances...)
}

// Invariant 1 — the persisted expectation is the instant the leader ALREADY computes, and the job
// carries the WINDOW it answers rather than an instant of the ledger's own.
//
// The owner chose this model over an absolute `floor(unix / interval)` grid for exactly this
// reason: a grid would have moved the probe instant of every monitor in every existing
// installation. So the assertion is on the relationship between the two — the advance's NextDue is
// the cadence instant, and the window it closes is the one the schedule already held.
func TestTheAdvancePersistsTheInstantTheLeaderAlreadyComputed(t *testing.T) {
	due := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)
	monitor := domain.Monitor{
		ID: "plain-monitor", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 7,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor},
		expectations: map[string]store.DueExpectation{
			monitor.ID: ledgerExpectation(monitor.ID, due, monitor.ExecutionRevision)}}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger()).
		WithLedgerCarrier(true).
		WithLocalLedgerRegions(domain.DefaultRegion)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	var delivered dispatch.DeliveredJob
	select {
	case delivered = <-disp.Jobs():
	case <-time.After(3 * time.Second):
		t.Fatal("the scheduler published no job")
	}
	// The identity rides the carrier that is DEFINED by carrying it, and only that one.
	if delivered.Job.ProtocolVersion != dispatch.ProtocolV4 {
		t.Fatalf("a monitor with a standing expectation was published on generation %d, want %d",
			delivered.Job.ProtocolVersion, dispatch.ProtocolV4)
	}
	if delivered.Job.DueAt.IsZero() || !delivered.Job.DueAt.Equal(due) {
		t.Fatalf("the job carries DueAt %s, want the standing expectation %s — minted from the "+
			"schedule and not from the 15-second snapshot (invariant 25a)", delivered.Job.DueAt, due)
	}
	if delivered.Job.JobID == "" || delivered.Job.IssuedAt.IsZero() {
		t.Errorf("a generation-4 job is missing identity: %+v", delivered.Job)
	}

	waitUntil(t, 3*time.Second, "the advance to be recorded", func() bool {
		return len(advancesFrom(fs)) > 0
	})
	advance := advancesFrom(fs)[0]
	if advance.MonitorID != monitor.ID || advance.JobID != delivered.Job.JobID {
		t.Fatalf("the advance records job %q for monitor %q; the published job was %q",
			advance.JobID, advance.MonitorID, delivered.Job.JobID)
	}
	// The FENCE is the expectation the job was published against, and the CARRIER is the one it
	// actually rode — every run fact on the row comes from the published job (invariant 25d).
	if !advance.ExpectedDue.Equal(due) {
		t.Errorf("the fence compares against %s, want the standing expectation %s", advance.ExpectedDue, due)
	}
	if advance.ExpectedRevision != monitor.ExecutionRevision {
		t.Errorf("the fence compares against generation %d, want the published %d",
			advance.ExpectedRevision, monitor.ExecutionRevision)
	}
	if advance.CarrierGeneration != delivered.Job.ProtocolVersion {
		t.Errorf("the advance records carrier %d, the job rode %d",
			advance.CarrierGeneration, delivered.Job.ProtocolVersion)
	}
	// The interval is the monitor's own and NEVER a backoff delay, and the next expectation is one
	// interval out — the instant the in-memory cadence map holds.
	if advance.IntervalInForce != monitor.IntervalSeconds {
		t.Errorf("the advance records interval %d, want the monitor's %d",
			advance.IntervalInForce, monitor.IntervalSeconds)
	}
	if gap := advance.NextDue.Sub(time.Now().UTC()); gap > time.Duration(monitor.IntervalSeconds)*time.Second ||
		gap < time.Duration(monitor.IntervalSeconds-5)*time.Second {
		t.Errorf("the next expectation is %s away, want ~%ds — the ledger must persist the instant "+
			"the leader already computed, not one of its own", gap, monitor.IntervalSeconds)
	}
	if advance.Confirming {
		t.Error("a monitor not in confirm phase was recorded as accelerating")
	}
}

// Invariant 10i's PRODUCER half — a monitor with no standing expectation is published BELOW the
// ledger carrier, never as a generation-4 delivery missing the field that defines it.
//
// The consumer's duty is to dead-letter such a delivery, and the producer's duty is never to make
// one. Both exist because a rule enforced on one side only is a rule two sides can come to
// disagree about, which is what nine of B1's findings were.
func TestAMonitorWithNoStandingExpectationStaysBelowTheLedgerCarrier(t *testing.T) {
	monitor := domain.Monitor{
		ID: "unscheduled", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 3,
	}
	// No entry in `expectations`: the monitor has no schedule row, which is what a push monitor, a
	// just-disabled one, or one a concurrent configuration write removed looks like from here.
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger()).
		WithLedgerCarrier(true).
		WithLocalLedgerRegions(domain.DefaultRegion)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case delivered := <-disp.Jobs():
		if delivered.Job.ProtocolVersion >= dispatch.ProtocolV4 {
			t.Fatalf("a monitor with no window was published on generation %d — a v4 consumer is "+
				"entitled to DEAD-LETTER that delivery, so the probe would never run",
				delivered.Job.ProtocolVersion)
		}
		if !delivered.Job.DueAt.IsZero() {
			t.Errorf("the job carries a DueAt it has no schedule row for: %s", delivered.Job.DueAt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the scheduler published no job at all; the ledger must degrade in TRUTH, never in delivery")
	}
	if got := advancesFrom(fs); len(got) != 0 {
		t.Errorf("%d advances were recorded for a monitor with no schedule row: %+v", len(got), got)
	}
}

// §16.1's third row, as a failure mode: the ledger degrades in TRUTH, never in delivery. A read
// that fails must cost no probe.
//
// This is the property that makes the whole feature safe to deploy: every window affected reads
// `unknown`, which is honest, and not one measurement is lost.
func TestALedgerReadFailureCostsNoProbe(t *testing.T) {
	monitor := domain.Monitor{
		ID: "still-probed", Type: domain.MonitorHTTP, Target: "https://example.com",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
		ExecutionRevision: 1,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor},
		expectationsErr: errors.New("the ledger is unavailable")}
	disp := dispatch.NewInProc(8)
	s := New(fs, disp, testLogger()).
		WithLedgerCarrier(true).
		WithLocalLedgerRegions(domain.DefaultRegion)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case delivered := <-disp.Jobs():
		if delivered.Job.ProtocolVersion >= dispatch.ProtocolV4 {
			t.Errorf("a job was published on generation %d with no expectation to carry",
				delivered.Job.ProtocolVersion)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a ledger read failure stopped the probe: the ledger degraded in DELIVERY")
	}
}

// Advance rules 1, 3 and 4 are all callers of the one primitive, on BOTH branches; rule 2 is not a
// caller at all. FR-029 shipped an in-flight claim on one branch only, and this structure exists
// so that mistake cannot be repeated (§7.2, invariant 2a).
func TestEveryForwardMovingRuleRecordsItsAdvanceAndRuleTwoRecordsNothing(t *testing.T) {
	due := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)
	// Rule 3 on the PLAIN branch: a canary with no capable runner in its region.
	t.Run("rule 3, a policy skip on the plain branch", func(t *testing.T) {
		canary := domain.Monitor{
			ID: "canary", Type: domain.MonitorAsyncCanary, Region: domain.DefaultRegion,
			Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5, ExecutionRevision: 2,
			Config: map[string]string{"workflow": "checkout@v1"},
		}
		fs := &fakeStore{leader: true, monitors: []domain.Monitor{canary},
			expectations: map[string]store.DueExpectation{
				canary.ID: ledgerExpectation(canary.ID, due, canary.ExecutionRevision)}}
		s := New(fs, dispatch.NewInProc(8), testLogger()).
			WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion).
			WithCredentialLiveRegions(staticCredentialRegions{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx)
		waitUntil(t, 3*time.Second, "the skip to be recorded", func() bool {
			return len(advancesFrom(fs)) > 0
		})
		advance := advancesFrom(fs)[0]
		if advance.SkipReason != store.SkipNoCapableRunner {
			t.Fatalf("the skip records reason %q, want %q", advance.SkipReason, store.SkipNoCapableRunner)
		}
		if advance.JobID != "" || advance.CarrierGeneration != 0 {
			t.Errorf("a skipped window carries a job or a carrier: %+v — §6.1's CHECK makes the "+
				"two one decision, and a skip made neither", advance)
		}
	})
	// Rule 4 on the CREDENTIALED branch: the authoritative read could not resolve this monitor's
	// secrets, so the instant moves by a BACKOFF while the interval does not. FR-029 shipped an
	// in-flight claim on one branch only, and rules 1, 3 and 4 all going through one primitive on
	// both branches is the structure arranged to make that unrepeatable.
	t.Run("rule 4, a backoff on the credentialed branch", func(t *testing.T) {
		monitor := domain.Monitor{
			ID: "unresolvable", Type: domain.MonitorPostgres, Target: "db:5432",
			Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
			ExecutionRevision: 4,
		}
		fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor},
			expectations: map[string]store.DueExpectation{
				monitor.ID: ledgerExpectation(monitor.ID, due, monitor.ExecutionRevision)},
			materialize: func(ids []string) ([]store.MaterializedExecution, error) {
				out := make([]store.MaterializedExecution, 0, len(ids))
				for _, id := range ids {
					out = append(out, store.MaterializedExecution{
						MonitorID: id, Reason: store.MaterializeMissingReference})
				}
				return out, nil
			}}
		s := New(fs, dispatch.NewInProc(8), testLogger()).
			WithCredentialEnvelopes(true).
			WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion).
			WithLocalCredentialRegions(domain.DefaultRegion)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx)
		waitUntil(t, 3*time.Second, "the backoff to be recorded", func() bool {
			return len(advancesFrom(fs)) > 0
		})
		advance := advancesFrom(fs)[0]
		if advance.SkipReason != store.SkipCredentialUnresolved {
			t.Fatalf("the backoff records reason %q, want %q", advance.SkipReason, store.SkipCredentialUnresolved)
		}
		if advance.IntervalInForce != monitor.IntervalSeconds {
			t.Fatalf("the backoff recorded interval %d, want the monitor's %d — a delay that became "+
				"interval_in_force would space every later gap window by a retry timer, invisibly "+
				"and permanently (invariant 2b)", advance.IntervalInForce, monitor.IntervalSeconds)
		}
		if !advance.NextDue.After(time.Now().UTC().Add(time.Duration(monitor.IntervalSeconds-5) * time.Second)) {
			t.Errorf("the next expectation is %s, which is not a backoff away", advance.NextDue)
		}
	})
	// Rule 2 on the plain branch: the dispatch FAILED, so the expectation is unchanged and there
	// is nothing to preserve. A row here would record a window as answered or abandoned when it is
	// neither.
	t.Run("rule 2, a failed dispatch records nothing", func(t *testing.T) {
		monitor := domain.Monitor{
			ID: "unpublishable", Type: domain.MonitorHTTP, Target: "https://example.com",
			Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
			ExecutionRevision: 1,
		}
		fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor},
			expectations: map[string]store.DueExpectation{
				monitor.ID: ledgerExpectation(monitor.ID, due, monitor.ExecutionRevision)}}
		// A dispatcher that refuses every publish is the failure this rule is about.
		s := New(fs, refusingDispatcher{}, testLogger()).
			WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx)
		// Give the leader several ticks: the assertion is an ABSENCE, so it has to be given time
		// to be violated.
		time.Sleep(1500 * time.Millisecond)
		if got := advancesFrom(fs); len(got) != 0 {
			t.Fatalf("a failed dispatch recorded %d advances: %+v — the expectation is unchanged, "+
				"so the next tick retries it and there is nothing to preserve", len(got), got)
		}
	})
}

// Invariant 10l — with the flag ON, `role=all` reaches generation 4 and its PULL regions do not.
//
// These are the two V3 failures §13.0 quotes, and they point in opposite directions: one left the
// feature inert in the most common deployment, the other left a row in a region whose agents could
// not claim it, so the monitor had no outcome at all until the row's TTL. §13.0 requires both
// rules at the SAME place — resolve time — "so neither builder order nor configuration can restore
// it".
func TestTheFlagOnRaisesTheInProcessRegionAndNeverItsPullRegions(t *testing.T) {
	monitor := domain.Monitor{
		ID: "credentialed", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: domain.DefaultRegion, Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
	}
	pulled := domain.Monitor{
		ID: "pulled", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: "edge", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor, pulled}}
	// No AMQP management client and no announcing agent: the ONLY thing that can raise a
	// generation here is the in-process rule, which is the case that was missing.
	s := New(fs, dispatch.NewInProc(8), testLogger()).
		WithCredentialEnvelopes(true).
		WithLedgerCarrier(true).
		WithLocalCredentialRegions(domain.DefaultRegion).
		WithLocalLedgerRegions(domain.DefaultRegion).
		WithPullRegions([]string{"edge"})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go s.Run(ctx)
	waitUntil(t, 4*time.Second, "the in-process region to be raised to generation 4", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		for _, policy := range fs.carrierPolicies {
			if policy[domain.DefaultRegion] >= dispatch.ProtocolV4 {
				return true
			}
		}
		return false
	})
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, policy := range fs.carrierPolicies {
		if got := policy["edge"]; got >= dispatch.ProtocolV4 {
			t.Fatalf("a pull-served region was raised to generation %d by the in-process rule; its "+
				"AGENTS decide what it can receive, and a row they cannot claim leaves the monitor "+
				"with no outcome until the row's TTL", got)
		}
	}
}

// refusingDispatcher fails every publish, which is advance rule 2's whole subject.
type refusingDispatcher struct{}

func (refusingDispatcher) PublishJob(context.Context, dispatch.CheckJob) error {
	return errors.New("broker unreachable")
}
func (refusingDispatcher) Jobs() <-chan dispatch.DeliveredJob { return nil }
func (refusingDispatcher) PublishResult(context.Context, domain.Heartbeat) error {
	return errors.New("broker unreachable")
}
func (refusingDispatcher) Results() <-chan domain.Heartbeat { return nil }
func (refusingDispatcher) Close() error                     { return nil }

// ---------------------------------------------------------------------------------------------
// Source scans. Three invariants here are properties of the code's SHAPE, and the audit says so
// because a behavioural test cannot see them: "one helper computes the effective interval" is not
// observable from any single run.

func parseSchedulerPackage(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	set := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(set, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		t.Fatalf("no sources parsed from %s; the guard would report green over nothing", dir)
	}
	return files
}

// callsMethodOn reports the functions whose body calls the named method.
func functionsCalling(files []*ast.File, method string) []string {
	var out []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
					found = true
				}
				return !found
			})
			if found {
				out = append(out, fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Invariant 20d — the effective interval has ONE owner.
//
// It was computed twice: `iv := m.Interval()` followed by a possible `iv = m.ConfirmInterval()` on
// the plain dispatch path and again on the credentialed one. Two sites computing one fact is the
// divergence this design was bitten by three times, and the two sites did NOT agree — the plain
// one omitted the `InConfirmPhase()` check.
//
// The mutation that must kill this: inline `m.ConfirmInterval()` back into either dispatch branch.
// The test names the function that took it, so the failure identifies the newcomer.
func TestTheEffectiveIntervalHasExactlyOneOwner(t *testing.T) {
	files := parseSchedulerPackage(t, ".")
	callers := functionsCalling(files, "ConfirmInterval")
	want := []string{"effectiveInterval", "enterConfirm"}
	if strings.Join(callers, ",") != strings.Join(want, ",") {
		t.Fatalf("ConfirmInterval() is called from %v, want exactly %v.\n"+
			"`enterConfirm` computes the accelerated INSTANT, which is a different question from "+
			"the effective interval; every dispatch path must ask `effectiveInterval` (invariant "+
			"20d), or §7.3 cannot say which interval spaced a window for half the monitors.",
			callers, want)
	}
}

// Invariant 2d — advance rule 5 has no ledger statement at all, so it is incapable of moving
// `next_due_at` forward in the strongest available sense.
//
// §7.3 originally gave rule 5 its own write. That write ran BEFORE any advance had produced a
// `next_due_at` from the confirm interval, leaving `interval_in_force` describing an instant it had
// not produced — the same mis-description the configuration write produced from the other
// direction. Both are gone: the advance writes the three columns together.
func TestConfirmAccelerationWritesNoLedgerStatement(t *testing.T) {
	files := parseSchedulerPackage(t, ".")
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name != "enterConfirm" {
				continue
			}
			for _, forbidden := range []string{"AdvanceExpectations", "SetConfirmAcceleration", "LoadDueExpectations"} {
				if len(functionsCalling([]*ast.File{{Decls: []ast.Decl{fn}, Name: file.Name}}, forbidden)) > 0 {
					t.Errorf("enterConfirm calls %s. Rule 5 moves the due instant EARLIER and only "+
						"when the standing expectation is later, so it has nothing to materialize "+
						"or fence — and a write there describes an instant no advance has produced "+
						"(invariants 2c, 2d).", forbidden)
				}
			}
			return
		}
	}
	t.Fatal("enterConfirm was not found; the guard would report green over nothing")
}

// Invariant 21 — nothing in the scheduler or the prober reads the ledger to decide what to probe.
// `expected_runs` is EVIDENCE, not a job queue.
//
// It inspects STRING LITERALS rather than the file's text, and among those only the ones that are
// SQL. Both narrowings were forced by the guard reporting a violation on a correct tree, which is
// the failure phase A's own guard recorded from the other side — a guard that miscounts its own
// subject gets deleted by whoever is unblocking CI:
//
//   - the table's name appears in the comments here deliberately and at length, so a text scan
//     fails immediately;
//   - and the leader's maintenance pass logs `purge_expected_runs_failed`, which NAMES the ledger
//     without reading it. A log key is not a query, and treating one as a violation would push the
//     next person to rename the log line rather than to think about the rule.
//
// What the invariant is actually about is a STATEMENT — something with a FROM, an INTO, a JOIN or
// an UPDATE beside the table's name. That is what a dispatch decision would have to contain.
func TestNoSchedulerOrProberPathReadsTheLedger(t *testing.T) {
	sqlKeywords := []string{"FROM", "INTO", "JOIN", "UPDATE ", "DELETE"}
	for _, dir := range []string{".", "../prober"} {
		files := parseSchedulerPackage(t, dir)
		for _, file := range files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.Contains(lit.Value, "expected_runs") {
					return true
				}
				upper := strings.ToUpper(lit.Value)
				for _, kw := range sqlKeywords {
					if strings.Contains(upper, kw) {
						t.Errorf("%s contains a SQL literal naming expected_runs: %s\n"+
							"The ledger is evidence and nothing reads it to decide what to probe "+
							"(invariant 21); a query here is one refactor away from being that.",
							dir, firstLine(lit.Value))
						break
					}
				}
				return true
			})
		}
	}
}

// The guard's own negative, because the narrowing above could have turned it into a test that
// passes on everything. A SQL literal naming the ledger must still be caught, and a bare log key
// must still be allowed — the two fixtures state the boundary in both directions.
func TestTheLedgerReadGuardStillCatchesASQLLiteral(t *testing.T) {
	sqlKeywords := []string{"FROM", "INTO", "JOIN", "UPDATE ", "DELETE"}
	catches := func(literal string) bool {
		if !strings.Contains(literal, "expected_runs") {
			return false
		}
		upper := strings.ToUpper(literal)
		for _, kw := range sqlKeywords {
			if strings.Contains(upper, kw) {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name    string
		literal string
		want    bool
	}{
		{"a select the scheduler must never contain", "SELECT 1 FROM expected_runs WHERE monitor_id = $1", true},
		{"an insert", "INSERT INTO expected_runs (monitor_id) VALUES ($1)", true},
		{"a join", "SELECT 1 FROM monitors m JOIN expected_runs e ON e.monitor_id = m.id", true},
		{"the maintenance pass's log key", "purge_expected_runs_failed", false},
		{"a metric name", "cerbix_expected_runs_hot_update_ratio", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := catches(tc.literal); got != tc.want {
				t.Fatalf("the guard's predicate answers %v for %q, want %v", got, tc.literal, tc.want)
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "..."
	}
	return s
}
