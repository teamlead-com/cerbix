package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-021 §15.0/§15.5 API surface: the component source, the previewed conversion, and the
// composite lifecycle.

// componentAccess loads a component and checks the caller may manage its page. Access is decided
// through the PAGE, never through the component's bindings: a component is a page's line, and the
// page's org is the tenancy that governs it.
func (h *Handler) componentAccess(w http.ResponseWriter, r *http.Request) (domain.Component, domain.StatusPage, bool) {
	p, _ := h.principal(r)
	c, err := h.store.GetComponent(r.Context(), r.PathValue("componentID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Component{}, domain.StatusPage{}, false
	}
	if err != nil {
		h.serverError(w, "get_component", err)
		return domain.Component{}, domain.StatusPage{}, false
	}
	sp, err := h.store.GetStatusPage(r.Context(), c.StatusPageID)
	if err != nil {
		h.serverError(w, "get_status_page", err)
		return domain.Component{}, domain.StatusPage{}, false
	}
	if !p.InOrg(sp.OrgID) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Component{}, domain.StatusPage{}, false
	}
	if !p.Can(authz.ActionOrgManage, sp.OrgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return domain.Component{}, domain.StatusPage{}, false
	}
	return c, sp, true
}

// updateComponent edits a component's presentation fields (org admin). It cannot change the
// source: that is the previewed conversion below, and an update path able to do it silently would
// be the way around the gate.
func (h *Handler) updateComponent(w http.ResponseWriter, r *http.Request) {
	c, sp, ok := h.componentAccess(w, r)
	if !ok {
		return
	}
	body := struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Group        string `json:"group"`
		Position     int    `json:"position"`
		ManualStatus string `json:"manual_status"`
	}{Name: c.Name, Description: c.Description, Group: c.GroupName, Position: c.Position,
		ManualStatus: string(c.ManualStatus)}
	if !decodeJSON(w, r, &body) {
		return
	}
	next := domain.Component{
		ID: c.ID, StatusPageID: c.StatusPageID, Name: body.Name, Description: body.Description,
		GroupName: body.Group, Position: body.Position,
		ManualStatus: domain.ComponentStatus(body.ManualStatus),
	}
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.store.UpdateComponent(r.Context(), sp.OrgID, next)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// conversionTargetBody is the shared request shape of preview and confirm.
type conversionTargetBody struct {
	Source       string `json:"source"`
	ServiceID    string `json:"service_id"`
	MonitorID    string `json:"monitor_id"`
	ManualStatus string `json:"manual_status"`
	// Revision and PageGeneration are required on CONFIRM only: they are the consent tokens the
	// preview handed out.
	Revision       *int64 `json:"revision"`
	PageGeneration *int64 `json:"page_generation"`
}

func (b conversionTargetBody) target() store.ComponentConversionTarget {
	return store.ComponentConversionTarget{
		Source:       domain.ComponentSource(b.Source),
		ServiceID:    b.ServiceID,
		MonitorID:    b.MonitorID,
		ManualStatus: domain.ComponentStatus(b.ManualStatus),
	}
}

// conversionPreviewView is what the operator consents to: the component line BEFORE and AFTER,
// the page summary BEFORE and AFTER, and the two tokens that bind the consent.
type conversionPreviewView struct {
	Component      componentView          `json:"component"`
	Proposed       componentView          `json:"proposed"`
	Summary        domain.PageSummary     `json:"summary"`
	ProposedSum    domain.PageSummary     `json:"proposed_summary"`
	Revision       int64                  `json:"revision"`
	PageGeneration int64                  `json:"page_generation"`
	NoOp           bool                   `json:"no_op"`
	RevertsTo      domain.ComponentSource `json:"reverts_to,omitempty"`
	// Notes are operator-facing consequences of the change that the two summaries do not show.
	Notes []string `json:"notes"`
}

// previewComponentConversion renders the page twice — as it is, and as it would be — using the
// SAME resolver the public page uses (org admin).
func (h *Handler) previewComponentConversion(w http.ResponseWriter, r *http.Request) {
	c, sp, ok := h.componentAccess(w, r)
	if !ok {
		return
	}
	var body conversionTargetBody
	if !decodeJSON(w, r, &body) {
		return
	}
	plan, err := h.store.PreviewComponentConversion(r.Context(), sp.OrgID, c.ID, body.target())
	if !h.writeConversionError(w, err) {
		return
	}
	view, ok := h.buildConversionPreview(w, r, sp, plan)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// confirmComponentConversion applies a previewed conversion under both CAS tokens (org admin).
func (h *Handler) confirmComponentConversion(w http.ResponseWriter, r *http.Request) {
	c, sp, ok := h.componentAccess(w, r)
	if !ok {
		return
	}
	var body conversionTargetBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Revision == nil || body.PageGeneration == nil {
		// Deliberately not defaulted: a confirmation without tokens is an unpreviewed conversion,
		// and accepting it would make the CAS optional in practice.
		writeError(w, http.StatusBadRequest,
			"revision and page_generation are required — confirm only what a preview returned")
		return
	}
	updated, err := h.store.ConfirmComponentConversion(r.Context(), sp.OrgID, c.ID, body.target(),
		*body.Revision, *body.PageGeneration, h.graphActor(r))
	if !h.writeConversionError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// writeConversionError maps the store's typed refusals to their HTTP answers, and reports whether
// the caller may continue.
func (h *Handler) writeConversionError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrComponentConversionStale):
		// The name the spec fixed (invariant 70), so a client can branch on it rather than on
		// prose: the page changed under the preview, re-read and try again.
		writeError(w, http.StatusConflict, "page_configuration_stale: "+err.Error())
	case errors.Is(err, store.ErrComponentConversionTarget):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrPageComponentCeiling):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.serverError(w, "component_conversion", err)
	}
	return false
}

// buildConversionPreview renders both sides of the change.
func (h *Handler) buildConversionPreview(
	w http.ResponseWriter, r *http.Request, sp domain.StatusPage, plan store.ComponentConversionPlan,
) (conversionPreviewView, bool) {
	ctx := r.Context()
	comps, err := h.store.ListComponentsByPage(ctx, sp.ID)
	if err != nil {
		h.serverError(w, "list_components", err)
		return conversionPreviewView{}, false
	}
	// The PROPOSED page is the current one with this component substituted — the whole page,
	// because the summary is a property of the page and not of the line being changed.
	proposedComps := make([]domain.Component, len(comps))
	copy(proposedComps, comps)
	for i := range proposedComps {
		if proposedComps[i].ID == plan.Proposed.ID {
			proposedComps[i] = plan.Proposed
		}
	}
	// No history on either side: a preview is a status question, and 90-day strips per component
	// would make an operator's click cost what a public render costs.
	before, err := h.resolveComponents(ctx, comps, false)
	if err != nil {
		h.serverError(w, "resolve_components", err)
		return conversionPreviewView{}, false
	}
	after, err := h.resolveComponents(ctx, proposedComps, false)
	if err != nil {
		h.serverError(w, "resolve_components", err)
		return conversionPreviewView{}, false
	}
	view := conversionPreviewView{
		Component:      previewComponentView(plan.Current, before[plan.Current.ID]),
		Proposed:       previewComponentView(plan.Proposed, after[plan.Proposed.ID]),
		Summary:        summarize(comps, before),
		ProposedSum:    summarize(proposedComps, after),
		Revision:       plan.Revision,
		PageGeneration: plan.PageGeneration,
		NoOp:           plan.NoOp,
		RevertsTo:      plan.RevertsTo,
		Notes:          conversionNotes(plan, after[plan.Proposed.ID]),
	}
	return view, true
}

// previewComponentView renders one line WITH its operator diagnostics — a preview is an
// authenticated surface by definition.
func previewComponentView(c domain.Component, res componentResolution) componentView {
	return componentView{
		ID: c.ID, Name: c.Name, Group: c.GroupName, Description: c.Description,
		Status: res.Status, Source: c.Source, Reason: res.Reason, Unavailable: res.Unavailable,
	}
}

// conversionNotes states the consequences the two summaries cannot show. They are the reason a
// preview is more than a diff: an operator about to publish `no_data` to customers should read
// that sentence before clicking, not after.
func conversionNotes(plan store.ComponentConversionPlan, after componentResolution) []string {
	var notes []string
	if plan.NoOp {
		return append(notes, "This component already renders from that source; confirming changes nothing.")
	}
	if after.Status == domain.CompNoData {
		notes = append(notes,
			"After this change the component publishes \"no data\" until its first measurement.")
	}
	if plan.RevertsTo != "" {
		notes = append(notes, "The current "+string(plan.RevertsTo)+
			" binding is kept, so this can be reverted without choosing it again.")
	}
	if plan.Current.Source == domain.ComponentSourceMonitor &&
		plan.Proposed.Source == domain.ComponentSourceService {
		notes = append(notes,
			"The 90-day history switches to the service's sealed facts and ends at its sealed watermark, "+
				"so the strip may be shorter than the monitor's.")
	}
	if plan.Current.Source == domain.ComponentSourceService &&
		plan.Proposed.Source != domain.ComponentSourceService {
		notes = append(notes, "The service's sealed history stops being published on this page.")
	}
	return notes
}

// ── The composite lifecycle (§15.5) ───────────────────────────────────────────────────────

// setMonitorSuccessor records or clears the service that now expresses what a monitor expresses.
// It is an annotation: nothing about execution or public output changes, which is exactly why it
// is a separate call from retiring.
func (h *Handler) setMonitorSuccessor(w http.ResponseWriter, r *http.Request) {
	mon, proj, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		ServiceID string `json:"service_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	updated, err := h.store.SetMonitorSuccessor(r.Context(), proj.ID, mon.ID, body.ServiceID, h.graphActor(r))
	if !h.writeLifecycleError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// retireMonitor ends a monitor's working life: the lifecycle statement AND the execution switch,
// in one store transaction. Nothing is deleted.
func (h *Handler) retireMonitor(w http.ResponseWriter, r *http.Request) {
	mon, proj, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	updated, err := h.store.RetireMonitor(r.Context(), proj.ID, mon.ID, h.graphActor(r))
	if !h.writeLifecycleError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// reactivateMonitor is the explicit inverse, so a mistaken retire is recoverable.
func (h *Handler) reactivateMonitor(w http.ResponseWriter, r *http.Request) {
	mon, proj, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	updated, err := h.store.ReactivateMonitor(r.Context(), proj.ID, mon.ID, h.graphActor(r))
	if !h.writeLifecycleError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// compositeConversionView reports what the conversion produced, including whether it was a
// re-confirm — the client shows "already converted" instead of a second success it did not cause.
type compositeConversionView struct {
	Service          domain.Service `json:"service"`
	Monitor          domain.Monitor `json:"monitor"`
	AlreadyConverted bool           `json:"already_converted"`
}

// convertCompositeToService builds a service that expresses what a composite expresses and links
// both ends. The composite keeps running; retiring stays a separate, explicit act.
//
// `sli` is REQUIRED and has no default: §15.5 says the reliability inputs need explicit
// confirmation, never a silent "all children". A caller that omits it gets a 400 naming the live
// children it may choose from, because guessing here would decide what availability MEANS for a
// customer-facing page.
func (h *Handler) convertCompositeToService(w http.ResponseWriter, r *http.Request) {
	mon, proj, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		SLI []string `json:"sli"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &body) {
		return
	}
	got, err := h.store.ConvertCompositeToService(r.Context(), proj.ID, mon.ID, body.SLI, h.graphActor(r))
	if !h.writeLifecycleError(w, err) {
		return
	}
	status := http.StatusCreated
	if got.AlreadyConverted {
		status = http.StatusOK
	}
	writeJSON(w, status, compositeConversionView{
		Service: got.Service, Monitor: got.Monitor, AlreadyConverted: got.AlreadyConverted,
	})
}

// writeLifecycleError maps the composite-lifecycle refusals. Each one is an answer the operator
// can act on, which is why none of them falls through to a 500.
func (h *Handler) writeLifecycleError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrMonitorAlreadyRetired), errors.Is(err, store.ErrMonitorNotRetired):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrSuccessorNotAService):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrCompositeNotComposite),
		errors.Is(err, store.ErrNotAComposite),
		errors.Is(err, store.ErrCompositeQuorumNotTranslatable),
		errors.Is(err, store.ErrCompositeSLIRequired),
		errors.Is(err, store.ErrCompositeSLINotAChild),
		errors.Is(err, store.ErrCompositeChildMissing):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrServiceSlugTaken), errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrManagedByFile), errors.Is(err, store.ErrServiceManagedByFile):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.serverError(w, "monitor_lifecycle", err)
	}
	return false
}
