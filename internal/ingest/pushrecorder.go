package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// PushStore is the store surface a PushRecorder needs.
type PushStore interface {
	RecordPushResult(ctx context.Context, monitorID string, up bool, msg string, receivedAt, observedAt time.Time) (store.ResultOutcome, error)
}

// PushRecorder applies a push heartbeat through its dedicated store entrypoint and runs
// the SAME post-commit reconciliation (SSE event, auto-incident open/resolve) as scheduled
// ingest — so a direct push does not bypass incidents/events. It does NOT go through the
// dispatcher/ResultSink: origin stays a trusted server-side call, and application is durable
// within the request.
type PushRecorder struct {
	store      PushStore
	reconciler *Reconciler
	recorder   Recorder
	logger     *slog.Logger
}

// NewPushRecorder builds a push recorder sharing the reconciler + recorder.
func NewPushRecorder(store PushStore, reconciler *Reconciler, recorder Recorder, logger *slog.Logger) *PushRecorder {
	return &PushRecorder{store: store, reconciler: reconciler, recorder: recorder, logger: logger}
}

// Record applies one push ping. receivedAt is the trusted ingress DB clock captured at the
// token lookup; observedAt is the optional raw client timestamp (zero = absent).
func (p *PushRecorder) Record(ctx context.Context, monitorID string, up bool, msg string, receivedAt, observedAt time.Time) {
	o, err := p.store.RecordPushResult(ctx, monitorID, up, msg, receivedAt, observedAt)
	if errors.Is(err, store.ErrNotFound) {
		p.logger.Info("push_result_for_deleted_monitor", "monitor_id", monitorID)
		return
	}
	if err != nil {
		p.logger.Error("record_push_result_failed", "monitor_id", monitorID, "error", err.Error())
		return
	}
	if !o.Applied && !o.Inserted && o.Reason == "" {
		// Dropped by the current-state re-check: disabled/retyped between lookup and apply.
		p.logger.Info("push_result_dropped", "monitor_id", monitorID)
		return
	}
	if p.recorder != nil {
		if o.Reason != "" {
			p.recorder.RecordResultOutcome(o.Reason)
		}
		if o.Inserted {
			p.recorder.RecordCheck(up)
		}
	}
	if o.Applied && o.Prev != o.Cur {
		hb := domain.Heartbeat{MonitorID: monitorID, Up: up, Msg: msg, Ts: receivedAt}
		p.reconciler.Reconcile(ctx, hb, o.Prev, o.Cur, o.Suppressed)
	}
}
