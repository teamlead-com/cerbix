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
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectRead)
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
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
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
		// PreviewID confirms a token issued for THIS mutation. Required only when the
		// window reaches back over sealed reliability facts — a prospective window changes
		// no settled number and needs no ceremony.
		PreviewID string `json:"preview_id"`
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
	created, err := h.store.CreateMaintenanceWindowChecked(r.Context(), mw, body.PreviewID, h.rawRetention())
	if h.writeMaintenanceError(w, "create_maintenance", err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// writeMaintenanceError maps the retroactive-mutation contract to HTTP. Reported honestly
// rather than as a 500: every one of these is something the operator can act on.
func (h *Handler) writeMaintenanceError(w http.ResponseWriter, op string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrRetroactiveNeedsPreview):
		writeError(w, http.StatusConflict, "preview_required")
	case errors.Is(err, store.ErrPreviewStale):
		writeError(w, http.StatusConflict, "preview_stale")
	case errors.Is(err, store.ErrPreviewApproximate):
		writeError(w, http.StatusConflict, "preview_approximate")
	case errors.Is(err, store.ErrUnrecomputableRange):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		h.serverError(w, op, err)
	}
	return true
}

// previewMaintenance issues a token BOUND to one mutation: this monitor, this exact range,
// this kind of change. It is also the only place an operator is told, before committing,
// which services a retroactive change would restate.
func (h *Handler) previewMaintenance(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		MonitorID string `json:"monitor_id"`
		Mutation  string `json:"mutation"`
		// MaintenanceID names the window an annul would remove. Required for `annul`: two
		// windows over the same monitor and range are different mutations.
		MaintenanceID string    `json:"maintenance_id"`
		StartsAt      time.Time `json:"starts_at"`
		EndsAt        time.Time `json:"ends_at"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	mutation := store.MutationCreate
	switch body.Mutation {
	case "", string(store.MutationCreate):
	case string(store.MutationAnnul):
		mutation = store.MutationAnnul
		if body.MaintenanceID == "" {
			writeError(w, http.StatusBadRequest, "maintenance_id is required to preview an annul")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "mutation must be create or annul")
		return
	}
	if !body.EndsAt.After(body.StartsAt) {
		writeError(w, http.StatusBadRequest, "ends_at must be after starts_at")
		return
	}
	p, err := h.store.PreviewMutationOf(r.Context(), proj.ID, body.MonitorID, body.MaintenanceID, mutation,
		body.StartsAt, body.EndsAt, h.rawRetention(), h.actorLabel(r))
	if h.writeMaintenanceError(w, "preview_maintenance", err) {
		return
	}
	// Both sides, both axes. A "before" alone is not a preview: the operator is being asked
	// to authorize a change to sealed numbers and would be shown only what they already are.
	type split struct {
		Good     int64 `json:"good_us"`
		Bad      int64 `json:"bad_us"`
		Unknown  int64 `json:"unknown_us"`
		Excluded int64 `json:"excluded_us"`
		// Health rides beside availability: a mutation can move one without the other — an
		// exclusion entirely inside already-degraded time changes health history and leaves
		// good/bad untouched — and a payload with only the first would show "no change" for
		// a change.
		Healthy       int64 `json:"healthy_us"`
		Degraded      int64 `json:"degraded_us"`
		Down          int64 `json:"down_us"`
		HealthUnknown int64 `json:"health_unknown_us"`
	}
	type affected struct {
		ServiceID string `json:"service_id"`
		Before    split  `json:"before"`
		After     split  `json:"after"`
		// Projected is false when the range exceeded the projection bound; the preview's
		// coverage then reads `approximate` and a confirm refuses it.
		Projected bool `json:"projected"`
	}
	out := struct {
		PreviewID string     `json:"preview_id"`
		ExpiresAt time.Time  `json:"expires_at"`
		Coverage  string     `json:"coverage"`
		Services  []affected `json:"services"`
	}{PreviewID: p.ID, ExpiresAt: p.ExpiresAt, Coverage: p.Coverage, Services: []affected{}}
	for _, svc := range p.Services {
		out.Services = append(out.Services, affected{
			ServiceID: svc.ServiceID,
			Before: split{svc.Before.Good, svc.Before.Bad, svc.Before.Unknown, svc.Before.Excluded,
				svc.Before.Healthy, svc.Before.Degraded, svc.Before.Down, svc.Before.HealthUnknown},
			After: split{svc.After.Good, svc.After.Bad, svc.After.Unknown, svc.After.Excluded,
				svc.After.Healthy, svc.After.Degraded, svc.After.Down, svc.After.HealthUnknown},
			Projected: svc.Projected,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// annulMaintenance says a window NEVER applied and repairs the facts computed under it.
// Distinct from archiving on purpose: archiving retires a window from active inventory and
// leaves the time it already covered excluded; annulling rewrites history and therefore needs
// a preview.
func (h *Handler) annulMaintenance(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		PreviewID string `json:"preview_id"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &body) {
		return
	}
	if h.writeMaintenanceError(w, "annul_maintenance",
		h.store.AnnulMaintenanceWindow(r.Context(), mw.ProjectID, mw.ID, body.PreviewID, h.rawRetention())) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	// ARCHIVE, not delete. A hard delete destroys the retained exclusion row, and with it the
	// only evidence of why a sealed window excluded the time it did — a recompute would then
	// silently restate settled numbers with no preview, no audit and no way back. Removing a
	// window's past effect is a different operation, and it is called annul.
	if h.writeMaintenanceError(w, "archive_maintenance",
		h.store.ArchiveMaintenanceWindow(r.Context(), mw.ProjectID, mw.ID)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
