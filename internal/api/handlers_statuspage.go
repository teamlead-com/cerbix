package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	// "not found" and "exists but in another org" return the SAME response so a caller
	// can't use the difference to enumerate monitor ids across tenant boundaries.
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
		writeError(w, http.StatusBadRequest, "monitor not found") // uniform: no cross-tenant existence oracle
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
	h.writeStatusPageRender(w, r, sp, false) // authed preview: full detail
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
	// §15.0's rate bound: a few seconds of shared bytes, keyed to the exact access shape so an
	// unlisted page's render is unreachable without its token — and COALESCED, so N simultaneous
	// cold requests cost ONE render rather than N ([318] P1-2).
	key := statusPageCacheKey(sp.ID, true, r.URL.Query().Get("token"))
	body, hit, err := h.renderCache.do(key, func() ([]byte, bool, error) {
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		h.writeStatusPageRender(rec, r, sp, true) // public: strip internal ids
		// ONLY a successful render is cached. A 503 over the safe limit or a 500 must be
		// re-derived, or one bad moment would be served for the whole TTL.
		return rec.bytesWithStatus(), rec.status == http.StatusOK, nil
	})
	if err != nil {
		h.serverError(w, "render_status_page", err)
		return
	}
	writeBufferedResponse(w, body, hit)
}

// dayPoint is one day of a component's 90-day availability strip.
type dayPoint struct {
	Day           string  `json:"day"` // YYYY-MM-DD (UTC)
	UptimePercent float64 `json:"uptime_percent"`
	Total         int64   `json:"total"`
}

type componentView struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Group       string                 `json:"group,omitempty"`
	Description string                 `json:"description,omitempty"`
	Status      domain.ComponentStatus `json:"status"`
	Uptime90d   *float64               `json:"uptime_90d,omitempty"`
	Daily       []dayPoint             `json:"daily,omitempty"`
	// The three fields below are AUTHENTICATED-only enrichment (§15.0): `source` is internal
	// topology, and `reason` / `unavailable` are operator diagnostics. A public page states what
	// is true about the service, never how cerbix is wired or which of its reads failed.
	Source domain.ComponentSource `json:"source,omitempty"`
	Reason string                 `json:"reason,omitempty"`
	// Unavailable marks a component whose state could not be READ (invariant 71a), as opposed to
	// one whose measurement is genuinely absent. PUBLIC: a failed read that looked like a calm
	// value would be the quiet lie this whole phase exists to remove.
	Unavailable bool `json:"unavailable,omitempty"`
	// UptimeWithheld names why `uptime_90d` is absent — insufficient history, a storage gap, zero
	// decidable time, coverage below the minimum, or facts spanning definition revisions. PUBLIC,
	// because a missing number without its reason is indistinguishable from a number nobody
	// bothered to compute (§11.2/§11.3).
	UptimeWithheld string `json:"withheld_reason,omitempty"`
}

type statusPageRender struct {
	Slug       string            `json:"slug"`
	Title      string            `json:"title"`
	Visibility domain.Visibility `json:"visibility"`
	// Summary is the worst MEASURED status, unchanged in name and type so every shipped client
	// keeps parsing it. SummaryState and Unmeasured are the halves it cannot express: a page can
	// be operational AND partly unmeasured, and those two facts must not be merged (invariant 67).
	Summary         domain.ComponentStatus     `json:"summary"`
	SummaryState    domain.PageSummaryState    `json:"summary_state"`
	Unmeasured      int                        `json:"unmeasured_count"`
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
func (h *Handler) enrichIncidents(w http.ResponseWriter, r *http.Request, incs []domain.Incident, withPostmortem, public bool) ([]incidentDetailView, bool) {
	ctx := r.Context()
	out := make([]incidentDetailView, 0, len(incs))
	if len(incs) == 0 {
		return out, true
	}
	// TWO statements for the whole list, not two per incident ([318] P1-1). A page with fifty
	// resolved incidents used to cost a hundred round trips on an unauthenticated surface.
	ids := make([]string, 0, len(incs))
	for _, in := range incs {
		ids = append(ids, in.ID)
	}
	timelines, err := h.store.IncidentTimelines(ctx, ids)
	if err != nil {
		h.serverError(w, "list_incident_updates", err)
		return nil, false
	}
	postmortems := map[string]domain.Postmortem{}
	if withPostmortem {
		if postmortems, err = h.store.PostmortemsForIncidents(ctx, ids); err != nil {
			h.serverError(w, "get_postmortem", err)
			return nil, false
		}
	}
	for _, in := range incs {
		updates := timelines[in.ID]
		var pm *domain.Postmortem
		if got, ok := postmortems[in.ID]; ok {
			pm = &got
		}
		// On the public endpoint, strip internal ids / actors from the incident AND its timeline
		// updates + postmortem before they leave the server to an unauthenticated viewer (each
		// carried its own id, incident id, and author UUID). The updates are COPIED first: they
		// come from a shared map now, and redacting in place would mutate what another call sees.
		if public {
			in = in.PublicRedacted()
			red := make([]domain.IncidentUpdate, len(updates))
			for i, u := range updates {
				red[i] = u.PublicRedacted()
			}
			updates = red
			if pm != nil {
				redPM := pm.PublicRedacted()
				pm = &redPM
			}
		}
		out = append(out, incidentDetailView{Incident: in, Updates: updates, Postmortem: pm})
	}
	return out, true
}

// bufferedResponse captures a render so the SAME bytes can be served and shared. Computing the
// view twice, or caching a struct a later handler could mutate, would let the two diverge.
type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(s int)   { b.status = s }
func (b *bufferedResponse) Write(p []byte) (int, error) {
	return b.body.Write(p)
}

// bytesWithStatus packs the status ahead of the body, so a coalesced waiter reproduces the whole
// response — a refusal shared as a 200 would be worse than no coalescing at all.
func (b *bufferedResponse) bytesWithStatus() []byte {
	out := make([]byte, 0, b.body.Len()+4)
	out = append(out, byte(b.status>>8), byte(b.status))
	return append(out, b.body.Bytes()...)
}

// writeBufferedResponse unpacks what the cache stored and writes it, marking a served-from-cache
// answer so operators (and the tests) can tell the two apart.
func writeBufferedResponse(w http.ResponseWriter, packed []byte, hit bool) {
	status, body := http.StatusOK, packed
	if len(packed) >= 2 {
		status, body = int(packed[0])<<8|int(packed[1]), packed[2:]
	}
	w.Header().Set("Content-Type", "application/json")
	if hit {
		w.Header().Set("X-Cerbix-Cache", "hit")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeStatusPageRender assembles and writes a page's public view: each
// component's derived status and 90-day uptime, the worst-of summary, and the
// unresolved incidents across the projects the components draw from.
func (h *Handler) writeStatusPageRender(w http.ResponseWriter, r *http.Request, sp domain.StatusPage, public bool) {
	ctx := r.Context()
	comps, err := h.store.ListComponentsByPage(ctx, sp.ID)
	if err != nil {
		h.serverError(w, "list_components", err)
		return
	}
	// The absolute fail-closed public ceiling (§15.0, invariant 71b). An unauthenticated render is
	// the one surface an attacker can amplify for free, so above the bound the page refuses AS A
	// WHOLE and names the numbers. A truncated subset would be worse than a refusal: it would look
	// like a complete page that happens to be healthy. The AUTHENTICATED view keeps listing
	// everything, so the operator can see and fix what the public page cannot serve.
	if public && len(comps) > publicComponentHardCeiling {
		h.logger.Error("status page exceeds the public safe limit",
			slog.String("page", sp.ID), slog.Int("components", len(comps)),
			slog.Int("limit", publicComponentHardCeiling))
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
			"status_page_over_safe_limit: this page has %d components, above the public limit of %d",
			len(comps), publicComponentHardCeiling))
		return
	}
	now := time.Now()
	win90, _ := sla.WindowByName("90d")
	resolved, err := h.resolveComponents(ctx, comps, true)
	if err != nil {
		h.serverError(w, "resolve_components", err)
		return
	}
	views := make([]componentView, 0, len(comps))
	projSet := map[string]struct{}{}
	for _, c := range comps {
		res := resolved[c.ID]
		if res.Project != "" {
			projSet[res.Project] = struct{}{}
		}
		view := componentView{
			ID: c.ID, Name: c.Name, Group: c.GroupName, Description: c.Description,
			Status: res.Status, Uptime90d: res.Uptime90d, Daily: res.Daily,
		}
		// `unavailable` is PUBLIC (§15.0, invariant 71a): it says our own read failed, which is a
		// statement about cerbix and not about the customer's topology. Without it, a failed read
		// would be byte-identical to the calm "no data" — the exact confusion that invariant
		// forbids. `withheld_reason` is public for the same reason a missing number always carries
		// one; `source` and `reason` stay authenticated, because those describe internal wiring.
		view.Unavailable = res.Unavailable
		view.UptimeWithheld = res.UptimeWithheld
		if !public {
			view.Source = c.Source
			view.Reason = res.Reason
		}
		views = append(views, view)
	}
	summary := summarize(comps, resolved)

	// Active incidents, resolved-incident history (last 90 days), and active or upcoming
	// maintenance — for ALL the page's projects in two statements and one, not per project. The
	// per-project loop made an unauthenticated render O(projects), which is the same amplification
	// the component projections had to remove ([318] P1-1).
	projects := make([]string, 0, len(projSet))
	for pid := range projSet {
		projects = append(projects, pid)
	}
	sort.Strings(projects) // deterministic argument order, so two identical pages issue identical SQL
	historyCutoff := now.Add(-win90.Duration)
	incidents, err := h.store.IncidentsForPage(ctx, projects, historyCutoff)
	if err != nil {
		h.serverError(w, "page_incidents", err)
		return
	}
	active, recent := incidents.Active, incidents.Recent
	maints, err := h.store.MaintenanceForPage(ctx, projects, now)
	if err != nil {
		h.serverError(w, "page_maintenance", err)
		return
	}

	// Both lists arrive ordered from SQL (resolved DESC, starts_at ASC); re-sorting here would be
	// a second ordering owner that could drift from the one the query states.

	// Enrich incidents so each can expand: active ones show their timeline
	// (latest update inline), past ones their timeline + postmortem.
	activeViews, ok := h.enrichIncidents(w, r, active, false, public)
	if !ok {
		return
	}
	recentViews, ok := h.enrichIncidents(w, r, recent, true, public)
	if !ok {
		return
	}
	// Strip internal project/monitor ids from maintenance windows on the public endpoint.
	if public {
		for i := range maints {
			maints[i] = maints[i].PublicRedacted()
		}
	}

	writeJSON(w, http.StatusOK, statusPageRender{
		Slug:            sp.Slug,
		Title:           sp.Title,
		Visibility:      sp.Visibility,
		Summary:         summary.Status,
		SummaryState:    summary.State,
		Unmeasured:      summary.UnmeasuredCount,
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
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error()) // fail closed — never issue a predictable token
	}
	return hex.EncodeToString(b)
}

// listSubscribers returns a page's subscribers, confirmed and pending alike
// (org admin) — the owner-facing view of who receives incident emails.
func (h *Handler) listSubscribers(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, true)
	if !ok {
		return
	}
	subs, err := h.store.ListSubscribersByPage(r.Context(), sp.ID)
	if err != nil {
		h.serverError(w, "list_subscribers", err)
		return
	}
	writeJSON(w, http.StatusOK, subs)
}

// deleteSubscriber removes a subscriber from a page (org admin).
func (h *Handler) deleteSubscriber(w http.ResponseWriter, r *http.Request) {
	sp, ok := h.statusPageAccess(w, r, true)
	if !ok {
		return
	}
	if err := h.store.DeleteSubscriber(r.Context(), sp.ID, r.PathValue("subscriberID")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.serverError(w, "delete_subscriber", err)
		return
	}
	h.audit(r, sp.OrgID, "subscriber.remove", "page "+sp.Slug)
	w.WriteHeader(http.StatusNoContent)
}
