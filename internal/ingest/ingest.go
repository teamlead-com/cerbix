// Package ingest consumes check results from the dispatcher, persists them as
// heartbeats, updates each monitor's last-known status, and opens/resolves
// auto-incidents on down/up transitions.
package ingest

import (
	"context"
	"errors"
	"log/slog"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/events"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Store is the persistence surface the ingester needs.
type Store interface {
	InsertHeartbeat(ctx context.Context, hb domain.Heartbeat) error
	// RecordCheckStatus applies a check result with alert confirmations and
	// maintenance suppression, returning the previous and new status and whether
	// the change was suppressed (in maintenance → don't open an incident).
	RecordCheckStatus(ctx context.Context, monitorID string, up bool) (prev, cur domain.MonitorStatus, suppressed bool, err error)
	GetMonitor(ctx context.Context, id string) (domain.Monitor, error)
	FindOpenAutoIncidentByMonitor(ctx context.Context, monitorID string) (domain.Incident, error)
	CreateIncident(ctx context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error)
	AddIncidentUpdate(ctx context.Context, upd domain.IncidentUpdate) (domain.IncidentUpdate, error)
}

// Recorder records per-check metrics.
type Recorder interface {
	RecordCheck(up bool)
	RecordIncidentOpened()
}

// autoIncidentAuthor labels timeline entries the pipeline writes.
const autoIncidentAuthor = "auto"

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
	events     Publisher
	logger     *slog.Logger
}

// New builds a results consumer.
func New(store Store, dispatcher dispatch.Dispatcher, recorder Recorder, logger *slog.Logger) *Consumer {
	return &Consumer{store: store, dispatcher: dispatcher, recorder: recorder, logger: logger}
}

// WithEvents attaches a realtime publisher for status changes. Optional and
// nil-safe.
func (c *Consumer) WithEvents(p Publisher) *Consumer {
	c.events = p
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
	if err := c.store.InsertHeartbeat(ctx, hb); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The monitor was deleted while the probe was in flight; the
			// scheduler drops it on its next snapshot refresh.
			c.logger.Info("heartbeat_for_deleted_monitor", "monitor_id", hb.MonitorID)
			return
		}
		c.logger.Error("insert_heartbeat_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	prev, cur, suppressed, err := c.store.RecordCheckStatus(ctx, hb.MonitorID, hb.Up)
	if errors.Is(err, store.ErrNotFound) {
		// Monitor deleted between the heartbeat insert and the status flip —
		// same benign race as the insert path, not a failure.
		c.logger.Info("status_for_deleted_monitor", "monitor_id", hb.MonitorID)
	} else if err != nil {
		c.logger.Error("record_check_status_failed", "monitor_id", hb.MonitorID, "error", err.Error())
	} else if prev != cur {
		c.logger.Info("monitor_status_changed", "monitor_id", hb.MonitorID, "prev", string(prev), "cur", string(cur), "suppressed", suppressed)
		c.reconcileTransition(ctx, hb, prev, cur, suppressed)
	}
	if c.recorder != nil {
		c.recorder.RecordCheck(hb.Up)
	}
}

// reconcileTransition handles any monitor status change: it publishes a live
// event for SSE subscribers, and on going down opens an auto-incident / on
// recovery resolves it (notifying the monitor's channels). The monitor is fetched
// once and shared. Best-effort: failures are logged, never fatal to ingestion.
func (c *Consumer) reconcileTransition(ctx context.Context, hb domain.Heartbeat, prev, cur domain.MonitorStatus, suppressed bool) {
	mon, err := c.store.GetMonitor(ctx, hb.MonitorID)
	if err != nil {
		c.logger.Error("transition_get_monitor_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if c.events != nil {
		c.events.Publish(events.Event{
			Type: "status", MonitorID: mon.ID, ProjectID: mon.ProjectID,
			Status: string(cur), LatencyMS: hb.LatencyMS, TS: hb.Ts,
		})
	}
	switch {
	case cur == domain.StatusDown && prev != domain.StatusDown:
		// Opening is opt-out per monitor and suppressed during maintenance;
		// resolving stays unconditional so an already-open auto-incident still
		// closes on recovery even if auto-incidents were turned off meanwhile.
		if mon.AutoIncident && !suppressed {
			c.openAutoIncident(ctx, mon, hb)
		}
	case cur == domain.StatusUp && prev == domain.StatusDown:
		c.resolveAutoIncident(ctx, hb)
	}
}

func (c *Consumer) openAutoIncident(ctx context.Context, mon domain.Monitor, hb domain.Heartbeat) {
	// Don't double-open while one is already active.
	if _, err := c.store.FindOpenAutoIncidentByMonitor(ctx, hb.MonitorID); err == nil {
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		c.logger.Error("find_open_auto_incident_failed", "monitor_id", hb.MonitorID, "error", err.Error())
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
	created, err := c.store.CreateIncident(ctx, inc, body, autoIncidentAuthor)
	if err != nil {
		c.logger.Error("auto_incident_open_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if c.recorder != nil {
		c.recorder.RecordIncidentOpened()
	}
	c.logger.Info("auto_incident_opened", "monitor_id", hb.MonitorID, "incident_id", created.ID)
}

func (c *Consumer) resolveAutoIncident(ctx context.Context, hb domain.Heartbeat) {
	inc, err := c.store.FindOpenAutoIncidentByMonitor(ctx, hb.MonitorID)
	if errors.Is(err, store.ErrNotFound) {
		return // nothing open (e.g. incident was resolved manually)
	}
	if err != nil {
		c.logger.Error("find_open_auto_incident_failed", "monitor_id", hb.MonitorID, "error", err.Error())
		return
	}
	if _, err := c.store.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID,
		Status:     domain.IncidentResolved,
		Body:       "Monitor recovered — automatically resolved.",
		Author:     autoIncidentAuthor,
	}); err != nil {
		c.logger.Error("auto_incident_resolve_failed", "monitor_id", hb.MonitorID, "incident_id", inc.ID, "error", err.Error())
		return
	}
	c.logger.Info("auto_incident_resolved", "monitor_id", hb.MonitorID, "incident_id", inc.ID)
}
