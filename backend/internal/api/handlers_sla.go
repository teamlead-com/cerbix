package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
	"github.com/teamlead-com/cerbix/internal/store"
)

type windowSLA struct {
	Window        string            `json:"window"`
	Total         int64             `json:"total"`
	Up            int64             `json:"up"`
	UptimePercent float64           `json:"uptime_percent"`
	AvgLatencyMS  float64           `json:"avg_latency_ms"`
	P95LatencyMS  float64           `json:"p95_latency_ms"`
	Objective     *float64          `json:"objective,omitempty"`
	Budget        *sla.Budget       `json:"error_budget,omitempty"`
	BurnAlert     bool              `json:"burn_alert,omitempty"`
	BurnFiring    bool              `json:"burn_firing,omitempty"` // any rule latched firing
	BurnRules     []domain.BurnRule `json:"burn_rules,omitempty"`
}

// monitorSLA reports SLI per standard window, with the SLO error budget where a
// target is set.
func (h *Handler) monitorSLA(w http.ResponseWriter, r *http.Request) {
	mon, ok := h.monitorAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	now := time.Now()
	windows := make([]windowSLA, 0, len(sla.StandardWindows))
	for _, win := range sla.StandardWindows {
		c, err := h.store.MonitorSLI(r.Context(), mon.ID, now.Add(-win.Duration))
		if err != nil {
			h.serverError(w, "monitor_sli", err)
			return
		}
		ws := windowSLA{
			Window: win.Name, Total: c.Total, Up: c.Up,
			UptimePercent: sla.Uptime(c.Up, c.Total), AvgLatencyMS: c.AvgLatencyMS, P95LatencyMS: c.P95LatencyMS,
		}
		if target, err := h.store.GetMonitorSLATarget(r.Context(), mon.ID, win.Name); err == nil {
			obj := target.Objective
			budget := sla.ErrorBudget(obj, c.Up, c.Total)
			ws.Objective = &obj
			ws.Budget = &budget
			ws.BurnAlert = target.BurnAlertEnabled
			ws.BurnFiring = target.AnyBurnFiring()
			ws.BurnRules = target.BurnRules
		} else if !errors.Is(err, store.ErrNotFound) {
			h.serverError(w, "get_sla_target", err)
			return
		}
		windows = append(windows, ws)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"monitor_id": mon.ID,
		"status":     mon.Status,
		"windows":    windows,
	})
}

// setMonitorSLATarget sets a monitor's SLO objective for a window.
func (h *Handler) setMonitorSLATarget(w http.ResponseWriter, r *http.Request) {
	mon, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		Objective float64            `json:"objective"`
		Window    string             `json:"window"`
		BurnAlert bool               `json:"burn_alert"`
		BurnRules *[]domain.BurnRule `json:"burn_rules"` // nil = keep existing (or seed defaults)
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Window == "" {
		body.Window = "30d"
	}
	if _, valid := sla.WindowByName(body.Window); !valid {
		writeError(w, http.StatusBadRequest, "unknown window")
		return
	}
	if body.Objective <= 0 || body.Objective > 100 {
		writeError(w, http.StatusBadRequest, "objective must be within (0,100]")
		return
	}
	var rules []domain.BurnRule
	if body.BurnRules != nil {
		rules = *body.BurnRules
		if rules == nil {
			rules = []domain.BurnRule{}
		}
		if err := domain.ValidateBurnRules(rules); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	target, err := h.store.UpsertMonitorSLATarget(r.Context(), mon.ID, body.Window, body.Objective, body.BurnAlert, rules)
	if err != nil {
		h.serverError(w, "upsert_sla_target", err)
		return
	}
	writeJSON(w, http.StatusOK, target)
}

// projectSLA reports project-level SLI per standard window.
func (h *Handler) projectSLA(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	now := time.Now()
	windows := make([]windowSLA, 0, len(sla.StandardWindows))
	for _, win := range sla.StandardWindows {
		c, err := h.store.ProjectSLI(r.Context(), proj.ID, now.Add(-win.Duration))
		if err != nil {
			h.serverError(w, "project_sli", err)
			return
		}
		windows = append(windows, windowSLA{
			Window: win.Name, Total: c.Total, Up: c.Up,
			UptimePercent: sla.Uptime(c.Up, c.Total), AvgLatencyMS: c.AvgLatencyMS, P95LatencyMS: c.P95LatencyMS,
		})
	}
	reportEnabled, err := h.store.ProjectSLAReportEnabled(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "project_sla_report_enabled", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":        proj.ID,
		"windows":           windows,
		"sla_report_weekly": reportEnabled,
	})
}

// setProjectSLAReport toggles a project's weekly SLA report (editor+).
func (h *Handler) setProjectSLAReport(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	enabled, err := h.store.SetProjectSLAReport(r.Context(), proj.ID, body.Enabled)
	if err != nil {
		h.serverError(w, "set_project_sla_report", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sla_report_weekly": enabled})
}

// listMaintenance lists a project's maintenance windows.
func (h *Handler) listMaintenance(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	windows, err := h.store.ListMaintenanceWindowsByProject(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_maintenance", err)
		return
	}
	writeJSON(w, http.StatusOK, windows)
}

// createMaintenance schedules a maintenance window for a project or one of its
// monitors.
func (h *Handler) createMaintenance(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		MonitorID string    `json:"monitor_id"`
		StartsAt  time.Time `json:"starts_at"`
		EndsAt    time.Time `json:"ends_at"`
		Reason    string    `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// A monitor-scoped window must reference a monitor in this project.
	if body.MonitorID != "" {
		mon, err := h.store.GetMonitor(r.Context(), body.MonitorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && mon.ProjectID != proj.ID) {
			writeError(w, http.StatusBadRequest, "monitor does not belong to this project")
			return
		}
		if err != nil {
			h.serverError(w, "get_monitor", err)
			return
		}
	}
	mw := domain.MaintenanceWindow{
		ProjectID: proj.ID, MonitorID: body.MonitorID,
		StartsAt: body.StartsAt, EndsAt: body.EndsAt, Reason: body.Reason,
	}
	if err := mw.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateMaintenanceWindow(r.Context(), mw)
	if err != nil {
		h.serverError(w, "create_maintenance", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// deleteMaintenance removes a maintenance window (project-write on its project).
func (h *Handler) deleteMaintenance(w http.ResponseWriter, r *http.Request) {
	mw, err := h.store.GetMaintenanceWindow(r.Context(), r.PathValue("maintenanceID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_maintenance", err)
		return
	}
	if _, ok := h.projectAccess(w, r, mw.ProjectID, authz.ActionProjectWrite); !ok {
		return
	}
	if err := h.store.DeleteMaintenanceWindow(r.Context(), mw.ID); err != nil {
		h.serverError(w, "delete_maintenance", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
