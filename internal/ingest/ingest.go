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
	RecordProbeError(ctx context.Context, monitorID string, revision int64, probeErr domain.ProbeError) (store.ProbeErrorOutcome, error)
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

type Consumer struct {
	store      Store
	dispatcher dispatch.Dispatcher
	recorder   Recorder
	reconciler *Reconciler
	logger     *slog.Logger
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
func (c *Consumer) Run(ctx context.Context) {
	results := c.dispatcher.Results()
	for {
		select {
		case <-ctx.Done():
			return
		case hb, ok := <-results:
			if !ok {
				return
			}
			c.handle(ctx, hb)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, hb domain.Heartbeat) {
	if hb.ProbeError != nil {
		c.handleProbeError(ctx, hb)
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

func (c *Consumer) handleProbeError(ctx context.Context, hb domain.Heartbeat) {
	o, err := c.store.RecordProbeError(ctx, hb.MonitorID, hb.ExecutionRevision, *hb.ProbeError)
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
