// Package ingest consumes check results from the dispatcher, persists them as
// heartbeats, updates each monitor's last-known status, and opens/resolves
// auto-incidents on down/up transitions.
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/events"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Store is the persistence surface the ingester needs.
type Store interface {
	// RecordScheduledResult records a scheduled (worker/agent) probe result in one
	// transaction following the ordered ingest pipeline (missing → lock → revision gate →
	// timestamp bounds → insert/dedup → watermark). The returned ResultOutcome says whether
	// live state was applied, whether a heartbeat was inserted (SLA), the prev/new status,
	// maintenance suppression, and — when not applied — the outcome reason for metrics.
	RecordScheduledResult(ctx context.Context, hb domain.Heartbeat) (store.ResultOutcome, error)
	// RecordProbeError stores a revision-fenced executor diagnostic. It takes the whole
	// heartbeat rather than three fields of it because a probe_error is a TERMINAL outcome for
	// the due window the result answers (FR-032 §6.1), and the window's identity — DueAt beside
	// JobID and JobIssuedAt — travels on the result exactly as it does for an ordinary one.
	RecordProbeError(ctx context.Context, hb domain.Heartbeat) (store.ProbeErrorOutcome, error)
	// RecordRunClaim fills `claimed_at` on the window this claim answers (FR-032 §8.4). It is
	// one idempotent statement over one row and takes no transaction: unlike a terminal, a claim
	// accompanies nothing that must land with it. It reports whether a window TOOK the claim, so
	// one that arrived before its window was committed can be re-offered rather than dropped.
	RecordRunClaim(ctx context.Context, hb domain.Heartbeat) (bool, error)
	GetMonitor(ctx context.Context, id string) (domain.Monitor, error)
	FindOpenAutoIncidentByMonitor(ctx context.Context, monitorID string) (domain.Incident, error)
	CreateIncidentBySystem(ctx context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error)
	// The SYSTEM door: the reconciler is a machine writer, so it takes no actor and writes no
	// audit row. The name says which door it is, which is the point of the split (FR-026 D3).
	AddIncidentUpdateBySystem(ctx context.Context, upd domain.IncidentUpdate) (domain.IncidentUpdate, error)
}

// Recorder records per-check metrics.
type Recorder interface {
	RecordCheck(up bool)
	RecordIncidentOpened()
	// RecordResultOutcome reports a non-applied result outcome (quarantined/ignored/
	// rejected) by its reason; an applied or benign-duplicate outcome is a no-op.
	RecordResultOutcome(reason string)
	// RecordResultMissingRevision counts a scheduled result accepted with no revision under
	// observe mode (the migration signal watched before switching to enforce).
	RecordResultMissingRevision()
	RecordExecutorProbeError(reason string)
}

// autoIncidentAuthor labels timeline entries the pipeline writes.
const autoIncidentAuthor = "auto"

// autoIncidentOpen{Attempts,Backoff} bound the retry of a failed auto-incident open
// for a monitor whose alerting depends on it (escalation ladder pages over it). Small
// and off the hot heartbeat path — only reached on a transition, and only loops on error.
const (
	autoIncidentOpenAttempts = 3
	autoIncidentOpenBackoff  = 250 * time.Millisecond
)

// sleepCtx waits for d or ctx cancellation; returns false if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Consumer reads results and writes them through the store. Outbound delivery
// (webhooks, notifications) is not done here: the store enqueues those events in
// the same transaction as the incident create/update and the status transition,
// and the outbox worker delivers them.
// Publisher receives live status-change events for SSE fan-out. Optional.
type Publisher interface {
	Publish(events.Event)
}

// Run-claim retry bounds (FR-032 §8.4).
//
// RESTATED against a re-measurement (audit-gap package 3, item C4). What stood here described an
// ordering phase F inverted, and the limit below was sized from it.
//
// The old text: the window is written by §7.1's advance at the END of the leader's tick, so on a
// fast transport the claim ARRIVES FIRST — "a running `role=all` instance recorded one claim across
// sixty-two generation-4 windows". That was true when the advance ran AFTER the publish. Phase F
// (§7.4, D-0243) moved the reserve BEFORE it: the window is committed before the job leaves the
// process, so a claim cannot outrun the row it answers.
//
// Re-measured on a `role=all` dev instance carrying 20052 generation-4 issued windows across a full
// day: 20050 of them hold a `claimed_at` — 99.99%, against the 1.6% the old note recorded. The two
// that do not were never reserved at all (`reserved_at IS NULL`): they are §8.3 ADOPTIONS, windows
// a terminal event created for a run the leader had not recorded, and no retry could have matched
// them because there was no row to match at any point.
//
// So the bound is no longer sized from a race, and the honest statement is that under phase F's
// ordering a re-offer cannot convert an unmatched claim into a matched one: every reachable
// unmatched shape — an adoption, a job id or revision the window does not carry, a window past the
// correlation floor — is unmatched for a reason the passage of two seconds does not change. The
// mechanism is kept because removing it is a behaviour change beyond a re-measurement, and it costs
// at most three further store calls for a message the system already treats as the least important
// one it carries. Its removal is worth deciding on its own terms rather than folding into this.
//
// Past the budget the claim is dropped in silence — it is a diagnostic, a terminal always outranks
// a missing one, and a claim that correlates to no window at all reaches the same end without a
// special case.
const (
	runClaimRetryEvery = 2 * time.Second
	// runClaimRetryLimit counts RE-offers, not offers: a claim is tried once when it arrives and
	// at most this many times again, so four store calls in the worst case. The number is NOT
	// derived from a measured race any more — see the re-measurement above — it is the ceiling on
	// what an unmatched diagnostic is allowed to cost.
	runClaimRetryLimit = 3
	// runClaimRetryMax bounds the memory this can hold — one tick's worth of claims for a very
	// large instance, past which the OLDEST are dropped. A ring rather than unbounded growth,
	// because the thing being buffered is the least important message in the system.
	runClaimRetryMax = 8192
)

// pendingRunClaim is one claim waiting for its window to be committed.
type pendingRunClaim struct {
	hb       domain.Heartbeat
	attempts int
	next     time.Time
}

type Consumer struct {
	store      Store
	dispatcher dispatch.Dispatcher
	recorder   Recorder
	reconciler *Reconciler
	logger     *slog.Logger
	// now is injectable so the claim-retry schedule can be driven by a test without waiting for
	// wall-clock seconds. Nil means time.Now.
	now func() time.Time
	// claimRetry is owned by Run's goroutine alone — handle and the retry sweep both run on it —
	// so it needs no lock.
	claimRetry []pendingRunClaim
}

// New builds a results consumer. Its post-commit reconciler shares the store/recorder;
// attach an events publisher with WithEvents.
func New(store Store, dispatcher dispatch.Dispatcher, recorder Recorder, logger *slog.Logger) *Consumer {
	return &Consumer{
		store:      store,
		dispatcher: dispatcher,
		recorder:   recorder,
		reconciler: NewReconciler(store, nil, recorder, logger),
		logger:     logger,
	}
}

// WithEvents attaches a realtime publisher for status changes. Optional and nil-safe.
func (c *Consumer) WithEvents(p Publisher) *Consumer {
	c.reconciler.WithEvents(p)
	return c
}

// Run blocks until ctx is cancelled, persisting each result heartbeat.
//
// The ticker is the run-claim retry sweep and nothing else. It shares this goroutine with result
// handling on purpose: the retry buffer is then owned by one goroutine and needs no lock, and a
// sweep that fell behind because results were arriving is a sweep that was busy doing the more
// important thing.
func (c *Consumer) Run(ctx context.Context) {
	results := c.dispatcher.Results()
	sweep := time.NewTicker(runClaimRetryEvery)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			c.retryRunClaims(ctx)
		case hb, ok := <-results:
			if !ok {
				return
			}
			c.handle(ctx, hb)
		}
	}
}

// clock is time.Now unless a test injected one.
func (c *Consumer) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Consumer) handle(ctx context.Context, hb domain.Heartbeat) {
	if hb.ProbeError != nil {
		c.handleProbeError(ctx, hb)
		return
	}
	// FR-032 §8.4: a claim is a statement that an executor took this job off the transport and is
	// about to probe. It is NOT a result — no heartbeat row, no status, no SLA, no incident — so
	// it branches out here beside the probe-error member and before any of that begins.
	if hb.Claim != nil {
		c.handleClaim(ctx, hb)
		return
	}
	// One transaction runs the ordered pipeline (missing → lock → revision gate → bounds →
	// insert/dedup → watermark): a duplicate re-delivery is deduped, a stale/out-of-order
	// probe is kept for SLA only, a future/out-of-window one is quarantined without an
	// insert, and a crash can't leave a status change without its heartbeat.
	o, err := c.store.RecordScheduledResult(ctx, hb)
	if errors.Is(err, store.ErrNotFound) {
		// The monitor was deleted while the probe was in flight; the scheduler drops
		// it on its next snapshot refresh.
		c.logger.Info("result_for_deleted_monitor", "monitor_id", hb.MonitorID)
		return
	}
	if err != nil {
		c.logger.Error("record_result_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if c.recorder != nil {
		if o.Reason != "" {
			c.recorder.RecordResultOutcome(o.Reason)
		}
		if o.MissingRevisionObserved {
			c.recorder.RecordResultMissingRevision()
		}
		if o.Inserted {
			c.recorder.RecordCheck(hb.Up) // count a real check only when a heartbeat was recorded
		}
	}
	if o.Reason == ReasonFutureTimestamp {
		// A future-beyond-skew scheduled result signals a broken worker clock; the
		// aggregate metric can't identify it, so log (rate-limiting is a P2 refinement).
		c.logger.Warn("result_quarantined", "monitor_id", hb.MonitorID, "reason", o.Reason, "ts", hb.Ts)
	}
	if o.Applied && o.Prev != o.Cur {
		c.logger.Info("monitor_status_changed", "monitor_id", hb.MonitorID, "prev", string(o.Prev), "cur", string(o.Cur), "suppressed", o.Suppressed)
		c.reconciler.Reconcile(ctx, hb, o.Prev, o.Cur, o.Suppressed)
	}
}

// handleClaim records the claim and nothing else.
//
// An ERROR is logged and dropped rather than retried, and the direction is the one this design
// accepts everywhere: a missing claim reads `issued_never_claimed`, which is WITHHOLDING — it says
// the ledger cannot witness that the run started, never that the run did not happen. A terminal
// outcome always outranks a missing claim (invariant 10), so the loss costs a diagnostic and never
// a coverage verdict.
//
// A claim that no window TOOK is a different case and is held, but NOT because it raced its window:
// phase F reserves before publishing, so a ledgered job cannot leave the process until the row it
// answers is committed — `TestNoJobIsPublishedBeforeItsWindowIsRecorded` pins that ordering. It is
// held because the retry mechanism is kept — see the retry bounds above, where the re-measurement
// and the reason for keeping it are recorded.
func (c *Consumer) handleClaim(ctx context.Context, hb domain.Heartbeat) {
	matched, err := c.store.RecordRunClaim(ctx, hb)
	if err != nil {
		c.logger.Warn("record_run_claim_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if !matched {
		c.deferRunClaim(hb, 1)
	}
}

// deferRunClaim parks an unmatched claim.
//
// Not "a claim whose window is not committed yet": under phase F's ordering that state is
// unreachable. Every shape that reaches here is unmatched for a reason a re-offer does not change.
//
// The ring drops the OLDEST when full rather than refusing the newest: a claim's value is its
// timing, so the one still worth landing is the one that just arrived.
func (c *Consumer) deferRunClaim(hb domain.Heartbeat, attempts int) {
	if attempts > runClaimRetryLimit {
		return
	}
	if len(c.claimRetry) >= runClaimRetryMax {
		c.claimRetry = c.claimRetry[1:]
	}
	c.claimRetry = append(c.claimRetry, pendingRunClaim{
		hb: hb, attempts: attempts, next: c.clock().Add(runClaimRetryEvery),
	})
}

// retryRunClaims re-offers every parked claim whose wait has elapsed.
//
// The claim's own instant is untouched, which is the point of retrying at all: `claimed_at` records
// when the executor STARTED, and the merge rule is "earliest wins", so a claim landing three
// seconds late still writes the instant it was taken at.
func (c *Consumer) retryRunClaims(ctx context.Context) {
	if len(c.claimRetry) == 0 {
		return
	}
	now := c.clock()
	// A FRESH slice rather than the filter-in-place idiom (`keep := c.claimRetry[:0]`). That form
	// writes retained entries over the head of the same backing array, so the early return on a
	// cancelled context would have left `c.claimRetry` at its old LENGTH with its head already
	// rewritten — a prefix of survivors followed by entries the sweep had passed. Harmless in
	// practice, because the only caller returns immediately afterwards, but it is a slice whose
	// contents depend on where a loop stopped, and one allocation per non-empty sweep is not a
	// price worth the subtlety.
	keep := make([]pendingRunClaim, 0, len(c.claimRetry))
	for i, p := range c.claimRetry {
		if ctx.Err() != nil {
			// The rest have not been offered this round, so they are carried unchanged: a
			// shutdown must not consume a claim's attempt budget.
			keep = append(keep, c.claimRetry[i:]...)
			break
		}
		if now.Before(p.next) {
			keep = append(keep, p)
			continue
		}
		matched, err := c.store.RecordRunClaim(ctx, p.hb)
		if err != nil {
			c.logger.Warn("record_run_claim_failed", "monitor_id", p.hb.MonitorID, "error", err.Error())
			continue
		}
		if matched || p.attempts >= runClaimRetryLimit {
			continue
		}
		p.attempts++
		p.next = now.Add(runClaimRetryEvery)
		keep = append(keep, p)
	}
	c.claimRetry = keep
}

func (c *Consumer) handleProbeError(ctx context.Context, hb domain.Heartbeat) {
	o, err := c.store.RecordProbeError(ctx, hb)
	if errors.Is(err, store.ErrNotFound) {
		c.logger.Info("probe_error_for_deleted_monitor", "monitor_id", hb.MonitorID)
		return
	}
	if err != nil {
		c.logger.Error("record_probe_error_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if o.Reason != "" {
		if c.recorder != nil {
			c.recorder.RecordResultOutcome(o.Reason)
		}
		return
	}
	if o.Recorded {
		if c.recorder != nil {
			c.recorder.RecordExecutorProbeError(hb.ProbeError.Reason)
		}
		c.logger.Warn("executor_probe_error", "monitor_id", hb.MonitorID, "reason", hb.ProbeError.Reason)
	}
}

// ReasonFutureTimestamp mirrors store.ReasonFutureTimestamp for the quarantine log branch.
const ReasonFutureTimestamp = store.ReasonFutureTimestamp
