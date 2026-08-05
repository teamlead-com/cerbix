package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
	"github.com/teamlead-com/cerbix/internal/store"
)

// statusPageAccess loads a status page and checks the caller may see it (org
// member) and, when requireManage is set, may manage it (org admin).
func (h *Handler) statusPageAccess(w http.ResponseWriter, r *http.Request, requireManage bool) (domain.StatusPage, bool) {
	p, _ := h.principal(r)
	sp, err := h.store.GetStatusPage(r.Context(), r.PathValue("pageID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.StatusPage{}, false
	}
	if err != nil {
		h.serverError(w, "get_status_page", err)
		return domain.StatusPage{}, false
	}
	if !p.InOrg(sp.OrgID) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.StatusPage{}, false
	}
	if requireManage && !p.Can(authz.ActionOrgManage, sp.OrgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return domain.StatusPage{}, false
	}
	return sp, true
}

// listStatusPages lists an org's status pages (org member).
func (h *Handler) listStatusPages(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	pages, err := h.store.ListStatusPagesByOrg(r.Context(), orgID)
	if err != nil {
		h.serverError(w, "list_status_pages", err)
		return
	}
	writeJSON(w, http.StatusOK, pages)
}

// createStatusPage creates a status page for an org (org admin). An unlisted page
// gets a generated secret token if none was supplied.
func (h *Handler) createStatusPage(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, orgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Slug       string `json:"slug"`
		Title      string `json:"title"`
		Visibility string `json:"visibility"`
		ProjectID  string `json:"project_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Visibility == "" {
		body.Visibility = string(domain.VisibilityInternal)
	}
	sp := domain.StatusPage{
		OrgID:      orgID,
		ProjectID:  body.ProjectID,
		Slug:       body.Slug,
		Title:      body.Title,
		Visibility: domain.Visibility(body.Visibility),
	}
	if sp.Visibility == domain.VisibilityUnlisted {
		sp.UnlistedToken = randomToken()
	}
	if err := sp.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateStatusPage(r.Context(), sp)
	if err != nil {
		// Slug uniqueness / cross-org project FK violations land here.
		writeError(w, http.StatusBadRequest, "could not create status page")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// getStatusPage returns a status page's configuration (org member).
func (h *Handler) getStatusPage(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

// updateStatusPage edits a page's title and/or visibility (org admin). Slug and
// org are immutable. Switching to unlisted mints a token if none exists;
// switching away from unlisted clears it.
func (h *Handler) updateStatusPage(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Title      *string `json:"title"`
		Visibility *string `json:"visibility"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Title != nil {
		sp.Title = *body.Title
	}
	if body.Visibility != nil {
		sp.Visibility = domain.Visibility(*body.Visibility)
	}
	switch {
	case sp.Visibility == domain.VisibilityUnlisted && sp.UnlistedToken == "":
		sp.UnlistedToken = randomToken()
	case sp.Visibility != domain.VisibilityUnlisted:
		sp.UnlistedToken = ""
	}
	if err := sp.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.store.UpdateStatusPage(r.Context(), sp)
	if err != nil {
		h.serverError(w, "update_status_page", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// deleteStatusPage removes a page and its components (org admin).
func (h *Handler) deleteStatusPage(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, true)
	if !ok {
		return
	}
	if err := h.store.DeleteStatusPage(r.Context(), sp.ID); err != nil {
		h.serverError(w, "delete_status_page", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listComponents lists a page's components (org member).
func (h *Handler) listComponents(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, false)
	if !ok {
		return
	}
	comps, err := h.store.ListComponentsByPage(r.Context(), sp.ID)
	if err != nil {
		h.serverError(w, "list_components", err)
		return
	}
	writeJSON(w, http.StatusOK, comps)
}

// createComponent adds a component to a page (org admin). A monitor-backed
// component must reference a monitor within the page's organization.
func (h *Handler) createComponent(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Group        string `json:"group"`
		Position     int    `json:"position"`
		MonitorID    string `json:"monitor_id"`
		ManualStatus string `json:"manual_status"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.MonitorID != "" && !h.monitorInOrg(w, r, body.MonitorID, sp.OrgID) {
		return
	}
	c := domain.Component{
		StatusPageID: sp.ID,
		Name:         body.Name,
		Description:  body.Description,
		GroupName:    body.Group,
		Position:     body.Position,
		MonitorID:    body.MonitorID,
		ManualStatus: domain.ComponentStatus(body.ManualStatus),
	}
	if err := c.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateComponent(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not create component")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// monitorInOrg verifies a monitor exists and belongs to the given organization.
// Writes 400 and returns false otherwise.
func (h *Handler) monitorInOrg(w http.ResponseWriter, r *http.Request, monitorID, orgID string) bool {
	mon, err := h.store.GetMonitor(r.Context(), monitorID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "monitor not found")
		return false
	}
	if err != nil {
		h.serverError(w, "get_monitor", err)
		return false
	}
	proj, err := h.store.GetProject(r.Context(), mon.ProjectID)
	if err != nil {
		h.serverError(w, "get_project", err)
		return false
	}
	if proj.OrgID != orgID {
		writeError(w, http.StatusBadRequest, "monitor is not in this organization")
		return false
	}
	return true
}

// deleteComponent removes a component (org admin on its page's org).
func (h *Handler) deleteComponent(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.GetComponent(r.Context(), r.PathValue("componentID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_component", err)
		return
	}
	sp, err := h.store.GetStatusPage(r.Context(), c.StatusPageID)
	if err != nil {
		h.serverError(w, "get_status_page", err)
		return
	}
	p, _ := h.principal(r)
	if !p.InOrg(sp.OrgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, sp.OrgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.DeleteComponent(r.Context(), c.ID); err != nil {
		h.serverError(w, "delete_component", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renderStatusPageAuthed renders a page for an authenticated org member, at any
// visibility.
func (h *Handler) renderStatusPageAuthed(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, false)
	if !ok {
		return
	}
	h.writeStatusPageRender(w, r, sp)
}

// renderStatusPagePublic renders a page without a session, enforcing visibility:
// public is served to anyone, unlisted requires the matching ?token=, and
// internal pages are hidden (404).
func (h *Handler) renderStatusPagePublic(w http.ResponseWriter, r *http.Request) {
	sp, err := h.store.GetStatusPageBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_status_page_by_slug", err)
		return
	}
	switch sp.Visibility {
	case domain.VisibilityPublic:
		// served
	case domain.VisibilityUnlisted:
		if tok := r.URL.Query().Get("token"); tok == "" || tok != sp.UnlistedToken {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
	default: // internal and anything else: hidden from the public endpoint
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.writeStatusPageRender(w, r, sp)
}

// dayPoint is one day of a component's 90-day availability strip.
type dayPoint struct {
	Day           string  `json:"day"` // YYYY-MM-DD (UTC)
	UptimePercent float64 `json:"uptime_percent"`
	Total         int64   `json:"total"`
}

type componentView struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Group     string                 `json:"group,omitempty"`
	Status    domain.ComponentStatus `json:"status"`
	Uptime90d *float64               `json:"uptime_90d,omitempty"`
	Daily     []dayPoint             `json:"daily,omitempty"`
}

type statusPageRender struct {
	Slug            string                     `json:"slug"`
	Title           string                     `json:"title"`
	Visibility      domain.Visibility          `json:"visibility"`
	Summary         domain.ComponentStatus     `json:"summary"`
	Components      []componentView            `json:"components"`
	ActiveIncidents []incidentDetailView       `json:"active_incidents"`
	RecentIncidents []incidentDetailView       `json:"recent_incidents"`
	Maintenance     []domain.MaintenanceWindow `json:"maintenance"`
	UpdatedAt       time.Time                  `json:"updated_at"`
}

// incidentDetailView is an incident enriched for the status page: its full
// timeline (updates), plus the published postmortem when one exists (resolved
// incidents only). Used for both active and past incidents so each can expand.
type incidentDetailView struct {
	domain.Incident
	Updates    []domain.IncidentUpdate `json:"updates"`
	Postmortem *domain.Postmortem      `json:"postmortem,omitempty"`
}

// enrichIncidents attaches each incident's update timeline, and (when
// withPostmortem) its published postmortem. N+1 is fine: the status page shows
// few incidents (active now, resolved over 90 days).
func (h *Handler) enrichIncidents(w http.ResponseWriter, r *http.Request, incs []domain.Incident, withPostmortem bool) ([]incidentDetailView, bool) {
	ctx := r.Context()
	out := make([]incidentDetailView, 0, len(incs))
	for _, in := range incs {
		updates, err := h.store.ListIncidentUpdates(ctx, in.ID)
		if err != nil {
			h.serverError(w, "list_incident_updates", err)
			return nil, false
		}
		var pm *domain.Postmortem
		if withPostmortem {
			if got, err := h.store.GetPostmortem(ctx, in.ID); err == nil {
				pm = &got
			} else if !errors.Is(err, store.ErrNotFound) {
				h.serverError(w, "get_postmortem", err)
				return nil, false
			}
		}
		out = append(out, incidentDetailView{Incident: in, Updates: updates, Postmortem: pm})
	}
	return out, true
}

// writeStatusPageRender assembles and writes a page's public view: each
// component's derived status and 90-day uptime, the worst-of summary, and the
// unresolved incidents across the projects the components draw from.
func (h *Handler) writeStatusPageRender(w http.ResponseWriter, r *http.Request, sp domain.StatusPage) {
	ctx := r.Context()
	comps, err := h.store.ListComponentsByPage(ctx, sp.ID)
	if err != nil {
		h.serverError(w, "list_components", err)
		return
	}
	now := time.Now()
	win90, _ := sla.WindowByName("90d")
	views := make([]componentView, 0, len(comps))
	statuses := make([]domain.ComponentStatus, 0, len(comps))
	projSet := map[string]struct{}{}
	for _, c := range comps {
		status := domain.CompOperational
		var uptime *float64
		var days []dayPoint
		if c.MonitorID != "" {
			mon, err := h.store.GetMonitor(ctx, c.MonitorID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				// Monitor gone (FK set null races): treat as manual/unknown.
			case err != nil:
				h.serverError(w, "get_monitor", err)
				return
			default:
				status = domain.ComponentStatusFromMonitor(mon.Status)
				projSet[mon.ProjectID] = struct{}{}
				counts, err := h.store.MonitorSLI(ctx, c.MonitorID, now.Add(-win90.Duration))
				if err != nil {
					h.serverError(w, "monitor_sli", err)
					return
				}
				u := sla.Uptime(counts.Up, counts.Total)
				uptime = &u
				// Per-day 90-day availability strip for a monitor-backed component.
				rows, err := h.store.MonitorDailyAvailability(ctx, c.MonitorID, now.Add(-win90.Duration))
				if err != nil {
					h.serverError(w, "monitor_daily_availability", err)
					return
				}
				for _, d := range rows {
					days = append(days, dayPoint{Day: d.Day.Format("2006-01-02"), UptimePercent: d.UptimePercent, Total: d.Total})
				}
			}
		}
		if c.ManualStatus != "" {
			status = c.ManualStatus // explicit manual override wins
		}
		statuses = append(statuses, status)
		views = append(views, componentView{ID: c.ID, Name: c.Name, Group: c.GroupName, Status: status, Uptime90d: uptime, Daily: days})
	}
	// Active incidents, resolved-incident history (last 90 days), and active or
	// upcoming maintenance across the projects the components draw from.
	active := make([]domain.Incident, 0)
	recent := make([]domain.Incident, 0)
	maints := make([]domain.MaintenanceWindow, 0)
	historyCutoff := now.Add(-win90.Duration)
	for pid := range projSet {
		open, err := h.store.ListOpenIncidentsByProject(ctx, pid)
		if err != nil {
			h.serverError(w, "list_open_incidents", err)
			return
		}
		active = append(active, open...)
		incs, err := h.store.ListIncidentsByProject(ctx, pid)
		if err != nil {
			h.serverError(w, "list_incidents", err)
			return
		}
		for _, in := range incs {
			if in.Status == domain.IncidentResolved && in.ResolvedAt != nil && in.ResolvedAt.After(historyCutoff) {
				recent = append(recent, in)
			}
		}
		mws, err := h.store.ListMaintenanceWindowsByProject(ctx, pid)
		if err != nil {
			h.serverError(w, "list_maintenance", err)
			return
		}
		for _, mw := range mws {
			if mw.EndsAt.After(now) { // active or scheduled
				maints = append(maints, mw)
			}
		}
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].ResolvedAt.After(*recent[j].ResolvedAt) })
	sort.Slice(maints, func(i, j int) bool { return maints[i].StartsAt.Before(maints[j].StartsAt) })

	// Enrich incidents so each can expand: active ones show their timeline
	// (latest update inline), past ones their timeline + postmortem.
	activeViews, ok := h.enrichIncidents(w, r, active, false)
	if !ok {
		return
	}
	recentViews, ok := h.enrichIncidents(w, r, recent, true)
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, statusPageRender{
		Slug:            sp.Slug,
		Title:           sp.Title,
		Visibility:      sp.Visibility,
		Summary:         domain.SummaryStatus(statuses),
		Components:      views,
		ActiveIncidents: activeViews,
		RecentIncidents: recentViews,
		Maintenance:     maints,
		UpdatedAt:       sp.UpdatedAt,
	})
}

// randomToken returns a 128-bit hex token for unlisted status pages.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
