package ingest

import (
	"context"
	"errors"
	"log/slog"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/events"
	"github.com/teamlead-com/cerbix/internal/store"
)

// ReconcileStore is the subset a post-commit reconciler needs.
type ReconcileStore interface {
	GetMonitor(ctx context.Context, id string) (domain.Monitor, error)
	FindOpenAutoIncidentByMonitor(ctx context.Context, monitorID string) (domain.Incident, error)
	CreateIncident(ctx context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error)
	AddIncidentUpdate(ctx context.Context, upd domain.IncidentUpdate) (domain.IncidentUpdate, error)
}

// Reconciler runs the shared post-commit flow for an APPLIED status transition, from ANY
// live-state origin (scheduled ingest, push, dead-man): publish an SSE event and open/
// resolve the auto-incident. Extracted so every origin runs identical post-commit logic
// instead of each re-implementing it. Best-effort: failures are logged, never fatal.
type Reconciler struct {
	store    ReconcileStore
	events   Publisher
	recorder Recorder
	logger   *slog.Logger
}

// NewReconciler builds a reconciler. events may be nil (no SSE); recorder may be nil.
func NewReconciler(store ReconcileStore, events Publisher, recorder Recorder, logger *slog.Logger) *Reconciler {
	return &Reconciler{store: store, events: events, recorder: recorder, logger: logger}
}

// WithEvents attaches the realtime publisher; nil-safe.
func (rc *Reconciler) WithEvents(p Publisher) *Reconciler {
	rc.events = p
	return rc
}

// Reconcile publishes the live status event and opens/resolves the auto-incident for a
// status change. The monitor is fetched once and shared.
func (rc *Reconciler) Reconcile(ctx context.Context, hb domain.Heartbeat, prev, cur domain.MonitorStatus, suppressed bool) {
	mon, err := rc.store.GetMonitor(ctx, hb.MonitorID)
	if err != nil {
		rc.logger.Error("transition_get_monitor_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if rc.events != nil {
		rc.events.Publish(events.Event{
			Type: "status", MonitorID: mon.ID, ProjectID: mon.ProjectID,
			Status: string(cur), LatencyMS: hb.LatencyMS, TS: hb.Ts,
		})
	}
	switch {
	case cur == domain.StatusDown && prev != domain.StatusDown:
		// Opening is opt-out per monitor and suppressed during maintenance; resolving stays
		// unconditional so an already-open auto-incident still closes on recovery even if
		// auto-incidents were turned off meanwhile.
		if mon.AutoIncident && !suppressed {
			rc.openAutoIncident(ctx, mon, hb)
		}
	case cur == domain.StatusUp && prev != domain.StatusUp:
		// Any transition INTO up resolves an open auto-incident — not just down→up.
		// A monitor re-enabled while a pre-disable auto-incident is still open recovers
		// as pending→up (re-arm sets pending, D-0144); that must close the stale incident
		// too. resolveAutoIncident is a no-op when nothing is open, so a normal first
		// pending→up (no incident) costs one lookup and does nothing.
		rc.resolveAutoIncident(ctx, hb)
	}
}

func (rc *Reconciler) openAutoIncident(ctx context.Context, mon domain.Monitor, hb domain.Heartbeat) {
	// Don't double-open while one is already active.
	if _, err := rc.store.FindOpenAutoIncidentByMonitor(ctx, hb.MonitorID); err == nil {
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		rc.logger.Error("find_open_auto_incident_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	body := "Automatically opened: the monitor is failing its checks."
	if hb.Msg != "" {
		body = "Automatically opened: " + hb.Msg
	}
	inc := domain.Incident{
		ProjectID: mon.ProjectID,
		MonitorID: mon.ID,
		Title:     mon.Name + " is down",
		Status:    domain.IncidentInvestigating,
		Impact:    domain.ImpactMajor,
		Source:    domain.SourceAuto,
	}
	// Retry a transient failure: for an escalation-policy monitor the on-call ladder pages
	// OVER this incident and the flat down-notify is suppressed, so a lost incident-create
	// means the outage goes entirely unalerted. A bounded retry rides out a brief DB blip.
	// (A concurrent create → ErrAlreadyOpen is success, not retried.)
	var created domain.Incident
	var err error
	for attempt := 1; ; attempt++ {
		created, err = rc.store.CreateIncident(ctx, inc, body, autoIncidentAuthor)
		if errors.Is(err, store.ErrAlreadyOpen) {
			return // a concurrent down transition opened it first (unique index) — benign
		}
		if err == nil || attempt >= autoIncidentOpenAttempts {
			break
		}
		rc.logger.Warn("auto_incident_open_retry", "monitor_id", hb.MonitorID, "attempt", attempt, "error", err.Error())
		if !sleepCtx(ctx, autoIncidentOpenBackoff) {
			return // shutting down
		}
	}
	if err != nil {
		rc.logger.Error("auto_incident_open_failed", "monitor_id", hb.MonitorID, "escalation", mon.EscalationPolicyID != "", "error", err.Error())
		return
	}
	if rc.recorder != nil {
		rc.recorder.RecordIncidentOpened()
	}
	rc.logger.Info("auto_incident_opened", "monitor_id", hb.MonitorID, "incident_id", created.ID)
}

func (rc *Reconciler) resolveAutoIncident(ctx context.Context, hb domain.Heartbeat) {
	inc, err := rc.store.FindOpenAutoIncidentByMonitor(ctx, hb.MonitorID)
	if errors.Is(err, store.ErrNotFound) {
		return // nothing open (e.g. incident was resolved manually)
	}
	if err != nil {
		rc.logger.Error("find_open_auto_incident_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if _, err := rc.store.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID,
		Status:     domain.IncidentResolved,
		Body:       "Monitor recovered — automatically resolved.",
		Author:     autoIncidentAuthor,
	}); err != nil {
		rc.logger.Error("auto_incident_resolve_failed", "monitor_id", hb.MonitorID, "incident_id", inc.ID, "error", err.Error())
		return
	}
	rc.logger.Info("auto_incident_resolved", "monitor_id", hb.MonitorID, "incident_id", inc.ID)
}
