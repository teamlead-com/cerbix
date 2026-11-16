package store_test

// FR-043 / NFR-036 — the recovery threshold is alerting hysteresis, not measurement
// (func-recovery-threshold §2, D4, §6 I5 and I6, §7 rows "Scheduler" and "Regression").
//
// Two monitors that differ ONLY in `recovery_threshold` (1 and 3) are driven by the REAL leader
// loop over the real store, and every scheduled job is answered with the same scripted result
// through `RecordScheduledResult`. A hand-inserted `heartbeats` fixture read at two thresholds
// would only prove that the read queries ignore the column; here the rows are what the scheduler
// asked for and the result path wrote, so a cadence that read the run, or a status writer that
// dropped or added a row during it, shows up as a difference.
//
// Each monitor sits in its own project with its own one-member service, otherwise identical, so
// the project SLA, the service SLI, the gate decision and the status-page bars can be compared
// side by side and not only the per-monitor inputs.
//
// Both tests also assert that the two STATUS timelines differ — the threshold-3 monitor is still
// down at three checks where the threshold-1 monitor has already recovered. Without that the
// invariance halves compare two identical monitors and prove nothing.
//
// The confirm phase is left off (`confirm_interval_seconds` 0, `failure_threshold` 1): this is
// NFR-036 statement (b) of revision 6 — with the confirm phase not enabled, the checks scheduled and
// the `heartbeats` written are identical for every threshold. With it enabled they may differ, by
// design (statement (c)): the confirm phase reads `status`, so a monitor held in `down` is never
// accelerated where a threshold-1 monitor already `up` again is. That case is pinned by
// TestRecoveryThresholdHoldsTheConfirmPhaseOffWhileDown.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/scheduler"
	"github.com/teamlead-com/cerbix/internal/sla"
	"github.com/teamlead-com/cerbix/internal/store"
)

// nfr036Result is one scripted check result.
type nfr036Result struct {
	up      bool
	latency int64
	code    int
	msg     string
}

// nfr036Script is the timeline both monitors are fed: success, failure, one success, failure,
// then four successes. A threshold-1 monitor recovers twice (indexes 2 and 4); a threshold-3
// monitor sees a run of one broken by a failure, then completes a run of three at index 6.
var nfr036Script = []nfr036Result{
	{up: true, latency: 11, code: 200, msg: "OK"},
	{up: false, latency: 1000, code: 503, msg: "HTTP 503"},
	{up: true, latency: 13, code: 200, msg: "OK"},
	{up: false, latency: 1000, code: 502, msg: "HTTP 502"},
	{up: true, latency: 15, code: 200, msg: "OK"},
	{up: true, latency: 16, code: 200, msg: "OK"},
	{up: true, latency: 17, code: 200, msg: "OK"},
	{up: true, latency: 18, code: 200, msg: "OK"},
}

// The status each monitor must report after each scripted result: §4.1's table applied to the
// script. Index 0 is pending → up whatever the threshold (I2).
var (
	nfr036StatusAt1 = []domain.MonitorStatus{"up", "down", "up", "down", "up", "up", "up", "up"}
	nfr036StatusAt3 = []domain.MonitorStatus{"up", "down", "down", "down", "down", "down", "up", "up"}
)

// nfr036Dispatch is one job as the leader handed it to the transport.
type nfr036Dispatch struct {
	issuedAt, dueAt time.Time
	carrier         int
	interval        int
}

// nfr036Observed is what the result path said about one scripted result.
type nfr036Observed struct {
	cur               domain.MonitorStatus
	applied, inserted bool
	reason            string
}

// nfr036Recorder is the transport as the leader sees it: it records every job before handing it
// to the in-process queue the scripted executor reads.
type nfr036Recorder struct {
	dispatch.Dispatcher
	mu  sync.Mutex
	log map[string][]nfr036Dispatch
}

func (r *nfr036Recorder) PublishJob(ctx context.Context, job dispatch.CheckJob) error {
	r.mu.Lock()
	r.log[job.Monitor.ID] = append(r.log[job.Monitor.ID], nfr036Dispatch{
		issuedAt: job.IssuedAt, dueAt: job.DueAt,
		carrier: job.ProtocolVersion, interval: job.Monitor.IntervalSeconds,
	})
	r.mu.Unlock()
	return r.Dispatcher.PublishJob(ctx, job)
}

// nfr036Side is one of the two otherwise identical setups.
type nfr036Side struct {
	threshold                                                   int
	projectID, monitorID, serviceID, revisionID, epochID, label string
}

type nfr036Run struct {
	st    *store.Store
	dsn   string
	sides [2]nfr036Side
	// era is the bucket both services' declarations are retroactive from — their
	// materialization era's start.
	era time.Time
	// dispatched and observed are keyed by monitor id.
	dispatched map[string][]nfr036Dispatch
	observed   map[string][]nfr036Observed
}

// nfr036Backfill is how far back both service declarations reach. The gate's budget clauses read
// the 24h service report, and the report withholds every number while its window
// [sealed_through − 24h, sealed_through) reaches before the materialization era
// (`decideServiceWindow`: insufficient_history / window_precedes_materialization_era) — which a
// service declared seconds before its first check always does, so the decision could only ever be
// UNKNOWN / budget_withheld on both sides. Declaring the services 24h and a margin back puts the
// whole window inside the era; the backfilled buckets carry no heartbeat and read UNKNOWN time, so
// the window is `partial` / low coverage, which the report still quotes (with its coverage) and
// the budget clauses can decide on.
const nfr036Backfill = 24*time.Hour + 10*time.Minute

// nfr036GateDoc is a v1 gate policy over the 24h window with every clause assigned, so the
// decision walks the report, the burn latches and the incident predicate.
func nfr036GateDoc() domain.GatePolicyDocument {
	assign := map[domain.GateClause]domain.ClauseAssignment{
		domain.ClauseBudgetExhausted:     domain.ClauseAssignBlock,
		domain.ClauseBudgetConsumed:      domain.ClauseAssignWarn,
		domain.ClausePageBurnFiring:      domain.ClauseAssignBlock,
		domain.ClauseTicketBurnFiring:    domain.ClauseAssignWarn,
		domain.ClauseServiceIncidentOpen: domain.ClauseAssignWarn,
	}
	doc := domain.GatePolicyDocument{
		SchemaVersion: domain.GatePolicySchemaV1, Window: "24h",
		BudgetConsumedPercent: 90, MaxSealLagSeconds: 900, UnknownBehavior: domain.GateUnknownWarn,
	}
	for _, c := range domain.GateClausesV1 {
		doc.Clauses = append(doc.Clauses, domain.GateClauseEntry{Clause: c, Assignment: assign[c]})
	}
	return doc
}

// nfr036Setup creates one side: a project, a monitor at the given threshold with a burn-alerting
// SLO, a service whose only SLI member is that monitor, a service SLO and a project gate policy.
func nfr036Setup(t *testing.T, ctx context.Context, st *store.Store, orgID string, threshold int, backfill time.Time) nfr036Side {
	t.Helper()
	side := nfr036Side{threshold: threshold, label: fmt.Sprintf("threshold %d", threshold)}
	proj, err := st.CreateProject(ctx, orgID, fmt.Sprintf("shop-t%d", threshold), fmt.Sprintf("Shop T%d", threshold))
	if err != nil {
		t.Fatalf("%s: project: %v", side.label, err)
	}
	side.projectID = proj.ID
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		RecoveryThreshold: threshold,
		ProjectID:         proj.ID, Name: "checkout", Type: domain.MonitorHTTP,
		Target:          "https://checkout.example.com/health",
		IntervalSeconds: 1, TimeoutSeconds: 1, FailureThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("%s: monitor: %v", side.label, err)
	}
	if mon.RecoveryThreshold != threshold {
		t.Fatalf("%s: stored recovery_threshold = %d", side.label, mon.RecoveryThreshold)
	}
	side.monitorID = mon.ID
	if _, err := st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99.9, true, domain.DefaultBurnRules()); err != nil {
		t.Fatalf("%s: monitor SLO: %v", side.label, err)
	}
	svc, err := st.CreateService(ctx, domain.Service{ProjectID: proj.ID, Slug: "checkout", Name: "Checkout"})
	if err != nil {
		t.Fatalf("%s: service: %v", side.label, err)
	}
	side.serviceID = svc.ID
	// Retroactive from the run's one backfill instant (nfr036Backfill): both epochs are then
	// effective at the same bucket boundary, before the first check, rather than at whichever
	// boundary followed each declaration — and the era starts early enough for the 24h window.
	rev, epoch, err := st.PutServiceDeclaration(ctx, proj.ID, svc.ID, domain.ServiceDeclaration{
		Monitors: []string{mon.ID}, SLI: []string{mon.ID},
	}, 0, store.DeclarationOptions{CreatedBy: "nfr-036", BackfillFrom: backfill})
	if err != nil {
		t.Fatalf("%s: declaration: %v", side.label, err)
	}
	side.revisionID, side.epochID = rev.ID, epoch.ID
	if err := st.UpsertServiceSLATarget(ctx, proj.ID, svc.ID, "24h", 99.9); err != nil {
		t.Fatalf("%s: service SLO: %v", side.label, err)
	}
	if _, _, err := st.PutProjectGatePolicy(ctx, proj.ID, nil, nfr036GateDoc(),
		store.GateActor{ViaToken: true, Label: "token:nfr-036"}); err != nil {
		t.Fatalf("%s: gate policy: %v", side.label, err)
	}
	return side
}

// nfr036RunTimeline runs the leader until both monitors have answered the whole script, then
// stops it and waits for it to return, so nothing writes while the results are read.
func nfr036RunTimeline(t *testing.T) *nfr036Run {
	t.Helper()
	st, _ := testStore(t)
	setup, cancelSetup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelSetup()
	org, err := st.CreateOrganization(setup, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	run := &nfr036Run{
		st: st, dsn: os.Getenv("CERBIX_TEST_DATABASE_DSN"),
		dispatched: map[string][]nfr036Dispatch{},
		observed:   map[string][]nfr036Observed{},
	}
	// ONE instant for both sides, floored here: two declarations floored separately could land
	// on either side of a minute boundary.
	run.era = time.Now().Add(-nfr036Backfill).UTC().Truncate(domain.CanonicalBucket)
	run.sides[0] = nfr036Setup(t, setup, st, org.ID, 1, run.era)
	run.sides[1] = nfr036Setup(t, setup, st, org.ID, 3, run.era)

	inner := dispatch.NewInProc(16)
	rec := &nfr036Recorder{Dispatcher: inner, log: map[string][]nfr036Dispatch{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The ledger carrier is on, as in the dev stack and `role=all`: jobs carry their window and
	// the result fills it, so the expected-run ledger has something to compare.
	sch := scheduler.New(scheduler.NewStoreAdapter(st), rec, logger).
		WithLedgerCarrier(true).WithLocalLedgerRegions(domain.DefaultRegion)

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	leaderDone := make(chan struct{})
	go func() { defer close(leaderDone); sch.Run(runCtx) }()

	var mu sync.Mutex
	execErr := make(chan error, 1)
	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		for {
			select {
			case <-runCtx.Done():
				return
			case d := <-inner.Jobs():
				job := d.Job
				mu.Lock()
				k := len(run.observed[job.Monitor.ID])
				mu.Unlock()
				if k >= len(nfr036Script) {
					continue // past the script: the window stays unanswered on both sides
				}
				if job.IssuedAt.IsZero() {
					select {
					case execErr <- fmt.Errorf("job %d for %s carries no issue instant", k, job.Monitor.ID):
					default:
					}
					continue
				}
				// The claim first, as the worker pool sends it (`Pool.publishClaim`): after the job
				// is off the transport and before the probe, through the same store call the
				// ingest consumer makes for it (`Consumer.handleClaim` → `RecordRunClaim`). Without
				// it every window's `claimed_at` stayed NULL on both sides, a state production's
				// role=all does not produce.
				claim, ok := dispatch.ClaimHeartbeat(d, time.Now().UTC())
				if !ok {
					select {
					case execErr <- fmt.Errorf("job %d for %s produced no claim (carrier %d)", k, job.Monitor.ID, d.CarrierGeneration):
					default:
					}
					continue
				}
				if matched, err := st.RecordRunClaim(runCtx, claim); err != nil || !matched {
					if runCtx.Err() != nil {
						return
					}
					select {
					case execErr <- fmt.Errorf("claim %d for %s: matched=%v err=%v", k, job.Monitor.ID, matched, err):
					default:
					}
					continue
				}
				r := nfr036Script[k]
				// The observation instant is the job's issue instant: one database statement mints
				// it for every monitor due in the same tick, so two monitors scheduled together get
				// the same `ts` and their rows can be compared value for value.
				hb := dispatch.StampResult(domain.Heartbeat{
					MonitorID: job.Monitor.ID, Ts: job.IssuedAt,
					Up: r.up, LatencyMS: r.latency, Code: r.code, Msg: r.msg,
					ExecutionRevision: job.Monitor.ExecutionRevision,
				}, job)
				out, err := st.RecordScheduledResult(runCtx, hb)
				if err != nil {
					if runCtx.Err() != nil {
						return
					}
					select {
					case execErr <- fmt.Errorf("record result %d for %s: %w", k, job.Monitor.ID, err):
					default:
					}
					continue
				}
				mu.Lock()
				run.observed[job.Monitor.ID] = append(run.observed[job.Monitor.ID], nfr036Observed{
					cur: out.Cur, applied: out.Applied, inserted: out.Inserted, reason: out.Reason,
				})
				mu.Unlock()
			}
		}
	}()

	deadline := time.Now().Add(60 * time.Second)
	for {
		mu.Lock()
		n0, n1 := len(run.observed[run.sides[0].monitorID]), len(run.observed[run.sides[1].monitorID])
		mu.Unlock()
		if n0 >= len(nfr036Script) && n1 >= len(nfr036Script) {
			break
		}
		select {
		case err := <-execErr:
			stop()
			<-leaderDone
			t.Fatalf("scripted executor: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			stop()
			<-leaderDone
			t.Fatalf("the leader did not dispatch the whole script in time: threshold 1 answered %d, threshold 3 answered %d of %d",
				n0, n1, len(nfr036Script))
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-leaderDone
	<-execDone

	rec.mu.Lock()
	for id, l := range rec.log {
		run.dispatched[id] = append([]nfr036Dispatch(nil), l...)
	}
	rec.mu.Unlock()
	return run
}

// assertStatusTimelinesDiffer is the precondition both tests share: the two monitors saw the same
// results and reported different statuses. Without it the comparison is between two identical
// monitors and every invariance assertion passes vacuously.
func (run *nfr036Run) assertStatusTimelinesDiffer(t *testing.T) {
	t.Helper()
	for i, want := range [][]domain.MonitorStatus{nfr036StatusAt1, nfr036StatusAt3} {
		side := run.sides[i]
		got := make([]domain.MonitorStatus, 0, len(nfr036Script))
		for _, o := range run.observed[side.monitorID][:len(nfr036Script)] {
			got = append(got, o.cur)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("PRECONDITION (FR-043 §4.1): %s reported statuses %v for the script, want %v — "+
				"the two monitors must recover at different checks for the invariance above to mean anything",
				side.label, got, want)
		}
	}
}

// TestRecoveryThresholdMovesNoScheduledCheck is I5's scheduler half: for one scripted timeline,
// every check the leader schedules for the threshold-3 monitor is scheduled in the same tick, for
// the same window, on the same carrier and at the same interval as the threshold-1 monitor's —
// although the first is down through checks at which the second is already up again. D4: nothing
// accelerates (or slows) the way up.
func TestRecoveryThresholdMovesNoScheduledCheck(t *testing.T) {
	run := nfr036RunTimeline(t)
	a, b := run.sides[0], run.sides[1]
	da, db := run.dispatched[a.monitorID], run.dispatched[b.monitorID]
	if len(da) < len(nfr036Script) || len(db) < len(nfr036Script) {
		t.Fatalf("dispatches: threshold 1 %d, threshold 3 %d, want at least %d each", len(da), len(db), len(nfr036Script))
	}
	for k := 0; k < len(nfr036Script); k++ {
		x, y := da[k], db[k]
		// The issue instant is minted by ONE statement per tick for every monitor due in it, so
		// equality is "scheduled in the same tick", exactly.
		if !x.issuedAt.Equal(y.issuedAt) {
			t.Errorf("check %d: threshold 1 issued at %s, threshold 3 at %s — not scheduled in the same tick",
				k, x.issuedAt.Format(time.RFC3339Nano), y.issuedAt.Format(time.RFC3339Nano))
		}
		// Window 0 is the schedule row's creation instant, and two monitors cannot be created in
		// one statement; every later window is the leader's own `now + interval`.
		if k > 0 && !x.dueAt.Equal(y.dueAt) {
			t.Errorf("check %d: threshold 1 answers window %s, threshold 3 window %s",
				k, x.dueAt.Format(time.RFC3339Nano), y.dueAt.Format(time.RFC3339Nano))
		}
		if x.dueAt.IsZero() || y.dueAt.IsZero() {
			t.Errorf("check %d carries no window (threshold 1 %s, threshold 3 %s): the ledger carrier did not engage", k, x.dueAt, y.dueAt)
		}
		if x.carrier != y.carrier || x.interval != y.interval {
			t.Errorf("check %d: threshold 1 carrier %d interval %ds, threshold 3 carrier %d interval %ds",
				k, x.carrier, x.interval, y.carrier, y.interval)
		}
	}
	run.assertStatusTimelinesDiffer(t)
}

// TestRecoveryThresholdChangesNoMeasurement is NFR-036's regression (I5 + I6): with the same
// scripted results fed through the scheduler and `RecordScheduledResult`, every measured number
// is the same at threshold 1 and at threshold 3 — the `heartbeats` rows, the expected-run ledger,
// monitor and project SLA, the status-page daily bars, the monitor burn rate, the service
// reliability facts, series and report, and the gate decision.
func TestRecoveryThresholdChangesNoMeasurement(t *testing.T) {
	run := nfr036RunTimeline(t)
	a, b := run.sides[0], run.sides[1]
	// Five minutes, not one: sealing the run's own buckets waits out the late-arrival grace below.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, run.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck // test teardown

	for _, side := range run.sides {
		for k, o := range run.observed[side.monitorID][:len(nfr036Script)] {
			if !o.applied || !o.inserted {
				t.Fatalf("%s: result %d was not applied and inserted (applied=%v inserted=%v reason=%q)",
					side.label, k, o.applied, o.inserted, o.reason)
			}
		}
	}

	// ── heartbeats: every column but the monitor id, row by row ─────────────────────────────
	hbA, hbB := nfr036Heartbeats(t, ctx, conn, a.monitorID), nfr036Heartbeats(t, ctx, conn, b.monitorID)
	if len(hbA) != len(nfr036Script) {
		t.Errorf("threshold 1 wrote %d heartbeats, want %d", len(hbA), len(nfr036Script))
	}
	nfr036Equal(t, "heartbeats", hbA, hbB)
	if len(hbA) == 0 {
		t.Fatal("no heartbeats: nothing below can be compared")
	}
	first, last := hbA[0].ts, hbA[len(hbA)-1].ts

	// ── the expected-run ledger: the scripted windows ───────────────────────────────────────
	erA := nfr036ScriptedWindows(t, a, nfr036ExpectedRuns(t, ctx, conn, a.monitorID))
	erB := nfr036ScriptedWindows(t, b, nfr036ExpectedRuns(t, ctx, conn, b.monitorID))
	if len(erA) != len(erB) {
		t.Errorf("expected-run ledger: threshold 1 holds %d windows up to its last scripted run, threshold 3 holds %d", len(erA), len(erB))
	}
	for k := 0; k < len(erA) && k < len(erB); k++ {
		nfr036Equal(t, fmt.Sprintf("expected-run window %d", k), erA[k], erB[k])
	}

	// ── monitor and project SLA, every standard window ──────────────────────────────────────
	for _, win := range sla.StandardWindows {
		since := time.Now().Add(-win.Duration)
		ma, err := run.st.MonitorSLI(ctx, a.monitorID, since)
		if err != nil {
			t.Fatalf("monitor SLI: %v", err)
		}
		mb, err := run.st.MonitorSLI(ctx, b.monitorID, since)
		if err != nil {
			t.Fatalf("monitor SLI: %v", err)
		}
		nfr036Equal(t, "monitor SLA "+win.Name, ma, mb)
		if ma.Total != int64(len(nfr036Script)) {
			t.Errorf("monitor SLA %s counts %d checks, want %d", win.Name, ma.Total, len(nfr036Script))
		}
		pa, err := run.st.ProjectSLI(ctx, a.projectID, since)
		if err != nil {
			t.Fatalf("project SLI: %v", err)
		}
		pb, err := run.st.ProjectSLI(ctx, b.projectID, since)
		if err != nil {
			t.Fatalf("project SLI: %v", err)
		}
		nfr036Equal(t, "project SLA "+win.Name, pa, pb)
	}

	// ── status-page daily availability bars, from the daily rollup ──────────────────────────
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := run.st.RollupDailyAvailability(ctx, today.AddDate(0, 0, -2), today.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	// The rollup rows themselves: the reads below take today from raw heartbeats, and the rollup is
	// what carries a day once raw retention has dropped it.
	nfr036Equal(t, "daily rollup rows", nfr036DailyRollup(t, ctx, conn, a.monitorID), nfr036DailyRollup(t, ctx, conn, b.monitorID))
	since := today.AddDate(0, 0, -2)
	dA, err := run.st.MonitorDailyAvailability(ctx, a.monitorID, since)
	if err != nil {
		t.Fatalf("monitor daily: %v", err)
	}
	dB, err := run.st.MonitorDailyAvailability(ctx, b.monitorID, since)
	if err != nil {
		t.Fatalf("monitor daily: %v", err)
	}
	nfr036Equal(t, "monitor daily availability", dA, dB)
	pdA, err := run.st.ProjectDailyAvailability(ctx, a.projectID, since)
	if err != nil {
		t.Fatalf("project daily: %v", err)
	}
	pdB, err := run.st.ProjectDailyAvailability(ctx, b.projectID, since)
	if err != nil {
		t.Fatalf("project daily: %v", err)
	}
	nfr036Equal(t, "project daily availability", pdA, pdB)

	// ── monitor burn rate: the evaluation's own numbers and the latches it leaves ───────────
	if _, _, err := run.st.EvaluateBurnAlerts(ctx); err != nil {
		t.Fatalf("burn evaluation: %v", err)
	}
	burnA, burnB := nfr036BurnAlerts(t, ctx, conn, a.monitorID), nfr036BurnAlerts(t, ctx, conn, b.monitorID)
	if len(burnA) == 0 {
		t.Errorf("no burn alert fired for threshold 1: the burn comparison would be vacuous")
	}
	nfr036Equal(t, "burn-rate alerts", burnA, burnB)
	tgA, err := run.st.GetMonitorSLATarget(ctx, a.monitorID, "30d")
	if err != nil {
		t.Fatalf("monitor SLO: %v", err)
	}
	tgB, err := run.st.GetMonitorSLATarget(ctx, b.monitorID, "30d")
	if err != nil {
		t.Fatalf("monitor SLO: %v", err)
	}
	nfr036Equal(t, "burn latches", tgA.BurnRules, tgB.BurnRules)

	// ── service reliability: the facts, the series and the report ───────────────────────────
	from := first.UTC().Truncate(domain.CanonicalBucket)
	to := last.UTC().Truncate(domain.CanonicalBucket).Add(domain.CanonicalBucket)
	// Every bucket of the era is materialized and SEALED through `to`, so the 24h report and the
	// gate decide on the run's own facts rather than withholding (see nfr036Backfill). A bucket
	// seals only once the database clock is past its end plus the late-arrival grace
	// (`materializeBucketTx`), so the backfilled part goes first, while the run's own buckets are
	// still open, and the whole era again once the clock has passed `to + LateArrivalGrace`: the
	// second pass seals the run's buckets and the few backfilled ones that were still inside the
	// grace on the first (a sealed bucket is immutable to it). The wait is two to three minutes,
	// which is the price of a real decision.
	for _, side := range run.sides {
		if from.After(run.era) {
			if _, err := run.st.MaterializeServiceRange(ctx, side.projectID, side.serviceID, run.era, from); err != nil {
				t.Fatalf("%s: materialize the backfilled era: %v", side.label, err)
			}
		}
	}
	nfr036AwaitDBClock(t, ctx, conn, to.Add(domain.LateArrivalGrace))
	for _, side := range run.sides {
		if _, err := run.st.MaterializeServiceRange(ctx, side.projectID, side.serviceID, run.era, to); err != nil {
			t.Fatalf("%s: materialize: %v", side.label, err)
		}
		var sealedThrough *time.Time
		if err := conn.QueryRow(ctx, `SELECT sealed_through FROM service_materialization WHERE service_id = $1`,
			side.serviceID).Scan(&sealedThrough); err != nil {
			t.Fatalf("%s: watermark: %v", side.label, err)
		}
		if sealedThrough == nil || !sealedThrough.Equal(to) {
			t.Errorf("PRECONDITION: %s sealed through %v, want %s — the report and the gate would not read the run's facts",
				side.label, sealedThrough, to.Format(time.RFC3339))
		}
	}
	bkA := nfr036Buckets(t, ctx, conn, a, from, to)
	bkB := nfr036Buckets(t, ctx, conn, b, from, to)
	measured := false
	for _, bk := range bkA {
		measured = measured || bk.bad > 0
	}
	if !measured {
		t.Errorf("no service bucket of threshold 1 measured a bad interval: the service comparison would be vacuous (%+v)", bkA)
	}
	nfr036Equal(t, "service reliability buckets", bkA, bkB)
	hour := from.Truncate(time.Hour)
	srA, err := run.st.ServiceReliabilitySeries(ctx, a.projectID, a.serviceID, hour.Add(-time.Hour), hour.Add(2*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	srB, err := run.st.ServiceReliabilitySeries(ctx, b.projectID, b.serviceID, hour.Add(-time.Hour), hour.Add(2*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	nfr036EqualJSON(t, "service reliability series", run, srA, srB)
	day, _ := sla.WindowByName("24h")
	rpA, err := run.st.ServiceReliabilityReport(ctx, a.projectID, a.serviceID, day)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	rpB, err := run.st.ServiceReliabilityReport(ctx, b.projectID, b.serviceID, day)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	nfr036EqualJSON(t, "service reliability report", run, rpA, rpB)

	// ── the gate decision ───────────────────────────────────────────────────────────────────
	gA, err := run.st.DecideGate(ctx, a.projectID, a.serviceID, 5*time.Second)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	gB, err := run.st.DecideGate(ctx, b.projectID, b.serviceID, 5*time.Second)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	// PRECONDITION: a real decision on both sides. UNKNOWN on both would compare two withheld
	// budgets and prove only that both were withheld.
	for i, g := range []domain.GateDecision{gA, gB} {
		if g.State == domain.GateStateUnknown || g.State == domain.GateStateNotConfigured {
			t.Errorf("PRECONDITION: %s gate state is %s (reasons %+v) — the comparison below would be vacuous",
				run.sides[i].label, g.State, g.Reasons)
		}
	}
	for i, rp := range []domain.ServiceWindowReport{rpA, rpB} {
		if rp.Budget == nil {
			t.Errorf("PRECONDITION: %s 24h report withholds the budget (status %s, reason %s)",
				run.sides[i].label, rp.Status, rp.Reason)
		}
	}
	nfr036EqualJSON(t, "gate decision", run, gA, gB)

	run.assertStatusTimelinesDiffer(t)
}

// nfr036AwaitDBClock waits until the database clock — the one the seal decision reads — reaches at.
func nfr036AwaitDBClock(t *testing.T, ctx context.Context, conn *pgx.Conn, at time.Time) {
	t.Helper()
	for {
		var now time.Time
		if err := conn.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Fatalf("database clock: %v", err)
		}
		if !now.Before(at) {
			return
		}
		wait := at.Sub(now)
		if wait > time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			t.Fatalf("database clock did not reach %s: %v", at.Format(time.RFC3339), ctx.Err())
		case <-time.After(wait + 10*time.Millisecond):
		}
	}
}

// ── readers ──────────────────────────────────────────────────────────────────────────────────

type nfr036Heartbeat struct {
	ts, observedAt time.Time
	up             bool
	latency        *int64
	code           *int32
	msg            *string
}

func nfr036Heartbeats(t *testing.T, ctx context.Context, conn *pgx.Conn, monitorID string) []nfr036Heartbeat {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT ts, observed_at, up, latency_ms, code, msg
		  FROM heartbeats WHERE monitor_id = $1 ORDER BY ts`, monitorID)
	if err != nil {
		t.Fatalf("heartbeats: %v", err)
	}
	defer rows.Close()
	var out []nfr036Heartbeat
	for rows.Next() {
		var h nfr036Heartbeat
		var observed *time.Time
		if err := rows.Scan(&h.ts, &observed, &h.up, &h.latency, &h.code, &h.msg); err != nil {
			t.Fatalf("scan heartbeat: %v", err)
		}
		if observed != nil {
			h.observedAt = *observed
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("heartbeats: %v", err)
	}
	return out
}

type nfr036ExpectedRun struct {
	dueAt                                      time.Time
	hasJob                                     bool
	revision                                   int64
	region                                     string
	carrier                                    *int32
	interval                                   *int32
	intervalAssumed                            bool
	issuedAt, claimedAt, terminalAt, refusedAt *time.Time
	reservedAt                                 *time.Time
	outcome, refusedReason, skip, withheld     string
}

func nfr036ExpectedRuns(t *testing.T, ctx context.Context, conn *pgx.Conn, monitorID string) []nfr036ExpectedRun {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT due_at, job_id IS NOT NULL, execution_revision, region, carrier_generation,
		       interval_seconds, interval_assumed, issued_at, claimed_at, terminal_at, refused_at,
		       reserved_at, COALESCE(outcome, ''), COALESCE(refused_reason, ''),
		       COALESCE(skip_reason, ''), COALESCE(withheld_reason, '')
		  FROM expected_runs WHERE monitor_id = $1 ORDER BY due_at`, monitorID)
	if err != nil {
		t.Fatalf("expected runs: %v", err)
	}
	defer rows.Close()
	var out []nfr036ExpectedRun
	for rows.Next() {
		var e nfr036ExpectedRun
		if err := rows.Scan(&e.dueAt, &e.hasJob, &e.revision, &e.region, &e.carrier,
			&e.interval, &e.intervalAssumed, &e.issuedAt, &e.claimedAt, &e.terminalAt, &e.refusedAt,
			&e.reservedAt, &e.outcome, &e.refusedReason, &e.skip, &e.withheld); err != nil {
			t.Fatalf("scan expected run: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("expected runs: %v", err)
	}
	return out
}

type nfr036Daily struct {
	day       time.Time
	up, total int64
}

func nfr036DailyRollup(t *testing.T, ctx context.Context, conn *pgx.Conn, monitorID string) []nfr036Daily {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT day, up, total FROM heartbeats_daily WHERE monitor_id = $1 ORDER BY day`, monitorID)
	if err != nil {
		t.Fatalf("daily rollup: %v", err)
	}
	defer rows.Close()
	var out []nfr036Daily
	for rows.Next() {
		var d nfr036Daily
		if err := rows.Scan(&d.day, &d.up, &d.total); err != nil {
			t.Fatalf("scan daily rollup: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("daily rollup: %v", err)
	}
	if len(out) == 0 {
		t.Errorf("monitor %s has no daily rollup row: the rollup comparison would be vacuous", monitorID)
	}
	return out
}

// nfr036ScriptedWindows returns the side's windows up to and including the one its last scripted
// result answered, with the instants that are not the leader's normalized.
//
//   - A due instant earlier than the leader's first reservation was minted when the monitor was
//     created, and two monitors cannot be created in one statement; it is compared by presence.
//     Every later due instant is the leader's own `now + interval` and is compared exactly.
//   - `reserved_at` is the identity instant ONE statement mints for every monitor due in a tick
//     (`LoadDueExpectations`), so it is compared exactly, unnormalized.
//   - `claimed_at` (the executor's instant, `ClaimHeartbeat`) and `terminal_at` (the moment the
//     result was recorded) are taken one monitor after the other within the same tick. Both are
//     compared by presence and must fall within a few seconds after the tick's reservation; on a
//     window a scripted result filled `claimed_at` must be present, because the scripted
//     executor claims every job it answers.
//   - `issued_at` is the moment each job's publish returned. `Scheduler.publishPending` reads it
//     after every `PublishJob` of the tick and hands the batch to `Store.ConfirmExpectations`,
//     the ONE writer of the column (`confirmExpectationsSQL`), once all of the tick's publishes
//     are done. The scripted executor answers from the in-process queue as soon as a job is
//     published, so its `RecordScheduledResult` usually commits BEFORE that confirm — the same
//     order the in-process worker pool of `role=all` produces — and `fillExpectedRunTerminalSQL`
//     leaves `issued_at` alone on a reserved row (its 27h note), so the later confirm still
//     lands. It must therefore be NON-NULL on every window a scripted result filled, on both
//     sides: a NULL there would mean the harness (or the scheduler) skipped the confirm, and a
//     presence comparison alone would let NULL on both sides pass. It is bounded as tightly as
//     the two sides allow: at or after its own reservation and strictly before the side's next
//     reservation — the leader loop is sequential, so a job of tick N is published and confirmed
//     before tick N+1 mints anything. The reservations are compared exactly across the sides, so
//     both sides' issue instants fall in the same half-open tick interval. Within it they differ
//     by the microseconds between two publishes, so the value itself is then normalized to a
//     presence marker. (A failure dump prints that marker, `time.Unix(0, 0)`, as
//     `{0 62135596800 <nil>}` because the fields are unexported; it is NOT a NULL — a NULL prints
//     as `nil`.)
//
// Windows after the last scripted one exist only because the leader is stopped asynchronously.
func nfr036ScriptedWindows(t *testing.T, side nfr036Side, rows []nfr036ExpectedRun) []nfr036ExpectedRun {
	t.Helper()
	var firstReserved *time.Time
	for _, r := range rows {
		if r.reservedAt != nil {
			firstReserved = r.reservedAt
			break
		}
	}
	if firstReserved == nil {
		t.Fatalf("%s: no window was reserved by the leader", side.label)
	}
	bounded := func(k int, what string, at *time.Time, reserved *time.Time) {
		if at == nil {
			return
		}
		if reserved == nil || at.Before(*reserved) || at.Sub(*reserved) > 5*time.Second {
			t.Errorf("%s window %d: %s %v is not within the tick that reserved it (%v)", side.label, k, what, at, reserved)
		}
	}
	// nextReserved is the side's first reservation after row k's: the start of the next tick that
	// reserved anything for this monitor. nil when the leader stopped before one.
	nextReserved := func(k int) *time.Time {
		for _, r := range rows[k+1:] {
			if r.reservedAt != nil && r.reservedAt.After(*rows[k].reservedAt) {
				return r.reservedAt
			}
		}
		return nil
	}
	present := time.Unix(0, 0).UTC()
	var out []nfr036ExpectedRun
	results := 0
	for k, r := range rows {
		if results == len(nfr036Script) {
			break
		}
		if r.outcome == "result" {
			results++
		}
		if r.dueAt.Before(*firstReserved) {
			r.dueAt = present
		}
		if r.outcome == "result" && r.claimedAt == nil {
			t.Errorf("%s window %d (due %s): claimed_at is NULL on a window a scripted result filled — "+
				"the claim (Store.RecordRunClaim) never landed", side.label, k, r.dueAt.Format(time.RFC3339Nano))
		}
		if r.outcome == "result" && r.reservedAt != nil {
			switch next := nextReserved(k); {
			case r.issuedAt == nil:
				t.Errorf("%s window %d (due %s): issued_at is NULL on a window a scripted result filled — "+
					"the confirm after the publish (Store.ConfirmExpectations) never landed",
					side.label, k, r.dueAt.Format(time.RFC3339Nano))
			case r.issuedAt.Before(*r.reservedAt):
				t.Errorf("%s window %d: issued_at %s precedes its own reservation %s",
					side.label, k, r.issuedAt.Format(time.RFC3339Nano), r.reservedAt.Format(time.RFC3339Nano))
			case next != nil && !r.issuedAt.Before(*next):
				t.Errorf("%s window %d: issued_at %s is not before the next tick's reservation %s — "+
					"the job was not published within the tick that reserved it",
					side.label, k, r.issuedAt.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
			}
		}
		bounded(k, "issued_at", r.issuedAt, r.reservedAt)
		bounded(k, "claimed_at", r.claimedAt, r.reservedAt)
		bounded(k, "terminal_at", r.terminalAt, r.reservedAt)
		if r.issuedAt != nil {
			r.issuedAt = &present
		}
		if r.claimedAt != nil {
			r.claimedAt = &present
		}
		if r.terminalAt != nil {
			r.terminalAt = &present
		}
		out = append(out, r)
	}
	if results != len(nfr036Script) {
		t.Errorf("%s: %d windows filled by a result, want %d", side.label, results, len(nfr036Script))
	}
	return out
}

// nfr036BurnAlerts returns the monitor's burn-alert payloads with the monitor id blanked, in a
// stable order.
func nfr036BurnAlerts(t *testing.T, ctx context.Context, conn *pgx.Conn, monitorID string) []domain.SLOBurnAlert {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT payload FROM outbox_events
		 WHERE topic = $1 AND payload->>'monitor_id' = $2`, domain.TopicSLOBurnAlert, monitorID)
	if err != nil {
		t.Fatalf("burn alerts: %v", err)
	}
	defer rows.Close()
	var out []domain.SLOBurnAlert
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan burn alert: %v", err)
		}
		var a domain.SLOBurnAlert
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("decode burn alert: %v", err)
		}
		a.MonitorID = ""
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("burn alerts: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WindowSeconds != out[j].WindowSeconds {
			return out[i].WindowSeconds < out[j].WindowSeconds
		}
		return out[i].Severity < out[j].Severity
	})
	return out
}

type nfr036Bucket struct {
	start                                   time.Time
	size, good, bad, unknown, excluded      int64
	healthy, degraded, down, healthUnknown  int64
	state                                   string
	sealedGeneration, maintenanceGeneration *int64
	epoch                                   string
	provenance                              string
}

func nfr036Buckets(t *testing.T, ctx context.Context, conn *pgx.Conn, side nfr036Side, from, to time.Time) []nfr036Bucket {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT bucket_start, bucket_size_us, good_us, bad_us, unknown_us, excluded_us,
		       healthy_us, degraded_us, down_us, health_unknown_us, state,
		       sealed_ingest_generation, maintenance_generation, epoch_id::text,
		       COALESCE(provenance::text, '')
		  FROM service_reliability_buckets
		 WHERE service_id = $1 AND bucket_start >= $2 AND bucket_start < $3
		 ORDER BY bucket_start`, side.serviceID, from, to)
	if err != nil {
		t.Fatalf("service buckets: %v", err)
	}
	defer rows.Close()
	var out []nfr036Bucket
	for rows.Next() {
		var b nfr036Bucket
		if err := rows.Scan(&b.start, &b.size, &b.good, &b.bad, &b.unknown, &b.excluded,
			&b.healthy, &b.degraded, &b.down, &b.healthUnknown, &b.state,
			&b.sealedGeneration, &b.maintenanceGeneration, &b.epoch, &b.provenance); err != nil {
			t.Fatalf("scan service bucket: %v", err)
		}
		b.epoch = side.anonymize(b.epoch)
		b.provenance = side.anonymize(b.provenance)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("service buckets: %v", err)
	}
	return out
}

// ── comparison ───────────────────────────────────────────────────────────────────────────────

var nfr036UUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// anonymize replaces this side's own identifiers with their role, and any other identifier with
// a placeholder, so two sides compare on what they say rather than on which rows they are. The
// gate's fact-revision digest is the SHA-256 of the sorted revision ids, so with one revision it
// is an identifier too.
func (s nfr036Side) anonymize(v string) string {
	digest := sha256.Sum256([]byte(s.revisionID))
	for id, role := range map[string]string{
		s.projectID: "<project>", s.monitorID: "<monitor>", s.serviceID: "<service>",
		s.revisionID: "<revision>", s.epochID: "<epoch>",
		hex.EncodeToString(digest[:]): "<digest of revision>",
	} {
		v = strings.ReplaceAll(v, id, role)
	}
	return nfr036UUID.ReplaceAllString(v, "<id>")
}

// nfr036Volatile are the keys stamped with a clock that is not the measured timeline: a read's
// own instant and identity (as_of, evaluated_at, decision_id), the gate's seal lag, which is
// evaluated_at minus sealed_through, and the instant the setup wrote the service SLO.
var nfr036Volatile = map[string]bool{
	"as_of": true, "evaluated_at": true, "decision_id": true,
	"seal_lag": true, "objective_updated_at": true,
}

func nfr036Strip(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if nfr036Volatile[k] {
				delete(x, k)
				continue
			}
			x[k] = nfr036Strip(val)
		}
	case []any:
		for i := range x {
			x[i] = nfr036Strip(x[i])
		}
	}
	return v
}

func nfr036Canonical(t *testing.T, side nfr036Side, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var tree any
	if err := json.Unmarshal([]byte(side.anonymize(string(raw))), &tree); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.MarshalIndent(nfr036Strip(tree), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

func nfr036EqualJSON(t *testing.T, what string, run *nfr036Run, a, b any) {
	t.Helper()
	ca, cb := nfr036Canonical(t, run.sides[0], a), nfr036Canonical(t, run.sides[1], b)
	if ca != cb {
		t.Errorf("%s differs between threshold 1 and threshold 3 (NFR-036):\nthreshold 1: %s\nthreshold 3: %s", what, ca, cb)
	}
}

func nfr036Equal(t *testing.T, what string, a, b any) {
	t.Helper()
	if !reflect.DeepEqual(a, b) {
		t.Errorf("%s differs between threshold 1 and threshold 3 (NFR-036):\nthreshold 1: %s\nthreshold 3: %s",
			what, nfr036Dump(a), nfr036Dump(b))
	}
}

func nfr036Dump(v any) string { return fmt.Sprintf("%+v", nfr036Deref(v)) }

// nfr036Deref prints pointers by value, so a difference is readable rather than two addresses.
func nfr036Deref(v any) any {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = nfr036Deref(rv.Index(i).Interface())
		}
		return out
	case reflect.Struct:
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Field(i)
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(rv.Type().Field(i).Name + ":")
			if f.Kind() == reflect.Pointer {
				if f.IsNil() {
					b.WriteString("nil")
				} else {
					fmt.Fprintf(&b, "%v", f.Elem())
				}
				continue
			}
			fmt.Fprintf(&b, "%v", f)
		}
		b.WriteString("}")
		return b.String()
	}
	return v
}
